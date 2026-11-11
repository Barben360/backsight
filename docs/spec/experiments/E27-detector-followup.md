# E27 — Save-detector follow-up on transcript replay (M3 gate, 0056 block 4)

Date: 2026-10-25. This machine's transcripts (44 files; Claude Code kept 30 days until retention was raised to 3 650 days on 2026-10-25). **V** = observed.

## Method
- Every human prompt of the main thread is taken in order: no sidechains or harness messages (E23's filter).
- **Detector verdict**: `backsight detect-replay` (backsight PR 152), the prompt hook's own `detect`. It is given the previous reply's last 1 000 characters, and whether an Agent or Task tool ran in the previous turn (the subagent flag).
- **Save**, in the same turn or the next: a backsight `memory_store`, the reference stack's `hindsight_ingest_document` or `hindsight_capture_initiative`, a remember or retro skill, or `backsight store`.
- Script: `E27-scripts/replay.py`. No LLM cost.

## Results (**V**)
| Detector verdict | Prompts | Followed by a save (same or next turn) |
|---|---|---|
| rule | 10 | 5 (50 %) |
| approval | 4 | 2 (50 %) |
| not fired | 171 | 19 (11 %) |

## Conclusions
1. When the detector fires, a save follows 4.5 times as often as on other prompts. Its moments are real save moments, and it fires on 8 % of prompts.
2. It catches 7 of the 26 turns followed by a save. Most saves happen without the user stating a rule: the detector is a nudge, not the main path.
3. The saves were made with the reference stack's own reminders active, so this measures where the user's intent lies, not backsight's nudge. In-use telemetry (`stats`: detector fired / followed by a save) takes over from the M4 cut-over.

## E24
The replay-gate rerun needs 50 new prompts. On 2026-10-25 there are 9 prompts of at least 60 characters typed after 2026-10-24 outside E23, none in French. Per 0056, E24 moves to the M4 cut-over, where it also replays 0057's previous reranker setting (INDEX row on 0057).
