#!/usr/bin/env bash
# The native chain (decision 0078, M7 block 1): the native runtime and the
# runtime switch proven for real on every pull request, on the runner's
# own systemd user manager. One install, set up from nothing on native
# with Hindsight's mock LLM, in an isolated HOME, BACKSIGHT_HOME and
# XDG_* (XDG_RUNTIME_DIR stays the user's: /run/user/<uid>, the manager's
# bus). Its legs, in order, each a named check that fails the run:
#
#   fresh             setup --runtime native --non-interactive; doctor has no error
#   store             memories stored and recalled; backup create; backup verify
#   to-docker         server runtime switch docker: no gate difference, overlap@10 recorded;
#                     then server runtime clean removes server/native whole
#   to-native         and back, from an install that never had native (uv, Python, the
#                     venv and pg0's Postgres fetched again before the verify): the
#                     fingerprint equals the one after store
#   refusals          the db unit, started by systemctl, refuses a missing, an empty
#                     and a foreign instance (its journal says which); a postgres
#                     started from the shell on the data is stopped by server start;
#                     a drop-in setting fsync=off makes server start exit 20
#   upgrade-rollback  server upgrade to a second lock, its smoke failing: exit 30
#                     (RolledBack), the unit back on the first venv, healthy, counts equal
#   switch-rollback   a switch to docker failing at its gate: exit 20, native healthy,
#                     Docker's containers stopped, server.runtime still native
#   lock              scripts/native-lock.sh --check; uv's tarball sha256 is UVSHA256
#
# The units are the user's (the manager reads them from its own unit
# directory, which the isolated HOME's ~/.config/systemd/user links to):
# <PROJECT>-db.service, <PROJECT>-server.service. The trap disables and
# stops the units the install makes, each named exactly (nc_own_unit:
# another bs-… project sharing the prefix is never touched), removes their
# files, runs compose down -v for the project, stops any postgres left on
# the run's data and removes the run's directory; it never changes the
# run's exit status.
#
# Usage: scripts/native-chain.sh [api port] (default 28700 +
# BACKSIGHT_CI_SLOT x 10, scripts/ports.sh; Docker's UI takes port + 1111,
# the native database the first free port after the API's). PROJECT names
# the units and the Compose project (default bs-native-chain); it never
# starts with backsight (never the live install). KEEP=1 leaves the units,
# the stack and the work directory; WORK is the parent of the run's own
# directory (it must allow executables). BACKSIGHT_HF_CACHE as in
# scripts/fresh-install.sh (Docker's model cache, for the to-docker leg);
# BACKSIGHT_NATIVE_HF names a directory holding native's models (<native>/hf):
# copied in before setup when it holds them, filled after setup when it does
# not. NATIVE_DRY=1 lists the legs and stops.
#
# Needs Linux x86_64, a systemd user manager with linger on, Docker with
# Compose, Claude Code (setup wires it), jq, curl, uv's network (PyPI,
# GitHub, Hugging Face) and Go (the builds).

NC_LEGS=(fresh store to-docker to-native refusals upgrade-rollback switch-rollback lock)

# Hindsight's operational history: tables it appends to on every request
# (a recall logs its embedding and reranking calls in llm_requests), so a
# switch's recall probes grow them. Compared by their keys only: none gone.
NC_HISTORY="llm_requests audit_log"

fail() { echo "native-chain: $*" >&2; exit 1; }

# nc_project_ok <project>: refuses the live install's names before
# anything runs: its units are backsight-*.service, its Compose project
# backsight, and the trap's <project>-* must never match them.
nc_project_ok() {
  case $1 in
    backsight*) echo "native-chain: PROJECT=$1: its units would match backsight-*, never the live install" >&2; return 2 ;;
    '' | *[!a-z0-9-]* | -*) echo "native-chain: PROJECT=$1: lowercase letters, digits and dashes only" >&2; return 2 ;;
  esac
}

# shellcheck disable=SC2154 # work, project, unitdir, native are main's
nc_db() { echo "$project-db.service"; }
nc_srv() { echo "$project-server.service"; }

# nc_mask: stdin with the install's secret values masked (a failure's
# output goes to a CI log).
nc_mask() {
  local sec=${BACKSIGHT_HOME:-/nonexistent}/config/secrets.toml
  if [ -r "$sec" ]; then
    awk 'NR == FNR { if (match($0, /^[a-z_]+ *= *"[^"]+"/)) { v = $0; sub(/^[^"]*"/, "", v); sub(/".*$/, "", v); if (length(v) >= 8) keys[v] = 1 }; next }
      { for (k in keys) while ((i = index($0, k)) > 0) $0 = substr($0, 1, i - 1) "***" substr($0, i + length(k)); print }' "$sec" -
  else
    cat
  fi
}

# nc_show <file…>: each file's tail, masked, on stderr.
nc_show() {
  local f
  for f in "$@"; do
    [ -f "$f" ] || continue
    echo "--- $(basename "$f")" >&2
    tail -n 80 "$f" | nc_mask >&2
  done
}

# nc_journal <unit> <since epoch>: the unit's journal lines since then.
nc_journal() {
  journalctl --user -u "$1" --since "@$2" --no-pager -o cat 2> /dev/null || true
}

