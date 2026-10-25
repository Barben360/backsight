# Security policy

backsight keeps a long-term memory of your Claude Code sessions on a Hindsight server you run. That memory can hold anything you discussed with Claude, so a flaw in backsight can expose it. This page says how to report one, what is supported, what backsight defends against, and how releases are signed.

## Reporting a vulnerability

Please do not open a public issue, discussion or pull request for a vulnerability.

Use GitHub's private vulnerability reporting, at https://github.com/Barben360/backsight/security/advisories/new (the "Report a vulnerability" button under the Security tab). Only the maintainer sees the report. There is no email contact.

Include what you can:

- the version: `backsight version --json`;
- what an attacker can do, and what they need first (local access, a crafted web page summarised by Claude, a network position…);
- the steps to reproduce it.

Never paste real memories, transcripts, API keys or backup archives into a report. If the proof needs one, say so and we will agree on a way to share it.

backsight is a one-person preview project: reports are answered on a best-effort basis. A fix ships as a new release, and the advisory is published once the release is out.

## Supported versions

backsight is a preview. Only the latest release, pre-releases included, gets security fixes: there are no backports. `backsight update check` tells you when a newer release exists, and `backsight update apply` installs it.

The `v0.0.1-m0` and `v0.0.2-m0` releases are early skeletons with no server or client, and are not supported.

## Threat model

What backsight defends against:

| Threat | What backsight does |
|---|---|
| **A secret reaches memory** (a key or password typed or printed in a session) | Text is redacted before every write (session transcripts, subagent text, `memory_store`): private keys, known token prefixes (`sk-`, `sk-ant-`, `ghp_`, `github_pat_`, `AKIA`, JWTs…), bearer headers, passwords in URLs, `KEY=secret` lines and high-entropy values next to secret-like names become `[redacted:<kind>]`. `backsight memory scan` re-checks what is already stored. `backsight forget --purge <id>` deletes the memory, its source facts and the document they came from; backups made before the purge still hold the secret, and the command lists them. |
| **Hostile content becomes memory** (a web page, README or issue that Claude summarised, later recalled into another project) | Tool results are never retained, only the conversation. Recalled memories are framed to Claude as quoted past notes to verify, not as instructions. Memories from other projects are injected into a prompt only above a higher score (`recall.global_min_score`, 0.6 by default). |
| **The server's API key leaks** | The server listens on loopback (127.0.0.1) by default, and its API and web UI always require a key. Secrets live only in `secrets.toml` and the server's rendered `.env`, both readable by your user only (mode 0600). The client does not follow HTTP redirects, so the key is never sent to another host, and `backsight doctor` warns when the key would travel unencrypted to a non-loopback host. |
| **A backup is copied** | Backup archives are always encrypted with [age](https://age-encryption.org), to the recipients you chose at setup. |
| **A tampered release or image** | Releases are signed (see below). The server images are pinned by digest in the signed binary, so Docker pulls exactly the image the release names ([docs/compatibility.md](docs/compatibility.md)). |

Out of scope: someone who can already run commands as your user, or as root, on the machine that holds the memory. They can read it directly.

Your transcripts do leave the machine in one place: the Hindsight server sends session text to the LLM provider you chose at setup to extract memories (no third party if you chose a local model). Setup shows this data flow and asks for your consent before anything is stored. `backsight pause` stops recording a session, and `backsight pause --project` a whole project.

## What backsight never does

- **No telemetry leaves the machine.** backsight logs its own events to local files under its state directory (`backsight paths` shows where), and sends them nowhere.
- **No automatic issues.** backsight never opens an issue or sends a report by itself. A diagnostic for a bug report is built locally and redacted (never memory text), and you decide whether to submit it.
- **No memory deleted on its own.** Only explicit commands delete memory: `forget`, `forget --purge`, `restore`, and uninstalling with its delete-data option.
- **No update applied without your opt-in.** `update.auto_apply` is off by default.
- **No secrets asked in chat.** Claude is told never to ask for a key or token in the conversation; secrets go through `backsight secret set` in a terminal.

Network connections the backsight binary makes itself: to your Hindsight server, and to GitHub's releases (`api.github.com`) once a day, on `update check`, and to download a release on `update apply`. Those requests carry no memory and no usage data; they carry `GH_TOKEN` or `GITHUB_TOKEN` when one is set in the environment. Docker pulls the pinned images from their registries (`ghcr.io`, Docker Hub) at setup and on a server upgrade.

## Release signing

Releases are signed with [minisign](https://jedisct1.github.io/minisign/) (Ed25519). Each release's `checksums.txt` lists the SHA-256 of every archive and bare binary, and carries a signature. Every binary trusts two public keys, embedded at build time and listed by `backsight version --json`:

| Role | Key id | Public key file |
|---|---|---|
| current | `CB9E8D3622A9CEDE` | [`minisign.pub`](minisign.pub) |
| next | `645A401244EFB224` | [`minisign.next.pub`](minisign.next.pub) |

- **What is accepted**: a prehashed signature (minisign's default `ED` format) by one of these two keys, whose trusted comment names `file:checksums.txt`. Legacy (non-prehashed) signatures are refused. `update apply` and the binary's own check apply this rule; `install.sh` applies it with the current key only.
- **Self-check**: `install.sh` and `update apply` keep `checksums.txt` and its signature next to the installed binary, which checks its own hash against them on its first scheduled run. `backsight doctor` reports the result (check `binary`).
- **Checking by hand**:

  ```sh
  minisign -Vm checksums.txt -p minisign.pub
  sha256sum -c --ignore-missing checksums.txt
  ```

- **Test keys never reach a release**: a binary trusts a test key only when built with the `testkey` build tag, which CI's tests use. Before a release is published, `make verify-keys` (run by goreleaser after each build) stops it if any target's binary was built with the `testkey` or `testhooks` tag, or if the binary the release machine can run trusts a key besides the two above or pins another Hindsight image than the source.
- **Where the secret keys live**: the current key's secret half is in the maintainer's password store and in the release workflow's encrypted secret; the next key's is only in the maintainer's password store, never on CI. Releases are built only from tags on `main`, by GitHub Actions pinned to commit SHAs.

### Key rotation

The next key exists so that a key can be replaced without asking anyone to re-trust anything:

1. **Planned rotation**, over two releases:
   - a release still signed by the current key ships binaries that trust the next key as their current key, plus a newly generated next key. `minisign.pub`, `minisign.next.pub` and the table above change in that release; check it by hand with the previous `minisign.pub`;
   - from the following release on, releases are signed by the former next key, which every binary already trusts, and `install.sh` switches to it.
2. **If the current key's secret leaks**: a release signed by the next key drops the leaked key and adds a new next key, announced in a security advisory. Binaries older than that release still trust the leaked key until they are updated, so update promptly when such an advisory appears.
3. **If the next key's secret leaks**: a release signed by the current key replaces it with a new next key.
4. **If both leak, or both are lost**: installed binaries cannot be updated safely any more. Reinstall from the bootstrap after checking the new `minisign.pub` against the security advisory.
