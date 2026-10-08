#!/usr/bin/env bash
# Agent workflows end to end (D72): what an AI agent actually does with a
# Docker sandbox, through the API only (create, files, exec, port URLs,
# delete), in sequence. The feature suites check each feature on its own;
# this found what they missed (docker exec into a non-root container under
# Kata, D71). One ubuntu-docker sandbox, five workflows:
#   1. counter app: upload, unit tests, compose (web + redis), port URL,
#      exec into redis (non-root);
#   2. Postgres in compose: wait until ready, SQL through docker exec psql
#      (postgres runs as non-root);
#   3. multi-stage Go build: tests in the build stage, the binary in a small
#      image, served through the port URL;
#   4. a WebSocket server, reached through the port URL (upgrade, echo);
#   5. Docker's storage (a tmpfs) filled up: the write fails cleanly, Docker
#      and the sandbox keep working, cleanup frees it.
# Prints PASS/FAIL per check; exits non-zero on any failure.
#
# It pulls six images from Docker Hub unauthenticated (names below); back-to-
# back runs from one address can hit Hub's rate limit, which shows as FAILs.
#
# Usage (from the project root, with the API deployed; minikube or AKS):
#   source ./env.sh && ./code/scripts/test-agent-workflow.sh
#   SANDBOX_RUNTIME=kata ./code/scripts/test-agent-workflow.sh   (on a cluster with Kata)
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
PORT="${AGENT_TEST_PORT:-18092}"
API="http://127.0.0.1:$PORT/v1"
KEY="$(cat "${SANDBOX_KEYFILE:-$ROOT/.devenv/state/admin-api-key}")"
mkdir -p "$ROOT/.devenv/state/tmp"
TMP="$(mktemp -d "$ROOT/.devenv/state/tmp/agent.XXXXXX")"
(umask 077 && printf 'Authorization: Bearer %s\n' "$KEY" >"$TMP/auth")
RT=${SANDBOX_RUNTIME:+,\"runtime\":\"$SANDBOX_RUNTIME\"}
PASSED=0 FAILED=0 SB=""
# Images the workflows use (in the sandbox, from Docker Hub).
PY_IMG=python:3.13-alpine REDIS_IMG=redis:8-alpine PG_IMG=postgres:17-alpine
GO_IMG=golang:1.27.1-alpine ALPINE_IMG=alpine:3.22 WS_IMG=jmalloc/echo-server:v0.3.7

pass() { PASSED=$((PASSED + 1)); printf "  \033[32mPASS\033[0m  %s\n" "$1"; }
fail() { FAILED=$((FAILED + 1)); printf "  \033[31mFAIL\033[0m  %s  (%s)\n" "$1" "$2"; }
die() { printf "setup failed: %s\n" "$1" >&2; exit 1; }

cleanup() {
	[ -n "$SB" ] && curl -s -o /dev/null -X DELETE -H @"$TMP/auth" "$API/sandboxes/$SB"
	[ -n "${PF:-}" ] && { kill "$PF"; wait "$PF"; } 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT

kubectl -n sandbox-system port-forward svc/sandbox-api "$PORT:80" >/dev/null 2>&1 &
PF=$!
for _ in $(seq 1 40); do curl -s -m 1 -o /dev/null "http://127.0.0.1:$PORT/healthz" && break; sleep 0.25; done

ssh-keygen -q -t ed25519 -N "" -f "$TMP/k"
SB=$(curl -s -H @"$TMP/auth" -X POST "$API/sandboxes" \
	-d "{\"image\":\"ubuntu-docker\",\"ttl_seconds\":3600,\"ssh_public_key\":\"$(cat "$TMP/k.pub")\"$RT}" |
	python3 -c "import json,sys; print(json.load(sys.stdin)['id'])") || die "could not create a sandbox"
s=""
for _ in $(seq 1 120); do
	s=$(curl -s -H @"$TMP/auth" "$API/sandboxes/$SB" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")
	[ "$s" = running ] && break
	sleep 1
done
[ "$s" = running ] || die "sandbox $SB not running (status: $s)"
printf "sandbox %s (ubuntu-docker, %s)\n" "$SB" "${SANDBOX_RUNTIME:-default runtime}"

# run <command> [timeout]: exec as `user`; prints "<exit_code>|<output, one line>"
run() {
	curl -s -H @"$TMP/auth" -X POST "$API/sandboxes/$SB/exec" \
		-d "$(python3 -c 'import json,sys; print(json.dumps({"command": sys.argv[1], "timeout_seconds": int(sys.argv[2])}))' "$1" "${2:-300}")" |
		python3 -c "import json,sys
try:
    d=json.load(sys.stdin); out=' '.join((d['stdout']+d['stderr']).split())
    print(str(d['exit_code'])+'|'+out[-300:])
except Exception: print('|exec failed')"
}
# works <name> <command> <expected substring>
works() {
	local r; r=$(run "$2" "${4:-300}")
	[ "${r%%|*}" = 0 ] && [[ "${r#*|}" == *"$3"* ]] && pass "$1" || fail "$1" "$r"
}
# put <local file> <path in /home/user>
put() {
	local code
	code=$(curl -s -o /dev/null -w '%{http_code}' -H @"$TMP/auth" -X PUT --data-binary @"$1" "$API/sandboxes/$SB/files?path=$2")
	[ "$code" = 200 ] || die "upload of $2 failed (HTTP $code)"
}
# url <port> <path>: GET through the port URL
url() { curl -s -m 30 -H @"$TMP/auth" "$API/sandboxes/$SB/ports/$1/$2"; }

# ---------------------------------------------------------------------------
printf "\n1. counter app: tests, compose, port URL, exec into redis\n"
mkdir -p "$TMP/counter"
cat >"$TMP/counter/app.py" <<'EOF'
import json, os
from http.server import BaseHTTPRequestHandler, HTTPServer

def render(count):
    return json.dumps({"visits": count})

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        import redis
        r = redis.Redis(host=os.environ.get("REDIS_HOST", "cache"))
        body = render(r.incr("visits")).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass

if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 8000), Handler).serve_forever()
EOF
cat >"$TMP/counter/test_app.py" <<'EOF'
import json, unittest
from app import render
class T(unittest.TestCase):
    def test_render(self): self.assertEqual(json.loads(render(3))["visits"], 3)
EOF
printf "FROM $PY_IMG\nRUN pip install --no-cache-dir redis==5.2.1\nCOPY app.py /app.py\nCMD [\"python\", \"/app.py\"]\n" >"$TMP/counter/Dockerfile"
printf 'services:\n  web:\n    build: .\n    ports: ["8000:8000"]\n    depends_on: [cache]\n  cache:\n    image: %s\n' "$REDIS_IMG" >"$TMP/counter/compose.yaml"
run "mkdir -p counter pg gobuild" >/dev/null
for f in app.py test_app.py Dockerfile compose.yaml; do put "$TMP/counter/$f" "counter/$f"; done
works "unit tests pass" "cd counter && python3 -m unittest 2>&1 | tail -1" "OK"
works "compose up --build (web + redis)" "cd counter && { docker compose up -d --build --quiet-pull >/tmp/up.log 2>&1 || { tail -5 /tmp/up.log; exit 1; }; } && docker compose ps --format '{{.Service}}={{.State}}' | sort | tr '\n' ' '" "web=running" 600
# The first visit is retried until the server answers; then two more.
visits() { url 8000 "" | python3 -c "import json,sys; print(json.load(sys.stdin)['visits'])" 2>/dev/null; }
v=""
for _ in $(seq 1 15); do v=$(visits) && [ -n "$v" ] && break; sleep 1; done
v=$(visits); v=$(visits)
[ "$v" = 3 ] && pass "port URL: three visits counted (Redis reached by name)" || fail "port URL visits" "got '$v', want 3"
works "docker exec into redis (non-root) reads the counter" "cd counter && docker compose exec -T cache redis-cli get visits | grep -qx 3 && echo counter=3" "counter=3"
run "cd counter && docker compose down -v" >/dev/null

# ---------------------------------------------------------------------------
printf "\n2. Postgres in compose: SQL through docker exec psql\n"
printf 'services:\n  db:\n    image: %s\n    environment: {POSTGRES_PASSWORD: example}\n    healthcheck: {test: ["CMD", "pg_isready", "-U", "postgres"], interval: 2s, retries: 30}\n' "$PG_IMG" >"$TMP/pg.yaml"
put "$TMP/pg.yaml" "pg/compose.yaml"
# Exactly "healthy" (a substring match would also take "unhealthy"), and
# `up --wait` must itself succeed.
works "postgres starts and becomes healthy" "cd pg && docker compose up -d --wait --wait-timeout 120 --quiet-pull >/tmp/pg.log 2>&1 && docker compose ps --format '{{.Health}}' | grep -qx healthy && echo health=ok" "health=ok" 600
works "docker exec psql: create, insert, query" "cd pg && docker compose exec -T db psql -U postgres -tAc \"create table t(n int); insert into t select generate_series(1,5); select sum(n) from t\" | grep -qx 15 && echo sum=15" "sum=15"
works "postgres runs as a non-root user" "cd pg && docker compose exec -T db stat -c %U /proc/1" "postgres"
run "cd pg && docker compose down -v" >/dev/null

# ---------------------------------------------------------------------------
printf "\n3. multi-stage Go build: tests in the build, served through the port URL\n"
cat >"$TMP/main.go" <<'EOF'
package main

import (
	"fmt"
	"net/http"
)

func greet(name string) string { return "hello, " + name }

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, greet("agent")) })
	http.ListenAndServe(":8081", nil)
}
EOF
cat >"$TMP/main_test.go" <<'EOF'
package main

