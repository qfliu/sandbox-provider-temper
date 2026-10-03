#!/usr/bin/env bash
# Builds the API image inside minikube and rolls it out to sandbox-system.
# Each build gets a unique tag, so Kubernetes always picks up the new code.
#
# Usage (from the project root): source ./env.sh && ./code/scripts/deploy-api.sh
# Then reach it from your Mac:   kubectl -n sandbox-system port-forward svc/sandbox-api 18080:80
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# <commit>[-dirty]-<time>: "-dirty" flags builds with uncommitted changes, so
# the commit in the tag never misdescribes what's running.
DIRTY="$(git -C "$ROOT" diff --quiet HEAD -- code || echo -dirty)"
TAG="$(git -C "$ROOT" rev-parse --short HEAD)${DIRTY}-$(date +%s)"
LOG="$ROOT/.devenv/state/tmp/api-build.log"

echo "==> building sandbox/api:$TAG"
minikube image build -t "sandbox/api:$TAG" "$ROOT/code" >"$LOG" 2>&1 \
  || { tail -30 "$LOG"; echo "API image build failed; full log: $LOG" >&2; exit 1; }

echo "==> deploying"
# The dashboard shows users how to SSH in through the gateway's NodePort (D38).
sed -e "s|sandbox/api:IMAGE_TAG|sandbox/api:$TAG|" \
    -e "s|\"SSH_GATEWAY_ADDR\"|\"$(minikube ip):30022\"|" \
    "$ROOT/code/deploy/dev/api.yaml" | kubectl apply -f - >/dev/null
kubectl -n sandbox-system rollout status deployment/sandbox-api --timeout=180s >/dev/null
kubectl -n sandbox-system get pods -l app=sandbox-api -o wide | cut -c1-90
echo "==> sandbox-api ready (sandbox-system/sandbox-api:80)"
echo "    dashboard: kubectl -n sandbox-system port-forward svc/sandbox-api 18080:80"
echo "               then open http://localhost:18080/ui and sign in with an API key"
