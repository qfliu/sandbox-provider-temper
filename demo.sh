#!/usr/bin/env bash
# A narrated end-to-end walkthrough of the sandbox provider, the way an AI
# agent would use it: create a sandbox over the HTTP API, run commands,
# move files, SSH in through the gateway, check the isolation, delete it.
# Each step prints the request it makes. Run ./up.sh first. Everything it
# creates is deleted at the end (also on Ctrl-C).
#
# Usage: ./demo.sh [ubuntu-dev|ubuntu-base]
set -uo pipefail
cd "$(dirname "$0")"
IMAGE="${1:-ubuntu-dev}"
# shellcheck source=env.sh
source ./env.sh
KEYFILE=$SANDBOX_KEYFILE   # this cluster's admin key (env.sh)
[ -s "$KEYFILE" ] || { echo "no API key in $KEYFILE: run ./up.sh first" >&2; exit 1; }
PORT="${DEMO_PORT:-18090}"
API="http://127.0.0.1:$PORT/v1"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/demo.XXXXXX")
# The key goes to curl in a header file (-H @file), never as an argument,
# so it doesn't show up in `ps`.
(umask 077 && { printf 'Authorization: Bearer '; tr -d '\n' <"$KEYFILE"; echo; } >"$TMP/auth")
AUTH=(-H @"$TMP/auth")
ID=""

