# 0002 — Never create GitHub issues automatically

- **Date**: 2026-10-23
- **Status**: accepted
- **Decided by**: author

## Context
The request included self-criticism able to open issues on the repository. Automatic issues can leak memories, prompts or paths into a public repo, need the user's GitHub credentials, and flood the maintainer with duplicates.

## Decision
The binary never opens an issue by itself. When a problem is worth reporting, it **suggests** it: it builds a redacted diagnostic, shows it to the user, and offers a pre-filled issue (browser URL) after checking for duplicates. The user submits, or not.
