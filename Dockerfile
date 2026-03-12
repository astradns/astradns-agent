# Build the agent binary
FROM golang:1.26.1 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . /workspace

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o /out/astradns-agent cmd/agent/main.go

# Runtime image: engine image as base + agent binary on top.
# The ENGINE_IMAGE already contains the DNS engine binaries and runtime libs.
# Override via --build-arg to select engine variant:
#   ENGINE_IMAGE=ghcr.io/astradns/unbound:1.24.2      (default)
#   ENGINE_IMAGE=ghcr.io/astradns/powerdns-recursor:5.1
#   ENGINE_IMAGE=ghcr.io/astradns/bind:9.20.20
ARG ENGINE_IMAGE=ghcr.io/astradns/unbound:1.24.2
FROM ${ENGINE_IMAGE}

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

COPY --from=builder /out/astradns-agent /usr/local/bin/astradns-agent

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/astradns-agent"]
