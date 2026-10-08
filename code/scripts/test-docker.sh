#!/usr/bin/env bash
# Docker inside a sandbox (step 8.2, D62): creates an ubuntu-docker sandbox
# through the API and, as the sandbox's `user` (no sudo), checks what the
# assigner asked for: a recent Docker, `docker build`, `docker run`, published
# ports (-p), and `docker compose` with service names and `ports:`. Then that
# the sandbox's isolation still holds for nested containers (no cloud
# metadata, no cluster; a nested --privileged container gets no more than
# the sandbox's own capabilities), and that forged source addresses don't
# leave the node: gvisor-docker lets the sandbox write raw Ethernet frames
# onto its pod interface (packet sockets), so a capture pod on the sandbox's
# node watches while the sandbox writes one frame with its own address (must
# reach the node's uplink, after NAT) and two with forged ones (must reach
# the node, proving they left the sandbox, but not its uplink). Prints
# PASS/FAIL per check; exits non-zero on any failure.
#
# Usage (from the project root, with the API deployed; minikube or AKS):
#   source ./env.sh && ./code/scripts/test-docker.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
: "${KUBECONFIG:?source ./env.sh first}"
PORT="${DOCKER_TEST_PORT:-18097}"
API="http://127.0.0.1:$PORT/v1"
KEY="$(cat "${SANDBOX_KEYFILE:-$ROOT/.devenv/state/admin-api-key}")"
mkdir -p "$ROOT/.devenv/state/tmp"
TMP="$(mktemp -d "$ROOT/.devenv/state/tmp/docker.XXXXXX")"
(umask 077 && printf 'Authorization: Bearer %s\n' "$KEY" >"$TMP/auth")
# Own name per run: a previous run's namespace may still be terminating.
CAPNS="docker-test-capture-$$"
# A pinned network toolbox for the capture pod (tcpdump), multi-arch.
NETSHOOT="nicolaka/netshoot:v0.15"
PASSED=0 FAILED=0 SB=""

pass() { PASSED=$((PASSED + 1)); printf "  \033[32mPASS\033[0m  %s\n" "$1"; }
fail() { FAILED=$((FAILED + 1)); printf "  \033[31mFAIL\033[0m  %s  (%s)\n" "$1" "$2"; }
die() { printf "setup failed: %s\n" "$1" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$CAPNS" --ignore-not-found --wait=false >/dev/null 2>&1
	[ -n "$SB" ] && curl -s -o /dev/null -X DELETE -H @"$TMP/auth" "$API/sandboxes/$SB"
	[ -n "${PF:-}" ] && { kill "$PF"; wait "$PF"; } 2>/dev/null
	rm -rf "$TMP"
}
trap cleanup EXIT

kubectl -n sandbox-system port-forward svc/sandbox-api "$PORT:80" >/dev/null 2>&1 &
PF=$!
for _ in $(seq 1 40); do curl -s -m 1 -o /dev/null "http://127.0.0.1:$PORT/healthz" && break; sleep 0.25; done

ssh-keygen -q -t ed25519 -N "" -f "$TMP/k"
T0=$(date +%s)
SB=$(curl -s -H @"$TMP/auth" -X POST "$API/sandboxes" \
	-d "{\"image\":\"ubuntu-docker\",\"ssh_public_key\":\"$(cat "$TMP/k.pub")\"}" |
	python3 -c "import json,sys; print(json.load(sys.stdin)['id'])") || die "could not create an ubuntu-docker sandbox"
s=""
for _ in $(seq 1 120); do
	s=$(curl -s -H @"$TMP/auth" "$API/sandboxes/$SB" | python3 -c "import json,sys; print(json.load(sys.stdin)['status'])")
	[ "$s" = running ] && break
	sleep 1
done
[ "$s" = running ] || die "sandbox $SB not running (status: $s)"
printf "sandbox %s (ubuntu-docker), running after %d s\n" "$SB" "$(($(date +%s) - T0))"

