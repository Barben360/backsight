# 0055 — M2 closes on E23's point estimates; the replay gate reruns in M3

- **Date**: 2026-10-25
- **Status**: accepted
- **Decided by**: author (2026-10-25, choosing "close M2, rerun at M3" over keeping M2 open or relaxing the margin), on the agent's recommendation

## Decision
- M2's ROADMAP criterion ("replay metrics ≥ the reference stack on the same prompts; chaos tests green") is met: E23's point estimates favour backsight on nDCG@3 (+0.082) and useful injection (+0.010), with injection precision 0.79 vs 0.58; chaos tests are green on every event.
- 0054's stricter rule (95 % CI lower bound > −0.05) was not met because only 100 prompts exist on this machine (intervals ±0.15). The rule stays as written; it is not relaxed after the fact.
- The replay gate runs again in M3, on prompts typed after 2026-10-24 (none of E23's), with the same −0.05 margin.
- Two M3 items come from E23:
  - **French prompts against English facts**: 5 of the 9 French prompts where only the reference was useful had nothing above backsight's floors. A measured fix is needed, for example a cross-language floor, judged on the M3 replay and not on E23's prompts.
  - **Prompt-hook latency**: p95 4.5 s on this machine's CPU rerankers, against 0048's target of 2.5 s.

## Why
The shortfall comes from the sample size, not from a measured regression. Waiting for enough new prompts would hold M3 back by weeks. Rerunning the gate on fresh prompts at M3 keeps the rule honest, because no tuning on E23's prompts can leak into it.
