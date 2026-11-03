# A GPU reranker

On the CPU, the reranker takes nearly all of a recall: it scores each candidate memory against your prompt. A GPU on the server's machine can run that model instead, through a service you add next to the server. backsight does not install or manage it: this page says what to install on the machine, then gives the service to add, how to check it and how to remove it. A coding agent can follow it from start to end with you.

## When it helps

| Machine | Search on the CPU | Search with the GPU reranker |
|---|---|---|
| NVIDIA RTX A2000 (laptop), Text Embeddings Inference | up to 2.9 s for the slowest prompts | 0.11 to 0.28 s |
| AMD Radeon 890M (integrated), llama.cpp on Vulkan | 0.72 to 0.88 s | 0.33 to 0.39 s |

These are whole `backsight search` calls, measured on real memories (30 candidates per search). The gain grows with the CPU's slowness and with the length of your prompts. doctor's `latency` check warns when the prompt hook's 95th percentile passes 2.5 s over 7 days: that is the clearest sign a GPU reranker is worth it.

Same results: the GPU service runs the very model the server runs on the CPU (`cross-encoder/mmarco-mMiniLMv2-L12-H384-v1`), so memories rank the same in every language. The embeddings stay on the CPU, as backsight sets them: the stored memories were embedded with `multilingual-e5-small`, and another model's vectors do not match them.

## Before you start

