# 0043 — Subscription lane: user-chosen, never used for unattended bulk work

- **Date**: 2026-10-24
- **Status**: accepted (agent, from the independent spec critique of 2026-10-24; open to veto)
- **Decided by**: agent

## Decision
- Amends 0010's anti-goal "background use of a Claude subscription against Anthropic's terms": backsight never picks the subscription lane itself; it exists only because the user chose it with an explicit acknowledgement (0032). Live session retain on that lane is the user's choice.
- **Backfill is refused on the subscription lane** by default: replaying months of transcripts is unattended bulk use. `retain backfill --allow-subscription` with a second acknowledgement overrides it; `--plan` shows the volume first.
- The acknowledgement text names this limit.

## Why
The critique found 0010 and 0032 in direct conflict, worst for backfill (large volume, nobody watching).
