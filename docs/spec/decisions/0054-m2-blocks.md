# 0054 — M2 in four blocks, and what the spec leaves open for the read side

- **Date**: 2026-10-24
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, at the start of M2 (author: "Go", 2026-10-24)

## Decision

### Blocks (each closed by its own review, then the M2 review)
1. **Read core**:
   - fix backsight issue 101 first (segment 1 replacing a document after a lost cursor);
   - the recall engine: §4 scopes, separate calls, merge with a +0.05 boost for the current project, raw-fact fallback while observations are not yet consolidated, `related` scope from a lazily built entity index;
   - session state;
   - CLI `search`, `read`, `why`;
   - instruction set v1;
   - `backsight mcp` with `memory_search`, `memory_read` and `memory_store`.
2. **Hooks**:
   - the supervisor and its chaos tests;
   - every event of §6.2: briefing, banner, env file, prompt recall with sticky muting, `MIN_GAP`, seen set, reminder, nudge, detector, gate, PostToolUse flag, subagent start and stop, Stop, SessionEnd;
   - the context budget;
   - the A.4 telemetry events;
   - the `components.read` flag.
3. **Client wiring**:
   - the plugin generator (local marketplace) and `client install [--plan]`: `claude plugin marketplace add/install`, then the permission rule through the config editor (`ensure-in-array`);
   - the SessionStart drift check;
   - installation into the dev profile only (`CLAUDE_CONFIG_DIR`, 0048), with a live smoke test (`claude -p` in that profile) proving the plugin loads, hooks inject and the three tools answer.
   - The CLAUDE.md block option waits for `setup` (M4); the engine already supports it.
4. **Replay gate**: experiment E23 replays real prompts from transcripts (E19 method).
   - Paths compared: backsight's prompt path against the dev bank, and the reference hook's recall against the reference bank.
   - Grading: one judge, blind.
   - Metrics: nDCG@3, useful-injection rate and injection precision.
   - **Pass**: backsight's point estimate ≥ the reference on nDCG@3 and on useful injections, with the paired difference's 95 % CI lower bound > −0.05.
   - Chaos tests must be green.
   - Scripts live in the spec repo with E23 and are re-run at each later milestone.

### Choices the spec leaves open
- **MCP**: the official Go SDK `github.com/modelcontextprotocol/go-sdk` (v1.7.0, CGO-free), stdio transport. The server's `instructions` is the short guide; tool descriptions come from the instruction set.
- **Instruction set**: package `internal/instructions`, Go string constants plus a version `v1`, with tests on every size limit of §8 and §6.2.
  - Texts are English.
  - The citation format is exactly `> 🧠 **From Hindsight memory (<project>)** — <facts>`, with project in place of bank, since one bank holds all projects.
- **Read scopes** run in parallel, each with its own deadline inside the 17 s recall budget. A scope that fails is logged and skipped; it is never fatal.
- **Session state** is `<state>/sessions/<id>.json` (A.3).
  - Hooks write it with a try-lock and give up instead of waiting.
  - The last writer wins only within one session, and one session runs one hook at a time in practice.
- **`why`** reads the session state. It never calls the server.
- **Tool names**: `mcp__plugin_backsight_memory__memory_search|memory_read|memory_store` (§5). The permission rule allows these three names exactly.

## Why
M2 replaces the reference hooks and MCP servers on the dev install, and its gate is a measurement (the replay), as M1's was (E22). Splitting it lets each review read one coherent part. Blocks 1 and 3 can be exercised by hand and in the sandbox before the hooks exist. A non-inferiority margin (−0.05) avoids failing the gate on noise with 100 prompts while still catching a real regression; E19's significant effects were ≥ 0.05.
