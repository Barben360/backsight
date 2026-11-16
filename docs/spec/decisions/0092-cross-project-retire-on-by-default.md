# 0092 — `feedback.cross_project_retire` on by default again

- **Date**: 2026-11-17
- **Status**: accepted
- **Decided by**: author (2026-11-17: "I'd like any project to be able to remove memories from another project without being blocked, by default. An option could be added to deactivate it.")

## Decision
- **The default is `true` again**, as from v0.1.27-m7 to v1.5.0: `memory_credit` `wrong`/`outdated` retires a memory of any project shown in the session, its correction stored in that memory's project. This reverses 0089 P1 only; P2 to P4 stand.
- **The option is the existing key**: `cross_project_retire = false` under `[feedback]` in `config.toml` keeps retirement to the session's project, the projects it links and the transverse pool, with 0089's refusal unchanged (it says retiring is off on this machine, never names the key). A repository's `.backsight.toml` still cannot set it, either way.
- **What does not change**: the other guards of 0089 and earlier hold whatever the key says: only memories shown in the session, a correction and evidence, `feedback.max_retirements` per session counting every project, one project per call, the memory's project read from the server (a memory it cannot read retires only under a near project's exact name), the target project's exclusions and privacy, and `backsight forget --restore <id>` undoes each retirement; `stats` and `memory stale` list cross-project retirements with both projects.
- **Migration**: none. Setup never writes `[feedback]`, so an install without the key gets `true` at its next MCP start; a `false` the user wrote in `config.toml` is kept. An older binary reads the same `config.toml`.

## Why
- The author's call: a wrong memory found in one project is often another project's (shared infrastructure, a decision made elsewhere), and handing the user a `backsight forget <id>` each time blocked the correction from reaching the project that holds the wrong fact.
- The risk 0089 named stays, accepted: a session acting on hostile text can retire up to `feedback.max_retirements` memories of projects it was shown. Each is reversible and listed for review, and the user who wants the narrower reach sets one key.
