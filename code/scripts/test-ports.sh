#!/usr/bin/env bash
# Sandbox port URLs (step 8.7, D66): an HTTP server inside a sandbox is
# reachable at /v1/sandboxes/{id}/ports/{port}/ with the owner's API key,
# and only that way. Checks that requests arrive with their path, query and
# body but without the caller's API key or cookies; that responses lose
# their cookies and gain a CSP sandbox header; that a port with nothing on
# it is a 502, another user's sandbox a 404, no key a 401; and that the
# forward can't leave the sandbox (only 127.0.0.1 inside it). Prints
# PASS/FAIL per check; exits non-zero on any failure.
#
# Usage (from the project root, with the API deployed; minikube or AKS):
#   source ./env.sh && ./code/scripts/test-ports.sh
#   SANDBOX_RUNTIME=kata ./code/scripts/test-ports.sh   (on a cluster with Kata)
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
PORT="${PORTS_TEST_PORT:-18094}"
API="http://127.0.0.1:$PORT/v1"
KEY="$(cat "${SANDBOX_KEYFILE:-$ROOT/.devenv/state/admin-api-key}")"
mkdir -p "$ROOT/.devenv/state/tmp"
TMP="$(mktemp -d "$ROOT/.devenv/state/tmp/ports.XXXXXX")"
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
	-d "{\"image\":\"ubuntu-dev\",\"ssh_public_key\":\"$(cat "$TMP/k.pub")\"$RT}" |
	python3 -c "import json,sys; print(json.load(sys.stdin)['id'])") || die "could not create a sandbox"
