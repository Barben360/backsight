# 0063 — M4 in five blocks: onboarding, then a gated cut-over

- **Date**: 2026-10-25
- **Status**: accepted
- **Decided by**: author (2026-10-25: "Q1, Q2, Q3: I follow your recommendations"), on the agent's proposal

## Proposal

### Blocks (each closed by its own reviews, then the M4 review)
1. **Jobs and safety rails**: what M3 handed over, plus what setup needs to exist first.
   - `jobs maintain` (daily, 0025): telemetry rotation, doctor snapshot, backups (block 2), update check (a stub until releases are public, see Q2).
   - OS scheduler entries for `jobs tick` (15 min) and `jobs maintain` (systemd user timers, launchd agents, Task Scheduler), written and removed through the config editor, with `--plan`.
   - doctor: job last run > 2× cadence.
   - The maintenance lock honoured by `forget`, `memory scan`, `ask`, `projects`. SessionStart checks the circuit breaker before its server calls.
2. **Backups** (0023):
   - `backup create|verify|list` and `backup identity show|import`, `age`-encrypted (`filippo.io/age`, CGO-free);
   - `jobs maintain` keeps 7 daily and 4 weekly;
   - `forget --purge` lists the backups that still hold the memory.
   - `restore` and the restore epoch (0044) stay in M5 per ROADMAP.
3. **`setup`** (§11.1–11.3, 0022):
   - the journaled, idempotent wizard with `--resume`/`--undo`, `--questions --json`, `--answer`, `--non-interactive` and progress lines;
   - pre-flight, choices (provider matrix and presets, retention question 0041, backups), consent;
   - `verify` (every hook through the supervisor, MCP handshake, a retain/recall round trip), offered backfill;
   - replacing reference pieces (§11.5) with pre-images and `setup --undo`.
4. **Fresh-install proof**:
   - **CI**: a job that runs `setup --non-interactive` from nothing on the runner's Docker with Hindsight's mock LLM, through to a recall. It extends `scripts/sandbox.sh`.
   - **This machine**: an isolated `HOME` and a scratch Claude Code profile, by hand and through an agent (`claude -p` reading the agent guide), each reaching a working memory with no hand edit.
   - Bootstrap scripts (`install.sh`, `install.ps1`) as Q2 allows.
5. **Cut-over on the author's machine**: every step needs the author's explicit go, asked at the time.
   - **Readiness**:
     - E24 if 50 new prompts exist, with 0054's rule, 0057's previous reranker setting replayed beside the current one, and French measured apart;
     - SPEC §9.4's retro comparison;
     - a verified backup of the reference server.
   - **The move** (Q1): `setup` in the main `~/.claude` profile, which retires the reference client pieces with pre-images kept.
   - **After**:
     - the reference server stays running, untouched, until M5's backups and restore are proven;
     - parity telemetry starts (§1.2: two weeks against the C12 baseline);
     - the author's `claude-proton-backup` job runs with `BACKSIGHT_REQUIRED=1`, so a missing backsight archive fails it;
     - detector follow-up and briefing use are measured in real use.

### Exit (ROADMAP M4)
A fresh install, by hand and through an agent, reaches a working memory with no hand edit. The author runs on backsight daily, and parity telemetry has started.

## Answers (author, 2026-10-25)
- **Q1**: (a), adopt the dev install at the cut-over.
- **Q2**: the repository stays private until M7 (ROADMAP). M4 installs use a local build or `gh release download`. The bootstrap scripts are written and tested against a local release directory, and published at M7.
- **Q3**: yes. Block 1 installs the `jobs tick` and `jobs maintain` user timers for the dev install.

## Questions as asked
- **Q1, the cut-over target**:
  - **(a) Adopt the dev install** (recommended): `setup` finds the running backsight server with its bank, already backfilled from every transcript and used by E23–E27, and wires the main profile to it. There is no second backfill (it saves about $1–3 and hours of extraction), and the bank's fact quality is the one already measured.
  - **(b) A fresh server and bank** with a full backfill: it proves onboarding on the real machine end to end, and block 4 already proves it elsewhere.
- **Q2, publishing**: the backsight repository is private.
  - `curl … | sh` bootstrap scripts, public signed releases, update checks and an agent guide at a stable URL all need it public, or a release mirror.
  - Until then, installs go through `gh release download` (authenticated) or a local build.
  - Making the repository public is outward-facing and the author's call: now, at the end of M4, or later (M7 per ROADMAP).
- **Q3, scheduler on this machine**: block 1 installs `jobs tick` and `jobs maintain` timers for the dev install, before the cut-over, so the M4 measures run on real cadence. It adds systemd user units next to the existing `hindsight-local-backup` timers. Yes (recommended) or only at cut-over?
