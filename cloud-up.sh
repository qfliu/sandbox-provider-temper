#!/usr/bin/env bash
# Deploys the sandbox provider to Azure (Phase 7, D54): an AKS cluster with
# 2 arm64 nodes and a container registry (Terraform), gVisor on the nodes,
# the platform images, the Helm chart with a public SSH gateway and a public
# HTTPS address for the API and dashboard (Let's Encrypt certificate on an
# Azure DNS name), and an admin API key. Re-run to update (e.g. to change
# the allowlist); ./cloud-down.sh deletes everything. TESTING.md is the
# guide for the people who then test it.
#
# Costs money while it exists (about $0.20/hour: 2 small VMs, a disk, a
# public IP; the AKS control plane is free). Needs: an Azure subscription
# and `source ./env.sh && az login` once, and the local cluster (./up.sh),
# which builds the arm64 images that are pushed to the registry.
#
# Usage: ./cloud-up.sh        then: source ./env.sh aks && ./demo.sh
# Settings (environment): AZURE_LOCATION (westus2), AZURE_NAME
# (sandbox-provider), AZURE_NODE_SIZE (Standard_D2ps_v5), AZURE_NODES (2);
# ALLOWED_CIDRS: comma-separated CIDRs (e.g. testers' office ranges) that
# alone may use the HTTPS API and the SSH gateway, your own IP always added;
# unset or empty: open to anyone, who still needs an API key / sandbox key.
# ACME_EMAIL: optional, for Let's Encrypt notices.
set -euo pipefail
cd "$(dirname "$0")"
START=$(date +%s)
LOCATION=${AZURE_LOCATION:-westus2}
NAME=${AZURE_NAME:-sandbox-provider}
NODE_SIZE=${AZURE_NODE_SIZE:-Standard_D2ps_v5}
NODES=${AZURE_NODES:-2}
NS=sandbox-system

[ -x .devenv/tools/bin/terraform ] && [ -x .devenv/tools/bin/az ] || ./code/scripts/bootstrap-cloud-tools.sh
# shellcheck source=env.sh
source ./env.sh aks
mkdir -p "$(dirname "$KUBECONFIG")" "$(dirname "$SANDBOX_KEYFILE")" .devenv/state/terraform
# minikube (the local cluster that builds the images) with its own
# kubeconfig: with KUBECONFIG on AKS, `minikube status` reports
# "Misconfigured" and fails.
mk() { KUBECONFIG="$_SB_STATE/kube/config" minikube "$@"; }
# After Terraform has created resources, any failure leaves them billing:
# say so, with the way out.
CREATED=""
trap 'rc=$?; if [ "$rc" != 0 ] && [ -n "$CREATED" ]; then
	echo "cloud-up.sh failed after creating Azure resources; they cost money until removed: ./cloud-down.sh" >&2
fi' EXIT

echo "==> checks"
az account show >/dev/null 2>&1 ||
	{ echo "not logged in to Azure: run  source ./env.sh && az login  in your Terminal" >&2; exit 1; }
SUB=$(az account show --query id -o tsv)
mk status >/dev/null 2>&1 ||
	{ echo "the local cluster isn't running (it builds the images): run ./up.sh first" >&2; exit 1; }
MYIP=$(curl -fsS https://api.ipify.org)
echo "    subscription $SUB, region $LOCATION, Kubernetes API open to $MYIP only"
# A new subscription has none of these enabled, and until Compute is, the
# size and quota lists below come back empty. Registering is free.
for ns in Microsoft.ContainerService Microsoft.ContainerRegistry Microsoft.Compute Microsoft.Network Microsoft.Storage; do
	az provider register --namespace "$ns" --wait >/dev/null
done
# The node size must be offered to this subscription in the region, and the
# region's vCPU quota (overall and for the size's family) must fit the
# nodes. Free-trial subscriptions often allow 4 vCPUs per region.
az vm list-skus -l "$LOCATION" --size "$NODE_SIZE" --resource-type virtualMachines -o json >"$TMPDIR/sku.json"
SKU=$(python3 - "$NODE_SIZE" "$TMPDIR/sku.json" <<'PY'
import json, sys
size, path = sys.argv[1], sys.argv[2]
skus = [s for s in json.load(open(path)) if s["name"] == size]
if not skus:
    print("- 0 missing"); sys.exit()
s = skus[0]
vcpus = next((c["value"] for c in s.get("capabilities", []) if c["name"] == "vCPUs"), "0")
# Only a region-wide restriction stops us; zone-only ones (type "Zone")
# don't matter to a cluster that doesn't pin zones.
region = [r for r in s.get("restrictions") or [] if r.get("type") == "Location"]
print(s["family"], vcpus, "restricted" if region else "ok")
PY
) || { echo "couldn't read the size list for $LOCATION" >&2; exit 1; }
read -r FAMILY VCPUS RESTRICTED <<<"$SKU"
case "$RESTRICTED" in
missing) echo "$NODE_SIZE isn't offered in $LOCATION; set AZURE_LOCATION or AZURE_NODE_SIZE" >&2; exit 1 ;;
restricted) echo "$NODE_SIZE is restricted for this subscription in $LOCATION; try another AZURE_LOCATION" >&2; exit 1 ;;
esac
NEED=$((VCPUS * NODES))
if [ ! -s .devenv/state/terraform/aks.tfstate ] || ! grep -q '"azurerm_kubernetes_cluster"' .devenv/state/terraform/aks.tfstate; then
	az vm list-usage -l "$LOCATION" -o json >"$TMPDIR/usage.json"
	python3 - "$TMPDIR/usage.json" "$FAMILY" "$NEED" <<'PY' || exit 1
