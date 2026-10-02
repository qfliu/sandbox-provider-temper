#!/usr/bin/env bash
# Installs gVisor (runsc) into the minikube node and registers the `gvisor`
# RuntimeClass. Replaces `minikube addons enable gvisor`, whose download URL
# is stale: gVisor now ships one gvisor.tar.bz2 bundle per arch instead of
# separate runsc / containerd-shim-runsc-v1 files.
#
# The node's / and /etc are tmpfs (reset on every VM boot) while /data is on
# disk, so binaries live in /data/gvisor and the symlinks + containerd config
# are re-applied each run. Safe to re-run; cluster-up.sh calls it after
# `minikube start`.
#
# Requires: source ./env.sh, minikube running.
set -euo pipefail

GVISOR_RELEASE=20260928

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

case "$(minikube ssh -- uname -m | tr -d '\r')" in
  aarch64) ARCH=aarch64 ;;
  x86_64)  ARCH=x86_64 ;;
  *) echo "unsupported node arch" >&2; exit 1 ;;
esac

CACHE="$ROOT/.devenv/state/gvisor/${GVISOR_RELEASE}-${ARCH}"
URL="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/${ARCH}/gvisor.tar.bz2"
mkdir -p "$CACHE"

# 1. Download + verify + extract on the host (cached across runs).
if [ ! -x "$CACHE/runsc" ]; then
  echo "==> downloading gVisor $GVISOR_RELEASE ($ARCH)"
  curl -fL --retry 3 --progress-bar "$URL" -o "$CACHE/gvisor.tar.bz2"
  curl -fsSL "$URL.sha512" -o "$CACHE/gvisor.tar.bz2.sha512"
  want="$(awk '{print $1}' "$CACHE/gvisor.tar.bz2.sha512")"
  got="$(shasum -a 512 "$CACHE/gvisor.tar.bz2" | awk '{print $1}')"
  [ "$want" = "$got" ] || { echo "checksum mismatch for $URL" >&2; exit 1; }
  tar -xjf "$CACHE/gvisor.tar.bz2" -C "$CACHE"
fi

# 2. Copy into the node's persistent disk, unless this release is already there.
if [ "$(minikube ssh -- "cat /data/gvisor/RELEASE 2>/dev/null || true" 2>/dev/null | tr -d '\r')" != "$GVISOR_RELEASE" ]; then
  echo "==> copying gVisor into the minikube node"
  minikube ssh -- "sudo rm -rf /data/gvisor && sudo mkdir -p /data/gvisor/gvisor-bin"
  for f in runsc containerd-shim-runsc-v1 $(cd "$CACHE" && ls -d gvisor-bin/*); do
    minikube cp "$CACHE/$f" "minikube:/data/gvisor/$f" >/dev/null
  done
  minikube ssh -- "sudo chmod 755 /data/gvisor/runsc /data/gvisor/containerd-shim-runsc-v1 /data/gvisor/gvisor-bin/* \
    && echo $GVISOR_RELEASE | sudo tee /data/gvisor/RELEASE >/dev/null"
fi

# 3. Link onto PATH and register the runsc runtime with containerd.
#    runsc finds its sidecar binaries in gvisor-bin/ next to its real path,
#    which is /data/gvisor (symlinks resolve through /proc/self/exe).
echo "==> configuring containerd"
minikube ssh -- 'set -e
  sudo ln -sf /data/gvisor/runsc /usr/bin/runsc
  sudo ln -sf /data/gvisor/containerd-shim-runsc-v1 /usr/bin/containerd-shim-runsc-v1
  if ! grep -q "runtimes.runsc\]" /etc/containerd/config.toml; then
    printf "\n[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]\n  runtime_type = \"io.containerd.runsc.v1\"\n" \
      | sudo tee -a /etc/containerd/config.toml >/dev/null
    sudo systemctl restart containerd
  fi'

# 4. RuntimeClass so pods can opt in with `runtimeClassName: gvisor`.
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF

echo "==> gVisor $GVISOR_RELEASE ready (RuntimeClass: gvisor)"
