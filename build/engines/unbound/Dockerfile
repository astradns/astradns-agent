# Unbound — multi-arch build from source
# Published to ghcr.io/astradns/unbound:<version>

ARG UNBOUND_VERSION=1.24.2

# ---------------------------------------------------------------------------
# Stage 1: build from source
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS builder

ARG UNBOUND_VERSION

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    ca-certificates \
    curl \
    libexpat1-dev \
    libevent-dev \
    libssl-dev \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL "https://nlnetlabs.nl/downloads/unbound/unbound-${UNBOUND_VERSION}.tar.gz" \
      -o /tmp/unbound.tar.gz

RUN tar xzf /tmp/unbound.tar.gz -C /tmp

WORKDIR /tmp/unbound-${UNBOUND_VERSION}

RUN ./configure \
      --prefix=/opt/unbound \
      --with-libevent \
      --with-pthreads \
      --with-ssl \
      --disable-flto \
      --disable-rpath \
    && make -j"$(nproc)" \
    && make install \
    && strip /opt/unbound/sbin/unbound \
    && strip /opt/unbound/sbin/unbound-control \
    && strip /opt/unbound/sbin/unbound-checkconf

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

ARG UNBOUND_VERSION

LABEL org.opencontainers.image.title="unbound" \
      org.opencontainers.image.description="Unbound DNS resolver — AstraDNS engine" \
      org.opencontainers.image.source="https://github.com/astradns/astradns-agent" \
      org.opencontainers.image.vendor="AstraDNS" \
      io.astradns.engine="unbound" \
      io.astradns.engine.version="${UNBOUND_VERSION}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libexpat1 \
    libevent-2.1-7 \
    libssl3 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /opt/unbound /opt/unbound

RUN ln -s /opt/unbound/sbin/unbound /usr/sbin/unbound \
    && ln -s /opt/unbound/sbin/unbound-control /usr/sbin/unbound-control \
    && ln -s /opt/unbound/sbin/unbound-checkconf /usr/sbin/unbound-checkconf

RUN groupadd --system --gid 65532 astradns \
    && useradd --system --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin astradns \
    && mkdir -p /etc/astradns /var/run/astradns/engine \
    && chown -R 65532:65532 /etc/astradns /var/run/astradns

USER 65532:65532
ENTRYPOINT ["/usr/sbin/unbound"]
CMD ["-d", "-v"]
