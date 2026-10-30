# Update

How backsight finds, checks and installs new releases.

## How you learn about a release

The daily job checks backsight's GitHub releases. When a newer one exists:

- the next Claude Code session shows one line, "⬆️ backsight update X available · see `backsight changelog`", with the version only;
- `backsight doctor` shows it in its `update` check.

`backsight update check` asks now and lists the newer releases with their notes. `backsight changelog` prints the notes of every release (`--since <version>` for the newer ones only). Release notes are not signed, so they are never shown inside a Claude session.

The checks carry no memory or usage data. GitHub limits anonymous requests; if you hit the limit, set `GH_TOKEN` (or `GITHUB_TOKEN`) in the environment.

## Pre-releases

backsight is a preview, so its releases are pre-releases. Pre-releases are offered when the installed version is a pre-release itself, or when `config.toml` opts in:

```toml
[update]
prereleases = true
```

A stable install is never moved to a pre-release otherwise.

## Installing an update

```sh
backsight update apply --plan
backsight update apply
```

`--plan` downloads and verifies the release into a temporary directory, lists the steps, the client migrations and the server change, and changes nothing.

`update apply`:

1. downloads the release archive with its `checksums.txt` and signature, and verifies both; if either fails, nothing changes (exit 20);
2. takes the maintenance lock, installs the new binary next to the old one, and switches the shim, so `backsight` on your PATH runs the new version;
3. runs the new version's client migrations, its plugin update and its scheduler refresh;
4. if the new release pins another Hindsight version, upgrades the server (below);
5. runs the new version's `bank apply`, which sends its bank template when it differs from the one the bank was given (a failure there is only a warning: run `backsight bank apply`). From v0.1.4-m5 it first renames a bank still called `claude-memory` to `backsight` (`backsight bank rename`); when the server is busy, the daily job does it after its backup;
6. indexes the new version's docs for `backsight guide search` (a failure there is only a warning);
7. records the update.

Then restart Claude Code (or run `/reload-plugins`) to load the new plugin.

After the upgrade that brings it, the maintenance run every 15 minutes (`jobs tick`) also adds earlier history to the per-memory counts: the memories shown and the clear credits that this machine's telemetry and the Claude Code transcripts still on disk recorded. It spends at most a minute per run and goes on at the next one until it is done; `backsight feedback import` does it at once.

`--version <v>` installs a given version instead of the newest. `--source <dir>` reads releases from a local directory instead of GitHub.

## Server upgrades

When a release pins a newer Hindsight server, `update apply` runs `backsight server upgrade` as the new binary. It:

1. pulls the new image, or on the [native runtime](native-runtime.md) installs the new version's virtual environment beside the running one, which keeps serving (a failure changes nothing);
2. makes a safety backup and verifies it in a throwaway database;
3. deploys and starts the new version (on native: points the server unit at the new environment and restarts it), waits for health and runs a recall.

Any failure from the deploy on puts the safety backup back into the previous version (on native, the server unit runs the previous environment again; the new one stays on disk) and exits 30. On native, a passed upgrade removes the environments older than the previous one. A server upgrade never downgrades. The pinned versions are listed in [compatibility.md](compatibility.md).

## When an update fails

- **Exit 20**: the download or its signature failed, or something blocks the update (a lock held, Docker stopped). Nothing changed.
- **Exit 30**: a step failed before the server upgrade passed. backsight put back its config and state, the server, the shim and the previous plugin: you are on the old version, as before.
- **Exit 40**: the update itself passed but recording it failed. Run `backsight update apply` again: it finishes the update.

If an update was interrupted (a crash, a reboot), `backsight doctor` shows an `update-incomplete` error with the command that resumes it, usually `backsight update apply`.

## Automatic updates

Updates are manual by default. To let the daily job install them:

```toml
[update]
auto_apply = true
```

A release that fails to install is skipped until a newer one appears or you run `backsight update apply` yourself.
