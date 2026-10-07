#!/usr/bin/env bash
# End-to-end test of SSH access (D6, D35, D36) the way a user or agent on
# this Mac uses it: real OpenSSH tools (ssh, scp, sftp) against the gateway's
# NodePort on the minikube node, with the gateway's host key pinned. Covers:
# login with the sandbox's key; refusal of a wrong key, of another
# sandbox's key, of an unknown and of a deleted sandbox; exit codes; an
# interactive terminal; scp and sftp; port forwarding refused; and an SSH
# session resetting the idle timer. Prints PASS/FAIL per check and exits
# non-zero on any failure. Every sandbox it creates is deleted.
#
# Usage (from the project root, with the API and gateway deployed):
#   source ./env.sh && ./code/scripts/test-gateway-e2e.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
PORT="${E2E_PORT:-18096}"
API="http://127.0.0.1:$PORT/v1"
KEY="$(cat "$ROOT/.devenv/state/admin-api-key")"
# shellcheck source=gateway-addr.sh
source "$ROOT/code/scripts/gateway-addr.sh"
read -r GW_HOST GW_PORT <<<"$(gateway_addr)"
[ -n "${GW_PORT:-}" ] || { echo "setup failed: can't find the SSH gateway's address" >&2; exit 1; }
TMP="$(mktemp -d "$ROOT/.devenv/state/tmp/e2e.XXXXXX")"
PASSED=0 FAILED=0 SANDBOXES=()

pass() { PASSED=$((PASSED + 1)); printf "  \033[32mPASS\033[0m  %s\n" "$1"; }
fail() { FAILED=$((FAILED + 1)); printf "  \033[31mFAIL\033[0m  %s  (%s)\n" "$1" "$2"; }
die() { printf "setup failed: %s\n" "$1" >&2; exit 1; }

