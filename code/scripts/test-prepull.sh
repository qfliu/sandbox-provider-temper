#!/usr/bin/env bash
# Proves the image pre-pull mechanism (D46, step 6.5): a DaemonSet shaped
# like sandbox-image-prepull really downloads an image a node doesn't have,
# and a pod started afterwards finds it cached instead of pulling.
#
# On minikube the sandbox images are built on the node, so the real
# DaemonSet only ever sees "already present"; this uses a public stand-in
# image (busybox) that is first removed from the node. Needs internet
# access from the minikube VM. Everything it creates is deleted.
#
# Usage (from the project root): source ./env.sh && ./code/scripts/test-prepull.sh
set -uo pipefail

: "${KUBECONFIG:?source ./env.sh first}"
IMAGE="${PREPULL_TEST_IMAGE:-docker.io/library/busybox:1.36.1}"
NS=sandbox-system
PASSED=0 FAILED=0

pass() { PASSED=$((PASSED + 1)); printf "  \033[32mPASS\033[0m  %s\n" "$1"; }
fail() { FAILED=$((FAILED + 1)); printf "  \033[31mFAIL\033[0m  %s  (%s)\n" "$1" "$2"; }
cleanup() {
	kubectl -n "$NS" delete daemonset prepull-probe --ignore-not-found --wait=false >/dev/null 2>&1
	kubectl -n "$NS" delete pod prepull-consumer --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

# events <pod>: the kubelet's image events for a pod, one per line
events() {
	kubectl -n "$NS" get events --field-selector "involvedObject.name=$1" \
		-o jsonpath='{range .items[*]}{.reason}: {.message}{"\n"}{end}' 2>/dev/null |
		grep -E "^(Pulling|Pulled):" || true
}

printf "stand-in image: %s\n" "$IMAGE"
minikube ssh -- "sudo crictl rmi $IMAGE" >/dev/null 2>&1
if minikube ssh -- "sudo crictl inspecti $IMAGE" >/dev/null 2>&1; then
	echo "setup failed: $IMAGE is still on the node" >&2
	exit 1
fi
pass "node starts without the image"

# The same shape as deploy/dev/prepull.yaml: an init container that uses the
# image and runs `true`, then idle on pause.
kubectl apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: prepull-probe, namespace: $NS}
spec:
  selector: {matchLabels: {app: prepull-probe}}
  template:
    metadata: {labels: {app: prepull-probe}}
    spec:
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
      initContainers:
      - {name: pull, image: $IMAGE, imagePullPolicy: IfNotPresent, command: ["true"],
         securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}}
      containers:
      - {name: pause, image: "registry.k8s.io/pause:3.10.2", imagePullPolicy: IfNotPresent,
         securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}}
EOF
kubectl -n "$NS" rollout status daemonset/prepull-probe --timeout=180s >/dev/null ||
	{ echo "setup failed: prepull-probe didn't roll out" >&2; exit 1; }
POD=$(kubectl -n "$NS" get pods -l app=prepull-probe -o jsonpath='{.items[0].metadata.name}')
sleep 2
ev=$(events "$POD")
printf "  DaemonSet pod events:\n%s\n" "$(printf '%s\n' "$ev" | sed 's/^/    /')"
if grep -q "^Pulling: .*$IMAGE" <<<"$ev" && grep -q "^Pulled: Successfully pulled" <<<"$ev"; then
	pass "the DaemonSet downloaded the image ($(grep -o 'in [0-9.]*m*s' <<<"$ev" | head -1))"
else
	fail "the DaemonSet downloaded the image" "no Pulling/Successfully pulled event"
fi

# A pod started afterwards (like a sandbox) finds it cached.
kubectl -n "$NS" run prepull-consumer --image="$IMAGE" --image-pull-policy=IfNotPresent --restart=Never \
	--overrides='{"spec":{"automountServiceAccountToken":false,"securityContext":{"runAsNonRoot":true,"runAsUser":65534,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"prepull-consumer","image":"'"$IMAGE"'","command":["true"],"securityContext":{"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,"capabilities":{"drop":["ALL"]}}}]}}' \
	>/dev/null
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Succeeded pod/prepull-consumer --timeout=60s >/dev/null 2>&1
ev=$(events prepull-consumer)
printf "  later pod events:\n%s\n" "$(printf '%s\n' "$ev" | sed 's/^/    /')"
if grep -q "already present on machine" <<<"$ev" && ! grep -q "^Pulling:" <<<"$ev"; then
	pass "a pod started afterwards found it cached (no pull)"
else
	fail "a pod started afterwards found it cached" "$ev"
fi

printf "\n%d passed, %d failed\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
