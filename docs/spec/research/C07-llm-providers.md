# C07 — Hindsight LLM providers (checked 2026-10-23)

Legend: **V** = VERIFIED, **I** = INFERRED. Code refs are in container `hindsight` (0.10.1) unless a URL is given; "docs" = https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/docs/developer/configuration.mdx (cfg) and `.../models.mdx` (models), main branch = 1.1.0.

## Common configuration (V, `/app/api/hindsight_api/config.py` l.159-162, 381-453, 1084-1119)

- `HINDSIGHT_API_LLM_PROVIDER` (default `openai`), `_API_KEY`, `_MODEL` (default = per-provider table below, fallback `gpt-4o-mini`), `_BASE_URL`, `_MAX_CONCURRENT` (32), `_TIMEOUT` (120 s), `_MAX_RETRIES` (3), `_REASONING_EFFORT`, `_TEMPERATURE` (`none` omits), `_EXTRA_BODY`, `_DEFAULT_HEADERS`, `_STRICT_SCHEMA` (+ `_RETAIN/_REFLECT/_CONSOLIDATION` variants).
- 0.10.1 accepts 31 provider ids (`llm_wrapper.py` l.960-1000): `openai openai-responses groq ollama ollama-cloud gemini anthropic lmstudio llamacpp vertexai openai-codex claude-code cursor github-copilot minimax deepseek litellm litellmrouter bedrock volcano openrouter requesty zai opencode-go atlas fireworks nous xai-oauth meta mock none`. 1.1.0 adds `chatgpt` (Sign in with ChatGPT). V
- LLM keys and base URLs are `_CREDENTIAL_FIELDS`: server-level only, never per-bank, never returned by the config API (`config.py` l.3584-3640). V

## Provider matrix

Structured output (SO) paths (V, code): **native-schema** = provider-side schema enforcement; **tool-forced** = schema sent as a forced tool; **soft** = schema appended to prompt (+ `json_object` when supported), strict `json_schema` only when `*_STRICT_SCHEMA=true` (`openai_compatible_llm.py` l.1196-1233). Tools = reflect's tool loop.

