FROM node:24-alpine AS frontend
WORKDIR /src/frontend
RUN npm install --global pnpm@11.20.0
COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY frontend/ ./
RUN pnpm build

FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS backend
ARG TARGETARCH
ARG BUILD_VERSION=development
ARG BUILD_REVISION=unknown
ARG BUILD_CHANNEL=development
ARG BUILD_SOURCE_GENERATION=
ARG BUILD_SOURCE_ROOT=
ARG BUILD_REVISION_TIME=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags "-X cove/internal/buildinfo.Version=${BUILD_VERSION} -X cove/internal/buildinfo.Revision=${BUILD_REVISION} -X cove/internal/buildinfo.Channel=${BUILD_CHANNEL} -X cove/internal/buildinfo.RevisionTime=${BUILD_REVISION_TIME} -X cove/internal/buildinfo.SourceGeneration=${BUILD_SOURCE_GENERATION} -X cove/internal/buildinfo.SourceRoot=${BUILD_SOURCE_ROOT}" -o /out/cove ./cmd/cove \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/cove-worker-linux-amd64 ./cmd/cove-worker \
 && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /out/cove-worker-linux-arm64 ./cmd/cove-worker

FROM scratch AS binaries
COPY --from=backend /out/ /

FROM docker:29-cli AS worker
ARG TARGETARCH
ENV DOCKER_CONFIG=/tmp/docker-config
RUN apk add --no-cache ca-certificates git
COPY --from=backend /out/cove-worker-linux-${TARGETARCH} /usr/local/bin/cove-worker
ENTRYPOINT ["/usr/local/bin/cove-worker", "daemon"]

FROM alpine:3.23 AS panel
RUN apk add --no-cache ca-certificates git && adduser -D -u 10001 cove
WORKDIR /app
COPY --from=backend /out/ /app/bin/
COPY release-signatures/ /app/bin/
COPY --from=frontend /src/frontend/dist/ /app/frontend/dist/
USER cove
ENV COVE_LISTEN=0.0.0.0:8080
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=6 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/bin/cove"]
