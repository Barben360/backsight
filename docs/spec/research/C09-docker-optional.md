# C09 — Can Hindsight run without Docker? (checked 2026-10-23)

Legend: **V** = VERIFIED (seen in source/docs or run here), **I** = INFERRED.
Container paths are inside `ghcr.io/vectorize-io/hindsight:0.10.1`, prefix `/app/api/` omitted.

## TL;DR

- Yes. `uv pip install "hindsight-api-slim[local-onnx,embedded-db]==0.10.1"` in a uv venv (Python 3.12) installed in **6 s**, started with the embedded Postgres **pg0** and the mock LLM, answered `/health` (database connected), created a bank, retained 5 items and recalled them correctly. Cold start 14.8 s (incl. a 470 MB model download), warm start 3.4-6.0 s. **V**
- pg0 = a vectorize Rust binary bundling **PostgreSQL 18.1 + pgvector 0.8.5** (verified: `vector 0.8.5`, `pg_trgm 1.6` in the instance), wheels and standalone binaries for Linux x64/arm64 (glibc and musl), macOS arm64/x64, Windows x64. Data lives in **`~/.pg0/instances/<name>/data`** (the Hindsight docs say `~/.hindsight/pg0/`, which is wrong for 0.10.1 + pg0 0.15.2). **V**
- Footprint: venv 1.1 GB without torch (of which 281 MB is `claude_agent_sdk` bundling the Claude Code CLI); the default `hindsight-api` meta-package pulls `[all]` incl. torch, which on Linux means CUDA wheels (~4.4 GB more, measured in another venv). **V**
- Not in the pip package: the Control Plane UI (separate npm package, needs Node). **V**
- vectorize already ships a "no Docker" path: `hindsight-embed` (pip/uvx CLI) starts `uvx hindsight-api@<version>` as a daemon on pg0. **V**
- Docker Desktop is free only for companies with < 250 employees **and** < $10 M revenue (plus personal, education, non-commercial OSS). **V**

## 1. Packages

| Package | Latest | What it is | Source | |
|---|---|---|---|---|
| `hindsight-api-slim` | 1.1.0 (0.10.1 = reference) | the server; `requires-python >=3.11`; MIT; extras `local-ml` (sentence-transformers, torch ≥2.11, flashrank, mlx on darwin-arm64), `local-llm` (llama-cpp-python), `local-onnx` (onnxruntime, transformers, tokenizers), `embedded-db` (`pg0-embedded>=0.15.0`), `oracle`, `all` = local-ml + local-onnx + embedded-db | container `pyproject.toml` l.1-9, l.114-167; https://pypi.org/pypi/hindsight-api-slim/json | V |
| `hindsight-api` | 1.1.0 | meta-package = `hindsight-api-slim[all]==<same version>` (4 KB wheel) | https://pypi.org/pypi/hindsight-api/1.1.0/json (`requires_dist`) | V |
| `hindsight-all` | 1.1.0 | `hindsight-api-slim[all]` + `hindsight-client` + `hindsight-embed` | https://pypi.org/pypi/hindsight-all/1.1.0/json | V |
| `hindsight-embed` | 1.1.0 | CLI "local memory operations without a server": auto-starts a daemon on `localhost:8888` with pg0, idle shutdown after 5 min; launches `uvx hindsight-api@<version>` (or `uv run --project …` in the monorepo) | https://github.com/vectorize-io/hindsight/blob/main/hindsight-embed/README.md ; `hindsight-embed/hindsight_embed/daemon_embed_manager.py` l.155-166, l.321-324, l.355-357 | V |
| Entry points | `hindsight-api`, `hindsight-worker`, `hindsight-local-mcp`, `hindsight-admin` | container `pyproject.toml` l.177-181 `[project.scripts]` | V |
| Python in the official image | 3.11.16 | `docker exec hindsight python --version` | V |
| Platform-specific deps | `uvloop` (non-Windows) / `winloop` (Windows); `litellm<1.92` on macOS because "litellm publishes NO macOS wheels for any release >= 1.92.0 … every install compiles the sdist's Rust/PyO3"; floor 1.93 elsewhere for Python 3.14 wheels | container `pyproject.toml` l.73-91 (comments) | V |
| Control Plane UI | `@vectorize-io/hindsight-control-plane` 1.1.0 on npm (ISC), Node CLI `hindsight-control-plane`; in the image it lives in `/app/control-plane`, started by `/app/start-all.sh` | `npm view @vectorize-io/hindsight-control-plane`; `docker exec hindsight ls /app` | V |
| Standalone binary / installer from vectorize for the server | none found (only pip/uvx and Docker/Helm) | repo root listing (`docker`, `helm`, `hindsight-api-slim`, `hindsight-embed`, …) | V (absence in repo) |

