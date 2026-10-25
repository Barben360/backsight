# shellcheck shell=bash disable=SC2154 # work and root are the caller's
# The update proof of scripts/fresh-install.sh (SPEC §16, decision 0067
# block 2): the install's binary is an "old" test build trusting a
# throwaway minisign key; a "new" test build (with the testhooks tag, whose
# migration fails on demand) is published in a local release directory
# signed with that key. The new build also pins another reference of the
# same Hindsight 1.1.0 (this platform's manifest digest instead of the
# multi-arch index), so the update upgrades the server for real: pull,
# backup and verify, start, health, smoke. `update apply --source` must
# roll back a failing migration and a failing server smoke (the testhooks
# build's, on demand), each leaving the old install working, then install
# the new one. The new pin cannot be ordered against the old (the same
# tag, another digest), so the update needs --allow-unordered (decision
# 0067 block review); the command on PATH, linked as an older install.sh
# did to a versioned binary, ends up on the shim and runs the new version.
#
# Sourced, never run. uc_build runs before HOME moves (Go's caches stay the
# caller's); uc_proof after the install. The caller sets root (the
# repository), work (the run's directory) and, for uc_proof, HOME and
# BACKSIGHT_HOME (the scratch install), and defines fail().

uc_old=0.900.0
uc_new=0.900.1
uc_hook=9000-test-hook # internal/migrate/testhooks.go
uc_old_image=$(sed -n 's/^const pinnedHindsight = "\(.*\)"$/\1/p' "$root/internal/server/render.go")

# uc_build <old binary path>: the throwaway key, the old build at the path
# given, the new build and its signed release directory in $work/releases.
uc_build() {
  local out=$1 keys=$work/keys stage pub key
  command -v minisign >/dev/null || { echo "update proof: minisign is needed" >&2; return 1; }
  mkdir -p "$keys"
  minisign -G -W -p "$keys/test.pub" -s "$keys/test.key" >/dev/null
  pub=$(sed -n 2p "$keys/test.pub")
  key=$keys/test.key
  make -s -C "$root" build OUT="$out" VERSION=$uc_old TEST_KEY="$pub"
  [ -n "$uc_old_image" ] || { echo "update proof: no pinnedHindsight in internal/server/render.go" >&2; return 1; }
  # The same image by this platform's manifest digest: a new pin that runs.
  local digest
  digest=$(DOCKER_CONFIG=$work/docker-config docker manifest inspect "$uc_old_image" |
    jq -r --arg arch "$(go env GOARCH)" '.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest')
  case "$digest" in
    *$'\n'*) echo "update proof: several linux/$(go env GOARCH) manifests in $uc_old_image: $digest" >&2; return 1 ;;
    sha256:*) ;;
    *) echo "update proof: no linux/$(go env GOARCH) manifest in $uc_old_image" >&2; return 1 ;;
  esac
  uc_new_image=${uc_old_image%@*}@$digest
  [ "$uc_new_image" != "$uc_old_image" ] || { echo "update proof: the new pin equals the old one" >&2; return 1; }
  make -s -C "$root" build OUT="$work/new/backsight" VERSION=$uc_new TEST_KEY="$pub" TEST_HOOKS=1 TEST_IMAGE="$uc_new_image"
  uc_release=$work/releases/v$uc_new
  stage=$work/new/stage
  mkdir -p "$uc_release" "$stage"
  local os arch archive bare
  os=$(go env GOOS) arch=$(go env GOARCH)
  archive=backsight_${uc_new}_${os}_${arch}.tar.gz bare=backsight_${uc_new}_${os}_${arch}
  cp "$work/new/backsight" "$stage/backsight"
  cp "$root/LICENSE" "$root/README.md" "$stage/"
  tar -czf "$uc_release/$archive" -C "$stage" backsight LICENSE README.md
  cp "$work/new/backsight" "$uc_release/$bare"
  (cd "$uc_release" && sha256sum "$archive" "$bare" > checksums.txt)
  minisign -S -s "$key" -m "$uc_release/checksums.txt" >/dev/null
  printf '## Changelog\n* 0000000 ✨ The update proof release\n' > "$uc_release/CHANGELOG.md"
}

