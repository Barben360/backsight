# C03 — Hindsight API: versions, OpenAPI, stability (checked 2026-10-23)

Legend: **V** = VERIFIED, **I** = INFERRED. Reference install = 0.10.1.

## Releases since 0.10.1

| Version | Date | Highlights (user-facing) | Breaking | DB migrations |
|---|---|---|---|---|
| 0.10.2 | 2026-09-29 | Bank **aliases** (zero-downtime bank-id migration), per-scope consolidation strategies, opt-in CUDA for ONNX embeddings, streamed exports, reasoning-token accounting | none listed | 6 (alters `entities`, high volume) |
| 0.10.3 | 2026-10-08 | Per-caller **tag scopes** on shared banks, `claude-code` auth from a server-held `claude setup-token` (PR #4796), option for OpenAI-compatible backends without JSON mode, rerank per-candidate token cap, over-long recall queries truncated instead of 400 | worker `--max-retries`/`--log-level` flags removed | 1 |
| 1.0.0 | 2026-10-13 | 503 + `Retry-After` when store sheds load; CLI/clients tolerate new fields; `claude-code` JSON repair + ToS docs | tag-scoped callers refused on whole-bank operations | 0 |
| 1.1.0 | 2026-10-21 | Per-document change history, tag-glob observation scopes + bank default scopes, experimental "decision model" (recall ranking, consolidation, reflect), ChatGPT sign-in provider, NVIDIA NIM embeddings/rerank, any ONNX cross-encoder reranker, Anthropic Workload Identity Federation, KB page PDF/MD download, live requests prioritised over background LLM jobs, `HINDSIGHT_API_RECALL_PREFER_OBSERVATIONS` | bank **imports** reject invalid / `_`-prefixed bank ids | 5 (alters `memory_units`, high volume; "Migrations run on startup") |

Sources: https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/src/pages/changelog/index.md (sections 1.1.0 → 0.10.2); release notes https://github.com/vectorize-io/hindsight/releases/tag/v1.1.0 , /v1.0.0 , /v0.10.3 , /v0.10.2 ; blog https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/blog/2026-10-21-version-1-1-0.md ("Everyone on 1.0.x should upgrade"). All **V**.

### Cadence (V, GitHub releases API)

- 55 releases between 2026-01-01 and 2026-10-23 (~1.3/week). Since 0.10.1: 4 releases in 30 days.
- Pattern: patch every ~1-2 weeks, minor every ~3-6 weeks; 0.x → 1.0.0 on 2026-10-13 with only one small breaking change.
- Integrations (incl. coding-agents npm plugin) release on their own cadence and changelog (changelog page header). V

## Stability / versioning promises

| Item | Finding | | 
|---|---|---|
| Written semver / API-stability policy | None found (README, CONTRIBUTING, docs, changelog, 1.x blog searched) | V (absence) |
| URL versioning | Paths are `/v1/default/...` — `v1` unchanged across 0.4 → 1.1 | V (OpenAPI) |
| CI guard | Job `check-openapi-compatibility` diffs `hindsight-docs/static/openapi.json` against the PR merge-base with `hindsight_dev/check_openapi_compatibility.py` (flags removed endpoints, new required request fields, removed required response fields, type changes) | V https://github.com/vectorize-io/hindsight/blob/main/.github/workflows/test.yml (l.5406-5467), https://github.com/vectorize-io/hindsight/blob/main/hindsight-dev/hindsight_dev/check_openapi_compatibility.py |
| Removal style | Removed endpoints stay in the spec marked "(removed — use …)" (e.g. `GET/PUT .../profile`, `POST .../background`, `GET .../document-transfer`) | V live spec |
| 0.10.1 → 1.1.0 spec diff | 94 → 101 operations; **7 added, 0 removed** (aliases ×4, `documents/{id}/history`, `audit-logs/{log_id}`, `consolidation-strategies/preview`) | V (diff of live 0.10.1 spec vs https://github.com/vectorize-io/hindsight/blob/v1.1.0/hindsight-docs/static/openapi.json) |
| Env-var stability | Some knobs documented as experimental: "may change or be removed in any release, without a deprecation period" | V `developer/configuration.mdx` l.1571 |
| Breaking changes elsewhere | Extension paths moved (e.g. `builtin.supabase_tenant` gone in 0.9.3 → `ModuleNotFoundError`) | V `developer/extensions.md` l.26-27 |

## OpenAPI spec

| Where | Status | |
|---|---|---|
| Live: `http://127.0.0.1:8888/openapi.json` | 200 **without auth** (even with ApiKeyTenantExtension on); OpenAPI 3.1.0, title "Hindsight HTTP API", `info.version 0.10.1`, 74 paths / 94 operations | V |
| Swagger UI `/docs` | 200 without auth | V |
| Hosted latest | https://hindsight.vectorize.io/openapi.json → 200, `info.version 1.1.0` (the `https://hindsight.dev/openapi.json` link in the Go docs → 404) | V |
| In repo | `hindsight-docs/static/openapi.json` (source for all clients) | V |
| `securitySchemes` | **None declared** (0.10.1 and 1.1.0) → generated clients say "Endpoints do not require authorization" | V (spec; Go client README l.202-204) |
| MCP endpoint | `/mcp/` (and `/mcp/{bank_id}/`) not in OpenAPI | V |

### Endpoint groups (0.10.1 live spec, tag → ops)

| Group | Ops | Key endpoints |
|---|---|---|
| Monitoring | 5 | `GET /health` (DB pool stats), `/health/ready`, `/health/live`, `/version` (api_version + feature flags), `/metrics` (Prometheus) |
| Banks | 17 | `GET /v1/default/banks`, `PUT/PATCH/DELETE /banks/{id}`, `GET/PATCH/DELETE .../config` (hierarchical bank config), `.../stats`, `.../stats/memories-timeseries`, `POST .../health/llm`, `POST .../consolidate`, `.../consolidation/recover`, `DELETE .../observations`, `POST .../prompts/preview` |
| Memory (recall/retain) | 14 | `POST .../memories` (retain; body `items`, `async`, `document_tags`, `operation_id`), `POST .../memories/recall` (`query`, `types`, `budget`, `max_tokens`, `tags`, `tags_match`, `tag_groups`, `temporal_window`, `prefer_observations`…), `POST .../reflect`, `GET .../memories/list`, `GET/PATCH .../memories/{id}`, `.../history`, `POST .../memories/dry-run-extract`, `GET .../tags`, `GET .../graph`, `GET .../observations/scopes`, attachments |
| Documents | 7 | list/get/patch/delete documents, chunks, `POST .../reprocess`, `GET /v1/default/chunks/{id}` |
| Operations (async) | 5 | `GET .../operations`, `GET/DELETE .../operations/{id}`, `POST .../retry`, `DELETE .../delete` |
| Mental Models | 9 | CRUD, `history`, `refresh`, `dry-run-refresh`, `clear` |
| Knowledge Base (pages) | 8 | `GET .../knowledge-base/tree`, `/search` (BM25+vector), `/pages/{id}`, `POST /folders`, `/pages`, `PATCH/DELETE /nodes/{id}`, `GET /export` (markdown bundle) |
| Directives | 5 | CRUD |
| Entities | 4 | list, graph, detail, regenerate (deprecated) |
| Bank Templates | 3 | `GET .../export`, `POST .../import`, `GET /v1/bank-template-schema` |
| Bank Transfer | 3 | async `transfer/export`, `transfer/import`, `clone` |
| Document Transfer | 4 | async document export/import, `GET /v1/default/files/download/{key}` |
| Files | 1 | `POST .../files/retain` (file → memories) |
| Webhooks | 5 | CRUD + deliveries |
| Audit | 2 | audit logs + stats (feature `audit_log: false` here) |
| LLM Traces | 2 | `llm-requests` + stats (feature `llm_trace: true`) |

`/version` here: `{"api_version":"0.10.1","features":{"observations":true,"mcp":true,"worker":true,"bank_config_api":true,"bank_llm_health":false,"file_upload_api":true,"document_export_api":true,"document_import_api":true,"audit_log":false,"llm_trace":true,"store_document_text":true}}` (V) → usable for capability detection.

## Go client

| Item | Finding | |
|---|---|---|
| Official client | `go get github.com/vectorize-io/hindsight/hindsight-clients/go`, generated by OpenAPI Generator (`generatorName: go`, `generateInterfaces: true`), Go ≥1.23 per docs (`go.mod` says `go 1.18`) | V https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/docs/sdks/go.md , `hindsight-clients/go/openapi-generator-config.yaml`, `go.mod` |
| Auth helper | Hand-written `NewAPIClientWithToken(baseURL, token)` adds `Authorization: Bearer` default header | V `hindsight-clients/go/hindsight_client.go` l.31-62 |
| Versioning | **No module tags**: Go proxy `@v/list` empty, `@latest` = pseudo-version `v0.0.0-20261023165002-b4a0da1e9fd2` → pin by commit only | V https://proxy.golang.org/github.com/vectorize-io/hindsight/hindsight-clients/go/@latest |
| README stale header | "API version: 0.4.11" | V |
| Dependencies | testify, `gopkg.in/validator.v2`, `go.yaml.in/yaml/v3` (pure Go) | V go.sum |
| Self-generation | Feasible from the 3.1 spec (oapi-codegen / ogen / openapi-generator); spec lacks `securitySchemes`, so auth must be injected by hand | I |

## Auth model

| Mode | How | |
|---|---|---|
| Default | `DefaultTenantExtension`: **no auth**, single schema (`HINDSIGHT_API_DATABASE_SCHEMA`, default `public`) | V container `/app/api/hindsight_api/extensions/builtin/tenant.py` l.8-41 |
| Built-in API key | `HINDSIGHT_API_TENANT_EXTENSION=hindsight_api.extensions.builtin.tenant:ApiKeyTenantExtension` + `HINDSIGHT_API_TENANT_API_KEY`; one shared key, compared with `!=`; same key for HTTP and MCP unless `HINDSIGHT_API_TENANT_MCP_AUTH_DISABLED=true` | V same file l.44-90 |
| Header | `Authorization: Bearer <key>` (a bare key without `Bearer ` also accepted) | V `/app/api/hindsight_api/api/http.py` l.5183-5205 |
| Reference install | ApiKeyTenantExtension enabled; `/v1/default/banks` without key → `401 {"detail":"Authentication failed: Invalid API key"}`; MCP initialize without key → 401 | V (curl) |
| Always unauthenticated | `/health*`, `/version`, `/metrics`, `/openapi.json`, `/docs` | V (curl 200) |
| Custom | Implement `TenantExtension` (JWT/OAuth, schema per tenant); `HINDSIGHT_API_EXTENSION_PASSTHROUGH_HEADERS` forwards extra headers | V `developer/extensions.md`, `developer/configuration.mdx` l.1643-1685 |
| Path segment `default` | `/v1/default/...` constant in all paths | V; meaning (tenant placeholder) I |

## Implications for hindsight-claude

- Upstream moves fast (≈weekly) and 1.1.0 is already two minors ahead of the reference 0.10.1; migrations run on container start and can be long on `memory_units`/`entities` → updates need a pre-update backup + health wait.
- The HTTP surface is additive so far (0 removals 0.10.1→1.1.0, CI-enforced), but there is no written semver promise; `/version` feature flags allow runtime capability checks.
- Everything our binary needs (banks, config, retain, recall, reflect, documents, operations, mental models, knowledge pages, transfer/export, health) is in the spec.
- The official Go client exists but is untagged (pseudo-versions only), generated with nullable wrappers, and the spec declares no auth → either vendor/pin it by commit or generate our own subset; Bearer header added manually either way.
- Auth is a single shared bearer key; `/metrics`, `/openapi.json`, `/docs`, `/version` stay open even with the key enabled.
- `claude setup-token` support in the `claude-code` provider exists only from 0.10.3 (see C07).
