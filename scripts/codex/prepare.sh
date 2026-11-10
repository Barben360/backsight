#!/usr/bin/env bash
# In the Codex test container (run.sh starts it): a throwaway backsight home
# and CODEX_HOME, backsight's plugin generated as `backsight client install`
# generates it, the fake model running, then the given command. Every run
# of the backsight shim is tapped (tap.sh): hook payloads, hook outputs and
# the MCP traffic land in /out/tap.
set -euo pipefail
: "${BACKSIGHT_HOME:?}" "${CODEX_HOME:?}"
export PATH=$HOME/bin:$PATH
mkdir -p "$HOME/bin" "$BACKSIGHT_HOME/config" "$CODEX_HOME" "$HOME/work" /out/tap /out/model
fake_port=18080

# backsight: a client of the scratch server (or of none: NO_SERVER).
if [ -f /opt/in/backsight/config/secrets.toml ]; then
  cp /opt/in/backsight/config/secrets.toml "$BACKSIGHT_HOME/config/"
fi
cat > "$BACKSIGHT_HOME/config/config.toml" <<TOML
schema_version = 1
[server]
url = "${BS_SERVER_URL:-http://127.0.0.1:9}"
bank = "codex"
mode = "remote"
project = "bs-codex"
[llm]
provider = "mock"
model = "mock-model"
TOML
cp /opt/bs-build/backsight "$HOME/bin/backsight"
# A stand-in claude CLI: client install runs it to register the plugin; the
# generated tree is what we hand to Codex.
cp /opt/harness/fake-claude.sh "$HOME/bin/claude"
backsight client install --json > /out/client-install.json 2>/out/client-install.err || {
  echo "client install failed:" >&2; cat /out/client-install.json /out/client-install.err >&2; exit 1
}
shim=$BACKSIGHT_HOME/data/bin/current
real=$(readlink -f "$shim")
# The shim becomes the tap, which runs the real binary.
rm -f "$shim"
sed -e "s#@REAL@#$real#" -e "s#@BSHOME@#$BACKSIGHT_HOME#" /opt/harness/tap.sh > "$shim"
chmod +x "$shim"
echo "$shim" > /out/shim-path
mkt=$BACKSIGHT_HOME/data/marketplace
# CODEXIFY=0 keeps the tree exactly as generated for Claude Code.
if [ "${CODEXIFY:-1}" = 1 ]; then
  /opt/harness/codexify.sh "$mkt"
fi

# The fake model, and Codex pointed at it. Codex's own sandbox is off: its
# bubblewrap cannot create a user namespace under Docker, and the container
# is the isolation.
/opt/bs-build/fakeresponses -addr "127.0.0.1:$fake_port" -log /out/model >/out/fakeresponses.log 2>&1 &
cat > "$CODEX_HOME/config.toml" <<TOML
model = "fake-model"
model_provider = "fake"
sandbox_mode = "danger-full-access"

[model_providers.fake]
name = "fake"
base_url = "http://127.0.0.1:$fake_port/v1"
wire_api = "responses"

[projects."$HOME/work"]
trust_level = "trusted"

[mcp_servers.probe]
command = "/opt/bs-build/probemcp"
args = ["-log", "/out/probemcp"]
TOML
cp "$CODEX_HOME/config.toml" /out/config-start.toml
# backsight's plugin, installed the way a user would.
codex plugin marketplace add "$mkt" > /out/codex-marketplace-add.txt
codex plugin add backsight@backsight > /out/codex-plugin-add.txt
codex plugin list --json > /out/codex-plugin-list.json
# The recording hooks on every event (user level, beside the plugin's).
cp /opt/harness/probe-hooks.json "$CODEX_HOME/hooks.json"
mkdir -p /tmp/probe
cd "$HOME/work"
git init -q . 2>/dev/null || true
exec "$@"