# nc_units <is-active|is-enabled> <yes|no>: both native units are (yes)
# or neither is (no). systemctl's own answer for several units is "any".
nc_units() {
  local u n=0
  for u in "$(nc_db)" "$(nc_srv)"; do
    systemctl --user "$1" --quiet "$u" 2> /dev/null && n=$((n + 1))
  done
  case $2 in yes) [ $n = 2 ] ;; no) [ $n = 0 ] ;; esac
}

nc_healthy() {
  "$bin" server status --json > "$work/status.json" 2>&1 || true
  jq -e '.ok and .data.api.healthy' "$work/status.json" > /dev/null
}

# nc_fingerprint <out>: the server's data fingerprint, once two reads 3 s
# apart agree (a stored memory's extraction runs in the background).
nc_fingerprint() {
  local a b i
  for i in $(seq 1 60); do
    a=$("$bin" server runtime fingerprint --json | jq -cS '.data | {tables, banks, control_documents}') || fail "server runtime fingerprint failed"
    sleep 3
    b=$("$bin" server runtime fingerprint --json | jq -cS '.data | {tables, banks, control_documents}') || fail "server runtime fingerprint failed"
    if [ "$a" = "$b" ]; then
      printf '%s\n' "$b" > "$1"
      return 0
    fi
    [ "$i" -lt 60 ] || fail "the fingerprint never settled"
  done
}

# nc_fp_diff <a> <b>: what differs between two fingerprints, one line
# each ("table x: rows 1 -> 2", "banks", …); nothing when they are equal.
nc_fp_diff() {
  jq -rn --slurpfile a "$1" --slurpfile b "$2" '$a[0] as $x | $b[0] as $y |
    ((($x.tables | keys) + ($y.tables | keys) | unique[]) as $t
      | select($x.tables[$t] != $y.tables[$t])
      | "table \($t): rows \($x.tables[$t].rows // "none") -> \($y.tables[$t].rows // "none")\(if $x.tables[$t].rows == $y.tables[$t].rows then ", keys differ" else "" end)"),
    (select($x.banks != $y.banks) | "banks \($x.banks) -> \($y.banks)"),
    (select($x.control_documents != $y.control_documents) | "control documents \($x.control_documents) -> \($y.control_documents)")'
}

# nc_runtime: server.runtime in config.toml (absent: docker).
nc_runtime() {
  local v
  v=$(sed -n 's/^runtime *= *"\(.*\)"$/\1/p' "$BACKSIGHT_HOME/config/config.toml" | head -n 1)
  echo "${v:-docker}"
}

# compose …: the project the install rendered, never backsight.
compose() {
  local f=$BACKSIGHT_HOME/data/server/compose.yaml p
  p=$(sed -n 's/^name: *//p' "$f" | head -n 1)
  if [ -z "$p" ] || [ "$p" != "$project" ]; then
    echo "refusing compose project '$p'" >&2
    return 1
  fi
  docker compose -p "$p" -f "$f" --env-file "$BACKSIGHT_HOME/data/server/.env" "$@"
}

nc_docker_running() { docker ps -q --filter "label=com.docker.compose.project=$project"; }

# nhf_seed, nhf_save: native's models from and to BACKSIGHT_NATIVE_HF
# (CI's cache). A failure is a warning: the setup downloads them.
nhf_read=
nhf_seed() {
  local src=${BACKSIGHT_NATIVE_HF:-}
  [ -n "$src" ] || return 0
  if [ ! -f "$src/.backsight-models" ]; then
    echo "native hf cache: $src holds no models yet, setup downloads them" >&2
    return 0
  fi
  nhf_read=1
  if mkdir -p "$native/hf" && cp -a "$src/." "$native/hf/"; then
    echo "native hf cache: $native/hf seeded from $src" >&2
  else
    echo "::warning::native hf cache: $native/hf could not be seeded from $src; setup downloads the models" >&2
    rm -rf "$native/hf"
  fi
}
nhf_save() {
  local dst=${BACKSIGHT_NATIVE_HF:-}
  [ -n "$dst" ] && [ -z "$nhf_read" ] && [ -f "$native/hf/.backsight-models" ] || return 0
  if mkdir -p "$dst" && cp -a "$native/hf/." "$dst/"; then
    echo "native hf cache: $native/hf saved to $dst" >&2
  else
    echo "::warning::native hf cache: $native/hf could not be saved to $dst" >&2
  fi
}

# ---- the legs ---------------------------------------------------------

