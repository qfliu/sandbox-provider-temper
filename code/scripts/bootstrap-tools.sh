#!/usr/bin/env bash
# Installs every CLI tool this project needs into ./.devenv/tools
# (project-local). Nothing is written outside the project directory.
# Remove ./.devenv to fully uninstall.
#
# Usage (from the project root): ./code/scripts/bootstrap-tools.sh   then   source ./env.sh
set -euo pipefail

GO_VERSION=1.27.1
KUBECTL_VERSION=v1.37.1
HELM_VERSION=v4.3.0
MINIKUBE_VERSION=v1.39.0
COLIMA_VERSION=v0.10.3
LIMA_VERSION=2.2.0
DOCKER_VERSION=29.8.2

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TOOLS="$ROOT/.devenv/tools"
BIN="$TOOLS/bin"
mkdir -p "$BIN"
# Scratch space for downloads also lives inside .devenv, not /tmp.
TMPD="$ROOT/.devenv/tmp"
mkdir -p "$TMPD"

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  OS=darwin; ARCH=arm64; CAP_OS=Darwin; DOCKER_ARCH=aarch64 ;;
  Darwin-x86_64) OS=darwin; ARCH=amd64; CAP_OS=Darwin; DOCKER_ARCH=x86_64 ;;
  *) echo "unsupported platform: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac
LIMA_ARCH=$([ "$ARCH" = arm64 ] && echo arm64 || echo x86_64)

fetch() { curl -fL --retry 3 --progress-bar "$1" -o "$2"; }
have() { [ -x "$1" ] && echo "  already installed: $(basename "$1")"; }

echo "==> go $GO_VERSION"
if ! have "$TOOLS/go/bin/go"; then
  fetch "https://go.dev/dl/go${GO_VERSION}.${OS}-${ARCH}.tar.gz" $TMPD/go.tgz.$$
  tar -xzf $TMPD/go.tgz.$$ -C "$TOOLS" && rm $TMPD/go.tgz.$$
fi

echo "==> kubectl $KUBECTL_VERSION"
if ! have "$BIN/kubectl"; then
  fetch "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${OS}/${ARCH}/kubectl" "$BIN/kubectl"
  chmod +x "$BIN/kubectl"
fi

echo "==> helm $HELM_VERSION"
if ! have "$BIN/helm"; then
  fetch "https://get.helm.sh/helm-${HELM_VERSION}-${OS}-${ARCH}.tar.gz" $TMPD/helm.tgz.$$
  tar -xzf $TMPD/helm.tgz.$$ -C "$TMPD" "${OS}-${ARCH}/helm"
  mv "$TMPD/${OS}-${ARCH}/helm" "$BIN/helm" && rm -rf $TMPD/helm.tgz.$$ "$TMPD/${OS}-${ARCH}"
fi

echo "==> minikube $MINIKUBE_VERSION"
if ! have "$BIN/minikube"; then
  fetch "https://github.com/kubernetes/minikube/releases/download/${MINIKUBE_VERSION}/minikube-${OS}-${ARCH}" "$BIN/minikube"
  chmod +x "$BIN/minikube"
fi

# lima provides the Linux VM (via macOS Virtualization.framework); colima
# wraps it to run a Docker engine. Lima needs its bin/ and share/ dirs kept
# together, so it gets its own prefix.
echo "==> lima $LIMA_VERSION"
if ! have "$TOOLS/lima/bin/limactl"; then
  mkdir -p "$TOOLS/lima"
  fetch "https://github.com/lima-vm/lima/releases/download/v${LIMA_VERSION}/lima-${LIMA_VERSION}-${CAP_OS}-${LIMA_ARCH}.tar.gz" $TMPD/lima.tgz.$$
  tar -xzf $TMPD/lima.tgz.$$ -C "$TOOLS/lima" && rm $TMPD/lima.tgz.$$
fi

echo "==> colima $COLIMA_VERSION"
if ! have "$BIN/colima"; then
  fetch "https://github.com/abiosoft/colima/releases/download/${COLIMA_VERSION}/colima-${CAP_OS}-${LIMA_ARCH}" "$BIN/colima"
  chmod +x "$BIN/colima"
fi

echo "==> docker cli $DOCKER_VERSION"
if ! have "$BIN/docker"; then
  fetch "https://download.docker.com/mac/static/stable/${DOCKER_ARCH}/docker-${DOCKER_VERSION}.tgz" $TMPD/docker.tgz.$$
  tar -xzf $TMPD/docker.tgz.$$ -C "$TMPD" docker/docker
  mv $TMPD/docker/docker "$BIN/docker" && rm -rf $TMPD/docker.tgz.$$ $TMPD/docker
fi

# Go writes telemetry counters to ~/Library/Application Support/go and has
# no env var to move them, so run go with a project-local HOME instead.
# (Only affects go itself; git/ssh in your normal shell are untouched.)
echo "==> go wrapper (keeps Go's telemetry inside .devenv)"
cat > "$BIN/go" <<'SHIM'
#!/usr/bin/env bash
STATE="$(cd "$(dirname "$0")/../.." && pwd)/state"
mkdir -p "$STATE/gohome"
HOME="$STATE/gohome" exec "$(dirname "$0")/../go/bin/go" "$@"
SHIM
chmod +x "$BIN/go"

# Binaries downloaded with curl normally have no quarantine flag, but clear it
# just in case so Gatekeeper doesn't block them.
xattr -dr com.apple.quarantine "$TOOLS" 2>/dev/null || true

rm -rf "$TMPD"

echo
echo "Done. Activate with:  source ./env.sh"
