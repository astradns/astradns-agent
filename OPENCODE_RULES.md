# OpenCode Rules -- astradns-agent

These rules define how AI agents may contribute to this repository.

1. Run `make test` and `make vet` before opening a PR.
2. Keep proxy-path changes non-blocking (`pkg/proxy`).
3. Do not add dependencies without explicit maintainer approval.
4. Do not modify shared contracts from `astradns-types` in this repository.
5. Use conventional commits and include rationale in PR descriptions.
6. Never commit secrets, credentials, tokens, or personal data.
7. Follow `AGENTS.md` for repository-specific constraints.
