#!/usr/bin/env bash
# Lets the local minikube (arm64 on an Apple Silicon Mac) build amd64
# images, for cloud nodes on x86 (D63): registers QEMU's x86-64 emulator with
# the VM's kernel (binfmt_misc), so BuildKit can run amd64 build steps.
# Builds then pass `--build-opt=opt=platform=linux/amd64`. Go code
# cross-compiles natively (code/Dockerfile); only the sandbox images' apt
# installs run emulated (about 5 minutes for all three).
#
# The VM's / is a tmpfs, so the registration is gone after a VM restart;
# this is idempotent and cloud-up.sh runs it before every amd64 build.
#
# Requires: source ./env.sh, minikube running.
set -euo pipefail

# tonistiigi/binfmt (Docker's own installer for these emulators), pinned.
BINFMT="tonistiigi/binfmt:qemu-v10.0.4@sha256:8f58e6214f4cc9dc83ce8f5acad1ece508eb6b20e696a8c1e9f274481982c541"

[ "$(minikube ssh -- uname -m | tr -d '\r')" = x86_64 ] && exit 0   # native already

minikube ssh -- 'mountpoint -q /proc/sys/fs/binfmt_misc || sudo mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc'
if minikube ssh -- 'test -e /proc/sys/fs/binfmt_misc/qemu-x86_64' 2>/dev/null; then
  exit 0
fi
echo "==> registering amd64 emulation in the minikube VM"
kubectl -n default delete pod binfmt --ignore-not-found --wait=true >/dev/null
kubectl -n default run binfmt --restart=Never --image="$BINFMT" \
  --overrides="{\"spec\":{\"containers\":[{\"name\":\"binfmt\",\"image\":\"$BINFMT\",\"args\":[\"--install\",\"amd64\"],\"securityContext\":{\"privileged\":true}}]}}" >/dev/null
kubectl -n default wait --for=jsonpath='{.status.phase}'=Succeeded pod/binfmt --timeout=180s >/dev/null ||
  { kubectl -n default logs binfmt | tail -5 >&2; echo "registering amd64 emulation failed" >&2; exit 1; }
kubectl -n default delete pod binfmt --wait=false >/dev/null
minikube ssh -- 'test -e /proc/sys/fs/binfmt_misc/qemu-x86_64' ||
  { echo "amd64 emulation isn't registered after all" >&2; exit 1; }
