#!/usr/bin/env bash
# Stops or removes the local sandbox provider.
#   ./down.sh            stop the minikube VM (frees CPU/RAM; state is kept and
#                        ./up.sh resumes in about a minute)
#   ./down.sh --delete   delete the VM, its disk and all data (sandboxes,
#                        users, keys); ./up.sh then starts from scratch
#   ./down.sh --purge    --delete, then also remove ./.devenv (downloaded
#                        tools, caches, keys): the clone is back to as cloned
set -euo pipefail
cd "$(dirname "$0")"
case "${1:-}" in
"" | --delete | --purge) ;;
*) echo "usage: ./down.sh [--delete | --purge]" >&2; exit 2 ;;
esac

if [ -x .devenv/tools/bin/minikube ]; then
	# shellcheck source=env.sh
	source ./env.sh
	if [ -z "${1:-}" ]; then
		./code/scripts/cluster-down.sh
	else
		./code/scripts/cluster-down.sh --delete
		rm -f .devenv/state/admin-api-key   # its user went with the database
	fi
fi
if [ "${1:-}" = --purge ]; then
	rm -rf ./.devenv
	echo "removed ./.devenv"
fi
