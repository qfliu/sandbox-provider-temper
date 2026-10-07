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
source ./env.sh local   # only sets variables; the tools come below. Always the local cluster.
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
# .devenv/state/admin-api-key (never printed).
echo "==> admin API key"
KEYFILE=.devenv/state/admin-api-key
# shellcheck source=code/scripts/admin-key.sh
source code/scripts/admin-key.sh
ensure_admin_key "$KEYFILE" up.sh

cat <<MSG

Sandbox provider is up ($(( $(date +%s) - START )) s).
  Walkthrough:  ./demo.sh
  Dashboard:    source ./env.sh && kubectl -n sandbox-system port-forward svc/sandbox-api 18080:80
                then open http://localhost:18080/ui and sign in with the key in $KEYFILE
  Stop:         ./down.sh            (keeps everything; ./up.sh resumes)
  Remove:       ./down.sh --delete   (deletes the VM and all data)
MSG
