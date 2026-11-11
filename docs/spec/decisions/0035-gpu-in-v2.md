# 0035 — GPU acceleration in v2

- **Date**: 2026-10-24
- **Status**: accepted
- **Decided by**: author (Q3)

## Decision
v1 runs embeddings and the reranker on CPU inside the Hindsight container and only detects and reports GPUs. The acceleration service (pinned prebuilt `llama-server`, Vulkan/CUDA/Metal, C08) is a v2 feature. Until then a user's own GPU service lives in `compose.override.yaml` (or on the host) and is reported by `doctor`, never managed.

## Why
Speed is not a v1 goal (author, 2026-10-23); the service adds per-vendor detection, a native-process exception on macOS (no GPU in Docker Desktop), a converted GGUF model and a per-vendor test matrix.
