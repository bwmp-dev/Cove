#!/bin/sh
set -eu
su -s /bin/sh cove -c '/usr/local/bin/cove-worker daemon' &
exec /usr/sbin/sshd -D -e -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=no
