#!/bin/sh
set -e
echo "tunnel:${SSH_PASSWORD}" | chpasswd
mkdir -p /home/tunnel/.ssh
if [ -n "$SSH_AUTHORIZED_KEY" ]; then echo "$SSH_AUTHORIZED_KEY" > /home/tunnel/.ssh/authorized_keys; fi
chown -R tunnel:tunnel /home/tunnel/.ssh && chmod 700 /home/tunnel/.ssh
exec /usr/sbin/sshd -D -e
