#!/usr/bin/env bash
# Verifies sandbox pod hardening (step 5.2, D45) inside a real ubuntu-dev
# sandbox: everyday work still works (sudo, apt, pip, gcc, chown) while
# what was taken away is refused (raw sockets, mknod), and even root's
# capability bounding set is exactly the allowed list. Also checks the
# admission policy rejects pods that don't follow the rules, including
# Docker's capabilities outside the gvisor-docker runtime (D61) and the host
# rules Pod Security baseline used to enforce. Prints
# PASS/FAIL per check; exits non-zero on any failure.
#
# Usage (from the project root, with the API deployed):
#   source ./env.sh && ./code/scripts/test-sandbox-hardening.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
PORT="${HARDEN_PORT:-18098}"
API="http://127.0.0.1:$PORT/v1"
KEY="$(cat "${SANDBOX_KEYFILE:-$ROOT/.devenv/state/admin-api-key}")"
TMP="$(mktemp -d "$ROOT/.devenv/state/tmp/harden.XXXXXX")"
# The key goes to curl in a header file, never on its command line (ps).
(umask 077 && printf 'Authorization: Bearer %s\n' "$KEY" >"$TMP/auth")
PASSED=0 FAILED=0 SB=""

pass() { PASSED=$((PASSED + 1)); printf "  \033[32mPASS\033[0m  %s\n" "$1"; }
fail() { FAILED=$((FAILED + 1)); printf "  \033[31mFAIL\033[0m  %s  (%s)\n" "$1" "$2"; }
die() { printf "setup failed: %s\n" "$1" >&2; exit 1; }

