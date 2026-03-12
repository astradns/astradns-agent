# ISC BIND — multi-arch build from source
# Published to ghcr.io/astradns/bind:<version>

ARG BIND_VERSION=9.20.20

# ---------------------------------------------------------------------------
# Stage 1: build from source
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS builder

ARG BIND_VERSION

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    ca-certificates \
    curl \
    pkg-config \
    libssl-dev \
    libuv1-dev \
    libnghttp2-dev \
    liburcu-dev \
    libjemalloc-dev \
    libcap-dev \
    libxml2-dev \
    libjson-c-dev \
    python3 \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL "https://downloads.isc.org/isc/bind9/${BIND_VERSION}/bind-${BIND_VERSION}.tar.xz" \
      -o /tmp/bind.tar.xz

RUN tar xJf /tmp/bind.tar.xz -C /tmp

WORKDIR /tmp/bind-${BIND_VERSION}

RUN ./configure \
      --prefix=/opt/bind \
      --sysconfdir=/etc/bind \
      --localstatedir=/var \
      --with-openssl \
      --with-libuv \
      --with-libnghttp2 \
      --with-jemalloc \
      --enable-full-report \
      --disable-linux-caps \
    && make -j"$(nproc)" \
    && make install \
    && strip /opt/bind/sbin/named \
    && strip /opt/bind/bin/rndc \
    && strip /opt/bind/bin/dig \
    && strip /opt/bind/bin/delv

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

ARG BIND_VERSION

LABEL org.opencontainers.image.title="bind" \
      org.opencontainers.image.description="ISC BIND ${BIND_VERSION} — AstraDNS engine" \
      org.opencontainers.image.source="https://github.com/astradns/astradns-agent" \
      org.opencontainers.image.vendor="AstraDNS" \
      io.astradns.engine="bind" \
      io.astradns.engine.version="${BIND_VERSION}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libssl3 \
    libuv1 \
    libnghttp2-14 \
    liburcu8 \
    libjemalloc2 \
    libxml2 \
    libjson-c5 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /opt/bind /opt/bind

RUN ln -s /opt/bind/sbin/named /usr/sbin/named \
    && ln -s /opt/bind/bin/rndc /usr/sbin/rndc \
    && ln -s /opt/bind/bin/dig /usr/bin/dig \
    && ln -s /opt/bind/bin/delv /usr/bin/delv

RUN groupadd --system --gid 65532 astradns \
    && useradd --system --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin astradns \
    && mkdir -p /etc/astradns /var/run/astradns/engine /var/cache/bind \
    && chown -R 65532:65532 /etc/astradns /var/run/astradns /var/cache/bind

USER 65532:65532
ENTRYPOINT ["/usr/sbin/named"]
CMD ["-f", "-g", "-u", "astradns"]
