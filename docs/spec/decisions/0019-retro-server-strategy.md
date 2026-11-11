# 0019 — The retro is a Hindsight retain strategy, the binary calls no LLM

- **Date**: 2026-10-24
- **Status**: superseded by 0028
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
`jobs tick` retains settled, condensed tranches under the bank strategy `retro` (custom instructions with the language rule, durable facts only, `kind` labels). The end-of-project retro is a skill where the agent writes facts with `memory_store`.

## Why
E7/E9: same facts as the `claude -p` retro for ~1/40 of the cost, in the right language, typed through `kind:*` tags, on the provider the user already chose; removes background use of a Claude subscription (decision 0010).
