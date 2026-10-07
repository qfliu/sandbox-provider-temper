#!/usr/bin/env bash
# Builds the platform image (API + SSH gateway) inside minikube and installs
# or upgrades the Helm chart (code/deploy/helm) into sandbox-system:
# Postgres, database roles, API, gateway, RBAC, NetworkPolicies, the
# sandbox admission policy and the image pre-pull DaemonSet. Keys and
# passwords are generated on first install and kept. Each build gets a
# unique tag, so Kubernetes always picks up the new code. Idempotent.
#
# Run after cluster-up.sh (minikube, gVisor, sandbox images).
# Usage (from the project root): source ./env.sh && ./code/scripts/deploy.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
NS=sandbox-system
# The local cluster only: in a shell switched to AKS (source ./env.sh aks),
# this install would replace the cloud release with local settings. The
# cloud is updated with ./cloud-up.sh.
CTX=$(kubectl config current-context 2>/dev/null || true)
if [ "${SANDBOX_TARGET:-local}" != local ] || [ "$CTX" != "${MINIKUBE_PROFILE:-minikube}" ]; then
	echo "deploy.sh deploys to the local minikube cluster, but kubectl points at '$CTX'." >&2
	echo "Run: source ./env.sh local   (for the Azure cluster use ./cloud-up.sh)" >&2
	exit 1
fi
mkdir -p "$ROOT/.devenv/state/tmp"
# <commit>[-dirty]-<time>: "-dirty" flags builds with uncommitted changes, so
# the commit in the tag never misdescribes what's running.
DIRTY="$(git -C "$ROOT" diff --quiet HEAD -- code || echo -dirty)"
TAG="$(git -C "$ROOT" rev-parse --short HEAD)${DIRTY}-$(date +%s)"
LOG="$ROOT/.devenv/state/tmp/platform-build.log"

echo "==> building sandbox/api:$TAG (API + gateway)"
minikube image build -t "sandbox/api:$TAG" "$ROOT/code" >"$LOG" 2>&1 \
  || { tail -30 "$LOG"; echo "image build failed; full log: $LOG" >&2; exit 1; }

echo "==> helm upgrade --install sandbox"
# The dashboard shows users how to SSH in through the gateway's NodePort.
# (Assigned first so a failing `minikube ip` stops the script under set -e.)
GATEWAY_IP=$(minikube ip)
# --take-ownership and --force-conflicts: adopt the objects an older,
# pre-Helm deploy created with kubectl apply (Helm 4 applies server-side
# and would otherwise refuse fields another field manager set).
helm upgrade --install sandbox "$ROOT/code/deploy/helm" -n "$NS" --create-namespace \
  --take-ownership --force-conflicts \
  --set image.tag="$TAG" --set api.sshGatewayAddr="$GATEWAY_IP:30022" --timeout 10m >/dev/null || {
  echo "helm failed; the database setup Job said:" >&2
  kubectl -n "$NS" logs job/sandbox-db-setup --tail=20 >&2 2>/dev/null || true
  exit 1
}
for r in statefulset/postgres deployment/sandbox-api deployment/sandbox-gateway daemonset/sandbox-image-prepull; do
  kubectl -n "$NS" rollout status "$r" --timeout=180s >/dev/null
done
kubectl -n "$NS" get pods -o wide | cut -c1-100
echo "==> ready"
echo "    dashboard: kubectl -n $NS port-forward svc/sandbox-api 18080:80   then open http://localhost:18080/ui"
echo "    first admin key: kubectl -n $NS exec deploy/sandbox-api -- /api create-user --name admin --admin"
echo "    ssh: ssh -p 30022 <sandbox-id>@$GATEWAY_IP"
GW_PUB=$(kubectl -n "$NS" exec deploy/sandbox-gateway -- /api public-key /keys/host/ssh_host_ed25519_key | cut -d' ' -f1,2)
echo "    gateway host key: $(ssh-keygen -lf - <<<"$GW_PUB" | cut -d' ' -f2)"
echo "    to pin it, add to ~/.ssh/known_hosts:  [$GATEWAY_IP]:30022 $GW_PUB"
