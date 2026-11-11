# 0023 — Backups default on, always encrypted when they hold secrets

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
Daily `backup create` (7 daily + 4 weekly) in one `.tar.gz` with a manifest; encrypted with `age` to an identity generated at setup (shown once) or the user's own recipient; `backup verify` restores into a throwaway stack.

## Why
A scheduled backup cannot prompt for a passphrase; an unencrypted archive would leak the LLM key and the server key (robustness review).
