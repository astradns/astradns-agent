# Build the agent binary
FROM golang:1.26.1 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY astradns-agent/go.mod astradns-agent/go.mod
COPY astradns-agent/go.sum astradns-agent/go.sum
COPY astradns-types/go.mod astradns-types/go.mod
COPY astradns-types/go.sum astradns-types/go.sum

WORKDIR /workspace/astradns-agent
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY astradns-agent /workspace/astradns-agent
COPY astradns-types /workspace/astradns-types

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o /out/astradns-agent cmd/agent/main.go

# Runtime image with unbound resolver binaries.
# Distroless was evaluated, but this image currently needs distro-packaged
# resolver binaries (unbound/unbound-control) and their runtime libraries.
FROM debian:bookworm-slim

ARG OCI_TITLE="astradns-agent"
ARG OCI_DESCRIPTION="AstraDNS node-local DNS agent"
ARG OCI_SOURCE="https://github.com/astradns/astradns"
ARG OCI_VERSION="dev"
ARG OCI_REVISION=""
ARG OCI_CREATED=""

LABEL org.opencontainers.image.title="${OCI_TITLE}" \
      org.opencontainers.image.description="${OCI_DESCRIPTION}" \
      org.opencontainers.image.source="${OCI_SOURCE}" \
      org.opencontainers.image.version="${OCI_VERSION}" \
      org.opencontainers.image.revision="${OCI_REVISION}" \
      org.opencontainers.image.created="${OCI_CREATED}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    unbound \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/astradns-agent /usr/local/bin/astradns-agent

RUN useradd --system --uid 65532 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin astradns && \
    mkdir -p /var/run/astradns/engine && \
    chown -R 65532:65532 /var/run/astradns

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/astradns-agent"]
