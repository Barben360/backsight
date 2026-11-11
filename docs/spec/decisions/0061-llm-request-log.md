# 0061 — Hindsight's LLM request log keeps token counts, not text

- **Date**: 2026-10-25
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, end-of-M3 privacy review

## Decision
`server render` sets:
- `HINDSIGHT_API_LLM_TRACE_RETENTION_DAYS=30`: the rows behind `GET …/llm-requests/stats` stay 30 days, as `stats [days]` and the backfill budget (0051) read them;
- `HINDSIGHT_API_LLM_TRACE_MAX_CHARS=1`: each row keeps a one-character preview of the request and response, not their text. 0 would mean "no limit".

## Why
- Hindsight 1.1.0 traces every LLM call by default, with up to 50 000 characters of input and output kept for one day. That covers retain input (transcripts), recall candidates (memory texts) and page refreshes. The text of a paused or excluded session's prompt queries, and of a purged secret, stayed on the server for up to a day, and in any database backup taken in that window.
- The one-day retention also deleted the rows `stats` and the backfill budget count. Any period longer than a day read as one day of spend.
