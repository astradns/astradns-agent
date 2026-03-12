# PowerDNS Recursor — multi-arch from official packages
# Published to ghcr.io/astradns/powerdns-recursor:<version>

ARG PDNS_REPO_VERSION=54
ARG PDNS_VERSION=5.4.0

# ---------------------------------------------------------------------------
# Stage 1: install from official PowerDNS repo
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS builder

ARG PDNS_REPO_VERSION

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    gnupg \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://repo.powerdns.com/FD380FBB-pub.asc | gpg --dearmor -o /usr/share/keyrings/powerdns.gpg

RUN echo "deb [signed-by=/usr/share/keyrings/powerdns.gpg] http://repo.powerdns.com/debian bookworm-rec-${PDNS_REPO_VERSION} main" \
    > /etc/apt/sources.list.d/powerdns.list

RUN echo "Package: pdns-*\nPin: origin repo.powerdns.com\nPin-Priority: 600" \
    > /etc/apt/preferences.d/powerdns

RUN apt-get update && apt-get install -y --no-install-recommends \
    pdns-recursor \
    && rm -rf /var/lib/apt/lists/*

# Collect runtime library dependencies
RUN mkdir -p /deps/lib && \
    for bin in /usr/sbin/pdns_recursor /usr/bin/rec_control; do \
      ldd "$bin" 2>/dev/null | awk '/=>/ {print $3}' | while read lib; do \
        cp -L "$lib" /deps/lib/ 2>/dev/null || true; \
      done; \
    done

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

ARG PDNS_VERSION

LABEL org.opencontainers.image.title="powerdns-recursor" \
      org.opencontainers.image.description="PowerDNS Recursor ${PDNS_VERSION} — AstraDNS engine" \
      org.opencontainers.image.source="https://github.com/astradns/astradns-agent" \
      org.opencontainers.image.vendor="AstraDNS" \
      io.astradns.engine="powerdns" \
      io.astradns.engine.version="${PDNS_VERSION}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/sbin/pdns_recursor /usr/sbin/pdns_recursor
COPY --from=builder /usr/bin/rec_control /usr/bin/rec_control
COPY --from=builder /deps/lib/ /usr/lib/

RUN ldconfig

RUN groupadd --system --gid 65532 astradns \
    && useradd --system --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin astradns \
    && mkdir -p /etc/astradns /var/run/astradns/engine \
    && chown -R 65532:65532 /etc/astradns /var/run/astradns

USER 65532:65532
ENTRYPOINT ["/usr/sbin/pdns_recursor"]
CMD ["--daemon=no", "--write-pid=no", "--disable-syslog=yes"]