import "testing"

func TestGreet(t *testing.T) {
	if got := greet("x"); got != "hello, x" {
		t.Fatalf("greet = %q", got)
	}
}
EOF
printf 'module example.com/hello\n\ngo 1.23\n' >"$TMP/go.mod"
printf 'FROM %s AS build\nWORKDIR /src\nCOPY . .\nRUN CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build -o /hello .\n\nFROM %s\nCOPY --from=build /hello /hello\nCMD ["/hello"]\n' "$GO_IMG" "$ALPINE_IMG" >"$TMP/Dockerfile.go"
for f in main.go main_test.go go.mod; do put "$TMP/$f" "gobuild/$f"; done
put "$TMP/Dockerfile.go" "gobuild/Dockerfile"
works "multi-stage build runs the Go tests and builds" "cd gobuild && docker build -q -t hello-go . >/dev/null && echo built" "built" 600
works "the small image runs the server" "docker run -d --name hello-go -p 8081:8081 hello-go >/dev/null && sleep 2 && docker ps --filter name=^hello-go$ --format '{{.Status}}'" "Up"
g=$(url 8081 "")
[ "$g" = "hello, agent" ] && pass "port URL serves the Go app" || fail "Go app through port URL" "got '$g'"
run "docker rm -f hello-go" >/dev/null

