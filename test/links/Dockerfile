# Disposable worker node for private link acceptance: its own Docker daemon, SSH and Cove worker.
FROM docker:29-dind
RUN apk add --no-cache openssh && adduser -D -h /var/lib/cove cove && passwd -u cove && mkdir -p /run/sshd /run/cove /var/lib/cove/.ssh && chown -R cove:cove /run/cove /var/lib/cove && ssh-keygen -A
COPY cove-worker /usr/local/bin/cove-worker
COPY entrypoint.sh /entrypoint-links.sh
RUN chmod 755 /entrypoint-links.sh /usr/local/bin/cove-worker
ENV DOCKER_TLS_CERTDIR=
ENTRYPOINT ["/entrypoint-links.sh"]
