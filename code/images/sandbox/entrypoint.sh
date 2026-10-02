#!/bin/sh
# Sandbox container entrypoint: the sandbox's host key, then sshd in the
# foreground (logs to stderr so `kubectl logs` shows them).
set -e

# Host key: the platform provides one per sandbox (D29) so it can verify it
# reached the right sandbox. Hand-made test pods without one get a random key.
if [ -f /etc/sandbox-hostkey/ssh_host_ed25519_key ]; then
  install -m 600 /etc/sandbox-hostkey/ssh_host_ed25519_key /etc/ssh/ssh_host_ed25519_key
  ssh-keygen -y -f /etc/ssh/ssh_host_ed25519_key > /etc/ssh/ssh_host_ed25519_key.pub
else
  ssh-keygen -q -t ed25519 -N "" -f /etc/ssh/ssh_host_ed25519_key
fi
mkdir -p /run/sshd

# /home/user is an emptyDir (D8) that starts empty and root-owned, hiding
# the image's dotfiles; restore them on first start.
[ -f /home/user/.bashrc ] || cp -a /etc/skel/. /home/user/
chown -R user:user /home/user

exec /usr/sbin/sshd -D -e
