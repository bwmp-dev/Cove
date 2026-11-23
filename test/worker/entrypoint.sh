#!/bin/sh
set -eu
if [ -n "${COVE_TUNNEL_FIXTURE_OWNER:-}" ]; then
    mkdir -p /fixture-tools
    cp /tmp/tunnel-docker-shim /fixture-tools/docker
    chmod 755 /fixture-tools/docker
    chmod 644 /var/lib/cove/tunnel-fixture-ca.pem
fi
su -s /bin/sh cove -c 'if [ -n "${COVE_TUNNEL_FIXTURE_OWNER:-}" ]; then export PATH=/fixture-tools:$PATH; fi; exec /usr/local/bin/cove-worker daemon' &
exec /usr/sbin/sshd -D -e -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=no
