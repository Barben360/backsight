# 0076 — M6 closes on the VM bench, the CI chain and the M6 review; the TLS proxy and the carried findings move to M7

- **Date**: 2026-10-29
- **Status**: accepted (agent, applying 0073's exit; the author delegated M6's completion on 2026-10-28; open to veto)
- **Decided by**: agent

## Decision
- **Built** (0073's five blocks; INDEX records each block as built):
  1. Server side: `server expose`, `server token [--rotate]`, the server-only role (`setup --role server`). https://github.com/Barben360/backsight/pull/73
  2. Client side: `connect` and `connect --undo`, joining from an install with its own server, version skew, reachability (`server-reach`, the banner, the `server_id` hold). https://github.com/Barben360/backsight/pull/77
  3. `server install --ssh` and `--print-commands`. https://github.com/Barben360/backsight/pull/82
  4. The libvirt VM bench (`scripts/vm-bench.sh`, 19 checks) and the CI container chain (`m6-chain`), with the product changes they brought (0075). https://github.com/Barben360/backsight/pull/84
  5. Docs (`docs/several-pcs.md`, the agent guide's remote section, troubleshooting rows for every doctor check), the spec as built, the M6 review and its fixes, the v0.1.13-m6 notes. https://github.com/Barben360/backsight/pull/96, released as https://github.com/Barben360/backsight/releases/tag/v0.1.13-m6
- **Exit (0073)**, clause by clause:
  - **Two clients on one backsight server**: bench `two-clients` (both doctors clean, `server-id` ok), and `m6-chain` on every PR and on main.
  - **A session never retained twice**: bench `dedup-copied`, `dedup-concurrent` (both PCs sent the same 5 sessions at once; each on the server once) and `dedup-split`; `m6-chain` re-runs the three. The exception the author accepted (2026-10-27, "never lose" over "never store twice"): a session continued on two PCs may store the shared part twice, and doctor names it (`split-session`). After a host restore, the PC that did not write last re-sends that session whole.
  - **A second PC joined with one command and no hand edit**: bench `ssh-install` (resumed after a dropped link, exit 0) and `two-clients` (B joined with `connect`).
  - **Proven on the VM bench and the CI chain**: bench run results/20261029T171531Z on commit f7fd84c, 18.5 min, all 19 checks pass, `unsent-cost` recorded (+64 ms for a tick with 200 uncursored transcripts); prompt-hook p95 1233, 1284 and 1434 ms at 0, 20 and 80 ms of added delay, no hook past its deadline. CI green on f7fd84c and on the merge commit, `m6-chain` and `fresh-install` included.
  - **The M6 review addressed**: below.
- **M6 review**: three independent reviewers on backsight 3c2c04d (conformance; safety and integration; test quality). 0 Critical, 7 Important, 28 Minor, 4 Info. All 7 Important were fixed in PR 96. Two of them were loss paths ("never lose" rules): a restore dropped a pending first-segment repair, and a fresh host restored from an older archive kept an epoch the clients already held. Minors were fixed where they touched safety or text a user reads. A final whole-branch review found 1 Important (a pending mark's 24 h window after a restore under the same epoch number), fixed before the merge. Rulings: `plans/2026-10-29-m6-block5-followups.md`.
- **Moved to M7**:
  - the TLS proxy and a managed ssh tunnel (0073; transport stays as §11.6, with Tailscale recommended);
  - macOS and Windows clients, validated with the native runtime (ROADMAP M7);
  - `dropSession` with the whole-line match on split documents (duplicates after a restore, not loss);
  - a first segment whose document is absent is reported after 24 h, not re-sent; a given-up part has no re-send command;
  - a mark that could never be checked is given up after 24 h;
  - `split-sessions.json` is never pruned;
  - the 300-byte unredacted server replies in `shared_error`, `feedback_error` and the doctor snapshot (no key or token path found);
  - connect's consent `NeedsUser` omits the ack question; `--resume` after an update reruns done steps;
  - bench: the fake-driver positive controls, a dropped ssh connection at the token or handback step, a changed host key on the command;
  - the deferred script minors of block 4 (`plans/2026-10-29-m6-block4-followups.md`).
- **Pending, the author's**: none for M6. The M5 outside-user preview (0048) runs on its own (0077).

## Why
0073's exit is met on both proofs, the VM bench on the released commit and the CI chain on every PR, and the M6 review's Important findings are fixed rather than accepted. What is carried is either M7's scope (transport, other OSes) or a residual whose outcome is reported, not silent: duplicates, a doctor line, a stale file. Holding M6 open for them would not change what a user sharing one memory between PCs gets.
