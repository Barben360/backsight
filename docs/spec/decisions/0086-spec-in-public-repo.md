# 0086 — The specification moves into the public repository

- **Date**: 2026-11-12
- **Status**: accepted
- **Decided by**: author (2026-11-12: "We'll go public after cleaning. It's important so people can propose their own PRs next.")
- **Supersedes**: 0006 (private specification repository, commits straight to `main`)

## Context

0006 kept the specification and every decision in a private repository, so that nothing was published before the product was. backsight is now public (1.0 released), and its code cites decisions by number in about 430 places that no reader can open. A contributor cannot see why the code is the way it is, nor propose a change the way the author does (a decision first).

## Decision

- The specification, the decisions, the roadmap, the open questions, the research and the experiment reports live in `docs/spec/` of `Barben360/backsight`. `docs/docs.go` embeds `docs/*.md` only, so none of it reaches the binary's `guide`.
- Imported as a cleaned copy, without its Git history. Left out: the implementation plans, the experiment scripts and raw outputs (they hold the author's own memory data), and anything personal. The author's other projects keep the stand-in names the experiments already used.
- The private repository is archived, not deleted: it keeps the full history and what was left out.
- From now on, changes to the specification go through pull requests in backsight, like code. A decision may ship in the same pull request as the code that builds it. The "spec first" order (decision, issue, plan, code) is unchanged.
- Anyone may propose a decision by pull request (status `proposed`); the author accepts, amends or declines it.

## Why

Readable reasons and an open way to propose changes are what a public project needs for outside pull requests. Copying without history is the only version that can be checked line by line before publishing: the 214 commits of the private repository include files removed since and raw data.
