# E22 — M1 "done when": backsight's facts vs the reference memory, blind-graded on the E18 rubric (2026-10-24)

Question (ROADMAP M1): is the fact quality of the dev install (backsight backfill, settled retain, English facts, Hindsight 1.1.0) at least that of the reference memory (live retain by the reference plugin, Hindsight 0.10.1) on the same sessions?

## Setup
- **Sessions**: the 11 transcripts both banks hold as `conversation:<id>` (2026-10-10 → 2026-10-23, 2.9 k to 102 k condensed chars); the session in progress at grading time was left out.
- **Facts**: each bank's memory units of that document, observations excluded (they are cross-session): reference 221, dev 252.
- **Judge**: `deepseek-v4-pro`, per session, first lists 5-15 key durable items from the condensed transcript (user/assistant text, harness tags stripped, last 150 k chars), final versions only; then grades the two fact lists labelled A/B in a seeded random order: key items covered, facts correct-durable-useful, facts contradicted by the session's final state. E18 used a Sonnet judge; the rubric is the same.
- Scripts: `experiments/E22-scripts/` (data local).

## Results

| | Coverage of key items | Precision | Facts contradicted by the final state |
|---|---|---|---|
| reference | 87/127 = **69 %** | 177/221 = **80 %** | 30 |
| backsight dev | 117/127 = **92 %** | 234/252 = **93 %** | 10 |

Per session, backsight's coverage is ≥ the reference on all 11 sessions (equal on 4) and its precision ≥ on 10 of 11 (98dd4c1b: 74 % vs 75 %). The largest gaps are short sessions where the reference kept 4-10 facts (77c20cb1: 22 % vs 89 % coverage; 7842b3d5: 18 % vs 100 %).

## Findings
1. **The M1 quality criterion is met**: higher coverage and higher precision, with a third of the contradicted facts — consistent with E18b (settled retain stores fewer overturned hypotheses than per-turn retain).
2. Differences mix several causes (settled vs live retain, backsight's strategy texts, Hindsight 1.1.0 vs 0.10.1, English vs French facts — E20 found language neutral); E22 measures the product as a whole, not one factor.

## Limits
One judge model, one machine, 11 sessions, one grading pass; the judge sees both lists at once (as in E18), so it may compare rather than grade absolutely.
