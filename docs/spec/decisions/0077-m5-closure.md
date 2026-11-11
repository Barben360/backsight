# 0077 — M5 closes on restore, update, uninstall and the M5 review; the outside-user preview runs on its own

- **Date**: 2026-10-29
- **Status**: accepted
- **Decided by**: author (2026-10-29: "Yes." to closing M5 formally after M6), on the agent's proposal; the details below are the agent's, open to veto

## Decision
- **Built** (0067's five blocks; INDEX records each block as built). PR and issue numbers before 2026-10-26 refer to `Barben360/backsight-archive` (0068):
  1. Restore: `restore` with a safety archive, the restore epoch and client reconcile (0044); closed with three block reviews (archive PRs 199, 200, 202, 205, 206).
  2. Update: `update check|apply`, signed releases and self-verification, `server upgrade` with rollback, the server runtime interface (0070); closed with three block reviews (archive PRs 210, 211, 213, 215, 217, 220, 221).
  3. Uninstall, `report`, SECURITY.md, the compatibility matrix and the CI chain; closed 2026-10-25 (archive PRs 226–230, 236–239).
  4. `guide`, the deferred doctor checks and `verify --live`; closed 2026-10-26 (archive PRs 252, 253, 268, 269, 278).
  5. The author's machine: the parity verdict (0065, 2026-10-26), the old server decommissioned with a final encrypted copy, v0.1.0-m5 installed, the reference hooks and timers retired.
- **Exit (0067)**:
  - **A real backup restores into a fresh stack, passes verify, and its clients reconcile**: proven in CI (`fresh-install`: backup → store → restore → reconcile) and on a scratch stack from the live install's latest archive (block 1). The M6 bench re-proved it across two PCs (`restore-across-clients`, 0076).
  - **A simulated failed update rolls back**: CI, a failing migration and a failing server smoke each leave the previous install working (block 2).
- **M5 review**: cross-cutting review of backsight 9ecf1fb (2026-10-26); its rules are in INDEX ("M5 review rules"), fixed in archive issues 281–285 and in https://github.com/Barben360/backsight/pull/1 (restore and purges, four review rounds).
- **Public and released**: the repository is public since 2026-10-26 (0068); M5 shipped signed pre-releases v0.1.0-m5 to v0.1.7-m5 with `install.sh` and the agent guide.
- **Moved on**:
  - E24, waived (0067);
  - E29's result: the native runtime is M7's, under E29's 8 conditions (0070);
  - Windows scheduler entries and a minimum version in `install.sh` stay M7's (0066).
- **Pending, the author's**: the preview to 3–5 outside Linux users (0048). 0067 put it after the exit; the repository is public, so it needs no access grant, only the invitations. The author's work PC is the first preview install (0068).

## Why
0067's exit is met and every block was closed by its own reviews, then the M5 review. ROADMAP still read "started" only because no record closed it; M6 was built on M5's lifecycle (restore epochs, tombstones, signed updates) and proved it again on two PCs.
