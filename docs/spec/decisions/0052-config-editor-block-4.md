# 0052 — M1 block 4: the Claude Code config editor and `client retention`

- **Date**: 2026-10-24
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, filling what 0041 and 0042 leave open for M1

## Context
ROADMAP v8 puts the config editor (0042) and `client retention` (0041) in M1, but the three blocks of 0048 left them out. They become **block 4**, closed by its own review before the M1 review. In M1 the only edit backsight owns is `cleanupPeriodDays`: the permission rule arrives with the MCP tools (M2), retiring reference pieces with the cut-over (M4).

## Decision
- **Engine** `internal/claudecfg`, as 0042: `set`, `ensure-in-array`, `remove-where`, `marked-block`, all idempotent.
  - JSON is patched in place at the byte level: siblings' indentation is reused, and order, unknown keys and the trailing newline are kept.
  - Every patch is checked twice: the result must parse, and it must decode to exactly what the same operation gives on the decoded tree. A mismatch refuses the write.
  - Invalid JSON is refused (exit 20) and never overwritten. A missing or empty file counts as `{}`.
- **Desired state**: `config.toml [client] retention_days` (0 or absent = not managed). `client config plan` lists the owned edits whose current value differs; `apply` makes them.
- **`client retention [--days N | --keep] [--plan]`** needs no server and creates a minimal config if none exists.
  - Without `--days` the target is 3650, and a user value already ≥ 3650 is left alone: retention is never lowered unless `--days` says so.
  - `--keep` stops managing it: the key leaves the config and the file is not touched.
  - A managed setting is reported, and nothing is written.
  - Uses Claude Code's directory (`CLAUDE_CONFIG_DIR` honoured), or `--config-dir`.
- **Previous value**: the user value before backsight's first change (`null` if the key was absent) goes to `state.client.retention_before`, so uninstall can offer it back (0041).
- **One run = one pre-image directory** `<state>/pre-images/<UTC ts>/`:
  - `manifest.json` `{ts, command, files[{target, pre_image|null, sha_before, sha_after}]}`, plus a copy of each file as it was just before the write that succeeded (0600);
  - `null` means the file did not exist;
  - the last 20 directories are kept;
  - one journal line per written file `{ts, command, step: "claude-config", action: edit|restore, target, pre_image, status}`.
- **Writes**: temp + fsync + rename only if the hash is still the one read. On a mismatch the engine re-reads and re-applies, up to 3 attempts, then exits 20.
- **`client config restore [<ts>] [--force]`** restores the given run, or the last one.
  - A file whose current hash is not the run's `sha_after` is refused unless `--force`.
  - A `null` pre-image deletes the file.
  - The restore is itself a run with its own pre-images, so it can be undone.
- **Plan output**: per operation, the file, the JSON path, and the old and new values of that path only. Removed entries are named by key and their values pass through redaction. A whole file is never printed.
- `doctor`'s retention fix becomes `backsight client retention`.

## Why
Retention protects the migration source now, before anything else is installed. The other edits need components that do not exist yet. The double check (bytes vs tree) makes byte-level patching safe without a full JSON round-trip, which would reorder and reformat users' files.
