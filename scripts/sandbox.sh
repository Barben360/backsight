#!/usr/bin/env bash
# Runs the client against a sandbox Hindsight (SPEC §16, from M1): the
# pinned stack rendered by backsight itself, with Hindsight's mock LLM, then
# the sandbox tests. Usage: scripts/sandbox.sh [api port] (default 28300 +
# BACKSIGHT_CI_SLOT x 10, scripts/ports.sh; the UI takes port + 1111).
# SANDBOX_PROJECT names the compose project (default bs-sandbox; never
# backsight, the machine's own install, which cleanup would wipe); KEEP=1
# leaves the stack running.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=scripts/ports.sh
. "$root/scripts/ports.sh"
port=$(slot_port 28300 "${1:-}")
project=${SANDBOX_PROJECT:-bs-sandbox}
[ "$project" != backsight ] || { echo "SANDBOX_PROJECT=backsight would be the machine's own install" >&2; exit 2; }
work=$(mktemp -d)
export BACKSIGHT_HOME=$work/backsight
bin=$work/backsight-bin
cleanup() {
  if [ "${KEEP:-}" != 1 ] && [ -f "$BACKSIGHT_HOME/data/server/compose.yaml" ]; then
    docker compose -p "$project" -f "$BACKSIGHT_HOME/data/server/compose.yaml" --env-file "$BACKSIGHT_HOME/data/server/.env" down -v >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

(cd "$root" && go build -o "$bin" ./cmd/backsight)
mkdir -p "$BACKSIGHT_HOME/config"
cat > "$BACKSIGHT_HOME/config/config.toml" <<TOML
schema_version = 1
[server]
url = "http://127.0.0.1:$port"
bank = "sandbox"
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
docker compose -p "$project" -f "$BACKSIGHT_HOME/data/server/compose.yaml" --env-file "$BACKSIGHT_HOME/data/server/.env" up -d --quiet-pull
for _ in $(seq 1 120); do
  curl -fsS "http://127.0.0.1:$port/health" >/dev/null 2>&1 && break
  sleep 5
done
curl -fsS "http://127.0.0.1:$port/health" >/dev/null
"$bin" bank apply
(cd "$root" && SANDBOX_HOME="$BACKSIGHT_HOME" go test -tags sandbox -count=1 -v ./internal/sandbox/)
