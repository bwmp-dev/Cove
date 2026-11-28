FROM docker:29-dind
RUN apk add --no-cache openssh git && adduser -D -h /var/lib/cove cove && addgroup cove root && passwd -u cove && mkdir -p /run/sshd /run/cove /var/lib/cove/.ssh && chown -R cove:cove /run/cove /var/lib/cove && ssh-keygen -A
COPY bin/cove-worker-linux-amd64 /usr/local/bin/cove-worker
COPY test/previews/entrypoint.sh /usr/local/bin/cove-fixture
RUN chmod 755 /usr/local/bin/cove-fixture /usr/local/bin/cove-worker
ENV COVE_ACME_DIRECTORY=https://127.0.0.1:9/directory
ENTRYPOINT ["/usr/local/bin/cove-fixture"]
