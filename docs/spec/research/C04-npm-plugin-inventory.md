# C04 — Behaviour inventory of `@vectorize-io/hindsight-coding-agents` 0.7.0

Date: 2026-10-23. Scope: the Claude Code (`claude-code` harness) path only; the 14 other harnesses are listed only where they shape shared code.

## Sources

- TypeScript source IS published: monorepo `vectorize-io/hindsight`, dir `hindsight-integrations/coding-agents`, tag `integrations/coding-agents/v0.7.0` (commit `0c0869b7`, 2026-09-24). Abbreviation below: **`$SRC`** = `https://github.com/vectorize-io/hindsight/blob/integrations/coding-agents/v0.7.0/hindsight-integrations/coding-agents/src`. Read locally from a sparse clone in the scratchpad.
- Installed runtime: `/home/user/.hindsight/coding-agents/` (`package.json` version 0.7.0, `.install-origin.json` = staged from `~/.npm/_npx/…`, 2026-10-10). **VERIFIED** dist == tag: `raw.pageRefreshEveryTurns || 1` and `MIN_IDEMPOTENT_RETAIN_VERSION = "0.8.6"` present in `dist/claude-hook.js` / `dist/claude-stop-hook.js`.
- Upstream main is already at 0.8.2 (tags v0.8.0–v0.8.2 exist, `git ls-remote`), so 0.7.0 is 3 releases behind. New core files on main not in 0.7.0: none relevant spotted beyond new harnesses (codebuddy, kimi, traecode, workbuddy) — **INFERRED**, not diffed.
- Live state: `~/.hindsight/coding-agents-logs/{diag.jsonl,plugin.log,usage.jsonl}`, `/tmp/hindsight-claude-code/*.json`, server `GET http://127.0.0.1:8888/version` → `api_version 0.10.1`.
- Licence: see C1 (not re-checked here).

Status tags: **V** = VERIFIED (read in code / observed), **I** = INFERRED.

## 1. Architecture at a glance

| Piece | File (dist) | Source | Role |
|---|---|---|---|
| UserPromptSubmit hook | `dist/claude-hook.js` | `$SRC/claude-hook.ts` → `harness/hook-lifecycle.ts#L78-L89` → `core/hook.ts#L382-L460` | auto-inject (reflect / pages / recall / none), knowledge-page roster refresh, safety-net seed |
| SessionStart hook | `dist/claude-sessionstart-hook.js` | `core/session-start.ts#L346-L412` | skill sync, auto-update check, bank resolution, spawn deepen engine, banner (`systemMessage`), tool guide (`additionalContext`) |
| Stop hook | `dist/claude-stop-hook.js` | `core/retain-hook.ts#L177-L269` | read transcript → JSONL turns → retain (replace or append) + usage telemetry |
| Background engine | `dist/deepen.js` (spawned detached `node deepen.js --repo … --gitlog-limit N --harness claude-code`) | `$SRC/deepen.ts`, `core/seed.ts#L20-L57` | configure bank (import manifest), seed knowledge pages, git ingest, drain, settle |
| MCP server (stdio) | `dist/mcp-server.js` | `$SRC/mcp-server.ts`, `core/knowledge-tools.ts` | 8 `hindsight_*` tools |
| Installer CLI | `dist/installer.js` (bin `hindsight-coding-agents`) | `$SRC/installer.ts` | `install` / `uninstall` / `update` / `stats` |
| Survey | headless `claude -p` | `core/survey.ts` | cold-repo codebase survey (off for us) |
| Daemon | `uvx hindsight-embed` | `core/daemon.ts` | only `serverMode: "daemon"` (off for us) |