cleanup() {
	for id in ${SANDBOXES[@]+"${SANDBOXES[@]}"}; do
		curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $KEY" "$API/sandboxes/$id" || true
	done
	[ -n "${PF:-}" ] && { kill "$PF"; wait "$PF"; } 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT

kubectl -n sandbox-system port-forward svc/sandbox-api "$PORT:80" >/dev/null 2>&1 &
PF=$!
for _ in $(seq 1 40); do curl -s -m 1 -o /dev/null "http://127.0.0.1:$PORT/healthz" && break; sleep 0.25; done
curl -s -m 2 -o /dev/null "http://127.0.0.1:$PORT/healthz" || die "API not reachable on 127.0.0.1:$PORT"

# Pin the gateway's host key: a client that skipped this would be open to a
# man in the middle. StrictHostKeyChecking=yes makes a mismatch fatal.
# (Derived from the chart-generated private key by the gateway pod itself:
# macOS ssh-keygen can't read its PKCS#8 format.)
GW_PUB=$(kubectl -n sandbox-system exec deploy/sandbox-gateway -- /api public-key /keys/host/ssh_host_ed25519_key | cut -d' ' -f1,2)
[ -n "$GW_PUB" ] || die "can't read the gateway's host key"
echo "[$GW_HOST]:$GW_PORT $GW_PUB" >"$TMP/known_hosts"

# Only the options given here count: no ~/.ssh/config, no ssh-agent keys.
# (ssh uses the first value given for an option, so a test that needs a
# different one puts its -o before these.)
SSH_OPTS=(-F /dev/null -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes
	-o UserKnownHostsFile="$TMP/known_hosts" -o StrictHostKeyChecking=yes
	-o ConnectTimeout=10 -o LogLevel=ERROR)
# gw_ssh <key> <sandbox> [ssh args…]
gw_ssh() { local k=$1 sb=$2; shift 2; ssh "${SSH_OPTS[@]}" -i "$TMP/$k" -p "$GW_PORT" "$sb@$GW_HOST" "$@"; }

# api_field <sandbox> <field>: a field of GET /sandboxes/<id> ("" on error)
api_field() {
	curl -s -H "Authorization: Bearer $KEY" "$API/sandboxes/$1" |
		python3 -c "import json,sys
try: print(json.load(sys.stdin)['$2'])
except Exception: print('')"
}
new_sandbox() { # new_sandbox <key name>: creates key + sandbox, prints its id
	ssh-keygen -q -t ed25519 -N "" -C "e2e-$1" -f "$TMP/$1"
	curl -s -H "Authorization: Bearer $KEY" -X POST "$API/sandboxes" \
		-d "{\"image\":\"ubuntu-base\",\"ssh_public_key\":\"$(cat "$TMP/$1.pub")\"}" |
		python3 -c "import json,sys; print(json.load(sys.stdin)['id'])"
}
wait_running() {
	local s=""
	for _ in $(seq 1 90); do
		s=$(api_field "$1" status)
		[ "$s" = running ] && return 0
		sleep 1
	done
	die "sandbox $1 not running (status: $s)"
}

A=$(new_sandbox a) || die "could not create sandbox A"
SANDBOXES=("$A")
B=$(new_sandbox b) || die "could not create sandbox B"
SANDBOXES=("$A" "$B")
ssh-keygen -q -t ed25519 -N "" -C e2e-stranger -f "$TMP/stranger"
wait_running "$A"
wait_running "$B"
printf "gateway %s:%s; sandboxes %s (key a) and %s (key b)\n" "$GW_HOST" "$GW_PORT" "$A" "$B"

# denied <name> <key> <sandbox>: login must fail with "Permission denied"
denied() {
	local out
	out=$(gw_ssh "$2" "$3" -n true 2>&1)
	[ $? = 255 ] && grep -q "Permission denied" <<<"$out" && pass "$1 → refused" || fail "$1" "$out"
}

printf "\nlogin\n"
out=$(gw_ssh a "$A" -n uname -r 2>&1)
[[ "$out" == *gvisor* ]] && pass "sandbox's own key → shell in the sandbox ($out)" || fail "own key" "$out"
denied "unregistered key" stranger "$A"
denied "another sandbox's key" b "$A"
denied "unknown sandbox" a "sb-000000000000"
echo "[$GW_HOST]:$GW_PORT $(cut -d' ' -f1,2 "$TMP/a.pub")" >"$TMP/wrong_known_hosts"
out=$(ssh -o UserKnownHostsFile="$TMP/wrong_known_hosts" "${SSH_OPTS[@]}" -i "$TMP/a" -p "$GW_PORT" -n "$A@$GW_HOST" true 2>&1)
grep -q "HOST IDENTIFICATION HAS CHANGED\|Host key verification failed" <<<"$out" &&
	pass "client pinning a different gateway host key → refused" || fail "host key pinning" "$out"

printf "\nsessions\n"
gw_ssh a "$A" -n 'exit 7' 2>/dev/null
rc=$?
[ "$rc" = 7 ] && pass "exit code relayed (7)" || fail "exit code" "got $rc"
out=$(gw_ssh a "$A" -n 'echo out; echo err >&2' 2>"$TMP/err")
[ "$out" = out ] && [ "$(cat "$TMP/err")" = err ] && pass "stdout and stderr kept apart" || fail "stdout/stderr" "$out / $(cat "$TMP/err")"
out=$(echo hello-stdin | gw_ssh a "$A" 'cat' 2>&1)
[ "$out" = hello-stdin ] && pass "stdin relayed" || fail "stdin" "$out"
out=$(printf 'tty\necho $((6*7))\nexit\n' | gw_ssh a "$A" -tt 2>&1 | tr -d '\r')
grep -q "^/dev/pts/" <<<"$out" && grep -q "^42$" <<<"$out" && pass "interactive shell with a terminal (ssh -tt)" || fail "interactive shell" "$out"

printf "\nfiles\n"
head -c 1048576 /dev/urandom >"$TMP/up.bin"
SUM=$(shasum -a 256 "$TMP/up.bin" | cut -d' ' -f1)
SCP=(scp "${SSH_OPTS[@]}" -i "$TMP/a" -P "$GW_PORT")
if "${SCP[@]}" "$TMP/up.bin" "$A@$GW_HOST:up.bin" 2>"$TMP/err" &&
	"${SCP[@]}" "$A@$GW_HOST:up.bin" "$TMP/down.bin" 2>>"$TMP/err" &&
	[ "$(shasum -a 256 "$TMP/down.bin" | cut -d' ' -f1)" = "$SUM" ]; then
	pass "scp 1 MiB up and back, checksum matches"
else
	fail "scp" "$(cat "$TMP/err")"
fi
printf 'mkdir e2e-dir\nput %s e2e-dir/f.bin\nls -l e2e-dir\n' "$TMP/up.bin" >"$TMP/batch"
out=$(sftp "${SSH_OPTS[@]}" -i "$TMP/a" -P "$GW_PORT" -b "$TMP/batch" "$A@$GW_HOST" 2>&1)
rc=$?
[ "$rc" = 0 ] && grep -q "1048576" <<<"$out" && pass "sftp mkdir, put, ls" || fail "sftp" "$out"
out=$(gw_ssh a "$A" -n 'sha256sum e2e-dir/f.bin' 2>&1)
[ "${out%% *}" = "$SUM" ] && pass "file written over sftp is intact in the sandbox" || fail "sftp file" "$out"

printf "\nforwarding\n"
out=$(ssh -o LogLevel=INFO "${SSH_OPTS[@]}" -i "$TMP/a" -p "$GW_PORT" -W "postgres.sandbox-system:5432" "$A@$GW_HOST" </dev/null 2>&1)
rc=$?
[ "$rc" != 0 ] && grep -q "prohibited: only sessions are allowed" <<<"$out" && pass "ssh -W (port forwarding) refused" || fail "port forwarding" "exit $rc: $out"

printf "\nidle timer\n"
# Pretend A has been idle 25 minutes, then use it over SSH only (no API
# exec): the gateway must reset its idle timer, or the reaper would delete
# a sandbox in use 5 minutes later.
idle_secs() { # seconds since last_activity_at
	python3 -c "import datetime,re,sys
t=re.sub(r'(\.\d{6})\d*', r'\1', sys.argv[1]).replace('Z','+00:00')
print(int((datetime.datetime.now(datetime.timezone.utc)-datetime.datetime.fromisoformat(t)).total_seconds()))" "$(api_field "$A" last_activity_at)"
}
# Both kinds of activity: the API's, and the SSH sessions earlier checks opened.
echo "UPDATE sandboxes SET last_activity_at = now() - interval '25 minutes' WHERE id = '$A';
UPDATE ssh_activity SET last_seen_at = now() - interval '25 minutes' WHERE sandbox_id = '$A';" |
	kubectl -n sandbox-system exec -i postgres-0 -- psql -q -v ON_ERROR_STOP=1 -U sandbox -d sandbox >/dev/null ||
	die "could not backdate last_activity_at"
before=$(idle_secs)
gw_ssh a "$A" -n 'sleep 2' >/dev/null 2>&1
after=""
for _ in $(seq 1 10); do # the gateway records activity in the background
	after=$(idle_secs)
	[ "$after" -lt 60 ] && break
	sleep 1
done
[ "$before" -ge 1490 ] && [ "$after" -lt 60 ] &&
	pass "an SSH session resets the idle timer (idle ${before}s → ${after}s)" ||
	fail "idle timer" "idle ${before}s → ${after}s"

printf "\nafter delete\n"
# Positive control first: B's own key does get in while B is running, so the
# refusal below is caused by the delete.
out=$(gw_ssh b "$B" -n 'echo in' 2>&1)
[ "$out" = in ] && pass "sandbox B's own key → in (before delete)" || fail "B's own key before delete" "$out"
curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $KEY" "$API/sandboxes/$B"
for _ in $(seq 1 30); do
	s=$(api_field "$B" status)
	[ "$s" != running ] && break
	sleep 1
done
denied "deleted sandbox (status: $s), its own key" b "$B"

printf "\n%d passed, %d failed\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
