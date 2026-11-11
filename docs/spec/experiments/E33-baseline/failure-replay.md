# E33 addendum - failure pitfall replay (2026-10-27)

Decides `recall.failure_pitfalls` (backsight v0.1.6-m5, off by default). `scripts/replay-failures.py` (backsight) on this machine's transcripts, live against the real bank `backsight` (read-only recall, no LLM), after the 10 memory-about-memory forgets of the same day. The script's query is the hook's (tool + command + error, 300 chars) but searches the whole bank, without the hook's project scopes and `Seen` filter.

| Metric | Value |
|---|---|
| Transcripts / failed tool calls | 263 / 593 (Bash 514, Edit 55, Write 17, MCP 6, WebFetch 1) |
| Distinct (session, signature) looked up | 496 (401 distinct signatures) |
| Would inject at least one memory at reranker >= 0.8 | 120 (24 %) |
| Top-hit score of those | median 0.97, min 0.80 |
| Same 4 memories as top hit | 50 of 120 (an Clinic-app issue summary 18, an clinic-app memory-notes fact 17, the my-garage repo fact 8, a backsight clone pitfall 7) |

Precision, judged by hand on the first 25 injections: about 2 relevant (a denied `gh pr merge` → the auto-mode "Merge Without Review" pitfall; a failed chained gate → the chained-gate pitfall). The rest pair harness errors (`Blocked: sleep 60`, `File has not been read yet`, auto-mode denials) with unrelated memories at 0.95-1.0: the reranker saturates on long, path-heavy error text, so the 0.8 floor does not discriminate.

**Decision: `recall.failure_pitfalls` stays off.** Before another trial: drop harness errors (tool_use_error, permission denials, "not read yet"), query on the error's first line without paths, and use a relative criterion (top hit clearly above the session's prompt hits) instead of an absolute reranker floor. Replay again with those before enabling.
