# 0066 — M4 closes on the cut-over and its review; E24 and the lifecycle checks move to M5

- **Date**: 2026-10-25
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, applying 0063's exit criterion

## Decision
- **Built** (0063's five blocks; INDEX records each block's PRs and reviews):
  1. Jobs and safety rails: `jobs tick` and `jobs maintain`, scheduler entries, the maintenance lock honoured by curation.
  2. Encrypted backups: `create`, `verify`, `list`, identity; 7 daily and 4 weekly kept; `forget --purge` lists the backups that hold the memory.
  3. `setup`, with `--undo` by inverse edits, `verify`, and the retirement of the reference pieces.
  4. Fresh-install proofs: in CI (mock LLM), and agent-driven on the author's machine. Bootstrap scripts written; they are published at M7.
  5. The cut-over on the author's machine, each step with the author's go:
     - the move;
     - CLAUDE.md's memory section rewritten for backsight;
     - the reference skills retired;
     - the dev profile archived;
     - parity telemetry started (0065).
- **Exit (0063)**:
  - A fresh install reaches a working memory with no hand edit, through an agent (passed twice) and in CI.
  - The author runs on backsight daily.
  - Parity telemetry has started.
  - "By hand" is covered by the scripted CI run (`setup --non-interactive` and `--resume`), the path an agent or a careful user takes. The author chose agent-only proof on this machine ("Let's do agent only", 2026-10-25). The interactive prompts stay a known test gap (0064 amendments).
- **M4 review**: three independent reviewers (spec conformance; integration and safety; test quality), about 30 findings, fixed in backsight PRs 186–196 or recorded below. See INDEX.
- **Moved to M5**, with the rest of the lifecycle:
  - **E24**: the replay gate on new prompts. Caveat: since the cut-over the reference bank learns nothing new, so a replay compares a frozen bank with a live one, which favours backsight. Parity telemetry (§1.2) becomes the primary gate; E24 runs only if 50 prompts typed before 2026-10-25T11:34Z exist, or is waived at M5's start.
  - doctor checks: "one project name seen with two remotes" (0045); "Claude Code config drift or a pre-image restore pending" (§15); "the reference server can be stopped" (§11.5, once restore is proven).
  - `guide` and the user documentation (OPEN D17); the binary re-verifying itself on first run (§11.1); the update check in `jobs maintain`, with `update check|apply`.
  - `restore`, update and uninstall in the CI chain (§16).
- **Moved to M7**: Windows Task Scheduler entries without the install's environment (backsight PR 188: `/TR` is capped at 261 characters; a generated `.cmd` wrapper is to be tried on Windows); a minimum version in `install.sh` (0063 block 4).
- **Pending, the author's**: the parity verdict on 2026-11-08 (0065).

## Why
0063's exit is met. What is left either belongs to M5's lifecycle (restore, update, uninstall, which the deferred checks depend on), or needs data the cut-over itself stopped producing (E24's reference side). Holding M4 open for them would not change the product.
