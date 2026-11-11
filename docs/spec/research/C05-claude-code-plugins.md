# C05 — Claude Code plugin system: can one plugin carry hindsight-claude?

Date: 2026-10-23. Local Claude Code: **2.1.309** (`claude --version`). Docs read the same day.

Sources (all VERIFIED by reading unless marked INFERRED):
- [M] Manifest reference — https://code.claude.com/docs/en/plugins-reference (redirects to `/plugins/manifest-reference`)
- [K] Marketplace reference — https://code.claude.com/docs/en/plugins/marketplace-reference
- [L] Plugin loading reference — https://code.claude.com/docs/en/plugins/loading
- [C] Plugin components — https://code.claude.com/docs/en/plugins/components
- [CLI] Plugin commands reference — https://code.claude.com/docs/en/plugins/cli-reference
- [MCP] MCP docs — https://code.claude.com/docs/en/mcp
- [H] Hooks reference — https://code.claude.com/docs/en/hooks

## 1. What a plugin can ship

| Component | Default location | Ship it? | Notes |
|---|---|---|---|
| Manifest | `.claude-plugin/plugin.json` | optional | only `name` required [M "Fields"] |
| Skills | `skills/<name>/SKILL.md` | yes | namespaced `/<plugin>:<skill>`; `skills` key ADDS dirs [C "Skills"] |
| Commands (legacy) | `commands/*.md` or inline `commands` map | yes | "Prefer `skills/` for new plugins" [M "Standard layout"] |
| Agents | `agents/*.md` | yes | frontmatter `hooks`, `mcpServers`, `permissionMode` ignored [C "Agents"] |
| Hooks | `hooks/hooks.json` (+ `hooks` key, merged) | yes | same shape as settings `hooks`, wrapped in top-level `"hooks"` [C "Hooks"] |
| MCP servers | `.mcp.json` (+ `mcpServers` key; also `.mcpb`/`.dxt` bundles, local or https) | yes | started automatically when enabled [MCP "Plugin-provided MCP servers"] |
| LSP servers | `.lsp.json` | yes | binary must be on user PATH [C "LSP servers"] |
| Output styles | `output-styles/*.md` | yes | appear in `/output-style` as `<plugin>:<name>`; user must select one [C "Themes and output styles"] |
| Themes | `themes/*.json` (`experimental.themes`) | yes | — |
| Monitors | `monitors/monitors.json` (`experimental.monitors`) | yes | background shell process per session, interactive only, output → notifications [C "Monitors"] |
| Workflows | `workflows/*.js` | yes | — |
| **Executables** | `bin/` | yes | "Files in `bin/` … are on the `PATH` of the Bash tool's shell while the plugin is enabled"; appended AFTER user PATH (cannot shadow); not exported to hooks/MCP; claude.ai/Cowork refuse plugins with top-level `bin/` [C "Executables"] |
| Default settings | `settings.json` / `settings` key | very limited | "Only `agent` and `subagentStatusLine` take effect; other keys are dropped" [M "settings"] — **no permissions, no env, no statusLine** |
| userConfig | manifest | yes | prompts user; `sensitive` → secure storage [M "User configuration"] |
| Channels | manifest | yes | chat bridges bound to an MCP server |
| `CLAUDE.md` | plugin root | **NO** | "A `CLAUDE.md` at the plugin root isn't loaded as context, and `claude plugin validate` warns" [M "Standard layout"] |
| Mods / hooks modules | `modules` key in `hooks/hooks.json` | yes | JS functions running inside Claude Code that can draw UI [C "Hooks"] — not relevant to a Go binary |
| Dependencies | `dependencies` | yes | other plugins [M "dependencies"] |

## 2. Manifest essentials

- `name`: kebab-case, no `@ : /`. **Reserved-name check** [M "name"]: starting with `claude-`/`anthropic-`/`cc-plugin-` → error; `claude-code` → error; "Has `claude`, `anthropic`, or `anthropics` as a whole word anywhere else, such as `mcp-for-claude`" → **warning** `Plugin name "<name>" reads as one of Anthropic's own`. `hindsight-claude` therefore draws a validator WARNING (not an error; still installs) — relevant to C2/D2 (INFERRED that `-` counts as a word boundary, by analogy with `mcp-for-claude`).
- `version`: free string; "Setting it pins the plugin to that version until you change it" [M "version"].
- `userConfig` option fields: `type` (string/number/boolean/directory/file), `title`, `description`, `required`, `default`, `options`, `multiple`, `sensitive`, `min`/`max`. Non-sensitive values → `pluginConfigs` in user `settings.json`; sensitive → "the platform's secure credential store" [M "Where values are stored"]. Referenced as `${user_config.KEY}` in MCP/LSP config, exec-form hook `args`, skill/agent content (non-sensitive only); exported to hooks as `CLAUDE_PLUGIN_OPTION_<KEY>`. Shell-form hook commands and monitors **reject** `${user_config.*}` [M "Fields that run through a shell"].
- Setting values non-interactively: `claude plugin install … --config KEY=VALUE`, or `claude plugin configure <id> --values-stdin` (JSON on stdin); `claude plugin install` never prompts [C "When the configuration dialog appears"], [CLI "plugin configure"].
- Unknown top-level keys stripped (warning); unknown keys inside strict objects (userConfig, lspServers, monitors, channels) → plugin fails to load [M "Unrecognized fields"].
- Validation: `claude plugin validate [--strict] <dir>` [M "Validate the manifest"].

