# 0067 — M5 in five blocks: restore first, then update, uninstall, docs, and the author's machine last

- **Date**: 2026-10-25
- **Status**: accepted
- **Decided by**: author (2026-10-25: "Plan + blocks 1-4", "Hold except fixes"), on the agent's proposal; the details below are the agent's, open to veto

## Decision

### Blocks (each closed by its own reviews, then the M5 review)
1. **Restore** (§11.8, 0044). Update rollback depends on it, and so does stopping the reference server.
   - `restore <file> [--plan] [--identity F]`:
     - takes the maintenance lock and backs up the current server first (the safety archive);
     - stops the server, restores the database and starts it again. An archive from an older image migrates; one from a newer image than the pinned one is refused;
     - verifies: recall smoke, and counts compared with the manifest;
     - writes a new restore epoch, then unlocks.
     - Any failure restores the safety archive (exit 30).
   - **Clients reconcile** (0044): the epoch is read at `jobs tick` and before any flush. On a mismatch the client drops the cursors and sent-index entries newer than `backup_created_at` and resumes from the server's document metadata. doctor reports a client still on an old epoch.
   - **Proof**:
     - CI: the `fresh-install` chain gains backup → store → restore → reconcile.
     - This machine: the latest real archive of the live install is restored into a scratch stack. The scratch stack has its own compose project, ports, `HOME` and Claude Code profile. It passes verify, and the scratch client re-sends what the backup lacks. The live server and `~/.claude` are not touched.
2. **Update** (§11.7, 0017, 0027).
   - **Release source**: GitHub releases of `Barben360/backsight`. While the repository is private (0063 Q2) it reads `GH_TOKEN` or `GITHUB_TOKEN` when set. `--source <dir>` reads a local release directory (goreleaser's `dist/`). An unreachable source makes `update check` exit 20; `jobs maintain` records the failure and doctor shows it as info.
   - **`update check [--json]`** returns the versions and changelog entries newer than the installed one. `jobs maintain` runs it daily, and SessionStart's banner shows "update x.y available (summary)". `changelog [--since v]` prints the same entries.
   - **`update apply [--plan] [--version v]`** follows §11.7 step by step:
     - `checksums.txt` is verified against its minisign signature with the current and next embedded keys, then the archive's hash;
     - the binary goes to `<data>/bin/<v>/` and the shim switches;
     - numbered, idempotent client migrations run, each recorded in state;
     - the plugin is regenerated and `claude plugin update` runs;
     - if the release pins a new digest, `server upgrade` runs: lock, backup and verify, pull, up, health, smoke.
     - Any failure switches the shim back, returns the previous digest, restores the safety archive (block 1) and exits 30.
     - Auto-apply stays off by default (`update.auto = false`).
   - **Self-verification** (§11.1): on its first run, a release binary checks its own hash against the signed `checksums.txt` kept next to it. A mismatch is a doctor error, and hooks keep running (a session is never broken). A dev build without checksums shows as info.
   - **Test signing key**: CI tests update end to end with a test key that only test builds embed (`-ldflags`). The release pipeline fails if a release binary reports any key besides the real current and next ones (`version --json`).
   - **Proof (CI)**: an update from the previous build to the new one, plus a simulated failed update (a failing migration and a failing server smoke), each leaving the previous install working.
3. **Uninstall, report, SECURITY.md, compatibility matrix** (§11.9, §15, §16).
   - `uninstall [--plan] [--stop | --delete-data] [--purge-local]` covers everything §11.9 lists, through the config editor. The retention value is offered back (0041). `--delete-data` needs the bank id typed, or `--answer confirm_bank=<id>` for an agent. Backups are never deleted.
   - `report [--json]`: a redacted diagnostic (versions, doctor JSON, counts, never memory text) and a pre-filled issue URL. It sends nothing; the user opens the URL (0002).
   - `SECURITY.md`: how to report a vulnerability, a threat-model summary (§13, 0046), the signing keys and their rotation.
   - `docs/compatibility.md`: per release, the pinned Hindsight digest, the minimum Claude Code version tested, Docker and Compose minima, and OS/arch status (Linux tested; macOS and Windows built, validated at M7). A CI check fails when the pin the binary reports (`version --json` `hindsight_image`, the same value `server render` uses) differs from the matrix.
   - **CI chain** (§16): fresh install → setup → verify → retain → recall → backup → restore → update from the previous build → undo → uninstall.
4. **Guide and the deferred doctor checks** (0066).
   - `guide [--json]` prints the user documentation embedded in the binary. `docs/` holds it: install, daily use, commands, privacy, backups and restore, update, uninstall, troubleshooting. The agent guide (`docs/agent-guide.md`) is kept in line with it (OPEN D17).
   - doctor:
     - **One project name seen with two remotes** (0045): warn, with the fix (`project` in `.backsight.toml`).
     - **Claude Code config drift, or a pre-image restore pending** (§15): a file that backsight edited no longer holds its edit, or a restore was journaled and never finished; warn, with the fix (`client install` or `client config restore`).
     - **The reference server can be stopped** (§11.5): shown as info only when a reference server is still running and a `backup verify` of backsight's newest archive passed within 7 days. The fix line gives the stop command and says the data is kept.
5. **The author's machine**: the author gives a go for each step, asked at the time.
   - Stop the reference server, after a final verified `pg_dump`. Its volume and dumps are kept, and deleting them is the author's later call.
   - Disable the `hindsight-local-backup` and `hindsight-offsite-backup` timers, and check the `claude-proton-backup` job against what it still needs.
   - Remove the reference client files left on disk (`memory-retro.py` and its disabled timer unit). Copies are kept in `~/.local/state/backsight/cutover-backup-20261025/`.
   - Install the M5 build into `~/.claude` after the parity window (below).

### The live install during the parity window
No new backsight build goes into `~/.claude` before 2026-11-08T11:34Z, except a fix for a bug that hurts daily use, with the author's go. The window then measures a single build (0065). Blocks 1 to 4 are proven in CI, the sandbox and scratch stacks.

### E24 waived
0066 runs E24 only if 50 prompts typed before the cut-over exist. On 2026-10-25, before 11:34Z, the transcripts hold 6 prompts of at least 60 characters (12 in all), so E24 is waived. Parity telemetry (§1.2, 0065) is the read-side gate.

### Exit (ROADMAP M5)
- A real backup restores into a fresh stack, passes verify, and its clients reconcile.
- A simulated failed update rolls back.
- After that comes the preview to 3–5 outside Linux users (0048). While the repository is private, who gets access is the author's call, asked at the end of M5.

## Why
Restore comes first because update rollback and stopping the reference server both rely on it. Uninstall and the CI chain come after update, so the chain runs update from a real previous build. The guide comes last, so it describes the commands as built. The author's machine is touched only in block 5, after the parity window, so the verdict is not mixed with lifecycle changes.
