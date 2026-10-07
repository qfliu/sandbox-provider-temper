#!/usr/bin/env bash
# Installs the extra tools for the Azure deployment (Phase 7) into
# ./.devenv/tools, like bootstrap-tools.sh does for the local ones: Terraform
# (one binary) and the Azure CLI (a Python package, in its own virtualenv).
# Nothing is written outside the project: pip's cache, Terraform's update
# check and the CLI's login all point into .devenv (here and in env.sh).
# Not needed for the local setup, so up.sh never runs it.
#
# Integrity: Terraform's zip is checked against HashiCorp's SHA256SUMS
# (catches a corrupt download; the file's GPG signature isn't checked, macOS
# has no gpg). The Azure CLI is pinned to one version; pip resolves its
# dependencies, which aren't hash-pinned.
#
# Usage (from the project root): ./code/scripts/bootstrap-cloud-tools.sh   then   source ./env.sh
set -euo pipefail

TERRAFORM_VERSION=1.16.5
AZURE_CLI_VERSION=2.91.0

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TOOLS="$ROOT/.devenv/tools"
BIN="$TOOLS/bin"
TMPD="$ROOT/.devenv/tmp"
mkdir -p "$BIN" "$TMPD"
export PIP_CACHE_DIR="$ROOT/.devenv/state/pip-cache" PIP_DISABLE_PIP_VERSION_CHECK=1
export CHECKPOINT_DISABLE=1   # terraform: no update check (it writes ~/.terraform.d)

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  OS=darwin; ARCH=arm64 ;;
  Darwin-x86_64) OS=darwin; ARCH=amd64 ;;
  *) echo "unsupported platform: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

fetch() { curl -fL --retry 3 --progress-bar "$1" -o "$2"; }

echo "==> terraform $TERRAFORM_VERSION"
if [ "$("$BIN/terraform" version -json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["terraform_version"])' 2>/dev/null)" != "$TERRAFORM_VERSION" ]; then
  zip="terraform_${TERRAFORM_VERSION}_${OS}_${ARCH}.zip"
  fetch "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/$zip" "$TMPD/$zip"
  fetch "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_SHA256SUMS" "$TMPD/tf.sums"
  (cd "$TMPD" && grep " $zip\$" tf.sums | shasum -a 256 -c - >/dev/null) || { echo "terraform checksum mismatch" >&2; exit 1; }
  unzip -o -q "$TMPD/$zip" terraform -d "$BIN"
  rm -f "$TMPD/$zip" "$TMPD/tf.sums"
else
  echo "  already installed: terraform"
fi

echo "==> azure-cli $AZURE_CLI_VERSION (virtualenv in .devenv/tools/azure-cli)"
VENV="$TOOLS/azure-cli"
# The Azure CLI needs Python 3.10+; macOS's own python3 is 3.9.
PY=""
for c in python3.13 python3.12 python3.11 python3.10 python3; do
  command -v "$c" >/dev/null && "$c" -c 'import sys; sys.exit(sys.version_info < (3, 10))' 2>/dev/null && { PY=$c; break; }
done
[ -n "$PY" ] || { echo "the Azure CLI needs Python 3.10 or newer (e.g. brew install python@3.12)" >&2; exit 1; }
if [ "$("$VENV/bin/python" -m pip show azure-cli 2>/dev/null | awk '/^Version:/ {print $2}')" != "$AZURE_CLI_VERSION" ]; then
  "$PY" -m venv "$VENV"
  "$VENV/bin/python" -m pip install --quiet --upgrade pip
  "$VENV/bin/python" -m pip install --quiet "azure-cli==$AZURE_CLI_VERSION"
else
  echo "  already installed: azure-cli"
fi
# The shim keeps the CLI's login and token cache in .devenv even when env.sh
# wasn't sourced (otherwise it would use ~/.azure).
cat >"$BIN/az" <<'SHIM'
#!/usr/bin/env bash
HERE="$(cd "$(dirname "$0")" && pwd)"
export AZURE_CONFIG_DIR="${AZURE_CONFIG_DIR:-$HERE/../../state/azure}"
export AZURE_CORE_COLLECT_TELEMETRY="${AZURE_CORE_COLLECT_TELEMETRY:-false}"
exec "$HERE/../azure-cli/bin/az" "$@"
SHIM
chmod +x "$BIN/az"

xattr -dr com.apple.quarantine "$BIN/terraform" 2>/dev/null || true
echo
echo "Done. Then: source ./env.sh && az login"
