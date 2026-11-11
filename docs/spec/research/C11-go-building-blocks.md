# C11 — Go building blocks for the hindsight-claude binary

Date: 2026-10-23. Repository metadata (licence SPDX as GitHub reports it, stars, last push, latest release) pulled with `gh api repos/<r>` and `gh api repos/<r>/releases/latest` the same day — VERIFIED unless marked INFERRED. "CGO" column: whether the library needs CGO for our targets (linux amd64/arm64, darwin arm64, windows amd64).

## 1. Summary table

| Need | Candidate | Licence | Latest | Last push | Stars | CGO? | Notes |
|---|---|---|---|---|---|---|---|
| MCP stdio server | **modelcontextprotocol/go-sdk** (official) | MIT → Apache-2.0 transition ("NOASSERTION" on GitHub; LICENSE: new code Apache-2.0, unconsented old code stays MIT) | v1.8.0 (2026-09-14) | 2026-10-23 | 5.3k | no | v1.7.0+ supports spec **2026-07-28** plus 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05; `mcp.NewServer(...)`, `server.Run(ctx, &mcp.StdioTransport{})` (README) |
| MCP stdio server | mark3labs/mcp-go | MIT | v1.3.0 (2026-10-14) | 2026-10-22 | 9.2k | no | implements spec 2025-11-25 (+ older); README: "under active development … some advanced capabilities are still in progress"; `server.ServeStdio(s)`; stdio, SSE, streamable HTTP |
| Release / cross-compile | **goreleaser/goreleaser** | MIT | v2.19.0 (2026-10-22) | 2026-10-23 | 16.1k | n/a | `CGO_ENABLED=0` matrix trivial; checksums, `signs` (cosign), SBOM, archives, nfpm (deb/rpm/apk), Homebrew, Scoop/winget, Docker; macOS sign+notarize cross-platform (quill) "Since v2.1", works from Linux, needs Apple Developer ID ($99/yr) + App Store Connect key; native notarize is Pro-only (https://goreleaser.com/customization/notarize/) |
| Signing (keyless) | **sigstore/cosign** (CLI in CI) | Apache-2.0 | v3.1.4 (2026-10-20) | 2026-10-22 | 6.4k | n/a | keyless via GitHub OIDC → Fulcio cert + Rekor entry; GoReleaser `signs:` integration |
| Verify sigstore bundles in-binary | **sigstore/sigstore-go** | Apache-2.0 | v1.3.0 (2026-07-30) | 2026-10-21 | 95 | no (I) | README: "considered stable and ready for production use", passes sigstore-conformance; bundle verify, TUF trusted-root fetch, custom trusted root, Rekor/TSA; "minimize the dependency tree" |
| Verify minisign | jedisct1/go-minisign | MIT | 0.2.6 (2026-05-27) | 2026-05-27 | 120 | no | tiny, verify-only; Ed25519 public key embedded in binary; needs minisign key management in CI |
| Verify minisign (alt) | aead/minisign | MIT | — | 2025-01-29 | 140 | no | sign + verify; quieter maintenance |
| Self-update | **creativeprojects/go-selfupdate** | MIT | tag v1.7.0 (no GH release object) | 2026-10-22 | 152 | no (I) | GitHub/GitLab/Gitea/HTTP sources; validators: SHA-256, ECDSA, single checksums file, custom `Validator` interface (README "Hash or Signature Validation") → plug sigstore-go or minisign there |
| Self-update (apply) | minio/selfupdate | Apache-2.0 | v0.6.0 (2023-01-22) | 2025-10-21 | 937 | no | low-level "replace running binary" (Windows rename trick), checksum + minisign/ed25519 verifier options (I from memory of API, not re-read); little activity |
| Self-update (alt) | fynelabs/selfupdate | Apache/MIT-style (copyright Alan Shreve 2015 + Fyne Labs 2022; GH "NOASSERTION") | tag v0.2.1 | 2026-10-14 | 162 | no | fork of go-update; ed25519 verification (I) |
| Self-update (legacy) | rhysd/go-github-selfupdate | MIT | — | 2023-09-19 | 646 | no | unmaintained → avoid |
| TUI / wizard | **charmbracelet/bubbletea** | MIT | v2.1.0 (2026-10-08) | 2026-10-21 | 45.5k | no | Elm-style TUI framework |
| Forms / wizard | **charmbracelet/huh** | MIT | v2.0.3 (2026-03-10) | 2026-10-01 | 7.2k | no | form/prompt library on bubbletea; has an accessible (non-TUI) mode (I) |
| Styling | charmbracelet/lipgloss | MIT | — | 2026-10-11 | 11.9k | no | — |
| CLI framework | spf13/cobra / urfave/cli | Apache-2.0 / MIT | — | 2026-07 / 2026-10 | 44.7k / 24.3k | no | — |
| OS services | **kardianos/service** | Zlib | tag v1.3.0 | 2026-08-29 | 4.9k | no | Windows service, systemd/Upstart/SysV, launchd; `UserService` option for systemd --user / launchd agents (README "BUGS" mentions UserService); Windows services need admin; no timer concept (I) |
| OS keychain | **zalando/go-keyring** | MIT | v0.2.8 (2026-03-23) | 2026-10-16 | 1.4k | **no** | macOS via `/usr/bin/security` CLI; Linux/BSD via Secret Service over D-Bus (pure Go); Windows Credential Manager (README). Fails without a Secret Service (headless servers, SSH sessions) (I) |
| OS keychain (alt) | 99designs/keyring | MIT | — | 2024-05-07 | 658 | **yes on macOS** (Keychain via cgo) (I) | stale; file/pass backends |
| Docker control | moby client `github.com/moby/moby/client` | Apache-2.0 | module tags `client/v0.6.x` (2026) | 2026-10-22 | 72k | no | Engine API client, separate Go module since moby v29 (V `client/go.mod`) |
| Docker Compose as library | `github.com/docker/compose/v5/pkg/compose` (`NewComposeService`) | Apache-2.0 | v5.6.1 (2026-10-21) | 2026-10-22 | 38k | no (I) | official SDK doc `docs/sdk.md`; pulls `github.com/docker/cli` (+ buildkit…) → large dependency tree / binary (I) |
| Compose file model | compose-spec/compose-go | Apache-2.0 | — | 2026-10-23 | 447 | no | parse/validate compose files only |
| Docker (alt) | shell out to `docker compose …` | — | — | — | — | no | zero deps, follows user's Docker/Podman/OrbStack CLI; needs CLI ≥ v2 on PATH; parse `--format json` |
| SSH client | **golang.org/x/crypto/ssh** | BSD-3-Clause | (x/crypto) | 2026-10-20 | 3.4k | no | client, agent (`ssh/agent`), known_hosts (`ssh/knownhosts`); no ~/.ssh/config parsing (needs e.g. kevinburke/ssh_config) (I) |
| Embedded Postgres | **fergusstrange/embedded-postgres** | MIT | v2.0.1 (2026-10-19) | 2026-10-19 | 1.3k | no | downloads checksum-pinned PostgreSQL bundles (theseus-rs / zonky binaries), defaults PG 18.6.1 (V15–V18), CI on Linux+macOS amd64/arm64, Windows amd64/arm64; persistent `DataDir`; **pgvector not bundled**: "Native extensions such as pgvector/PostGIS can live in caller-owned distributions" via `BinaryProvider`/hooks (README) |
| PG binaries | zonkyio/embedded-postgres-binaries | Apache-2.0 | — | 2026-08-26 | 218 | n/a | Maven-packaged PG binaries (no pgvector) (I) |
| pgvector | pgvector/pgvector | PostgreSQL licence (GH "NOASSERTION") | — | 2026-10-23 | 23.5k | n/a | C extension; must be compiled per PG major × OS × arch, or taken from a distro/Docker image (`pgvector/pgvector:pg18`) |
| Postgres driver | jackc/pgx | MIT | — | 2026-10-21 | 14.4k | no | if the binary ever talks to PG directly (backup checks) |
| Encryption | **filippo.io/age** (FiloSottile/age) | BSD-3-Clause | v1.3.2 (2026-08-29) | 2026-08-29 | 24k | no | Go library + CLI; X25519, scrypt passphrase, SSH keys, plugins; v1.3.0 adds post-quantum recipients (release notes "brings post-quantum…") |
| Compression / archive | stdlib `archive/tar` + `compress/gzip`; klauspost/compress (zstd, faster gzip) | BSD-3 / BSD-3+Apache (GH NOASSERTION) | — | 2026-10-21 | 5.7k | no | stdlib sufficient for tar.gz; zstd via klauspost |
| Archive (multi-format) | mholt/archives | MIT | — | 2026-08-05 | 448 | no | unified tar/zip/zstd/xz; probably unnecessary |
| Safe path join (restore) | cyphar/filepath-securejoin | BSD-3 / MPL (GH NOASSERTION) | — | 2026-06-18 | 113 | no | prevents tar path traversal on restore; Go 1.24 `os.Root` is a stdlib alternative (I) |
| OpenPGP (if ever) | ProtonMail/go-crypto | BSD-3-Clause | — | 2026-09-29 | 423 | no | not needed if age + sigstore/minisign |
| Local state DB (if needed) | modernc.org/sqlite (pure Go) vs mattn/go-sqlite3 | BSD-3 / MIT | — | — | — | modernc: no; **mattn: yes** | flag: prefer modernc or plain JSON files (I, not fetched) |

## 2. MCP SDK comparison

| Criterion | go-sdk (official) | mcp-go |
|---|---|---|
| Governance | modelcontextprotocol org, maintained with Google (I) | community (mark3labs) |
| Spec coverage | 2026-07-28 latest (v1.7.0+) | 2025-11-25 |
| Stability statement | v1.x semver, compatibility table | README "under active development" |
| Stdio server | `StdioTransport` | `ServeStdio` |
| Tool `_meta` / annotations (needed for `anthropic/alwaysLoad`, read-only hints) | supported in spec types (I) | supported (I) |
| Licence | MIT/Apache-2.0 mix (transition) | MIT |
Sources: READMEs via `gh api repos/{modelcontextprotocol/go-sdk,mark3labs/mcp-go}/readme`.

## 3. Release, signing and update chain (facts)

- Cross-compilation: Go builds linux/amd64, linux/arm64, darwin/arm64, windows/amd64 from Linux with `CGO_ENABLED=0` as long as no dependency needs CGO; every candidate above is pure Go except 99designs/keyring (macOS) and mattn/go-sqlite3.
- GoReleaser OSS produces archives + `checksums.txt`, signs with cosign keyless in GitHub Actions (OIDC), can also produce SBOMs; GitHub artifact attestations (`actions/attest-build-provenance`, verified by `gh attestation verify`) are sigstore bundles too (I).
- macOS: unsigned/un-notarized CLI binaries downloaded by a browser get the quarantine attribute and Gatekeeper prompts; binaries fetched by `curl` or by the binary's own updater are not quarantined (I). Cross-platform notarization from Linux is possible with GoReleaser OSS (quill) and an Apple Developer account.
- Windows: Authenticode signing optional (SmartScreen reputation); not covered by GoReleaser natively beyond custom `signs` commands (I).
- In-binary verification options: (a) sigstore-go verifying a cosign bundle against the expected GitHub Actions identity (`https://github.com/<owner>/<repo>/.github/workflows/release.yml@refs/tags/v*`) — keyless, but needs the Sigstore trusted root (TUF fetch or embedded snapshot, refreshed periodically); (b) minisign/Ed25519 with a public key embedded at build time — simplest, offline, but a long-lived private key in CI secrets; (c) both.
- go-selfupdate's `Validator` interface is the integration point for either (a) or (b); replacing a running `.exe` on Windows needs the rename-then-replace trick these libraries implement (I).

## 4. Docker control: SDK vs CLI (facts)

| | moby client SDK | compose SDK | shell `docker compose` |
|---|---|---|---|
| Deps | moderate | heavy (docker/cli, buildkit, …) (I) | none |
| Compose support | none (raw containers) | full | full |
| Podman / OrbStack / Colima | via Docker-compatible socket (I) | same | via their `docker` shim (I) |
| Determinism / testability | high | high | output parsing (`--format json`) |
| Version skew | API version negotiation | tied to compose release | user's installed CLI |

## 5. CGO flags

- **Requires CGO**: 99designs/keyring (macOS Keychain backend) (I); mattn/go-sqlite3 (I). Avoid both.
- **Pure Go**: go-sdk, mcp-go, sigstore-go (I, no cgo mentioned), go-minisign, go-selfupdate, minio/selfupdate, bubbletea/huh/lipgloss, kardianos/service, zalando/go-keyring (macOS via `security` CLI, Linux via D-Bus, Windows via syscalls), moby client, x/crypto/ssh, embedded-postgres (downloads native PG binaries at runtime — native code, but not linked), age, klauspost/compress, stdlib archive.

## Implications for hindsight-claude

- A fully CGO-free single binary for the four targets is achievable with mainstream, actively maintained libraries (all pushed within the last ~2 months except minio/selfupdate and the minisign libs).
- The official MCP Go SDK is the one tracking the newest spec (2026-07-28) with a semver compatibility table; mcp-go is more popular but one spec behind and self-described as in active development.
- Signature verification inside the updater is not built into any self-update library: it is a `Validator` hook where sigstore-go (keyless, needs trusted-root management) or an embedded minisign/Ed25519 key (simple, offline) must be wired.
- Keychain storage via zalando/go-keyring works on desktops but has no backend on headless Linux (no Secret Service), which is exactly the remote-server case (C10); a file-based fallback is unavoidable there.
- kardianos/service covers long-running services (systemd/launchd/Windows service) but not periodic timers; scheduled jobs (backups, retro) need per-OS timer units, Task Scheduler, or a resident process.
- Embedded Postgres in Go is mature (PG 15–18, pinned checksums, all four targets), but pgvector — required by Hindsight — is not bundled and would have to be built and distributed per PG major × OS × arch by us.
- Docker control: shelling out to `docker compose` keeps the binary small and engine-agnostic; the Compose SDK exists but drags in the Docker CLI dependency tree.
- age (BSD-3) covers backup encryption with passphrase, X25519 or SSH-key recipients, in pure Go; stdlib tar/gzip suffices for archives, with a safe-join check on restore.
