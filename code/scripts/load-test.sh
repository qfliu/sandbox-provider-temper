#!/usr/bin/env bash
# Concurrent sandbox startup load test (step 5.5, D48): creates N sandboxes
# at once and reports p50/p95 time to "running" and to a first exec, then
# deletes them. Uses a dedicated user "load-test" whose quota is raised to N
# (its key is cached in .devenv/state/load-test-key). Results go to
# .devenv/state/tmp/load-<N>-<time>.csv.
#
# Usage (from the project root, with the API deployed):
#   source ./env.sh && ./code/scripts/load-test.sh 10 [ubuntu-base|ubuntu-dev]
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
N="${1:-10}"
IMAGE="${2:-ubuntu-base}"
PORT="${LOAD_PORT:-18094}"
KEYFILE="$ROOT/.devenv/state/load-test-key"

kexec() { kubectl -n sandbox-system exec deploy/sandbox-api -- /api "$@"; }
if [ ! -s "$KEYFILE" ]; then
	out=$(kexec create-user --name load-test 2>&1) ||
		out=$(kexec create-key --user load-test --name load 2>&1) || { echo "$out" >&2; exit 1; }
	(umask 077 && printf '%s\n' "$out" | tail -1 >"$KEYFILE")
fi
# Quota = N so the test measures the cluster, not the per-user limit.
echo "UPDATE users SET max_sandboxes = $((N)) WHERE name = 'load-test';" |
	kubectl -n sandbox-system exec -i postgres-0 -- psql -q -v ON_ERROR_STOP=1 -U sandbox -d sandbox >/dev/null

TMP="$(mktemp -d "$ROOT/.devenv/state/tmp/load.XXXXXX")"
ssh-keygen -q -t ed25519 -N "" -C load-test -f "$TMP/k"

kubectl -n sandbox-system port-forward svc/sandbox-api "$PORT:80" >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null; wait $PF 2>/dev/null; rm -rf "$TMP"' EXIT
for _ in $(seq 1 40); do curl -s -m 1 -o /dev/null "http://127.0.0.1:$PORT/healthz" && break; sleep 0.25; done

python3 "$ROOT/code/scripts/loadtest.py" --api "http://127.0.0.1:$PORT/v1" --key-file "$KEYFILE" --pubkey-file "$TMP/k.pub" \
	--n "$N" --image "$IMAGE" --deadline "${LOAD_DEADLINE:-180}" --csv "$ROOT/.devenv/state/tmp/load-$N-$(date +%s).csv"
