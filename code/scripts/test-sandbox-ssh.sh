#!/usr/bin/env bash
# Phase 1 end-to-end check: run the hand-written sandbox pod under gVisor and
# SSH into it the way the control plane / gateway will (pod IP, internal key).
#
# kubectl port-forward can't be used: it dials 127.0.0.1 in the pod's network
# namespace, but gVisor runs the sandbox's network stack inside the sandbox,
# so the namespace's loopback can't reach sshd. A helper pod on the normal
# runtime connects to the sandbox's pod IP instead.
#
# Requires: source ./env.sh, cluster up, images built.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
KEYS="$ROOT/.devenv/state/internal-key"
mkdir -p "$KEYS"
[ -f "$KEYS/id" ] || ssh-keygen -q -t ed25519 -N "" -C sandbox-internal -f "$KEYS/id"

kubectl create secret generic sandbox-internal-key \
  --from-file=authorized_keys="$KEYS/id.pub" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create secret generic sandbox-internal-key-private \
  --from-file=id="$KEYS/id" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

kubectl delete pod sb-manual ssh-client --ignore-not-found --wait=true >/dev/null
start=$(date +%s)
kubectl apply -f "$ROOT/code/deploy/dev/sandbox-pod.yaml" >/dev/null
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Pod
metadata: {name: ssh-client}
spec:
  containers:
  - name: c
    image: sandbox/ubuntu-base:0.1
    imagePullPolicy: Never
    command: ["sleep", "infinity"]
    volumeMounts: [{name: k, mountPath: /keys, readOnly: true}]
  volumes: [{name: k, secret: {secretName: sandbox-internal-key-private, defaultMode: 0400}}]
EOF
kubectl wait --for=condition=Ready pod/sb-manual --timeout=120s >/dev/null
echo "sandbox ready in $(( $(date +%s) - start ))s"
kubectl wait --for=condition=Ready pod/ssh-client --timeout=60s >/dev/null

IP="$(kubectl get pod sb-manual -o jsonpath='{.status.podIP}')"
echo "sandbox pod IP: $IP"

# Everything below runs inside the helper pod.
status=0
kubectl exec -i ssh-client -- bash -s -- "$IP" <<'INNER' || status=$?
set -u
IP=$1
# -n: don't let ssh read stdin; this script itself arrives on stdin.
O=(-n -i /keys/id -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes)
pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1"; FAILED=1; }
FAILED=0

out=$(ssh "${O[@]}" "user@$IP" 'echo "$(whoami) $(uname -r)"')
[[ "$out" == "user "*gvisor* ]] && pass "ssh as user; kernel is gVisor ($out)" || fail "ssh/gVisor check: '$out'"

ssh "${O[@]}" "user@$IP" 'python3 -m venv ~/venv && ~/venv/bin/pip install -q requests && ~/venv/bin/python -c "import requests"' >/dev/null 2>&1 \
  && pass "pip install from the internet" || fail "pip install"

ssh "${O[@]}" "user@$IP" 'mkdir -p ~/js && cd ~/js && npm init -y >/dev/null && npm install --silent left-pad && node -e "require(\"left-pad\")"' >/dev/null 2>&1 \
  && pass "npm install from the internet" || fail "npm install"

ssh "${O[@]}" "user@$IP" 'sudo -n true' 2>/dev/null && pass "passwordless sudo inside the sandbox" || fail "sudo"

echo "hello via sftp" > /tmp/hello.txt
echo "put /tmp/hello.txt" | sftp -q "${O[@]:1}" "user@$IP" >/dev/null 2>&1
[[ "$(ssh "${O[@]}" "user@$IP" cat hello.txt 2>/dev/null)" == "hello via sftp" ]] && pass "sftp upload" || fail "sftp upload"

ssh "${O[@]}" "root@$IP" true 2>/dev/null && fail "root login should be rejected" || pass "root login rejected"
ssh-keygen -q -t ed25519 -N '' -f /tmp/other <<<y >/dev/null 2>&1
ssh -n -i /tmp/other -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes "user@$IP" true 2>/dev/null \
  && fail "unknown key should be rejected" || pass "unknown key rejected"

echo "  (inner checks completed)"
exit $FAILED
INNER

kubectl delete pod sb-manual ssh-client --wait=false >/dev/null
[ $status -eq 0 ] && echo "ALL CHECKS PASSED" || { echo "SOME CHECKS FAILED"; exit 1; }
