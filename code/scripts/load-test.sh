#!/usr/bin/env bash
# Concurrent sandbox startup load test (step 5.5, D48): creates N sandboxes
# at once and reports p50/p95 time to "running" and to a first exec, then
# deletes them. Uses a dedicated user "load-test" whose quota is raised to N
# (its key is cached per cluster, next to that cluster's admin key: e.g.
# .devenv/state/load-test-key, or .devenv/state/aks/). Results go to
# .devenv/state/tmp/load-<N>-<time>.csv.
#
# Usage (from the project root, with the API deployed):
#   source ./env.sh && ./code/scripts/load-test.sh 10 [ubuntu-base|ubuntu-dev]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
N="${1:-10}"
IMAGE="${2:-ubuntu-base}"
PORT="${LOAD_PORT:-18094}"
# One key per cluster: the local and the AKS databases have different users.
KEYFILE="$(dirname "${SANDBOX_KEYFILE:-$ROOT/.devenv/state/admin-api-key}")/load-test-key"
mkdir -p "$(dirname "$KEYFILE")"

kexec() { kubectl -n sandbox-system exec deploy/sandbox-api -- /api "$@"; }
save_key() { # save_key <output of create-user/create-key>
	local key
	key=$(grep -o 'sk_[A-Za-z0-9_-]*' <<<"$1" | head -1) || true # set -e would exit silently
	[ -n "$key" ] || { echo "no key in: $1" >&2; exit 1; }
	(umask 077 && printf '%s\n' "$key" >"$KEYFILE")
}
# The user exists from here on (create-user fails harmlessly if it does);
# a saved key that no longer works is replaced below once the API is up.
if out=$(kexec create-user --name load-test 2>&1); then
	save_key "$out"
elif ! grep -q "already exists" <<<"$out"; then
	echo "$out" >&2
	exit 1
elif [ ! -s "$KEYFILE" ]; then
	save_key "$(kexec create-key --user load-test --name load 2>&1 || true)"
fi
# Quota = N so the test measures the cluster, not the per-user limit; put
# back afterwards (Phase 5 batch review).
psql_su() { kubectl -n sandbox-system exec -i postgres-0 -- psql -tA -q -v ON_ERROR_STOP=1 -U sandbox -d sandbox; }
OLD_QUOTA=$(echo "SELECT max_sandboxes FROM users WHERE name = 'load-test';" | psql_su)
echo "UPDATE users SET max_sandboxes = $((N)) WHERE name = 'load-test';" | psql_su >/dev/null

TMP="$(mktemp -d "$ROOT/.devenv/state/tmp/load.XXXXXX")"
ssh-keygen -q -t ed25519 -N "" -C load-test -f "$TMP/k"

kubectl -n sandbox-system port-forward svc/sandbox-api "$PORT:80" >/dev/null 2>&1 &
PF=$!
restore_quota() { echo "UPDATE users SET max_sandboxes = $((OLD_QUOTA)) WHERE name = 'load-test';" | psql_su >/dev/null; }
trap 'restore_quota; kill $PF 2>/dev/null; wait $PF 2>/dev/null || true; rm -rf "$TMP"' EXIT
UP=""
for _ in $(seq 1 40); do curl -s -m 1 -o /dev/null "http://127.0.0.1:$PORT/healthz" && { UP=1; break; }; sleep 0.25; done
[ -n "$UP" ] || { echo "API not reachable on 127.0.0.1:$PORT" >&2; exit 1; }
# The key goes to curl in a header file, never on its command line.
(umask 077 && { printf 'Authorization: Bearer '; tr -d '\n' <"$KEYFILE"; echo; } >"$TMP/auth")
if [ "$(curl -s -o /dev/null -w '%{http_code}' -H @"$TMP/auth" "http://127.0.0.1:$PORT/v1/me")" != 200 ]; then
	save_key "$(kexec create-key --user load-test --name load 2>&1 || true)"
fi

PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/code/scripts/loadtest.py" --api "http://127.0.0.1:$PORT/v1" --key-file "$KEYFILE" --pubkey-file "$TMP/k.pub" \
	--n "$N" --image "$IMAGE" --deadline "${LOAD_DEADLINE:-180}" --csv "$ROOT/.devenv/state/tmp/load-$N-$(date +%s).csv"