uc_shim() { echo "$BACKSIGHT_HOME/data/bin/current"; }
uc_target() { readlink -f "$(uc_shim)"; }
uc_version() { "$(uc_shim)" version --json | jq -r .data.version; }
uc_state() { jq -c "$1" "$BACKSIGHT_HOME/state/state.json"; }
uc_image() { sed -n '/^  hindsight:$/,/^  [a-z]/s/^    image: //p' "$BACKSIGHT_HOME/data/server/compose.yaml" | head -n 1; }
uc_plugin() { claude plugin list --json | jq -r 'map(select(.id == "backsight@backsight"))[0].version // "none"'; }
uc_path() { echo "$BACKSIGHT_HOME/bin/backsight"; } # the command on PATH (paths.Dirs.Bin)
uc_bins() { ls "$BACKSIGHT_HOME/data/bin"; }

# uc_working <version> <shim target> <plugin version> <image> <what>: the
# install runs that version through its shim, Claude Code loads that
# plugin, the server runs that image, answers and finds the memory, and
# doctor reports no error.
uc_working() {
  local v=$1 target=$2 plugin=$3 image=$4 what=$5
  [ "$(uc_image)" = "$image" ] || fail "$what: the server runs $(uc_image), want $image"
  [ ! -e "$BACKSIGHT_HOME/state/restore-incomplete.json" ] || fail "$what: an upgrade or restore is marked in progress"
  [ "$(uc_version)" = "$v" ] || fail "$what: the shim runs $(uc_version), want $v"
  [ "$(uc_target)" = "$target" ] || fail "$what: the shim points at $(uc_target), want $target"
  if [ -e "$(uc_path)" ]; then
    [ "$("$(uc_path)" version --json | jq -r .data.version)" = "$v" ] || fail "$what: the PATH command $(uc_path) runs $("$(uc_path)" version --json | jq -r .data.version), want $v"
  fi
  [ "$(uc_plugin)" = "$plugin" ] || fail "$what: Claude Code has plugin $(uc_plugin), want $plugin"
  [ "$(uc_state .plugin_version)" = "\"$plugin\"" ] || fail "$what: state.json plugin_version $(uc_state .plugin_version), want $plugin"
  "$(uc_shim)" server status --json > "$work/uc-status.json" || { cat "$work/uc-status.json" >&2; fail "$what: the server is not healthy"; }
  "$(uc_shim)" search --all --min-score 0 --json "Which PostgreSQL version does billing keep?" > "$work/uc-search.json" 2>&1 || true
  jq -e '.ok and (.data.hits | length > 0)' "$work/uc-search.json" >/dev/null || { cat "$work/uc-search.json" >&2; fail "$what: search finds nothing"; }
  "$(uc_shim)" doctor --json > "$work/uc-doctor.json" || true
  jq -e '.data.checks | map(select(.status == "error")) | length == 0' "$work/uc-doctor.json" >/dev/null ||
    { jq -c '.data.checks[]? | select(.status == "error")' "$work/uc-doctor.json" >&2; fail "$what: doctor reports errors"; }
}

