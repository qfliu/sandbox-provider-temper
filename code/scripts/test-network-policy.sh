#!/usr/bin/env bash
# Verifies the network isolation of sandboxes (D18, D37) from inside a real
# sandbox: internet and DNS work; the platform, other sandboxes, the
# Kubernetes API, node IPs and the metadata endpoint don't. Also checks that
# only the API and gateway can reach a sandbox's port 22, and only they can
# reach Postgres. Prints PASS/FAIL per check; exits non-zero on any failure.
#
# Usage (from the project root, with the API and gateway deployed):
#   source ./env.sh && ./code/scripts/test-network-policy.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
PORT="${NETPOL_PORT:-18095}"
API="http://127.0.0.1:$PORT/v1"
KEY="$(cat "${SANDBOX_KEYFILE:-$ROOT/.devenv/state/admin-api-key}")"
TMP="$(mktemp -d "$ROOT/.devenv/state/tmp/netpol.XXXXXX")"
PASSED=0 FAILED=0 SANDBOXES=()

pass() { PASSED=$((PASSED + 1)); printf "  \033[32mPASS\033[0m  %s\n" "$1"; }
fail() { FAILED=$((FAILED + 1)); printf "  \033[31mFAIL\033[0m  %s  (%s)\n" "$1" "$2"; }

