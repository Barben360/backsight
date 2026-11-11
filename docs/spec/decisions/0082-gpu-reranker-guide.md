# 0082 — GPU reranker: documented recipes, setup and doctor point to them

- **Date**: 2026-11-03
- **Status**: accepted
- **Decided by**: author (2026-11-03: "it would be a good thing if it could be documented in our guide", "It should be proposed in setup too", and, choosing "point, don't write" over a setup question that writes the service: "we don't want to have to manage dependencies on the system itself (missing NVidia packages, etc). We just need to say 'hey, install these NVidia packages to have a GPU reranker' and the agent will find the way to do so itself"), on the agent's proposal

## Decision
Amends 0035 without reversing it: the binary still installs no GPU runtime, writes no GPU service and never touches `compose.override.yaml`; the managed acceleration service stays v2.

- **Guide**: a new user page, `docs/gpu-reranker.md`, gives per vendor what the machine needs (packages, groups, a container check) and a `compose.override.yaml` recipe running the same reranker (`cross-encoder/mmarco-mMiniLMv2-L12-H384-v1`): NVIDIA through TEI (`tei` provider), AMD and Intel through llama.cpp's `llama-server` on Vulkan (`cohere` provider, GGUF converted from the official weights at the pinned revision by a one-shot service). Also: macOS and Windows (host service, untested), checks, moving from one's own reranker, going back to the CPU, updates and backups.
- **Setup**: the pre-flight `gpu` line, when a GPU is found on a Docker server, names `backsight guide gpu-reranker`; on native it says a GPU reranker needs Docker; on a client it says the server runs elsewhere.
- **Doctor**: the `gpu` check reads (only) the override; a `HINDSIGHT_API_RERANKER_PROVIDER` there is reported as the user's own reranker; otherwise, with a GPU on a local Docker server, its fix names the guide.
- **Agent guide**: after setup, when the `gpu` line names a GPU, the agent offers the reranker, installs the prerequisites with the user's go-ahead, runs the container check, writes or merges the override, and backs it out if the server does not come back healthy.

## Why
- Users with a GPU already run their own reranker this way (author's work PC, NVIDIA RTX A2000: searches from up to 2.9 s to 0.11–0.28 s); documenting it costs no support surface in the binary.
- Drivers and container toolkits are per-distribution and per-vendor: an agent installs them better than backsight could detect and fix them.

## Evidence (2026-11-03)
- Radeon 890M, llama.cpp `server-vulkan-b11723`: `backsight search` 0.72–0.88 s on CPU → 0.33–0.39 s; same top-3 on 5 queries but one near-tie swap; ~330 MB GPU memory.
- `llama-server` returns HTTP 400 for a pair over 512 tokens and Hindsight then fails the whole recall (4,760-char query). `HINDSIGHT_API_RECALL_MAX_QUERY_TOKENS=250` (Hindsight truncates the query) keeps pairs ≤ 445 tokens on 4,760 and 16,320-char and CJK queries. TEI 1.9.4 truncates by itself (`/info` `auto_truncate: true`, checked on its CPU image).
- The one-shot conversion service passes `docker compose up -d --wait` (Compose 5.6.1) from an empty volume in 19 s and is deterministic (same sha256 as a manual conversion).
