# astradns-agent

AstraDNS Agent is the data-plane component of AstraDNS. It runs as a DaemonSet on every node and provides DNS proxying, metrics collection, query logging, and health reporting.

## Architecture

```
Client :53 --> [DNS Proxy :5353] --> [Engine subprocess :5354]
                    |
                    +--> [Metrics exporter :9153]
                    +--> [Query logger (slog/JSON)]
                    +--> [Health checker :8080]
                    +--> [Config watcher]
```

The agent listens for DNS queries on port 5353 (UDP/TCP), forwards them to a backend engine subprocess on port 5354, and exposes Prometheus metrics, structured query logs, and health endpoints.

## Components

| Component | Description |
|---|---|
| **Proxy** | DNS proxy that intercepts queries and forwards to the engine subprocess |
| **Metrics** | Prometheus exporter (query counts, latency histograms, error rates) on `:9153` |
| **Query Logger** | Structured JSON logging via `slog` with configurable sampling |
| **Health Checker** | Liveness (`/healthz`) and readiness (`/readyz`) endpoints on `:8080` |
| **Config Watcher** | Watches the ConfigMap mount for EngineConfig JSON changes and triggers engine reload |

## Supported Engines

The agent supports multiple DNS engine backends, selected at startup via the `ASTRADNS_ENGINE_TYPE` environment variable:

| Engine | Value |
|---|---|
| Unbound | `unbound` |
| CoreDNS | `coredns` |
| PowerDNS | `powerdns` |

## Configuration

The agent reads an `EngineConfig` JSON file from a ConfigMap mounted at `ASTRADNS_CONFIG_PATH` (default: `/etc/astradns/config/config.json`). Engine-specific rendered files are written to `ASTRADNS_ENGINE_CONFIG_DIR` (default: `/var/run/astradns/engine`).

`ASTRADNS_CONFIG_PATH` can be either a directory or a file path:

- Directory: `<path>/config.json` is used.
- File path: that file is used directly.

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `ASTRADNS_ENGINE_TYPE` | `unbound` | DNS engine backend to use |
| `ASTRADNS_CONFIG_PATH` | `/etc/astradns/config/config.json` | ConfigMap mount path (directory or `config.json` file path) |
| `ASTRADNS_ENGINE_CONFIG_DIR` | `/var/run/astradns/engine` | Writable directory for generated engine config files |
| `ASTRADNS_LISTEN_ADDR` | `0.0.0.0:5353` | Address the DNS proxy listens on |
| `ASTRADNS_ENGINE_ADDR` | `127.0.0.1:5354` | Address of the engine subprocess |
| `ASTRADNS_METRICS_ADDR` | `:9153` | Address for the Prometheus metrics endpoint |
| `ASTRADNS_HEALTH_ADDR` | `:8080` | Address for the health check endpoint |
| `ASTRADNS_HEALTH_PROBE_DOMAIN` | `.` | DNS name queried by upstream health checks |
| `ASTRADNS_HEALTH_PROBE_TYPE` | `NS` | DNS record type used by upstream health checks (name like `A`/`NS` or numeric code) |
| `ASTRADNS_PROXY_TIMEOUT` | `2s` | Per-query proxy timeout when forwarding to engine |
| `ASTRADNS_PROXY_RATE_LIMIT_GLOBAL_RPS` | `2000` | Global DNS query rate limit in requests per second |
| `ASTRADNS_PROXY_RATE_LIMIT_GLOBAL_BURST` | `4000` | Global token bucket burst for DNS query spikes |
| `ASTRADNS_PROXY_RATE_LIMIT_PER_SOURCE_RPS` | `200` | Per-source DNS query rate limit in requests per second |
| `ASTRADNS_PROXY_RATE_LIMIT_PER_SOURCE_BURST` | `400` | Per-source token bucket burst for short spikes |
| `ASTRADNS_PROXY_RATE_LIMIT_PER_SOURCE_STATE_TTL` | `5m` | Retention time for per-source limiter state before cleanup |
| `ASTRADNS_PROXY_RATE_LIMIT_PER_SOURCE_MAX_SOURCES` | `10000` | Max source IPs tracked for per-source limiting |
| `ASTRADNS_PROXY_CACHE_MAX_ENTRIES` | `10000` | Max DNS responses cached at proxy layer before forwarding to engine |
| `ASTRADNS_PROXY_CACHE_DEFAULT_TTL` | `30s` | Fallback TTL for proxy-layer cache entries without explicit record TTL |
| `ASTRADNS_ENGINE_CONN_POOL_SIZE` | `64` | Max reusable DNS client connections per protocol to the local engine |
| `ASTRADNS_CONFIG_WATCH_DEBOUNCE` | `1s` | Debounce window for config file change events before reload |
| `ASTRADNS_ENGINE_RECOVERY_INTERVAL` | `5s` | Interval to probe engine responsiveness and auto-recover crashes |
| `ASTRADNS_COMPONENT_ERROR_BUFFER` | `5` | Buffer size for component error channel before overflow drops |
| `ASTRADNS_METRICS_BEARER_TOKEN` | `` | Optional bearer token required to access `/metrics` |
| `ASTRADNS_TRACING_ENABLED` | `false` | Enable OpenTelemetry trace export for control-plane operations |
| `ASTRADNS_TRACING_ENDPOINT` | `localhost:4318` | OTLP/HTTP collector endpoint used when tracing is enabled |
| `ASTRADNS_TRACING_INSECURE` | `true` | Use plaintext OTLP/HTTP transport to the collector endpoint |
| `ASTRADNS_TRACING_SAMPLE_RATIO` | `0.1` | Fraction of traces sampled (0-1) when tracing is enabled |
| `ASTRADNS_TRACING_SERVICE_NAME` | `astradns-agent` | Service name reported in exported OpenTelemetry traces |
| `ASTRADNS_LOG_MODE` | `sampled` | Query log mode (`full`, `sampled`, `errors-only`, `off`) |
| `ASTRADNS_LOG_SAMPLE_RATE` | `0.1` | Fraction of queries to log when mode is `sampled` |

