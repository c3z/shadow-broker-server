# syntax=docker/dockerfile:1
ARG GO_VERSION=1
FROM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /usr/src/app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
# Static binary, no cgo. If the embedded SQLite driver ever requires cgo,
# flip CGO_ENABLED to 1 - the final image is glibc-based, so it still runs.
RUN CGO_ENABLED=0 go build -v -o /run-app .


FROM debian:bookworm-slim

# ca-certificates: needed for outbound TLS (Turso, etc.), otherwise:
#   tls: failed to verify certificate: x509: certificate signed by unknown authority
# curl: used by the docker-compose healthcheck
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && update-ca-certificates

# Run as an unprivileged user
RUN groupadd --system broker \
    && useradd --system --gid broker --home-dir /app broker

WORKDIR /app

# Embedded SQLite database lives here (file:/data/broker.db via SHADOW_BROKER_CONFIG).
# Mount a named volume at /data to persist it across container rebuilds.
RUN mkdir -p /data && chown broker:broker /data
VOLUME ["/data"]

COPY --from=builder /run-app /usr/local/bin/run-app

# The server binds to loopback by default (safe for bare-metal runs). Inside a
# container it must listen on all interfaces of its own namespace; restrict the
# host side with `-p 127.0.0.1:8080:8080` instead.
ENV BIND_HOST=0.0.0.0

USER broker
EXPOSE 8080
CMD ["run-app"]