- **The Docker runtime.** The service lives in the server's `compose.override.yaml`, next to its `compose.yaml` (`~/.local/share/backsight/server` on Linux; `server.compose_dir` in `config.toml`). Compose merges that file, backsight never writes it, and backups include it. The [native runtime](native-runtime.md) has no such file: a GPU reranker needs Docker in this version.
- **The server's machine only.** With [several PCs](several-pcs.md), only the host runs the reranker; clients change nothing.
- **A GPU the containers can reach.** setup's pre-flight and doctor's `gpu` check list the GPUs they find. Then follow the section for yours: [NVIDIA](#nvidia-text-embeddings-inference), [AMD or Intel](#amd-or-intel-llamacpp-on-vulkan), or [macOS and Windows](#macos-and-windows).
- **Drivers and toolkits are yours.** Each section names the packages the machine needs. Install them with your distribution's package manager, as its documentation says; backsight neither checks nor updates them.

## NVIDIA: Text Embeddings Inference

Hugging Face's Text Embeddings Inference (TEI) serves the reranker on CUDA. Hindsight talks to it with its `tei` provider.

**The machine needs:**

- the NVIDIA driver: `nvidia-smi` lists the GPU;
- the NVIDIA Container Toolkit (`nvidia-container-toolkit`), then Docker set to use it: `sudo nvidia-ctk runtime configure --runtime=docker` and a restart of Docker;
- a check that a container sees the GPU: `docker run --rm --gpus all ubuntu nvidia-smi` lists it.

**Pick the image for the GPU's compute capability**, which `nvidia-smi --query-gpu=name,compute_cap --format=csv` prints:

| Compute capability | GPUs (examples) | Image |
|---|---|---|
| 7.5 | RTX 20xx, T4 | `ghcr.io/huggingface/text-embeddings-inference:turing-1.9.4` (experimental) |
| 8.0 | A100, A30 | `ghcr.io/huggingface/text-embeddings-inference:1.9.4` |
| 8.6 | RTX 30xx, RTX A2000 to A6000, A10, A40 | `ghcr.io/huggingface/text-embeddings-inference:86-1.9.4` |
| 8.9 | RTX 40xx, L4, L40 | `ghcr.io/huggingface/text-embeddings-inference:89-1.9.4` |
| 9.0 | H100 | `ghcr.io/huggingface/text-embeddings-inference:hopper-1.9.4` |
| 10.0, 12.0 | Blackwell | `…:100-1.9.4`, `…:120-1.9.4` (experimental) |

Below 7.5, TEI has no CUDA image: keep the CPU.

**`compose.override.yaml`** (here for compute capability 8.6):

```yaml
# GPU reranker for backsight's server (not managed by backsight).
services:
  reranker:
    image: ghcr.io/huggingface/text-embeddings-inference:86-1.9.4
    restart: unless-stopped
    command:
      - --model-id
      - cross-encoder/mmarco-mMiniLMv2-L12-H384-v1
      - --max-client-batch-size
      - "64"
    volumes:
      - tei_cache:/data
    healthcheck:
      test: ["CMD", "curl", "-sf", "http://localhost:80/health"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 120s
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: 1
              capabilities: [gpu]

  hindsight:
    depends_on:
      reranker:
        condition: service_healthy
    environment:
      HINDSIGHT_API_RERANKER_PROVIDER: tei
      HINDSIGHT_API_RERANKER_TEI_URL: http://reranker:80

volumes:
  tei_cache:
```

- The healthcheck and `depends_on` matter: without them, the server starts before the reranker has loaded its model, fails its first call to it and exits.
- TEI downloads the model from Hugging Face at its first start, into the `tei_cache` volume.
- TEI cuts by itself a prompt and memory longer than the model's 512 tokens, so long searches work unchanged.

Then: `backsight server start`, and [check it works](#check-it-works).

## AMD or Intel: llama.cpp on Vulkan

llama.cpp's `llama-server` serves the reranker on any GPU with a Vulkan driver. Hindsight talks to it with its `cohere` provider: `llama-server` has no `/info` route, which the `tei` provider needs to start. On AMD, Vulkan was faster than llama.cpp's ROCm build on the same GPU, and needs no ROCm install.

**The machine needs:**

- the Mesa Vulkan driver: RADV for AMD (`vulkan-radeon` on Arch, `mesa-vulkan-drivers` on Debian and Ubuntu, Fedora), ANV for Intel (`vulkan-intel` on Arch, the same `mesa-vulkan-drivers` elsewhere);
- the GPU's device nodes under `/dev/dri` (`card*`, `renderD*`);
- the numeric ids of the `render` and `video` groups, which the container joins to open them: `getent group render video`;
- a check that a container sees the GPU (put your two ids after `--group-add`):

```sh
docker run --rm --device /dev/dri --group-add 987 --group-add 983 \
  ghcr.io/ggml-org/llama.cpp:server-vulkan-b11723 --list-devices
```

It lists the GPU as `Vulkan0`. Only the CPU listed: the driver or the groups are missing.

**`compose.override.yaml`** (put your `render` and `video` ids under `group_add`):

```yaml
# GPU reranker for backsight's server (not managed by backsight).
services:
  # One-shot: converts the official weights to GGUF when the file is
  # missing (first start, a new machine, after a restore), then exits.
  reranker-model:
    image: ghcr.io/ggml-org/llama.cpp:full-b11723
    entrypoint: ["sh", "-c"]
    command:
      - |
        set -e
        f=/models/mmarco-mMiniLMv2-L12-H384-v1-f16.gguf
        test -s "$$f" && exit 0
        hf download cross-encoder/mmarco-mMiniLMv2-L12-H384-v1 \
          --revision 1427fd652930e4ba29e8149678df786c240d8825 --local-dir /tmp/m \
          --exclude "onnx/*" "openvino/*" "*.h5" "*.msgpack" "*.ot"
        python3 /app/convert_hf_to_gguf.py /tmp/m --outtype f16 --outfile "$$f.tmp"
        mv "$$f.tmp" "$$f"
    volumes:
      - reranker_models:/models

  reranker:
    image: ghcr.io/ggml-org/llama.cpp:server-vulkan-b11723
    restart: unless-stopped
    command:
      - --model
      - /models/mmarco-mMiniLMv2-L12-H384-v1-f16.gguf
      - --reranking
      - --n-gpu-layers
      - "99"
      - --parallel
      - "32"
      - --ctx-size
      - "16384"
      - --batch-size
      - "2048"
      - --ubatch-size
      - "2048"
    devices:
      - /dev/dri:/dev/dri
    group_add:
      - "987" # render
      - "983" # video
    volumes:
      - reranker_models:/models:ro
    depends_on:
      reranker-model:
        condition: service_completed_successfully
    healthcheck:
      test: ["CMD", "curl", "-sf", "http://localhost:8080/health"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 60s

  hindsight:
    depends_on:
      reranker:
        condition: service_healthy
    environment:
      HINDSIGHT_API_RERANKER_PROVIDER: cohere
      HINDSIGHT_API_RERANKER_COHERE_BASE_URL: http://reranker:8080/v1/rerank
      HINDSIGHT_API_RERANKER_COHERE_MODEL: mmarco-mMiniLMv2-L12-H384-v1
      HINDSIGHT_API_RERANKER_COHERE_API_KEY: unused
      HINDSIGHT_API_RECALL_MAX_QUERY_TOKENS: "250"

volumes:
  reranker_models:
```

- **The model file.** `llama-server` reads GGUF files. `reranker-model` downloads the official weights at the revision the server runs on the CPU and converts them with llama.cpp's own converter, once: about 240 MB in the `reranker_models` volume, a few seconds after the image's download (about 2 GB, the first time). Converted this way, the model scores as on the CPU: 0.9999 rank correlation, and the same keep-or-drop decision on 99.98% of memories. Once the file exists, it exits at once.
- **Keep `HINDSIGHT_API_RECALL_MAX_QUERY_TOKENS`.** `llama-server` refuses a prompt and memory longer than the model's 512 tokens, and the whole search then fails. Prompt recall sends only 300 characters, but `backsight search` and Claude's `memory_search` send what they are given. Capping the query at 250 tokens keeps every pair under the limit (memories are already capped at 128).
- Hindsight's `cohere` provider insists on a key; `llama-server` ignores it.
- The service listens only on the Compose network: no port is published.
- `--parallel 32` scores up to 32 memories at once; it uses about 330 MB of GPU memory on a Radeon 890M.

Intel GPUs take the same recipe with the ANV driver; it was tested on AMD only.

Then: `backsight server start`, and [check it works](#check-it-works).

## macOS and Windows

Neither has been tested with a GPU reranker.

- **macOS**: Docker Desktop gives containers no GPU. Run `llama-server` on the Mac itself (llama.cpp's macOS build uses Metal by default), on a GGUF file made by the `reranker-model` script of [AMD or Intel](#amd-or-intel-llamacpp-on-vulkan) (run it once with `docker run --rm -v "$PWD:/models" --entrypoint sh ghcr.io/ggml-org/llama.cpp:full-b11723 -c '<the script, with $ for $$>'`), with `--reranking --host 127.0.0.1 --port 8081`, and point the server at it: keep only the `hindsight` part of that override, with `HINDSIGHT_API_RERANKER_COHERE_BASE_URL: http://host.docker.internal:8081/v1/rerank` and no `depends_on`.
- **Windows with an NVIDIA GPU and Docker Desktop**: Docker Desktop passes the GPU to containers through WSL 2 once the NVIDIA driver is installed on Windows; the [NVIDIA](#nvidia-text-embeddings-inference) override then applies as it is. The [native runtime](native-runtime.md), Windows' default, has no override file.
- **Windows with an AMD or Intel GPU**: run llama.cpp's Windows Vulkan build on the host, as for macOS above.

A reranker on the host must listen on an address the server's container reaches (`host.docker.internal`); keep it off the network otherwise.

## Check it works

1. `docker compose ps`, in the server's directory, shows `reranker` healthy and `hindsight` running.
2. The server's log names the provider: `docker compose logs hindsight | grep Reranker` shows `provider=tei` or `provider=cohere`.
3. `backsight search "something you know is in memory"` answers with the same memories as before, faster. Run it a few times: the first call after a start warms the GPU.
4. The GPU shows the work: `nvidia-smi` lists the service's process; on AMD and Intel, the service's memory appears in the GPU tools of your desktop, or with `radeontop` / `intel_gpu_top`.
5. Over the next days, doctor's `latency` check and the dashboard's recall times say whether recall got faster.

A search that fails with `memory unavailable` and an HTTP 400 from the reranker in `docker compose logs hindsight` means a prompt and memory went over 512 tokens: see the query cap in [AMD or Intel](#amd-or-intel-llamacpp-on-vulkan).

## Moving from your own reranker

An earlier Hindsight setup of yours may already run a GPU reranker: a Python service on the host, a container of its own. Move it next to backsight's server:

- **Keep it on the host**: point the server at it with only the `hindsight` part of the override. A TEI-compatible service takes `tei` and `HINDSIGHT_API_RERANKER_TEI_URL: http://host.docker.internal:<port>`; any other takes `cohere` as in [macOS and Windows](#macos-and-windows). The service must answer Hindsight's calls: TEI's `/info` and `/rerank`, or a Cohere-style `/v1/rerank`.
- **Or replace it** with one of the services above, then stop the old one.

Either way, it must serve the same model, `cross-encoder/mmarco-mMiniLMv2-L12-H384-v1`: another model ranks differently, and an English-only one misses prompts in other languages.

## Going back to the CPU

Remove the services you added (`reranker`, and `reranker-model` on AMD and Intel) and the `hindsight` part from `compose.override.yaml` (or the whole file if it holds nothing else), then `backsight server start`: the server runs the reranker on the CPU again. `docker compose up` leaves the old containers behind: `docker compose up -d --remove-orphans`, in the server's directory, removes them. The model volume (`backsight_tei_cache` or `backsight_reranker_models`) stays until you delete it with `docker volume rm`; `backsight uninstall --delete-data` does not delete volumes only the override declares.

## Updates and backups

- `backsight update` and `backsight server upgrade` keep `compose.override.yaml` as it is. A later Hindsight release can rename the variables above: the release notes say so, and doctor's `latency` check and a failing search show it.
- The images above are pinned to a release; move to a newer one when you choose.
- Backups include `compose.override.yaml` but not the model volume. On a new machine, install the prerequisites of your GPU before you restore: the server then fetches the model again by itself at its first start (TEI downloads it, `reranker-model` converts it).
