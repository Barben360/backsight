# 0011 — The Hindsight stack runs in Docker Compose only (v1)

- **Date**: 2026-10-23
- **Status**: accepted
- **Decided by**: author
- **Settles part of**: D3 (architecture)

## Decision
In v1 the server side (Hindsight API + UI, Postgres with pgvector) runs only as a Docker Compose project managed by the binary. A native run (`hindsight-api-slim` + embedded `pg0`, research/C09) is postponed: it is mostly infrastructure and can come later without changing the client.

## Why
- It is what runs today on both of the author's machines, with known migrations, backups and failure modes.
- The native path still needs Python ≥ 3.11, ships no UI, and left `pg0` running after a failed start (research/C09).

## Consequences
- Docker (Engine on Linux, Desktop or a compatible engine elsewhere) is a prerequisite that the installer checks and explains. Docker Desktop licensing (free under 250 employees and $10 M revenue) is mentioned in the docs.
- GPU acceleration for the reranker and embeddings stays a host-side service reached from the containers (research/C08); its form is decided in D8.
- Podman compatibility of the compose file is untested; `host-gateway` and `env_file` with `required: false` are the known risks.