# run <command> [timeout]: exec in the sandbox as `user`; prints
# "<exit_code>|<last output line>"
run() {
	curl -s -H @"$TMP/auth" -X POST "$API/sandboxes/$SB/exec" \
		-d "$(python3 -c 'import json,sys; print(json.dumps({"command": sys.argv[1], "timeout_seconds": int(sys.argv[2])}))' "$1" "${2:-300}")" |
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
# put <path> < content: upload a file into /home/user
put() { curl -s -o /dev/null -w '%{http_code}' -H @"$TMP/auth" -X PUT --data-binary @- "$API/sandboxes/$SB/files?path=$1"; }

printf "\nDocker works, as the sandbox's user (no sudo)\n"
works "Docker Engine 29 answers"      "docker version --format '{{.Server.Version}}'" "29."
works "Compose plugin 5"              "docker compose version --short | grep -q '^5\\.' && echo compose-ok" "compose-ok"
works "storage on the sandbox's tmpfs" "[ \"\$(docker info --format '{{.DockerRootDir}}')\" = /var/lib/docker ] && findmnt -n -o FSTYPE /var/lib/docker" "tmpfs"
works "docker pull"                   "docker pull -q alpine:3.22" "alpine:3.22"
works "docker run, with internet"     "docker run --rm alpine:3.22 sh -c 'wget -qO- https://example.com | grep -o \"<title>[^<]*\"'" "<title>"

run "mkdir -p app" >/dev/null
printf 'FROM alpine:3.22\nRUN apk add --no-cache python3\nCMD ["python3", "-m", "http.server", "8000"]\n' | put app/Dockerfile >/dev/null
cat <<'EOF' | put app/compose.yaml >/dev/null
services:
  web:
    build: .
    ports: ["8090:8000"]
  redis:
    image: redis:8-alpine
  client:
    image: redis:8-alpine
    depends_on: [redis, web]
    # PONG from redis, then 2: the web page's title and heading.
    command: sh -c 'sleep 2; redis-cli -h redis ping; for i in 1 2 3 4 5; do wget -qO- -T5 http://web:8000/ && break; sleep 1; done | grep -c "Directory listing"'
EOF
works "docker build"                  "cd app && docker build -q -t web . && docker image inspect web --format ok" "ok"
# curl retries while the server starts.
GET="curl -s --retry 10 --retry-delay 1 --retry-all-errors -o /dev/null -w '%{http_code}'"
works "docker run -p (published port)" "docker run -d --name w1 -p 8081:8000 web >/dev/null && $GET http://127.0.0.1:8081/" "200"
works "two containers, same internal port" "docker run -d --name w2 -p 8082:8000 web >/dev/null && $GET http://127.0.0.1:8082/" "200"
works "published port on the sandbox's own address" "$GET http://\$(hostname -i | cut -d' ' -f1):8081/" "200"
run "docker rm -f w1 w2" >/dev/null
works "docker compose up --build"     "cd app && docker compose up -d --build --quiet-pull 2>&1 | tail -1 && sleep 6 && docker compose ps --format '{{.Service}}={{.State}}' | sort | tr '\n' ' '" "web=running"
works "compose: services reach each other by name" "cd app && docker compose logs --no-log-prefix client | tr '\n' ' '" "PONG 2"
works "compose: ports: published"      "$GET http://127.0.0.1:8090/" "200"
works "compose down"                   "cd app && docker compose down >/dev/null 2>&1; echo remaining=\$(docker ps -q | wc -l)" "remaining=0"

printf "\nisolation still holds for nested containers\n"
# TCP connects, not HTTP requests: an HTTP error (401, 400) would also make
# wget fail although the address is reachable (review of #73). The control
# shows the same probe does connect where allowed; the Kubernetes API is
# probed by its Service IP so a DNS failure can't pass for a block.
KIP=$(kubectl get svc kubernetes -n default -o jsonpath='{.spec.clusterIP}')
tcp() { run "docker run --rm alpine:3.22 sh -c 'nc -z -w 3 $1 $2; echo connect=\$?'"; }
r=$(tcp example.com 443)
[[ "$r" == *"connect=0" ]] && pass "nested container connects to the internet (control)" || fail "nested connect control" "$r"
r=$(tcp 169.254.169.254 80)
[[ "$r" == *"connect=1" ]] && pass "nested container can't reach the cloud metadata address" || fail "metadata" "$r"
r=$(tcp "$KIP" 443)
[[ "$r" == *"connect=1" ]] && pass "nested container can't reach the Kubernetes API ($KIP)" || fail "kubernetes API" "$r"
# Inside gVisor, --privileged can only hand out what dockerd holds: the
# sandbox's 16 capabilities (D61), whose bounding set is a82435fb.
r=$(run "docker run --rm --privileged alpine:3.22 grep CapBnd /proc/self/status")
[[ "$r" == "0|"*"00000000a82435fb" ]] && pass "nested --privileged gets only the sandbox's 16 capabilities" || fail "nested --privileged" "$r"

printf "\nforged source addresses don't leave the node\n"
NODE=$(kubectl -n sandboxes get pod "$SB" -o jsonpath='{.spec.nodeName}')
kubectl create namespace "$CAPNS" >/dev/null
kubectl label namespace "$CAPNS" pod-security.kubernetes.io/enforce=privileged >/dev/null
kubectl -n "$CAPNS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: capture}
spec:
  nodeName: $NODE
  hostNetwork: true
  automountServiceAccountToken: false
  containers:
  - name: c
    image: $NETSHOOT
    securityContext: {capabilities: {add: [NET_ADMIN, NET_RAW]}}
    # Every interface of the node, so a forged frame is seen arriving from
    # the pod; the uplink is where the default route goes.
    command: [sh, -c, 'echo "uplink=\$(ip route show default | sed -n "s/.* dev \\([^ ]*\\).*/\\1/p" | head -1)"; exec tcpdump -l -n -i any "udp and dst host 1.1.1.1 and dst portrange 40021-40023"']
EOF
kubectl -n "$CAPNS" wait --for=condition=Ready pod/capture --timeout=180s >/dev/null || die "capture pod didn't start"
for _ in $(seq 1 30); do kubectl -n "$CAPNS" logs capture 2>/dev/null | grep -q "listening on" && break; sleep 1; done
# Raw IPv4/UDP frames on eth0 through a packet socket: this skips the
# sandbox's own IP stack and NAT. The gateway's MAC is learned from a frame
# it sends us (gVisor has no neighbour table to read).
cat <<'EOF' | put spoof.py >/dev/null
import socket, struct, sys
def csum(b):
    if len(b) % 2: b += b"\0"
    s = sum(struct.unpack("!%dH" % (len(b) // 2), b)); s = (s >> 16) + (s & 0xffff); s += s >> 16
    return ~s & 0xffff
def ipudp(src, dport):
    udp = struct.pack("!HHHH", 40000, dport, 13, 0) + b"spoof"
    ip = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(udp), 0, 0, 64, 17, 0,
                     socket.inet_aton(src), socket.inet_aton("1.1.1.1"))
    return ip[:10] + struct.pack("!H", csum(ip)) + ip[12:] + udp
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0800))
s.bind(("eth0", 0)); s.settimeout(5)
t = socket.socket(); t.settimeout(3)
try: t.connect(("1.1.1.1", 443))
except OSError: pass
# Both MACs from the reply of 1.1.1.1, which came through the gateway
# (getsockname gives zeros in gVisor; another pod's frame could come from
# elsewhere on a bridge CNI).
gw = mine = None
for _ in range(200):
    f, addr = s.recvfrom(2048)
    if addr[2] == socket.PACKET_HOST and f[26:30] == socket.inet_aton("1.1.1.1"):
        mine, gw = f[0:6], f[6:12]; break
assert gw, "no frame from the gateway"
for src, dport in ((sys.argv[1], 40021), ("10.99.99.99", 40022), ("8.8.4.4", 40023)):
    for _ in range(3): s.send(gw + mine + b"\x08\x00" + ipudp(src, dport))
print("sent")
EOF
works "frames written from the sandbox (own address + 2 forged)" "sudo python3 spoof.py \$(hostname -i | cut -d' ' -f1)" "sent"
sleep 3
CAP=$(kubectl -n "$CAPNS" logs capture 2>/dev/null)
UPLINK=$(sed -n 's/^uplink=//p' <<<"$CAP")
up() { awk -v u="$UPLINK" -v p=".$1:" '$2 == u && index($0, p)' <<<"$CAP" | grep -c .; }
seen() { grep -c "\.$1:" <<<"$CAP"; }
[ "$(up 40021)" -gt 0 ] && pass "own-address frame reaches the node's uplink ($UPLINK)" ||
	fail "own-address frame on the uplink ($UPLINK)" "$(tail -4 <<<"$CAP" | tr '\n' ' ')"
[ "$(seen 40022)" -gt 0 ] && [ "$(seen 40023)" -gt 0 ] && pass "forged frames left the sandbox (seen on the node)" ||
	fail "forged frames seen on the node" "$(tail -4 <<<"$CAP" | tr '\n' ' ')"
# Only meaningful when the own-address frame did get through.
[ "$(up 40021)" -gt 0 ] && [ "$(up 40022)" = 0 ] && [ "$(up 40023)" = 0 ] && pass "forged frames don't reach the node's uplink" ||
	fail "forged frames reached the uplink" "$(grep -E '\.4002[23]:' <<<"$CAP" | head -3 | tr '\n' ' ')"

printf "\n%d passed, %d failed\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
