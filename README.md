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