## Container Image Notes

The default container image includes the `unbound` engine binary. `coredns` and `powerdns` engine types remain available in code but require a custom image that also ships those executables.

Distroless runtime images were evaluated, but the default image currently stays on `debian:bookworm-slim` because it depends on distro-packaged resolver binaries and runtime libraries.

The proxy layer includes a short-lived response cache to smooth local burst traffic, while resolver-grade cache behavior remains in the selected DNS engine.

Release tags publish multi-arch images to GHCR at `ghcr.io/astradns/astradns-agent:<tag>`.

## Deployment

The Kubernetes manifest is at `config/daemonset.yaml`. It includes the DaemonSet, ServiceAccount, and metrics Service.

The manifest targets namespace `astradns-operator-system` to match the operator's default deployment namespace and shared `astradns-agent-config` ConfigMap.

```sh
kubectl apply -f config/daemonset.yaml
```

## Data Path Modes

The agent supports two deployment styles:

- `hostPort` mode (default): listens on `0.0.0.0:5353` and exposes host ports.
- `linkLocal` mode: listens on a node-local IP such as `169.254.20.11:5353`.

Set this through `ASTRADNS_LISTEN_ADDR` (for example, `169.254.20.11:5353`).

The Helm chart in `astradns-operator` wires these modes via `agent.network.mode` and `agent.network.linkLocalIP`.

## Development

```sh
# Build the agent binary
make build

# Run unit tests
make test

# Run static analysis
make vet

# Run tests + vet
make check

# Run integration suite
make integration-test

# Run e2e suite (requires a kind cluster)
make e2e-test

# Clean build artifacts
make clean
```

## Release

Tagging with `vX.Y.Z` triggers the release workflow (`.github/workflows/release.yml`) which runs tests, vet, builds the binary, and publishes the artifact.

## Contribution Policy

- Human and AI contributions: `CONTRIBUTING.md`
- OpenCode-specific guardrails: `OPENCODE_RULES.md`
- Repository-level AI constraints: `AGENTS.md`

### Docker

```sh
docker build -f Dockerfile -t astradns-agent ..
```

For production with multiple engines, build/publish one image per engine binary set and select the image in Helm using `agent.engineImages.<engine>`.
