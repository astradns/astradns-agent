# Contributing to astradns-agent

Thanks for contributing to the AstraDNS data plane.

## Pull Request Checklist

- Keep changes scoped and explain the operational impact.
- Run `make test` and `make vet` locally.
- Use conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`).
- Do not introduce new dependencies without prior approval.
- Do not change shared contracts from `astradns-types` in this repository.

## AI/OpenCode Contributions

AI-assisted changes are welcome, but must follow repository guardrails in `AGENTS.md`.

Minimum requirements for AI-generated changes:

- No secrets, credentials, or personal data.
- No blocking operations in proxy hot paths.
- Preserve import boundaries (`astradns-agent` must not import `astradns-operator`).
- Include tests for behavioral changes.
