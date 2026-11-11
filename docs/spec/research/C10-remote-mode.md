# C10 — Remote mode: Hindsight on another machine (checked 2026-10-23)

Legend: **V** = VERIFIED, **I** = INFERRED. Container paths refer to `hindsight` 0.10.1; "cfg" = https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/docs/developer/configuration.mdx (1.1.0).

## What Hindsight offers over a network

| Topic | Finding | Source | |
|---|---|---|---|
| Default auth | None ("By default, Hindsight runs without authentication") | cfg l.1643-1645; `extensions/builtin/tenant.py` l.8-41 | V |
| Built-in auth | One shared bearer key: `HINDSIGHT_API_TENANT_EXTENSION=…builtin.tenant:ApiKeyTenantExtension` + `HINDSIGHT_API_TENANT_API_KEY`; header `Authorization: Bearer <key>`; HTTP **and** MCP (unless `HINDSIGHT_API_TENANT_MCP_AUTH_DISABLED=true`) | `extensions/builtin/tenant.py` l.44-90; `api/http.py` l.5183-5205 | V |
| Key comparison | Plain `!=` (not constant-time) | `tenant.py` l.73 | V (timing-attack relevance over WAN: I, low) |
| Extra MCP token | `HINDSIGHT_API_MCP_AUTH_TOKEN` (optional, MCP only) | cfg l.2566, l.2592-2597 | V |
| Scoped keys / per-bank ACL | Not built in. Per-caller bank/tag restriction needs a custom `TenantExtension` + `OperationValidatorExtension.resolve_tag_scope` (Python, loaded in the server) | cfg l.1685; `developer/extensions.md` l.385-475 | V |
| Rate limiting / lockout | None found in API auth path | code read of `tenant.py`, `http.py` auth | I (absence) |
| Endpoints open even with key on | `/health`, `/health/ready`, `/health/live`, `/version` (versions + feature flags), `/metrics` (1405 series; labels include `tenant`, `endpoint`, `provider`, `model`; no bank ids seen), `/openapi.json`, `/docs` | curl on 127.0.0.1:8888 → 200 | V |
| TLS in server | uvicorn `--ssl-keyfile/--ssl-certfile` CLI flags only (no env var) | `/app/api/hindsight_api/main.py` l.246-248, l.462-465; `config.py` has no SSL var | V |
| TLS in Docker image | `start-all.sh` starts plain `hindsight-api &` with no args → no TLS unless the entrypoint is replaced | `/app/start-all.sh` l.350-354 | V |
| Reverse-proxy support | `--proxy-headers`, `--forwarded-allow-ips`; `HINDSIGHT_API_BASE_PATH` + `NEXT_PUBLIC_BASE_PATH` for sub-paths; upstream nginx compose example and Traefik snippet | `main.py` l.238-244; cfg l.1694, l.3127-3190; https://github.com/vectorize-io/hindsight/tree/main/docker/docker-compose/nginx | V |
| CORS | No CORS middleware in API code (grep `cors`/`allow_origins` → none); `OPTIONS` preflight → 405 | grep in `/app/api/hindsight_api`; curl OPTIONS | V |
| Bind | `HINDSIGHT_API_HOST` default `0.0.0.0`; reference compose publishes `127.0.0.1:8888` and `127.0.0.1:9999` only | cfg l.1691; `docker inspect hindsight` PortBindings | V |
| Load shedding | 1.0.0+: 503 + `Retry-After` when store unavailable | changelog 1.0.0 | V |
| Multi-tenant isolation | Tenant = PostgreSQL **schema** chosen by the tenant extension; built-in extensions always return one schema (`HINDSIGHT_API_DATABASE_SCHEMA`, default `public`) | `extensions/tenant.py` l.24-35; builtin `tenant.py` | V |
| Multi-bank isolation | Banks share one schema; every op is bank-scoped by path `/v1/default/banks/{bank_id}/…`, but any holder of the key can list (`GET /v1/default/banks`) and read/write every bank | OpenAPI; ApiKeyTenantExtension has no bank check | V |
| Upstream remote recipe | `cloudflare-oauth-proxy` (Cloudflare Worker, OAuth 2.1 + password gate, CORS allowlist, Cloudflare Tunnel to origin) — aimed at cloud MCP clients | https://github.com/vectorize-io/hindsight/blob/main/hindsight-integrations/cloudflare-oauth-proxy/README.md | V |

## Control plane (UI, port 9999)

| Fact | Source | |
|---|---|---|
| Next.js server; binds `HINDSIGHT_CP_HOSTNAME` (default `0.0.0.0`), port `HINDSIGHT_CP_PORT` (9999) | `/app/start-all.sh` l.158-202, l.387-389 | V |
| Talks to the API with `HINDSIGHT_CP_DATAPLANE_API_URL` + `HINDSIGHT_CP_DATAPLANE_API_KEY` (sent as Bearer on every request) | cfg l.2963-2983 | V |
| UI login only if `HINDSIGHT_CP_ACCESS_KEY` set ("none — auth disabled" otherwise) | cfg l.2971; https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/docs/developer/installation.md l.288 | V |
| Exposes server-side proxy routes `/api/{banks,memories,recall,reflect,documents,entities,graph,knowledge-base,operations,files,stats,chunks,version,health,…}`; reference install: `/` → 307 `/login`, `/api/banks` → 401 | `/app/control-plane/.next/server/app/api/`; curl | V |
| → Without `HINDSIGHT_CP_ACCESS_KEY`, anyone reaching :9999 acts with the dataplane key's full rights | derived from the two rows above | I |