## 2. pg0 (embedded Postgres)

| Item | Finding | Source | |
|---|---|---|---|
| What | "Zero-config PostgreSQL with pgvector. A single binary that runs PostgreSQL locally — no installation, no configuration, no Docker"; Rust; CLI `start/stop/drop/info/list/psql/logs`, Python SDK `pg0-embedded` | https://github.com/vectorize-io/pg0 (README) | V |
| Versions bundled | PostgreSQL 18.1.0, pgvector 0.8.5 | https://github.com/vectorize-io/pg0/blob/main/versions.env | V |
| Verified in a live instance | `vector 0.8.5`, `pg_trgm 1.6`, `plpgsql 1.0` | `pg0 psql --name hs-cold -c "select extname, extversion from pg_extension"` | V |
| Binaries (v0.15.2, 2026-09-14) | `pg0-darwin-aarch64` 16 MB, `pg0-darwin-x86_64` 17 MB, `pg0-linux-{x86_64,aarch64}-{gnu,musl}` 32-34 MB, `pg0-windows-x86_64.exe` 60 MB | `gh api repos/vectorize-io/pg0/releases/latest` | V |
| Wheels (`pg0-embedded` 0.15.2) | macosx_14_0_arm64 (13 MB), macosx_15_0_x86_64, manylinux_2_35 x86_64/aarch64 (30 MB), win_amd64 (55 MB); no deps | https://pypi.org/pypi/pg0-embedded/0.15.2/json | V |
| Platform caveats | glibc ≥ 2.35 for the wheels (bundles libxml2 + ICU 70 from Ubuntu 22.04); musl binary needs ICU 74 (Alpine 3.20 ok, 3.22+ not); "PostgreSQL cannot run as root"; Linux needs libssl3, libgssapi-krb5, tzdata, libreadline | README "Docker" section; `versions.env` comments | V |
| Licence | `license = "MIT"` in `Cargo.toml` and wheel metadata; **no LICENSE file** in the repo | https://github.com/vectorize-io/pg0/blob/main/Cargo.toml ; repo tree; C01 | V |
| Data location (run) | installation extracted to `~/.pg0/installation/18.1.0/` (74 MB); instance data `~/.pg0/instances/<name>/data` (41-43 MB empty Hindsight schema); logs `…/data/log/`. SDK/CLI accept `--data-dir` | `pg0 list`, `du`, `ps` (postgres `-D …/.pg0/instances/hs-bench/data -p 18432 -c shared_buffers=256MB …`); `pg0/__init__.py` l.214-248 | V |
| Hindsight docs vs reality | docs: "Starts an embedded PostgreSQL instance on port 5555 … Stores data in `~/.hindsight/pg0/`" | https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/docs/developer/storage.md l.58-63 | V (contradicted by the run: data under `~/.pg0/`, port as given in the URL) |
| How Hindsight uses it | default `DATABASE_URL = "pg0"`; URL forms `pg0://name`, `pg0://name:port`, `pg0://user:pwd@name:port`; Hindsight starts pg0 in parallel with model loading and stops it on shutdown **only if it started it** | container `hindsight_api/config.py` l.1082; `hindsight_api/pg0.py` l.149-170; `engine/memory_engine.py` l.4997-5027 | V |
| Orphan on failed start | After a failed startup (provider init error) the pg0 postgres kept running on its port; it had to be stopped by hand | observed (`hs-tei-llama` run) | V |
| Integrity concern upstream | the image's `start-all.sh` checks pg0 data dirs for a missing `PG_VERSION` because of past data-loss reports (#675) and mentions several on-disk layouts across pg0 versions | container `/app/start-all.sh` header | V |

## 3. Real run (this machine)