| Provider id | Auth / env | Default model (0.10.1) | Default base URL | SO | Tools | Notable quirks | |
|---|---|---|---|---|---|---|---|
| `openai` | `LLM_API_KEY` | `gpt-4o-mini` | OpenAI | soft (`json_object`); strict opt-in | yes | Generic **OpenAI-compatible** lane via `LLM_BASE_URL` (vLLM, llama-server, Azure `/openai/v1`, xAI API…); batch API (opt-in `RETAIN_BATCH_ENABLED`, async retain only) | V |
| `openai-responses` | key | `gpt-5.6` | OpenAI `/v1/responses` | `text.format` | yes, with reasoning | For reasoning models that reject `reasoning_effort` + tools on chat/completions | V |
| `anthropic` | key, or Workload Identity Federation (1.1.0) | `claude-haiku-4-5` | Anthropic; **custom `LLM_BASE_URL` passed to `AsyncAnthropic(base_url=…)`** | tool-forced (`tool_choice` named tool) | yes | Adds `cache_control` ephemeral breakpoints on system + last block; Message Batches API supported (`supports_batch_api → True`) — docs capability table omits it | V `anthropic_llm.py` l.62-92, 200-224, 314-340, 715-717 |
| `claude-code` | Claude Pro/Max via Claude Agent SDK; ≥0.10.3: `LLM_API_KEY = sk-ant-oat01-…` from `claude setup-token`, passed as `CLAUDE_CODE_OAUTH_TOKEN` | `claude-sonnet-4-5-20250929` | n/a (spawns CLI) | soft (schema in prompt, "ONLY the JSON") | yes, via in-process SDK MCP server, `max_turns=1` | Ignores temperature/max tokens/reasoning effort; one CLI subprocess per call; see §(a) | V `claude_code_llm.py` |
| `gemini` / `vertexai` | key / project+region+ADC or SA key | `gemini-3.5-flash` / `google/gemini-3.1-flash-lite` | Google | native schema | yes | Explicit prompt caching (`CachedContent`, on by default); safety settings; Flex tier | V docs + llmProviders.json |
| `groq` | key | `openai/gpt-oss-120b` | api.groq.com | soft | yes | Batch API; service tier `auto/flex/on_demand` | V |
| `deepseek` (reference install) | key | `deepseek-v4-flash` | api.deepseek.com | soft | yes | v4-flash runs in thinking mode, handled transparently; `deepseek-chat` = non-thinking alias | V cfg l.485-495 |
| `openrouter` / `requesty` | key | `qwen/qwen3.5-9b` / `openai/gpt-4o-mini` | gateways | soft; OpenRouter sends `require_parameters=true` | yes | Models rejecting `temperature` (e.g. `anthropic/claude-haiku-5.5`) fail unless `LLM_TEMPERATURE=none`; some upstreams reject forced `tool_choice` | V cfg l.462-479 |
| `ollama` | none (`localhost:11434/v1`) | `gemma3:12b` | localhost | **native `/api/chat` with schema** for SO calls | yes | No default `num_ctx` (set `LLM_OLLAMA_NUM_CTX`); gpt-oss needs `EXTRA_BODY={"think":"low"}`; skips `json_object` | V `openai_compatible_llm.py` l.1128-1142, 1226; cfg l.325-334 |
| `ollama-cloud` | key | `gemma3:12b` | ollama.com/v1 | as ollama | yes | — | V |
| `lmstudio` | none (`localhost:1234/v1`) | `local-model` | localhost | soft, **no** `json_object` (schema in prompt only) | yes | Least enforced SO path | V l.1226 |
| `llamacpp` | none | `gemma-4-e2b-it` (auto-downloads ~3.5 GB GGUF) | spawns `llama-cpp-python` server | soft + JSON grammar (unless `LLAMACPP_NO_GRAMMAR`) | yes | **`llama_cpp` not in the Docker image** (`ModuleNotFoundError`); needs `local-llm` extra or upstream `docker/docker-compose/local-llm/`. An external `llama-server` works through `openai` + base URL | V container import test; models doc |
| `litellm` / `litellmrouter` / `bedrock` | per LiteLLM / Router JSON / AWS creds | `gpt-4o-mini` / config / `us.amazon.nova-2-lite-v1:0` | — | `response_format` or forced tool (`STRUCTURED_OUTPUT_FORCED_TOOL`) | yes | litellm present in image | V |
| `minimax` `zai` `opencode-go` `atlas` `meta` `volcano` `fireworks` `nous` | key (nous: `~/.hermes/auth.json` JWT) | `MiniMax-M3`, `glm-4.5-flash`, `deepseek-v4-flash`, `deepseek-ai/deepseek-v4-pro`, `muse-spark-1.3`, `doubao-pro-32k`, llama-v3p1-8b, `deepseek/deepseek-v4-flash` | provider defaults (`llm_wrapper.py` l.1001-1027) | soft | yes | Meta Muse always reasons → raise reflect timeouts; fireworks batch | V |
| `openai-codex` / `chatgpt` (1.1.0) | ChatGPT Plus/Pro OAuth (`~/.codex/auth.json`, `CODEX_HOME`) / `hindsight-admin chatgpt login` | `gpt-5.4-mini` | — | — | yes | Subscription lane like claude-code | V cfg l.423-435, l.317-318 |
| `cursor`, `github-copilot`, `xai-oauth` | CLI/OAuth logins | `auto`, `gpt-5.6-terra`, `grok-4.5` | — | cursor: emulated SO via prompt | cursor: emulated | cursor turns 15-30 s → raise timeouts | V cfg l.442-454 |
| `none` | — | — | — | — | — | Chunk storage + semantic search only, no extraction | V cfg l.560 |

