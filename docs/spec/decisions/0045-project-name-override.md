# 0045 — Project names can be overridden; collisions are detected

- **Date**: 2026-10-24
- **Status**: accepted (agent, from the independent spec critique of 2026-10-24; open to veto)
- **Decided by**: agent

## Decision
- `.backsight.toml` at the repository root may set `project = "<name>"`, overriding the worktree basename. It is meant to be committed so every clone and machine uses the same name.
- Session state records the git remote URL (normalised) seen for each project. `doctor` warns when one project name is seen with two different remotes ("two repositories share project:api"), with the fix (`project = …` in one of them).
- Renaming a repository: `project = "<old name>"` keeps its history in one pool; no retag in v1.
- Tagging by remote URL is rejected: tags must stay readable in recall output and continuous with the reference stack's names.

## Why
Generic names (`api`, `backend`) would merge two observation pools and let corrections from one repository overwrite the other's facts.
