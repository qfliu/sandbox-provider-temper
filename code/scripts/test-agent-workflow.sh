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
put() { curl -s -o /dev/null -w '%{http_code}' -H @"$TMP/auth" -X PUT --data-binary @"$1" "$API/sandboxes/$SB/files?path=$2"; }
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
printf 'FROM python:3.13-alpine\nRUN pip install --no-cache-dir redis==5.2.1\nCOPY app.py /app.py\nCMD ["python", "/app.py"]\n' >"$TMP/counter/Dockerfile"
printf 'services:\n  web:\n    build: .\n    ports: ["8000:8000"]\n    depends_on: [cache]\n  cache:\n    image: redis:8-alpine\n' >"$TMP/counter/compose.yaml"
run "mkdir -p counter pg gobuild" >/dev/null
for f in app.py test_app.py Dockerfile compose.yaml; do put "$TMP/counter/$f" "counter/$f" >/dev/null; done
works "unit tests pass" "cd counter && python3 -m unittest 2>&1 | tail -1" "OK"
works "compose up --build (web + redis)" "cd counter && docker compose up -d --build --quiet-pull 2>&1 | tail -1 && sleep 3 && docker compose ps --format '{{.Service}}={{.State}}' | sort | tr '\n' ' '" "web=running" 600
v=""
for _ in 1 2 3; do v=$(url 8000 "" | python3 -c "import json,sys; print(json.load(sys.stdin)['visits'])" 2>/dev/null); done
[ "$v" = 3 ] && pass "port URL: three visits counted (Redis reached by name)" || fail "port URL visits" "got '$v', want 3"
works "docker exec into redis (non-root) reads the counter" "cd counter && docker compose exec -T cache redis-cli get visits" "3"
run "cd counter && docker compose down" >/dev/null

# ---------------------------------------------------------------------------
printf "\n2. Postgres in compose: SQL through docker exec psql\n"
printf 'services:\n  db:\n    image: postgres:17-alpine\n    environment: {POSTGRES_PASSWORD: example}\n    healthcheck: {test: ["CMD", "pg_isready", "-U", "postgres"], interval: 2s, retries: 30}\n' >"$TMP/pg.yaml"
put "$TMP/pg.yaml" "pg/compose.yaml" >/dev/null
works "postgres starts and becomes healthy" "cd pg && docker compose up -d --wait --quiet-pull 2>&1 | tail -1; docker compose ps --format '{{.Health}}'" "healthy" 600
works "docker exec psql: create, insert, query" "cd pg && docker compose exec -T db psql -U postgres -tAc \"create table t(n int); insert into t select generate_series(1,5); select sum(n) from t\"" "15"
works "postgres runs as a non-root user" "cd pg && docker compose exec -T db stat -c %U /proc/1" "postgres"
run "cd pg && docker compose down" >/dev/null

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
printf 'FROM golang:1.23-alpine AS build\nWORKDIR /src\nCOPY . .\nRUN CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build -o /hello .\n\nFROM alpine:3.22\nCOPY --from=build /hello /hello\nCMD ["/hello"]\n' >"$TMP/Dockerfile.go"
for f in main.go main_test.go go.mod; do put "$TMP/$f" "gobuild/$f" >/dev/null; done
put "$TMP/Dockerfile.go" "gobuild/Dockerfile" >/dev/null
works "multi-stage build runs the Go tests and builds" "cd gobuild && docker build -q -t hello-go . >/dev/null && echo built" "built" 600
works "the small image runs the server" "docker run -d --name hello-go -p 8081:8081 hello-go >/dev/null && sleep 2 && docker ps --filter name=hello-go --format '{{.Status}}'" "Up"
g=$(url 8081 "")
[ "$g" = "hello, agent" ] && pass "port URL serves the Go app" || fail "Go app through port URL" "got '$g'"
run "docker rm -f hello-go" >/dev/null

# ---------------------------------------------------------------------------
printf "\n4. WebSocket server through the port URL\n"
works "websocket echo server starts" "docker pull -q jmalloc/echo-server:v0.3.7 >/dev/null && docker run -d --name ws -p 8086:8080 jmalloc/echo-server:v0.3.7 >/dev/null && sleep 3 && docker ps --filter name=ws --format '{{.Status}}'" "Up" 300
ws=$(python3 - "$PORT" "$SB" "$TMP/auth" <<'EOF'
import base64, os, socket, sys
port, sb, authfile = int(sys.argv[1]), sys.argv[2], sys.argv[3]
auth = open(authfile).read().strip()
key = base64.b64encode(os.urandom(16)).decode()
s = socket.create_connection(("127.0.0.1", port), timeout=15)
s.sendall((f"GET /v1/sandboxes/{sb}/ports/8086/.ws HTTP/1.1\r\nHost: x\r\n{auth}\r\n"
           f"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"
           f"Sec-WebSocket-Key: {key}\r\n\r\n").encode())
buf = b""
while b"\r\n\r\n" not in buf:
    buf += s.recv(4096)
head, rest = buf.split(b"\r\n\r\n", 1)
if b" 101 " not in head.split(b"\r\n")[0]:
    print("no-upgrade:" + head.split(b"\r\n")[0].decode()); sys.exit()
def frame(text):  # a masked client text frame
    p, m = text.encode(), os.urandom(4)
    return bytes([0x81, 0x80 | len(p)]) + m + bytes(b ^ m[i % 4] for i, b in enumerate(p))
def read_frame(rest):
    while len(rest) < 2: rest += s.recv(4096)
    n = rest[1] & 0x7F
    while len(rest) < 2 + n: rest += s.recv(4096)
    return rest[2:2 + n].decode(errors="replace"), rest[2 + n:]
msg, rest = read_frame(rest)          # the server's greeting
s.sendall(frame("ping from the agent"))
msg, rest = read_frame(rest)
print("echo:" + msg)
EOF
)
[ "$ws" = "echo:ping from the agent" ] && pass "WebSocket upgrade and echo through the port URL" || fail "WebSocket through port URL" "$ws"
run "docker rm -f ws" >/dev/null

# ---------------------------------------------------------------------------
printf "\n5. Docker's storage full: clean failure, Docker survives, cleanup frees it\n"
r=$(run "docker pull -q alpine:3.22 >/dev/null; docker run --rm alpine:3.22 sh -c 'dd if=/dev/zero of=/fill bs=1M count=4000 2>&1 | grep -m1 -o \"No space left on device\"'" 300)
[[ "$r" == *"No space left on device"* ]] && pass "filling it fails with 'No space left on device'" || fail "storage full" "$r"
works "Docker still answers" "docker info --format '{{.ServerVersion}}'" "."
works "the sandbox still runs commands" "echo alive" "alive"
works "cleanup frees the space" "docker system prune -af >/dev/null 2>&1; df -h /var/lib/docker | tail -1 | awk '{print \$5}'" "%"

printf "\n%d passed, %d failed\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
