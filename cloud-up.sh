#!/usr/bin/env bash
# Deploys the sandbox provider to Azure (Phase 7, D54): an AKS cluster with
# 2 arm64 nodes and a container registry (Terraform), gVisor on the nodes,
# the platform images, the Helm chart with a public SSH gateway and a public
# HTTPS address for the API and dashboard (Let's Encrypt certificate on an
# Azure DNS name), and an admin API key. Re-run to deploy new code (this
# restarts the API and gateway, ending SSH sessions); change only who may
# connect with --allowlist-only. ./cloud-down.sh deletes everything.
# TESTING.md is the guide for the people who then test it.
#
# Costs money while it exists (about $0.20/hour: 2 small VMs, a disk, a
# public IP; the AKS control plane is free). Needs: an Azure subscription
# and `source ./env.sh && az login` once, and the local cluster (./up.sh),
# which builds the arm64 images that are pushed to the registry.
#
# Usage: ./cloud-up.sh                    then: source ./env.sh aks && ./demo.sh
#        ALLOWED_CIDRS=... ./cloud-up.sh --allowlist-only
#                                        only changes who may connect: no
#                                        image build or rollout, so testers'
#                                        sessions keep running
# Settings (environment): AZURE_LOCATION (westus2), AZURE_NAME
# (sandbox-provider), AZURE_NODE_SIZE (Standard_D2ps_v5), AZURE_NODES (2);
# ALLOWED_CIDRS: comma-separated CIDRs (e.g. testers' office ranges) that
# alone may use the HTTPS API and the SSH gateway, your own IP always added;
# empty or "none": open to anyone, who still needs an API key / sandbox key.
# Settings are remembered (.devenv/state/aks/settings): a re-run without
# them keeps the last values, and changing one that would replace an
# existing cluster (region, name, node size or count) is refused.
# ACME_EMAIL: optional, for Let's Encrypt notices. AZURE_DNS_LABEL: the
# name's first part (default sandbox-<8 hex of the subscription>, stable);
# Let's Encrypt issues at most 5 certificates a week for one name and every
# cloud-up after a cloud-down needs a new one, so past that use another.
set -euo pipefail
cd "$(dirname "$0")"
START=$(date +%s)
NS=sandbox-system
MODE=full
case "${1:-}" in
"") ;;
--allowlist-only) MODE=allowlist ;;
*) echo "usage: ./cloud-up.sh [--allowlist-only]" >&2; exit 2 ;;
esac

[ -x .devenv/tools/bin/terraform ] && [ -x .devenv/tools/bin/az ] || ./code/scripts/bootstrap-cloud-tools.sh
# shellcheck source=env.sh
source ./env.sh aks
mkdir -p "$(dirname "$KUBECONFIG")" "$(dirname "$SANDBOX_KEYFILE")" .devenv/state/terraform
# Terraform's state holds the cluster's admin credentials; keys live here.
chmod 700 .devenv/state/terraform "$(dirname "$SANDBOX_KEYFILE")"

