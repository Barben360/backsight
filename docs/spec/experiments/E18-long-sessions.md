# E18 — Long real sessions: session retain (durable rules) vs the Python retro, blind-graded

Date: 2026-10-24. Four long sessions of this machine (clinic-app 12.6 MB / 12 prompts, letters 3.5 MB / 9, my-garage 1.8 MB / 10, `~/.claude` 1.0 MB / 11). **V** = measured.

- **(a) Python retro**: `memory-retro.py backtest` (Haiku triage, Sonnet synthesis, dedup against the live memory) — 7, 6, 4, 5 facts; $0.08-0.09 per session. Note: its dedup drops facts the live bank already holds (these sessions were retained live), which lowers its coverage here.
- **(b) Session retain as the product would do it**: normalised JSONL (user/assistant text + action lines), 12 000-char chunks, `dry-run-extract` with the `conversation` strategy + durability and language rules (E17), DeepSeek — 120, 13, 31, 26 facts; 159 k, 19 k, 41 k, 37 k tokens.
- A Sonnet judge, blind to the method (A/B shuffled per session), listed each session's key durable items from the condensed session, then scored coverage and precision.

| Session | Retro coverage | Retro precision | Session-retain coverage | Session-retain precision |
|---|---|---|---|---|
| clinic-app (long, messy debugging) | 40 % | 85 % | **95 %** | 55 % |
| letters | 90 % | 95 % | 90 % | 65 % |
| my-garage (energy repair) | 35 % | 100 % | **95 %** | 75 % |
| `~/.claude` (Hindsight audit) | 40 % | 100 % | **95 %** | 70 % |

Judge's verdict: session retain makes the better long-term memory (coverage); its noise is mostly near-duplicates that consolidation merges. Its real weakness: **superseded hypotheses stored as facts** (in clinic-app, wrong diagnoses later overturned in the session; elsewhere a "no backup" state fixed later in the same session). The retro's weakness: capped at ~5 facts, it drops root causes and key decisions.

Consequences: confirms 0028 (no retro job) on long sessions. The stale-hypothesis risk needs a mitigation — tested in E18b (does consolidation clean them? does sending the session in larger, later chunks help?).

## E18b — consolidated observations: incremental vs settled retain (clinic-app session, real retains, sandbox 1.1)
- `e18inc`: the session sent as it happened, 16 appends of 12 000-char chunks (what a per-Stop flush does) → 122 raw facts, **75 observations**.
- `e18end`: the whole session sent once at the end, strategy chunk 30 000 chars → 56 raw facts, **43 observations**.

Blind-ish Sonnet judge against the 9 key items of E18:

| Variant | Observations | Coverage (/9) | Overturned hypotheses kept as facts | Noise |
|---|---|---|---|---|
| incremental | 75 | 7.0 | 4 sure, up to 6 (emulator-dead diagnosis, bisect, #564 as a real defect…) | ~35 % |
| settled, 30 k chunks | 43 | 7.5 | 1 likely | ~15 % |

Reading: chunks extracted before the resolution exists store hypotheses that later chunks cannot retract (consolidation sees partial views); extracting once the session (or a long segment of it) has settled lets the extractor see conclusions, with half the facts and less noise. **V**

Consequence: decision 0031 — settled retain.
