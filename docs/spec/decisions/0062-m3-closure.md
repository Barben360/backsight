# 0062 — M3 closes on the detector measure and the reviews; E24 moves to M4

- **Date**: 2026-10-25
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, applying 0056's own closing rule

## Decision
- **Built**: every item of 0056 is built, or amended in INDEX.
  1. **Block 1** (backsight PRs 133–142): `pause`/`resume` with stretches; the MCP server finds its session through the by-pid record; `forget [--purge]`, `memory scan`, `ask`, `projects`.
  2. **Block 2** (PRs 137, 144, 146): the four skills, `stats`, and the doctor usage checks.
  3. **Block 3** (PRs 148, 150, 156):
     - prompt-hook latency to p95 2.2 s (0057, a trade-off accepted on purpose);
     - French measured apart, with no floor change (0058, inconclusive);
     - the mental-model briefing (0060, superseding 0059).
  4. **Block 4** (PRs 152, 154): the save detector, measured on transcript replay (E27). When it fires, a save follows 50 % of the time, against 11 % otherwise.
- **Reviews**: each block had its independent reviews, and the end-of-milestone review (conformance, privacy, integration) was addressed in PR 158 and decision 0061.
- **Live smoke** (Claude Code 2.1.310), all green: plugin MCP, SessionStart hook, memory tool, exact citation, the four skills, and the MCP server finding its session in the same run.
- **E24**: the replay gate rerun has 9 new prompts against the 50 it needs. As 0056 provides, it moves to the M4 cut-over, together with the items ROADMAP M4 lists as handed over by M3.

## Why
0056 set M3's gate as the detector measure plus the reviews, with E24 when enough new prompts exist. The detector is measured, the reviews are addressed, and the new prompts do not exist yet. Holding M3 open for them would wait on the author's typing, not on the product.