| Step | Command / setting | Result | |
|---|---|---|---|
| venv | `uv venv --python 3.12 venv-hs` (uv-managed CPython 3.12.13 already present) | ok | V |
| install | `UV_CACHE_DIR=<empty> uv pip install "hindsight-api-slim[local-onnx,embedded-db]==0.10.1"` | **6.0 s** wall incl. venv, venv **1.1 GB**; `onnxruntime 1.31.0` (providers: Azure, CPU) | V |
| biggest parts | `claude_agent_sdk` 281 MB, `litellm` 114 MB, `onnxruntime` 63 MB, `transformers` 51 MB, `pandas` 45 MB, `pg0` 33 MB, `numpy` 32+27 MB, `botocore` 25 MB | `du site-packages/*` | V |
| with torch (`[all]` / `hindsight-api`) | torch from PyPI on Linux = CUDA build: venv with torch + sentence-transformers measured **5.5 GB** (torch 1.2 GB, `nvidia/*` 3.2 GB, triton 0.9 GB); CPU wheel instead: 1.2 GB | scratchpad `venv-tools`, before/after swapping to `torch==2.14.1+cpu` | V |
| env | `HOME=<sandbox> HINDSIGHT_API_HOST=127.0.0.1 HINDSIGHT_API_PORT=18888 HINDSIGHT_API_DATABASE_URL=pg0://hs-cold:18434 HINDSIGHT_API_LLM_PROVIDER=mock HINDSIGHT_API_EMBEDDINGS_PROVIDER=onnx HINDSIGHT_API_RERANKER_PROVIDER=cohere HINDSIGHT_API_RERANKER_COHERE_BASE_URL=http://127.0.0.1:18081/v1/rerank HINDSIGHT_API_RERANKER_COHERE_API_KEY=dummy` (reranker = llama-server, see C08) | — | V |
| startup, cold | fresh HOME: pg0 extraction + initdb + e5-small onnx download (470 MB HF cache) | **14.8 s** to `/health` 200; pg0 start itself 0.6 s (log 23:10:23.860 → 23:10:24.423) | V |
| startup, warm | existing pg0 instance, models cached | **6.0 s**; new pg0 instance with installation cached: **3.4 s** | V |
| memory | Hindsight process peak RSS | 1.5 GB (`VmHWM` 1 553 864 kB) | V |
| `/health` | `{"status":"healthy","database":"connected",…}`; `/version` → `api_version 0.10.1` | | V |
| bank | `PUT /v1/default/banks/bench` | created | V |
| retain | `POST /v1/default/banks/bench/memories {"items":[5 FR/EN items],"async":false}` | success in 161 ms (mock LLM); 20 memory units listed | V |
| recall | `POST …/memories/recall {"query":"Comment est faite la sauvegarde ?"}` | 106-108 ms; right fact first; trace shows cross-encoder logits normalised by sigmoid | V |
| without local-ml | CLI prints "Local ML provider configured … 'sentence-transformers' is not installed. The API will fail at startup" unless embeddings/reranker providers are set (defaults are `local`) | `hindsight-api --help` output; `config.py` l.1204, l.1264 | V |
| GPU via torch | not tried (torch extra not installed); local provider picks CUDA/MPS if torch sees one | `engine/local_device.py` l.104-134 (C08) | I |

Logs: `bench/results/hs-*.log` in the scratchpad.

## 4. Container runtimes and licences

| Runtime | Licence / cost | Platforms | Compose | GPU in containers | Source | |
|---|---|---|---|---|---|---|
| Docker Engine (Linux) | Apache-2.0 (Moby); unaffected by Desktop terms ("The licensing and distribution terms for Docker and Moby open-source projects, such as Docker Engine, aren't changing") | Linux | `docker compose` plugin | NVIDIA (toolkit), AMD (`/dev/kfd`,`/dev/dri`) | https://docs.docker.com/subscription/desktop-license/ | V |
| Docker Desktop | Free for "Small businesses (fewer than 250 employees AND less than $10 million in annual revenue)", personal use, education, non-commercial OSS; paid (Pro/Team/Business) for "Professional use in larger organizations", government | macOS, Windows, Linux | yes | only Windows + WSL2 + NVIDIA (`--gpus`) | https://docs.docker.com/subscription/desktop-license/ ; https://docs.docker.com/desktop/features/gpu/ | V |
| Podman / Podman Desktop | Apache-2.0 (I) | Linux native; macOS/Windows via `podman machine` | `podman compose` = "thin wrapper around an external compose provider such as docker-compose or podman-compose"; docker-compose preferred if installed; Docker Compose v2 works through Podman's Docker API since 4.1 | macOS: libkrun/krunkit VM with Venus-Vulkan (~75-80 % native per Red Hat) | https://docs.podman.io/en/latest/markdown/podman-compose.1.html ; https://www.redhat.com/sysadmin/podman-compose-docker-compose ; https://developers.redhat.com/articles/2025/09/18/reach-native-speed-macos-llamacpp-container-inference | V (licence I) |
| OrbStack | Free for "Personal, non-commercial use"; Pro "Business & commercial use" $8/user/month | macOS (I: platforms not stated on pricing page) | Docker-compatible | — | https://orbstack.dev/pricing | V (macOS-only I) |
| Colima | MIT | macOS (Intel, Apple Silicon), Linux; no Windows | Docker runtime → docker compose | "GPU accelerated containers for AI workloads" via `krunkit` VM type (Apple Silicon, macOS 13+) | https://github.com/abiosoft/colima | V |
| Rancher Desktop | Apache-2.0, free | macOS, Windows, Linux | moby/dockerd option | — | (not checked) | I |

Our compose file vs Podman (not run: Podman is not installed here and system installs are out of scope):

