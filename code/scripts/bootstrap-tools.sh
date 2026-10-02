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
VFKIT_VERSION=v0.6.4

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TOOLS="$ROOT/.devenv/tools"
BIN="$TOOLS/bin"
mkdir -p "$BIN"
# Scratch space for downloads also lives inside .devenv, not /tmp.
TMPD="$ROOT/.devenv/tmp"
mkdir -p "$TMPD"

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  OS=darwin; ARCH=arm64 ;;
  Darwin-x86_64) OS=darwin; ARCH=amd64 ;;
  *) echo "unsupported platform: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

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

# vfkit boots minikube's Linux VM directly via macOS Virtualization.framework
# (minikube --driver=vfkit). The release binary is a signed universal binary.
echo "==> vfkit $VFKIT_VERSION"
if ! have "$BIN/vfkit"; then
  fetch "https://github.com/crc-org/vfkit/releases/download/${VFKIT_VERSION}/vfkit" "$BIN/vfkit"
  chmod +x "$BIN/vfkit"
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