# ---------------------------------------------------------------------------
printf "\n4. WebSocket server through the port URL\n"
works "websocket echo server starts" "docker pull -q $WS_IMG >/dev/null && docker run -d --name ws -p 8086:8080 $WS_IMG >/dev/null && sleep 3 && docker ps --filter name=^ws$ --format '{{.Status}}'" "Up" 300
ws=$(python3 - "$PORT" "$SB" "$TMP/auth" <<'EOF'
# A minimal WebSocket client: the upgrade through the port URL, then one
# text frame each way. jmalloc/echo-server greets each connection first.
import base64, os, socket, struct, sys
port, sb, authfile = int(sys.argv[1]), sys.argv[2], sys.argv[3]
try:
    auth = open(authfile).read().strip()
    s = socket.create_connection(("127.0.0.1", port), timeout=15)
    def more():
        d = s.recv(4096)
        if not d:
            raise RuntimeError("connection closed")
        return d
    key = base64.b64encode(os.urandom(16)).decode()
    s.sendall((f"GET /v1/sandboxes/{sb}/ports/8086/.ws HTTP/1.1\r\nHost: x\r\n{auth}\r\n"
               f"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"
               f"Sec-WebSocket-Key: {key}\r\n\r\n").encode())
    buf = b""
    while b"\r\n\r\n" not in buf:
        buf += more()
    head, rest = buf.split(b"\r\n\r\n", 1)
    if b" 101 " not in head.split(b"\r\n")[0]:
        raise RuntimeError("no upgrade: " + head.split(b"\r\n")[0].decode())
    def need(rest, n):
        while len(rest) < n:
            rest += more()
        return rest
    def read_text(rest):  # the next text frame; control frames skipped
        while True:
            rest = need(rest, 2)
            op, n, off = rest[0] & 0x0F, rest[1] & 0x7F, 2
            if n == 126:
                rest = need(rest, 4); n, off = struct.unpack("!H", rest[2:4])[0], 4
            elif n == 127:
                rest = need(rest, 10); n, off = struct.unpack("!Q", rest[2:10])[0], 10
            rest = need(rest, off + n)
            payload, rest = rest[off:off + n], rest[off + n:]
            if op == 1:
                return payload.decode(errors="replace"), rest
    def frame(text):  # a masked client text frame (short)
        p, m = text.encode(), os.urandom(4)
        return bytes([0x81, 0x80 | len(p)]) + m + bytes(b ^ m[i % 4] for i, b in enumerate(p))
    _, rest = read_text(rest)  # the server's greeting
    s.sendall(frame("ping from the agent"))
    msg, rest = read_text(rest)
    print("echo:" + msg)
except Exception as e:
    print("error:", e)
EOF
)
[ "$ws" = "echo:ping from the agent" ] && pass "WebSocket upgrade and echo through the port URL" || fail "WebSocket through port URL" "$ws"
run "docker rm -f ws" >/dev/null

# ---------------------------------------------------------------------------
printf "\n5. Docker's storage full: clean failure, Docker survives, cleanup frees it\n"
# Fill it from a container that stays (no --rm), so the prune below has
# something to free; 4000 MB is more than the 2 GB tmpfs.
r=$(run "docker pull -q $ALPINE_IMG >/dev/null; docker run --name fill $ALPINE_IMG sh -c 'dd if=/dev/zero of=/fill bs=1M count=4000 2>&1 | grep -m1 -o \"No space left on device\"'" 300)
[[ "$r" == *"No space left on device"* ]] && pass "filling it fails with 'No space left on device'" || fail "storage full" "$r"
works "Docker still answers" "docker info --format 'docker={{.ServerVersion}}'" "docker=2"
works "the sandbox still runs commands" "echo alive" "alive"
works "storage is nearly full before cleanup" "u=\$(df --output=pcent /var/lib/docker | tail -1 | tr -dc 0-9); [ \"\$u\" -ge 90 ] && echo used=\$u" "used="
works "cleanup frees the space" "docker system prune -af --volumes >/dev/null 2>&1; u=\$(df --output=pcent /var/lib/docker | tail -1 | tr -dc 0-9); [ \"\$u\" -lt 50 ] && echo used=\$u" "used="

printf "\n%d passed, %d failed\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