nc_leg_fresh() {
  printf 'mock-not-a-key\n' | "$bin" secret set llm_api_key > /dev/null
  nhf_seed
  "$bin" setup --runtime native --non-interactive --json --answer provider=mock --answer model=mock-model --answer consent=yes \
    --answer retention=3650 --answer project="$project" --answer api_port="$port" --answer ui_port=$((port + 1111)) --answer scheduler=off \
    < /dev/null > "$work/setup.json" 2> "$work/setup.err" || { nc_show "$work/setup.json" "$work/setup.err"; fail "setup --runtime native failed"; }
  nhf_save
  [ "$(nc_runtime)" = native ] || fail "config.toml says runtime $(nc_runtime) after setup --runtime native"
  if [ ! -f "$unitdir/$(nc_db)" ] || [ ! -f "$unitdir/$(nc_srv)" ]; then fail "no $(nc_db) and $(nc_srv) in $unitdir"; fi
  nc_units is-active yes || fail "the units are not both active after setup"
  nc_healthy || { nc_show "$work/status.json"; fail "the server is not healthy after setup"; }
  "$bin" doctor --json > "$work/doctor.json" 2> /dev/null || true
  jq -e '.data.checks | length > 0' "$work/doctor.json" > /dev/null || { nc_show "$work/doctor.json"; fail "doctor listed no check"; }
  jq -e '.data.checks | map(select(.status == "error")) | length == 0' "$work/doctor.json" > /dev/null ||
    { jq -c '.data.checks[] | select(.status == "error")' "$work/doctor.json" | nc_mask >&2; fail "doctor reports errors on a fresh native install"; }
  echo "units $(nc_db) $(nc_srv) active, doctor $(jq -r '[.data.checks[].status] | group_by(.) | map("\(.[0]) \(length)") | join(", ")' "$work/doctor.json")"
}

nc_leg_store() {
  local i found='' archive
  for i in 1 2 3; do
    "$bin" store --json --kind decision "Native chain note $i: the billing service keeps PostgreSQL 1$i with partial indexes." > "$work/store-$i.json" ||
      { nc_show "$work/store-$i.json"; fail "store $i failed"; }
    jq -e '.ok and .data.status == "stored"' "$work/store-$i.json" > /dev/null || { nc_show "$work/store-$i.json"; fail "note $i was not stored"; }
  done
  for i in $(seq 1 60); do
    "$bin" search --all --min-score 0 --json "Which PostgreSQL version does billing keep?" > "$work/search.json" 2>&1 || true
    if jq -e '.ok and (.data.hits | length > 0)' "$work/search.json" > /dev/null 2>&1; then
      found=1
      break
    fi
    sleep 3
  done
  [ -n "$found" ] || { nc_show "$work/search.json"; fail "search never found the stored notes"; }
  "$bin" backup create --json > "$work/backup.json" || { nc_show "$work/backup.json"; fail "backup create failed"; }
  archive=$(jq -r 'select(.ok and (.data.skipped // "") == "") | .data.path // empty' "$work/backup.json")
  if [ -z "$archive" ] || [ ! -s "$archive" ]; then
    nc_show "$work/backup.json"
    fail "backup create wrote no archive"
  fi
  "$bin" backup verify --json "$archive" > "$work/backup-verify.json" || { nc_show "$work/backup-verify.json"; fail "backup verify failed"; }
  jq -e '.ok and .data.ok and ([.data.restored[]] | add > 0)' "$work/backup-verify.json" > /dev/null ||
    { nc_show "$work/backup-verify.json"; fail "the backup restored no rows"; }
  nc_fingerprint "$work/fp-store.json"
  "$bin" server runtime fingerprint --keys "$work/keys-store.json" --json > "$work/keys-store.out" || { nc_show "$work/keys-store.out"; fail "server runtime fingerprint --keys failed"; }
  echo "3 notes stored and found, $(basename "$archive") verified ($(jq -r '[.data.restored[]] | add' "$work/backup-verify.json") rows)"
}

# nc_switch <to> <name>: the switch, consented; its JSON in $work/<name>.json.
nc_switch() {
  "$bin" server runtime switch "$1" --answer consent=yes --non-interactive --json < /dev/null > "$work/$2.json" 2> "$work/$2.err" ||
    { nc_show "$work/$2.json" "$work/$2.err"; fail "server runtime switch $1 failed"; }
  jq -e '.ok and (.data | has("gate_differences") | not)' "$work/$2.json" > /dev/null || { nc_show "$work/$2.json"; fail "the switch to $1 reports gate differences"; }
  jq -e '.data.probes.overlap_at_10 | type == "number"' "$work/$2.json" > /dev/null || { nc_show "$work/$2.json"; fail "the switch to $1 recorded no overlap@10"; }
  [ "$(nc_runtime)" = "$1" ] || fail "config.toml says runtime $(nc_runtime) after the switch to $1"
  nc_healthy || { nc_show "$work/status.json"; fail "the server is not healthy on $1 after the switch"; }
}

nc_leg_to_docker() {
  # shellcheck disable=SC2153 # project is main's
  hfc_seed "$project"
  nc_switch docker to-docker
  hfc_save "$project"
  [ -n "$(nc_docker_running)" ] || fail "no container of $project runs after the switch to docker"
  nc_units is-active no || fail "a native unit still runs on docker"
  nc_units is-enabled no || fail "a native unit is still enabled on docker: two runtimes would start at boot"
  # to-native then starts from an install that never had native (the
  # v0.1.16-m7 round trip: pg0's Postgres missing at the switch's verify).
  "$bin" server runtime clean --answer consent=yes --non-interactive --json < /dev/null > "$work/clean-native.json" 2> "$work/clean-native.err" ||
    { nc_show "$work/clean-native.json" "$work/clean-native.err"; fail "server runtime clean failed"; }
  jq -e '.ok and .data.deleted and .data.runtime == "native"' "$work/clean-native.json" > /dev/null || { nc_show "$work/clean-native.json"; fail "server runtime clean deleted no native runtime"; }
  [ ! -e "$native" ] || fail "$native is still there after server runtime clean"
  echo "overlap@10 $(jq -r '.data.probes.overlap_at_10' "$work/to-docker.json") ($(jq -r '.data.probes.queries' "$work/to-docker.json") probes, $(jq -r '.data.probes.own_at_rank_1' "$work/to-docker.json") own first); native cleaned"
}

