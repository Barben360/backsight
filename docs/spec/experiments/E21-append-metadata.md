# E21 — Append retains and document metadata; operation_id idempotence (Hindsight 1.1.0)

Date: 2026-10-24. Scratch bank `e21-scratch` on the dev install (127.0.0.1:18888), deleted afterwards. **V** = observed.

| Check | Result |
|---|---|
| Retain `conversation:e21` with `metadata {segment:"1", last_uuid:"aaa"}`, then `update_mode: append` with `metadata {segment:"2", last_uuid:"bbb"}` | **V** `document_metadata` = `{segment:"2", last_uuid:"bbb"}`: the append **replaces** the metadata |
| `original_text` after the append | **V** both segments, joined by `\n` |
| Same `operation_id` sent twice (`async: true`) | **V** both answers carry that id; one `batch_retain` operation; 1 memory unit |

Consequences: decision 0038's server-held cursor works as designed (no `cursor:<session_id>` fallback needed); resends after a crash are safe with a deterministic `operation_id`.
