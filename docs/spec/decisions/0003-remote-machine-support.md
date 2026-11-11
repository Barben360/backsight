# 0003 — The stack can run on a remote machine

- **Date**: 2026-10-23
- **Status**: accepted (transport and security details open: check C10, decision D3/D16)
- **Decided by**: author

## Decision
The Hindsight server side can run on a machine other than the one running Claude Code. Installation helps the user in that case, either:
- through an SSH access the user explicitly authorizes, the binary driving the remote install; or
- by giving the user the exact commands to run themselves on the remote machine.

## Consequences
Remote mode needs authenticated, encrypted transport and hooks that tolerate network latency. It also opens the multi-machine case (several Claude Code machines sharing one memory).
