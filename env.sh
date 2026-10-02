# Project-local environment, like a Python venv for CLI tools.
# Usage:  source ./env.sh      (undo with: deactivate_sandbox_env)
#
# All tools, caches, VM disks and configs live under ./.devenv,
# so nothing touches ~/.kube, ~/.docker, ~/.minikube, ~/.colima or ~/go.

_SB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]:-${(%):-%x}}")" && pwd)"
_SB_TOOLS="$_SB_ROOT/.devenv/tools"
_SB_STATE="$_SB_ROOT/.devenv/state"

_SB_OLD_PATH="$PATH"
_SB_OLD_PS1="${PS1:-}"
_SB_OLD_TMPDIR="${TMPDIR:-}"

export PATH="$_SB_TOOLS/bin:$_SB_TOOLS/go/bin:$_SB_TOOLS/lima/bin:$_SB_STATE/gopath/bin:$PATH"

# Go
export GOROOT="$_SB_TOOLS/go"
export GOPATH="$_SB_STATE/gopath"
export GOMODCACHE="$_SB_STATE/gopath/pkg/mod"
export GOCACHE="$_SB_STATE/gocache"
export GOTOOLCHAIN=local
export GOENV="$_SB_STATE/goenv"   # where `go env -w` writes (default is ~/Library/Application Support/go/env)
export TMPDIR="$_SB_STATE/tmp"    # go build, docker build etc. scratch files

# VM + container runtime (colima/lima) and Docker CLI
export LIMA_HOME="$_SB_STATE/lima"
export COLIMA_HOME="$_SB_STATE/colima"
export DOCKER_CONFIG="$_SB_STATE/docker"
export DOCKER_HOST="unix://$_SB_STATE/colima/default/docker.sock"

# Kubernetes
export MINIKUBE_HOME="$_SB_STATE/minikube"
export KUBECONFIG="$_SB_STATE/kube/config"
export HELM_CONFIG_HOME="$_SB_STATE/helm/config"
export HELM_CACHE_HOME="$_SB_STATE/helm/cache"
export HELM_DATA_HOME="$_SB_STATE/helm/data"

mkdir -p "$_SB_STATE"/{gopath,gocache,lima,colima,docker,minikube,kube,helm,tmp}

export PS1="(sandbox) $_SB_OLD_PS1"

deactivate_sandbox_env() {
  export PATH="$_SB_OLD_PATH"
  export PS1="$_SB_OLD_PS1"
  export TMPDIR="$_SB_OLD_TMPDIR"
  unset GOROOT GOPATH GOMODCACHE GOCACHE GOTOOLCHAIN GOENV LIMA_HOME COLIMA_HOME \
        DOCKER_CONFIG DOCKER_HOST MINIKUBE_HOME KUBECONFIG \
        HELM_CONFIG_HOME HELM_CACHE_HOME HELM_DATA_HOME
  unset -f deactivate_sandbox_env
}
