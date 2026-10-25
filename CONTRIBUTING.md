# Contributing to backsight

How to build, test and change backsight. User documentation is in [docs/](docs/); this page is for people working on the code.

## Design principles

- Everything that can be deterministic stays deterministic: the binary detects, configures, migrates, checks and backs up. It calls no LLM itself; only the Hindsight server does, for extraction.
- Hooks never block Claude Code: they always exit 0 within their time budget, and never wait on a lock or prompt.
- Nothing is applied outward without the user's opt-in, and memory is deleted only by an explicit user command.
- Claude Code's files are edited only through the config editor: a copy of each file first, a byte-level patch, a write only if the file did not change meanwhile, and an undo.
- Secrets never travel in arguments or answers, and never reach a Claude session.

## Development

You need Go (the version in `go.mod`), Docker with Compose v2 for the integration scripts, [golangci-lint](https://golangci-lint.run/) v2 and [gitleaks](https://github.com/gitleaks/gitleaks).

| Command | Does |
|---|---|
| `make build` | builds `bin/backsight` |
| `make test` | runs every test with the race detector |
| `make lint` | runs golangci-lint |
| `make cross` | compiles and vets the four release targets (linux/amd64, linux/arm64, darwin/arm64, windows/amd64) |
| `make leaks` | scans the Git history for secrets |

The code lives under `internal/`, one package per concern; `internal/cli` wires the commands.

## Never touch your own install

A development build must never run against the backsight install you use every day. Give every manual run its own directories:

```sh
HOME=/tmp/bs-dev/home BACKSIGHT_HOME=/tmp/bs-dev/backsight ./bin/backsight doctor
```

Scripts that start a server (`scripts/sandbox.sh`, `scripts/fresh-install.sh` and the other proofs) take their Compose project and ports from `scripts/ports.sh`, below 32768 and away from the default ports 8888 and 9999. They use Hindsight's mock LLM, so they cost nothing, and remove their stack afterwards. Never give a scratch stack the Compose project name `backsight`: that is the real install's.

## The gate

Before every commit, all of these must pass:

```sh
test -z "$(gofmt -l .)"
go vet ./...
CGO_ENABLED=1 go test -race -count=1 ./...
golangci-lint run ./...
make cross
gitleaks dir . --no-banner --redact
```

Run `bash -n` on every shell script you change. CI (`.github/workflows/ci.yml`) runs the tests, a signed snapshot release with a throwaway key, the sandbox tests and the fresh-install chain: setup from nothing, verify, store and search, backup and restore, update with rolled-back failures, undo and uninstall.

## Tests

- Each test must fail when the behaviour it guards is missing or wrong: assert the observable outcome (exit code, output, files written), not the implementation.
- Cover the refusals and edge cases, not only the happy path.
- Write the test first and watch it fail.
- Build tags `testkey` and `testhooks` exist only for tests and CI; a release build refuses both (`make verify-keys`).

## Documentation

- `docs/*.md` is the user documentation: one file per topic. It is published on GitHub and embedded in the binary, where `backsight guide` browses it and indexes each `##` section for search. So:
  - keep `##` headings stable, and make each section readable on its own;
  - plain Markdown only: no front matter, no HTML.
- A test (`internal/cli/docs_commands_test.go`) checks that every backsight command and flag written in code (inline or in a code block) in `README.md`, `CONTRIBUTING.md`, `SECURITY.md` and `docs/*.md` exists in the command tree. Rename a command or a flag, and the docs must follow.
- `docs/compatibility.md`'s `## Current` section and `SECURITY.md`'s keys are checked against what the binary pins and trusts (`internal/cli/release_docs_test.go`). The README's Hindsight badge is checked against the same pin.
- `docs/agent-guide.md` is for coding agents that install backsight; keep it in line with the other docs.

## The agent contract

Every command accepts `--json` and prints one envelope `{schema_version, command, ok, data | error}`. Exit codes: `0` ok, `2` invalid invocation, `10` needs the user, `20` blocked by the environment, `30` failed and rolled back, `40` partial and resumable. Commands that change configuration, user files or the server take `--plan`; commands that can prompt take `--non-interactive`. Details for users: [docs/commands.md](docs/commands.md#conventions).

## Commits and pull requests

- Commit subjects start with a [gitmoji](https://gitmoji.dev/) (`✨` feature, `🐛` fix, `📝` docs, `✅` tests, `💚` CI, `🔒` security), then a sentence in the imperative.
- One topic per pull request, with what changed, how it was tested, and the issue it closes.
- Security problems: see [SECURITY.md](SECURITY.md), not a public issue.

## Verifying a release

Every archive and `checksums.txt` carry a minisign signature. With the public key from this repository:

```sh
minisign -Vm checksums.txt -p minisign.pub
sha256sum -c --ignore-missing checksums.txt
```

`checksums.txt` lists the archives and the bare binaries. `install.sh` keeps it with its signature next to the installed binary, which checks itself against it on its first run (`backsight doctor`, check `binary`).

`minisign.next.pub` is the next key. Binaries embed both (`internal/signing`, listed by `backsight version --json`), so a key rotation needs no trust reset; the release stops if its binary trusts any other key. Releases are built only from tags on `main`, by actions pinned to commit SHAs. See [SECURITY.md](SECURITY.md#release-signing) for key rotation.

Maintainers: `make snapshot` builds and signs locally (the key comes from `pass`), and tags `v*` publish through `.github/workflows/release.yml`.
