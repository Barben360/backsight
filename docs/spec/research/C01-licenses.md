# C01 — Licenses (checked 2026-10-23)

Legend: **V** = VERIFIED (seen in source/docs), **I** = INFERRED.

## Hindsight server (repo + image)

| Item | Finding | Source | |
|---|---|---|---|
| Repo license | MIT, "Copyright (c) 2025 Vectorize AI, Inc." | https://github.com/vectorize-io/hindsight/blob/main/LICENSE ; GitHub API `license.spdx_id=MIT` | V |
| README | "MIT — see LICENSE"; product named "Hindsight™" | https://github.com/vectorize-io/hindsight/blob/main/README.md (l.12, l.24, l.434) | V |
| Python package in image | `hindsight-api-slim` 0.10.1, `license = "MIT"` | container `/app/api/pyproject.toml` l.1-9 | V |
| Image label | `org.opencontainers.image.licenses=MIT`, `source=https://github.com/vectorize-io/hindsight`, revision `f8950b0c…` | `docker image inspect ghcr.io/vectorize-io/hindsight:0.10.1` | V |
| Inconsistency | The generated OpenAPI `info.license` says **Apache 2.0** (0.10.1 live spec and v1.1.0 static spec) while every LICENSE file says MIT | `http://127.0.0.1:8888/openapi.json` `.info.license`; https://github.com/vectorize-io/hindsight/blob/v1.1.0/hindsight-docs/static/openapi.json | V (looks like a FastAPI boilerplate leftover — I) |
| Per-integration LICENSE files | Several integrations carry their own MIT LICENSE (cursor, agentcore, obsidian "Copyright (c) 2026 Vectorize", dify, google-adk, pipecat, roo-code, vapi) | repo tree, e.g. https://github.com/vectorize-io/hindsight/blob/main/hindsight-integrations/cursor/LICENSE | V |
| Third-party bundled in image | `claude-agent-sdk` 0.2.87 (MIT) **bundles the proprietary Claude Code CLI 2.1.150** (`_bundled/claude`, 238 MB); Claude Code is "© Anthropic PBC. All rights reserved", Commercial Terms | container `/app/api/.venv/lib/python3.11/site-packages/claude_agent_sdk/_bundled/claude`, `_cli_version.py`; https://github.com/anthropics/claude-code/blob/main/LICENSE.md | V |
| Embedded Postgres `pg0` | `pg0_embedded` 0.15.0, `License-Expression: MIT` (the GitHub repo vectorize-io/pg0 has no LICENSE file) | container `.../pg0_embedded-0.15.0.dist-info/METADATA`; https://github.com/vectorize-io/pg0 | V |
| CLA | None. CONTRIBUTING: "We don't accept pull requests from outside the Hindsight team" — no CLA/DCO workflow in `.github/workflows` | https://github.com/vectorize-io/hindsight/blob/main/CONTRIBUTING.md (l.7-10); workflows list | V |
| Trademark clause | None in repo (no TRADEMARK/NOTICE file). "Hindsight™" = claimed, unregistered-style mark. Vectorize general ToU claims its trademarks/logos | README l.24; https://blog.vectorize.io/terms (Oct 2024) | V |
| NOTICE file | None in repo | repo tree (`grep -i notice`) | V |
| Image signing | Images signed with cosign keyless (Sigstore), identity `…/.github/workflows/sign-images.yml@…` | https://github.com/vectorize-io/hindsight/blob/main/.github/workflows/sign-images.yml (l.42-70) | V |

## npm plugin `@vectorize-io/hindsight-coding-agents`

| Item | Finding | Source | |
|---|---|---|---|
| Latest version | 0.8.2 (modified 2026-10-19) | `npm view @vectorize-io/hindsight-coding-agents` | V |
| `license` field | **Absent** in package.json and npm metadata | tarball `package/package.json`; `npm view … license` → empty | V |
| LICENSE file in tarball | **Absent** (contents: README.md, plugin.json, package.json, hooks/, skill/, dist/, index.js, cordis.patch.yml) | https://registry.npmjs.org/@vectorize-io/hindsight-coding-agents/-/hindsight-coding-agents-0.8.2.tgz | V |
| Source | `repository.directory = hindsight-integrations/coding-agents` in the MIT monorepo; that directory has no own LICENSE, so the root MIT LICENSE covers it | https://github.com/vectorize-io/hindsight/tree/main/hindsight-integrations/coding-agents | V (coverage by root LICENSE: I, standard monorepo reading) |
| Skill text | Generated from `skill-src/` into `skill/` in the same MIT tree | repo tree `hindsight-integrations/coding-agents/skill-src`, `/skill` | V |