| Compose feature in `/home/user/ref-stack/compose.yaml` | Podman status | |
|---|---|---|
| `extra_hosts: host.docker.internal:host-gateway` (l.33-34) | `--add-host` accepts `host-gateway` in current Podman, with "no guarantee that Podman can determine the host-gateway address automatically"; on `podman machine` the names are resolved by gvproxy; Podman 3.2 rejected it | V (https://man.archlinux.org/man/podman-run.1.en ; https://github.com/containers/podman/issues/10878) |
| `env_file` with `path:` / `required: false` (l.26-30) | needs a recent Compose spec implementation; works if docker-compose v2 is the provider | I |
| `depends_on: condition: service_healthy`, healthcheck, `shm_size`, named volumes, IPAM subnet | supported by docker-compose v2 over the Podman socket; podman-compose implements "a subset" | I |
| Hindsight images | multi-arch OCI on ghcr.io, should pull with Podman | I |

## 5. What a "no Docker" mode would cost us (facts)

| Concern | Docker mode (today) | No-Docker mode (pip + pg0) | |
|---|---|---|---|
| Runtime to provide | Docker Engine/Desktop (licence for big companies on macOS/Windows) | a Python ≥ 3.11 interpreter + venv: `uv` can install both (`uv python install`, `uv venv`, `uv pip install`) | V |
| Reproducibility | image pins every dependency and the OS libs; cosign-signed images (C01) | resolver output drifts unless we ship a lock (e.g. `uv pip compile --generate-hashes` per OS/arch) — 1.1 GB of transitive deps incl. Rust/PyO3 wheels (litellm, toktok-rs, obstore) | V (sizes) / I (drift) |
| macOS specifics | containers have no GPU | native process can use MPS/MLX (torch extra) or a host llama-server; `litellm` pinned <1.92 on darwin | V |
| Windows specifics | Docker Desktop + WSL2 | pg0 Windows binary (60 MB, longer startup per `daemon_embed_manager.py` l.69 comment), `winloop`; hindsight-embed contains Windows-specific process handling (pythonw trampolines) | V |
| Postgres | `pgvector/pgvector:pg18` container, data in a named volume | pg0 (PG 18.1 + pgvector 0.8.5), data in `~/.pg0/instances/<name>/data`; backup = `pg_dump` via bundled binaries or `pg0 psql` | V |
| Process supervision | `restart: unless-stopped` | must be provided (systemd user unit / launchd / Windows service or a resident Go daemon); orphan pg0 after a failed start observed | V |
| UI | Control Plane in the image | separate Node app (npm) or none | V |
| Size on disk | image (not measured here) | venv 1.1 GB (no torch) / ~5.5 GB (CUDA torch) / ~1.2 GB (CPU torch) + pg0 74 MB + models (e5-small onnx 470 MB) | V |
| Install time | image pull | 6 s on this connection with a cold uv cache (no torch) | V |
| Upstream support | Docker/Helm documented for production | pg0 documented as "Development"; production = PostgreSQL 15+ via `HINDSIGHT_API_DATABASE_URL` | V (storage.md l.65-70) |
| uv itself | — | single static binary per OS (I: Apache-2.0/MIT dual licence, standalone installer); here at `~/.local/bin/uv` | I |

## Implications for hindsight-claude

- Docker can be made optional: the server runs natively from PyPI with an embedded Postgres that already includes pgvector, on all four target OS/arch pairs (pg0 has binaries for each; Hindsight wheels are pure Python plus common native wheels). (§1, §2, §3)
- A native mode means our Go binary would manage a Python environment it did not write: uv (download), a pinned Python, a pinned dependency lock per platform, the `hindsight-api` process, and pg0's lifecycle. Docker hides all of these behind one pinned image. (§5)
- vectorize's own `hindsight-embed` already does "uvx hindsight-api@<version> + pg0" as a daemon, which is precedent for this design and a source to read for Windows/macOS edge cases. (§1)
- Choosing extras controls the footprint: `[local-onnx,embedded-db]` = 1.1 GB with no torch; `hindsight-api` (=`[all]`) drags CUDA torch on Linux (~5.5 GB). GPU reranking does not need torch if it is served by llama-server (C08). (§3)
- pg0 data and Docker volume data are different places; backups/restores and migrations between modes must handle both (`~/.pg0/instances/<name>/data` vs the `pg_data` volume). Hindsight's own docs give the wrong pg0 path. (§2)
- Docker Desktop is a paid product for larger companies; Podman/Colima/Rancher Desktop are free alternatives, and Linux Docker Engine is unaffected. Compatibility of our compose file with Podman is plausible but untested here (`host-gateway`, `env_file.required`). (§4)
- No UI in native mode unless Node + the npm control plane are also managed. (§1)
