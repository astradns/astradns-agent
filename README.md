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
| `ASTRADNS_LOG_MODE` | `sampled` | Query log mode (`full`, `sampled`, `errors-only`, `off`) |
| `ASTRADNS_LOG_SAMPLE_RATE` | `0.1` | Fraction of queries to log when mode is `sampled` |

## Container Image Notes

The default container image includes the `unbound` engine binary. `coredns` and `powerdns` engine types remain available in code but require a custom image that also ships those executables.

## Deployment

The Kubernetes manifest is at `config/daemonset.yaml`. It includes the DaemonSet, ServiceAccount, and metrics Service.

The manifest targets namespace `astradns-operator-system` to match the operator's default deployment namespace and shared `astradns-agent-config` ConfigMap.

```sh
kubectl apply -f config/daemonset.yaml
```

## Development

```sh
# Build the agent binary
make build

# Run unit tests
make test

# Run static analysis
make vet
```

## Contribution Policy

- Human and AI contributions: `CONTRIBUTING.md`
- OpenCode-specific guardrails: `OPENCODE_RULES.md`
- Repository-level AI constraints: `AGENTS.md`

### Docker

```sh
docker build -f Dockerfile -t astradns-agent ..
```
