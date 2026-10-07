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

# --purge would delete the Azure login and Terraform's state while an AKS
# cluster from cloud-up.sh may still be billing; that cluster could then
# only be removed by hand in the Azure portal.
if [ "${1:-}" = --purge ] && grep -q '"azurerm_kubernetes_cluster"' .devenv/state/terraform/aks.tfstate 2>/dev/null; then
	echo "an Azure cluster from ./cloud-up.sh still exists (it bills until removed): run ./cloud-down.sh first" >&2
	exit 1
fi
if [ -x .devenv/tools/bin/minikube ]; then
	# shellcheck source=env.sh
	source ./env.sh local   # down.sh only manages the local cluster
	if [ -z "${1:-}" ]; then
		./code/scripts/cluster-down.sh
	else
		./code/scripts/cluster-down.sh --delete
		# Keys for the database that went with the VM.
		rm -f .devenv/state/admin-api-key .devenv/state/load-test-key
		rm -rf .devenv/state/testers
	fi
fi
if [ "${1:-}" = --purge ]; then
	rm -rf ./.devenv
	echo "removed ./.devenv"
fi
