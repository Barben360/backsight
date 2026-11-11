# 0042 — The binary backs up and edits Claude Code's configuration itself

- **Date**: 2026-10-24
- **Status**: accepted
- **Decided by**: author (asked 2026-10-24: "make the Golang executable able to backup and edit the configuration itself with our different required edits"); design agent

## Decision
One edit engine (`internal/claudecfg`) performs every change backsight makes to Claude Code's own files. No hand edit, no `jq`, no instructions asking the user or an agent to edit JSON.

| File | Edits backsight owns |
|---|---|
| `~/.claude/settings.json` | `permissions.allow` rule for the three memory tools (§5); `cleanupPeriodDays` (0041); retiring reference hook groups and the reference npm plugin's `enabledPlugins` entry (§11.5) |
| `~/.claude.json` | retiring reference user-scope `mcpServers` entries (§11.5) |
| `~/.claude/CLAUDE.md` | the optional marked block (`setup --claude-md`); retiring a reference memory section |

- **Declarative, idempotent operations**: `set(path, value)`, `ensure-in-array(path, value)`, `remove-where(path, match)`, `marked-block(id, text)`. Applying the same plan twice changes nothing the second time.
- **Minimal in-place edits**: JSON is patched at the byte level (key order, indentation, unknown keys and other tools' entries preserved), never decoded and re-encoded whole. The result must parse as JSON before it is written.
- **Backup first**: before the first write of a run, each touched file is copied to `<state>/pre-images/<ts>/` (0600), journaled; the last 20 runs are kept.
- **Concurrency**: write = temp + fsync + rename only if the file's hash is unchanged since it was read; Claude Code rewrites `~/.claude.json` often, so on a mismatch the engine re-reads and re-applies (operations are idempotent), up to 3 times, then exits 20.
- **Preview and undo**: `--plan` lists each operation with the JSON path and the old and new value of that path only (never a whole-file diff: `~/.claude.json` holds account tokens, which are never printed). `client config restore [<ts>]` and `setup --undo` restore pre-images, refusing a file changed since unless `--force`.
- **CLAUDE.md**: backsight's block sits between markers. A reference memory section without markers is removed only when its text matches a known reference text (hash), otherwise the plan shows it and removal needs `--answer claude_md_section=remove`.
- **Out of scope**: plugin and marketplace registration stay with `claude plugin …` (0012, E1: Claude Code owns those files); project-level and managed settings are read, never written; nothing else in `~/.claude.json` is touched.
- Exposed as `client config plan|apply|restore`, and used by `setup`, `client install`, `client retention`, `uninstall`.

## Why
The reference install needed hand edits with ad-hoc `.bak` copies (2026-10-23 alignment); making them deterministic, previewable and undoable is the product's point (0004, 0005). Byte-level patching keeps diffs reviewable for users who version `~/.claude` in git.
