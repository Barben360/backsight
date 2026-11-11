# C06 — Claude Code hook contract (events relevant to hindsight-claude)

Date: 2026-10-23. Local Claude Code **2.1.309**. Primary source: hooks reference https://code.claude.com/docs/en/hooks ([H]), read in full the same day (279 kB page, read in 4 slices). Secondary: plugin docs (see C05), our hook `~/.claude/hooks/memory-search.py`, the npm plugin (`$SRC` = `https://github.com/vectorize-io/hindsight/blob/integrations/coding-agents/v0.7.0/hindsight-integrations/coding-agents/src`), local transcripts under `~/.claude/projects/`.

V = VERIFIED (doc text or observed), I = INFERRED.

## 1. Execution model (all events)

| Aspect | Contract | Source |
|---|---|---|
| Transport | Event JSON on **stdin**; response on stdout (JSON or plain text); stderr for errors | [H "Common input fields"] V |
| Handler types | `command`, `http`, `mcp_tool`, `prompt`, `agent`. `SessionStart` (and `Setup`) support only `command` and `mcp_tool` | [H "Prompt-based hooks"] V |
| Command fields | `type`, `command`, optional `args` (exec form: no shell, `command` resolved as executable), `shell` (`bash` / `powershell`), `timeout` (s), `async`, `asyncRewake`, `onFailure` (`continue`/`block`, v2.1.295+), `statusMessage`, `once` (skills only), `if` (permission-rule filter, tool events only) | [H "Command hook fields"] V |
| Shell | "`sh -c` on macOS and Linux, Git Bash on Windows, or PowerShell when Git Bash isn't installed"; `shell:"powershell"` forces PowerShell (`pwsh.exe`, fallback `powershell.exe`); Git Bash path override `CLAUDE_CODE_GIT_BASH_PATH`; exec form on Windows "requires `command` to resolve to a real executable such as a `.exe`" (`.cmd`/`.bat` need shell form) | [H "Windows shell", "Windows PowerShell tool"] V |
| Placeholders in PowerShell | `${CLAUDE_PROJECT_DIR}`, `${CLAUDE_PLUGIN_ROOT}`, `${CLAUDE_PLUGIN_DATA}` rewritten to `${env:NAME}`; bare `$CLAUDE_PROJECT_DIR` resolves to `$null` | [H] V |
| Default timeouts | `command`/`http`/`mcp_tool` **600 s**; `prompt` 30 s; `agent` 60 s; lowered to **30 s on UserPromptSubmit** (and Pre/PostModelSwitch), 10 s on MessageDisplay; **SessionEnd: 1.5 s total budget**, raised by per-hook `timeout` up to 60 s — "Timeouts set on plugin-provided hooks don't raise the budget"; `CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS` | [H "Command hook fields", "SessionEnd"] V |
| Maximum timeout | not stated in [H] | — |
| Async | `async:true` → background, can't block; results (`additionalContext`, `systemMessage`) delivered next turn; timeout not enforced; killed at teardown in `-p` mode ("start a fully detached process" to outlive) | [H "Run hooks in the background"] V |
| Concurrency | "When an event fires, its matching hooks run in parallel"; identical handler across settings files runs once, "A plugin's or skill's copy of the same handler stays separate"; entries merge across settings levels | [H] V |
| Matchers | omitted / `""` / `"*"` = all. SessionStart matcher = `source`; PreToolUse = tool name; PreCompact = `manual`/`auto`; SessionEnd = reason; MCP tools `mcp__<server>__<tool>` (plugin: `mcp__plugin_<plugin>_<server>__<tool>`) | [H], [C05] V |
| Environment | inherits parent env (minus `OTEL_*`); `CLAUDE_PROJECT_DIR` (project root where session started); `CLAUDE_PLUGIN_ROOT`, `CLAUDE_PLUGIN_DATA`, `CLAUDE_PLUGIN_OPTION_<KEY>` (plugin hooks); `CLAUDE_EFFORT`; `CLAUDE_CODE_REMOTE="true"` in web sessions; `CLAUDE_CODE_BRIDGE_SESSION_ID`; `CLAUDE_ENV_FILE` (SessionStart: append `export X=…` lines to set env for later Bash commands). No `$CLAUDE_MODEL` | [H] V |
| Workspace trust | interactive sessions hold back ALL settings hooks (even user ones) until the folder is trusted; `-p`/SDK treat folder as trusted | [H "Workspace trust"] V |
| Debug | `claude --debug` → `~/.claude/debug/<session-id>.txt`; v2.1.296+ logs `Hooks: <Event>:<match> [cmd] finished with status N (Xms)` / `timed out after` | [H "Debug hooks"] V |

