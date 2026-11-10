#!/usr/bin/env bash
# Runs a real Codex CLI against a development build of backsight, inside a
# container, with a deterministic fake model and a scratch memory server
# (Codex epic, issue 285; CI reuses it, issue 284). Nothing touches the
# host's ~/.codex or backsight: CODEX_HOME and BACKSIGHT_HOME live in the
# container and die with it.
#
#   scripts/codex/run.sh [api port]
#
# By default it records the fixtures (scripts/codex/record.sh) into $OUT
# (default: a new directory, printed). HARNESS_CMD runs another command in
# the prepared container instead, e.g. HARNESS_CMD=bash for a shell. The
# memory stack is Hindsight with its mock LLM, as scripts/sandbox.sh starts
# it, on port 28800 + BACKSIGHT_CI_SLOT x 10 (scripts/ports.sh);
# CODEX_PROJECT names its Compose project (default bs-codex, bs-codex-s<n>
# for a CI slot; never backsight). The Codex container sits on an internal
# network with the server alone, so nothing it runs reaches the internet
# (without a server: no network at all). KEEP=1 leaves the stack and the work directory. NO_SERVER=1
# skips the stack: the hooks then run with the server unreachable.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
# shellcheck source=scripts/ports.sh
. "$root/scripts/ports.sh"
# shellcheck source=scripts/hf-cache.sh
. "$root/scripts/hf-cache.sh"
port=$(slot_port 28800 "${1:-}")
# One project (and container) per CI slot, so two slots run side by side.
project=${CODEX_PROJECT:-bs-codex${BACKSIGHT_CI_SLOT:+-s$BACKSIGHT_CI_SLOT}}
[ "$project" != backsight ] || { echo "CODEX_PROJECT=backsight would be the machine's own install" >&2; exit 2; }
net=${project}-offline
# The image is named after its Dockerfile: a changed one is rebuilt.
image=${CODEX_IMAGE:-bs-codex-test:$(sha256sum "$here/Dockerfile" | cut -c1-12)}
work=$(mktemp -d)
out=${OUT:-$work/out}
mkdir -p "$out" "$work/in" "$work/bin"
export BACKSIGHT_HOME=$work/in/backsight
cleanup() {
  if [ "${KEEP:-}" != 1 ]; then
    docker network disconnect -f "$net" "${project}-hindsight-1" >/dev/null 2>&1 || true
    docker network rm "$net" >/dev/null 2>&1 || true
    if [ -f "$BACKSIGHT_HOME/data/server/compose.yaml" ]; then
      docker compose -p "$project" -f "$BACKSIGHT_HOME/data/server/compose.yaml" --env-file "$BACKSIGHT_HOME/data/server/.env" down -v >/dev/null 2>&1 || true
    fi
    [ -n "${OUT:-}" ] || cp -r "$out" "${work}.out" 2>/dev/null || true
    rm -rf "$work"
  fi
}
trap cleanup EXIT

# The builds the container runs, static so the image needs no Go.
(cd "$root" && CGO_ENABLED=0 go build -o "$work/bin/backsight" ./cmd/backsight &&
  CGO_ENABLED=0 go build -o "$work/bin/fakeresponses" ./scripts/codex/fakeresponses &&
  CGO_ENABLED=0 go build -o "$work/bin/probemcp" ./scripts/codex/probemcp)
docker image inspect "$image" >/dev/null 2>&1 || docker build -q -t "$image" "$here" >/dev/null

network=(--network none)
if [ "${NO_SERVER:-}" != 1 ]; then
  bin=$work/bin/backsight
  mkdir -p "$BACKSIGHT_HOME/config"
  cat > "$BACKSIGHT_HOME/config/config.toml" <<TOML
schema_version = 1
[server]
url = "http://127.0.0.1:$port"
bank = "codex"
project = "$project"
ui_port = $((port + 1111))
compose_dir = "$BACKSIGHT_HOME/data/server"
[llm]
provider = "mock"
model = "mock-model"
TOML
  printf 'sandbox-not-a-key\n' | "$bin" secret set llm_api_key >/dev/null
  for name in api_key ui_key db_password; do
    openssl rand -hex 24 | "$bin" secret set "$name" >/dev/null
  done
  "$bin" server render >/dev/null
  hfc_seed "$project"
  docker compose -p "$project" -f "$BACKSIGHT_HOME/data/server/compose.yaml" --env-file "$BACKSIGHT_HOME/data/server/.env" up -d --quiet-pull
  for _ in $(seq 1 120); do
    curl -fsS "http://127.0.0.1:$port/health" >/dev/null 2>&1 && break
    sleep 5
  done
  curl -fsS "http://127.0.0.1:$port/health" >/dev/null
  hfc_save "$project"
  "$bin" bank apply >/dev/null
  # An internal network (no route out) holds the Codex container and the
  # server, reached by its service name: the recording runs offline, and the
  # host's own ports stay out of its reach. Hindsight keeps its Compose
  # network too, which it needs to fetch its models at the first start.
  docker network create --internal "$net" >/dev/null
  docker network connect --alias hindsight "$net" "${project}-hindsight-1"
  network=(--network "$net" -e "BS_SERVER_URL=http://hindsight:8888")
fi

tty=()
cmd=(/opt/harness/record.sh)
if [ -n "${HARNESS_CMD:-}" ]; then
  read -r -a cmd <<< "$HARNESS_CMD"
  [ ! -t 0 ] || tty=(-it)
fi
echo "codex harness: work $work, fixtures $out" >&2
docker run --rm --name "${project}-codex" "${tty[@]}" "${network[@]}" \
  -v "$work/bin:/opt/bs-build:ro" \
  -v "$here:/opt/harness:ro" \
  -v "$work/in:/opt/in:ro" \
  -v "$out:/out" \
  --user "$(id -u):$(id -g)" \
  -e HOME=/home/tester -e CODEX_HOME=/home/tester/.codex -e BACKSIGHT_HOME=/home/tester/backsight \
  -e CODEXIFY="${CODEXIFY:-1}" \
  "$image" /opt/harness/prepare.sh "${cmd[@]}"