## (a) `claude-code` from inside a container

| Fact | Source | |
|---|---|---|
| Image already bundles the Claude Agent SDK 0.2.87 **and the Claude Code CLI 2.1.150** (238 MB binary) + node; no host CLI install needed | `.../claude_agent_sdk/_bundled/claude`, `_cli_version.py`, `which node` | V |
| Each call spawns the CLI with `CLAUDE_CONFIG_DIR=<fresh tempdir>` and `CLAUDE_SECURESTORAGE_CONFIG_DIR=""` (isolates host plugins/hooks; issue #1751) | `claude_code_llm.py` l.37-60, l.278, l.607 | V |
| Consequence: a mounted `~/.claude/.credentials.json` is **not** read (CLI keeps credentials under `CLAUDE_CONFIG_DIR`) | docs https://code.claude.com/docs/en/authentication ("If you've set CLAUDE_CONFIG_DIR … keeps the .credentials.json under that directory") + code above | I (strong) |
| The SDK subprocess inherits the container's environment (`os.environ` minus `CLAUDECODE`) merged with `options.env` | `claude_agent_sdk/_internal/transport/subprocess_cli.py` l.425-480 | V |
| → A container env `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`, 1-year token, "for CI pipelines and scripts") would authenticate even on 0.10.1; ≥0.10.3 the official route is `HINDSIGHT_API_LLM_API_KEY=sk-ant-oat01-…` → passed as `CLAUDE_CODE_OAUTH_TOKEN` | auth docs (precedence #5, "Generate a long-lived token"); 0.10.3 PR https://github.com/vectorize-io/hindsight/pull/4796 ; models doc "Server-held token" | V (docs) / I (0.10.1 env route untested) |
| 0.10.1 constructor: `api_key  # Will be ignored, uses CLI auth` | `claude_code_llm.py` l.109 | V |
| Hindsight's own warning: "local, personal development use only … does not make the provider suitable for servers, Docker deployments or shared hosts. For those, use the `anthropic` provider with an API key." | https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/docs/developer/models.mdx ("Claude Code Setup", l.655-745) | V |
| Model is pinned per call (`model=self.model`), else the CLI default (Opus-class on Pro/Max) would run | `claude_code_llm.py` l.270-276 | V |
| Usage counts against subscription limits; AUP refusals detected and not retried | models doc; `claude_code_llm.py` l.75-97 | V |

## (b) `anthropic` + custom base URL

- Yes: `HINDSIGHT_API_LLM_BASE_URL` → `AsyncAnthropic(base_url=...)` (`anthropic_llm.py` l.217-218) **V**. Also `LLM_DEFAULT_HEADERS`, `LLM_EXTRA_BODY` wired into the Anthropic provider (cfg table) **V**.
- So any Anthropic-Messages-compatible endpoint (gateways, vendors exposing `/v1/messages`) can be targeted **I** (depends on the endpoint honouring forced `tool_choice` for SO and optionally `cache_control` / Batches).

## (c) Local options

| Option | Fit | |
|---|---|---|
| `ollama` | Best-enforced local SO (native schema on `/api/chat`); host service reachable from container needs `LLM_BASE_URL=http://host…:11434/v1` | V code / I networking |
| `lmstudio` | Schema-in-prompt only → least reliable SO | V |
| `llamacpp` (built-in) | Not runnable in the stock image; needs extra Python deps | V |
| External `llama-server` / vLLM via `openai` + base URL | `json_object`/strict schema depend on server; `LLM_OPENAI_COMPATIBLE_JSON_MODE` (≥0.10.3) for servers without JSON mode | V cfg, changelog 0.10.3 |
| Leaderboard reality check | All 8 small local Ollama models (≤3B: qwen2.5 0.5b/3b, gemma3 270m/1b, llama3.2, smollm2, granite3.1 2b, deepseek-r1 1.5b) → **0 successful retains** | V https://github.com/vectorize-io/hindsight-benchmarks/tree/HEAD/results/leaderboard/llm (`local-ollama-*.json`, `retain.summary.success = 0`) |

## (d) Per-operation overrides

- `HINDSIGHT_API_{RETAIN,REFLECT,CONSOLIDATION,MENTAL_MODEL_REFRESH}_LLM_{PROVIDER,API_KEY,MODEL,BASE_URL,MAX_CONCURRENT,MAX_RETRIES,INITIAL_BACKOFF,MAX_BACKOFF,TIMEOUT,REASONING_EFFORT,EXTRA_BODY,CACHE_AFFINITY}` — present in 0.10.1 (`config.py` l.381-453) **V**; each falls back to the global `LLM_*`; mental-model refresh falls back to the reflect LLM (cfg l.743-800) **V**.
- Vision slot `HINDSIGHT_API_VLM_*` for attachment chunks only **V** (`config.py` l.394).
- Multi-LLM failover / round-robin: `HINDSIGHT_API_LLM_<n>_*` + per-operation chains `HINDSIGHT_API_RETAIN_LLM_1_PROVIDER`, `..._LLM_STRATEGY` (`engine/multi_llm.py` exists in 0.10.1) **V** (cfg l.615-703).
- Per-op temperatures (`LLM_TEMPERATURE_RETAIN` 0.1, `_REFLECT` 0.9, `_CONSOLIDATION` 0.0) **V** cfg l.290-296.
- Docs guidance: "Retain (fact extraction) benefits from models with strong structured output capabilities, while Reflect … can use lighter, faster models." **V** cfg l.745.
- 1.1.0 adds an experimental shared "decision model" with per-operation overrides **V** (changelog 1.1.0).

## (e) Anthropic terms on Pro/Max subscription use (facts + URLs only)

| Statement | Source | |
|---|---|---|
| "**OAuth authentication** is intended exclusively for purchasers of Claude Free, Pro, Max, Team, and Enterprise subscription plans and is designed to support ordinary use of Claude Code and other native Anthropic applications." | https://code.claude.com/docs/en/legal-and-compliance ("Authentication and credential use") | V |
| "Developers building products or services that interact with Claude's capabilities, including those using the Agent SDK, should use API key authentication … Anthropic does not permit third-party developers to offer Claude.ai login into their own applications, or to route requests through Free, Pro, or Max plan credentials on behalf of their users. Moreover, developers may not collect, store, or intermediate Claude.ai credentials or session tokens" | same | V |
| "Nor does it prevent an end user from signing in to the unmodified Claude Code binary with their own Claude subscription" | same | V |
| "Advertised usage limits for Pro and Max plans assume ordinary, individual usage of Claude Code and the Agent SDK." | same ("Acceptable use") | V |
| "Anthropic reserves the right to take measures to enforce these restrictions and may do so without prior notice." | same | V |
| Agent SDK: "Unless previously approved, Anthropic does not allow third party developers to offer claude.ai login or rate limits for their products, including agents built on the Claude Agent SDK." | https://code.claude.com/docs/en/agent-sdk/overview | V |
| Consumer Terms (effective 2025-10-08), prohibited: "Except when you are accessing our Services via an Anthropic API Key or where we otherwise explicitly permit it, to access the Services through automated or non-human means, whether through a bot, script, or otherwise." Also: no sharing of account login/credentials | https://www.anthropic.com/legal/consumer-terms (§3 item 7, §2) | V |
| `claude setup-token`: "For CI pipelines, scripts, or other environments where interactive browser login isn't available, generate a one-year OAuth token … This token authenticates with your Claude subscription" | https://code.claude.com/docs/en/authentication ("Generate a long-lived token") | V |
| Hindsight's interpretation: uses official Agent SDK, performs no login itself, "intended for local, personal development use only", refers to Jan-2026 enforcement against tools spoofing the Claude Code client | models doc l.661-686 (links https://paddo.dev/blog/anthropic-walled-garden-crackdown/) | V |

## Hindsight's guidance on fact-extraction quality

- Docs point to the Model Leaderboard https://benchmarks.hindsight.vectorize.io/ ("accuracy, speed, cost, and reliability for retain, reflect, and observation consolidation") **V** (models doc l.96-100). Data: https://github.com/vectorize-io/hindsight-benchmarks (no licence).
- Page headline (2026-10-23): Retain #1 "GPT-5.6 Luna (low reasoning)" (36 models), Reflect #1 `openai/gpt-oss-120b` on Groq (21), Reranker #1 MiniLM-L6, Embeddings #1 Nemotron 3.5 Embed 1B **V**.
- Raw JSON (LoCoMo 3k/50 retain; quality = downstream QA accuracy, older runs on 242 Qs, newer runs on 80 Qs → two incomparable series) **V**:

| Series | Model | Quality % | Reflect % | Avg retain latency s |
|---|---|---|---|---|
| 242-Q (Feb 2026) | openai/gpt-5-mini | 89.7 | 70.7 | 68.5 |
| | gemini-3.1-pro-preview | 89.7 | 89.3 | 58.2 |
| | gemini-3.1-flash-lite | 88.4 | 94.2 | 2.3 |
| | gemini-3.5-flash | 88.0 | 93.8 | 24.5 |
| | openai/gpt-4.1-nano | 87.2 | 80.2 | 8.5 |
| | openai/gpt-5.4-mini | 86.4 | 83.5 | 5.4 |
| | groq/gpt-oss-120b | 84.7 | 94.2 | 8.1 |
| | openai/gpt-4o-mini | 81.0 | 93.8 | 9.0 |
| 80-Q (Aug 2026) | openai/gpt-5.6-luna-low | 50.0 | — | 11.2 |
| | gemini-3.7-flash-high / qwen3.6-35b-a3b | 48.8 | — | 12.4 / 11.4 |
| | gcp gpt-oss-20b-low / deepseek-v4-flash-low (37/50 ok) | 47.5 | — | 5.1 / 45.7 |

- **No Anthropic, DeepSeek-API or claude-code entries** in the leaderboard **V**. "Tested models" list includes only old Anthropic ids (`claude-sonnet-4-20250514`, `claude-3-5-sonnet-20241022`) **V** (models doc l.102-122).

## Implications for hindsight-claude

- Provider choice = env vars only; Hindsight already covers every lane the request lists (subscription, Anthropic key, Anthropic-compatible URL, OpenAI, Ollama, OpenRouter, DeepSeek…) → our binary needs a matrix + live validation (`POST /banks/{id}/health/llm` exists; feature `bank_llm_health:false` here), no provider code.
- `claude-code` inside Docker works without mounting `~/.claude` (CLI bundled; token via env), but needs a `claude setup-token` token; native support for that token arrives in 0.10.3 → the reference 0.10.1 install would need an upgrade or the raw `CLAUDE_CODE_OAUTH_TOKEN` env.
- Anthropic's published texts restrict routing Pro/Max credentials through third-party products and automated access, while also documenting `setup-token` for scripts and "ordinary, individual usage of … the Agent SDK"; Hindsight itself labels the lane "personal development only". These facts are what D7 must weigh; the `anthropic` API-key lane has no such ambiguity.
- Anthropic-compatible endpoints work through `anthropic` + `LLM_BASE_URL`; OpenAI-compatible through `openai` + `LLM_BASE_URL`.
- Retain needs reliable structured output: small local models (≤3B) fail completely in upstream benchmarks; `ollama` (native schema) > `lmstudio` (prompt-only) for local.
- Per-operation overrides allow e.g. cheap model for retain/consolidation and another for reflect; credentials are server-level, not per bank.
- No Hindsight benchmark data exists for Claude models or `deepseek-v4-flash` via API → our own benchmark would be the only evidence for presets.
