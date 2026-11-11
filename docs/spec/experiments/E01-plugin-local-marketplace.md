# E1 — Delivering the Claude Code side as a plugin from a binary-generated local marketplace

Date: 2026-10-23. Claude Code 2.1.309. Isolated config (`CLAUDE_CONFIG_DIR` in the scratchpad, credentials copied), so the author's own config was untouched. **V** = observed.

## Setup
A directory `mkt/` with `.claude-plugin/marketplace.json` (`{"name":"hc-local","plugins":[{"name":"hc-memory","source":"./plugins/hc-memory"}]}`) and a plugin whose `hooks/hooks.json` and `.mcp.json` call a fake binary **by absolute path in exec form** (`"command": "/abs/path/fakebin", "args": ["hook","session-start"]`). Hooks: SessionStart, UserPromptSubmit, PreToolUse[AskUserQuestion], PostToolUse[`mcp__plugin_hc-memory_memory__.*`], Stop, SessionEnd, PostCompact, SubagentStart, SubagentStop. One stdio MCP server `memory` (`alwaysLoad: true`), one skill.

Commands: `claude plugin marketplace add <dir>` → `claude plugin install hc-memory@hc-local` → `claude -p … --output-format stream-json --verbose`.

## Results
| Check | Result |
|---|---|
| Install from a local directory marketplace | **V** works, no prompt, no `command` source, no y/N. `claude plugin validate` only warns about missing `author` and icon. |
| Hooks in exec form with an absolute path | **V** all fired: SessionStart, UserPromptSubmit, Stop, SessionEnd, SubagentStart, SubagentStop, PostToolUse. |
| SessionStart `additionalContext` reaches the model | **V** (model quoted the marker). `systemMessage` banner accepted. |
| UserPromptSubmit `additionalContext` reaches the model | **V** |
| MCP server from the plugin | **V** `plugin:hc-memory:memory` connected; tool name `mcp__plugin_hc-memory_memory__memory_search`; callable with `--allowedTools` on that name. |
| Skill namespacing | **V** `hc-memory:memory` |
| **SubagentStart `additionalContext` reaches the subagent** | **V** the subagent answered the codename injected only there. |
| PostToolUse matcher on the plugin MCP tool name (regex) | **V** fired after `memory_search` → a "searched" flag can be written by a hook, no transcript parsing needed for the AskUserQuestion gate. |
| Update | **V** bump `version` in plugin.json → `claude plugin marketplace update hc-local` → `claude plugin update hc-memory@hc-local` → "updated from 0.0.1 to 0.0.2 … Restart to apply". |
| Subagent completion notice | **V** arrives as a new UserPromptSubmit (machine message) → the machine-message filter stays necessary. |

## Consequences for the spec
- The binary can own the whole Claude Code integration as a plugin it generates (OS-specific absolute paths, versions in lockstep with the binary) with zero interactive confirmation.
- Settings merge is still needed for exactly one thing: a permission allow rule for the read-only memory tools (plugins cannot set permissions, C05).
- Tool names are long but stable: `mcp__plugin_<plugin>_<server>__<tool>`. Choose short plugin/server names (e.g. plugin `memory`… see D2) so the names stay readable.
- Not tested: Windows path forms, `/reload-plugins` mid-session, behaviour when the binary path disappears (to test in M1).
