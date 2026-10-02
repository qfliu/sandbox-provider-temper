#!/usr/bin/env bash
# Brings up the local cluster from scratch (or restarts a stopped one):
#   minikube VM (vfkit) + Calico + gVisor + both sandbox images.
# Idempotent; safe to re-run.
#
# Usage (from the project root): source ./env.sh && ./code/scripts/cluster-up.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE_TAG=0.1

echo "==> starting minikube"
minikube start \
  --driver=vfkit \
  --container-runtime=containerd \
  --cni=calico \
  --cpus="${MINIKUBE_CPUS:-4}" \
  --memory="${MINIKUBE_MEMORY:-8192}" \
  --disk-size="${MINIKUBE_DISK:-40g}"

"$ROOT/code/scripts/install-gvisor.sh"

echo "==> building sandbox images"
LOG="$ROOT/.devenv/state/tmp/image-build.log"
for target in base dev; do
  # BuildKit (containerd) spells the target option `--opt target=...`.
  minikube image build -t "sandbox/ubuntu-$target:$IMAGE_TAG" \
    --build-opt=opt=target="$target" "$ROOT/code/images/sandbox" >"$LOG" 2>&1 \
    || { tail -30 "$LOG"; echo "image build failed; full log: $LOG" >&2; exit 1; }
  echo "    sandbox/ubuntu-$target:$IMAGE_TAG"
done

echo "==> cluster ready"
kubectl get nodes -o wide | cut -c1-100
