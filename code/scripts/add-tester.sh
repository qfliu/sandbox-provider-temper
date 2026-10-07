#!/usr/bin/env bash
# Gives a tester their own account on the current cluster (source ./env.sh
# aks for the Azure one): a user and an API key, written to a 0600 file
# rather than printed, so it can be sent over a channel you trust (a
# password manager share, not chat or email). `--revoke <name>` revokes
# all of that user's keys and sets their quota to 0, ending their access
# (their running sandboxes end at their TTL, or delete them from the admin
# dashboard).
#
# Usage: ./code/scripts/add-tester.sh <name>          (e.g. alice)
#        ./code/scripts/add-tester.sh --revoke <name>
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh (or ./env.sh aks) first}"
psql_su() { kubectl -n sandbox-system exec -i postgres-0 -- psql -tA -q -v ON_ERROR_STOP=1 -U sandbox -d sandbox; }
echo "cluster: ${SANDBOX_TARGET:-local} (kubectl context $(kubectl config current-context 2>/dev/null))"

if [ "${1:-}" = --revoke ]; then
	NAME=${2:?usage: add-tester.sh --revoke <name>}
	[[ "$NAME" =~ ^[a-z0-9][a-z0-9-]{0,30}$ ]] || { echo "name: lowercase letters, digits, dashes" >&2; exit 2; }
	[ "$NAME" != admin ] || { echo "refusing to revoke the operator's own admin user" >&2; exit 2; }
	n=$(printf "UPDATE api_keys SET revoked_at = now() WHERE revoked_at IS NULL AND user_id = (SELECT id FROM users WHERE name = '%s'); UPDATE users SET max_sandboxes = 0 WHERE name = '%s'; SELECT count(*) FROM users WHERE name = '%s';" "$NAME" "$NAME" "$NAME" | psql_su | tail -1)
	[ "$n" = 1 ] || { echo "no user named $NAME" >&2; exit 1; }
	echo "revoked every key of $NAME and set their quota to 0 (their running sandboxes end at their TTL, or delete them from the admin dashboard)"
	exit 0
fi

NAME=${1:?usage: add-tester.sh <name>}
[[ "$NAME" =~ ^[a-z0-9][a-z0-9-]{0,30}$ ]] || { echo "name: lowercase letters, digits, dashes" >&2; exit 2; }
DIR="$(dirname "${SANDBOX_KEYFILE:-$ROOT/.devenv/state/admin-api-key}")/testers"
mkdir -p "$DIR" && chmod 700 "$DIR"
api() { kubectl -n sandbox-system exec deploy/sandbox-api -- /api "$@" 2>&1; }
if ! out=$(api create-user --name "$NAME"); then
	# An existing user (maybe revoked earlier): an additional key, and the
	# default quota back. Their older keys keep working unless revoked.
	grep -q "already exists" <<<"$out" || { echo "$out" >&2; exit 1; }
	out=$(api create-key --user "$NAME" --name tester) || { echo "$out" >&2; exit 1; }
	printf "UPDATE users SET max_sandboxes = 10 WHERE name = '%s' AND max_sandboxes = 0;" "$NAME" | psql_su >/dev/null
fi
key=$(grep -o 'sk_[A-Za-z0-9_-]*' <<<"$out" | head -1) || true
[ -n "$key" ] || { echo "no key in: $out" >&2; exit 1; }
(umask 077 && printf '%s\n' "$key" >"$DIR/$NAME.key")
chmod 600 "$DIR/$NAME.key" # also if the file existed with a looser mode
echo "API key for $NAME written to $DIR/$NAME.key (0600; send it over a channel you trust, then delete the file)"
