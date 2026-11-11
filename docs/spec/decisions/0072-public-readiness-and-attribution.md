# 0072 — Public readiness for every Claude Code user, automatic attribution that never diverges

- **Date**: 2026-10-26
- **Status**: accepted
- **Decided by**: author (2026-10-26: "Je te suis sur tes suggestions, développe tout selon tes recommandations. Le but recherché est que backsight soit le plus stable possible et ne diverge jamais (pas d'augmentation du bruit, du mauvais étiquetage, etc, quitte à faire des consolidations de temps en temps si absolument nécessaire. Le mieux étant de ne rien laisser diverger)."), on the agent's triage of the five public reviews and of E32
- **Amends**: 0071 (C2b becomes S5), 0042/C3 (`.backsight.toml` trust), SPEC §11.1 (audience)

## Principle: no divergence
backsight must stay stable over time: noise, wrong project labels and stale data must not grow. Prevention at write time comes first; a consolidation is acceptable only when prevention cannot reach the data, and then it is a reusable, idempotent command, never a one-off script. Every rule that shapes the memory gets a doctor check that shows when it drifts.

## Audience
Claude Code users of every level: a simple path (cost, prerequisites, a "does it work" check, how to turn it off) and full depth (design, privacy model, config reference). Nothing a user sees names a private reference (decision numbers, experiments, milestones, SPEC sections).

## Public-review lots (reports of 2026-10-26)
- **S1** redaction gaps (age keys, netrc, docker, webhooks, PGP, Azure…), **S2** supply chain (Go 1.26.10, govulncheck, Dependabot, throwaway snapshot key, uncached release; backsight PR 11).
- **A** setup robustness: Docker permission and daemon detection on Docker 26, resume after a wrong key (own server on the ports), clear wrong-key message, download progress, backup identity, changing an answer, doctor on a previous install's backup, uninstall leftovers (images, anonymous volume).
- **B** no internal labels in user-facing text, with a test; hand-written release notes; stale versions in SECURITY.md and compatibility.md.
- **C** docs for every level, privacy statements made true (Hugging Face downloads; Claude is told never to ask for a key: the sentence goes into the injected guide), provider terms and data location, the `claude-code` preset kept with Anthropic's clause quoted and an API key recommended, third-party components.
- **D** third-party license notices in archives and `backsight version --licenses`. **E** Ollama on Linux (`host-gateway`). **F (small)** `make check`, CONTRIBUTING prerequisites, the chaos switch behind `testhooks`. The larger refactors of F (design doc, `internal/cli` split, shared fake server, one lock helper) come after the first outside users.
- **M5** a `.backsight.toml` tracked by git is trusted only for what protects (`retain = false`, exclusions); its `project` and `related` are ignored and doctor says so; an untracked local file is trusted fully.
- **M6** the memory tools stay auto-allowed; `memory_store` notes go to the current project, the transverse pool only when asked explicitly; recent notes are easy to review (`memory scan`, doctor).
- Repository settings (applied 2026-10-26): Dependabot security updates on, `govulncheck` a required check, only GitHub-owned actions plus goreleaser, golangci-lint and gitleaks, SHA pinning required, a required reviewer (the author) on the `release` environment with no admin bypass.

## Attribution (E32)
- **S5** (path majority per turn over the author's own repositories, writes weigh 3, can override the launch repository; third-party clones are never projects) is applied to **subagent** turns now (blind confirmation: 95 % on 80 moved turns).
- On **main turns** S5 runs in **shadow mode**: its answer is logged (telemetry) next to the applied one. It is switched on after a blind grading of fresh sessions passes 0071's gate (≥ 90 % on ≥ 40 moved turns).
- **Families**: projects co-touched in ≥ 5 turns of ≥ 2 sessions that make ≥ 20 % of the larger project's turns over 60 days are linked: searched in the project scope like `related`, no boost, pairwise, at most 10. Validated by a recall replay before it is on by default.
- **`backsight retain reattribute [--plan]`**: recomputes the attribution of sent documents with the current rule; a document whose whole content changes project is re-tagged (`PATCH /documents/{id}`, free), partial moves and legacy mixed documents are queued for a re-send with their cost. Idempotent; after a verified backup. Run once on the past, and again after any rule change. `jobs maintain` applies the free re-tags and doctor reports pending re-sends and the shadow disagreement rate.
