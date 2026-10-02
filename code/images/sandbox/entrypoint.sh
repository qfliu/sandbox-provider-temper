#!/bin/sh
# Sandbox container entrypoint: per-sandbox host keys, then sshd in the
# foreground (logs to stderr so `kubectl logs` shows them).
set -e

ssh-keygen -A >/dev/null
mkdir -p /run/sshd

# /home/user is an emptyDir (D8) that starts empty and root-owned, hiding
# the image's dotfiles; restore them on first start.
[ -f /home/user/.bashrc ] || cp -a /etc/skel/. /home/user/
chown -R user:user /home/user

exec /usr/sbin/sshd -D -e
