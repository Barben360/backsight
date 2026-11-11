# 0053 — Amendments from the end-of-M1 review

- **Date**: 2026-10-24
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, from four independent reviews of the M1 implementation against the spec (block-4 conformance, data safety, test quality, completeness and privacy)

## Decision
- **`--plan` scope (§11.2)**: required on commands that change configuration, user files or the server's setup: `server install|render|start|stop`, `bank apply`, `client config apply|restore`, `client retention`, `retain backfill`, later `setup`, `client install`, `uninstall`, `restore`, `update`. Data-plane sends (`store`, `retain flush`, `retain retry-quarantine`, `jobs tick`) have no plan: they send what was asked, idempotently, and `retain backfill --plan` and `doctor` are their previews.
- **`--non-interactive` and progress lines (§11.2)** land with `setup` in M4. Until then the only prompt is `secret set` on a terminal, which agents never have.
- **Exit codes**: 20 when the environment stops a command midway (port taken, health timeout, refused credentials), with the steps already done kept in the error envelope's data. 40 when part of the work is done and a plain rerun finishes it (sessions or notes to retry, a budget pause, a restore or retention that wrote part of its changes). Text mode always prints the reason on stderr.
- **Config editor runs**: only runs with a manifest count. A run that writes nothing leaves no directory. Pruning happens after a run, never during one. A non-forced restore writes only over the bytes its run left. Hash-checked writes re-check the hash after syncing the temp file, right before the rename.
- **Retention (0041)**: the user value is recorded before Claude Code's file is written. Project-level settings are not read: transcript cleanup is a user-level setting. This is unverified, and is checked against Claude Code's documentation before M4's pre-flight.
- **Redaction (§13)** also covers:
  - `PASS` as a key segment;
  - hyphenated `api-key` headers;
  - JSON escaped inside strings;
  - any `Authorization` scheme;
  - GitLab tokens.
- **Sandbox CI (§16)**: from M1, `scripts/sandbox.sh` renders the pinned stack with backsight itself, using Hindsight's `mock` LLM, and runs `internal/sandbox`. It is the reference way to test the client against a real server.
- **Known issue for M2**: after a lost cursor, while an async retain is still pending, segment 1 can replace a document (backsight issue 101). It must be fixed before the read side relies on complete documents.

## Why
The reviews found:
- one proven data-loss window: a concurrent write during the fsync;
- undo that a failed run could break;
- leaking secret shapes;
- an exit-code contract that grouped unrelated failures;
- tests that passed without the behaviour they guard.

The code fixes are backsight PRs 92, 94, 96, 98, 100 and 103. This decision records the spec side, so the contract matches what is tested.
