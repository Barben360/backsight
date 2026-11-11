# 0017 — Target Hindsight 1.1.x, pinned by digest, bumped on our cadence

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
Releases pin an image digest validated in CI (migration + recall parity on a fixture DB). Never upstream `latest`. Adopt upgrades 0.10.1 installs after a verified backup.

## Why
E2: 0.10.1 → 1.1.0 migrates in < 1 s on the author's DB with identical recall on identical data; 1.1 adds tag-glob scopes, default scopes and invalidation used by this design. Upstream ships ~1.3 releases a week with no written stability promise (C03).
