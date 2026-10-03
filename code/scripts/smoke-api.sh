#!/usr/bin/env bash
# End-to-end smoke test of the control plane API, run the way an agent uses
# it: against the API deployed in the cluster, over HTTP with an API key.
# Prints PASS/FAIL per check and exits non-zero if anything failed.
#
# It uses a dedicated "smoke-test" user (created on first run) and a second
# throwaway user for the isolation checks, and deletes every sandbox it
# creates.
#
# Usage (from the project root, after cluster-up / deploy-postgres /
# deploy-api):
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
json() { python3 -c "import json,sys; d=json.load(open('$1')); print($2)" 2>/dev/null; }

# req <key> <method> <path> [curl args...] → body in $TMP/out, status in $CODE
req() {
	local key=$1 method=$2 path=$3; shift 3
	CODE=$(curl -s -o "$TMP/out" -w '%{http_code}' -X "$method" ${key:+-H "Authorization: Bearer $key"} "$@" "$API$path")
	LAST="HTTP $CODE $(head -c 200 "$TMP/out")"
}

cleanup() {
	for id in "${SANDBOXES[@]}"; do
		curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $KEY" "$API/sandboxes/$id" || true
	done
	[ -n "${PF:-}" ] && { kill "$PF"; wait "$PF"; } 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT

# --- setup -------------------------------------------------------------------
kubectl -n sandbox-system port-forward svc/sandbox-api "$PORT:80" >/dev/null 2>&1 &
PF=$!
for _ in $(seq 1 40); do curl -s -o /dev/null "http://127.0.0.1:$PORT/healthz" && break; sleep 0.25; done

new_key() { # new_key <user>: an API key for <user>, creating the user if needed
	local out
	out=$(kubectl -n sandbox-system exec deploy/sandbox-api -- /api create-user --name "$1" 2>/dev/null) ||
		out=$(kubectl -n sandbox-system exec deploy/sandbox-api -- /api create-key --user "$1" --name smoke 2>/dev/null)
	printf '%s\n' "$out" | tail -1
}
KEY=$(new_key smoke-test)
OTHER=$(new_key "smoke-other-$(date +%s)")
[[ "$KEY" == sk_* && "$OTHER" == sk_* ]] || { echo "could not get API keys (is the API deployed?)"; exit 1; }
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

# --- validation --------------------------------------------------------------
section "validation"
req "$KEY" POST /sandboxes -d "{\"image\":\"alpine\",\"ssh_public_key\":\"$SSH_PUB\"}"
check "unknown image → 400" test "$CODE" = 400
req "$KEY" POST /sandboxes -d "{\"image\":\"ubuntu-dev\",\"ssh_public_key\":\"$SSH_PUB\",\"ttl_seconds\":10}"
check "TTL below 60 s → 400" test "$CODE" = 400
req "$KEY" POST /sandboxes -d '{"image":"ubuntu-dev","ssh_public_key":"not a key"}'
check "invalid SSH key → 400" test "$CODE" = 400

# --- lifecycle ---------------------------------------------------------------
section "lifecycle"
START=$(date +%s)
req "$KEY" POST /sandboxes -d "{\"image\":\"ubuntu-dev\",\"ssh_public_key\":\"$SSH_PUB\",\"ttl_seconds\":600}"
ID=$(json "$TMP/out" "d['id']")
check "create → 201 with an sb- id" test "$CODE ${ID:0:3}" = "201 sb-"
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
check "runs under gVisor" grep -q gvisor <<<"$(json "$TMP/out" "d['stdout']")"
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
check "download is byte-identical" cmp -s "$TMP/blob" "$TMP/back"
req "$KEY" GET "/sandboxes/$ID/files?path=missing.txt"
check "missing file → 404" test "$CODE" = 404
req "$KEY" PUT "/sandboxes/$ID/files?path=/etc/smoke.txt" --data-binary x
check "write outside user's permissions → 403" test "$CODE" = 403

# --- isolation ---------------------------------------------------------------
section "isolation (another user)"
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
GONE=""
for _ in $(seq 1 30); do
	kubectl -n sandboxes get pod "$ID" >/dev/null 2>&1 || { GONE=yes; break; }
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