uc_proof() {
  local old_target old_plugin before code bins
  old_target=$(uc_target) old_plugin=$(uc_plugin)
  [ "$(uc_version)" = $uc_old ] || fail "the install runs $(uc_version), not the old build $uc_old"
  [ "$old_plugin" != none ] || fail "Claude Code has no backsight plugin before the update: the plugin checks would prove nothing"
  # The command on PATH as install.sh made it before: a versioned binary.
  mkdir -p "$(dirname "$(uc_path)")"
  ln -sfn "$old_target" "$(uc_path)"
  uc_working $uc_old "$old_target" "$old_plugin" "$uc_old_image" "before the update"
  before=$(uc_state '{shim_target, applied_migrations, plugin_version}')

  bins=$(uc_bins)

  # 1. The plan downloads and verifies, and changes nothing. Pins that
  # cannot be ordered are refused without --allow-unordered.
  set +e
  "$(uc_shim)" update apply --plan --source "$work/releases" --json > "$work/uc-unordered.json" 2>/dev/null
  code=$?
  set -e
  [ $code -eq 20 ] && jq -e '.error.message | test("--allow-unordered")' "$work/uc-unordered.json" >/dev/null ||
    { cat "$work/uc-unordered.json" >&2; fail "unordered pins: exit $code, want 20 naming --allow-unordered"; }
  "$(uc_shim)" update apply --plan --allow-unordered --source "$work/releases" --json > "$work/uc-plan.json" || { cat "$work/uc-plan.json" >&2; fail "update apply --plan failed"; }
  jq -e --arg v $uc_new --arg h $uc_hook --arg img "$uc_new_image" '.ok and .data.plan and .data.target == $v and (.data.migrations | index($h) != null) and (.data.key_id | type == "string" and length > 0) and .data.server.upgrade and .data.server.target == $img' \
    "$work/uc-plan.json" >/dev/null || { cat "$work/uc-plan.json" >&2; fail "the plan does not show the new release and its migration"; }
  [ "$(uc_state '{shim_target, applied_migrations, plugin_version}')" = "$before" ] || fail "the plan changed state.json"
  [ "$(uc_target)" = "$old_target" ] || fail "the plan switched the shim"
  [ "$(uc_image)" = "$uc_old_image" ] || fail "the plan changed the server"
  [ "$(uc_plugin)" = "$old_plugin" ] || fail "the plan changed Claude Code's plugin to $(uc_plugin)"
  [ "$(uc_bins)" = "$bins" ] || fail "the plan installed a binary: $(uc_bins | tr '\n' ' ')"
  [ ! -e "$BACKSIGHT_HOME/state/update-incomplete.json" ] || fail "the plan left an update marker"
  [ "$(readlink "$(uc_path)")" = "$old_target" ] || fail "the plan moved the PATH command"

  # 2. A failing client migration (the testhooks build's, on demand): exit
  # 30, and the old install works as before.
  set +e
  BACKSIGHT_TEST_FAIL_MIGRATION=1 "$(uc_shim)" update apply --allow-unordered --source "$work/releases" --json > "$work/uc-fail.json" 2> "$work/uc-fail.progress"
  code=$?
  set -e
  [ $code -eq 30 ] || { cat "$work/uc-fail.json" "$work/uc-fail.progress" >&2; fail "a failing migration: exit $code, want 30"; }
  jq -e '.data.rolled_back and (.error.message | test("test hook"))' "$work/uc-fail.json" >/dev/null || { cat "$work/uc-fail.json" >&2; fail "the failed update does not say it rolled back"; }
  [ "$(uc_state '{shim_target, applied_migrations, plugin_version}')" = "$before" ] || fail "the rollback left state.json changed: $(uc_state '{shim_target, applied_migrations, plugin_version}')"
  [ ! -e "$BACKSIGHT_HOME/state/update-incomplete.json" ] || fail "the rollback left the update marker"
  # update apply pointed the PATH command at the shim, which runs the old
  # binary again.
  [ "$(readlink "$(uc_path)")" = "$(uc_shim)" ] || fail "the PATH command still points at $(readlink "$(uc_path)"), not the shim"
  uc_working $uc_old "$old_target" "$old_plugin" "$uc_old_image" "after the failed migration"

  # 3. A failing server smoke (the testhooks build's, on demand) after the
  # new server started and may have migrated the database: the new
  # binary's server upgrade restores its safety dump into the old image,
  # then update apply rolls the client back: exit 30, old install working.
  set +e
  BACKSIGHT_TEST_FAIL_SMOKE=1 "$(uc_shim)" update apply --allow-unordered --source "$work/releases" --json > "$work/uc-smoke.json" 2> "$work/uc-smoke.progress"
  code=$?
  set -e
  [ $code -eq 30 ] || { cat "$work/uc-smoke.json" "$work/uc-smoke.progress" >&2; fail "a failing server smoke: exit $code, want 30"; }
  jq -e '.data.rolled_back and (.error.message | test("BACKSIGHT_TEST_FAIL_SMOKE"))' "$work/uc-smoke.json" >/dev/null || { cat "$work/uc-smoke.json" >&2; fail "the failed server upgrade does not say why"; }
  for s in 'server upgrade start done' 'server upgrade smoke failed' 'server upgrade rollback done' 'update apply rollback done'; do
    jq -rR 'fromjson? | select(.event == "progress") | "\(.command) \(.step) \(.status)"' "$work/uc-smoke.progress" | grep -qx "$s" ||
      { cat "$work/uc-smoke.progress" >&2; fail "the failed server upgrade's progress lacks: $s"; }
  done
  [ "$(uc_state '{shim_target, applied_migrations, plugin_version}')" = "$before" ] || fail "the server rollback left state.json changed"
  uc_working $uc_old "$old_target" "$old_plugin" "$uc_old_image" "after the failed server upgrade"

  # 4. The update: the new binary next to its signed checksums, the shim
  # on it, its migration recorded, its plugin in Claude Code, the server on
  # its pin, doctor clean (its self-check verifies it against the test
  # key's signature).
  "$(uc_shim)" update apply --allow-unordered --source "$work/releases" --json > "$work/uc-apply.json" 2> "$work/uc-apply.progress" ||
    { cat "$work/uc-apply.json" "$work/uc-apply.progress" >&2; fail "update apply failed"; }
  local new_target new_plugin
  new_target=$(uc_target)
  case "$new_target" in "$BACKSIGHT_HOME/data/bin/$uc_new-"*/backsight) ;; *) fail "the shim points at $new_target" ;; esac
  for f in checksums.txt checksums.txt.minisig; do [ -s "$(dirname "$new_target")/$f" ] || fail "no $f next to the new binary"; done
  uc_state '.applied_migrations' | jq -e --arg h $uc_hook 'index($h) != null' >/dev/null || fail "the migration was not recorded: $(uc_state .applied_migrations)"
  [ "$(uc_state .client.shim_previous)" = "\"$old_target\"" ] || fail "shim_previous is $(uc_state .client.shim_previous)"
  new_plugin=$(uc_plugin)
  [ "$new_plugin" != "$old_plugin" ] && case "$new_plugin" in $uc_new*) true ;; *) false ;; esac || fail "Claude Code has plugin $new_plugin after the update (was $old_plugin)"
  jq -e --arg img "$uc_new_image" '.data.server.upgraded and .data.server.target == $img' "$work/uc-apply.json" >/dev/null || { cat "$work/uc-apply.json" >&2; fail "the server was not upgraded"; }
  uc_working $uc_new "$new_target" "$new_plugin" "$uc_new_image" "after the update"
  [ "$(readlink "$(uc_path)")" = "$(uc_shim)" ] && [ "$("$(uc_path)" version --json | jq -r .data.version)" = $uc_new ] ||
    fail "the PATH command $(uc_path) → $(readlink "$(uc_path)") does not run $uc_new"
  jq -e '.data.checks[] | select(.id == "binary") | .status == "ok"' "$work/uc-doctor.json" >/dev/null ||
    { jq -c '.data.checks[] | select(.id == "binary")' "$work/uc-doctor.json" >&2; fail "the new binary did not verify itself"; }

  # 5. Up to date now; its migration never runs twice.
  "$(uc_shim)" update apply --allow-unordered --source "$work/releases" --json > "$work/uc-again.json" || fail "a second update apply failed"
  jq -e '.data.up_to_date' "$work/uc-again.json" >/dev/null || fail "the new binary is not up to date"
  [ "$(uc_state '.applied_migrations | map(select(. == "'$uc_hook'")) | length')" = 1 ] || fail "the migration was recorded twice"
}
