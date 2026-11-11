# 0005 — Adopted recommendations from the initial review

- **Date**: 2026-10-23
- **Status**: accepted in principle (details per topic in later decisions)
- **Decided by**: author ("for everything else, let's make it this way")

See `research/00-initial-request-and-review.md`. Adopted:
- Updates = numbered idempotent migrations run by the binary, applied-version state, managed blocks in user files; changelog for humans and agents.
- Signed releases only; update notification by default; auto-apply opt-in; automatic rollback when health checks fail after an update.
- Backup = one tar.gz (setup + memory), optional encryption, version metadata (Hindsight, schema, pgvector, embedding model), `verify` restore into a throwaway instance.
- The binary is the single source of truth for install/update; the agent guide drives it, never re-implements it.
- Redaction of secrets before retain; clear statement of what leaves the machine.
- Uninstall command; cost visibility per provider; memory hygiene (forget, opt-out); CI matrix; tracking of Claude Code hook/settings changes.
- Language presets (at least EN, FR) instead of today's hard-coded French.
