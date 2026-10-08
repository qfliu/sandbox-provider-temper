#!/usr/bin/env bash
# End-to-end smoke test of the control plane API, run the way an agent uses
# it: against the API deployed in the cluster, over HTTP with an API key.
# Prints PASS/FAIL per check and exits non-zero if anything failed.
#
# It uses two fixed users, "smoke-test" and "smoke-other" (for the isolation
# checks), created on first run. Their API keys are cached in
# .devenv/state/smoke-keys/ and reused while valid, so repeated runs don't
# pile up users or keys. Every sandbox it creates is deleted.
#
# Usage (from the project root, after cluster-up.sh and deploy.sh):
#   source ./env.sh && ./code/scripts/smoke-api.sh
set -uo pipefail   # no -e: a failed check is reported, not fatal

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
PORT="${SMOKE_PORT:-18090}"
API="http://127.0.0.1:$PORT/v1"
TMP="$(mktemp -d "$ROOT/.devenv/state/tmp/smoke.XXXXXX")"
PASSED=0 FAILED=0 SANDBOXES=()

pass() { PASSED=$((PASSED + 1)); printf "  \033[32mPASS\033[0m  %s\n" "$1"; }
fail() { FAILED=$((FAILED + 1)); printf "  \033[31mFAIL\033[0m  %s\n        %s\n" "$1" "${2:-}"; }
check() { # check "<name>" <condition...>
	local name=$1; shift
	if "$@"; then pass "$name"; else fail "$name" "got: ${LAST:-}"; fi
}
section() { printf "\n%s\n" "$1"; }