s=""
for _ in $(seq 1 120); do
	s=$(curl -s -H @"$TMP/auth" "$API/sandboxes/$SB" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")
	[ "$s" = running ] && break
	sleep 1
done
[ "$s" = running ] || die "sandbox $SB not running (status: $s)"
printf "sandbox %s (ubuntu-dev, %s)\n" "$SB" "${SANDBOX_RUNTIME:-default runtime}"

# An app inside: echoes what it received as JSON and sets a cookie.
cat >"$TMP/app.py" <<'EOF'
import http.server, json, sys
class H(http.server.BaseHTTPRequestHandler):
    def reply(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.dumps({"method": self.command, "path": self.path,
                           "headers": {k.lower(): v for k, v in self.headers.items()},
                           "body": self.rfile.read(n).decode()}).encode()
        self.send_response(200)
        self.send_header("Set-Cookie", "session=from-the-sandbox; Path=/ui")
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)
    do_GET = do_POST = reply
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer((sys.argv[1], int(sys.argv[2])), H).serve_forever()
EOF
curl -s -o /dev/null -H @"$TMP/auth" -X PUT --data-binary @"$TMP/app.py" "$API/sandboxes/$SB/files?path=app.py"
curl -s -o /dev/null -H @"$TMP/auth" -X POST "$API/sandboxes/$SB/exec" \
	-d '{"command":"(setsid python3 app.py 127.0.0.1 8000 >/dev/null 2>&1 &); (setsid python3 app.py 0.0.0.0 8001 >/dev/null 2>&1 &); sleep 1"}'

printf "\nproxied to the sandbox\n"
U="$API/sandboxes/$SB/ports"
code=$(curl -s -o "$TMP/r" -D "$TMP/h" -w '%{http_code}' -H @"$TMP/auth" -H 'Cookie: mine=1' \
	-X POST --data 'hello' "$U/8000/items/1?q=x")
field() { python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(eval(sys.argv[2]))" "$TMP/r" "$1" 2>/dev/null; }
[ "$code" = 200 ] && pass "server on 127.0.0.1:8000 → 200" || fail "127.0.0.1:8000" "$code $(head -c 200 "$TMP/r")"
[ "$(field "d['method']+' '+d['path']+' '+d['body']")" = "POST /items/1?q=x hello" ] &&
	pass "method, path, query and body arrive" || fail "request" "$(head -c 300 "$TMP/r")"
[ "$(field "'authorization' in d['headers'] or 'cookie' in d['headers']")" = False ] &&
	pass "the API key and cookies don't reach the sandbox" || fail "credentials forwarded" "$(head -c 300 "$TMP/r")"
[ "$(field "d['headers'].get('x-forwarded-prefix')")" = "/v1/sandboxes/$SB/ports/8000" ] &&
	pass "X-Forwarded-Prefix tells the app its path" || fail "X-Forwarded-Prefix" "$(head -c 300 "$TMP/r")"
! grep -qi '^set-cookie' "$TMP/h" && grep -qi '^content-security-policy: sandbox' "$TMP/h" &&
	pass "response: cookie removed, CSP sandbox added" || fail "response headers" "$(tr '\r\n' ' ' <"$TMP/h")"
code=$(curl -s -o /dev/null -w '%{http_code}' -H @"$TMP/auth" "$U/8001/")
[ "$code" = 200 ] && pass "server on 0.0.0.0:8001 → 200" || fail "0.0.0.0:8001" "$code"
code=$(curl -s -o /dev/null -w '%{http_code}' -H @"$TMP/auth" "$U/8000")
[ "$code" = 308 ] && pass "no trailing slash → redirect" || fail "redirect" "$code"

printf "\nrefused\n"
code=$(curl -s -o "$TMP/r" -w '%{http_code}' -H @"$TMP/auth" "$U/8002/")
[ "$code" = 502 ] && grep -q "nothing answered on port 8002" "$TMP/r" && pass "nothing listening → 502" || fail "closed port" "$code $(cat "$TMP/r")"
code=$(curl -s -o /dev/null -w '%{http_code}' "$U/8000/")
[ "$code" = 401 ] && pass "no API key → 401" || fail "no key" "$code"
code=$(curl -s -o /dev/null -w '%{http_code}' -H @"$TMP/auth" "$API/sandboxes/sb-000000000000/ports/8000/")
[ "$code" = 404 ] && pass "unknown sandbox → 404" || fail "unknown sandbox" "$code"
bad=""
for p in 0 65536 x; do
	code=$(curl -s -o /dev/null -w '%{http_code}' -H @"$TMP/auth" "$U/$p/")
	[ "$code" = 400 ] || bad="$bad $p→$code"
done
[ -z "$bad" ] && pass "ports 0, 65536 and 'x' → 400" || fail "bad ports" "$bad (want 400)"
# Another user's sandbox looks like it doesn't exist (smoke-api.sh's second
# user; smoke-api.sh refreshes its key for the current cluster).
OKEY=$(cat "$ROOT/.devenv/state/smoke-keys/smoke-other" 2>/dev/null || true)
if [ -n "$OKEY" ] && [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $OKEY" "$API/me")" = 200 ]; then
	(umask 077 && printf 'Authorization: Bearer %s\n' "$OKEY" >"$TMP/oauth")
	code=$(curl -s -o /dev/null -w '%{http_code}' -H @"$TMP/oauth" "$U/8000/")
	[ "$code" = 404 ] && pass "another user's key → 404" || fail "another user" "$code"
else
	fail "another user" "no valid key for smoke-other on this cluster: run smoke-api.sh first"
fi
# The forward stays inside the sandbox: its sshd permits only loopback.
r=$(curl -s -H @"$TMP/auth" -X POST "$API/sandboxes/$SB/exec" -d '{"command":"grep -E \"^(AllowTcpForwarding|PermitOpen)\" /etc/ssh/sshd_config | tr \"\\n\" \" \""}' |
	python3 -c "import json,sys; print(json.load(sys.stdin)['stdout'])")
[[ "$r" == *"AllowTcpForwarding local"*"PermitOpen localhost:* 127.0.0.1:*"* ]] &&
	pass "sandbox sshd forwards only to its own loopback" || fail "sshd forwarding config" "$r"

printf "\n%d passed, %d failed\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