nc_leg_to_native() {
  [ ! -e "$native" ] || fail "$native exists before the switch to native: to-native would not start from an install that never had native"
  nhf_seed # the models only: uv, Python, the venv and pg0's Postgres are fetched again
  nc_switch native to-native
  compgen -G "$native/home/.pg0/installation/*/bin/initdb" > /dev/null || fail "no pg0 Postgres in $native/home/.pg0 after the switch to native"
  [ -z "$(nc_docker_running)" ] || fail "Docker's containers still run after the switch to native"
  nc_units is-enabled yes || fail "the native units are not both enabled after the switch to native"
  nc_fingerprint "$work/fp-native.json"
  # Equal to store's but for Hindsight's operational history (NC_HISTORY),
  # which every recall of the switches' probes appends to; no key of
  # store's is gone from any table, that history included.
  jq "del(.tables.${NC_HISTORY// /, .tables.})" "$work/fp-store.json" > "$work/fp-store-data.json"
  jq "del(.tables.${NC_HISTORY// /, .tables.})" "$work/fp-native.json" > "$work/fp-native-data.json"
  nc_fp_diff "$work/fp-store-data.json" "$work/fp-native-data.json" > "$work/fp-diff.txt"
  [ ! -s "$work/fp-diff.txt" ] || { cat "$work/fp-diff.txt" >&2; fail "the fingerprint after the round trip differs from the one after store"; }
  "$bin" server runtime fingerprint --compare "$work/keys-store.json" --json > "$work/keys-compare.json" 2>&1 ||
    { jq -r '.error.message // .' "$work/keys-compare.json" | head -c 2000 >&2; fail "rows of store's are gone after the round trip"; }
  echo "overlap@10 $(jq -r '.data.probes.overlap_at_10' "$work/to-native.json"); fingerprint equal to store's ($(jq -r '.tables | length' "$work/fp-native-data.json") tables; $(nc_fp_diff "$work/fp-store.json" "$work/fp-native.json" | tr '\n' ' ')no key gone)"
}

# nc_unit_refuses <reason> <pattern>: the db unit, started by systemctl,
# fails, and its journal names the refusal. Stopped and reset after.
nc_unit_refuses() {
  local since i
  since=$(date +%s)
  sleep 1
  if systemctl --user start "$(nc_db)" 2> /dev/null; then
    systemctl --user stop "$(nc_db)" || true
    fail "the db unit started on a $1 instance"
  fi
  for i in $(seq 1 20); do
    nc_journal "$(nc_db)" "$since" > "$work/journal-$1.txt"
    grep -q -- "$2" "$work/journal-$1.txt" && break
    sleep 1
  done
  systemctl --user stop "$(nc_db)" 2> /dev/null || true
  systemctl --user reset-failed "$(nc_db)" 2> /dev/null || true
  grep -q -- "$2" "$work/journal-$1.txt" || { nc_show "$work/journal-$1.txt"; fail "the db unit's journal does not say the instance is $1 ($2)"; }
  ! systemctl --user is-active --quiet "$(nc_db)" || fail "the db unit runs after refusing a $1 instance"
}

