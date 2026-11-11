# Open questions

## Checks (facts first) — results in `research/`

| # | Check | Status |
|---|---|---|
| C1 | Licenses: Hindsight server, npm plugin `@vectorize-io/hindsight-coding-agents` | done |
| C2 | Naming: Anthropic brand rules for "Claude", vectorize for "Hindsight"; GitHub name, Go module path | done |
| C3 | Hindsight API: latest version, OpenAPI spec, stability, release pace | done |
| C4 | Full inventory of the npm plugin (transcript parsing, chunking, retain payload, tags/metadata, bank creation, MCP tools, pages) | done |
| C5 | Claude Code plugins: can one plugin ship hooks + MCP + skills + commands + per-OS binary? Update flow | done |
| C6 | Claude Code hook contract per event, timeouts, Windows shell behaviour | done |
| C7 | LLM provider matrix: settings, structured output, `claude-code` auth inside a container, subscription terms | done |
| C8 | GPU inference without Python: llama.cpp `llama-server` (rerank/embeddings, Vulkan/ROCm), TEI, onnxruntime; benchmark on the Radeon 890M | done |
| C9 | Docker optional? Hindsight native run with embedded Postgres (`pg0`); Docker Desktop licensing; Podman/OrbStack | done |
| C10 | Remote mode: Hindsight auth over a network, TLS, hook latency, SSH tunnel vs TLS+key vs Tailscale | done |
| C11 | Go building blocks: MCP Go SDK, goreleaser, signing, self-update, TUI, CGO-free | done |
| C12 | Usefulness data from the current hooks (stats, event log, retro log) | done |
| C13 | Migration of existing installs (author's two machines) | done |

## Decisions (in dependency order)

| # | Decision | Status |
|---|---|---|
| D1 | v1 scope and non-goals | decided → 0010 |
| D2 | Name, license, module path, versioning, release channels | author → 0034 (`backsight`, MIT); versioning semver v0.x, channel = GitHub releases (0027) |
| D3 | Architecture: what runs where; local / remote / multi-machine | 0011, 0024, 0033 (separate memories) |
| D4 | Fully owning the client (dropping the npm plugin) | agent → 0018, 0031 (settled retain, E18b), ROADMAP M2 |
| D5 | Delivery into Claude Code | agent → 0012 (E1) |
| D6 | Install UX, agent contract, remote flow | agent → 0022, 0024; retention question 0041 and config editor 0042 (author requests) |
| D7 | LLM provider matrix, defaults, cost, secret storage | agent → 0014, SPEC §12; subscription lane → 0032 (author); local lane measured in E13 |
| D8 | Embedding / reranker presets, hardware | agent → 0026; GPU service → v2 (0035, author) |
| D9 | Memory model | agent → 0015, 0016, 0030 (E10-E12, E17) |
| D10 | Hook behaviours, instructions, retro default | agent → 0020, 0021, 0028 (no retro job), 0029 |
| D11 | Privacy | agent → SPEC §13 |
| D12 | Updates | 0005 + agent → 0017, 0027 |
| D13 | Backups | agent → 0023 |
| D14 | Periodic tasks | agent → 0025 |
| D15 | Doctor / self-criticism | agent → SPEC §15 |
| D16 | Testing and release pipeline | agent → SPEC §16 |
| D17 | User documentation and agent guide | agent → SPEC §11.1-11.2 (published guide); agent guide written in M4 (`docs/agent-guide.md` in the backsight repository), user documentation in M5, embedded in the CLI with search (0069) |

## Open for the author (2026-10-24)

| # | Question | Recommendation | Evidence |
|---|---|---|---|
| Q5 | Language of facts and agent-facing texts | **decided** → 0039 | E19, E20 |
| Q6 | Roadmap order after migration-by-rebuild (0036) | **decided** → 0040 | ROADMAP v7 |
| Q7 | Backfill from an old Hindsight's stored session text (`original_text`) for sessions without a local transcript | **no** for the Linux machine (its 12 stored sessions all have transcripts, 2026-10-24); the work machine is out of scope (author, 2026-10-25) | API read of the reference bank |
| Q8 | Efficiency phase 3 (0083 C8, "Not done"; 2026-11-08): (1) the credit loop: skip Stop-check blocks on turns where Claude searched itself, an implicit `none`, or keep as is; (2) briefing refresh on demand at SessionStart instead of after consolidation; (3) recall scopes merged into one tag-filtered call | decide each on one week of data after the 0083 release, then a follow-up issue or "not doing" in the spec; (3) needs an A/B: overlap@10 ≥ 0.9 on 50 real prompts and no credit-rate drop | https://github.com/Barben360/backsight/issues/230; telemetry `stop_block`, `tool.query_hits`, `briefing_trigger` (0083 C8) |

## Spec critique (2026-10-24)

Independent critique of SPEC v8 (no fact checking, design only): 15 points. Folded into SPEC v9 through 0043 (subscription lane limits), 0044 (restore epoch), 0045 (project name override), 0046 (memory threats), 0047 (backfill budget), 0048 (preview after M5, success criteria, M1 blocks, dev profile); smaller points edited in place (briefing wording, `verify --live`, uninstall scope, `pause --project`, retry-quarantine, cleartext LAN warning, retention default). Kept as is: remote role in M6 (decision 0003, author). Amendment index: `decisions/INDEX.md`.