## 2. Common stdin fields

`session_id`, `prompt_id` (UUID of current prompt), `transcript_path`, `cwd`, `scratchpad_dir` (v2.1.257+, optional), `permission_mode` (`default|plan|acceptEdits|auto|dontAsk|bypassPermissions`), `effort.level`, `hook_event_name`, `agent_id` / `agent_type` (inside a subagent or `--agent`) — [H "Common input fields"] V.

## 3. Exit codes and output

| Exit | Meaning | Source |
|---|---|---|
| 0 | success; JSON fields applied; plain stdout = **context for Claude only on UserPromptSubmit, UserPromptExpansion, SessionStart, PostModelSwitch**, else debug log; "Claude never sees stderr from a hook that exits 0" | [H "Exit code 0"] V |
| **2** | blocking error; stderr = reason; "Exit 2 blocks even when JSON is printed". Blocks on: **PreToolUse, UserPromptSubmit (erases prompt), Stop, SubagentStop**, PreCompact, PostToolBatch, TaskCreated/Completed, TeammateIdle, ConfigChange, Pre ModelSwitch, Elicitation(Result), Worktree*. Non-blocking (stderr to user): SessionStart, SubagentStart, SessionEnd, PostCompact, CwdChanged, FileChanged. Stderr to Claude: PostToolUse(Failure). Ignored: Notification, PermissionRequest, StopFailure, InstructionsLoaded… | [H exit-2 table] V |
| other (incl. 1) | non-blocking error, action proceeds (unless `onFailure:"block"`) | [H] V |

Common JSON output: `continue` (false = stop everything), `stopReason`, `suppressOutput` (no effect), `systemMessage` (shown to user), `terminalSequence`, top-level `decision:"block"` + `reason`, `hookSpecificOutput{hookEventName,…}`. **`additionalContext`, `systemMessage`, `initialUserMessage` and plain stdout are each capped at 10 000 chars**; beyond → saved to a file, replaced by path + 2 000-char preview — [H "JSON output"] V.

**Design consequence**: a Go hook must never exit 2 by accident (an unrecovered Go panic terminates the process with exit status 2 — known Go runtime behaviour, also used by `log.Fatal`-free crash paths; I, not re-tested here), or it blocks the prompt / tool / stop. All hook paths should recover panics and exit 0.

## 4. Per-event contract