## Other images / components we may pull or call

| Component | License | Source | |
|---|---|---|---|
| `pgvector/pgvector:pg18` image | pgvector extension: PostgreSQL License (GitHub shows NOASSERTION; LICENSE text is the PostgreSQL licence). Image = `FROM postgres:<v>-bookworm` + pgvector build | https://github.com/pgvector/pgvector/blob/master/LICENSE ; https://github.com/pgvector/pgvector/blob/master/Dockerfile | V |
| Base `postgres` image | Dockerfiles MIT (docker-library/postgres); PostgreSQL = PostgreSQL License; image also contains Debian bookworm packages under their own licences (GPL etc.) | https://github.com/docker-library/postgres ; `hindsight-db` `/etc/os-release` = Debian 12 | V |
| pgvectorscale (optional DiskANN) | PostgreSQL License | https://github.com/timescale/pgvectorscale | V |
| VectorChord (optional) | **Dual AGPLv3 / ELv2** — copyleft/source-available, avoid bundling | https://github.com/tensorchord/VectorChord/blob/main/LICENSE | V |
| HF text-embeddings-inference | Apache-2.0 | https://github.com/huggingface/text-embeddings-inference | V |
| llama.cpp (`llama-server`) | MIT | https://github.com/ggml-org/llama.cpp | V |
| Ollama | MIT | https://github.com/ollama/ollama | V |
| Caddy | Apache-2.0 | https://github.com/caddyserver/caddy | V |
| Tailscale client | BSD-3-Clause | https://github.com/tailscale/tailscale | V |
| claude-agent-sdk-python | MIT (but drives the proprietary CLI) | https://github.com/anthropics/claude-agent-sdk-python | V |
| Official Go client `hindsight-clients/go` | Part of MIT monorepo; generated by OpenAPI Generator 7.10.0; deps testify (MIT), validator.v2 (Apache-2.0), yaml/v3 | https://github.com/vectorize-io/hindsight/tree/main/hindsight-clients/go | V (dep licences: I) |

## What we may do (MIT terms, facts only)

| Action | MIT allows? | Obligation |
|---|---|---|
| Copy/adapt plugin skill text, hook logic, prompts, Go client code | Yes ("use, copy, modify, merge, publish, distribute, sublicense, and/or sell") | Keep "Copyright (c) 2025 Vectorize AI, Inc." + permission notice "in all copies or substantial portions" (V, LICENSE text) |
| Redistribute the Hindsight image or re-tag it | Yes for Hindsight code | Image also contains the proprietary Claude Code CLI (bundled by the SDK) → re-hosting the image redistributes Anthropic software under Commercial Terms (I) |
| Pull the official image at install time | No redistribution by us | None beyond user-side terms (I) |
| Use "Hindsight" in our name | Not a copyright question; MIT grants no trademark rights (I, MIT is silent on marks) | See C02 |

## Implications for hindsight-claude

- Code reuse from the server, plugin and Go client is permitted; any copied "substantial portion" needs the Vectorize MIT notice → a `THIRD_PARTY_NOTICES`/`NOTICE` file in our repo and in release archives.
- The npm tarball itself ships no licence text; attribution must point to the monorepo root LICENSE (MIT, Vectorize AI, Inc.).
- No CLA and no external PRs upstream: fixes we need in Hindsight must go through issues, not PRs.
- Pull images from upstream registries rather than mirroring them: the Hindsight image embeds the proprietary Claude Code CLI.
- Optional add-ons: VectorChord is AGPL/ELv2 (keep out of defaults); everything else listed is permissive (MIT / Apache-2.0 / BSD / PostgreSQL).
- Upstream images are cosign-signed: our binary can verify `ghcr.io/vectorize-io/hindsight` signatures deterministically before an update.
