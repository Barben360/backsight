# Initial request and review (2026-10-23)

## The request (author's words, condensed)

New repository `hindsight-claude`, public eventually, private for now:

- Like `ref-stack`: document and provide everything needed to run the exact Hindsight stack for Claude Code used today.
- Installation by an agent: instructions clear enough to set everything up, with user choices: LLM service (Claude subscription, Claude API key or any Anthropic-compatible API, OpenAI, Ollama, and more).
- Choice of local model for embeddings/search (multilingual, English, size depending on the machine).
- System autodetection (ROCm, CUDA, anything that improves performance).
- Updates: a changelog so anyone can bring their install to the latest state.
- Backups: a Go CLI, precompiled for Windows, Linux x64/arm64, macOS arm64, handling the whole stack's operations and producing, in one command, a tar.gz from which the whole setup and memory can be restored. What users do with it is their responsibility.
- Self-healing: periodic self-update from the latest version, self-criticism, and the possibility to open issues on the repository when something is wrong.

## Review findings (accepted by the author)

| Topic | Finding | Outcome |
|---|---|---|
| "Exact same stack" | Today's stack is personal: French prompts and citation regexes, French bank instructions, `tu` CLAUDE.md section, hard-coded transverse names, Linux-only pieces (systemd, docker0, pass). | Must become configuration with language presets. |
| LLM providers | Hindsight 0.10.1 already supports ~30 providers natively (incl. `claude-code`, `anthropic`, `openai`, `ollama`, `llamacpp`, `lmstudio`, `litellm`, `openrouter`, `gemini`, `bedrock`, `vertexai`, `deepseek`). | Installer = provider matrix + live validation, no provider code. Subscription use in background to check against Anthropic terms. |
| Embeddings | Changing the model after data exists means re-embedding everything. | Few presets; model recorded in stack metadata; a migration, not a setting. |
| GPU | Containers on macOS have no GPU; host service needed (as done today for ROCm). | Detect, then benchmark to decide. |
| Updates | A changelog applied by an agent is fragile. | Numbered idempotent migrations run by the binary + managed blocks; changelog explains them. |
| Runtimes | Go + Python + Node + Docker = 4 runtimes; Python is painful on Windows/macOS. | Author decided: everything in Go, one binary (decision 0001). |
| Backup | Archive holds secrets and the whole memory; restore needs versions. | Optional encryption, version metadata, `verify` command. |
| Self-update | Hooks see every prompt: auto-updating from `main` of a public repo is a supply-chain risk for every user. | Signed releases only, notify by default, opt-in auto-apply, rollback on failed health check. |
| Auto-issues | Privacy leak and spam risk. | Author decided: suggest only (decision 0002). |
| Forgotten | Code ownership, licenses and names, privacy/redaction, multi-machine, uninstall, owning the client vs the npm plugin, Claude Code API drift, CI matrix, cost visibility, memory hygiene, one install path. | All tracked in `OPEN.md` / decisions. Code ownership confirmed by the author (decision 0009). |
