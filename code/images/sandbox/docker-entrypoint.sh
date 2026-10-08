#!/bin/sh
# Entrypoint of the ubuntu-docker image (D62): start dockerd, then hand over
# to the usual entrypoint (host key, sshd). Follows gVisor's "Docker in
# gVisor" guide (images/basic/docker/start-dockerd.sh in google/gvisor):
#   - Docker's storage on a tmpfs: gVisor can't stack overlayfs on its own
#     root or on the /home emptyDir, only on tmpfs. It counts against the
#     sandbox's memory limit; DOCKER_STORAGE_SIZE caps it.
#   - no Docker iptables rules (gVisor's nat table has no MASQUERADE, and
#     NAT only for TCP and UDP): published ports (-p) work through Docker's
#     userland proxy instead, and SNAT rules here give nested containers the
#     internet over TCP and UDP. ICMP (ping) from nested containers on a
#     bridge network can't be NATed and doesn't work; --network=host does.
#     (`iptables` is the legacy backend in this image: gVisor has no
#     nftables.)
#   - Docker's MTU matched to the pod's (fragmented forwarding in gVisor is
#     unreliable, gVisor issue 14011).
# Needs the gvisor-docker runtime (raw sockets, packet writes) and Docker's
# capabilities, which the API gives this image only (D61). If dockerd can't
# start, the sandbox still starts with SSH, and /var/log/dockerd.log says why.
set -u

log() { echo "docker-entrypoint: $*" >&2; }

start_dockerd() {
  if ! mountpoint -q /var/lib/docker; then
    mkdir -p /var/lib/docker
    mount -t tmpfs -o "size=${DOCKER_STORAGE_SIZE:-2g}" tmpfs /var/lib/docker || return 1
  fi
  dev=$(ip route show default | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -1)
  addr=$(ip -4 -o addr show dev "$dev" | sed -n 's/.* inet \([0-9.]*\)\/.*/\1/p' | head -1)
  # From `ip link`: gVisor has no /sys/class/net/<dev>/mtu, and a 1500
  # fallback is too big behind Calico's IP-in-IP (1480), so full-size
  # packets from nested containers were dropped (TLS, image pulls stalled).
  mtu=$(ip -o link show dev "$dev" | sed -n 's/.* mtu \([0-9]*\).*/\1/p')
  [ -n "$dev" ] && [ -n "$addr" ] && [ -n "$mtu" ] || { log "no default route"; return 1; }
  echo 1 >/proc/sys/net/ipv4/ip_forward
  # -C first: a container restart keeps the pod's (gVisor's) network stack.
  for proto in tcp udp; do
    iptables -t nat -C POSTROUTING -o "$dev" -p "$proto" -j SNAT --to-source "$addr" 2>/dev/null ||
      iptables -t nat -A POSTROUTING -o "$dev" -p "$proto" -j SNAT --to-source "$addr" || return 1
  done
  dockerd --iptables=false --ip6tables=false --mtu="$mtu" >>/var/log/dockerd.log 2>&1 &
  # Wait for the API socket (about a second), so the sandbox reports
  # "running" only once Docker answers.
  i=0
  while [ $i -lt 150 ]; do
    docker version >/dev/null 2>&1 && return 0
    kill -0 $! 2>/dev/null || return 1
    sleep 0.1; i=$((i + 1))
  done
  return 1
}

if start_dockerd; then
  log "dockerd ready"
else
  log "dockerd didn't start; see /var/log/dockerd.log"
fi
exec /usr/local/bin/entrypoint.sh
