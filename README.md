# backsight

A self-hosted [Hindsight](https://github.com/vectorize-io/hindsight) long-term memory for Claude Code, packaged as one Go binary that installs, wires, updates, backs up and repairs it.

> A *backsight* is the surveying reading taken back on a known point before moving forward.

**Status**: pre-alpha, milestone M0 (skeleton). Not usable yet.

backsight is an independent project, not affiliated with or endorsed by Anthropic or Vectorize.

## Principles

- Everything that can be deterministic stays deterministic: the binary detects, configures, migrates, checks and backs up; it calls no LLM itself.
- Hooks never block Claude Code: they always exit 0 within their budget.
- Nothing is applied outward without the user's opt-in; memory is only deleted by explicit user commands.

## License

MIT, see `LICENSE`.

## Development

`make test` (race detector), `make lint` (golangci-lint v2), `make cross` (compiles the four release targets), `make build` → `bin/backsight`.

Every command accepts `--json` and prints one envelope `{schema_version, command, ok, data | error}`. Exit codes: `0` ok, `2` invalid invocation, `10` needs the user, `20` blocked by the environment, `30` failed and rolled back, `40` partial and resumable.

## Verifying a release

Every archive and `checksums.txt` carry a minisign signature. With the public key from this repository:

```sh
minisign -Vm checksums.txt -p minisign.pub
sha256sum -c --ignore-missing checksums.txt
```

`checksums.txt` lists the archives and the bare binaries; `install.sh` keeps it with its signature next to the installed binary, which checks itself against it on its first run (`backsight doctor`, check `binary`). `minisign.next.pub` is the next key: binaries embed both (`internal/signing`, listed by `backsight version --json`) so a key rotation needs no trust reset; the release stops if its binary trusts any other key. Releases are built only from tags on `main`, by actions pinned to commit SHAs. Maintainers: `make snapshot` builds and signs locally (key from `pass`), and tags `v*` publish through `.github/workflows/release.yml`.