bold() { printf "\n\033[1m%s\033[0m\n" "$*"; }
note() { printf "  %s\n" "$*"; }
# call <method> <path> [curl args...]: shows the request (key elided), then
# prints the response body indented. Body also in $TMP/out, status in $CODE.
call() {
	local method=$1 path=$2
	shift 2
	printf '  \033[2m$ curl -X %s -H "Authorization: Bearer $KEY" %s %s\033[0m\n' "$method" \
		"$(python3 -c 'import shlex,sys; print(" ".join(shlex.quote(a) for a in sys.argv[1:]))' "$API$path" "$@")"
	CODE=$(curl -s -o "$TMP/out" -w '%{http_code}' -X "$method" "${AUTH[@]}" "$@" "$API$path")
	[ -n "${QUIET:-}" ] && return
	python3 -c 'import json,sys
try: print(json.dumps(json.load(open(sys.argv[1])), indent=2))
except Exception: print(open(sys.argv[1], errors="replace").read())' "$TMP/out" | sed 's/^/    /' | head -25
}
field() { python3 -c "import json,sys; print(json.load(open(sys.argv[1]))$1)" "$TMP/out"; }
run() { # run <command>: exec in the sandbox through the API; shows the
	# response's stdout/stderr as text rather than JSON
	QUIET=1 call POST "/sandboxes/$ID/exec" -d "$(python3 -c 'import json,sys; print(json.dumps({"command": sys.argv[1]}))' "$1")"
	python3 -c 'import json,sys
d = json.load(open(sys.argv[1]))
if "exit_code" not in d: print(json.dumps(d)); sys.exit()
print("\033[2m→ exit %s, %s ms\033[0m" % (d["exit_code"], d["duration_ms"]))
for line in d["stdout"].splitlines(): print(line)
for line in d["stderr"].splitlines(): print("\033[33m" + line + "\033[0m")' "$TMP/out" | sed 's/^/    /'
}
cleanup() {
	if [ -n "$ID" ]; then
		curl -s -o /dev/null -X DELETE "${AUTH[@]}" "$API/sandboxes/$ID" || true
	fi
	[ -n "${PF:-}" ] && kill "$PF" 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

kubectl -n sandbox-system port-forward --address 127.0.0.1 svc/sandbox-api "$PORT:80" >"$TMP/pf.log" 2>&1 &
PF=$!
# Only talk (and send the key) to our own port-forward: wait until it says
# it's listening; if the port was taken, it exits instead.
for _ in $(seq 1 40); do
	grep -q "Forwarding from" "$TMP/pf.log" && break
	kill -0 "$PF" 2>/dev/null || break
	sleep 0.25
done
grep -q "Forwarding from" "$TMP/pf.log" && kill -0 "$PF" 2>/dev/null ||
	{ echo "couldn't forward port $PORT (in use? set DEMO_PORT): $(tail -1 "$TMP/pf.log")" >&2; exit 1; }
curl -s -m 2 "http://127.0.0.1:$PORT/healthz" | grep -q '"status":"ok"' ||
	{ echo "API not reachable (is ./up.sh done?)" >&2; exit 1; }

bold "1. Who am I? Every API call carries an API key."
call GET /me

bold "2. Create a sandbox ($IMAGE) with my SSH public key and a 10-minute lifetime."
ssh-keygen -q -t ed25519 -N "" -C demo -f "$TMP/id"
note "(a throwaway SSH key pair for this demo: $TMP/id)"
START=$(python3 -c 'import time; print(time.time())')
call POST /sandboxes -d "{\"image\":\"$IMAGE\",\"ttl_seconds\":600,\"ssh_public_key\":\"$(cat "$TMP/id.pub")\"}"
ID=$(field "['id']")
[ "$CODE" = 201 ] && [ -n "$ID" ] || { echo "create failed (HTTP $CODE)" >&2; exit 1; }
note "Waiting for it to start: a gVisor pod, sshd ready..."
for _ in $(seq 1 120); do
	curl -s -o "$TMP/out" "${AUTH[@]}" "$API/sandboxes/$ID"
	[ "$(field "['status']")" = running ] && break
	sleep 0.25
done
[ "$(field "['status']")" = running ] ||
	{ echo "not running after 30 s (status $(field "['status']")); the dashboard shows why" >&2; exit 1; }
printf "  \033[32m%s is running after %.1f s\033[0m\n" "$ID" "$(python3 -c "import time; print(time.time()-$START)")"

bold "3. Run commands through the API (what an agent does most)."
run 'uname -sr && whoami && python3 --version'
note "^ the kernel is gVisor's (a user-space kernel), not the node's: code in here never talks to the host kernel directly."
run 'python3 -m venv ~/venv && ~/venv/bin/pip install --quiet cowsay && ~/venv/bin/cowsay -t "hello from $(hostname)"'
note "^ packages come from the internet (PyPI) into a virtualenv; the sandbox is a normal Ubuntu with sudo."

bold "4. Files: upload a script, run it, download what it wrote."
printf 'import platform, json\njson.dump({"python": platform.python_version(), "machine": platform.machine()}, open("result.json", "w"))\n' >"$TMP/job.py"
call PUT "/sandboxes/$ID/files?path=job.py" --data-binary @"$TMP/job.py"
run 'python3 job.py && ls -l result.json'
call GET "/sandboxes/$ID/files?path=result.json"

bold "5. SSH in through the gateway, with my own key (as a person or an agent would)."
# shellcheck source=code/scripts/gateway-addr.sh
source code/scripts/gateway-addr.sh
read -r GW_HOST GW_PORT <<<"$(gateway_addr)" && [ -n "$GW_PORT" ] || { echo "can't find the SSH gateway's address" >&2; exit 1; }
GW_PUB=$(kubectl -n sandbox-system exec deploy/sandbox-gateway -- /api public-key /keys/host/ssh_host_ed25519_key | cut -d' ' -f1,2)
# known_hosts names port 22 by the bare host; other ports as [host]:port.
KH_HOST=$([ "$GW_PORT" = 22 ] && echo "$GW_HOST" || echo "[$GW_HOST]:$GW_PORT")
echo "$KH_HOST $GW_PUB" >"$TMP/known_hosts"
note "(the gateway's host key is pinned, so a man in the middle would be refused)"
printf '  \033[2m$ ssh -p %s %s@%s '"'"'hostname; cat job.py | wc -l'"'"'\033[0m\n' "$GW_PORT" "$ID" "$GW_HOST"
ssh -F /dev/null -i "$TMP/id" -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes \
	-o UserKnownHostsFile="$TMP/known_hosts" -o StrictHostKeyChecking=yes -p "$GW_PORT" \
	"$ID@$GW_HOST" 'hostname; cat job.py | wc -l' 2>&1 | sed 's/^/    /'

bold "6. Isolation: the internet works, the cluster doesn't."
run 'curl -s -o /dev/null -w "pypi.org: HTTP %{http_code}\n" -m 5 https://pypi.org/simple/'
run 'curl -s -m 3 -k https://kubernetes.default.svc >/dev/null && echo "reached the Kubernetes API" || echo "Kubernetes API: blocked"; curl -s -m 3 http://169.254.169.254/ >/dev/null && echo "reached the metadata endpoint" || echo "cloud metadata endpoint: blocked"'
note "NetworkPolicies allow DNS and the public internet only; other sandboxes, the platform, the nodes and private ranges are unreachable."

bold "7. The dashboard shows the same sandboxes (and, for admins, everyone's)."
note "source ./env.sh $SANDBOX_TARGET && kubectl -n sandbox-system port-forward svc/sandbox-api 18080:80"
note "then open http://localhost:18080/ui and sign in with the key in $KEYFILE"

bold "8. Delete it."
call DELETE "/sandboxes/$ID"
for _ in $(seq 1 40); do
	curl -s -o "$TMP/out" "${AUTH[@]}" "$API/sandboxes/$ID"
	[ "$(field "['status']")" = deleted ] && break
	sleep 0.25
done
if [ "$(field "['status']")" = deleted ]; then
	note "status: deleted (the pod and its secrets are gone; the record stays for history)"
else
	note "status: $(field "['status']") (still shutting down; it finishes in the background)"
fi
ID=""
bold "Done."
