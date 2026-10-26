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

Tested on the author's machine with Docker 29.9 and Compose 5.6 (as of 2026-10-25). CI installs the Claude Code version pinned in `.github/workflows/ci.yml`.

## Platforms

Release binaries are built for four targets. Linux is the only platform tested today; the macOS and Windows binaries are compiled on every change and validated at milestone M7.

| OS / arch | Release binary | Status |
|---|---|---|
| Linux amd64 | yes | **tested**: Ubuntu 24.04 (CI, on GitHub's `ubuntu-latest` runners) and CachyOS (rolling, Arch-based; the author's machine) |
| Linux arm64 | yes | built, not tested |
| macOS arm64 (Apple silicon) | yes | built; validated at M7 |
| Windows amd64 | yes | built; validated at M7 |

Ubuntu 24.04:

- CI runs every check, the fresh-install chain included, on GitHub's `ubuntu-latest` runners (Ubuntu 24.04). From 2026-10-25 until the repository became public, CI ran on self-hosted runners on the author's CachyOS machine instead.
- The author's work PC (Ubuntu 24.04) is the first preview install.

Other targets (macOS on Intel, Windows on arm64, 32-bit) have no release binary.

## Releases

One row per release, added when it is tagged. The current section above is what the next release ships.

| Release | Hindsight server | Minimum Claude Code | Notes |
|---|---|---|---|
| v0.0.2-m0 | none | none | M0 skeleton: no server, no client; not usable |
| v0.0.1-m0 | none | none | M0 skeleton: no server, no client; not usable |
