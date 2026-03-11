# AI Agent Guidelines -- astradns-agent

This document governs AI-assisted contributions to the `astradns-agent` repository. AI agents (Claude, Copilot, and others) must follow these guidelines alongside standard project conventions.

## Principles

AI contributions are held to the same quality standards as human contributions. There are no exceptions. Every change must be reviewable, testable, and justified.

## Rules

1. **All AI-generated code must pass existing tests and linters.** Run `make test` and `make vet` before proposing any change.
2. **Do not introduce new dependencies without explicit approval.** Propose dependency additions in the PR description with a justification for why the standard library or existing dependencies are insufficient.
3. **Do not modify API contracts without discussion.** The `engine.Engine` interface and CRD types are defined in `astradns-types`. If changes are needed there, open a separate discussion or PR against that repository first.
4. **Do not commit secrets, credentials, or PII.** No tokens, passwords, API keys, or personal data in code, comments, or test fixtures.
5. **Follow conventional commit format.** Use prefixes such as `feat:`, `fix:`, `refactor:`, `docs:`, `test:`, `chore:`.
6. **Respect import boundaries.** The agent imports from `astradns-types` only. It must never import from `astradns-operator`. The operator and agent are independent consumers of the shared types module.

## Repo-Specific Context

This is the **data-plane component** of AstraDNS, deployed as a Kubernetes DaemonSet on each node. It contains:

- **DNS proxy** (`pkg/proxy/`) -- the hot path that intercepts and forwards DNS queries. Performance is critical here.
- **Engine implementations** (`pkg/engine/`) -- CoreDNS, Unbound, and PowerDNS adapters implementing the `engine.Engine` interface from `astradns-types`.
- **Metrics collector** (`pkg/metrics/`) -- Prometheus metrics exposition for DNS query observability.
- **Health checker** (`pkg/health/`) -- liveness and readiness probes for the agent and managed engines.
- **Config watcher** (`pkg/watcher/`) -- watches for configuration changes and triggers engine reloads.
- **Logging** (`pkg/logging/`) -- structured logging setup.
- **Entry point** (`cmd/agent/main.go`) -- agent bootstrap and lifecycle management.

### Performance Constraints

**Do not add blocking operations in the proxy hot path** (`pkg/proxy/`). DNS query handling is latency-sensitive. Specifically:

- No synchronous disk I/O in the request path.
- No unbounded allocations per query.
- No blocking channel operations without timeouts.
- Logging in the hot path should be at debug level only, guarded by level checks.
- Prefer pre-allocated buffers and object pools for high-frequency operations.

### Engine Implementations

Each engine adapter in `pkg/engine/` must:

- Implement the full `engine.Engine` interface from `astradns-types`.
- Be registered in the engine registry.
- Handle graceful reload without dropping in-flight queries.
- Include health check logic that verifies the engine is actually serving DNS, not just that the process is running.

## Code Style

- **Language:** Go
- **Follow existing patterns** in the codebase. Do not introduce new structural conventions without discussion.
- **Structured logging:** Use `log/slog` from the standard library. Do not add external logging frameworks.
- **Error handling:** Wrap errors with context using `fmt.Errorf("operation: %w", err)`. Do not discard errors silently.
- **Context propagation:** All long-running operations must accept and respect `context.Context` for cancellation.

## Testing Expectations

- **Unit tests** are required for all packages. Each package should have `*_test.go` files exercising both success and failure paths.
- Engine implementations must be tested against the `engine.Engine` interface contract: Configure, Start, Reload, Stop, and HealthCheck.
- The proxy package requires tests that verify correct forwarding behavior under both normal and error conditions.
- The metrics package must test that all expected metrics are registered and updated correctly.
- Run `make test` to execute the full test suite. Run `make vet` for static analysis.

## Build and Docker

- `make build` produces the agent binary at `bin/astradns-agent`.
- The `Dockerfile` builds the container image used in the DaemonSet. Changes to the Dockerfile must maintain multi-stage build structure and produce a minimal final image.
