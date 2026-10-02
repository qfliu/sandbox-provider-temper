# Sandbox images (D2). One Dockerfile, two targets, so `dev` builds on `base`
# without needing a registry:
#   minikube image build -t sandbox/ubuntu-base:0.1 --build-opt=opt=target=base code/images/sandbox
#   minikube image build -t sandbox/ubuntu-dev:0.1  --build-opt=opt=target=dev  code/images/sandbox
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
