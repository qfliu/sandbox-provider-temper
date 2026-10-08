#!/usr/bin/env bash
# Lets the local minikube (arm64 on an Apple Silicon Mac) build amd64
# images, for cloud nodes on x86 (D63): registers QEMU's x86-64 emulator with
# the VM's kernel (binfmt_misc), so BuildKit can run amd64 build steps.
# Builds then pass `--build-opt=opt=platform=linux/amd64`. Go code
# cross-compiles natively (code/Dockerfile); the sandbox images' RUN steps
# run emulated (about 5 minutes for all three).
#
# Registrations are kernel state, gone when the VM restarts; this is
# idempotent and cloud-up.sh runs it before every amd64 build.
#
# Always acts on the local cluster, whichever one `source ./env.sh` chose
# (it runs a privileged pod; never on the cloud cluster).
#
# Requires: source ./env.sh (local or aks), minikube running.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${MINIKUBE_HOME:?source ./env.sh first}"
kc() { kubectl --kubeconfig "$ROOT/.devenv/state/kube/config" -n default "$@"; }

# tonistiigi/binfmt (Docker's own installer for these emulators), pinned.
BINFMT="tonistiigi/binfmt:qemu-v10.0.4@sha256:8f58e6214f4cc9dc83ce8f5acad1ece508eb6b20e696a8c1e9f274481982c541"

[ "$(minikube ssh -- uname -m | tr -d '\r')" = x86_64 ] && exit 0   # native already

minikube ssh -- 'mountpoint -q /proc/sys/fs/binfmt_misc || sudo mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc'
if minikube ssh -- 'test -e /proc/sys/fs/binfmt_misc/qemu-x86_64' 2>/dev/null; then
  exit 0
fi
echo "==> registering amd64 emulation in the minikube VM"
trap 'kc delete pod binfmt --ignore-not-found --wait=false >/dev/null 2>&1' EXIT
kc delete pod binfmt --ignore-not-found --wait=true >/dev/null
kc run binfmt --restart=Never --image="$BINFMT" \
  --overrides="{\"spec\":{\"automountServiceAccountToken\":false,\"containers\":[{\"name\":\"binfmt\",\"image\":\"$BINFMT\",\"args\":[\"--install\",\"amd64\"],\"securityContext\":{\"privileged\":true}}]}}" >/dev/null
phase=""
for _ in $(seq 1 90); do
  phase=$(kc get pod binfmt -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [ "$phase" = Succeeded ] || [ "$phase" = Failed ] && break
  sleep 2
done
[ "$phase" = Succeeded ] ||
  { kc logs binfmt 2>/dev/null | tail -5 >&2; echo "registering amd64 emulation failed (pod: ${phase:-not started})" >&2; exit 1; }
minikube ssh -- 'test -e /proc/sys/fs/binfmt_misc/qemu-x86_64' ||
  { echo "amd64 emulation isn't registered after all" >&2; exit 1; }
