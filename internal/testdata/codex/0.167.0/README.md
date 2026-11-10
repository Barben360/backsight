# Codex 0.167.0 fixtures

Recorded by `scripts/codex/run.sh` (the harness, see its README) on 2026-11-10: a real Codex CLI 0.167.0 in a container, a fake model, a development build of backsight, a scratch memory server. The Codex blocks replay them (hooks, MCP identity, conversations). Session and turn ids are real and consistent across files; the install id, the host name and temporary directory names are scrubbed. To record again: `OUT=<dir> scripts/codex/run.sh`, then copy `<dir>/fixtures` here.

| Folder | What |
|---|---|
| `hooks/` | The payload Codex sent for each event, one per variant: `<Event>[.<source, trigger or reason>][.subagent][.<tool_name>].json`. Recorded by a user-level hook on every event (`scripts/codex/probe.sh`). |
| `backsight/` | backsight's own hooks under Codex: `<event>.in.json` (stdin), `.out.json` (stdout, when it answered), `.rc`. The generated plugin, adapted for Codex (`scripts/codex/codexify.sh`). Its SessionStart banner carries a drift line: the harness replaces the shim with a recording wrapper. |
| `mcp/` | backsight's MCP server traffic, both directions (`root.*` for the session, `subagent.*` for a subagent's own server process); `processes-default.txt` / `processes-shared.txt`: server processes for a session with one subagent, by default and with Codex's MCP sharing flags; `shared-process-calls.json`: the `_meta` of two threads' calls on one shared process. |
| `model/` | Whole model requests: the first of a turn (where hook context lands), a subagent's first, a compaction, a 43,200-character `additionalContext` (the spill), hook exit codes, a Stop block. |
| `rollouts/` | `root.jsonl` (startup, tools, an approval, a subagent, a compaction, then resumed), `subagent.jsonl`, `fork.jsonl`, `clear-new.jsonl` (the session `/clear` started), `compressed.jsonl.zst` (a rollout the compression flag replaced; `compression.txt`). |
| `env/` | Environment of a hook, of the MCP server (root and subagent) and of a shell command (root and subagent). |
| `approval/` | The approval prompt, and which MCP calls `codex exec` ran or denied: `exec-default` (workspace-write), `exec-fullaccess`, `exec-config` (with `config-with-approvals.toml`). Probe tools: `readonly`, `closed` (destructive and open-world hints false), `plain` (no hints), `destructive`. |
| `trust/` | The hook review screens and `config.toml` once all hooks were trusted (`[hooks.state]`). |
| `config/` | `config.toml` at the start and the end, `codex plugin list --json`, the install commands' output, `codex features list`, `codex --version`. |
| `memories/<v2\|v1\|dual>/` | Codex's own memory stores after three sessions (`scripts/codex/memories.sh`): every file Codex wrote (`<store>.listing.txt` has names and sizes, `.git` and `.codex-baseline` listed, not copied), the `$CODEX_HOME` entries, the state databases' tables. `rollout_summaries/*.md` headers and `raw_memories.md` are Codex's; the bodies (`fake …`), `MEMORY.md` and `memory_summary.md` were written by the fake model, in the shape Codex checks. |
| `session-root.txt`, `session-compressed.txt` | The ids of the main session and of the compressed one. |