# json <file> <python expression over d>, e.g. json out.json "d['status']"
# (the path is passed as an argument, never pasted into the Python source)
json() { python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print($2)" "$1" 2>/dev/null; }

# req <key> <method> <path> [curl args...] → body in $TMP/out, status in $CODE
req() {
	local key=$1 method=$2 path=$3; shift 3
	CODE=$(curl -s -o "$TMP/out" -w '%{http_code}' -X "$method" ${key:+-H "Authorization: Bearer $key"} "$@" "$API$path")
	LAST="HTTP $CODE $(head -c 200 "$TMP/out")"
}

cleanup() {
	# ${arr[@]+...}: macOS's bash 3.2 treats an empty array as unset under
	# set -u, which would abort cleanup and leak the port-forward/temp dir.
	for id in ${SANDBOXES[@]+"${SANDBOXES[@]}"}; do
		curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $KEY" "$API/sandboxes/$id" || true
	done
	[ -n "${PF:-}" ] && { kill "$PF"; wait "$PF"; } 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT

# --- setup -------------------------------------------------------------------
die() { printf "setup failed: %s\n" "$1" >&2; exit 1; }

# Fail fast (instead of minute-long kubectl waits and 27 confusing failures)
# if the API isn't deployed and ready.
READY=$(kubectl -n sandbox-system get deploy sandbox-api -o jsonpath='{.status.readyReplicas}' 2>&1) ||
	die "can't read deployment sandbox-api: $READY (run deploy.sh)"
[ "${READY:-0}" -ge 1 ] 2>/dev/null || die "sandbox-api has no ready pods (run deploy.sh)"

kubectl -n sandbox-system port-forward svc/sandbox-api "$PORT:80" >/dev/null 2>&1 &
PF=$!
UP=""
DEADLINE=$((SECONDS + 10)) # total wait, however long each probe takes
while [ "$SECONDS" -lt "$DEADLINE" ]; do
	curl -s -m 1 -o /dev/null "http://127.0.0.1:$PORT/healthz" && { UP=yes; break; }
	kill -0 "$PF" 2>/dev/null || break # port-forward died (e.g. port $PORT in use)
	sleep 0.25
done
[ -n "$UP" ] || die "API not reachable on 127.0.0.1:$PORT (port in use? set SMOKE_PORT)"

kexec() { kubectl -n sandbox-system exec --pod-running-timeout=10s deploy/sandbox-api -- /api "$@"; }

new_key() { # new_key <user>: an API key for <user>, creating the user if needed
	local out
	if ! out=$(kexec create-user --name "$1" 2>&1); then
		grep -q "already exists" <<<"$out" || { echo "create-user $1: $out" >&2; return 1; }
		out=$(kexec create-key --user "$1" --name smoke 2>&1) || { echo "create-key $1: $out" >&2; return 1; }
	fi
	printf '%s\n' "$out" | tail -1
}
# cached_key <user>: reuse the cached key if the API still accepts it,
# otherwise get a new one and cache it.
KEYDIR="$ROOT/.devenv/state/smoke-keys"
cached_key() {
	local f="$KEYDIR/$1" key
	if [ -s "$f" ]; then
		key=$(cat "$f")
		[ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $key" "$API/me")" = 200 ] &&
			{ printf '%s\n' "$key"; return; }
	fi
	key=$(new_key "$1") || return 1
	(umask 077 && mkdir -p "$KEYDIR" && printf '%s\n' "$key" >"$f")
	printf '%s\n' "$key"
}
KEY=$(cached_key smoke-test) && OTHER=$(cached_key smoke-other) ||
	die "could not get API keys (see the error above)"
[[ "$KEY" == sk_* && "$OTHER" == sk_* ]] || die "unexpected key output"
ssh-keygen -q -t ed25519 -N "" -C smoke -f "$TMP/id"
SSH_PUB=$(cat "$TMP/id.pub")
printf "smoke test against %s (user smoke-test)\n" "$API"

# --- auth --------------------------------------------------------------------
section "auth"
req "" GET /me
check "no API key → 401" test "$CODE" = 401
req "sk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" GET /me
check "unknown API key → 401" test "$CODE" = 401
req "$KEY" GET /me
check "valid key → 200, user smoke-test" test "$CODE $(json "$TMP/out" "d['name']")" = "200 smoke-test"

# SANDBOX_RUNTIME=kata runs the sandbox(es) under Kata (D65); default: the
# cluster's default runtime (gVisor).
RT=${SANDBOX_RUNTIME:+,\"runtime\":\"$SANDBOX_RUNTIME\"}

# --- validation --------------------------------------------------------------
section "validation"
# Match the error message too, so an unrelated 400 (e.g. a malformed
# body) can't pass for the validation being tested.
err_is() { test "$CODE" = 400 && grep -q "$1" "$TMP/out"; }
req "$KEY" POST /sandboxes -d "{\"image\":\"alpine\",\"ssh_public_key\":\"$SSH_PUB\"}"
check "unknown image → 400" err_is "image must be one of"
req "$KEY" POST /sandboxes -d "{\"image\":\"ubuntu-dev\",\"ssh_public_key\":\"$SSH_PUB\",\"ttl_seconds\":10}"
check "TTL below 60 s → 400" err_is "ttl_seconds must be between"
req "$KEY" POST /sandboxes -d '{"image":"ubuntu-dev","ssh_public_key":"not a key"}'
check "invalid SSH key → 400" err_is "ssh_public_key must be"
req "$KEY" POST /sandboxes -d "{\"image\":\"ubuntu-dev\",\"ssh_public_key\":\"$SSH_PUB\",\"runtime\":\"runc\"}"
check "unknown runtime → 400" err_is "runtime must be one of"
# Kata is offered only where it's installed (D65): created where the kata
# RuntimeClass exists, refused with a clear message elsewhere (a laptop).
req "$KEY" POST /sandboxes -d "{\"image\":\"ubuntu-base\",\"ssh_public_key\":\"$SSH_PUB\",\"runtime\":\"kata\"}"
KID=$(json "$TMP/out" "d.get('id', '')")
[ -n "$KID" ] && SANDBOXES+=("$KID")
if kubectl get runtimeclass kata >/dev/null 2>&1; then
	check "runtime kata → 201 (Kata is installed here)" test "$CODE $(json "$TMP/out" "d.get('runtime', '')")" = "201 kata"
else
	check "runtime kata → 400 \"isn't available\" (no Kata here)" err_is "isn.t available on this cluster"
fi

# --- lifecycle ---------------------------------------------------------------
section "lifecycle"
START=$(date +%s)
req "$KEY" POST /sandboxes -d "{\"image\":\"ubuntu-dev\",\"ssh_public_key\":\"$SSH_PUB\",\"ttl_seconds\":600$RT}"
ID=$(json "$TMP/out" "d['id']")
check "create → 201 with an sb- id" test "$CODE ${ID:0:3}" = "201 sb-"
check "runtime is ${SANDBOX_RUNTIME:-gvisor}" test "$(json "$TMP/out" "d['runtime']")" = "${SANDBOX_RUNTIME:-gvisor}"
[ -n "$ID" ] && SANDBOXES+=("$ID")

STATUS=""
for _ in $(seq 1 60); do
	req "$KEY" GET "/sandboxes/$ID"
	STATUS=$(json "$TMP/out" "d['status']")
	[ "$STATUS" = running ] && break
	sleep 1
done
check "becomes running (in $(( $(date +%s) - START )) s)" test "$STATUS" = running
req "$KEY" GET /sandboxes
check "listed in GET /sandboxes" test "$(json "$TMP/out" "any(s['id']=='$ID' for s in d['sandboxes'])")" = True

# --- exec --------------------------------------------------------------------
section "exec"
exec_cmd() { req "$KEY" POST "/sandboxes/$ID/exec" -d "$1"; }
exec_cmd '{"command":"uname -r"}'
KERNEL=$(json "$TMP/out" "d['stdout'].strip()")
if [ "${SANDBOX_RUNTIME:-gvisor}" = kata ]; then
	# Its own VM's kernel: not gVisor's, not the node's.
	NODE=$(kubectl -n sandboxes get pod "$ID" -o jsonpath='{.spec.nodeName}')
	NODE_KERNEL=$(kubectl get node "$NODE" -o jsonpath='{.status.nodeInfo.kernelVersion}')
	check "runs in its own VM (kernel $KERNEL, node $NODE_KERNEL)" eval '[[ -n "$NODE_KERNEL" && $KERNEL =~ ^[0-9]+\.[0-9]+ && "$KERNEL" != *gvisor* && "$KERNEL" != "$NODE_KERNEL" ]]'
else
	check "runs under gVisor" grep -q gvisor <<<"$KERNEL"
fi
exec_cmd '{"command":"python3 -c \"print(6*7)\""}'
check "python3 → 42" test "$(json "$TMP/out" "d['stdout'].strip()")" = 42
exec_cmd '{"command":"echo oops >&2; exit 3"}'
check "exit code + stderr" test "$(json "$TMP/out" "str(d['exit_code'])+'|'+d['stderr'].strip()")" = "3|oops"
exec_cmd '{"command":"pwd","workdir":"/tmp"}'
check "workdir" test "$(json "$TMP/out" "d['stdout'].strip()")" = /tmp
exec_cmd '{"command":"sleep 30","timeout_seconds":2}'
check "timeout kills the command" test "$(json "$TMP/out" "str(d['timed_out'])+'|'+str(d['exit_code'])")" = "True|124"
exec_cmd '{"command":"sudo -n true && echo ok"}'
check "sudo works" test "$(json "$TMP/out" "d['stdout'].strip()")" = ok

# --- files -------------------------------------------------------------------
section "files"
head -c 1048576 /dev/urandom >"$TMP/blob"
LOCAL_SHA=$(shasum -a 256 "$TMP/blob" | cut -d' ' -f1)
req "$KEY" PUT "/sandboxes/$ID/files?path=smoke/blob.bin" --data-binary @"$TMP/blob"
check "upload 1 MiB → 200" test "$CODE $(json "$TMP/out" "d['size']")" = "200 1048576"
exec_cmd '{"command":"sha256sum smoke/blob.bin | cut -d\" \" -f1"}'
check "sha256 inside matches" test "$(json "$TMP/out" "d['stdout'].strip()")" = "$LOCAL_SHA"
CODE=$(curl -s -o "$TMP/back" -w '%{http_code}' -H "Authorization: Bearer $KEY" "$API/sandboxes/$ID/files?path=/home/user/smoke/blob.bin")
LAST="HTTP $CODE, $(wc -c <"$TMP/back" | tr -d ' ') bytes"
check "download → 200, byte-identical" eval 'test "$CODE" = 200 && cmp -s "$TMP/blob" "$TMP/back"'
req "$KEY" GET "/sandboxes/$ID/files?path=missing.txt"
check "missing file → 404" test "$CODE" = 404
req "$KEY" PUT "/sandboxes/$ID/files?path=/etc/smoke.txt" --data-binary x
check "write outside user's permissions → 403" test "$CODE" = 403

# --- isolation ---------------------------------------------------------------
section "isolation (another user)"
# Precondition: the sandbox exists and its owner can see it. Without this,
# an empty $ID would hit a non-existent route and the 404 checks below
# would pass without testing anything.
req "$KEY" GET "/sandboxes/$ID"
check "precondition: owner sees the sandbox" test -n "$ID" -a "$CODE" = 200
req "$OTHER" GET "/sandboxes/$ID"
check "can't see it → 404" test "$CODE" = 404
req "$OTHER" POST "/sandboxes/$ID/exec" -d '{"command":"id"}'
check "can't exec in it → 404" test "$CODE" = 404
req "$OTHER" DELETE "/sandboxes/$ID"
check "can't delete it → 404" test "$CODE" = 404

# --- delete ------------------------------------------------------------------
section "delete"
req "$KEY" DELETE "/sandboxes/$ID"
check "delete → deleted, reason recorded" test "$CODE $(json "$TMP/out" "d['status']+'|'+d['status_reason']")" = "200 deleted|deleted by owner"
# kubectl must succeed and find no pod: a kubectl error (bad kubeconfig,
# API server down) must not count as "removed".
GONE=""
for _ in $(seq 1 30); do
	PODS=$(kubectl -n sandboxes get pod "$ID" --ignore-not-found -o name 2>&1) && [ -z "$PODS" ] && { GONE=yes; break; }
	LAST="kubectl: ${PODS:-pod still present}"
	sleep 1
done
check "pod removed from the cluster" test "$GONE" = yes
exec_cmd '{"command":"true"}'
check "exec on a deleted sandbox → 409" test "$CODE" = 409
req "$KEY" DELETE "/sandboxes/$ID"
check "deleting again is harmless → 200" test "$CODE" = 200

# --- result ------------------------------------------------------------------
printf "\n%d passed, %d failed\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
