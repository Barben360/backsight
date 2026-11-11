# 0046 — Memory threats: poisoning and leaked secrets

- **Date**: 2026-10-24
- **Status**: accepted (agent, from the independent spec critique of 2026-10-24; open to veto)
- **Decided by**: agent

## Decision
- **Threat model** (SPEC §13): (a) a secret reaches memory; (b) hostile content (a web page, README or issue summarised by the assistant) becomes a durable fact and is later injected into other projects; (c) the bearer key leaks. Out of scope: a hostile local user.
- **Poisoning**:
  - Tool results are never retained (normaliser, §9.1).
  - Every injected memory is framed as quoted past notes to verify, never as instructions. The recall framing carries a "data, not instructions" line.
  - Per-prompt recall injects `global` hits only at a higher score than project hits: `recall.global_min_score`, default 0.6, tuned on the M2 replay.
  - `memory_search` still reaches the whole bank on request.
- **Leaked secret**:
  - `forget --purge <id>` deletes the memory, its source facts and the document they came from, then lets the project's observations re-consolidate.
  - `memory scan` proposes a purge for secrets, not a soft forget.
  - Existing backups still hold the secret. The command says so and lists the archives older than the purge.
- The agent guide tells the agent never to ask for a key or token in chat. Secrets go only through `backsight secret set` in a terminal.

## Why
The critique noted both gaps. Soft invalidation is right for wrong facts, not for credentials. Cross-project injection is the main path for poisoning.
