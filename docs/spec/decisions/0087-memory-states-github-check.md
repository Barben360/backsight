# 0087 — `memory states`: check "still open" memories against GitHub, retire on the user's command

- **Date**: 2026-11-12
- **Status**: accepted
- **Decided by**: author (2026-11-12: "Let's do action 1. Create an independent issue, a PR, and merge it."), on the agent's proposal in https://github.com/Barben360/backsight/issues/325 (suggestions 1 and 6), scoped in https://github.com/Barben360/backsight/issues/331

## Decision
A new command, `backsight memory states [--retire] [--project <name>]`:

- **What it reads**: the live raw facts (world, experience) tagged `kind:state`, the server's tag filter and a client-side check both applied.
- **What it checks**: a fact that names a GitHub pull request or issue (a URL, `owner/repo#N`, or a bare `PR #N` / `pull request #N` / `issue #N`) and says it is not done yet ("is open", "kept open", "not (yet) merged", "unmerged", "ready to merge", "awaiting", "pending review/merge"). A fact that also says it was done since ("later squash-merged", "was merged into", "merged as <sha>", "no longer open", "closed by", "delivered by"), not negated within the three words before, is skipped: it holds its own update.
- **Where a bare ref lives**: under each of the user's own owners (`projects.own_owners`, the owners learnt from retained sessions, minus `projects.deny_owners`; github.com only), in the repository named by the project just before the ref ("my-garage PR #12"), else the one project the text names, else the fact's own project. A word before the ref that is no known project, several projects named, no owner, the item found under no owner or under several: skipped and counted.
- **How**: `gh api repos/<owner>/<repo>/issues/<n>` (the endpoint answers for pull requests too, with `pull_request.merged_at`), each item once per run. The binary calls no LLM; `gh` carries the user's login, so private repositories are read too.
- **Verdict**: a fact is outdated when every item it names is closed (merged or not). Any item still open: still open. Any doubt (not found, ambiguous, GitHub error): skipped. A failed login stops the run (exit 10, `gh auth login`); `gh` missing too, before any server call.
- **Retiring**: only with `--retire`, run by the user: each outdated fact is invalidated like `forget` (reason `outdated (backsight memory states): GitHub says <owner/repo#n> merged <time>`), journaled as a forget, and `forget --restore <id>` undoes it. The observations drawn from it are rewritten by the server without it, as after any retirement.

## Why
- On 2026-11-12, 7 of 7 sampled memories saying a pull request was "open, not merged" were wrong: GitHub showed all merged (5 in backsight, 2 in two of the author's other repositories), 4 of them from sub-agent reports. The extraction rule already forbids such facts (decision 0071 C5; `internal/server/bank.go`: "pull requests opened or updated, waiting states"), yet they still get through, and nothing re-checks them: recall and the briefing's open items keep showing them.
- Retiring them in a session costs two `memory_credit` calls each (the observation, then its source fact), and only for the memories that happen to be shown.
- GitHub's answer is a deterministic check, so the binary needs no LLM to decide.

## Not done
- **A `jobs` step retiring on its own**: an automatic change to memory, which CONTRIBUTING reserves to a decision of its own. To be decided after a few weeks of `--retire` runs with no false retirement.
- **"Merged but in no release" claims**: they need the merge commit's ancestry against the latest tag; next, once this check proves itself.
- **Expiry dates on volatile facts** (`stale_after`, https://github.com/Barben360/backsight/issues/325 suggestion 6) and **the extraction leak itself**: measured separately.
