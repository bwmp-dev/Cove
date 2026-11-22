#!/bin/sh
set -eu
# A private Docker daemon keeps acceptance workloads, networks and address pools off the host daemon.
dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock >/var/log/dockerd.log 2>&1 &
until docker info >/dev/null 2>&1; do sleep 1; done
chgrp root /var/run/docker.sock
chmod 660 /var/run/docker.sock
su -s /bin/sh cove -c 'GIT_SSL_CAINFO=/run/cove-ca/ca.pem exec /usr/local/bin/cove-worker daemon' &
exec /usr/sbin/sshd -D -e -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=no
