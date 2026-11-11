# C08 — GPU inference without our own Python (checked 2026-10-23)

Legend: **V** = VERIFIED (seen in source/docs or measured here), **I** = INFERRED.
Container paths are inside `ghcr.io/vectorize-io/hindsight:0.10.1` (container `hindsight`), prefix `/app/api/hindsight_api/` omitted.

## TL;DR

- llama.cpp `llama-server` (MIT, prebuilt for every target OS/backend) runs the **exact same reranker** we use today (`cross-encoder/mmarco-mMiniLMv2-L12-H384-v1`, converted to GGUF with llama.cpp's own converter): Spearman 0.9999 and the same ≥0.5 decision on 99.98 % of pairs compared with the live torch-ROCm service. **V**
- On this Radeon 890M, Vulkan is as fast as today's torch-ROCm service on short inputs (p50 543 ms vs 682 ms for 200 candidates) and 1.6× faster on long prompts (2.70 s vs 4.21 s). The prebuilt ROCm/HIP llama.cpp build is slower than Vulkan here (best 1.12 s). **V**
- Hindsight's `tei` reranker provider **refuses to start** against llama-server (it requires `GET /info`, which llama-server lacks: 404, startup fails). The `cohere` provider pointed at llama-server's `/v1/rerank` works end to end; so does the `openai` embeddings provider pointed at `/v1/embeddings`. **V**
- llama-server **does not truncate** rerank inputs: a pair longer than the model's 512 tokens returns HTTP 400 and fails the whole request (sentence-transformers truncates silently). Our hook sends whole user prompts as the query, so this matters. **V**
- Production logs of today's GPU service: p50 632 ms, **p90 4.4 s, max 10.1 s** per request (116 requests), not the 0.1-0.4 s quoted in the brief. The long-prompt benchmark reproduces this (4.2-6.0 s), so the cost driver is likely the query length (the whole prompt), not the candidate count. **V** (logs, bench) / **I** (cause)

## 1. llama.cpp `llama-server`

### 1.1 Rerank and embeddings support

| Item | Finding | Source | |
|---|---|---|---|
| Rerank flag | `--rerank, --reranking` "enable reranking endpoint on server"; or `--embedding --pooling rank` | `tools/server/README.md` l.214, l.778 (tag b11840) | V |
| Rerank routes | `POST /rerank`, `/reranking`, `/v1/rerank`, `/v1/reranking` (same handler) | `tools/server/server.cpp` l.304-307 | V |
| Two wire formats | Body with `texts` → **TEI format**: bare array `[{index, score}]` (+`text` if `return_text`); body with `documents` → Jina/Cohere format `{model, object, usage, results:[{index, relevance_score}]}` | `server-context.cpp` l.6858-6861 (`is_tei_format = body.contains("texts")`); `server-common.cpp` l.1609-1650 | V |
| Score scale | Score = `embd[0]` of the rank-pooled output, i.e. the **raw classifier logit** for BERT/XLM-R cross-encoders (measured range −10.4…+10.9 for mmarco). The README claims "match probability in [0, 1]" — not true for these models | `server-context.cpp` l.2546-2575; README l.839; measured | V |
| No `/info`, no TEI `/embed` | `GET /info` → 404; `POST /embed` → 404. Has `/health`, `/props`, `/v1/models`, `/embedding(s)`, `/v1/embeddings` | `server.cpp` l.280-307; curl probes | V |
| Input limit | Encoder rerankers: whole pair must fit `--ubatch-size` **and** the slot context, capped at the model's training context (512 for mmarco). Longer input → error `input (519 tokens) is larger than the max context size (512 tokens)`, HTTP 400 for the request. No truncate option for rerank | `server-context.cpp` l.3947-3964; README l.843-845; measured with the long workload | V |
| Embeddings | `--embedding --pooling mean|cls|last|rank`, OpenAI-compatible `POST /v1/embeddings`, `--embd-normalize` (default 2 = L2) | `llama-server --help`; README l.749-755 | V |
| Default port | Changed to **9931** in v0.7.0 (Docker images keep 8080) | release notes https://github.com/ggml-org/llama.cpp/releases/tag/v0.7.0 | V |
| Licence | MIT, "Copyright (c) 2023-2026 The ggml authors" | https://github.com/ggml-org/llama.cpp/blob/master/LICENSE | V |
| Release pace | Build tags several times a day (b11826…b11840 on 2026-10-22/23, all "prerelease"); plus semver releases (v0.7.0, 2026-10-15) | `gh api repos/ggml-org/llama.cpp/releases` | V |

### 1.2 Prebuilt binaries (build b11840, 2026-10-23)

| Target | Asset (size) | Notes | |
|---|---|---|---|
| Linux x64 CPU | `llama-b11840-bin-ubuntu-x64.tar.gz` (22 MB) | runtime-dispatched CPU variants (`libggml-cpu-zen4.so`, `-haswell`…) | V |
| Linux x64 Vulkan | `…-ubuntu-vulkan-x64.tar.gz` (35 MB) | needs only the system Vulkan loader + driver (Mesa RADV here) | V |
| Linux arm64 CPU / Vulkan | `…-ubuntu-arm64.tar.gz` (18 MB), `…-ubuntu-vulkan-arm64.tar.gz` (28 MB) | | V |
| Linux x64 ROCm/HIP | `…-ubuntu-rocm-10.0-x64.tar.gz` (250 MB; `libggml-hip.so` 1.1 GB unpacked) | **links the host ROCm** (`libamdhip64.so.7`, `libhipblas.so.3`, `librocblas.so.5` resolved from `/opt/rocm` 7.2.4); kernels for gfx908…gfx1201 incl. **gfx1150**. Without a host ROCm install it does not load | V (`ldd`, `strings`) |
| Linux x64 CUDA | `…-ubuntu-cuda-12.8-x64` (178 MB), `…-cuda-13.4-x64` (159 MB), `…-cuda-13.4-arm64`; separate `cudart-…` archives bundle the CUDA runtime (440-594 MB) | | V |
| Linux x64 others | OpenVINO, SYCL fp16/fp32 | | V |
| macOS arm64 | `…-macos-arm64.tar.gz` (16 MB) contains `libggml-metal.dylib` and `libggml-blas.dylib` (Accelerate); "On MacOS, Metal is enabled by default" | archive listing; https://github.com/ggml-org/llama.cpp/blob/master/docs/build.md | V |
| macOS x64 | `…-macos-x64.tar.gz` (12 MB) | | V |
| Windows x64 | CPU (24 MB), **Vulkan** (37 MB, `ggml-vulkan.dll`), CUDA 12.4/13.4, **ROCm 10.0** (264 MB), SYCL, OpenVINO | | V |
| Windows arm64 | CPU, Vulkan, CUDA 13.4, OpenCL-Adreno | | V |

Source for the list: `gh api repos/ggml-org/llama.cpp/releases/tags/b11840` (36 assets). **V**

### 1.3 Models in GGUF

| Model | Licence | GGUF source | Works in llama-server b11840? | |
|---|---|---|---|---|
| `cross-encoder/mmarco-mMiniLMv2-L12-H384-v1` (today's reranker, 118 M params, XLM-R vocab) | Apache-2.0 | community: `Gramscii-IT/…-GGUF` (f16), `mono-of-pg/…-gguf` (f32), `keisuke-miyako/…-q8_0` / `-q4_k_m`. **We converted it ourselves** from the official HF weights with `convert_hf_to_gguf.py` (arch `XLMRobertaForSequenceClassification`, no patch needed): f32 477 MB, f16 243 MB, q8_0 133 MB | yes, scores match (see §5) | V |
| `BAAI/bge-reranker-v2-m3` (568 M) | Apache-2.0 | `gpustack/bge-reranker-v2-m3-GGUF` (FP16 1.16 GB … Q2_K 366 MB) | yes | V |
| `jinaai/jina-reranker-v2-base-multilingual` (278 M) | **CC-BY-NC-4.0** (non-commercial) | `gpustack/jina-reranker-v2-base-multilingual-GGUF` (FP16 566 MB, Q8_0 305 MB …) | yes | V |
| `Qwen/Qwen3-Reranker-0.6B` (causal reranker, no 512 limit per README) | Apache-2.0 | not tested | README mentions Qwen3(-VL) rerankers | I |
| `intfloat/multilingual-e5-small` (today's embedder, 384 dims) | MIT | community: `keisuke-miyako/multilingual-e5-small-gguf-f16` → **broken tokenizer** (unknown-token ids, cos 0.83 vs HF, top-10 overlap 0.18); `cstr/multilingual-e5-small-GGUF` → **does not load** ("bert model needs to define token type count", it targets CrispEmbed). Stock conversion fails ("BPE pre-tokenizer was not recognized"). **Working workaround**: copy the HF dir, set `architectures=["XLMRobertaModel"]` and drop `pad_token_id` in `config.json`, then convert → tokenization identical to HF, cos ≥ 0.9986 vs sentence-transformers | only with our own conversion | V |
| `intfloat/multilingual-e5-base` | MIT | `dinab/…-Q4_K_M-GGUF`, `cstr/…`, `groonga/…` (not tested; same tokenizer issue expected) | not tested | I |
| `BAAI/bge-m3` (568 M, 1024 dims) | MIT | `ggml-org/bge-m3-Q8_0-GGUF` (635 MB), `gpustack/bge-m3-GGUF` | yes (CLS pooling) | V |

Sources: HF API `https://huggingface.co/api/models?search=…`, model cards' `license` tags (`https://huggingface.co/api/models/<id>`). **V**

### 1.4 Compatibility with Hindsight 0.10.1 providers

| Hindsight provider | What it sends / needs | llama-server | Result (tested with pip Hindsight 0.10.1 on :18888) | |
|---|---|---|---|---|
| reranker `tei` | `GET /info` at init (needs `model_id`), then `POST /rerank {query, texts, return_text:false}` in batches of 128, 8 concurrent; expects `[{index, score}]` | `/rerank` with `texts` answers in TEI shape, **but `/info` is 404** | **Startup fails**: `RuntimeError: Failed to connect to TEI server … HTTP 404 from …/info` → "Application startup failed. Exiting." | V (`engine/cross_encoder.py` l.367-557 incl. l.511-532 `initialize`, l.534-557 request; config l.1275-1276; log `hs-tei-llama.log`) |
| reranker `cohere` + `HINDSIGHT_API_RERANKER_COHERE_BASE_URL=http://127.0.0.1:18081/v1/rerank`, any API key | `POST {base_url} {model, query, documents, top_n, return_documents:false}`, all candidates of a query in one request; reads `results[].relevance_score` | native Jina/Cohere format | **Works**: recall trace shows `cross_encoder_score` −2.41 → normalised 0.083 | V (`engine/cross_encoder.py` l.616-697, l.700-780; factory l.2062-2110) |
| score normalisation | Per request: if all scores ∈ [0,1] pass through, else sigmoid | logits → sigmoid, same as today's service (which also returns logits, despite `reranker/server.py`'s docstring saying "sigmoid scores") | equivalent behaviour | V (`engine/search/reranking.py` l.391-409; live service range −10.41…+10.86) |
| embeddings `tei` | `GET /info` + `POST /embed {inputs}` | both 404 | incompatible | V (`engine/embeddings.py` l.648-825) |
| embeddings `openai` + `HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL=http://127.0.0.1:18082/v1` (+ `HINDSIGHT_API_EMBEDDINGS_QUERY_PREFIX="query: "`, `…_PASSAGE_PREFIX="passage: "` for e5) | `/v1/embeddings` | native | **Works**: dim 384 detected, retain + recall correct | V (`engine/embeddings.py` l.827-880; `config.py` l.469, l.491; log `hs-openai-emb-llama.log`) |
| reranker `litellm` / `siliconflow` / `zeroentropy` | other Cohere-like paths | not tested | — | I |

## 2. HuggingFace Text Embeddings Inference (TEI)

| Item | Finding | Source | |
|---|---|---|---|
| Licence / version | Apache-2.0; latest v1.9.4 (2026-09-15) | `gh api repos/huggingface/text-embeddings-inference` | V |
| CUDA images | `turing-1.9` (exp.), `1.9` (Ampere 80), `86-1.9`, `89-1.9`, `hopper-1.9`, `100-/120-/121-1.9` (Blackwell, exp.); compute capability < 7.5 unsupported | https://github.com/huggingface/text-embeddings-inference (README) | V |
| CPU images | `cpu-1.9` (x86_64), `cpu-arm64-1.9` (no Metal in Docker) | same | V |
| ROCm | **Experimental, "Only AMD Instinct GPUs (MI200, MI300 series) are tested"**; procedure = build from source inside `rocm/pytorch` (Rust router + Python backend). README mentions a `rocm-latest` tag; consumer Radeon/APU not covered | https://huggingface.co/docs/text-embeddings-inference/amd_gpu.md | V |
| Apple | Metal only for local installs (Homebrew binary, or `cargo install … --features metal`); not in Docker | README | V |
| Windows | no native binary; Docker CPU/CUDA (WSL2) | README | I |
| Reranker archs | "CamemBERT, RoBERTa, XLM-RoBERTa, and GTE Sequence Classification" (mmarco = XLM-R → supported) | README | V |
| API | `/rerank {query, texts, raw_scores=false, return_text, truncate=false, truncation_direction}`, `/info` (model_id, max_input_length, …), `/embed` | https://raw.githubusercontent.com/huggingface/text-embeddings-inference/main/docs/openapi.json | V |
| Hindsight fit | It is the reference of Hindsight's `tei` providers (drop-in). TEI default `raw_scores=false` (I: sigmoid applied server-side, then Hindsight passes [0,1] through) | `engine/cross_encoder.py` l.367-375 | V / I |

## 3. onnxruntime (Go)

| Item | Finding | Source | |
|---|---|---|---|
| Latest | 1.31.0 (2026-10-09); prebuilt C libs: linux x64/aarch64, linux x64 CUDA 12/13, osx-arm64, win x64/arm64, win CUDA 12/13. No ROCm/MIGraphX/DirectML archive | `gh api repos/microsoft/onnxruntime/releases/latest` | V |
| EPs | CPU, CUDA, TensorRT, TensorRT-RTX, OpenVINO, oneDNN, DirectML, QNN, NNAPI, XNNPACK, MIGraphX, WebGPU; **ROCm marked deprecated**; CoreML "preview" | https://onnxruntime.ai/docs/execution-providers/ | V |
| ROCm EP | **Removed from the source tree in 1.23.0**; AMD users pointed to MIGraphX or Vitis AI | 1.23.0 release notes (https://newreleases.io/project/pypi/onnxruntime/release/1.23.0) | V |
| DirectML | "DirectML is in sustained engineering"; "new feature development has moved to WinML" | https://onnxruntime.ai/docs/execution-providers/DirectML-ExecutionProvider.html | V |
| Go bindings | `yalue/onnxruntime_go` (MIT): **needs cgo** + shipping the onnxruntime shared lib (`SetSharedLibraryPath`); TensorRT, CUDA, CoreML "tested and confirmed to work" | https://github.com/yalue/onnxruntime_go | V |
| CGO-free option | `cnxxy-cn/onnxruntime_purego` (MIT, purego/dlopen, targets ORT 1.24) — unverified, small project | https://pkg.go.dev/github.com/cnxxy-cn/onnxruntime_purego | V (exists) / I (quality) |
| Tokenizer | ONNX models need an XLM-R SentencePiece tokenizer in Go (not part of ORT) | — | I |
| Hindsight's own onnx | `OnnxEmbeddings` hard-codes `providers=["CPUExecutionProvider"]`: embeddings only, CPU only | `engine/embeddings.py` l.440, l.557 | V |

## 4. Hindsight's own options

| Option | Finding | Source | |
|---|---|---|---|
| `local` (sentence-transformers/torch) | default for embeddings (`BAAI/bge-small-en-v1.5`) and reranker (`cross-encoder/ms-marco-MiniLM-L-6-v2`); device = CUDA if `torch.cuda.is_available()`, else MPS, else CPU. The official image is CPU-only torch; GPU needs a GPU torch inside the container or on the host | `config.py` l.1204-1266; `engine/local_device.py` l.104-134 | V |
| `onnx` embeddings | default model `intfloat/multilingual-e5-small`, `onnx/model.onnx`, mean pooling, prefixes `query: `/`passage: `, CPU only | `config.py` l.1208-1222; `engine/embeddings.py` l.557 | V |
| `jina-mlx` reranker | jina-reranker-v3 on Apple Silicon via MLX (`mlx`, `mlx-lm` extras, darwin/arm64 only); code and model **CC BY-NC 4.0** | `engine/jina_mlx_reranker.py` l.1-13; `engine/cross_encoder.py` l.1571-1664; HF `jinaai/jina-reranker-v3-mlx` | V |
| `flashrank`, `rrf` (no rerank), remote APIs (cohere, google, …) | available | `engine/cross_encoder.py` factory l.2062-2223 | V |
| Container on macOS / Windows | Docker Desktop GPU passthrough only on Windows + WSL2 + NVIDIA; no Metal in containers | https://docs.docker.com/desktop/features/gpu/ | V |

## 5. Benchmarks on this machine

### 5.1 Setup

| Item | Value |
|---|---|
| Hardware | AMD Ryzen AI 9 HX PRO 370 (12C/24T), Radeon 890M iGPU (gfx1150, RDNA 3.5, shared memory), 54 GB RAM |
| OS / drivers | CachyOS, kernel 7.2.10-1-cachyos; Vulkan: Mesa 26.2.5 RADV (STRIX1), API 1.4.354; ROCm 7.2.4 (`/opt/rocm`) |
| llama.cpp | build b11840 (commit 36e36e63a, "0.7.0-dev"), prebuilt `ubuntu-vulkan-x64`, `ubuntu-x64` (CPU), `ubuntu-rocm-10.0-x64` |
| Reference (a) | live `hindsight-reranker` service: torch ROCm wheel, sentence-transformers 5.2.0, transformers 5.12.1, fp16, batch 64, length-sorted (`/home/user/ref-stack/reranker/server.py`), called read-only at `http://172.17.0.1:8081/rerank` |
| Python CPU reference | sentence-transformers 5.2.0 / transformers 5.12.1 / torch 2.14.1+cpu, fp32, 12 threads, batch 64 (same stack as the container's `local` fallback) |
| Workload "short" | 200 passages (100 FR + 100 EN, 200-348 chars, avg 273), 20 queries (10 FR + 10 EN, one-line questions); generated deterministically (seed 42) from topic sentence pools mimicking memory facts (deploy, backup, reranker, embeddings, Go, hooks, LLM, off-topic) |
| Workload "long" | same passages prefixed like Hindsight does (`[Date: …] <context>: <fact>`, avg 325 chars) and queries = question + ~1400 chars of pasted context, **cut to 1100 chars** so every pair fits 512 tokens (the uncut 1500-char version makes llama-server return 400) |
| Measure | per request = 1 query × 200 candidates, client wall time (Python urllib, keep-alive off); 2 warm-up requests; short: 20 queries × 3 = 60 samples, long: × 2 = 40 samples. Load time = exec → `/health` 200 |
| Memory | peak RSS = `VmHWM`; "VRAM Δ" = change of `rocm-smi` total VRAM used — **noisy** (the live torch service shares the GPU and its allocator moves), indicative only |

Commands (paths under the scratchpad `bench/`):

```bash
# server, one per run (Vulkan example; CPU = same without -ngl; ROCm = llama-rocm build)
llama-server -m mmarco-mMiniLMv2-L12-H384-v1-f16.gguf --reranking -ngl 99 \
  -np 32 -c 16384 -b 16384 -ub 2048 --host 127.0.0.1 --port 18081
# embeddings
llama-server -m multilingual-e5-small-own-f16.gguf --embedding --pooling mean -ngl 99 \
  -np 8 -c 4096 -b 4096 -ub 4096 --port 18082
# conversion of the official weights
python convert_hf_to_gguf.py <hf snapshot of cross-encoder/mmarco-mMiniLMv2-L12-H384-v1> --outtype f16|q8_0 --outfile …
# client
python -I scripts/bench_rerank.py results/data.json http://127.0.0.1:18081 tei out.json 3 <label>
python -I scripts/compare.py results/rr-a-torch-rocm-fp16.json out.json   # agreement
python -I scripts/quality.py results/data.json out.json                  # topical P@k
```

Model files (sha256 prefix, bytes): mmarco f16 `1116037dfab84887` 242 628 704; mmarco q8_0 `7e14c51fce2ad35b` 132 582 848 (both converted from HF snapshot `1427fd65…`); bge-reranker-v2-m3 FP16 `5df93be121c09c43` 1 159 776 896, Q8_0 `a43c7c9b11a4c151` 635 676 416; jina-reranker-v2 FP16 `3fc100523b450b55` 565 520 224, Q8_0 `4dba405265ce8f00` 305 339 552; e5-small own f16 `22a2a16e6d25fe20` 242 358 848, own q8_0 `8f2ab6340b22f066` 132 438 976; bge-m3 q8_0 `aa473d51f451a22f` 634 553 760. **V**

### 5.2 Reranker latency — short workload (200 candidates per request)

| # | Backend | Model / quant | Server params | p50 ms | p95 ms | Load s | Peak RSS MB | VRAM Δ MB |
|---|---|---|---|---|---|---|---|---|
| a | torch-ROCm service (live) | mmarco fp16 | batch 64 | **682** | 707 | ~6 (journal: exec 13:35:33 → ready 13:35:39) | 2 073 (VmHWM; RSS now 314 + 1 GB swapped) | KFD reports 13.2 GB for the process (I: PyTorch caching allocator in shared memory) |
| b | llama Vulkan | mmarco f16 | np 32, ub 16384 | **543** | 595 | 0.97 | 265 | ~750 |
| b | llama Vulkan | mmarco f16 | np 32, ub 2048 | 559 | 587 | — | — | — |
| b | llama Vulkan | mmarco f16 | np 8, ub 4096 | 616 | 700 | — | 286 | ~130 |
| b | llama Vulkan | mmarco f16 | np 64 | 570 | 607 | 0.98 | 276 | ~2 490 |
| b | llama Vulkan | mmarco f16 | np 32, **`-fa off`** | 2 707 | 2 893 | 1.09 | 284 | — |
| b | llama Vulkan | mmarco q8_0 | np 32 | 577 | 623 | 1.07 | 268 | ~60 |
| b | llama Vulkan | jina-v2 FP16 | np 32 | 1 206 | 2 558 | 1.51 | 233 | ~225 |
| b | llama Vulkan | jina-v2 Q8_0 | np 32 | 1 173 | 1 251 | 1.08 | 235 | ~150 |
| b | llama Vulkan | bge-v2-m3 FP16 | np 32 | 3 648 | 3 758 | 1.29 | 268 | ~680 |
| b | llama Vulkan | bge-v2-m3 Q8_0 | np 32 | 3 208 | 3 326 | 0.97 | 242 | ~380 |
| c | llama ROCm/HIP | mmarco f16 | np 8, ub 512 | **1 123** | 1 156 | 1.30 | 604 | — |
| c | llama ROCm/HIP | mmarco f16 | np 32, ub 1024 | 1 527 | 1 610 | — | — | — |
| c | llama ROCm/HIP | mmarco f16 | np 32, ub 16384 | 3 827 | 4 185 | 7.54 (first run) | 1 797 | — |
| c | llama ROCm/HIP | mmarco f16 | np 32, `HSA_OVERRIDE_GFX_VERSION=11.0.0` | 2 932 | 3 021 | 4.75 | 1 894 | — |
| d | llama CPU (24 thr) | mmarco f16 | np 8 | 2 085 | 2 172 | 0.76 | 451 | 0 |
| d | llama CPU | mmarco f16 | np 32 | 2 458 | 2 624 | 0.65 | 541 | 0 |
| d | llama CPU | mmarco q8_0 | np 8 | **1 409** | 1 555 | 0.65 | 345 | 0 |
| d | llama CPU | jina-v2 Q8_0 | np 8 | 3 992 | 4 354 | 0.97 | 526 | 0 |
| d | llama CPU | bge-v2-m3 Q8_0 | np 8 | 13 102 | 13 548 | 0.75 | 840 | 0 |
| d | Python CPU (ST fp32, 12 thr) | mmarco fp32 | batch 64 | 1 672 | 1 743 | 1.27 (warm cache) | — | 0 |

All **V** (raw JSON per run in `bench/results/rr-*.json`, server logs `log-*.txt.gz`). Notes: Vulkan flash attention is on by default (`-fa auto`), turning it off costs 5×. The ROCm build warns "Flash Attention is assigned to device CPU" with large ubatches. **V**

### 5.3 Reranker latency — long workload (query = 1100-char prompt, 200 candidates)

| Backend | Model | p50 ms | p95 ms | |
|---|---|---|---|---|
| torch-ROCm service (live), uncut 1500-char queries | mmarco fp16 | 5 965 | 6 344 | V |
| torch-ROCm service (live) | mmarco fp16 | 4 212 | 4 457 | V |
| llama Vulkan (np 16, c 8192, ub 2048) | mmarco f16 | **2 705** | 2 862 | V |
| llama CPU (np 16) | mmarco f16 | 10 965 | 11 474 | V |
| Python CPU (ST fp32) | mmarco fp32 | 7 793 | 8 589 | V |
| llama Vulkan, uncut 1500-char queries | mmarco f16 | **HTTP 400** (pairs of 513-519 tokens rejected) | | V |

Production comparison (today's service, `journalctl --user -u hindsight-reranker`, 116 requests from Hindsight before this bench): p10 122 ms, **p50 632 ms, p90 4 356 ms, max 10 097 ms**; e.g. "rerank 128 texts in 2733 ms", "65 texts in 3855 ms" (Hindsight's TEI client splits 200 candidates into 128 + 72 concurrent requests, serialised by the service lock). **V**

### 5.4 Score agreement vs the live mmarco service (short workload, 20 × 200 pairs)

Both sides normalised as Hindsight does (sigmoid of logits); the hook keeps a memory when normalised score ≥ 0.5.

| Run | Spearman ρ mean / min | Top-10 overlap mean / min | Raw score range | Max abs diff (normalised) | ≥0.5 decision agreement | Pairs ≥0.5 ref / run |
|---|---|---|---|---|---|---|
| Python CPU ST fp32 | 1.0000 / 1.0000 | 1.00 / 1.0 | [−10.41, 10.86] | 0.003 | 1.0000 | 217 / 217 |
| llama Vulkan mmarco f16 | 0.9999 / 0.9999 | 1.00 / 1.0 | [−10.41, 10.86] | 0.021 | 0.9998 | 217 / 216 |
| llama Vulkan mmarco q8_0 | 0.9995 / 0.9991 | 0.98 / 0.9 | [−10.42, 10.85] | 0.074 | 1.0000 | 217 / 217 |
| llama CPU mmarco f16 | 1.0000 / 0.9999 | 1.00 / 1.0 | [−10.41, 10.86] | 0.019 | 1.0000 | 217 / 217 |
| llama CPU mmarco q8_0 | 0.9995 / 0.9989 | 0.98 / 0.9 | [−10.42, 10.87] | 0.098 | 0.9995 | 217 / 217 |
| llama ROCm mmarco f16 | 0.9999 / 0.9999 | 0.99 / 0.9 | [−10.41, 10.86] | 0.028 | 1.0000 | 217 / 217 |
| llama Vulkan bge-reranker-v2-m3 FP16 | 0.5807 / −0.0669 | 0.80 / 0.4 | [−11.05, 6.68] | 0.981 | 0.9780 | 217 / 167 |
| llama Vulkan bge-reranker-v2-m3 Q8_0 | 0.5776 / −0.0728 | 0.80 / 0.4 | [−11.05, 6.65] | 0.981 | 0.9782 | 217 / 168 |
| llama Vulkan jina-v2 FP16 | 0.6363 / 0.3230 | 0.81 / 0.4 | [−3.74, 1.96] | 0.590 | 0.9852 | 217 / 190 |
| llama Vulkan jina-v2 Q8_0 | 0.6352 / 0.3204 | 0.81 / 0.4 | [−3.74, 1.96] | 0.595 | 0.9850 | 217 / 189 |

Long workload: llama Vulkan f16 ρ 0.9998, ≥0.5 agreement 0.9990 (1059 / 1055 pairs); Python CPU fp32 ρ 1.0000. **V**

Spearman over all 200 candidates is dominated by the order among irrelevant ones; the low ρ for bge/jina means "different model", not "worse". Coarse topical quality (a passage is relevant when it contains a sentence of the query's topic; 20 queries):

| Run | P@5 | P@10 | nDCG@10 | Pairs ≥0.5 | Precision ≥0.5 | Recall ≥0.5 |
|---|---|---|---|---|---|---|
| mmarco (live service) | 0.970 | 0.915 | 0.938 | 217 | 0.968 | 0.346 |
| mmarco Vulkan f16 | 0.970 | 0.915 | 0.938 | 216 | 0.972 | 0.346 |
| bge-reranker-v2-m3 | 0.960 | 0.895 | 0.920 | 167 | 1.000 | 0.275 |
| jina-reranker-v2 | 0.960 | 0.930 | 0.945 | 190 | 1.000 | 0.313 |

The 0.5 threshold is model-specific: with the same threshold bge keeps 23 % fewer pairs and jina 12 % fewer than mmarco. **V** (measured; the labels are synthetic, so only large gaps mean anything — I)

### 5.5 Embeddings (200 passages in requests of 32; 20 single-query requests)

| Backend | Model / quant | Passages/s | Query p50 ms | Load s | Peak RSS MB | Cos vs ST fp32 (min / mean) | Top-10 retrieval overlap vs ST |
|---|---|---|---|---|---|---|---|
| Python CPU ST fp32 (proxy of the container's onnx CPU path) | e5-small fp32 | 146 | 10.4 | 1.24 | — | 1 / 1 | 1.00 |
| llama Vulkan | e5-small own f16 | **347** | 4.4 | 0.65 | 217 | 0.9987 / 0.9998 (passages), 0.9987 / 0.9995 (queries) | 0.965 |
| llama Vulkan | e5-small own q8_0 | 321 | 4.1 | 0.97 | 216 | 0.9989 / 0.9997 | 0.960 |
| llama CPU | e5-small own f16 | 121 | 4.2 | 0.97 | 434 | 0.9989 / 0.9998 | 0.970 |
| llama CPU | e5-small own q8_0 | 182 | 3.1 | 0.97 | 329 | 0.9988 / 0.9997 | 0.960 |
| llama Vulkan | e5-small community F16 (keisuke-miyako) | 410 | 4.8 | 0.75 | — | **0.78 / 0.83** (broken tokenizer) | 0.18 |
| llama Vulkan / CPU | bge-m3 Q8_0 (1024 dims) | 64 / 18 | 17 / 19 | 1.29 / 0.65 | — | other model | — |

All **V** (`bench/results/emb-*.json`). The near-but-not-exact match (cos ≥ 0.9986) means GGUF e5 vectors are close to, not identical with, the vectors already stored by the onnx provider (I: mixing them in one bank would slightly perturb similarity; a re-embed avoids the question).

## 6. Surprises / blockers

1. `tei` provider + llama-server = Hindsight startup failure (`/info` missing). Use the `cohere` provider, or put a shim in front that serves `/info`. **V**
2. No truncation in llama-server rerank: any query+passage > 512 tokens → HTTP 400 for the whole batch. Today sentence-transformers truncates silently. Something (hook, shim, or Hindsight `max_tokens_per_doc`-like option — only litellm providers have it, `engine/cross_encoder.py` l.1370, l.1426) must cap the query. **V**
3. Today's GPU reranker is much slower in production than the brief says (p90 4.4 s); the long-prompt bench reproduces it. Capping the query length is likely a bigger win than the backend choice. **V** / **I**
4. The prebuilt llama.cpp ROCm build needs a host ROCm install and is 2× slower than Vulkan on gfx1150. **V**
5. Community e5-small GGUFs are broken or incompatible; a self-made conversion with a config tweak works. **V**
6. jina-reranker-v2 (and Hindsight's `jina-mlx` v3) are CC-BY-NC-4.0. **V**
7. Hindsight PyPI/GitHub is at 1.1.0 (2026-10-21) while the reference stack runs 0.10.1; the provider code was checked on 0.10.1 only. **V**

## Implications for hindsight-claude

- A Go binary can serve rerank + embeddings on GPU on every target with **zero Python** by downloading a pinned prebuilt `llama-server` (MIT) per OS/backend: Vulkan (Linux, Windows; AMD/Intel/NVIDIA), Metal (macOS arm64), CUDA (NVIDIA), CPU everywhere. Vulkan needs only the GPU driver; ROCm/CUDA builds need host runtimes or the large `cudart` bundle. (facts §1.2)
- On this machine Vulkan matches or beats today's torch-ROCm service, with 0.2-0.3 GB RSS vs ~2 GB and ~1 s load vs ~6 s. (§5.2, §5.3)
- Keeping the current mmarco model is possible (self-converted GGUF, 133-243 MB, Apache-2.0) and keeps scores interchangeable with the CPU fallback and the hook's 0.5 threshold; switching models changes how many memories pass 0.5. (§5.4)
- Wiring into Hindsight 0.10.1 needs no Hindsight change: reranker `cohere` + base URL `…/v1/rerank`, embeddings `openai` + base URL `…/v1` + e5 prefixes. The `tei` provider needs `/info`, which llama-server lacks. (§1.4)
- Inputs over 512 tokens must be bounded before llama-server, otherwise recalls fail; the query (user prompt) is the long side. (§1.1, §5.3)
- If a GPU service is added, the container's CPU `local` member remains a usable failover (Python CPU 1.7 s short / 7.8 s long), faster than llama.cpp CPU on long inputs (11 s). (§5.2, §5.3)
- Embeddings on GPU are 2.4× faster than CPU here (347 vs 146 passages/s), but embeddings are not on the latency-critical path at this size (one query ≈ 4-10 ms either way); GGUF vectors are not bit-identical to existing onnx vectors. (§5.5)
- onnxruntime from Go means cgo (or an unproven purego binding) plus shipping native libs, and its AMD story is MIGraphX only (ROCm EP removed). TEI covers CUDA well but AMD only for Instinct and Apple only outside Docker. (§2, §3)
