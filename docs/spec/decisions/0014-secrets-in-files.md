# 0014 — Secrets live in a 0600 file in v1, no OS keychain

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
`<config>/secrets.toml` (0600) is the only secret store; the binary renders server secrets into the compose `.env` (0600). Secrets never travel in answers files or arguments; users set them with `secret set` (stdin, env var or file).

## Why
Keychains fail or prompt in hooks, headless jobs and servers (C11), and the compose stack needs the secrets in a file anyway.
