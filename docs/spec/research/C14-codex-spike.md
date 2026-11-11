# C14 — Codex 0.167.0 on the machine: what the spike confirmed

Source: a real Codex CLI 0.167.0 (official release asset `codex-x86_64-unknown-linux-musl.tar.gz`, sha256 `9d64836106d5b374e90ee4454c7ebcde7ca7106558bd7e41a34b9f26ebda251a`) run in a Docker container against a development build of backsight (`feat/codex` = main `3a3f46a5`), a deterministic fake Responses API server and a scratch Hindsight with its mock LLM (VERIFIED 2026-11-10), the container on an internal network with no route out (`checks/results.txt`, line 7). Codex source read at openai/codex `0d37c062de16c81caa1376e072ccd15cf3d68506` (paths below are relative to its `codex-rs/`). The harness and the recorded fixtures are in backsight: `scripts/codex/` and `internal/testdata/codex/0.167.0/` (fixture paths below are relative to that folder). Epic: https://github.com/Barben360/backsight/issues/276; block 1: https://github.com/Barben360/backsight/issues/285.

What a scripted model cannot show is marked **to confirm with DeepSeek** (how a real model follows the guide, tokens per session).

## 1. The plugin tree

- **The Claude layout loads as is.** `codex plugin marketplace add <data>/marketplace` registers it (`[marketplaces.backsight] source_type = "local"`), `codex plugin add backsight@backsight` installs it (`[plugins."backsight@backsight"] enabled = true`), and `codex plugin list --json` reports it (`config/plugin-list.json`: `pluginId`, `version`, `installed`, `enabled`, `source.path`, `marketplaceSource`). No `.codex-plugin/plugin.json` is needed: Codex reads `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` (`utils/plugins/src/plugin_namespace.rs`).
- Codex **copies** the plugin to `$CODEX_HOME/plugins/cache/<marketplace>/<plugin>/<version>/`; hooks run with `PLUGIN_ROOT` and `CLAUDE_PLUGIN_ROOT` pointing there, `PLUGIN_DATA`/`CLAUDE_PLUGIN_DATA` at `$CODEX_HOME/plugins/data/backsight-backsight`.
- A change in the local marketplace is **not** picked up by itself, version bumped or not: `codex plugin add backsight@backsight` again re-copies it (same version included) and replaces the cache folder.
- **Two changes are needed for the hooks**, everything else works unchanged:
  - **A hook handler is one shell command string.** The exec form's `"args"` is ignored: with the tree as generated (`checks/1-as-generated/hooks.json`, the installed copy), the review screen shows `Source Plugin - backsight@backsight` and `Command /home/tester/backsight/data/bin/current`, no arguments (`checks/1-as-generated/review-hook.txt`), and once trusted the hooks ran the shim with **no arguments** (4 runs seen by the tap, no backsight hook ran: `checks/results.txt`, line 1). The Codex tree needs `"command": "<shim> hook <event>"` (`protocol/src/hooks.rs` `HookHandlerConfig::Command`: `command`, `commandWindows`, `timeout`, `async`, `statusMessage`, `additionalContextLimit`). `${PLUGIN_ROOT}`-style placeholders are expanded from the plugin env before running.
  - **The PostToolUse matcher** must be `mcp__memory__.*` (the tool names below).
- The MCP server's `.mcp.json` works as generated: exec form (`command` + `args`) is honoured for MCP servers; `env` (literal values) and `env_vars` (names forwarded from Codex's environment) are both honoured (`env/mcp-*.env`).
- Skills load from the plugin as `backsight:<name>` (listed in the developer `<skills_instructions>` message, also in `codex exec`).

## 2. Tool names

