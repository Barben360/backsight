# 0048 — Outside preview after lifecycle; success measured after the cut-over

- **Date**: 2026-10-24
- **Status**: accepted (agent, from the independent spec critique of 2026-10-24; open to veto)
- **Decided by**: agent

## Decision
- The preview for 3-5 outside users moves from M4 to the end of M5, once uninstall, update with rollback and restore exist. The author's own cut-over stays at M4, since the author can recover by hand.
- §1 success criteria:
  - Parity telemetry is measured over two weeks after the M4 cut-over. The M2 replay is the earlier gate.
  - Onboarding is measured on the preview: median time from bootstrap to the first recalled memory is ≤ 15 min, excluding the image download. At least 4 of 5 installs finish without help.
  - Harmlessness gets a latency target: the prompt hook's wall time is p95 ≤ 2.5 s on a local server. The deadlines stay as safety nets only.
- Until M4, the dev install's plugin lives only in a separate Claude Code profile (`CLAUDE_CONFIG_DIR`), so it never injects alongside the reference hooks. The replay harness calls the binary directly. Backfill reads the main profile's transcripts (`--projects-dir`).
- M1 is delivered in three ordered blocks, each closed by its own review:
  1. server, bank and provider check;
  2. write path and CLI `store` (the MCP `memory_store` comes in M2);
  3. backfill and E20.

## Why
The critique flagged four problems:
- outside users would get no supported way to back out;
- the parity criterion was measured before the author used backsight daily;
- "beyond its budget" allowed 20 s stalls;
- M1 was too large and left coexistence unspecified.
