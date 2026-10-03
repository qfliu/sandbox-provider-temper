#!/usr/bin/env bash
# One command from a fresh clone to a running sandbox provider on this Mac:
# CLI tools (into ./.devenv, nothing system-wide), a minikube VM with gVisor,
# the sandbox images, the platform (Helm chart) and an admin API key.
# Idempotent: re-run after ./down.sh or a code change.
#
# Usage: ./up.sh        then: ./demo.sh
set -euo pipefail
cd "$(dirname "$0")"
START=$(date +%s)

[ "$(uname -s)" = Darwin ] || { echo "up.sh targets macOS (minikube's vfkit driver)" >&2; exit 1; }
# shellcheck source=env.sh
source ./env.sh   # only sets variables; the tools come below
# Before any download: minikube's VM socket lives in this clone, and macOS
# limits socket paths (cluster-up.sh explains; this is the same check).
SOCK="$MINIKUBE_HOME/.minikube/machines/$MINIKUBE_PROFILE/vfkit.sock"
SOCK_BYTES=$(printf %s "$SOCK" | LC_ALL=C wc -c | tr -d ' ')   # bytes, not characters
[ "$SOCK_BYTES" -le 103 ] || {
	echo "This clone's path is too long for minikube's VM driver (a socket path of $SOCK_BYTES" >&2
	echo "bytes; macOS allows 103). Clone to a shorter path, e.g. ~/sandbox-provider." >&2
	exit 1
}
for c in curl git openssl ssh-keygen python3; do
	command -v "$c" >/dev/null || { echo "missing prerequisite: $c" >&2; exit 1; }
done

if ! [ -x .devenv/tools/bin/minikube ] || ! [ -x .devenv/tools/bin/helm ] || ! [ -x .devenv/tools/go/bin/go ]; then
	./code/scripts/bootstrap-tools.sh
fi

./code/scripts/cluster-up.sh
./code/scripts/deploy.sh

# An admin user and key for the demo and the dashboard, kept in
# .devenv/state/admin-api-key (never printed). A key from an earlier
# cluster whose database is gone is replaced, since the user won't exist.
echo "==> admin API key"
KEYFILE=.devenv/state/admin-api-key
api() { kubectl -n sandbox-system exec deploy/sandbox-api -- /api "$@" 2>&1; }
# key_works: the saved key still authenticates (it may have been revoked,
# or come from another database). The key goes to curl in a header file,
# never on a command line.
key_works() {
	local port=18091 pf hdr code log
	log=$(mktemp "$TMPDIR/pf.XXXXXX")
	# 127.0.0.1 only, so a port already in use fails instead of half-working.
	kubectl -n sandbox-system port-forward --address 127.0.0.1 svc/sandbox-api "$port:80" >"$log" 2>&1 &
	pf=$!
	for _ in $(seq 1 40); do
		grep -q "Forwarding from" "$log" && break
		kill -0 "$pf" 2>/dev/null || break
		sleep 0.25
	done
	rm -f "$log"
	if ! kill -0 "$pf" 2>/dev/null; then
		echo "    (couldn't check the saved key: port $port is in use)"
		return 0
	fi
	hdr=$(mktemp "$TMPDIR/auth.XXXXXX")
	{ printf 'Authorization: Bearer '; tr -d '\n' <"$KEYFILE"; echo; } >"$hdr"
	code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H @"$hdr" "http://127.0.0.1:$port/v1/me")
	rm -f "$hdr"
	kill "$pf" 2>/dev/null
	wait "$pf" 2>/dev/null
	[ "$code" = 200 ]
}
if out=$(api create-user --name admin --admin); then
	:
elif grep -q "already exists" <<<"$out" && [ -s "$KEYFILE" ] && key_works; then
	out=""  # same database, saved key still valid
elif grep -q "already exists" <<<"$out"; then
	out=$(api create-key --user admin --name up.sh) || { echo "$out" >&2; exit 1; }
else
	echo "$out" >&2; exit 1
fi
if [ -n "$out" ]; then
	key=$(grep -o 'sk_[A-Za-z0-9_-]*' <<<"$out" | head -1) || true
	[ -n "$key" ] || { echo "no key in the output of /api (see above)" >&2; exit 1; }
	(umask 077 && printf '%s\n' "$key" >"$KEYFILE")
	echo "    new key saved to $KEYFILE"
else
	echo "    existing key in $KEYFILE"
fi

cat <<MSG

Sandbox provider is up ($(( $(date +%s) - START )) s).
  Walkthrough:  ./demo.sh
  Dashboard:    source ./env.sh && kubectl -n sandbox-system port-forward svc/sandbox-api 18080:80
                then open http://localhost:18080/ui and sign in with the key in $KEYFILE
  Stop:         ./down.sh            (keeps everything; ./up.sh resumes)
  Remove:       ./down.sh --delete   (deletes the VM and all data)
MSG