| Where | Name |
|---|---|
| Model's tool list | a `namespace` tool `mcp__memory` holding functions `memory_search`, `memory_read`, `memory_store`, `memory_credit`; the server's `instructions` become the namespace description |
| Hook payloads (`tool_name`), matchers | `mcp__memory__memory_search` … (the plugin is not in the name; the server key `memory` is) |
| Approval config | `[plugins."backsight@backsight".mcp_servers.memory.tools.<tool>]` |
| MCP `_meta` | `plugin_id = "backsight@backsight"` |
| Shell tool in hooks | `Bash`, `tool_input.command` (Claude's names); the model calls it `exec_command` |
| `apply_patch` | with a model without metadata (any non-OpenAI slug, as DeepSeek's), the model runs it through `exec_command` (`apply_patch <<'EOF'…`): PreToolUse fires as `Bash`, **no PostToolUse** fires for it |
| Others | `spawn_agent`, `multi_agent_v1wait_agent` (sic), `request_user_input` |

A user MCP server also named `memory` in `config.toml` would share the `mcp__memory__` names (not tried). `codex mcp list --json` prints the configured servers.

## 3. Hook trust

- Every hook is skipped until trusted. The TUI asks at start ("Hooks need review · N hooks are new or changed · 1. Review hooks 2. Trust all and continue 3. Continue without trusting"), then `/hooks` lists them per event (`trust/`).
- Trust is stored **per handler** in `config.toml`: `[hooks.state."<key>"]` with `trusted_hash` and, for a plugin hook, `trusted_content_hash`. The key is `<plugin id>:<path in the plugin>:<event_snake>:<group index>:<handler index>`, e.g. `backsight@backsight:hooks/hooks.json:session_start:1:0`; for a user hook `<absolute path of hooks.json>:<event>:<g>:<h>` (`trust/config-after-trust.toml`).
- `trusted_hash` = sha256 of the normalized declaration: event, matcher, the handler after OS selection (`command`, `timeout`, `async`, `statusMessage`, `additionalContextLimit`; `commandWindows` is dropped once resolved) (`hooks/src/engine/discovery.rs` `hook_hash`).
- `trusted_content_hash` = sha256 over **every file of the installed plugin folder** (names, sizes, bytes; `hooks/src/engine/plugin_fingerprint.rs`), checked at each hook run (`plugin_trust.rs`).
- Measured on 0.167.0 (`scripts/codex/checks.sh`; each step is one line of `checks/results.txt`; "hooks run" means backsight's SessionStart ran in a `codex exec` turn):
  - one plugin, adapted for Codex and trusted (`checks/2-one-plugin/config-trusted.toml`, 9 handlers sharing one `trusted_content_hash`): hooks run (2a);
  - the binary behind the fixed shim replaced (outside the plugin folder; sha256 `dccc56e47f99…` → `1d6cfb40eea0…`): **trust kept**, hooks run (2b);
  - one skill's text changed, then `codex plugin add`: hooks **stop** (2c), and the next TUI start asks for review (`checks/2-one-plugin/review-after-skill-change.txt`); the same after bumping only `plugin.json`'s version (2d, `review-after-version-bump.txt`). In `codex exec` such a hook shows as `hook: SessionStart Failed` on stderr, nothing on `--json`;
  - **hooks in a plugin of their own** (`backsight-hooks@backsight`: its manifest and `hooks/hooks.json`, nothing else; skills and the MCP server stay in `backsight@backsight`; `checks/3-two-plugins/marketplace-files.txt`, `plugin-list.json`): trusted, under keys `backsight-hooks@backsight:hooks/hooks.json:…` (`config-trusted.toml`), hooks run (3a); a skill changed in the main plugin and `codex plugin add backsight@backsight`: hooks **still run** (3b) and the next TUI start asks nothing (`review-after-main-change.txt`); the hooks plugin's own version bumped: they stop until reviewed (3c, `review-after-hooks-change.txt`);
  - a user-level hook (`$CODEX_HOME/hooks.json`) has a `trusted_hash` and no `trusted_content_hash` (the probe hooks in `trust/config-after-trust.toml`): only its declaration counts (source: no plugin root, so no fingerprint).
- Codex never trusts a hook by itself; a hook that was never reviewed never runs, in the TUI or in `codex exec`.
- **Removal** (`checks/6-removal/`: `config.diff`, `left.txt`): `codex plugin remove <id>` and `codex plugin marketplace remove backsight` delete the `[plugins."…"]` and `[marketplaces.backsight]` tables and the cache copy, but **leave every `[hooks.state."backsight@backsight:…"]` table**, an empty `plugins/cache/backsight/` folder, and legacy trust baseline files `$CODEX_HOME/.hook-trust/<sha256>`, one per plugin (`plugin_trust.rs` `legacy_trust_path`).

## 4. Tool approvals

- Decision per call (`core/src/mcp_tool_call.rs`): a tool's approval mode is `auto` (default), `prompt`, `writes` or `approve`. With `auto`, approval is needed unless `readOnlyHint: true`; a tool with `destructiveHint: true` always needs it; otherwise it is needed unless **both** `destructiveHint: false` and `openWorldHint: false` are declared (missing hints count as true).
- Before that, with approval policy `never` and full disk write (`danger-full-access`, or no sandbox), every MCP call is approved (`codex-mcp/src/mcp/mod.rs` `mcp_permission_prompt_is_auto_approved`).
- Measured (`approval/exec-*.jsonl` with the probe tools and backsight's; `checks/4-approvals-with-hints/` with **backsight's own server** whose `tools/list` answer the harness tap gives `destructiveHint: false` and `openWorldHint: false` on `memory_store` and `memory_credit`, `tools-list-annotations.json`). "src" marks a cell read from the source, not run:

| Tool's hints | TUI (policy on-request) | `codex exec` (policy never), workspace-write | `codex exec`, full access |
|---|---|---|---|
| `readOnlyHint: true` (`memory_search`, `memory_read`) | runs | runs | runs |
| `destructiveHint: false`, `openWorldHint: false` | **runs, no prompt** (backsight's `memory_store` and `memory_credit`, `checks/4-approvals-with-hints/tui-screen.txt`; results 4a) | runs (probe `closed`; backsight's `memory_store`, results 4b) | runs |
| none (`memory_store`, `memory_credit` today) | prompt: Allow / Allow for this session / Always allow / Cancel (`approval/memory_store-prompt.txt`) | **denied**: "MCP tool call requires approval, but approval policy is never" | runs |
| `destructiveHint: true` | prompt (src) | denied | runs |
| any, with `approval_mode = "approve"` | runs (src; "Always allow" writes it) | runs | runs |

- In 4a/4b, `memory_credit` ran and answered "memory session not ready": backsight's Claude session pairing (block 5's work), not an approval.
- Other approval policies (src, `core/src/mcp_tool_call.rs` `request_mcp_tool_user_approval`, `protocol/src/protocol.rs` `AskForApproval`): only `never` turns a needed approval into a denial; `untrusted` (projects not trusted) and `granular` show the prompt as `on-request` does (granular's `mcp_elicitations = false` only drops MCP auth elicitations). The hints decide whether an approval is needed under every policy.

- "Always allow" in the TUI writes exactly `[plugins."backsight@backsight".mcp_servers.memory.tools.memory_store]` `approval_mode = "approve"`. The same key in the config makes `codex exec` run it (`approval/config-with-approvals.toml`). The other keys of that table: `default_tools_approval_mode`, `enabled_tools`, `disabled_tools`, `enabled`, and per tool `output_token_limit` (`config/src/types.rs` `PluginMcpServerConfig`).
- The hints act the same on a plugin's MCP server: backsight's (above), and in an ad hoc run (not recorded) a plugin-provided probe server, whose `closed` tool ran in `codex exec` with no config while `plain` and `destructive` were denied.
- A **PermissionRequest** hook fires whenever an approval is needed, in the TUI before the prompt and in `codex exec` before the denial (`tool_name`, `tool_input`, no `tool_use_id`), and may answer allow or deny (`hooks/src/events/permission_request.rs`).

## 5. `codex exec`

- Trusted hooks run (SessionStart through SessionEnd, the Stop block included); skills are listed; MCP tools are offered.
- Non-read-only tools without the annotations above or an approval key are denied (§4); with `-s danger-full-access` everything runs.
- Codex's sandbox (bubblewrap) cannot create a user namespace under Docker: every `exec_command` fails ("bwrap: No permissions to create a new namespace") and the TUI warns at start. The container must run Codex with `sandbox_mode = "danger-full-access"`.
- The standalone binary needs `--no-daemon` for its TUI ("this CLI has no complete local package"); `codex exec` does not.

## 6. Subagents

- `spawn_agent` creates a thread with its own id and rollout. Its hooks carry `session_id` = the **root** session, `agent_id` = the subagent thread id, `agent_type` (`default`), and the subagent's own `turn_id` (`hooks/PreToolUse.subagent.*.json`).
- SubagentStart (`agent_id`, `agent_type`, `transcript_path` = the subagent's rollout) and SubagentStop (`transcript_path` = the parent's, `agent_transcript_path` = the subagent's, `last_assistant_message`, `stop_hook_active`).
- **A subagent's task fires UserPromptSubmit** (with `agent_id`), and its SubagentStart context arrives as a developer message before its task.
- MCP `_meta` in a subagent: `sessionId` = the root session = the hooks' `session_id`; `threadId` = the subagent thread; `x-codex-turn-metadata` adds `parent_thread_id`, `subagent_kind: "thread_spawn"`, `thread_source: "subagent"`.

## 7. MCP `_meta` and processes

- `tools/call` `_meta` (`mcp/root.in.jsonl`): `callId` (= the hooks' `tool_use_id`), `sessionId`, `threadId`, `windowId`, `itemId`, `progressToken`, `plugin_id`, and `x-codex-turn-metadata` {`session_id`, `thread_id`, `turn_id`, `model`, `turn_started_at_unix_ms`, `thread_source`, `turn_trigger`, `sandbox`, `sandbox_mode`, `workspaces`, `codex_version`, …}.
- `_meta.sessionId` = the hooks' `session_id` for the same call (paired on `callId` = `tool_use_id` and the turn), in a fresh root session, after `/clear` (a new id on both sides), in `codex exec resume <id>` (the original id) and in `codex exec fork <id>` (the fork's new id) (`checks/5-identity/pairs.json`, results line 5); in a subagent see §6. In each case `threadId` = `sessionId` at the root.
- `initialize`: protocol `2025-06-18`, client `codex-mcp-client` 0.167.0, capabilities `elicitation`, `experimental.codex/auth-change`. backsight's server (go-sdk v1.8.0) answers it.
- **By default, one server process per thread**: the root session and each subagent start their own `backsight mcp` (`mcp/processes-default.txt`: 2 for a session with one subagent). Nothing is shared across Codex processes.
- **Sharing is behind development flags**: with `features.mcp_2026_07_28` and `features.mcp_connection_sharing` on and the server's launch env `CODEX_MCP_PROTOCOL_VERSION = "2026-07-28"`, one process served both threads (`mcp/processes-shared.txt`, `mcp/shared-process-calls.json`); go-sdk v1.8.0 speaks 2026-07-28; calls carry the same `sessionId`, their own `threadId`, plus `io.modelcontextprotocol/clientInfo|protocolVersion|clientCapabilities`; no `codex/connectionLease` (go-sdk does not declare Codex's lease capability). Both flags are "under development", off.

## 8. Hook events

Recorded payloads: `hooks/<Event>[.<source|trigger>][.subagent][.<tool>].json`; backsight's own input and answer: `backsight/<event>.in.json`, `.out.json`, `.rc`.

| Event | Fields beyond `session_id`, `transcript_path`, `cwd`, `hook_event_name`, `model` | Notes |
|---|---|---|
| SessionStart | `source` (`startup`, `resume`, `clear`, `compact`, `fork`), `permission_mode` | Fires at the first prompt, not when the TUI opens. `compact` fires at the next turn after a compaction. `/clear` starts a new session id and **ends the old one with no SessionEnd**. `fork` (`codex exec fork <id>`) is a new session id. |
| UserPromptSubmit | `turn_id`, `prompt`, `permission_mode`; `agent_id`/`agent_type` in a subagent | exit 2 blocks the prompt: no model request. |
| PreToolUse | `turn_id`, `tool_name`, `tool_input`, `tool_use_id` | exit 2 + stderr: the call is blocked, the model gets "Tool call blocked by PreToolUse hook: <stderr>. Tool: <name>". |
| PermissionRequest | `turn_id`, `tool_name`, `tool_input` | allow/deny verdict. |
| PreToolUse `request_user_input` | as PreToolUse | Measured: the tool is in the model's tool list in Default mode (`model/turn-first-request.json`), and a call fires PreToolUse with `permission_mode: "default"` (`hooks/PreToolUse.request_user_input.json`); the tool then answers "request_user_input is unavailable in Default mode" (`rollouts/root.jsonl`). From the source (`core/src/tools/handlers/request_user_input_spec.rs`, feature `default_mode_request_user_input` off): it works in Plan mode only. Plan mode was not run; **to confirm with a real model**. |
| PostToolUse | `turn_id`, `tool_name`, `tool_input`, `tool_response`, `tool_use_id` | Fires for MCP tools; `tool_response` is the MCP result as an object (`{"content":[{"type":"text","text":…}]}`), a string for `Bash` (the output) and Codex's own tools (their JSON). Not for the intercepted `apply_patch`, nor for an MCP call that failed (an error result, e.g. `memory_read` of an unknown id) or was denied; there is no PostToolUseFailure. |
| PreCompact / PostCompact | `turn_id`, `trigger` (`manual`; `auto` per source) | |
| Stop | `turn_id`, `stop_hook_active`, `last_assistant_message` | `{"decision":"block","reason":…}` or exit 2 + stderr: Codex continues the turn with a user message `<hook_prompt hook_run_id="stop:…">reason</hook_prompt>` (no UserPromptSubmit for it); the next Stop has `stop_hook_active: true`. |
| SubagentStart / SubagentStop | §6 | |
| SessionEnd | `reason` (always `"other"` on 0.167) | Root threads only; the rollout is flushed first. |
| Interrupt, PreModelRequest, PostModelRequest | | New events, no Claude counterpart. |

- Accepted outputs: `hookSpecificOutput.additionalContext` (SessionStart, UserPromptSubmit, SubagentStart, PreToolUse, PostToolUse), plain stdout on UserPromptSubmit/SessionStart (taken as context), `systemMessage` (shown in the TUI as "↳ Hook · …"), Stop's `decision: "block"`, exit 2. backsight's current SessionStart, UserPromptSubmit and Stop answers were all accepted unchanged.
- Timeouts: default **600 s**; SessionEnd and Interrupt default 1 s, clamped to 1-3 s (`discovery.rs` `normalize_command_hook`).
- A command runs as `$SHELL -lc "<command>"` (a **login shell**) on Unix and `%COMSPEC% /C "<command>"` on Windows, where `commandWindows` replaces `command` (`hooks/src/engine/command_runner.rs`); its env is Codex's own plus `PLUGIN_ROOT`, `PLUGIN_DATA` and their `CLAUDE_*` twins (`env/hook.env`). No `CODEX_THREAD_ID` in hooks.
- Machine schemas of every event's input and output: `hooks/schema/generated/*.schema.json` in the Codex source.

## 9. Rollouts

- `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<local time>-<thread id>.jsonl`, one per thread (subagents included). First line `session_meta`: `id`, `session_id` (the root), `parent_thread_id` (subagent), `source` (`cli`, or `{"subagent":{"thread_spawn":{"parent_thread_id","depth","agent_nickname",…}}}`), `thread_source`, `originator`, `cli_version`, `model_provider`, `history_mode: "paginated"`, `cwd`, `git`, `base_instructions`.
- Record types seen (`rollouts/root.jsonl`): `session_meta`, `turn_context`, `response_item` (`message`, `function_call`, `function_call_output`), `event_msg` (`task_started`, `task_complete`, `item_completed`, `token_count`, `thread_settings_applied`), `compacted`, `world_state`, `token_usage_record`. New since the study: `world_state`, `token_usage_record`, `thread_settings_applied`, `item_completed`.
- Resume appends to the same file. Fork writes a new file with a new id (`rollouts/fork.jsonl`).
- **Compression** (`features.local_thread_store_compression`, under development): at startup a worker replaces each rollout older than 7 days (by mtime) with `<same name>.zst` (zstd level 3, mtime kept, plain file removed); byte-identical once decompressed (`rollouts/compression.txt`, `rollouts/compressed.jsonl.zst`; `rollout/src/compression.rs`).
- **Pagination**: new threads are `history_mode: "paginated"` by default on 0.167: a SQLite projection (`$CODEX_HOME/thread_history_1.sqlite`, table `thread_items`) beside the JSONL, which stays complete. `codex migrate-rollouts [--apply]` and `features.background_paginated_rollout_migration` convert legacy threads.

## 10. What the model is sent

- AGENTS.md: a **user** message, `# AGENTS.md instructions for <cwd>\n\n<INSTRUCTIONS>…</INSTRUCTIONS>`, together with `<environment_context>`.
- Hook `additionalContext`: a **developer** message; SessionStart's before the user's prompt, UserPromptSubmit's right after it (`model/turn-first-request.json`).
- The cap is a **spill**, not a cut: past `additionalContextLimit` (default **2,500 tokens**, estimated as bytes / 4) Codex keeps a head and a tail (≈ 2,470 tokens, ≈ 10,100 characters of 43,200), writes `Warning: truncated output (original token count: 10800)`, `…8331 tokens truncated…` in the middle, and saves the whole text to `/tmp/hook_outputs/<thread>/<uuid>.txt`, named at the end (`model/additional-context-cap.json`). `additionalContextLimit: 0` on a handler disables the spill (`hooks/src/output_spill.rs`).

## 11. Codex's own memories

- Feature `memories` (stable, **off** by default): `[features] memories = true` in `config.toml` (or a profile, or `--enable memories`). `[memories]` configures it (`generate_memories`, `use_memories`, …; `config/src/types.rs` `MemoriesToml`).
- Detect: `codex features list` prints `memories  stable  true|false`, the effective state.
- Turn off: `codex features disable memories` removes the key (leaving an empty `[features]` table); `memories = false` does the same by hand.

### The stores on disk (VERIFIED with the fake model; `memories/<v2|v1|dual>/`)

Driven by `scripts/codex/memories.sh`: the feature on, three interactive sessions, their threads aged in `state_5.sqlite` past the idle floor (`min_rollout_idle_hours`, 6 by default, 1 at least) and the previous consolidation aged past Codex's 6-hour pause between consolidations, then one more session. Source: `codex-rs/memories/README.md`, `memories/write/src/{start,phase1,phase2,storage,workspace}.rs`, `core/src/config/mod.rs`.

- **Which store**: `$CODEX_HOME/memories_v2/` (`version = "v2"`) for a profile with no `memories/` folder when the feature is turned on (the default for a fresh profile); `$CODEX_HOME/memories/` (v1) when `memories/` already exists or `[memories] version = "v1"`; **both** with `version = "v1"` and `dual_write = true`. Beside them: `memories_1.sqlite`, `memories_v2_1.sqlite` (tables `jobs`, `stage1_outputs`, `consolidation_progress`), `memories_v2.lock`. The stores are global, not per project.
- **When**: at the start of each root interactive session (`cli`, VS Code; not `codex exec`, not subagents, not ephemeral), in the background. Phase 1 sends each eligible thread (idle ≥ the floor, younger than `max_rollout_age_days` = 10, at most `max_rollouts_per_startup` = 2 per start) to the model with a structured-output schema (`rollout_summary`, `rollout_slug`, and in v1 `raw_memory`), redacts secrets, stores it in the database. Phase 2, at most once per 6 hours, writes the files below and runs a **consolidation agent** (model `consolidation_model`, through Codex's code mode: it needs the `codex-code-mode-host` binary of the packaged CLI; the standalone binary alone fails closed).
- **Files**, v2 (`memories_v2/`):
  - `memory_summary.md`: the consolidated memory, written by the agent; Codex checks it starts with the line `v1`, has `## User Profile`, `## User preferences`, `## General Tips`, `## What's in Memory`, and stays under 10,000 bytes. The prompt asks for recent work under `### <project scope>` / `#### <YYYY-MM-DD>` with pointers `rollout_summaries/<file> — <sentence>; thread_id=<id>`. Injected at the start of every new session.
  - `rollout_summaries/<YYYY-MM-DDTHH-MM-SS>-<4 chars>-<slug>.md`, one per selected thread (the time is the thread's creation, from its UUIDv7), each with a header `thread_id:`, `updated_at:` (RFC 3339), `rollout_path:`, `cwd:`, `git_branch:` (when known), then the summary (≤ 9,000 bytes in v2). Pruned when the thread is no longer selected (unused for `max_unused_days` = 30).
  - `extensions/ad_hoc/instructions.md` (Codex's template; from the source only, never observed: notes would go in `extensions/<name>/resources/<YYYY-MM-DDTHH-MM-SS>*.md`, pruned after 14 days; nothing in the runs wrote one).
  - `.git/` (the consolidation baseline), `.codex-baseline/`, and `phase2_workspace_diff.md` while an agent runs (removed after).
- **Files**, v1 (`memories/`): the same, plus `MEMORY.md` (the handbook, agent-written), `raw_memories.md` (Codex's merge of the phase-1 raw memories: `## Thread \`<id>\`` with `updated_at`, `cwd`, `rollout_path`, `rollout_summary_file`, then the text), and optional agent-written `skills/<name>/`.
- **Sizes** here are the fake model's (summaries 256 bytes); real sizes and the quality of `memory_summary.md` / `MEMORY.md` come from a real model: **to confirm by the author**.

## 12. Environment

| Process | Gets |
|---|---|
| MCP server | `HOME`, `PATH`, `PWD`, `SHELL`, `TERM` (+ locale when set) and what `.mcp.json`'s `env`/`env_vars` name; **not** `CODEX_HOME`, not `BACKSIGHT_HOME` |
| Hooks | Codex's whole environment + `PLUGIN_ROOT`, `PLUGIN_DATA`, `CLAUDE_PLUGIN_ROOT`, `CLAUDE_PLUGIN_DATA` |
| Shell commands | Codex's environment + `CODEX_SESSION_ID` (root), `CODEX_THREAD_ID`, `CODEX_TOOL_CALL_ID`, `CODEX_VERSION`, `CODEX_CI=1`, `CODEX_HOME`, `NO_COLOR=1`, `PAGER=cat` |

`CODEX_HOME`: the variable when set and non-empty (it must exist and be a directory; canonicalized), else `~/.codex` (`utils/home-dir/src/lib.rs`).

## 13. Minimum version

0.167.0. Everything above was observed on it; older versions were not run. The study's sources point at features recent in 0.16x (plugin hooks with content trust, the `mcp__` namespace tools, `_meta.sessionId`); without a run on an older release, nothing older is claimed.

## To confirm with DeepSeek (the author)

- Whether a real model calls `memory_search` / `memory_credit` as the guide asks, with the namespace tool and the developer-message context.
- Tokens per session (input, cached, output) for block 10's budget.
- The DeepSeek provider block on 0.167 (`wire_api = "responses"`, `env_key`), or the LiteLLM fallback.
- With DeepSeek's slug (no Codex model metadata), whether `apply_patch` again goes through `exec_command` only, as with the fake model.
- Codex's own memories with a real model (packaged CLI, so the code-mode host is there): the real `memory_summary.md` / `MEMORY.md`, sizes, and whether the consolidation runs at all with a non-OpenAI consolidation model.
