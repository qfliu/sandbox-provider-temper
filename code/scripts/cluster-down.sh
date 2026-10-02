#!/usr/bin/env bash
# Stops the local cluster VM (frees CPU/RAM; state is kept for a fast restart).
#   --delete   also delete the VM and its disk (next cluster-up starts fresh)
#
# Usage (from the project root): source ./env.sh && ./code/scripts/cluster-down.sh [--delete]
set -euo pipefail

if [ "${1:-}" = "--delete" ]; then
  minikube delete
else
  minikube stop
fi
