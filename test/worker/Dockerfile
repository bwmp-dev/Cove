FROM docker:29-cli
RUN apk add --no-cache openssh && adduser -D -h /var/lib/cove cove && addgroup cove root && passwd -u cove && mkdir -p /run/sshd /run/cove /var/lib/cove/.ssh && chown -R cove:cove /run/cove /var/lib/cove && ssh-keygen -A
COPY bin/cove-worker-linux-amd64 /usr/local/bin/cove-worker
COPY test/worker/entrypoint.sh /entrypoint.sh
RUN chmod 755 /entrypoint.sh /usr/local/bin/cove-worker
ENTRYPOINT ["/entrypoint.sh"]
