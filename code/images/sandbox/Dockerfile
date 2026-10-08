# Sandbox images (D2, D62). One Dockerfile, three targets, so `dev` builds on
# `base` and `docker` on `dev` without needing a registry:
#   minikube image build -t sandbox/ubuntu-base:0.2   --build-opt=opt=target=base   code/images/sandbox
#   minikube image build -t sandbox/ubuntu-dev:0.2    --build-opt=opt=target=dev    code/images/sandbox
#   minikube image build -t sandbox/ubuntu-docker:0.2 --build-opt=opt=target=docker code/images/sandbox
#
# sshd runs as root (it needs to switch users); people and the platform log
# in as the unprivileged `user`. Pods only trust the platform-internal key
# (D7), mounted from a Secret at /etc/sandbox/authorized_keys.

FROM ubuntu:24.04 AS base

ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      openssh-server openssh-sftp-server sudo ca-certificates \
      git curl wget less vim-tiny procps build-essential \
 && rm -rf /var/lib/apt/lists/*

# Ubuntu 24.04 images ship a uid-1000 `ubuntu` user; replace it with `user`.
# useradd leaves the password as "!" (locked), and with UsePAM=no sshd rejects
# locked accounts even for valid keys, so set "*" (no password, not locked).
RUN userdel -r ubuntu 2>/dev/null || true \
 && useradd --create-home --uid 1000 --shell /bin/bash user \
 && usermod -p '*' user \
 && echo 'user ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/user \
 && chmod 440 /etc/sudoers.d/user

COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 755 /usr/local/bin/entrypoint.sh \
 && rm -f /etc/ssh/ssh_host_*

WORKDIR /home/user
EXPOSE 22
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]


FROM base AS dev

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv nodejs npm \
 && rm -rf /var/lib/apt/lists/*


# Docker inside the sandbox (D60, D62): Docker Engine + Compose + Buildx,
# pinned. Runs only under the gvisor-docker RuntimeClass (D61), which the API
# picks for this image. dockerd starts the way gVisor's "Docker in gVisor"
# guide does (start-dockerd.sh), before sshd, so "running" means Docker is
# ready too.
FROM dev AS docker

ARG DOCKER_VERSION=5:29.8.2-1~ubuntu.24.04~noble
ARG COMPOSE_VERSION=5.6.0-1~ubuntu.24.04~noble
ARG BUILDX_VERSION=0.37.1-1~ubuntu.24.04~noble
ARG CONTAINERD_VERSION=2.3.6-1~ubuntu.24.04~noble
# Docker's apt signing key, checked against its published fingerprint.
ARG DOCKER_KEY_FPR=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
RUN apt-get update \
 && apt-get install -y --no-install-recommends gnupg iproute2 iptables \
 && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /tmp/docker.asc \
 && [ "$(gpg --show-keys --with-colons /tmp/docker.asc | awk -F: '$1 == "pub" {n++} /^fpr/ && !f {f = $10} END {print n ":" f}')" = "1:$DOCKER_KEY_FPR" ] \
 && install -m 0755 -d /etc/apt/keyrings \
 && gpg --dearmor -o /etc/apt/keyrings/docker.gpg /tmp/docker.asc \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu noble stable" \
      > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      docker-ce="$DOCKER_VERSION" docker-ce-cli="$DOCKER_VERSION" \
      containerd.io="$CONTAINERD_VERSION" \
      docker-compose-plugin="$COMPOSE_VERSION" docker-buildx-plugin="$BUILDX_VERSION" \
 && apt-get purge -y gnupg && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/* /tmp/docker.asc /root/.gnupg \
 && usermod -aG docker user

# gVisor implements iptables' legacy interface, not nftables. dockerd calls
# `iptables` for its embedded DNS (127.0.0.11) in each container even with
# --iptables=false; with Ubuntu's default (nft) that failed and compose
# services couldn't resolve each other.
RUN update-alternatives --set iptables /usr/sbin/iptables-legacy \
 && update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy

COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 755 /usr/local/bin/docker-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
