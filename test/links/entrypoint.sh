#!/bin/sh
set -eu
dockerd-entrypoint.sh dockerd >/var/log/dockerd.log 2>&1 &
i=0
until docker info >/dev/null 2>&1; do
  i=$((i+1))
  [ "$i" -lt 90 ] || { cat /var/log/dockerd.log >&2; exit 1; }
  sleep 1
done
chgrp cove /var/run/docker.sock
chmod 660 /var/run/docker.sock
su -s /bin/sh cove -c 'exec /usr/local/bin/cove-worker daemon >>/var/lib/cove/worker.log 2>&1' &
exec /usr/sbin/sshd -D -e -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=no