Every hook = fresh `node` process; state between invocations lives in temp files (`$TMPDIR/hindsight-claude-code/`). Every entry point first returns if `HINDSIGHT_DISABLE_HOOKS` is set (anti-recursion for the survey's own `claude -p`) — `core/hook.ts#L387`, `core/retain-hook.ts#L183`, `core/session-start.ts#L352` (V).

## 2. Hooks

### 2.1 Registration (what the installer writes) — V

`$SRC/harness/hook-lifecycle.ts#L222-L240`, `installer.ts#L210-L212` (`cmdHook`):

| Event | Command | Timeout (s) | Matcher |
|---|---|---|---|
| SessionStart | `node "<home>/.hindsight/coding-agents/dist/claude-sessionstart-hook.js"` | 30 | none (all sources: startup, resume, clear, compact) |
| UserPromptSubmit | `node "…/dist/claude-hook.js"` | 30 | — |
| Stop | `node "…/dist/claude-stop-hook.js"` | 60 | — |

Our `~/.claude/settings.json` (V): UserPromptSubmit timeout raised to 60; SessionStart wrapped by `python3 ~/.claude/hooks/memory-search.py plugin-sessionstart <node hook>` (timeout 35) which **replaces the plugin's `additionalContext` with our GUIDE** and keeps its `systemMessage` banner (`memory-search.py#L836-L852`).

### 2.2 SessionStart — `core/session-start.ts`

Input fields read: `cwd`, `session_id` (`hook-lifecycle.ts#L206-L211`). `source` is NOT read → runs identically on startup/resume/clear/compact (V).

Order (V, `#L357-L411`):
1. Parse stdin JSON; invalid → silent exit.
2. `loadConfig({harness:"claude-code"})` (env < file, then `harnesses.claude-code` section).
3. `syncCompanionSkill("claude-code")` — **re-copies the packaged `skill/` over `~/.claude/skills/hindsight-coding-agent/` whenever the installed `SKILL.md` differs** (presence-gated: only if it exists) — `core/skill-sync.ts#L33-L56`. Runs even before the `disabled` check. Our installed copy is byte-identical to the packaged one (V, `cmp`), i.e. any local edit there would be overwritten at next session start.
4. `disabled` → exit.
5. `maybeAutoUpdate(cfg)` (detached; see §9).
6. `sessionRootDir()` — first hook of a session writes `$TMPDIR/hindsight-claude-code/<session>.root` = cwd; later hooks resolve the bank from that root (#3563) — `core/session-cache.ts`.
7. Bank resolution `deriveBankIdOrSkip` + `applyBankConfig` (§5). Unresolvable → exit silently.
8. `ensureDaemon` (no-op unless `serverMode: "daemon"`; waits ≤12 s) — `core/daemon.ts#L232-L242`.
9. `buildSessionStartContext` (`#L140-L343`):
   - if `autoSeed !== false` and cwd has git history: `GET /documents?tags=source:git&tags_match=all_strict` (tri-state cold check). If reachable → **always spawn the deepen engine** (detached). If 0 docs → "cold": optional codebase survey (+ `survey-baseline:<sha>` marker retain, strategy `survey`); `diag seed_started`. Warm + `codebaseSurvey` → re-survey after `surveyRefreshCommits` (20) commits.
   - `GET /knowledge-base/tree` → page roster.
   - `additionalContext` = `buildKnowledgePreamble(pages)` (§7.2).
   - `deferInitialReflect` = cold OR roster known-empty → written to session cache so the FIRST prompt skips auto-inject.
   - Banner `systemMessage` (`buildSeedBanner`, `#L64-L72`): `"<🧠 Hindsight> is learning this repo — …"` (cold) / `"… is tracking the decisions, conventions and history of this repo"` (warm) + `"  ↳ memory bank “<id>” · <git note>"`. Plus legacy-plugin warning if `hindsight-memory@<marketplace>` is enabled in Claude's plugin registry (`core/legacy.ts#L26-L82`).
   - `diag session_start {bank,cold,pages,ms}`.
10. Output (only if banner or context): `{"systemMessage": …, "hookSpecificOutput": {"hookEventName":"SessionStart","additionalContext": …}}`.
11. Whole body in try/catch — never throws, always exit 0.

**Quirk on our config (V)**: with `gitIngest: "none"` the bank never gets a `source:git` document, so the cold check is *permanently* true → banner always says "is learning this repo", `seed_started` + `reflect_deferred_new_bank` logged every session (29/29 in `diag.jsonl`), deepen spawned every session. `session_start` records show `cold:true, pages:5`.

### 2.3 UserPromptSubmit — `core/hook.ts`

Input: `prompt`, `cwd`, `session_id` (`hook-lifecycle.ts#L80-L84`). Empty prompt → exit. Budget: host timeout 30 s; `reflectTimeoutMs` default 20 000 ms; fallback chain 7 000 ms (`HOOK_FALLBACK_BUDGET_MS`, `#L91`); each HTTP request hard-capped at 15 s (`hindsight.ts#L372-L393`).

Flow (`buildHookOutput`, `#L196-L379`), state in `$TMPDIR/hindsight-claude-code/<session>.json` (`{turns, reflectAnswer, reflectAttempts, deferInitialReflect, pages:{atTurn,list}}`):

1. Safety-net: no cache file yet (first prompt) and `autoSeed` → spawn deepen (`#L444-L446`).
2. Auto-inject, once per session, per `autoInject`:
   - `reflect` (default): `POST /reflect {query: buildReflectQuery(prompt), budget:"low"}` with `reflectTimeoutMs`. On timeout/5xx → fallback: knowledge-page search, then recall (`#L165-L178`).
   - `pages`: `GET /knowledge-base/search?q=<prompt[:500]>&limit=<pageSearchLimit>`.
   - `recall`: `POST /memories/recall {...recallOptions, query: prompt[:2000]}` (default `types:["observation"], budget:"low", max_tokens:2000, include:{entities:null}` — `hindsight.ts#L254-L259`).
   - `none`: nothing.
   - Failure retried on next turn, max 2 attempts (`HOOK_INJECT_ATTEMPTS`); `null` (ran, empty) resolves the session.
   - `deferInitialReflect` marker skips turn 1 and is consumed.
3. Page roster refresh when stale (`pageRefreshEveryTurns`, default **1**): `GET /knowledge-base/tree`.
4. Injection blocks: `<hindsight_memory>…</hindsight_memory>` (calibrated wrapper, `inject.ts#L74`) only on the turn the source ran; `<hindsight_knowledge_refresh>` roster+tool guide when `turns % cadence == 0`.
5. User notice (`systemMessage`) only on a turn that ran a source: `"🧠 Hindsight · goal: recall this repo's past decisions about “<48 chars>”\n↳ <140-char preview>"`, or on failure `"… · no memory this turn — see <diag path>"`.
6. Mid-session heal: roster empty → spawn deepen again.
7. Output `{systemMessage?, hookSpecificOutput:{hookEventName:"UserPromptSubmit", additionalContext}}` only if something to say.

**With our config (V by code)**: `autoInject:"none"` → no reflect/recall; `pageRefreshEveryTurns: 100000` → roster fetched on turn 1 only and the refresh block injected only at turn 100 000 (never). Net effect per prompt: one `GET /knowledge-base/tree` on turn 1 + cache write. The hook is almost inert; the visible recall comes from our `memory-search.py`.

### 2.4 Stop — `core/retain-hook.ts` + `core/chat.ts`

Input: `session_id`, `transcript_path`, `cwd` (`hook-lifecycle.ts#L234-L238`). `stop_hook_active` is not read. No stdout. Deadline = spawn + 60 s − 2 s margin.

1. config / disabled / bank / `retainSessions` gates (`#L201-L236`).
2. `readClaudeTranscript(transcript_path)` (§3).
3. `recordUsage` → `usage.jsonl` (§8).
4. `retainLiveSession` (§4) under document id `conversation:<session_id>`.
5. `diag retain_ok {ms,turns,session}` / `retain_failed`. 85 `retain_ok` in our diag (V).

Fires on every assistant turn end (Stop), so the session document is rewritten/extended turn by turn; there is no SessionEnd/PreCompact/SubagentStop hook. Subagent work is excluded (sidechain lines dropped).

## 3. Transcript reading (Claude Code JSONL) — `core/transcript.ts`, `core/transcript-util.ts`, `core/jsonl.ts` (V)

- Streams the file in 64 KiB chunks, whole file every time (`jsonl.ts#L31-L65`); a tail window was removed because it broke the append cursor (#4380).
- Keeps only lines with `type ∈ {user, assistant}`; drops `isMeta === true`, `isSidechain === true`, `isCompactSummary === true`, lines without object `message` (`transcript.ts#L97-L106`). Role taken from `type`, not `message.role`. Keeps `timestamp`.
- `message.content` string → one turn. Array → `text` blocks joined with `\n` into one prose turn; **each `tool_use` → separate `{role:"action", content:"<Tool> <target>"}`** where target = first non-empty string among input keys `file_path, path, notebook_path, command, pattern, query, url, name, id`, first line only, capped 100 chars (`transcript-util.ts#L32-L68`). `tool_result` and `thinking` blocks dropped entirely (no tool output retained). `TOOL_TEXT_CAP = 2000` exists but is unused by the Claude reader.
- Strips tag blocks (kept surrounding text): `hook_prompt, task-notification, system-reminder, hindsight_memory, hindsight_memories, hindsight_bank, relevant_memories, user_feedback, hindsight_knowledge, hindsight_knowledge_refresh` (`transcript-util.ts#L20-L25`). Empty turns dropped.
- Hook `additionalContext` (ours and the plugin's) is persisted as a separate record `{"type":"attachment","attachment":{"type":"hook_additional_context",…}}`, not inside the user message (V, current session transcript `~/.claude/projects/-home-user-Workspace-github-com-Barben360-ref-stack/88cf850f-….jsonl`), so the `type ∈ {user, assistant}` filter already excludes it; the tag stripping is a second line of defence (tool results quoting injected text are already dropped as `tool_result`).
- No chunking or condensing client-side: chunking is server-side via the strategy's `retain_chunk_size` (12 000 for `conversation`), JSONL lines being atomic units (`chat.ts#L24-L35`).

## 4. Retain API calls and payloads (V)

All retains go through `HindsightClient.retain` (`hindsight.ts#L397-L428`):

```
POST {apiUrl}/v1/default/banks/{bank}/memories
Authorization: Bearer <apiToken>     (if set)
{ "items": [ { "content", "context", "document_id", "tags", "strategy",
               "observation_scopes", "timestamp"?, "metadata"?, "update_mode"? } ],
  "async": true, "operation_id"? }
```
Always `async: true`; returned `operation_id` collected for `drain()`. 429 → `RateLimitedError(Retry-After)`; 401 → re-read token from live config and replay once (`#L351-L356`).

### 4.1 Live session write-back (`chat.ts#L204-L353`)

| Field | Value |
|---|---|
| document_id | `conversation:<session_id>` (`no-session` fallback) |
| content | JSONL, line 1 `{"role":"system","content":"REF-ID: conversation:<sid>","timestamp":<startTs>}`, then one `{role,content,timestamp?}` per turn |
| context | `"coding agent session"` |
| strategy | `"conversation"` |
| tags | `[...retainTags resolved, "source:chat", "harness:claude-code"]` deduped; `retainTags` entries starting `source:`/`harness:` are refused (`retain-stamp.ts`) |
| metadata | `{...retainMetadata resolved, source:"chat", session_id, ref_id, harness:"claude-code"}` (built-ins win) |
| timestamp | first turn's timestamp |
| observation_scopes | config value (`"shared"` default; `per_source` expanded client-side to `[[], ["source:…"]…]`) |
| update_mode | `"append"` on append path |
| operation_id | `uuidV5(bank \n refId \n mode \n content)` — deterministic, so resubmits collapse server-side |

Replace vs append (`retain-cursor.ts`): cursor file `$TMPDIR/hindsight-claude-code/<sid>.retain.json` `{turns, fingerprint(sha1 of first N turns), bank, appendSupported, dirty, pending[]}`. Append only if server `GET /version` → `api_version ≥ 0.8.6` (cached on cursor), cursor clean, same bank, prefix fingerprint matches, pending outbox < 12 h and < 256 KiB; else full replace. Outbox entries written before the request (crash-safe replay). Retry on 429 at most 2×, only if the wait + 15 s fits before the host deadline and the write is still newest. Our live cursor: `turns 119, appendSupported true, pending 0` (V).

### 4.2 Other retains

| Caller | document_id | strategy | tags | context |
|---|---|---|---|---|
| MCP `hindsight_ingest_document` (`knowledge-tools.ts#L346-L390`) | ASCII slug of title; non-ASCII/empty → `<slug|doc>--<sha256(title)>` | `document` | stamp + `source:upload` + `harness:claude-code` | `"ingested document"` |
| MCP `hindsight_capture_initiative` marker (`hindsight.ts#L983-L1033`) | `initiative-marker-<slug>-<ms>` | `document` | stamp + `knowledge:feature-work` | `initiative marker for [[page:<id>]]`, metadata `relatedPageId` |
| Survey baseline | `survey-baseline:<sha>` | `survey` | stamp + `source:survey-baseline` (+`survey-state:done`) | `hindsight codebase-survey baseline` |
| git message seed | `gitlog:<repoName>` (tag `gitlog-head:<sha>`) | `gitlog` | stamp + `source:git`, `source:git-log` | — (`git.ts#L206-L276`) |
| git full diff | `git:<sha>` | `git` | stamp + `source:git` | — (`git.ts#L71-L98`) |
| history import (`install --import-conversations`) | `chat:<id>` | `conversation` | stamp + `source:chat` | `"developer chat"` (`chat.ts#L38-L100`) |

## 5. Bank resolution and creation

### 5.1 Resolution — `core/bank.ts` (V)
Order: `mapPathToBank` longest prefix → static `bankId` if set and `dynamicBankId` not true → template (default `coding-agent::{gitProject}`; placeholders `{gitProject} {project} {harness} {channel} {user}`). `{gitProject}` = basename of the main worktree root (git common dir), candidates cwd → session root → `$CLAUDE_PROJECT_DIR`; git probe failure inside a repo → refuse (`BankResolutionError`, hook goes silent). Then `banks.<id>` override section (can disable or rename, single hop). `optInOnly`/`optInPaths` gate. Ours: static `coding-agent::memoire`.

### 5.2 Bank config applied — `configureBank` (`hindsight.ts#L494-L530`) + `codingBankManifest` (`missions.ts#L677-L726`)
Run by deepen at every session start. `GET /banks/{id}/config` → overrides; then `POST /banks/{id}/import` with ONLY missing fields (additive, never overwrites, #3927); this POST also creates the bank:
- `reflect_mission`, `observations_mission`, `retain_mission` (seeded once as a group), `enable_observations: true`, `retain_extraction_mode` = `retainExtractionMode` (default `concise`);
- `retain_default_strategy: "git"`, `entities_allow_free_form: true`;
- `retain_strategies` {`git`, `gitlog` (chunk 12000), `conversation` (chunk 12000), `document` (chunk 12000), `survey` (custom instructions: markers → no facts)} merged per entry; extraction mode of the plugin's own strategies re-synced to config on drift (#4560);
- `entity_labels` += group `knowledge` (multi-values, `tag:true` → emits `knowledge:<feature-work|decision|convention|component|concept>` tags).
- Mission texts: `missions.ts#L12-L73`. `manageBankConfig:false` skips all of this.
- Live: `"[bank] coding-agent::memoire already carries the coding structure — nothing to apply"` (V, plugin.log).

## 6. Knowledge pages

| Aspect | Behaviour (V) | Source |
|---|---|---|
| Creation | deepen → `seedPages`: `GET /knowledge-base/tree`; for each taxonomy page missing by name → `POST /knowledge-base/pages {name, source_query, tags, max_tokens:4096, trigger}` (409 tolerated) | `hindsight.ts#L805-L926` |
| Taxonomy | 5 pages: Component map (`knowledge:component`), Core concepts (`knowledge:concept`), Conventions and patterns (`knowledge:convention`), Key decisions and rationale (`knowledge:decision`), Initiatives and enhancements (`knowledge:feature-work`) + `pageScopeRule(project or bank)` clause | `missions.ts#L243-L286` |
| Customisation | `pages` (disable / own `source_query`), `customPages` (extra pages) | `config.ts` |
| Re-sync | existing page whose `description` ≠ configured `source_query` or whose trigger drifted → `PATCH /knowledge-base/nodes/{id}` (overwrites edits made in the UI). Live log today: `0 created, 5 re-synced` at 18:44 | `hindsight.ts#L860-L913` |
| Trigger | `{fact_types:["world","experience","observation"], tags_match:"all", refresh_cron:"H * * * *" (hashed per page) | refresh_after_consolidation | manual}` per `pageTriggerType` | `missions.ts#L354-L610` |
| Initiatives | MCP `capture_initiative`: ensure root folder "Initiatives" (`POST /knowledge-base/folders`), `POST /knowledge-base/pages` with `parent_id`, then marker retain | `hindsight.ts#L983-L1060` |
| Injection | NOT auto-injected as content. SessionStart preamble states the page COUNT (titles deliberately not listed) and the tool guide; `autoInject:"pages"` injects search snippets on turn 1 | `knowledge-injection.ts#L21-L145` |
| Refresh by plugin | none (server-side triggers only) | — |

## 7. Text the plugin injects

### 7.1 Banner — §2.2. Brand word `🧠 Hindsight` with ANSI gradient (`core/brand.ts`).
### 7.2 Tool guide (`<hindsight_knowledge>`, `knowledge-injection.ts#L55-L131`)
~2 KB: when to call `hindsight_search_knowledge_pages` (bug report, writing tests, new work, "why", before commit), mandatory credit line `> 🧠 **From Hindsight memory (<page>)** — …`, list/read pages, reflect for the WHY, capture_initiative after plan approval, ingest_document for notes and `Correction: <topic>` docs; when `autoInject ≠ reflect` adds "new goal → search pages first". On our machine this text is replaced by our GUIDE (memory-search.py wrapper).
### 7.3 `<hindsight_memory>` wrapper (`inject.ts#L74-L112`) and reflect query (`inject.ts#L10-L40`).

## 8. Logging / telemetry (V)

| File | Content | Source |
|---|---|---|
| `~/.hindsight/coding-agents-logs/plugin.log` | leveled text log (`logLevel`, `HINDSIGHT_LOG_LEVEL`), rotated at 10 MiB → `.1`, mode 0600 | `core/log.ts` |
| `~/.hindsight/coding-agents-logs/diag.jsonl` | one JSON per outcome: `session_start, seed_started, reflect_ok/empty/failed, inject_pages, pages_ok, retain_ok/failed, deepen_*, bank_unresolved, …` (`HINDSIGHT_DIAG_FILE`) | `core/diag.ts` |
| `~/.hindsight/coding-agents-logs/usage.jsonl` | per user turn `{harness, session, bank, turn, calls:[hindsight_* tools], credited}`; credit = regex `/from hindsight memory/i` on assistant text; previous turn re-emitted (`reviseLastTurn`) | `core/usage.ts#L1-L60,#L104` |
| `$TMPDIR/hindsight-claude-code/<sid>.{json,root,retain.json,usage.json}` | session cache, root, retain cursor, usage cursor | `core/session-cache.ts` |
| `$TMPDIR/hindsight-coding-agent/deepen-<bank>.lock` | per-bank lock `{pid,ts}`, stale after 30 min or dead pid | `deepen.ts#L98-L128` |
`installer stats` prints a usage report from `usage.jsonl`.

## 9. Update mechanism (`core/auto-update.ts`) (V)

- At SessionStart, at most once per 24 h (`.update-check.json` in the runtime dir), only if the runtime was staged from an npx cache (`.install-origin.json`), and `npm`+`npx` on PATH: `npm view @vectorize-io/hindsight-coding-agents version --json` (5 s budget) → if newer, spawn detached `npx @vectorize-io/hindsight-coding-agents@<v> update` which re-stages `~/.hindsight/coding-agents/dist` (rm + cp) without touching host configs. A new hook entry point still needs a manual `install`. Lock against concurrent updaters. No signature verification (npm registry trust only).
- `autoUpdate:false` / `HINDSIGHT_AUTO_UPDATE=false` disables. Ours: false.

## 10. Installer (`npx @vectorize-io/hindsight-coding-agents install claude-code`) (V, `installer.ts`)

1. `stageRuntime`: copy package `dist/`, `skill/`, `package.json`, `plugin.json`, `hooks/`, `index.js` to `~/.hindsight/coding-agents/` (dist replaced wholesale), write `.install-origin.json` (`#L835-L892`).
2. `configureServer`: writes `~/.hindsight/coding-agent.json` `serverMode`/`apiUrl`/`apiToken` (flags `--server cloud|self-hosted|daemon`, `--api-url`, `--api-token`; interactive picker; adopts legacy `~/.hindsight/claude-code.json` endpoint; non-interactive default Cloud) (`#L996-L1082`).
3. `~/.claude/settings.json`: read, `hooks.<Event>` arrays: drop every entry whose JSON contains the substring **`"coding-agents"`** (`MARKER`, `#L62`), append ours; first touch backs up to `settings.json.hindsight-backup`; rewritten with `JSON.stringify(…, 2)` (`#L138-L190, #L545-L553`).
4. `~/.claude/skills/hindsight-coding-agent/` ← packaged skill (`#L290-L298`).
5. `claude mcp remove --scope user hindsight` then `claude mcp add --scope user hindsight --env HINDSIGHT_MCP_HARNESS=claude-code -- node <runtime>/dist/mcp-server.js` (→ `~/.claude.json` `mcpServers.hindsight`) (`#L556-L586`). Our entry matches (V).
6. Optional `--import-conversations`: reads `~/.claude/projects/<cwd with non-alnum → '->/*.jsonl` for the current repo, retains `chat:<id>` docs (`core/history.ts#L62, #L169`).
- `uninstall`: strips the same marker entries, `claude mcp remove`, removes skill. `update`: stage only.
- **Hazard (I)**: our wrapper command `python3 … memory-search.py plugin-sessionstart "/home/user/.hindsight/coding-agents/dist/claude-sessionstart-hook.js"` contains `coding-agents`, so a re-`install` or `uninstall` would delete it.
- The package's own `plugin.json`/`hooks/hooks.json` (copied to `~/Downloads/hindsight-setup-20261023/plugin/`) are the **Dcode Agent Plugin** manifest (`${PLUGIN_ROOT}`, `HINDSIGHT_MCP_HARNESS=dcode`), not a Claude Code plugin; there is no Claude Code marketplace for this package. An older Vectorize Claude Code plugin `hindsight-memory@<marketplace>` existed and is detected as "legacy" (`core/legacy.ts#L26`).

## 11. MCP server (`mcp-server.ts`, `knowledge-tools.ts`) (V)

Stdio, `@modelcontextprotocol/sdk` ^1.29 `McpServer({name:"hindsight", version:"0.1.0"})`. Requires env `HINDSIGHT_MCP_HARNESS` (refuses to start without it). Bank resolved from process cwd (or `HINDSIGHT_MCP_PROJECT_CWD`) exactly like hooks, ONCE at startup. `disabled` → empty tool list. Token re-read from config on 401. Results = JSON text; errors → `isError:true`. Annotations: reads `readOnlyHint:true`, writes `readOnlyHint:false, destructiveHint:false`.

| Tool | Params | Endpoint(s) | Notes |
|---|---|---|---|
| `hindsight_sync_status` | — | `GET /documents?tags=source:git…`, `source:chat`, `source:upload`, `source:survey-baseline`, tree, `GET /operations` | `synced` boolean |
| `hindsight_diagnose` | — | none | bank, harness, workspace, config path, api_url, booleans about token; never the token |
| `hindsight_search_knowledge_pages` | `query` | `GET /knowledge-base/search?q=&limit=10` | returns `{pages:[{page,page_id,snippet}], crediting}`; score hidden on purpose |
| `hindsight_list_knowledge_pages` | — | `GET /knowledge-base/tree` (flattened) | `{items:[{id,name,description?,folder?}]}` |
| `hindsight_read_knowledge_page` | `page_id` | `GET /knowledge-base/pages/{id}` | shaped `{id,name,description,tags,last_updated_at,body}` |
| `hindsight_reflect` | `query` | `POST /reflect {query, budget: reflectBudget (high)}` timeout `reflectToolTimeoutMs` (330 s) | |
| `hindsight_capture_initiative` | `title, summary, relates_to_page_id?` | folders, `POST /knowledge-base/pages`, retain | §6 |
| `hindsight_ingest_document` | `title, content` | `POST /memories` | §4.2 |

Not exposed: raw recall, document delete, bank admin. (Our separate `user-memory` MCP provides `memory_search` via `hindsight-memory-mcp.py`.)

## 12. Config keys (`~/.hindsight/coding-agent.json`, `core/config.ts#L50-L265, #L519-L627`) (V)

Layering: env `HINDSIGHT_<SCREAMING_SNAKE>` (fallback) < file (`HINDSIGHT_CONFIG` overrides path) < `harnesses.<name>` < (after bank resolution) `banks.<id>`. A "project-local config" is mentioned in comments (`#L630-L637`) but `loadConfig` reads only the global file in 0.7.0 (V).

| Key | Default | Effect | Ours |
|---|---|---|---|
| serverMode | `cloud` | cloud / self-hosted / daemon (daemon → `http://127.0.0.1:{apiPort}`) | self-hosted |
| apiUrl | `https://api.hindsight.vectorize.io` | base URL | `http://127.0.0.1:8888` |
| apiToken | — | Bearer | set |
| apiPort / daemonIdleTimeout / daemonProfile / embedVersion / embedPackagePath | 9077 / none / `coding-agent` / latest / — | daemon only | — |
| bankId / dynamicBankId / bankIdTemplate / mapPathToBank / resolveWorktrees | — / dynamic iff no bankId / `coding-agent::{gitProject}` / — / true | §5 | `coding-agent::memoire`, false |
| optInOnly / optInPaths | false / [] | gate | — |
| harness | asking harness | template + backfill | — |
| disabled | false | kill switch | false |
| retainSessions | true | Stop write-back + history import | — |
| maxParallelRetains | 10 | concurrency | — |
| reflectTimeoutMs | 20000 | hook reflect | 45000 |
| reflectToolTimeoutMs | max(330000, reflectTimeoutMs) | MCP reflect | — |
| reflectBudget | high | MCP reflect | — |
| autoInject (autoReflect deprecated) | reflect | §2.3 | none |
| pageSearchLimit | 10 (comment says 3; code 10) | search hits | — |
| recallOptions | see §2.3 | recall body | — |
| pageRefreshEveryTurns | 1 (comment says 10; code 1) | roster/guide cadence | 100000 |
| pageTriggerType / pageTriggerCron | cron / `H * * * *` | page refresh policy | manual |
| pages / customPages | {} | page taxonomy overrides | — |
| autoSeed | true | deepen spawn at SessionStart + prompt safety net | (true) |
| seedLimit | 300 | gitlog commit cap | 100 |
| codebaseSurvey / surveyModel / surveyBudgetUsd / surveyRefreshCommits | true / haiku / 2 / 20 | headless `claude -p` survey | false |
| gitIngest | message | none / message / full | none |
| retainTags | [] | extra tags (placeholders `{gitProject} {project} {harness} {bankId} {sessionId} {timestamp} {channel} {user}`) | `project:{gitProject}` |
| retainMetadata | {} | extra metadata, same placeholders | `{session:"{sessionId}"}` |
| manageBankConfig | true | §5.2 | — |
| retainExtractionMode | concise | concise / verbose / verbatim / chunks | — |
| observationScopes | shared | shared / combined / per_tag / all_combinations / per_source / string[][] | combined |
| logLevel | info | log | — |
| autoUpdate | true | §9 | false |
| harnesses / banks | {} | overrides | — |

## 13. Hard-to-reimplement or subtle parts

| Item | Why it is subtle | Size |
|---|---|---|
| Append cursor + outbox + deterministic `operation_id` (§4.1) | crash/kill safety across fresh processes, server-version gate (≥0.8.6), fingerprint of prefix, replay rules | ~350 LOC TS + tests |
| Transcript normalisation (§3) | which record types/flags to drop, tag stripping list, action-line compaction; must track Claude Code transcript format changes | ~200 LOC |
| Additive bank manifest merge (§5.2) | per-entry merge of `retain_strategies`, never overwriting user edits, extraction-mode re-sync | ~60 LOC |
| Mission / page query texts | prompt engineering with incident history (#3476, #4560, #3379…) — content to copy or rewrite, not code | text |
| Page seed + re-sync + hashed cron | drift detection, tolerance of old servers, `H` cron expansion | ~150 LOC |
| Capability probing (404 "Not Found" vs bank-missing, 401 token refresh) | server-version quirks | ~60 LOC |
| Everything else (hooks plumbing, MCP tools, config) | straightforward | — |

## 14. Parity checklist

Our config: `autoInject none`, `pageTriggerType manual`, `dynamicBankId false` (static `coding-agent::memoire`), `retainTags project:{gitProject}`, `observationScopes combined`, `gitIngest none`, `codebaseSurvey false`, `autoUpdate false`, `reflectTimeoutMs 45000`, `pageRefreshEveryTurns 100000`.

| Feature | Used in our config? | Must reimplement / can drop |
|---|---|---|
| Stop write-back of transcript, `conversation:<sid>`, strategy `conversation`, tags `project:<x>`,`source:chat`,`harness:claude-code`, metadata `session` | YES — the core value (85 retain_ok) | **Must** (keep doc-id scheme and tags for continuity with existing bank) |
| Append mode + cursor + operation_id | YES (server 0.10.1, cursor `appendSupported:true`) | **Must** (or accept full replace each turn = re-extraction cost) |
| Transcript filtering / action lines / tag stripping | YES | **Must** |
| SessionStart banner (`systemMessage`) | YES (shown via our wrapper) | Must (rewrite: current text is wrong under gitIngest none — always "learning") |
| SessionStart tool guide (`additionalContext`) | NO — replaced by our GUIDE | Drop plugin text; keep the mechanism for our own guide |
| Auto-inject reflect / pages / recall on first prompt | NO (`none`); our recall is memory-search.py | Can drop (our recall hook replaces it); keep `recall` endpoint knowledge |
| Roster refresh per turn | NO (100000) | Drop |
| Deepen engine: bank import manifest (missions, strategies, entity labels) | YES (idempotent no-op today) | **Must** at install/upgrade time (deterministic `configure-bank` command), not per session |
| Deepen engine: page seeding / re-sync | YES (5 pages exist, re-synced today) | Should (one-shot at install/upgrade; manual trigger) |
| git ingest (gitlog / full diff) | NO (`none`) | Drop for v1 (or optional later) |
| Codebase survey (`claude -p`) | NO | Drop |
| Conversation history import (`--import-conversations`) | NO (not used) | Can drop; possibly useful for migration (C13) |
| Daemon mode (`hindsight-embed` via uvx) | NO | Drop (our binary manages the server) |
| Cloud mode | NO | Optional (remote mode C10) |
| MCP `hindsight_ingest_document` | YES (usage.jsonl shows calls; CLAUDE.md "Correction:" flow) | **Must** |
| MCP `hindsight_capture_initiative` | Listed in tool guide; rarely used (I) | Should (cheap) |
| MCP `hindsight_list/read/search_knowledge_pages` | Read used; search de-emphasised by our GUIDE | Should (read/list); search optional |
| MCP `hindsight_reflect` | Available | Should |
| MCP `hindsight_sync_status` | git-centric, meaningless under gitIngest none | Drop / replace by our `doctor` |
| MCP `hindsight_diagnose` | Rare | Replace by our `doctor` |
| Usage telemetry (`usage.jsonl`, credit regex) | YES (feeds stats, C12) | Should (merge with memory-search.py telemetry) |
| diag.jsonl / plugin.log | YES | Must (own format) |
| Companion skill sync to `~/.claude/skills/hindsight-coding-agent` | YES (73 KB skill, overwritten each session) | Replace by our own skill; drop the overwrite behaviour |
| Auto-update via npm | NO (`autoUpdate false`) | Drop (own signed self-update, C11) |
| Legacy `hindsight-memory` plugin warning | NO | Drop |
| `banks.<id>` overrides, `mapPathToBank`, opt-in, templates | NO (single static bank) | Can drop for v1 (single bank + project tag model) |
| `{gitProject}` resolution (worktree-aware, `$CLAUDE_PROJECT_DIR` fallback) | YES (tag `project:{gitProject}`) | **Must** (same naming as memory-search.py `git_project`) |
| 401 token refresh / 429 handling | YES (implicitly) | Must |
| Multi-harness support (14 other agents) | NO | Drop |

## Implications for hindsight-claude

- The source is available and readable (TS, ~10 k LOC core excl. tests); everything the Claude Code path does maps to ~15 HTTP endpoints of the Hindsight API (`/memories`, `/memories/recall`, `/reflect`, `/documents`, `/operations`, `/config`, `/import`, `/knowledge-base/{tree,search,pages,nodes,folders}`, `/version`).
- Under our config, the plugin's live value reduces to: Stop write-back (with append cursor), the banner, bank/strategy/page bootstrap, and 4–5 MCP tools. Auto-inject, roster, git ingest, survey, daemon, auto-update and 14 harnesses are dead code for us.
- The only genuinely delicate logic is the retain cursor/outbox/operation-id scheme and transcript normalisation; both are deterministic and testable from fixtures (the upstream test files `retain-cursor.test.ts`, `transcript.test.ts`, `retain-hook.test.ts` document the edge cases).
- Continuity constraints if the existing bank is kept: same `document_id` scheme (`conversation:<sid>`, slug ids for ingested docs), same tags (`source:chat`, `harness:claude-code`, `project:<x>`), same strategy names (`conversation`, `document`), same mission texts unless deliberately changed.
- Per-session work done today (deepen spawn, bank config GET, page tree read + PATCH re-sync) is idempotent but runs every session and re-syncs page queries over UI edits; a deterministic install/upgrade-time `configure` step would remove that.
- The banner text is currently misleading for `gitIngest: none` (perpetual "learning"); the plugin's skill sync silently overwrites `~/.claude/skills/hindsight-coding-agent` and its installer deletes any hook entry containing `coding-agents` — both are hazards during migration.
- No signature verification anywhere in the npm update path.