cleanup() {
	for id in ${SANDBOXES[@]+"${SANDBOXES[@]}"}; do
		curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $KEY" "$API/sandboxes/$id" || true
	done
	kubectl -n default delete pod netpol-outsider --ignore-not-found --wait=false >/dev/null 2>&1
	[ -n "${PF:-}" ] && { kill "$PF"; wait "$PF"; } 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT

kubectl -n sandbox-system port-forward svc/sandbox-api "$PORT:80" >/dev/null 2>&1 &
PF=$!
for _ in $(seq 1 40); do curl -s -m 1 -o /dev/null "http://127.0.0.1:$PORT/healthz" && break; sleep 0.25; done

new_sandbox() {
	ssh-keygen -q -t ed25519 -N "" -f "$TMP/k$1" 2>/dev/null
	curl -s -H "Authorization: Bearer $KEY" -X POST "$API/sandboxes" \
		-d "{\"image\":\"ubuntu-base\",\"ssh_public_key\":\"$(cat "$TMP/k$1.pub")\"}" |
		python3 -c "import json,sys; print(json.load(sys.stdin)['id'])"
}
A=$(new_sandbox a) || { echo "could not create sandbox A"; exit 1; }
SANDBOXES=("$A")
B=$(new_sandbox b) || { echo "could not create sandbox B"; exit 1; }
SANDBOXES=("$A" "$B")
kubectl -n sandboxes wait --for=condition=Ready pod/"$A" pod/"$B" --timeout=90s >/dev/null || { echo "sandboxes not ready"; exit 1; }
s=""
for _ in $(seq 1 30); do # the API also needs to see them running
	s=$(curl -s -H "Authorization: Bearer $KEY" "$API/sandboxes/$A" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")
	[ "$s" = running ] && break; sleep 1
done
[ "$s" = running ] || { echo "sandbox $A not running in the API (status: $s)"; exit 1; }
B_IP=$(kubectl -n sandboxes get pod "$B" -o jsonpath='{.status.podIP}')
NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[0].address}')
printf "sandboxes %s (tests run inside it) and %s (%s); node %s\n" "$A" "$B" "$B_IP" "$NODE_IP"

# in_a <command>: run in sandbox A via the API; prints "<exit_code>|<stdout>"
# (just "|" if the exec call itself failed)
in_a() {
	curl -s -H "Authorization: Bearer $KEY" -X POST "$API/sandboxes/$A/exec" \
		-d "$(python3 -c 'import json,sys; print(json.dumps({"command": sys.argv[1], "timeout_seconds": 20}))' "$1")" |
		python3 -c "import json,sys
try: d=json.load(sys.stdin); print(str(d['exit_code'])+'|'+d['stdout'].strip())
except Exception: print('|')"
}
# tcp_from_a <host> <port>: exit code of a 3 s TCP connect attempt from A.
# 124 means the attempt timed out, i.e. the packets were dropped, which is
# what a NetworkPolicy does. Anything else (0 connected, 1 refused or no
# such name, empty if the exec failed) is not proof of a block.
tcp_from_a() { in_a "timeout 3 bash -c '</dev/tcp/$1/$2' 2>/dev/null; echo \$?" | cut -d'|' -f2; }

printf "\nfrom inside a sandbox: allowed\n"
r=$(in_a "getent hosts pypi.org")
[ "${r%%|*}" = 0 ] && [ -n "${r#*|}" ] && pass "DNS (pypi.org resolves)" || fail "DNS" "$r"
r=$(in_a "curl -sS -m 15 -o /dev/null -w '%{http_code}' https://pypi.org/simple/")
[ "${r#*|}" = 200 ] && pass "internet (https://pypi.org → 200)" || fail "internet" "$r"

printf "\nfrom inside a sandbox: blocked\n"
blocked() { # blocked <name> <host> <port>
	local rc; rc=$(tcp_from_a "$2" "$3")
	[ "$rc" = 124 ] && pass "$1 ($2:$3) blocked" || fail "$1 ($2:$3)" "exit ${rc:-?}, want 124 (timed out)"
}
blocked "Postgres"              postgres.sandbox-system 5432
blocked "control plane API"     sandbox-api.sandbox-system 80
blocked "SSH gateway"           sandbox-gateway.sandbox-system 22
blocked "Kubernetes API"        kubernetes.default 443
blocked "another sandbox's sshd" "$B_IP" 22
blocked "node (kubelet)"        "$NODE_IP" 10250
blocked "node (API server)"     "$NODE_IP" 8443
blocked "gateway NodePort"      "$NODE_IP" 30022 # step 3.6; a sandbox must not loop back in
blocked "cloud metadata"        169.254.169.254 80

printf "\ninto a sandbox\n"
r=$(in_a "echo ok")
[ "$r" = "0|ok" ] && pass "API → sandbox:22 (exec works)" || fail "API → sandbox:22" "$r"

# The sandbox image as this cluster names it (minikube: built on the node;
# a cloud cluster: in its registry), from the API's own settings.
env_of() { kubectl -n sandbox-system get deploy sandbox-api -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name=='$1')].value}"; }
IMG="$(env_of SANDBOX_IMAGE_REPO)ubuntu-base:$(env_of SANDBOX_IMAGE_TAG)"
[ "$IMG" != "ubuntu-base:" ] || IMG=sandbox/ubuntu-base:0.1
kubectl -n default run netpol-outsider --image="$IMG" --image-pull-policy=IfNotPresent \
	--restart=Never --command -- sleep 120 >/dev/null 2>&1
kubectl -n default wait --for=condition=Ready pod/netpol-outsider --timeout=60s >/dev/null ||
	{ echo "outsider pod not ready"; exit 1; }
outsider() { kubectl -n default exec netpol-outsider -- timeout 3 bash -c "</dev/tcp/$1/$2" >/dev/null 2>&1; echo $?; }
rc=$(outsider "$B_IP" 22)
[ "$rc" = 124 ] && pass "unrelated pod → sandbox:22 blocked" || fail "unrelated pod → sandbox:22" "exit $rc, want 124"
rc=$(outsider postgres.sandbox-system 5432)
[ "$rc" = 124 ] && pass "unrelated pod → Postgres blocked" || fail "unrelated pod → Postgres" "exit $rc, want 124"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/readyz")
[ "$code" = 200 ] && pass "API → Postgres still works (/readyz 200)" || fail "API → Postgres" "readyz $code"

printf "\n%d passed, %d failed\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