| Event | Specific stdin | Output that matters | Blocking | Default timeout | Relevant to us |
|---|---|---|---|---|---|
| **SessionStart** | `source`: `startup` \| `resume` \| `clear` \| `compact` \| `fork`; `model` (may be absent); `agent_type` | `hookSpecificOutput.additionalContext` ("added to Claude's context at the start of the conversation"), `systemMessage` (banner), `initialUserMessage` (`-p` only), `sessionTitle`, `reloadSkills`; `CLAUDE_ENV_FILE` | no (exit 2 → stderr to user) | 600 s | banner, guide text, compact replay |
| **UserPromptSubmit** | `prompt` (pasted content expanded) | `additionalContext`, `systemMessage`, `decision:"block"`+`reason`, `sessionTitle`, `suppressOriginalPrompt` | yes | **30 s** | recall injection, nudges |
| UserPromptExpansion | expanded slash command | as above | yes | — | no |
| **PreToolUse** | `tool_name`, `tool_input`, `tool_use_id` | `permissionDecision` `allow\|deny\|ask\|defer` (precedence deny > defer > ask > allow), `permissionDecisionReason` (deny → shown to Claude), `updatedInput`, `additionalContext` | yes | 600 s | AskUserQuestion gate |
| PermissionRequest | — | `decision.behavior` | n/a | — | no |
| **PostToolUse** | `tool_input`, `tool_response` | `decision:"block"`+`reason`, `additionalContext`, `updatedToolOutput` / `updatedMCPToolOutput` | no (stderr to Claude) | 600 s | possible: post-search telemetry |
| PostToolBatch | — | can stop loop | yes | — | no |
| **Stop** | `stop_hook_active`, `last_assistant_message`, `background_tasks`, `session_crons`; "Does not run if the stoppage occurred due to a user interrupt. API errors fire StopFailure." | `decision:"block"`+`reason` (forces continuation; 8-consecutive cap `CLAUDE_CODE_STOP_HOOK_BLOCK_CAP`) | yes | 600 s | **write-back**. Doc: "the transcript file isn't guaranteed to include the final message at Stop time on all versions" → use `last_assistant_message` |
| StopFailure | API error end | none | no | — | maybe: retain on error turns |
| **SubagentStop** | `agent_transcript_path` (nested `subagents/` file), `last_assistant_message`, `agent_id/type` | `decision:"block"` keeps subagent running | yes | 600 s | not used today |
| SubagentStart | — | context | no | — | no |
| **PreCompact** | `trigger` `manual\|auto`, `custom_instructions` | exit 2 blocks compaction; `systemMessage`/`continue` discarded | yes | 600 s | possible pre-compaction retain |
| **PostCompact** | `trigger`, `compact_summary` | none | no | 600 s | possible: replay (we use SessionStart[compact] instead) |
| **SessionEnd** | `reason` `clear\|resume\|logout\|prompt_input_exit\|other` | none (output discarded) | no | **1.5 s budget** (plugin timeouts don't raise it) | weak write-back point |
| **Notification** | `message`, `title?`, `notification_type` ∈ `permission_prompt, idle_prompt, auth_success, elicitation_*, agent_needs_input, agent_completed, quota_auto_resume_*, plugin_notification` | none | no | 600 s | no |
| InstructionsLoaded | CLAUDE.md / rules file loaded | none | no | — | possible: detect our CLAUDE.md block |
| CwdChanged / DirectoryAdded | new dir | — | no | — | possible: project re-resolution |
| ConfigChange | config file changed | can block | yes | — | possible: detect user editing our hooks |

All V from [H] except the "Relevant to us" column (design notes).

## 5. Transcript (`transcript_path`)

- Path: `~/.claude/projects/<cwd with every non-alphanumeric char → '->/<session_id>.jsonl` (V, e.g. `-home-user-Workspace-github-com-Barben360-ref-stack/88cf850f-….jsonl`; encoding rule documented in `$SRC/core/history.ts#L57-L64`, verified there against Claude Code 2.1.241). Subagent transcripts live in `<session_id>/subagents/agent-<id>.jsonl` (V, local listing) — `agent_transcript_path` in SubagentStop [H].
- The doc calls it "Path to conversation JSON" and gives no schema → the JSONL format is **undocumented/internal** (I: no schema page found; the npm plugin's reader is maintained by observation, issue numbers #3379, #3023, #4380).
- Record `type` values observed in 7 recent transcripts (V, `jq` count): `assistant`, `user`, `attachment` (subtypes `hook_success`, `hook_additional_context`, `hook_system_message`, `total_tokens_reminder`, `deferred_tools_record/delta`, `date`, `skill_listing`, `instructions`, `environment`, `session_context`, `mcp_instructions_delta`, `edited_text_file`, `auto_mode`, `task_reminder`, …), `system` (`stop_hook_summary`, `turn_duration`, `away_summary`), `api-request`, `api-request-blob`, `api-request-shape`, `last-prompt`, `mode`, `permission-mode`, `ai-title`, `custom-title`, `agent-name`, `queue-operation`, `file-history-snapshot/delta`, `cost-state`, `bridge-session`, `atis-latch`.
- `user`/`assistant` records: `message.content` is a string (typed prompt) or an array of blocks; **each assistant content block is its own record** (observed: 111 `tool_use`-only, 96 `thinking`-only, 64 `text`-only records); `user` arrays carry `tool_result` (111) or `text`. Flags: `isMeta` (7 records), `isSidechain` (false in main file now that subagents have separate files), `isCompactSummary` (none in the sample; documented by the plugin, `$SRC/core/transcript.ts#L100-L105`), plus `uuid`, `parentUuid`, `timestamp`, `cwd`, `sessionId`, `gitBranch`, `version`, `promptId`, `toolUseResult`, `permissionDecision`.
- Hook `additionalContext` is persisted as `attachment` / `hook_additional_context`, not inside the user message (V) — readers that keep only `user`/`assistant` never re-ingest injected memory.

## 6. Cross-check with our hooks (`~/.claude/hooks/memory-search.py`)

| Our usage | Line | Contract status |
|---|---|---|
| reads `hook_event_name`, `cwd`, `session_id`, `prompt`, `source`, `transcript_path`, `tool_name` from stdin | `#L672-L757` | all documented common/event fields (V) |
| UserPromptSubmit → `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":…}}` | `#L650-L667` | V; must stay < 10 000 chars (our cap `PROMPT_CHARS = 800` ×2) |
| SessionStart[`compact`] replays last recalled hits as `additionalContext` | `#L688-L691` | V (`source:"compact"` documented); settings register a `"matcher":"compact"` group |
| SessionStart wrapper runs plugin node hook with `timeout=28`, replaces `additionalContext`, keeps `systemMessage` | `#L836-L852` | V; registered timeout 35 s (default would be 600) |
| PreToolUse[`AskUserQuestion`] → `permissionDecision:"deny"` + `permissionDecisionReason` (shown to Claude), once per prompt | `#L740-L757` | V; `AskUserQuestion` is a tool name matcher |
| Transcript parsing: `type`, `isMeta`, `isSidechain`, `message.content` string/blocks, `tool_use.name/input`, `text` | `#L759-L833` | depends on the undocumented JSONL format (I risk) |
| Always exits 0, swallows exceptions | `#L1146-L1157` | correct w.r.t. exit-2 semantics |
| Timeouts in settings: UserPromptSubmit 25 s (ours) and 60 s (plugin), PreToolUse 10 s, SessionStart 35/10 s, Stop 60 s | `~/.claude/settings.json` | explicit values override the 30 s / 600 s defaults (I: doc says defaults are "lowered", implying an explicit `timeout` wins) |
| Plugin Stop hook reads `transcript_path`, ignores `last_assistant_message` | `$SRC/harness/hook-lifecycle.ts#L231-L239` | doc warns the final message may be missing at Stop; plugin compensates by re-reading the whole transcript at every Stop (next Stop picks up the late message) and `reviseLastTurn` in usage |
| Plugin SessionStart ignores `source` | `$SRC/harness/hook-lifecycle.ts#L206-L211` | runs bank check + deepen spawn on every resume/clear/compact too |

## Implications for hindsight-claude

- Exit code 2 is the one dangerous code: Go's default panic exit status is 2, so every hook entry point needs a top-level recover and an explicit `os.Exit(0)`; non-zero non-2 codes are harmless.
- UserPromptSubmit has a 30 s default and sits on the user's critical path; SessionStart and Stop default to 600 s; SessionEnd is effectively unusable for network work (1.5 s, not extendable from a plugin). Stop (fires every turn) remains the write-back point, with `last_assistant_message` as the documented way to get the final reply.
- `additionalContext` is the CLAUDE.md-equivalent channel (SessionStart, re-fired with `source: compact|clear|resume`), capped at 10 000 chars per field.
- One hooks.json cannot vary by OS: on Windows the same command string runs in Git Bash or PowerShell depending on the machine; exec form (`command`+`args`) with a real `.exe` avoids shell differences entirely and also avoids quoting of `${CLAUDE_PLUGIN_ROOT}`.
- The transcript JSONL is undocumented and evolving (subagents moved to separate files; one record per content block; many new record types); parsing must be whitelist-based (`user`/`assistant`, known block types) and fixture-tested against real transcripts from several Claude Code versions.
- All hook input fields our Python hooks rely on are documented; the parts at risk are transcript parsing (AskUserQuestion gate, outcome telemetry, write-back).
