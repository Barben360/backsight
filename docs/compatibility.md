# Compatibility

What backsight runs on, and what each release pins.

## Current

What this source tree pins and requires. A release built from it ships exactly this, and `backsight version --json` reports the Hindsight image (`hindsight_image`). A Go test (`internal/cli/release_docs_test.go`) fails when the Hindsight, PostgreSQL and Claude Code rows differ from what the binary pins or requires.

| Component | Pinned or required | Enforced by |
|---|---|---|
| Hindsight server | `ghcr.io/vectorize-io/hindsight:1.1.0@sha256:50311d02b4d5d2b7c5242bd336edf8735df8374eaad496a92f49c83b37d2aa06` | the binary: `server render` writes this image; `server render`, `server start`, `server install` and setup refuse a deployed server on another pin (`server upgrade` and `update apply` move it) |
| PostgreSQL | `pgvector/pgvector:pg18@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11` | the binary: `server render` writes this image |
| Claude Code | `2.1.309` or newer | setup's pre-flight and `client install` refuse an older version |
| Docker | Docker Engine (Linux) or Docker Desktop, usable by your user without `sudo` (the `docker` group, or rootless Docker) | setup's pre-flight runs `docker info`; it checks no minimum version |
| Docker Compose | v2 or newer, the `docker compose` plugin (not the old `docker-compose` v1) | setup's pre-flight runs `docker compose version` and refuses v1 |

Both images are pinned by the digest of their multi-arch index, so the tag is only a label: a signed release fixes the digest, and Docker pulls exactly that image (no separate image signature).

Tested with Docker 29.9 and Compose 5.6 (as of 2026-10-25), and on every change with the Docker of GitHub's `ubuntu-latest` runners. CI installs the Claude Code version pinned in `.github/workflows/ci.yml`.

## Platforms

Release binaries are built for four targets. Linux amd64 is the only platform tested today; the other binaries are compiled on every change but not tested yet.

| OS / arch | Release binary | Status |
|---|---|---|
| Linux amd64 | yes | **tested**: Ubuntu 24.04 (CI, on every change, the fresh-install chain included) and CachyOS (rolling, Arch-based) |
| Linux arm64 | yes | built, not tested |
| macOS arm64 (Apple silicon) | yes | built, not tested |
| Windows amd64 | yes | built, not tested |

Other targets (macOS on Intel, Windows on arm64, 32-bit) have no release binary.

## Releases

One row per release, newest first, added with the release's notes in `docs/releases/<tag>.md`: the release workflow refuses a tag without both. The current section above is what the next release ships.

| Release | Hindsight server | Minimum Claude Code | Notes |
|---|---|---|---|
| v0.1.9-m6 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | Claude reports wrong or outdated memories and retires them (`forget --restore` undoes it), `memory stale`, the memory server installed on another machine (`server install --ssh`, `--print-commands`); Linux amd64 tested ([notes](releases/v0.1.9-m6.md)) |
| v0.1.8-m6 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | one memory for all your PCs (`server expose`, `server token`, `connect`, `setup --role server`), `memory_credit` and the record of which memories Claude used (`feedback log`, `feedback rebuild`); Linux amd64 tested ([notes](releases/v0.1.8-m6.md)) |
| v0.1.7-m5 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | doctor knows the `tools-used` transcript records; Linux amd64 tested ([notes](releases/v0.1.7-m5.md)) |
| v0.1.6-m5 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | memory kinds on search results, search and close-the-loop guidance, retro rewrite, tool-failure pitfalls (off by default); Linux amd64 tested ([notes](releases/v0.1.6-m5.md)) |
| v0.1.5-m5 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | backfill progress on stderr and `retain backfill --status`; Linux amd64 tested ([notes](releases/v0.1.5-m5.md)) |
| v0.1.4-m5 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | banks renamed to backsight, `/backsight:doctor`, setup and backup fixes; Linux amd64 tested ([notes](releases/v0.1.4-m5.md)) |
| v0.1.3-m5 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | shorter memory citation, `gpt-6-luna` as the OpenAI default; Linux amd64 tested ([notes](releases/v0.1.3-m5.md)) |
| v0.1.2-m5 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | setup without a user scheduler, doctor fixes; Linux amd64 tested ([notes](releases/v0.1.2-m5.md)) |
| v0.1.1-m5 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | security, setup and memory-quality fixes; Linux amd64 tested ([notes](releases/v0.1.1-m5.md)) |
| v0.1.0-m5 | 1.1.0 (`sha256:50311d02…`) | 2.1.309 | first public pre-release; Linux amd64 tested ([notes](releases/v0.1.0-m5.md)) |
