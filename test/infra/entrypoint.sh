#!/bin/sh
# Inject SSH public key from /tmp/authorized_key.pub (bind-mounted or docker-cp'd)
# into both root and agent authorized_keys, then start sshd.
set -e

KEY_FILE="/tmp/authorized_key.pub"
if [ -f "$KEY_FILE" ]; then
  cp "$KEY_FILE" /root/.ssh/authorized_keys
  cp "$KEY_FILE" /home/agent/.ssh/authorized_keys
  chmod 600 /root/.ssh/authorized_keys /home/agent/.ssh/authorized_keys
  chown agent:agent /home/agent/.ssh/authorized_keys
fi

exec /usr/sbin/sshd -D -e
