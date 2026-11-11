# 0069 — Documentation embedded in the CLI, browsable, with semantic search in a docs bank and a keyword fallback

- **Date**: 2026-10-25
- **Status**: accepted
- **Decided by**: author (2026-10-25: "I'd like documentation to be embedded in backsight's CLI, with browsing and search … a Hindsight database holding the documentation, to have semantic search"; then "I agree with your recommendations"), on the agent's design

## Decision
- **Source of truth**: Markdown in `docs/` of the backsight repository, embedded in the binary (`embed`) and published on GitHub as the same files. The agent guide (`docs/agent-guide.md`) is one of them (§11.1, OPEN D17).
- **Browsing**: `guide` lists the topics; `guide <topic>` shows one through `$PAGER` (plain output when not a terminal); `--json` for agents. No full-screen interface.
- **Semantic search**: `guide search <query> [--json]` recalls from a docs bank `<bank>-docs`.
  - One document per section, tagged with the docs version (the binary version's docs hash).
  - Bank settings as for the control bank (0044 amendment): `retain_extraction_mode: chunks`, `enable_observations: false`. That means 0 LLM calls; only the server's local embeddings run.
  - The multilingual embedding and reranker models (0039) let a question in French find English text.
- **Offline fallback**: when the server cannot be reached, or the docs bank is missing or on another version and cannot be refreshed, the same command runs a local keyword search over the embedded docs and says so in its output. Docs work before a server exists and while it is down.
- **Indexing**: at `setup`, after the bank step, and at `update apply`. Also lazily from `guide search` when the bank holds another docs version, for example after a restore or on a shared server. Documents whose section hash is unchanged are not resent.
- **Isolation**: like the control bank, recall, doctor's untagged and drift checks, `bank apply`, stats and `projects` never read `<bank>-docs`. It can always be rebuilt from the binary.
- **For Claude**: the `backsight:memory` skill can run `backsight guide search` for questions about backsight itself. No new MCP tool, so no extra context per session.
- **Gate**: before semantic search becomes the default, a small measure on about 20 real questions (some in French) compares it with the keyword fallback. Semantic stays the default only if it ranks the right section higher; otherwise the keyword search is the default and the bank is dropped.
- **Placement**: 0067 block 4, with `guide` and `verify --live` (0068).

## Why
The docs corpus is small, so keyword search alone would mostly work, except for questions phrased differently from the text, or in another language. The server already runs multilingual embeddings for free in `chunks` mode, so semantic search costs nothing extra. The keyword fallback keeps the docs usable exactly when they matter most: before install, and while the server is broken.