cleanup() {
	kubectl -n sandboxes delete serviceaccount hardening-probe --ignore-not-found >/dev/null 2>&1
	[ -n "$SB" ] && curl -s -o /dev/null -X DELETE -H @"$TMP/auth" "$API/sandboxes/$SB"
	[ -n "${PF:-}" ] && { kill "$PF"; wait "$PF"; } 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT

kubectl -n sandbox-system port-forward svc/sandbox-api "$PORT:80" >/dev/null 2>&1 &
PF=$!
for _ in $(seq 1 40); do curl -s -m 1 -o /dev/null "http://127.0.0.1:$PORT/healthz" && break; sleep 0.25; done

ssh-keygen -q -t ed25519 -N "" -f "$TMP/k"
SB=$(curl -s -H @"$TMP/auth" -X POST "$API/sandboxes" \
	-d "{\"image\":\"ubuntu-dev\",\"ssh_public_key\":\"$(cat "$TMP/k.pub")\"}" |
	python3 -c "import json,sys; print(json.load(sys.stdin)['id'])") || die "could not create a sandbox"
s=""
for _ in $(seq 1 90); do
	s=$(curl -s -H @"$TMP/auth" "$API/sandboxes/$SB" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")
	[ "$s" = running ] && break
	sleep 1
done
[ "$s" = running ] || die "sandbox $SB not running (status: $s)"
printf "sandbox %s (ubuntu-dev)\n" "$SB"

# run <command>: exec in the sandbox; prints "<exit_code>|<last output line>"
run() {
	curl -s -H @"$TMP/auth" -X POST "$API/sandboxes/$SB/exec" \
		-d "$(python3 -c 'import json,sys; print(json.dumps({"command": sys.argv[1], "timeout_seconds": 240}))' "$1")" |
		python3 -c "import json,sys
try:
    d=json.load(sys.stdin); out=(d['stdout']+d['stderr']).strip().splitlines()
    print(str(d['exit_code'])+'|'+(out[-1] if out else ''))
except Exception: print('|exec failed')"
}
# works <name> <command> <expected substring of the last line>
works() {
	local r; r=$(run "$2")
	[ "${r%%|*}" = 0 ] && [[ "${r#*|}" == *"$3"* ]] && pass "$1" || fail "$1" "$r"
}

printf "\neveryday work still works\n"
works "sudo"                    "sudo whoami" "root"
works "apt install (as root)"   "sudo apt-get update -qq >/dev/null 2>&1; sudo apt-get install -y -qq jq >/dev/null 2>&1; jq --version" "jq-"
works "pip install"             "python3 -m pip install --user --quiet --break-system-packages six >/dev/null 2>&1; python3 -c 'import six; print(\"six ok\")'" "six ok"
works "gcc compile and run"     "printf '#include <stdio.h>\nint main(){puts(\"c ok\");}' > /tmp/h.c && gcc /tmp/h.c -o /tmp/h && /tmp/h" "c ok"
works "chown as root"           "touch /tmp/f && sudo chown root:root /tmp/f && stat -c %U /tmp/f" "root"
# Threads and curl's threaded DNS lookup: these broke when runsc's
# oci-seccomp was tried (review of #45), so they guard against it.
works "threads start"           "python3 -c 'import threading; t=threading.Thread(target=print, args=(\"threads ok\",)); t.start(); t.join()'" "threads ok"
works "curl to the internet"    "curl -sS -m 15 -o /dev/null -w '%{http_code}' https://pypi.org/simple/" "200"

printf "\nwhat was taken away is refused (even for root)\n"
# Raw sockets are also off in gVisor by default (runsc net-raw=false), so
# this shows the sandbox's behaviour; the bounding set below is what proves
# NET_RAW and MKNOD are gone.
r=$(run "sudo python3 -c 'import socket; socket.socket(socket.AF_INET, socket.SOCK_RAW, 1)' 2>&1 | tail -1")
[[ "$r" == *"Operation not permitted"* ]] && pass "raw socket refused" || fail "raw socket" "$r"
r=$(run "sudo mknod /tmp/fifo p && sudo mknod /tmp/null2 c 1 3 2>/dev/null; echo rc=\$?")
[[ "$r" == *"|rc=1" ]] && pass "device node refused (a FIFO is fine)" || fail "mknod" "$r"
r=$(run "sudo grep CapBnd /proc/self/status")
bnd=$(printf '%s' "${r#*|}" | awk '{print $2}')
[ "$bnd" = "00000000200404fb" ] && pass "root's capability bounding set is exactly the allowed 10 ($bnd)" ||
	fail "capability bounding set" "$r"

printf "\nadmission policy rejects non-conforming pods\n"
# A real (throwaway) service account, so the "another service account" probe
# reaches the policy instead of failing the account lookup first.
kubectl -n sandboxes create serviceaccount hardening-probe >/dev/null 2>&1
cat >"$TMP/pod.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: hardening-probe
  namespace: sandboxes
  labels: {app.kubernetes.io/managed-by: sandbox-api}
spec:
  runtimeClassName: gvisor
  automountServiceAccountToken: false
  securityContext: {seccompProfile: {type: RuntimeDefault}}   # pod level; a container may not override it
  containers:
  - name: c
    image: sandbox/ubuntu-base:0.1
    resources: {limits: {cpu: "1", memory: 1Gi}}
    securityContext:
      capabilities: {drop: [ALL], add: [CHOWN]}
      seccompProfile: {type: RuntimeDefault}
EOF
# probe <name> <perl expression | "+<line to append at pod level>">
probe() {
	if [[ "$2" == +* ]]; then
		{ cat "$TMP/pod.yaml"; printf '  %s\n' "${2#+}"; } | kubectl apply --dry-run=server -f - 2>&1 | tail -1
	else
		perl -pe "$2" "$TMP/pod.yaml" | kubectl apply --dry-run=server -f - 2>&1 | tail -1
	fi
}
# Edits used below: run under gvisor-docker; add a line to the container's
# securityContext.
DOCKER='s/runtimeClassName: gvisor$/runtimeClassName: gvisor-docker/;'
CSC() { printf 's/^(      capabilities: .*)$/$1\\n      %s/;' "$1"; }
admitted() { # admitted <name> <expr>
	local out; out=$(probe "$1" "$2")
	[[ "$out" == *"created (server dry run)"* ]] && pass "$1 is admitted (dry run)" || fail "$1" "$out"
}
admitted "a conforming pod" 's/^//'
admitted "a gvisor-docker pod with Docker's capabilities" \
	"$DOCKER"'s/add: \[CHOWN\]/add: [CHOWN, MKNOD, NET_RAW, SETFCAP, SETPCAP, SYS_ADMIN, NET_ADMIN]/'
for c in "runs without gVisor|\$_='' if /runtimeClassName/|must run under gVisor" \
	"keeps default capabilities|s/drop: \[ALL\], //|drop ALL" \
	"adds MKNOD|s/add: \[CHOWN\]/add: [CHOWN, MKNOD]/|may only add back" \
	"adds SYS_ADMIN without gvisor-docker|s/add: \[CHOWN\]/add: [CHOWN, SYS_ADMIN]/|may only add back" \
	"adds SYS_PTRACE under gvisor-docker|${DOCKER}s/add: \[CHOWN\]/add: [CHOWN, SYS_ADMIN, SYS_PTRACE]/|may only add back" \
	"adds ALL under gvisor-docker|${DOCKER}s/add: \[CHOWN\]/add: [ALL]/|may only add back" \
	"is privileged under gvisor-docker|${DOCKER}$(CSC 'privileged: true')|must not be privileged" \
	"lacks a seccomp profile|\$_='' if /seccompProfile/|seccomp" \
	"overrides seccomp with Localhost|s/^      seccompProfile: \{type: RuntimeDefault\}/      seccompProfile: {type: Localhost, localhostProfile: x.json}/|seccomp" \
	"lacks CPU and memory limits|\$_='' if /resources:/|CPU and memory limits" \
	"mounts a service account token|+volumes: [{name: tok, projected: {sources: [{serviceAccountToken: {path: token}}]}}]|service account token" \
	"uses another service account|+serviceAccountName: hardening-probe|default service account" \
	"uses the host network|+hostNetwork: true|must not use the host's network, PID or IPC namespace" \
	"uses the host PID namespace|+hostPID: true|must not use the host's network, PID or IPC namespace" \
	"uses the host IPC namespace|+hostIPC: true|must not use the host's network, PID or IPC namespace" \
	"mounts a hostPath volume|+volumes: [{name: h, hostPath: {path: /}}]|must not mount hostPath volumes" \
	"uses a host port|s/^(    resources: )/    ports: [{containerPort: 22, hostPort: 2222}]\n\$1/|must not use host ports" \
	"sets a sysctl|s/^  securityContext: \{seccompProfile: \{type: RuntimeDefault\}\}/  securityContext: {seccompProfile: {type: RuntimeDefault}, sysctls: [{name: net.ipv4.ping_group_range, value: \"0 0\"}]}/|must not set sysctls" \
	"runs AppArmor-unconfined|$(CSC 'appArmorProfile: {type: Unconfined}')|must not run AppArmor-unconfined" \
	"sets SELinux options|$(CSC 'seLinuxOptions: {type: spc_t}')|must not set SELinux options" \
	"sets pod-level SELinux options|s/^  securityContext: \{seccompProfile: \{type: RuntimeDefault\}\}/  securityContext: {seccompProfile: {type: RuntimeDefault}, seLinuxOptions: {type: spc_t}}/|must not set SELinux options" \
	"has an AppArmor-unconfined annotation|s/^(  labels: .*)\$/\$1\n  annotations: {container.apparmor.security.beta.kubernetes.io\/c: unconfined}/|must not run AppArmor-unconfined"; do
	IFS='|' read -r name expr want <<<"$c"
	out=$(probe "$name" "$expr")
	[[ "$out" == *"$want"* ]] && pass "pod that $name is denied" || fail "pod that $name" "$out"
done

printf "\n%d passed, %d failed\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
