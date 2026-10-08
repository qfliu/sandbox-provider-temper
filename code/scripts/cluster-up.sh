#!/usr/bin/env bash
# Brings up the local cluster from scratch (or restarts a stopped one):
#   minikube VM (vfkit) + Calico + gVisor + both sandbox images.
# The platform itself is installed by deploy.sh (Helm chart). Idempotent;
# safe to re-run.
#
# Usage (from the project root): source ./env.sh && ./code/scripts/cluster-up.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE_TAG=0.2 # = sandboxImages.tag in the chart

# vfkit talks to the VM over a Unix socket under MINIKUBE_HOME, and macOS
# caps socket paths at 104 bytes; past that, minikube just times out after
# 6 minutes. Fail fast instead: a clone path of about 40 characters fits.
SOCK="${MINIKUBE_HOME:?source ./env.sh first}/.minikube/machines/${MINIKUBE_PROFILE:-minikube}/vfkit.sock"
SOCK_BYTES=$(printf %s "$SOCK" | LC_ALL=C wc -c | tr -d ' ')   # bytes, not characters
if [ "$SOCK_BYTES" -gt 103 ]; then
  echo "This clone's path is too long for minikube's vfkit driver: its socket" >&2
  echo "  $SOCK" >&2
  echo "would be $SOCK_BYTES bytes (macOS allows 103). Clone the repo to a shorter" >&2
  echo "path, e.g. ~/sandbox-provider, and run ./up.sh there." >&2
  exit 1
fi

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
for target in base dev docker; do
  # BuildKit (containerd) spells the target option `--opt target=...`.
  minikube image build -t "sandbox/ubuntu-$target:$IMAGE_TAG" \
    --build-opt=opt=target="$target" "$ROOT/code/images/sandbox" >"$LOG" 2>&1 \
    || { tail -30 "$LOG"; echo "image build failed; full log: $LOG" >&2; exit 1; }
  echo "    sandbox/ubuntu-$target:$IMAGE_TAG"
done

echo "==> cluster ready"
kubectl get nodes -o wide | cut -c1-100
