#!/usr/bin/env bash
# Deletes everything cloud-up.sh made in Azure (Terraform destroy of the
# resource group with the AKS cluster and registry; the cluster's own
# node resource group goes with it), checks nothing is left, and removes
# the local kubeconfig and admin key for it.
#
# Usage: ./cloud-down.sh
set -euo pipefail
cd "$(dirname "$0")"
NAME=${AZURE_NAME:-sandbox-provider}
# shellcheck source=env.sh
source ./env.sh aks
STATE=.devenv/state/terraform/aks.tfstate
[ -s "$STATE" ] || { echo "no Terraform state ($STATE): nothing to delete"; exit 0; }
az account show >/dev/null 2>&1 ||
	{ echo "not logged in to Azure: run  source ./env.sh && az login  in your Terminal" >&2; exit 1; }
SUB=$(az account show --query id -o tsv)

echo "==> terraform destroy (takes ~5-10 minutes)"
TF=(terraform -chdir=code/deploy/cloud)
"${TF[@]}" init -input=false -backend-config="path=$PWD/$STATE" >/dev/null
# The variables must validate even for a destroy; their values don't matter.
"${TF[@]}" destroy -input=false -auto-approve -var "subscription_id=$SUB" -var "name=$NAME" -var "location=${AZURE_LOCATION:-westus2}" \
	-var 'admin_cidrs=["127.0.0.1/32"]' | grep -E '^(Destroy complete|Error)' || true

echo "==> checking nothing is left"
left=$(az group list --query "[?name=='$NAME' || starts_with(name, 'MC_${NAME}_')].name" -o tsv)
if [ -n "$left" ]; then
	echo "still in Azure (billing may continue): $left" >&2
	echo "delete with: az group delete -n <name> --yes" >&2
	exit 1
fi
rm -f "$KUBECONFIG" "$SANDBOX_KEYFILE"
echo "    no resource groups left for $NAME; local kubeconfig and key removed"
