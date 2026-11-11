# 0024 — Remote server install driven by the binary over the system ssh

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
`server install --ssh user@host` copies the matching release, runs the server steps remotely with JSON output, and brings back a connection token (URL + key) into the client's secrets. Exposure is loopback or a private address; public binds are refused. Without SSH: printed commands + `connect --token -`.

## Why
Decision 0003 asks the binary to drive the remote install; using the system ssh reuses the user's keys and known_hosts and adds no SSH code; the token avoids copying keys by hand.