nc_leg_refusals() {
  local inst=$native/home/.pg0/instances/hindsight pg data pgport stray main cg since code
  data=$inst/data
  pg=$(echo "$native"/home/.pg0/installation/*/)
  pg=${pg%/}
  [ -x "$pg/bin/postgres" ] || fail "no bundled postgres in $pg"
  nc_fingerprint "$work/fp-before-refusals.json"
  "$bin" server stop --json > "$work/stop.json" || { nc_show "$work/stop.json"; fail "server stop failed"; }

  # missing: the instance moved away.
  mv "$inst" "$inst.aside"
  nc_unit_refuses missing "is missing: refusing to start"
  mv "$inst.aside" "$inst"

  # empty: an empty data directory in its place.
  mv "$inst" "$inst.aside"
  mkdir -p "$data"
  chmod 700 "$data"
  nc_unit_refuses empty "is empty: refusing to start"
  rm -rf "$inst"
  mv "$inst.aside" "$inst"

  # identity: another cluster (a throwaway one, made by the bundled
  # initdb as pg0 makes its own) swapped in.
  env -i PATH=/usr/bin:/bin LANG=C.UTF-8 HOME="$native/home" LD_LIBRARY_PATH="$pg/lib" \
    "$pg/bin/initdb" -D "$work/other-cluster" -U hindsight --auth=trust -E UTF8 > "$work/initdb.log" 2>&1 ||
    { nc_show "$work/initdb.log"; fail "initdb of a throwaway cluster failed"; }
  mv "$data" "$data.aside"
  mv "$work/other-cluster" "$data"
  nc_unit_refuses identity "is not the one backsight recorded"
  rm -rf "$data"
  mv "$data.aside" "$data"

  # A stray: pg0's postgres started from the shell on the data. server
  # start stops it (the db unit's native-check) and runs its own.
  pgport=$(sed -n 's/^BACKSIGHT_PG_PORT=//p' "$native/server.env")
  [ -n "$pgport" ] || fail "server.env names no BACKSIGHT_PG_PORT"
  (exec env -i PATH=/usr/bin:/bin LANG=C.UTF-8 HOME="$native/home" LD_LIBRARY_PATH="$pg/lib" \
    "$pg/bin/postgres" -D "$data" -p "$pgport" -c listen_addresses=127.0.0.1 -c unix_socket_directories= > "$work/stray.log" 2>&1 < /dev/null) &
  stray=$!
  for _ in $(seq 1 60); do
    env -i LD_LIBRARY_PATH="$pg/lib" "$pg/bin/pg_isready" -q -h 127.0.0.1 -p "$pgport" && break
    sleep 0.5
  done
  env -i LD_LIBRARY_PATH="$pg/lib" "$pg/bin/pg_isready" -q -h 127.0.0.1 -p "$pgport" || { nc_show "$work/stray.log"; fail "the stray postgres never answered"; }
  [ "$(head -n 1 "$data/postmaster.pid")" = "$stray" ] || fail "postmaster.pid does not name the stray $stray"
  since=$(date +%s)
  sleep 1
  "$bin" server start --json > "$work/start-stray.json" 2>&1 || { nc_show "$work/start-stray.json"; kill -INT "$stray" 2> /dev/null; fail "server start beside a stray failed"; }
  wait "$stray" 2> /dev/null || true
  [ ! -e "/proc/$stray" ] || { kill -INT "$stray" 2> /dev/null; fail "the stray postgres $stray still runs after server start"; }
  nc_journal "$(nc_db)" "$since" > "$work/journal-stray.txt"
  grep -q "a postgres (pid $stray) runs on .* outside $(nc_db): stopping it" "$work/journal-stray.txt" ||
    { nc_show "$work/journal-stray.txt"; fail "the db unit's journal does not report the stray $stray"; }
  main=$(systemctl --user show -p MainPID --value "$(nc_db)")
  if [ -z "$main" ] || [ "$main" = 0 ] || [ "$(head -n 1 "$data/postmaster.pid")" != "$main" ]; then fail "the postmaster is not the db unit's ($main)"; fi
  cg=$(sed -n 's/^0:://p' "/proc/$main/cgroup")
  case $cg in */"$(nc_db)") ;; *) fail "the postmaster $main runs in $cg, not $(nc_db)'s cgroup" ;; esac
  nc_healthy || { nc_show "$work/status.json"; fail "the server is not healthy after stopping the stray"; }

  # fsync=off through a drop-in: server start exits 20 naming fsync.
  "$bin" server stop --json > "$work/stop.json" || { nc_show "$work/stop.json"; fail "server stop failed"; }
  mkdir -p "$unitdir/$(nc_db).d"
  printf '[Service]\nExecStart=\nExecStart=%s -c fsync=off\n' "$(sed -n 's/^ExecStart=//p' "$unitdir/$(nc_db)")" > "$unitdir/$(nc_db).d/native-chain-fsync.conf"
  systemctl --user daemon-reload
  set +e
  "$bin" server start --json > "$work/start-fsync.json" 2>&1
  code=$?
  set -e
  rm -rf "${unitdir:?}/$(nc_db).d"
  systemctl --user daemon-reload
  [ "$code" = 20 ] || { nc_show "$work/start-fsync.json"; fail "server start with fsync=off: exit $code, want 20"; }
  jq -e '.error.message | test("fsync")' "$work/start-fsync.json" > /dev/null || { nc_show "$work/start-fsync.json"; fail "the refusal does not name fsync"; }
  nc_units is-active no || fail "a unit still runs after the fsync refusal"
  nc_units is-enabled no || fail "a unit is still enabled after the fsync refusal"

  # Everything back: the server starts on its own data.
  "$bin" server start --json > "$work/start.json" 2>&1 || { nc_show "$work/start.json"; fail "server start after the refusals failed"; }
  nc_healthy || { nc_show "$work/status.json"; fail "the server is not healthy after the refusals"; }
  nc_fingerprint "$work/fp-after-refusals.json"
  nc_fp_diff "$work/fp-before-refusals.json" "$work/fp-after-refusals.json" > "$work/fp-diff.txt"
  [ ! -s "$work/fp-diff.txt" ] || { cat "$work/fp-diff.txt" >&2; fail "the data changed across the refusals"; }
  echo "missing, empty, identity refused by the unit; stray $stray stopped, postmaster $main in $(nc_db); fsync=off refused (exit 20)"
}

# nc_venv: the venv the server unit runs.
nc_venv() { sed -n 's|^ExecStart=.*/venv/\([^/]*\)/bin/hindsight-api.*$|\1|p' "$unitdir/$(nc_srv)"; }

