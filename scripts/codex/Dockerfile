# The Codex test image (Codex epic, issue 285; reused by CI in issue 284):
# the official Codex CLI release, checksum verified, and the tools the
# harness drives it with. backsight itself is never baked in: run.sh mounts
# a development build read-only, so no rebuild per change.
FROM debian:trixie@sha256:9c6e381421d0f48d1b8133e573f33c185ee16cbf505eda9c4a8a4791b25ea8a6

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl jq tmux zstd sqlite3 bubblewrap procps git \
 && rm -rf /var/lib/apt/lists/*

# The release asset and its sha256, as GitHub lists the asset's digest
# (gh release view rust-v0.167.0 -R openai/codex --json assets).
ARG CODEX_VERSION=0.167.0
ARG CODEX_SHA256=9d64836106d5b374e90ee4454c7ebcde7ca7106558bd7e41a34b9f26ebda251a
RUN set -eu; d=$(mktemp -d); \
    curl -fsSL -o "$d/codex.tar.gz" "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-x86_64-unknown-linux-musl.tar.gz"; \
    echo "${CODEX_SHA256}  $d/codex.tar.gz" | sha256sum -c -; \
    tar -xzf "$d/codex.tar.gz" -C "$d"; \
    install -m 0755 "$d/codex-x86_64-unknown-linux-musl" /usr/local/bin/codex; \
    rm -rf "$d"; \
    codex --version
# The code-mode host, a separate asset of the same release: the agent that
# consolidates Codex's own memories runs its tools through it.
ARG CODE_MODE_HOST_SHA256=b5074d5dd64643b2c073927e494a588eff31c1f5a27f12c1e2c45f9a6cf07dd4
RUN set -eu; d=$(mktemp -d); \
    curl -fsSL -o "$d/host.tar.gz" "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-code-mode-host-x86_64-unknown-linux-musl.tar.gz"; \
    echo "${CODE_MODE_HOST_SHA256}  $d/host.tar.gz" | sha256sum -c -; \
    tar -xzf "$d/host.tar.gz" -C "$d"; \
    install -m 0755 "$d/codex-code-mode-host-x86_64-unknown-linux-musl" /usr/local/bin/codex-code-mode-host; \
    rm -rf "$d"

# A plain user: Codex and backsight never run as root in the tests. run.sh
# runs the container as the caller's uid (its files in the output folder
# stay the caller's), so the home is open to any uid.
RUN useradd -m -u 1000 -s /bin/bash tester && chmod 0777 /home/tester
USER tester
WORKDIR /home/tester
