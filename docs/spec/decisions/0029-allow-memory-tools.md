# 0029 — Allow-list all three memory tools

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments
- **Amends**: 0012 (the single settings write now covers `memory_store` too)

## Decision
The permission rule written to user settings allows `memory_search`, `memory_read` and `memory_store`.

## Why
E15: `readOnlyHint` does not skip permission prompts. A prompt on every `memory_store` would defeat the success criterion "records what it learns" and the detector nudges. Writes are redacted, go only to the user's own memory, and are never destructive (corrections are new facts, 0016).