## Transport options for our binary (client ↔ remote server)

| | SSH tunnel (`ssh -L` / Go `x/crypto/ssh`) | TLS + bearer key (Caddy in front) | Tailscale / WireGuard |
|---|---|---|---|
| Confidentiality | SSH encryption; server port stays on 127.0.0.1 | TLS; port exposed to the network/Internet | WireGuard encryption; port on tailnet only |
| AuthN | SSH keys (per-user) + Hindsight key | Hindsight shared key only (+ optional Caddy basic auth / mTLS) | Tailnet identity/ACLs + Hindsight key |
| Attack surface | sshd only | Public HTTPS incl. the unauthenticated `/metrics`, `/openapi.json`, `/docs`, `/version` unless the proxy filters them | None public |
| Certificates | none | Public: needs DNS name + ACME (Caddy automatic HTTPS). LAN-only: `tls internal` → each client must trust Caddy's root | `tailscale serve` → Let's Encrypt cert for `<host>.<tailnet>.ts.net`; requires MagicDNS + HTTPS enabled; **machine names published in CT logs** |
| Setup friction | Needs sshd on server (Windows server: optional feature) and a key; a tunnel process must stay up (hooks are short-lived processes) | DNS + port-forward/firewall + Caddy config; key distribution | Install client on both, log in to the same tailnet; free Personal plan "Up to 6 users", "Unlimited user devices", non-commercial |
| Windows / macOS clients | OpenSSH client built into Windows 10+/macOS (I); Go `x/crypto/ssh` is pure Go → in-binary, no external ssh needed (I) | Any HTTPS client; Go `net/http` native | Official clients Linux/macOS/Windows (I); `serve` of files limited on the App-Store macOS variant |
| Fits "assisted remote install over SSH" (decision 0003) | Same channel for install and runtime | Separate | Separate (but can use Tailscale SSH) |
| Licence | OpenSSH BSD / x/crypto BSD | Caddy Apache-2.0 | Tailscale client BSD-3 (coordination server proprietary; Headscale alternative) |

Sources: Tailscale pricing https://tailscale.com/pricing (V), serve https://tailscale.com/kb/1312/serve (V), HTTPS certs https://tailscale.com/kb/1153/enabling-https (V: Let's Encrypt, CT ledger, MagicDNS prerequisite); Caddy licence (C01, V). OS availability rows marked I.

## Latency budget (prompt hook: ~17 s recall budget; recall 0.1-3 s locally)

| Component | Estimate | |
|---|---|---|
| Recall server time | 0.1-3 s (given), unchanged by transport | given |
| Network RTT | LAN <1-2 ms; same-country WAN ~10-40 ms; intercontinental 100-250 ms | I |
| New connection per hook call | Hooks are fresh processes → no keep-alive reuse: TCP (1 RTT) + TLS 1.3 (1 RTT) + request (1 RTT) ≈ 3 RTT ≈ ≤0.75 s worst-case WAN | I |
| SSH per-call (no resident tunnel) | TCP + SSH key exchange + auth + channel ≈ 3-5 RTT + crypto → ~0.1-1.3 s WAN | I |
| Tailscale | Direct path ≈ raw RTT + small overhead; via DERP relay adds the relay detour | I |
| Margin | Even worst case (3 s recall + ~1.5 s transport) stays well under 17 s; the real risk is an unreachable server → connect timeout must be short (≈1-2 s) so the hook fails open fast | I |
| Retain/backup traffic | Retain bodies are transcripts (KB-MB); export archives via async `transfer/export` + `files/download` — throughput, not latency, matters | V (endpoints) / I (sizes) |

## Implications for hindsight-claude

- Hindsight's only built-in network auth is one shared bearer key with full access to all banks; no TLS in the stock image, no CORS, no rate limiting, and `/metrics`, `/openapi.json`, `/docs`, `/version` stay open → never expose 8888/9999 directly; a transport layer is required.
- Reverse-proxy deployment is supported upstream (proxy headers, base path, nginx/Traefik examples); a proxy can also block the open endpoints.
- The control plane is a full-rights proxy to the API; remote exposure requires `HINDSIGHT_CP_ACCESS_KEY` at minimum, ideally kept on loopback/tailnet.
- SSH reuses the channel decision 0003 already needs for assisted install and can be embedded in a pure-Go binary, but short-lived hooks need a resident tunnel/daemon or pay a per-call handshake.
- Tailscale gives the lowest exposure and valid certs with low friction, but adds a third-party account dependency and publishes host names in CT logs.
- Latency is not the constraint (transport adds ≤~1.5 s worst case vs a 17 s budget); fast failure on unreachable servers is.
- Per-user/per-bank isolation for a shared remote server would require shipping a custom Python tenant extension into the Hindsight container.