# Remembered settings. EXISTS: a cluster from an earlier run is still up.
SETTINGS=.devenv/state/aks/settings
saved() { [ -f "$SETTINGS" ] && sed -n "s/^$1=//p" "$SETTINGS" | tail -1 || true; }
EXISTS=""
grep -q '"azurerm_kubernetes_cluster"' .devenv/state/terraform/aks.tfstate 2>/dev/null && EXISTS=yes
# setting <key> <env var> <default> <why>: the variable if set, else the
# remembered value, else the default. With a cluster up, a different value is
# refused, for <why>.
setting() {
	local key=$1 var=$2 def=$3 why=$4 env=${!2:-} old
	old=$(saved "$key")
	if [ -n "$EXISTS" ] && [ -n "$env" ] && [ -n "$old" ] && [ "$env" != "$old" ]; then
		echo "$var=$env, but the running cluster has $old: $why." >&2
		echo "Run ./cloud-down.sh first, or leave $var unset." >&2
		return 1
	fi
	printf '%s' "${env:-${old:-$def}}"
}
REPLACE="changing it would make Terraform replace the cluster and delete its data"
RESIZE="cloud-up.sh doesn't resize a running cluster (its quota check only runs before the first apply)"
LOCATION=$(setting LOCATION AZURE_LOCATION westus2 "$REPLACE")
NAME=$(setting NAME AZURE_NAME sandbox-provider "$REPLACE")
NODE_SIZE=$(setting NODE_SIZE AZURE_NODE_SIZE Standard_D2ps_v5 "$RESIZE")
NODES=$(setting NODES AZURE_NODES 2 "$RESIZE")
# Who may connect: as given (empty or "none": anyone), else as last time.
if [ "${ALLOWED_CIDRS+set}" = set ]; then CIDRS=$ALLOWED_CIDRS; else CIDRS=$(saved ALLOWED_CIDRS); fi
[ "$CIDRS" != none ] || CIDRS=""
ACME_EMAIL=${ACME_EMAIL:-$(saved ACME_EMAIL)}
save_settings() {
	(umask 077 && printf 'LOCATION=%s\nNAME=%s\nNODE_SIZE=%s\nNODES=%s\nALLOWED_CIDRS=%s\nLABEL=%s\nACME_EMAIL=%s\n' \
		"$LOCATION" "$NAME" "$NODE_SIZE" "$NODES" "$CIDRS" "$LABEL" "$ACME_EMAIL" >"$SETTINGS")
}
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
MYIP=$(curl -fsS https://api.ipify.org)
LABEL=${AZURE_DNS_LABEL:-$(saved LABEL)}
LABEL=${LABEL:-sandbox-$(printf %s "$SUB" | shasum | cut -c1-8)}
ALLOWED=""
[ -z "$CIDRS" ] || ALLOWED="$MYIP/32,${CIDRS// /}"

if [ "$MODE" = allowlist ]; then
	# Only the allowlists change: the proxy reloads its config and Azure
	# updates the gateway load balancer's rules; nothing else restarts.
	[ -n "$EXISTS" ] || { echo "no cluster from ./cloud-up.sh is running; run ./cloud-up.sh first" >&2; exit 1; }
	echo "==> allowlist: ${ALLOWED:-anyone}"
	helm upgrade sandbox code/deploy/helm -n "$NS" --reset-then-reuse-values --timeout 10m \
		--set "publicApi.allowedSourceRanges={$ALLOWED}" --set "gateway.service.loadBalancerSourceRanges={$ALLOWED}" >/dev/null
	kubectl -n "$NS" rollout status deploy/public-api --timeout=5m >/dev/null
	save_settings
	echo "    done (API and SSH gateway sessions were not restarted)"
	exit 0
fi
mk status >/dev/null 2>&1 ||
	{ echo "the local cluster isn't running (it builds the images): run ./up.sh first" >&2; exit 1; }
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
HOST="$LABEL.$LOCATION.cloudapp.azure.com"
HELM=(helm upgrade --install sandbox code/deploy/helm -n "$NS" --create-namespace -f code/deploy/cloud/values-aks.yaml
	--set "image.repository=$ACR/sandbox/api" --set "image.tag=$TAG" --set "sandboxImages.repository=$ACR/sandbox/"
	--set "publicApi.host=$HOST" --set-string "publicApi.service.annotations.service\.beta\.kubernetes\.io/azure-dns-label-name=$LABEL")
[ -z "${ACME_EMAIL:-}" ] || HELM+=(--set "publicApi.acmeEmail=$ACME_EMAIL")
HELM+=(--set "publicApi.allowedSourceRanges={$ALLOWED}" --set "gateway.service.loadBalancerSourceRanges={$ALLOWED}")
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

save_settings
echo "==> admin API key"
# shellcheck source=code/scripts/admin-key.sh
source code/scripts/admin-key.sh
ensure_admin_key "$SANDBOX_KEYFILE" cloud-up.sh

echo "==> https://$HOST (waiting for DNS and the Let's Encrypt certificate)"
# Caddy asks for the certificate as soon as it starts, which can be before
# Azure publishes the DNS name; its retries then back off for minutes. So:
# wait for the name to resolve, restart Caddy once if the certificate isn't
# there yet (an immediate retry; the certificate volume keeps any it has),
# then wait. curl checks the certificate, so success means a trusted one.
https_code() { curl -s -m 5 -o /dev/null -w '%{http_code}' "https://$HOST/healthz" 2>/dev/null || true; }
for _ in $(seq 1 60); do
	dig +short "$HOST" 2>/dev/null | grep -q '^[0-9]' && break
	sleep 5
done
if [ "$(https_code)" != 200 ] && [ "$(https_code)" != 403 ]; then
	kubectl -n "$NS" rollout restart deploy/public-api >/dev/null
	kubectl -n "$NS" rollout status deploy/public-api --timeout=5m >/dev/null
fi
CODE=""
for _ in $(seq 1 120); do
	CODE=$(https_code)
	case "$CODE" in 200 | 403) break ;; esac
	sleep 5
done
case "$CODE" in
200) ;;
403) echo "    the certificate is valid; this Mac's IP as Azure sees it isn't on the allowlist (add it to ALLOWED_CIDRS)" ;;
*)
	echo "https://$HOST has no valid certificate after 10 minutes. Re-running ./cloud-up.sh continues from here." >&2
	echo "Caddy's log: kubectl -n $NS logs deploy/public-api. If Let's Encrypt says too many certificates for this" >&2
	echo "name this week, set AZURE_DNS_LABEL=sandbox-<something new> and re-run." >&2
	exit 1
	;;
esac

cat <<MSG

Sandbox provider is up on AKS ($(( $(date +%s) - START )) s): cluster $CLUSTER in $LOCATION.
  API:         https://$HOST/v1     Dashboard: https://$HOST/ui
  SSH:         ssh <sandbox-id>@$GW
  Allowed:     ${ALLOWED:-anyone with a key (restrict: ALLOWED_CIDRS=... ./cloud-up.sh --allowlist-only)}
  Your key:    $SANDBOX_KEYFILE
  Testers:     ./code/scripts/add-tester.sh <name>, then send them TESTING.md, the URL and their key
  Use it here: source ./env.sh aks    (kubectl, demo.sh and the test scripts then target AKS)
  Costs about \$0.20/hour while it exists. Remove it all: ./cloud-down.sh
MSG