## 3. Path variables

| Variable | Value | Where substituted | Exported to process |
|---|---|---|---|
| `${CLAUDE_PLUGIN_ROOT}` | version dir `~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/` — changes on every update | hook `command`/`args`, MCP stdio `command`/`args`/`env`, LSP, monitors, skill/agent/command markdown | hooks, MCP stdio, LSP |
| `${CLAUDE_PLUGIN_DATA}` | `~/.claude/plugins/data/<id>/` (id = `name@marketplace`, non `[A-Za-z0-9_-]` → `-`), survives updates, deleted on last-scope uninstall unless `--keep-data` | same | hooks, MCP stdio, LSP |
| `${CLAUDE_PROJECT_DIR}` | project root | same | hooks, LSP |

Not present in Bash-tool commands [M "Where each variable resolves"]. On Windows substituted paths use forward slashes [M "Quoting and path separators"].

## 4. Hooks and MCP inside a plugin

- Hooks: registered when the session loads the plugin; fire on their events regardless of skill use [C "When plugin hooks fire"]. Run in parallel with settings hooks for the same event; "If you define the same handler in more than one settings file, it runs once. A plugin's or skill's copy of the same handler stays separate" [H]. Exec form (`command` + `args`, no shell) recommended for paths; on Windows exec form needs a real `.exe` [H "Windows shell"].
- **SessionEnd caveat**: "Timeouts set on plugin-provided hooks don't raise the budget" (default 1.5 s) [H "SessionEnd"].
- Two enabled plugins with the same manifest name → only one registers hooks [L "Hooks when two enabled plugins share a name"].
- MCP: server shown as `plugin:<plugin>:<server>`; **tool names become `mcp__plugin_<plugin>_<server>__<tool>`** [C "Server names, tool names, and reloads"] — e.g. `mcp__plugin_hindsight-claude_memory__memory_search`, versus today's `mcp__user-memory__memory_search` and `mcp__hindsight__hindsight_ingest_document`. Permission rules, hook matchers, CLAUDE.md text and our telemetry regexes key on these names.
- Plugin servers can't be added/removed with `/mcp` but can be toggled off there [MCP].
- `alwaysLoad: true` "is available on all server types" (exempts from tool-search deferral, startup waits ≤5 s); a server can also mark individual tools upfront with `_meta["anthropic/alwaysLoad"]`, honoured for plugin servers (v2.1.285+) [MCP "Exempt a server from deferral", "Mark a tool to load upfront"]. Our `user-memory` server uses `alwaysLoad: true` today (V, `~/.claude.json`).
- Server instructions/descriptions cut at 4 096 chars (tool search) [MCP]. Output warning >10k tokens, cap 25k (`MAX_MCP_OUTPUT_TOKENS`) [MCP].
- Reload: `/reload-plugins` reconnects changed servers; mid-session update keeps old paths until reload [L "When auto-update runs"].

## 5. Per-OS native binaries — options

The manifest has **no per-OS/arch field** (no such key in [M "Fields"], none in [K]). Options found:

| Option | How | Pros | Cons |
|---|---|---|---|
| A. Ship all binaries in the plugin + launcher | hooks call a launcher in `${CLAUDE_PLUGIN_ROOT}` that picks `bin/<os>-<arch>/…` | pure marketplace flow, deterministic, works offline | ~4 × 10–20 MB per version in the git repo / cache; launcher needs a shell: `sh`/Git Bash on Windows works, PowerShell-only Windows needs `shell:"powershell"` variant (hooks can't vary by OS in one file) (I); exec form on Windows needs `.exe` [H] |
| B. **`command` plugin source** | marketplace entry `{"source":"command","command":"hindsight-claude claude-plugin-path"}`; "Claude Code runs the command when the user installs or updates the plugin, and again once per session"; prints one absolute dir; `copy` mode hashes files into the cache, `link` mode loads in place (not on Windows) [K "command plugin source"], [L "When a command source re-runs"] | the already-installed native binary GENERATES the plugin (hooks/MCP pointing at its own absolute path, skills, guide text) → per-OS handled by the binary itself; version = hash of output; changes picked up next session and auto-reloaded | requires the binary installed first (bootstrap outside Claude); install shows `Run this command now? [y/N]`; non-TTY needs `-y`/`--accept-command <sha256>`, and `-y` "Ignored when the command runs inside a Claude Code session, such as from the Bash tool or a hook" [CLI "plugin install"]; admins can disable (`disableCommandPluginSources`); requires v2.1.229+; ≤500 ASCII chars command, timeout 1–600 s; refused if printed dir is the session dir or a parent |
| C. Bootstrap download in a SessionStart hook | hook downloads the right binary into `${CLAUDE_PLUGIN_DATA}` (doc pattern for `npm install` [C "Install dependencies into the data directory"]) | small plugin | first session non-deterministic, network at hook time, signature check needed; still needs a cross-OS bootstrap script |
| D. npm source | `source: npm` | — | install scripts never run [K "npm plugin source"] → no postinstall download; Node deps only |
| E. `.mcpb` bundle | `mcpServers: "./x.mcpb"` or https URL | MCPB supports per-platform binaries (I — MCPB spec, not checked) | covers only the MCP server, not hooks |

## 6. Marketplaces, install, update, pinning

- Marketplace = `.claude-plugin/marketplace.json` (`name`, `owner`, `plugins[]`) at a repo root [K "Marketplace file"]. Hosting: GitHub repo (`claude plugin marketplace add owner/repo[@ref]`), any git URL, direct URL to the JSON (relative plugin paths then impossible), local file/dir, inline `settings` source [K "Marketplace sources"]. The product repo itself can be its own marketplace (relative path source `"./"`) (I, standard pattern).
- Plugin sources: relative path, `github` (`repo`,`ref`,`sha`), `url` (git), `git-subdir`, `npm`, `archive` (https zip + `sha256` pin, v2.1.224+), `command` [K "Plugin sources"].
- Install (scriptable): `claude plugin marketplace add Barben360/hindsight-claude` then `claude plugin install <plugin>@<marketplace> [--scope user|project|local] [--config K=V]`; exit 0/1; `--json` output [CLI]. Interactive: `/plugin`. Enable/disable: `claude plugin enable|disable` → `enabledPlugins` in the chosen scope's settings [L "Find where a plugin is enabled"].
- Versioning: computed version = manifest `version` > entry `version` > git SHA (12) / archive sha256 / command-output hash [L "How Claude Code computes the version"]. Pinning: marketplace entry `ref`+`sha`, or a fixed `version` string.
- Auto-update: background, random delay ≤10 min after first message; **default ON only for Anthropic official marketplaces, OFF "for every other marketplace"**; per-marketplace `autoUpdate` in `extraKnownMarketplaces` or the `/plugin` toggle; `DISABLE_AUTOUPDATER` etc. turn it off; new version applies after `/reload-plugins` or next launch [L "When auto-update runs"]. Manual: `claude plugin update <id>` (needs `-y` for a changed command source when non-TTY).
- Old versions kept 14 days (`.orphaned_at`) [L "Cleanup of previous versions"].
- Cache layout: `~/.claude/plugins/{cache,data,marketplaces,installed_plugins.json,known_marketplaces.json}` (`CLAUDE_CODE_PLUGIN_CACHE_DIR` overrides) [L "Find plugins on disk"]. Copied plugins can't reference files above their root.
- Uninstall: `claude plugin uninstall <id> [--keep-data]` deletes `${CLAUDE_PLUGIN_DATA}` by default [CLI].
- Project-scope plugins (committed `.claude/settings.json`) go through the workspace trust check and per-server MCP approval [L "Trust check and component limits"]; user-scope plugins don't.
- Precedent: Vectorize previously shipped a Claude Code plugin `hindsight-memory@<marketplace>` (detected as legacy in `coding-agents/src/core/legacy.ts#L26`), and `vectorize-io/hindsight/hindsight-integrations/agent-plugin/` holds a plugin manifest + `mcp.json` + skills (V, `gh api` listing).

## 7. Can a plugin replace merging into `~/.claude/settings.json` and `~/.claude.json`?

| Need (current setup) | Plugin can do it? | Evidence |
|---|---|---|
| Hooks: SessionStart, SessionStart[compact], UserPromptSubmit, PreToolUse[AskUserQuestion], Stop | **Yes** (`hooks/hooks.json`) | [C "Hooks"] |
| MCP servers `hindsight` + `user-memory` | **Yes**, but tool names change to `mcp__plugin_<plugin>_<server>__*` | [C] |
| `alwaysLoad: true` on the memory server | Yes | [MCP] |
| Skills (`memory-search`, `hindsight-retro`, `hindsight-coding-agent`) | Yes (namespaced `/<plugin>:<skill>`) | [C "Skills"] |
| Permission allow rule `mcp__user-memory__memory_search` (V, settings.json) | **No** — plugin `settings` only honours `agent`, `subagentStatusLine` | [M "settings"] |
| `statusLine`, `env`, model settings | No | same |
| API URL / token config | Partly: `userConfig` (token `sensitive` → secure store), passed to MCP `env` via `${user_config.KEY}` and to hooks via `CLAUDE_PLUGIN_OPTION_*`; the binary can equally keep its own config file | [M] |
| Persistent state dir | Yes `${CLAUDE_PLUGIN_DATA}` (deleted on uninstall) | [M] |
| CLI on Claude's PATH (`hindsight-claude doctor` from Bash tool) | Yes via `bin/` (Bash tool only, not hooks) | [C "Executables"] |
| CLAUDE.md "memory" section text | **Not as CLAUDE.md**; equivalents below | [M] |
| systemd/launchd timers, Docker stack | No (outside Claude Code's scope) | — |

Conclusion (facts): a plugin can carry hooks, MCP servers, skills, CLI-on-PATH and config prompts, i.e. everything currently merged into `settings.json.hooks` and `~/.claude.json.mcpServers`. It cannot add permission rules (so the MCP read tools would prompt unless the user/installer adds an allow rule, or the user accepts "always allow"), cannot set env/statusLine, and cannot install anything outside Claude Code (server, timers, backups).

## 8. Adding text equivalent to a CLAUDE.md section

| Mechanism | Always in context? | Limits | Source |
|---|---|---|---|
| SessionStart hook `additionalContext` | Yes, "added to Claude's context at the start of the conversation, before the first prompt"; re-fired on `source: compact`/`clear`/`resume` (one hook, matcher optional) | 10 000 chars per field, above → saved to file + 2 000-char preview | [H "SessionStart", "JSON output"] |
| UserPromptSubmit `additionalContext` | per prompt | 10 000 chars | [H] |
| Skill | Only description (always) + body on demand | description budget | [C "Skills"] |
| MCP server `instructions` | Loaded at session start even with tool search | 4 096 chars | [MCP "tool search"] |
| Output style | Replaces/extends system prompt, but user must pick it in `/output-style`; only one active | — | [C], https://code.claude.com/docs/en/output-styles |
| Plugin `settings.agent` | Replaces main-thread system prompt with a plugin agent | far too invasive | [C "Default settings"] |
| Plugin `CLAUDE.md` | not loaded | — | [M] |
| Writing a managed block into `~/.claude/CLAUDE.md` | yes | outside plugin system (settings-merge style) | — |

The npm plugin and our wrapper already use the first mechanism (SessionStart `additionalContext` = tool guide).

## Implications for hindsight-claude

- A Claude Code plugin can deliver all Claude-side integration (hooks, MCP, skills, guide text via SessionStart `additionalContext`, CLI on Claude's PATH, token prompt) without touching `~/.claude/settings.json` hooks or `~/.claude.json` — except permission allow rules, which plugins cannot set.
- Per-OS binaries have no first-class support; the `command` plugin source is the one mechanism where the native binary itself produces the plugin directory each session (deterministic, version = content hash), at the price of a one-time `[y/N]` acceptance (or `-y` from a real terminal, never from inside Claude) and a binary that must already be installed.
- Shipping binaries inside a git-hosted plugin works but multiplies repo/cache size per version and needs a cross-shell launcher (sh / Git Bash / PowerShell).
- Moving MCP into a plugin renames every tool (`mcp__plugin_<plugin>_<server>__<tool>`), which ripples into CLAUDE.md text, permission rules, hook matchers (AskUserQuestion gate's "searched?" check) and telemetry regexes; plugin and manual registrations would also coexist under different names during migration.
- Third-party marketplaces do not auto-update by default; the user (or our installer through `extraKnownMarketplaces.<name>.autoUpdate`) must opt in, otherwise updates need `claude plugin update`.
- `${CLAUDE_PLUGIN_DATA}` is deleted on uninstall by default — memory state, logs or backups must not live there.
- The name `hindsight-claude` will draw a `claude plugin validate` warning ("reads as one of Anthropic's own") — input for C2.
- A SessionEnd hook from a plugin is capped at the 1.5 s default budget (plugin timeouts don't raise it); Stop remains the reliable write-back point.