import json, sys
path, family, need = sys.argv[1], sys.argv[2], int(sys.argv[3])
usage = {u["name"]["value"]: u for u in json.load(open(path))}
bad = []
for key, label in (("cores", "total regional vCPUs"), (family, family + " vCPUs")):
    u = usage.get(key)
    if u is None:
        bad.append(f"no quota data for {label}")
        continue
    free = int(u["limit"]) - int(u["currentValue"])
    if free < need:
        bad.append(f"{label}: {free} free, {need} needed")
if bad:
    print("quota too small: " + "; ".join(bad) + ". Request more in the Azure portal (Quotas) or set AZURE_NODES / AZURE_NODE_SIZE.", file=sys.stderr)
    sys.exit(1)
PY
fi

echo "==> terraform apply (AKS + registry; the first run takes ~5-10 minutes)"
TF=(terraform -chdir=code/deploy/cloud)
"${TF[@]}" init -input=false -backend-config="path=$PWD/.devenv/state/terraform/aks.tfstate" >/dev/null
CREATED=yes # from here on, a failure may leave resources behind
if ! "${TF[@]}" apply -input=false -auto-approve -no-color \
	-var "subscription_id=$SUB" -var "location=$LOCATION" -var "name=$NAME" \
	-var "node_size=$NODE_SIZE" -var "node_count=$NODES" -var "admin_cidrs=[\"$MYIP/32\"]" \
	>"$TMPDIR/tf-apply.log" 2>&1; then
	tail -40 "$TMPDIR/tf-apply.log" >&2
	echo "terraform apply failed (full log: $TMPDIR/tf-apply.log)" >&2
	exit 1
fi
grep -E '^Apply complete' "$TMPDIR/tf-apply.log" | sed 's/^/    /' || true
RG=$("${TF[@]}" output -raw resource_group)
CLUSTER=$("${TF[@]}" output -raw cluster_name)
ACR=$("${TF[@]}" output -raw acr_login_server)
ACR_NAME=$("${TF[@]}" output -raw acr_name)
az aks get-credentials -g "$RG" -n "$CLUSTER" --file "$KUBECONFIG" --overwrite-existing >/dev/null
kubectl get nodes -o wide | cut -c1-100

echo "==> gVisor on the nodes"
kubectl apply -f code/deploy/cloud/gvisor-installer.yaml >/dev/null
kubectl -n gvisor-system rollout status ds/gvisor-installer --timeout=10m >/dev/null

echo "==> images: build on the local cluster (arm64), push to $ACR"
DIRTY="$(git diff --quiet HEAD -- code || echo -dirty)"
TAG="$(git rev-parse --short HEAD)${DIRTY}-$(date +%s)"
mk image build -t "$ACR/sandbox/api:$TAG" code >"$TMPDIR/cloud-build.log" 2>&1 ||
	{ tail -30 "$TMPDIR/cloud-build.log"; echo "platform image build failed" >&2; exit 1; }
# The registry token reaches ctr on the minikube node through stdin, never
# on a command line on this Mac. Inside the VM it is in ctr's arguments
# while a push runs (a single-user local VM). It lasts about 3 hours and is
# good only for this registry.
TOKEN=$(az acr login -n "$ACR_NAME" --expose-token --query accessToken -o tsv --only-show-errors) ||
	{ echo "couldn't get a token for the registry $ACR_NAME" >&2; exit 1; }
