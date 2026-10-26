# Contributing to backsight

How to build, test and change backsight. User documentation is in [docs/](docs/); this page is for people working on the code.

## Design principles

- Everything that can be deterministic stays deterministic: the binary detects, configures, migrates, checks and backs up. It calls no LLM itself; only the Hindsight server does, for extraction.
- Hooks never block Claude Code: they always exit 0 within their time budget, and never wait on a lock or prompt.
- Nothing is applied outward without the user's opt-in, and memory is deleted only by an explicit user command.
- Claude Code's files are edited only through the config editor: a copy of each file first, a byte-level patch, a write only if the file did not change meanwhile, and an undo.
- Secrets never travel in arguments or answers, and never reach a Claude session.

## Development

You need:

| Tool | Why |
|---|---|
| Go, the `toolchain` version in `go.mod` | builds and tests; an older Go downloads that toolchain by itself |
| a C compiler (`gcc` or `clang`) | `make test` runs the race detector, which needs cgo |
| `git` | some tests create repositories (project detection, transcript flush) |
| [golangci-lint](https://golangci-lint.run/) v2 | `make lint`; CI's version is in `.github/workflows/ci.yml` |
| `make` and `bash` | the Makefile and the scripts |

Optional:

| Tool | Without it |
|---|---|
| [gitleaks](https://github.com/gitleaks/gitleaks) | `make check` skips the secret scan and says so; CI runs it |
| `minisign` | the tests that check signatures with the real minisign skip; CI installs it |
| `openssl`, `curl` | the `install.sh` tests that use them skip |
| Docker with Compose v2 | needed only by the scripts that start a server (`scripts/sandbox.sh`, `scripts/fresh-install.sh` and the other proofs); `make test` and `make check` never touch Docker |

The first `make check` downloads the modules and builds the race runtime: count a few minutes; later runs take about a minute and a half.

| Command | Does |
|---|---|
| `make check` | the whole gate below, in one command |
| `make build` | builds `bin/backsight` |
| `make test` | runs every test with the race detector |
| `make test-hooks` | runs the packages with test-only code (the `testhooks` build tag) with that tag |
| `make lint` | runs golangci-lint |
| `make cross` | compiles and vets the four release targets (linux/amd64, linux/arm64, darwin/arm64, windows/amd64) |
| `make leaks` | scans the Git history for secrets |
| `make licenses` | regenerates `internal/licenses/THIRD_PARTY_LICENSES` from `go.mod`: run it after adding or updating a dependency and commit the file (CI and the release refuse a stale one, and a module without a license file) |

The code lives under `internal/`, one package per concern; `internal/cli` wires the commands.

## Never touch your own install

A development build must never run against the backsight install you use every day. Give every manual run its own directories:

```sh
HOME=/tmp/bs-dev/home BACKSIGHT_HOME=/tmp/bs-dev/backsight ./bin/backsight doctor
```

Scripts that start a server (`scripts/sandbox.sh`, `scripts/fresh-install.sh` and the other proofs) take their Compose project and ports from `scripts/ports.sh`, below 32768 and away from the default ports 8888 and 9999. They use Hindsight's mock LLM, so they cost nothing, and remove their stack afterwards. Never give a scratch stack the Compose project name `backsight`: that is the real install's.

## The gate

Before every commit, `make check` must pass. It runs, and stops at the first failure:

- `gofmt -l .` (it must list nothing) and `go vet ./...`;
- `make lint`, `make test` and `make test-hooks`;
- `make cross`;
- `gitleaks dir . --no-banner --redact`, when gitleaks is installed.

Run `bash -n` on every shell script you change. CI (`.github/workflows/ci.yml`) runs the tests, a signed snapshot release with a throwaway key, the sandbox tests and the fresh-install chain: setup from nothing, verify, store and search, backup and restore, update with rolled-back failures, undo and uninstall.

## Tests

- Each test must fail when the behaviour it guards is missing or wrong: assert the observable outcome (exit code, output, files written), not the implementation.
- Cover the refusals and edge cases, not only the happy path.
- Write the test first and watch it fail.
- Build tags `testkey` and `testhooks` exist only for tests and CI; a release build refuses both (`make verify-keys`). Code that makes backsight fail on purpose (a failing migration or server smoke, the `BACKSIGHT_HOOK_CHAOS` switch of hook children) lives in `testhooks` files with a no-op twin, so no environment variable can trigger it in a release.

## Documentation

- `docs/*.md` is the user documentation: one file per topic. It is published on GitHub and embedded in the binary, where `backsight guide` browses it and indexes each `##` section for search. So:
  - keep `##` headings stable, and make each section readable on its own;
  - plain Markdown only: no front matter, no HTML.
- A test (`internal/cli/docs_commands_test.go`) checks that every backsight command and flag written in code (inline or in a code block) in `README.md`, `CONTRIBUTING.md`, `SECURITY.md` and `docs/*.md` exists in the command tree. Rename a command or a flag, and the docs must follow.
- `docs/compatibility.md`'s `## Current` section and `SECURITY.md`'s keys are checked against what the binary pins and trusts (`internal/cli/release_docs_test.go`). The README's Hindsight badge is checked against the same pin.
- `docs/agent-guide.md` is for coding agents that install backsight; keep it in line with the other docs.
- Text a user sees (help, setup questions, errors, doctor checks, generated files, release notes) names no design record, experiment, milestone or spec section: users cannot look them up. Say the reason in plain words instead. `internal/cli/publictext_test.go` enforces it; code comments may cite them.

## The agent contract

Every command accepts `--json` and prints one envelope `{schema_version, command, ok, data | error}`. Exit codes: `0` ok, `2` invalid invocation, `10` needs the user, `20` blocked by the environment, `30` failed and rolled back, `40` partial and resumable. Commands that change configuration, user files or the server's setup take `--plan`; data operations (`secret set`, `backup create`, `store`, `forget <id>`) do not; commands that can prompt take `--non-interactive`. Details for users: [docs/commands.md](docs/commands.md#conventions).

## Dependency updates

A Dependabot `gomod` PR that changes `go.mod` fails `TestThirdPartyLicensesAreCurrent` until the embedded licenses file is regenerated. Fix it on the PR's branch with one line:

```sh
gh pr checkout <n> && make licenses && git commit -am "🔧 Regenerate the third-party licenses" && git push
```

This is deliberately not automated. A workflow that pushes to a Dependabot branch needs `contents: write`, which Dependabot-triggered `pull_request` runs do not get; `pull_request_target` would grant it, but then runs the PR's code with a write token. Commits pushed with `GITHUB_TOKEN` also do not start new CI runs. Read the changelog of the updated modules too: a license change is visible in the regenerated file.

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

Maintainers: `make snapshot` builds and signs locally with a throwaway key (never the release key, which only `.github/workflows/release.yml` holds), and tags `v*` publish through that workflow. Before tagging, write the release notes in `docs/releases/<tag>.md` (what changed for users, requirements, known limits) and add the tag's row to `docs/compatibility.md`'s Releases table: the workflow refuses a tag without either, and publishes the notes as the release's description.