nc_leg_upgrade_rollback() {
  local venv code
  venv=$(nc_venv)
  [ -n "$venv" ] || fail "the server unit names no venv"
  nc_fingerprint "$work/fp-before-upgrade.json"
  "$bin" server runtime fingerprint --keys "$work/keys-before-upgrade.json" --json > "$work/keys-before-upgrade.out" ||
    { nc_show "$work/keys-before-upgrade.out"; fail "server runtime fingerprint --keys failed"; }
  jq -e '.control_documents | has("backsight:epoch") | not' "$work/fp-before-upgrade.json" > /dev/null ||
    fail "the bank already has a restore epoch: the counts below assume the rollback writes the first one"
  set +e
  BACKSIGHT_TEST_FAIL_SMOKE=1 "$bin2" server upgrade --allow-unordered --json < /dev/null > "$work/upgrade.json" 2> "$work/upgrade.err"
  code=$?
  set -e
  # A rolled-back upgrade exits 30, RolledBack (ruling R5).
  [ "$code" = 30 ] || { nc_show "$work/upgrade.json" "$work/upgrade.err"; fail "a failing smoke: exit $code, want 30"; }
  jq -e '.data.rolled_back and (.error.message | test("BACKSIGHT_TEST_FAIL_SMOKE"))' "$work/upgrade.json" > /dev/null ||
    { nc_show "$work/upgrade.json"; fail "the failed upgrade does not say it rolled back"; }
  if [ "$(jq -r .data.current "$work/upgrade.json")" != "$venv" ] || [ "$(jq -r .data.target "$work/upgrade.json")" = "$venv" ]; then
    nc_show "$work/upgrade.json"
    fail "the upgrade did not go from $venv to the second lock's venv"
  fi
  [ -f "$native/venv/$(jq -r .data.target "$work/upgrade.json")/bin/hindsight-api" ] || fail "the second venv was never installed"
  [ "$(nc_venv)" = "$venv" ] || fail "the server unit runs $(nc_venv) after the rollback, want $venv"
  nc_healthy || { nc_show "$work/status.json"; fail "the server is not healthy after the rollback"; }
  nc_fingerprint "$work/fp-after-upgrade.json"
  # The counts as before: the rollback restored the safety dump (it checks
  # the restored counts itself), then wrote a new restore epoch (ruling
  # R5), its one control document. So: no row of before is gone, one
  # document more, and no control document but the epoch's changed.
  "$bin" server runtime fingerprint --compare "$work/keys-before-upgrade.json" --json > "$work/keys-upgrade.json" 2>&1 ||
    { jq -r '.error.message // .' "$work/keys-upgrade.json" | head -c 2000 >&2; fail "rows are gone after the rollback"; }
  jq -e --argjson n "$(jq '.tables.documents.rows' "$work/fp-before-upgrade.json")" '.tables.documents.rows == $n + 1' "$work/fp-after-upgrade.json" > /dev/null ||
    { nc_fp_diff "$work/fp-before-upgrade.json" "$work/fp-after-upgrade.json" >&2; fail "the rollback left $(jq '.tables.documents.rows' "$work/fp-after-upgrade.json") documents, want $(jq '.tables.documents.rows' "$work/fp-before-upgrade.json") and the epoch's"; }
  jq -en --slurpfile a "$work/fp-before-upgrade.json" --slurpfile b "$work/fp-after-upgrade.json" \
    '[($a[0].control_documents + $b[0].control_documents) | keys[] | select($a[0].control_documents[.] != $b[0].control_documents[.])] == ["backsight:epoch"]' > /dev/null ||
    { nc_fp_diff "$work/fp-before-upgrade.json" "$work/fp-after-upgrade.json" >&2; fail "the rollback changed other control documents than its epoch"; }
  jq -e '.data.epoch.epoch >= 1' "$work/upgrade.json" > /dev/null || { nc_show "$work/upgrade.json"; fail "the rollback reports no restore epoch"; }
  echo "exit 30, rolled back to $venv from $(jq -r .data.target "$work/upgrade.json"), healthy; counts as before (no row gone; $(nc_fp_diff "$work/fp-before-upgrade.json" "$work/fp-after-upgrade.json" | tr '\n' ';' | sed 's/;$//'): epoch $(jq -r .data.epoch.epoch "$work/upgrade.json")'s document)"
}

nc_leg_switch_rollback() {
  local code
  set +e
  BACKSIGHT_TEST_FAIL_SWITCH=gate "$bin" server runtime switch docker --answer consent=yes --non-interactive --json < /dev/null > "$work/switch-fail.json" 2> "$work/switch-fail.err"
  code=$?
  set -e
  [ "$code" = 20 ] || { nc_show "$work/switch-fail.json" "$work/switch-fail.err"; fail "a switch failing at its gate: exit $code, want 20"; }
  jq -e '.error.message | test("BACKSIGHT_TEST_FAIL_SWITCH=gate")' "$work/switch-fail.json" > /dev/null || { nc_show "$work/switch-fail.json"; fail "the failed switch does not name its gate"; }
  [ "$(nc_runtime)" = native ] || fail "server.runtime is $(nc_runtime) after a failed switch, want native"
  nc_healthy || { nc_show "$work/status.json"; fail "native is not healthy after the failed switch"; }
  nc_units is-enabled yes || fail "the native units are not both enabled after the failed switch"
  [ -z "$(nc_docker_running)" ] || fail "Docker's containers run after the failed switch: $(nc_docker_running | tr '\n' ' ')"
  echo "exit 20 at the gate, native healthy and enabled, no container running, server.runtime native"
}