push() { # push <local ref> <registry ref>
	local out
	out=$(printf '%s\n' "$TOKEN" | mk ssh -- "read -r T; sudo ctr -n k8s.io images tag --force $1 $2 >/dev/null &&
		sudo ctr -n k8s.io images push --user 00000000-0000-0000-0000-000000000000:\$T $2 >/dev/null && echo pushed-ok" 2>&1 |
		tr -d '\r') || true
	if ! grep -q pushed-ok <<<"$out"; then
		echo "push of $2 failed: $(tail -3 <<<"$out")" >&2
		echo "(if it says 'content digest … not found', rebuild the local images: ./up.sh)" >&2
		return 1
	fi
	echo "    $2"
}
push "$ACR/sandbox/api:$TAG" "$ACR/sandbox/api:$TAG"
for img in ubuntu-base ubuntu-dev; do
	push "docker.io/sandbox/$img:0.1" "$ACR/sandbox/$img:0.1"
done
unset TOKEN

echo "==> helm upgrade --install sandbox"
# The public HTTPS address: an Azure DNS name on the API's load balancer,
# <label>.<region>.cloudapp.azure.com, stable for this subscription.
LABEL="sandbox-$(printf %s "$SUB" | shasum | cut -c1-8)"
HOST="$LABEL.$LOCATION.cloudapp.azure.com"
HELM=(helm upgrade --install sandbox code/deploy/helm -n "$NS" --create-namespace -f code/deploy/cloud/values-aks.yaml
	--set "image.repository=$ACR/sandbox/api" --set "image.tag=$TAG" --set "sandboxImages.repository=$ACR/sandbox/"
	--set "publicApi.host=$HOST" --set-string "publicApi.service.annotations.service\.beta\.kubernetes\.io/azure-dns-label-name=$LABEL")
[ -z "${ACME_EMAIL:-}" ] || HELM+=(--set "publicApi.acmeEmail=$ACME_EMAIL")
ALLOWED=""
if [ -n "${ALLOWED_CIDRS:-}" ]; then
	ALLOWED="$MYIP/32,${ALLOWED_CIDRS// /}"
	HELM+=(--set "publicApi.allowedSourceRanges={$ALLOWED}" --set "gateway.service.loadBalancerSourceRanges={$ALLOWED}")
fi
GW=$(kubectl -n "$NS" get svc sandbox-gateway-nodeport -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
"${HELM[@]}" ${GW:+--set "api.sshGatewayAddr=$GW:22"} --timeout 15m >/dev/null
if [ -z "$GW" ]; then
	# The load balancer's IP exists only once its Service does; then the
	# dashboard learns the address.
	for _ in $(seq 1 60); do
		GW=$(kubectl -n "$NS" get svc sandbox-gateway-nodeport -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
		[ -n "$GW" ] && break
		sleep 5
	done
	[ -n "$GW" ] || { echo "the gateway's load balancer got no IP" >&2; exit 1; }
	"${HELM[@]}" --set "api.sshGatewayAddr=$GW:22" --timeout 15m >/dev/null
fi
for r in statefulset/postgres deployment/sandbox-api deployment/sandbox-gateway daemonset/sandbox-image-prepull deployment/public-api; do
	kubectl -n "$NS" rollout status "$r" --timeout=10m >/dev/null
done
echo "==> https://$HOST (waiting for DNS and the Let's Encrypt certificate)"
# curl checks the certificate: success means a real, trusted certificate.
for _ in $(seq 1 60); do
	curl -fsS -m 5 -o /dev/null "https://$HOST/healthz" 2>/dev/null && break
	sleep 5
done
curl -fsS -m 5 -o /dev/null "https://$HOST/healthz" ||
	{ echo "https://$HOST isn't answering with a valid certificate yet (kubectl -n $NS logs deploy/public-api)" >&2; exit 1; }

echo "==> admin API key"
# shellcheck source=code/scripts/admin-key.sh
source code/scripts/admin-key.sh
ensure_admin_key "$SANDBOX_KEYFILE" cloud-up.sh

cat <<MSG

Sandbox provider is up on AKS ($(( $(date +%s) - START )) s): cluster $CLUSTER in $LOCATION.
  API:         https://$HOST/v1     Dashboard: https://$HOST/ui
  SSH:         ssh <sandbox-id>@$GW
  Allowed:     ${ALLOWED:-anyone with a key (set ALLOWED_CIDRS and re-run to restrict)}
  Your key:    $SANDBOX_KEYFILE
  Testers:     ./code/scripts/add-tester.sh <name>, then send them TESTING.md, the URL and their key
  Use it here: source ./env.sh aks    (kubectl, demo.sh and the test scripts then target AKS)
  Costs about \$0.20/hour while it exists. Remove it all: ./cloud-down.sh
MSG
