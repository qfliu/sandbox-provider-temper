#!/bin/sh
# Entrypoint of the ubuntu-docker image (D62, D64): start dockerd, then hand
# over to the usual entrypoint (host key, sshd). Two runtimes, two recipes:
#
# Under gVisor (gvisor-docker), gVisor's "Docker in gVisor" guide
# (images/basic/docker/start-dockerd.sh in google/gvisor):
#   - no Docker iptables rules (gVisor's nat table has no MASQUERADE, and
#     NAT only for TCP and UDP): published ports (-p) work through Docker's
#     userland proxy instead, and SNAT rules here give nested containers the
#     internet over TCP and UDP. ICMP (ping) from nested containers on a
#     bridge network can't be NATed and doesn't work; --network=host does.
#     (`iptables` is the legacy backend in this image: gVisor has no
#     nftables.)
#   - Docker's MTU matched to the pod's (fragmented forwarding in gVisor is
#     unreliable, gVisor issue 14011).
# Under Kata (a VM with a real Linux kernel), Docker's normal networking
# (its own iptables rules), after three changes that, like the capabilities
# they use, reach only this sandbox's own VM (docker-in-docker's usual setup):
#   - /proc/sys writable (Docker turns on IP forwarding);
#   - a cgroup2 filesystem mounted from inside the container's cgroup
#     namespace (the runtime's is read-only, and its root can lie above the
#     namespace, so runc wrote nested containers' cgroups to the wrong
#     place), with every process
#     moved into a child cgroup so Docker may create siblings
#     ("no internal processes" rule);
#   - the controllers enabled for those children.
# Both: Docker's storage on a tmpfs. Neither runtime can stack overlayfs on
# the pod's root or /home (gVisor's own filesystem; Kata's virtio-fs share);
# tmpfs works in both. It counts against the sandbox's memory limit;
# DOCKER_STORAGE_SIZE caps it.
#
# Needs Docker's capabilities, which the API gives this image only (D61), and
# under gVisor the gvisor-docker runtime (raw sockets, packet writes). If
# dockerd can't start, the sandbox still starts with SSH, and
# /var/log/dockerd.log says why.
set -u
RUNTIME=""

log() { echo "docker-entrypoint: $*" >&2; }

pod_net() { # sets dev, addr, mtu from the pod's default route
  dev=$(ip route show default | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -1)
  addr=$(ip -4 -o addr show dev "$dev" | sed -n 's/.* inet \([0-9.]*\)\/.*/\1/p' | head -1)
  # From `ip link`: gVisor has no /sys/class/net/<dev>/mtu, and a 1500
  # fallback is too big behind Calico's IP-in-IP (1480), so full-size
  # packets from nested containers were dropped (TLS, image pulls stalled).
  mtu=$(ip -o link show dev "$dev" | sed -n 's/.* mtu \([0-9]*\).*/\1/p')
  # Some CNIs (Cilium's tunnel modes) set a smaller MTU on the route than
  # on the link; use the smaller.
  rmtu=$(ip route show default | sed -n 's/.* mtu \([0-9]*\).*/\1/p' | head -1)
  [ -n "$rmtu" ] && [ -n "$mtu" ] && [ "$rmtu" -lt "$mtu" ] && mtu=$rmtu
  [ -n "$dev" ] && [ -n "$addr" ] && [ -n "$mtu" ] ||
    { log "can't read the pod's network (device '$dev', address '$addr', MTU '$mtu')"; return 1; }
}

prepare_gvisor() { # sets DOCKERD_ARGS
  echo 1 >/proc/sys/net/ipv4/ip_forward || { log "can't enable IP forwarding"; return 1; }
  # -C first: a container restart keeps the pod's (gVisor's) network stack.
  for proto in tcp udp; do
    iptables -t nat -C POSTROUTING -o "$dev" -p "$proto" -j SNAT --to-source "$addr" 2>/dev/null ||
      iptables -t nat -A POSTROUTING -o "$dev" -p "$proto" -j SNAT --to-source "$addr" || return 1
  done
  DOCKERD_ARGS="--iptables=false --ip6tables=false --mtu=$mtu"
}

prepare_vm() { # sets DOCKERD_ARGS
  mount -o remount,rw,bind /proc/sys 2>/dev/null || mount -o remount,rw /proc/sys ||
    { log "can't make /proc/sys writable"; return 1; }
  # Always a fresh mount: the runtime's is read-only, and its root may lie
  # outside the container's cgroup namespace. (A restarted container gets
  # the runtime's mount again.)
  umount /sys/fs/cgroup && mount -t cgroup2 cgroup2 /sys/fs/cgroup ||
    { log "can't mount cgroup2"; return 1; }
  mkdir -p /sys/fs/cgroup/init
  # Retried: a process may exit while being moved.
  for _ in 1 2 3 4 5; do
    xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs 2>/dev/null || :
    [ -s /sys/fs/cgroup/cgroup.procs ] || break
  done
  sed -e 's/ / +/g' -e 's/^/+/' </sys/fs/cgroup/cgroup.controllers >/sys/fs/cgroup/cgroup.subtree_control ||
    { log "can't enable cgroup controllers"; return 1; }
  DOCKERD_ARGS="--mtu=$mtu"
}

start_dockerd() {
  if ! mountpoint -q /var/lib/docker; then
    mkdir -p /var/lib/docker
    mount -t tmpfs -o "size=${DOCKER_STORAGE_SIZE:-2g}" tmpfs /var/lib/docker || return 1
  fi
  pod_net || return 1
  case "$(uname -r)" in
  *gvisor*) RUNTIME=gvisor; prepare_gvisor || return 1 ;;
  *) RUNTIME=vm; prepare_vm || return 1 ;;
  esac
  # Restarted if it exits (a crash, or killed for memory): Docker state on
  # the tmpfs survives that, unlike a restart of the whole container.
  (
    while :; do
      # shellcheck disable=SC2086 # DOCKERD_ARGS is a list of flags
      dockerd $DOCKERD_ARGS >>/var/log/dockerd.log 2>&1
      echo "docker-entrypoint: dockerd exited ($?); restarting in 2 s" >>/var/log/dockerd.log
      sleep 2
    done
  ) &
  # Wait for the API socket (about a second), so the sandbox reports
  # "running" only once Docker answers.
  i=0
  while [ $i -lt 150 ]; do
    docker version >/dev/null 2>&1 && return 0
    sleep 0.1; i=$((i + 1))
  done
  return 1
}

if start_dockerd; then
  log "dockerd ready ($RUNTIME)"
else
  log "dockerd didn't start; see /var/log/dockerd.log"
fi
exec /usr/local/bin/entrypoint.sh