nc_leg_lock() {
  local uv ver asset want got
  uv=$(echo "$native"/uv/*/uv)
  [ -x "$uv" ] || fail "no uv in $native/uv"
  PATH="$(dirname "$uv"):$PATH" "$root/scripts/native-lock.sh" --check > "$work/lock.txt" 2>&1 || { nc_show "$work/lock.txt"; fail "native-lock.sh --check failed"; }
  ver=$(sed -n 's/^[[:space:]]*UVVersion *= *"\(.*\)"$/\1/p' "$root/internal/server/nativelock/uv.go")
  asset=$(sed -n 's/^[[:space:]]*UVAsset *= *"\(.*\)"$/\1/p' "$root/internal/server/nativelock/uv.go")
  want=$(sed -n 's/^[[:space:]]*UVSHA256 *= *"\(.*\)"$/\1/p' "$root/internal/server/nativelock/uv.go")
  if [ -z "$ver" ] || [ -z "$asset" ] || [ ${#want} != 64 ]; then fail "cannot read uv's pin from internal/server/nativelock/uv.go"; fi
  curl -fsSL --retry 3 -o "$work/$asset" "https://github.com/astral-sh/uv/releases/download/$ver/$asset" || fail "cannot download uv $ver's $asset"
  got=$(sha256sum "$work/$asset" | cut -d' ' -f1)
  [ "$got" = "$want" ] || fail "uv $ver's $asset has sha256 $got, the binary pins $want"
  echo "$(tail -n 1 "$work/lock.txt"); uv $ver $asset sha256 $got"
}

# ---- the run ----------------------------------------------------------

# nc_own_unit <unit>: one of the units this run's install makes, named
# exactly (another bs-… project sharing the prefix is never matched): the
# two native units, the database's first-start, unpack and verify transients,
# and the job units, should the scheduler be on.
nc_own_unit() {
  case $1 in
    "$project-db.service" | "$project-server.service" | "$project-db-init.service" | "$project-pg-unpack.service") return 0 ;;
    "$project-tick.service" | "$project-tick.timer" | "$project-maintain.service" | "$project-maintain.timer") return 0 ;;
  esac
  [[ $1 =~ ^${project}-verify-[0-9a-f]{12}\.service$ ]] # VerifyDump's: 6 random bytes in hex
}

# nc_cleanup: the trap. Nothing in it fails the cleanup or changes the
# run's exit status.
nc_cleanup() {
  local status=$? u pid units=()
  set +e +o pipefail
  if [ $status -ne 0 ] && [ -n "${project:-}" ] && [ -n "${work:-}" ]; then
    for u in "$project-db.service" "$project-server.service"; do
      echo "--- journal of $u" >&2
      journalctl --user -u "$u" -n 40 --no-pager -o cat 2> /dev/null | nc_mask >&2 || true
    done
    if [ -f "${BACKSIGHT_HOME:-/nonexistent}/data/server/compose.yaml" ]; then
      compose logs --tail 40 hindsight 2>&1 | nc_mask >&2 || true
    fi
  fi
  if [ "${KEEP:-}" != 1 ] && [ -n "${project:-}" ] && nc_project_ok "$project" 2> /dev/null; then
    for u in $( (systemctl --user list-units --all --plain --no-legend "$project-*" 2> /dev/null; systemctl --user list-unit-files --plain --no-legend "$project-*" 2> /dev/null) | awk '{print $1}' | sort -u); do
      nc_own_unit "$u" && units+=("$u")
    done
    for u in "${units[@]}"; do
      systemctl --user disable --now "$u" > /dev/null 2>&1 || systemctl --user stop "$u" > /dev/null 2>&1 || true
    done
    if [ -n "${work:-}" ]; then
      for pid in $(pgrep -u "$(id -u)" -x postgres 2> /dev/null); do
        if tr '\0' ' ' < "/proc/$pid/cmdline" 2> /dev/null | grep -qF -- "$work/"; then
          kill -INT "$pid" 2> /dev/null || true
        fi
      done
    fi
    if [ -n "${unitdir:-}" ] && [ -d "$unitdir" ]; then
      for u in db.service server.service tick.service tick.timer maintain.service maintain.timer; do
        rm -rf "${unitdir:?}/$project-$u" "$unitdir/$project-$u.d" "$unitdir/default.target.wants/$project-$u" "$unitdir/timers.target.wants/$project-$u" || true
      done
      systemctl --user daemon-reload 2> /dev/null || true
      [ ${#units[@]} = 0 ] || systemctl --user reset-failed "${units[@]}" 2> /dev/null || true
    fi
    if [ -n "${BACKSIGHT_HOME:-}" ] && [ -f "$BACKSIGHT_HOME/data/server/compose.yaml" ]; then
      compose down -v > /dev/null 2>&1 || true
    fi
    docker volume rm -f "${project}_hf_cache" > /dev/null 2>&1 || true # seeded, if no switch ran
    if [ -n "${work:-}" ]; then
      rm -f "$work/home/.config/systemd/user" # the link to the manager's unit directory, never followed
      chmod -R u+w "$work" 2> /dev/null || true
      rm -rf "$work"
    fi
  fi
  exit $status
}

nc_main() {
  local leg t0 t1 rc failed='' detail
  root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
  project=${PROJECT:-bs-native-chain}
  nc_project_ok "$project" || exit 2
  # shellcheck source=scripts/ports.sh
  . "$root/scripts/ports.sh"
  port=$(slot_port 28700 "${1:-}") || exit 2
  if [ "${NATIVE_DRY:-}" = 1 ]; then
    printf '%s\n' "${NC_LEGS[@]}"
    exit 0
  fi
  [ "$(uname -s)/$(uname -m)" = Linux/x86_64 ] || fail "the native runtime runs on Linux x86_64 only"
  XDG_RUNTIME_DIR=/run/user/$(id -u)
  export XDG_RUNTIME_DIR
  systemctl --user show-environment > /dev/null || fail "no systemd user manager answers (XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR)"
  # The manager's unit directory: its own XDG_CONFIG_HOME, else ~/.config
  # (jobs_install.go's rule), read before HOME moves.
  unitdir=$(systemctl --user show-environment | sed -n 's/^XDG_CONFIG_HOME=//p' | head -n 1)
  unitdir=${unitdir:-$HOME/.config}/systemd/user
  if [ -n "${WORK:-}" ]; then
    mkdir -p "$WORK"
    work=$(mktemp -d "$WORK/run.XXXXXX")
  else
    work=$(mktemp -d)
  fi
  trap nc_cleanup EXIT
  bin=$work/bin/backsight bin2=$work/bin/backsight-lock2
  # Built before HOME moves: Go's caches stay the caller's. The second
  # build loads a copy of the lock with one comment line changed: another
  # lock hash, so another venv, of the same Hindsight.
  make -s -C "$root" build OUT="$bin" VERSION=0.950.0 TEST_HOOKS=1
  sed '1s/.*/# Generated by scripts\/native-lock.sh: do not edit (native-chain second lock)./' \
    "$root/internal/server/nativelock/requirements-linux-amd64.txt" > "$work/lock2.txt"
  cmp -s "$work/lock2.txt" "$root/internal/server/nativelock/requirements-linux-amd64.txt" && fail "the second lock equals the first"
  make -s -C "$root" build OUT="$bin2" VERSION=0.950.0 TEST_HOOKS=1 TEST_LOCK="$work/lock2.txt"
  # shellcheck source=scripts/hf-cache.sh
  . "$root/scripts/hf-cache.sh"
  mkdir -p "$unitdir"
  export HOME=$work/home BACKSIGHT_HOME=$work/backsight DISABLE_AUTOUPDATER=1
  export XDG_CONFIG_HOME=$HOME/.config XDG_DATA_HOME=$HOME/.local/share XDG_STATE_HOME=$HOME/.local/state XDG_CACHE_HOME=$HOME/.cache
  unset CLAUDE_CONFIG_DIR BACKSIGHT_STATE_DIR SSH_AUTH_SOCK
  mkdir -p "$XDG_CONFIG_HOME/systemd" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$XDG_CACHE_HOME"
  ln -s "$unitdir" "$XDG_CONFIG_HOME/systemd/user"
  native=$BACKSIGHT_HOME/data/server/native
  : > "$work/results.tsv"

  for leg in "${NC_LEGS[@]}"; do
    if [ -n "$failed" ]; then
      printf '%s\tSKIP\t-\tafter %s failed\n' "$leg" "$failed" >> "$work/results.tsv"
      continue
    fi
    echo "native-chain: leg $leg" >&2
    t0=$(date +%s)
    set +e
    (set -e; "nc_leg_${leg//-/_}") > "$work/leg-$leg.out"
    rc=$?
    set -e
    t1=$(date +%s)
    detail=$(tail -n 1 "$work/leg-$leg.out" | nc_mask)
    if [ $rc -eq 0 ]; then
      printf '%s\tPASS\t%ss\t%s\n' "$leg" $((t1 - t0)) "$detail" >> "$work/results.tsv"
    else
      printf '%s\tFAIL\t%ss\t%s\n' "$leg" $((t1 - t0)) "see above" >> "$work/results.tsv"
      failed=$leg
    fi
  done
  echo "native-chain results (project $project):"
  column -t -s "$(printf '\t')" < "$work/results.tsv" || cat "$work/results.tsv"
  [ -z "$failed" ] || fail "leg $failed failed"
  [ "$(grep -c $'\tPASS\t' "$work/results.tsv")" = "${#NC_LEGS[@]}" ] || fail "not every leg passed"
  echo "native-chain: ok (${NC_LEGS[*]}) on project $project"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  nc_main "$@"
fi
