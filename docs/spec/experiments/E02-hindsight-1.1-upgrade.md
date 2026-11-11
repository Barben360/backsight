# E2 — Hindsight 0.10.1 → 1.1.0 on a copy of the live database

Date: 2026-10-24. Sandboxes in Docker (separate compose projects, ports 18888/28888), restored from the 03:30 daily dump (323 memory units). The live stack was only read. **V** = observed.

| Check | Result |
|---|---|
| Migrations at container start (`a7c2e9f41b60` → `c7e2f9a4d1b8`, 9 revisions incl. bank aliases, entity trigram index, reasoning tokens) | **V** < 1 s on this DB, no error, 323 units kept |
| `/version` | **V** `api_version 1.1.0`, same feature flags |
| Recall parity on identical data (20 real prompts, observations, budget low): 0.10.1 sandbox vs 1.1.0 sandbox | **V** injected top-3 identical 20/20, top-10 overlap 100 %, mean score delta 0.000 |
| Recall vs the *live* 0.10.1 | top-3 identical 10/20 — explained by data drift since 03:30 (new observations, retag), not by the version |
| New bank config keys | **V** `default_observation_scopes` (default `combined`), `consolidation_strategies`, `recall_prefer_observations`, `*_use_decision_model` (experimental) |
| Retain without `observation_scopes` (tags `project:e2test, source:chat, harness:claude-code`) → consolidation | **V** observation created with the same three tags in < 1 min |
| Consolidator tag rule (1.1.0 `consolidator.py` l.2488-2493) | **V** observation tags = the override scope, else the source memories' own tags (batches share one tag set) → untagged observations come only from untagged sources or an explicit empty scope (`shared`) |

Consequences:
- v1 targets Hindsight **1.1.x**; the adopt path upgrades 0.10.1 installs (backup first; migrations are fast at this size).
- Root cause of today's 99 untagged observations on this machine is the old `shared` setting; the residual 1-3 %/day on the source machine stays unexplained → keep the retag job as a guarded safety net plus a `doctor` count, and always send explicit tags and scopes on every retain.
- Bank aliases (0.10.2+) allow renaming `coding-agent::memoire` without moving data.
