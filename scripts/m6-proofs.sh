# shellcheck shell=bash
# The M6 proofs (decision 0073 block 4): several PCs sharing one memory
# server. Each check is one function, m6_check_<name with _ for ->, run by
# m6_check; it reaches a machine through the driver alone, so the VM bench
# (scripts/vm-bench.sh) and the CI chain run the same proofs.
#
# Sourced, never run. Driver contract (the caller defines):
#   m6_on <a|b|srv> <backsight words…>  that machine's backsight; stdin,
#                                       stdout, stderr and status passed through
#   m6_sh <a|b|srv> <sh command>        an sh command there, stdin passed through
#   m6_put <who> <path>                 stdin to that file there, mode 0600
#   m6_get <who> <path>                 that file's bytes
#   m6_home <who>                       the home there (absolute)
#   M6_RESULTS                          the JSONL file the results go to
#   M6_WORK                             a private scratch directory here
#   M6_VERSION, M6_COMMIT               what the results are of
#   fail <message>                      stops the caller
# A is a PC with Claude Code (the bench's bs-m6-client), B the other one
# (the bench's host client), srv the server's host. The checks of the VM
# bench alone (ssh-*, login-shells, timers) also need:
#   M6_SERVER_IP, M6_ROOT (this checkout)
#   m6_link a <up|down>                 A's network link
#   m6_host_key <new|old>               the server's ed25519 host key, and the
#                                       driver's own trust of it
#
# Secrets: a key or token is never in an argv, a trace or a result. B's
# API key is read in a process substitution (m6_auth, curl -K) and every
# result's detail and values are scrubbed (m6_result).

# shellcheck source=scripts/proof-lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/proof-lib.sh"

M6_STOPPED=

# m6_q <word…>: the words quoted for sh, space separated.
m6_q() {
  local w out='' sq="'\\''"
  for w in "$@"; do out+="${out:+ }'${w//\'/$sq}'"; done
  printf '%s' "$out"
}

# m6_scrub: stdin with every connection token (bst1.…) masked.
m6_scrub() { sed -E 's/bst1\.[A-Za-z0-9_.=-]*/[token]/g'; }

# m6_result <check> <PASS|FAIL|RECORDED|NOT RUN> <detail> [values-json]:
# one line `bs-m6 <check>: <STATUS> <detail>` and one JSONL record. A key
# named for a secret (api_key, token, secret, password, key) is dropped
# from values, and a string holding a token masked.
m6_result() {
  local check=$1 status=$2 detail values=${4:-'{}'} rec
  detail=$(printf '%s' "$3" | m6_scrub)
  rec=$(jq -cn --arg check "$check" --arg status "$status" --arg detail "$detail" --argjson values "$values" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg version "${M6_VERSION:-}" --arg commit "${M6_COMMIT:-}" '
    def scrub: walk(if type == "object" then with_entries(select(.key | test("api_key|token|secret|password|(^|_)key$"; "i") | not))
      elif type == "string" then gsub("bst1\\.[A-Za-z0-9_.=-]*"; "[token]") else . end);
    {check: $check, status: $status, detail: $detail, values: ($values | scrub), at: $at, version: $version, commit: $commit}') ||
    fail "the result of $check has values that are not JSON"
  printf 'bs-m6 %s: %s %s\n' "$check" "$status" "$detail"
  printf '%s\n' "$rec" >> "$M6_RESULTS"
}

# m6_results_of <check>: how many records M6_RESULTS holds for it.
m6_results_of() {
  [ -s "$M6_RESULTS" ] || { echo 0; return; }
  jq -s --arg c "$1" '[.[] | select(.check == $c)] | length' "$M6_RESULTS"
}

# m6_check <name> <function>: runs the function in a subshell under set -e
# (inherited by command substitutions), where fail ends it; a fail (or any
# non-zero exit) is the check's FAIL with the message, and every check
# after it is NOT RUN (later checks build on the state earlier ones
# left: M6_STOPPED names the failed one). A function must record its own
# PASS or RECORDED: one that records nothing fails. Never call m6_check
# in a condition (if, &&, ||): bash turns set -e off there, even in a
# subshell, and a failing step would go on; it refuses.
m6_check() {
  local name=$1 fn=$2 rc=0 errexit=0 before probe
  if [ -n "$M6_STOPPED" ]; then
    m6_result "$name" "NOT RUN" "after the FAIL of $M6_STOPPED"
    return 0
  fi
  case $- in *e*) errexit=1 ;; esac
  set +e
  probe=$(
    set -e
    false
    echo on
  )
  if [ "$errexit" = 1 ]; then set -e; fi
  [ -z "$probe" ] || fail "m6_check $name was called in a condition (if, &&, ||), where set -e is off: run it as a command of its own"
  M6_FAIL_FILE=$M6_WORK/fail-$name
  rm -f "$M6_FAIL_FILE"
  before=$(m6_results_of "$name")
  set +e
  (
    set -e
    shopt -s inherit_errexit
    fail() {
      printf '%s\n' "$*" >> "$M6_FAIL_FILE"
      printf 'bs-m6 %s: %s\n' "$name" "$*" >&2
      exit 1
    }
    "$fn"
  )
  rc=$?
  if [ "$errexit" = 1 ]; then set -e; fi
  if [ "$rc" = 0 ] && [ "$(m6_results_of "$name")" -gt "$before" ]; then
    return 0
  fi
  M6_STOPPED=$name
  if [ -s "$M6_FAIL_FILE" ]; then
    m6_result "$name" FAIL "$(awk 'NR > 1 { printf "; " } { printf "%s", $0 }' "$M6_FAIL_FILE")"
  elif [ "$rc" = 0 ]; then
    m6_result "$name" FAIL "the check recorded no result"
  else
    m6_result "$name" FAIL "exited $rc without a message"
  fi
}

# m6_json <what> <text>: the text when it is one JSON value, else fail.
m6_json() {
  jq -e . > /dev/null 2>&1 <<<"$2" || fail "$1 answered no JSON: $(printf '%s' "$2" | tail -n 3 | m6_scrub)"
  printf '%s\n' "$2"
}

# m6_paths <who>: backsight paths --json's data there.
m6_paths() {
  local out
  out=$(m6_on "$1" paths --json < /dev/null) || fail "backsight paths on $1 failed"
  m6_json "paths on $1" "$out" | jq -c .data
}

# m6_toml <section> <key>: a string value of stdin's TOML ("" for the top level).
m6_toml() {
  awk -v want="$1" -v key="$2" '
    /^[[:space:]]*\[/ { sec = $0; gsub(/^[[:space:]]*\[|\][[:space:]]*$/, "", sec); next }
    sec == want && $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
      v = $0; sub(/^[^=]*=[[:space:]]*/, "", v); sub(/[[:space:]]*(#.*)?$/, "", v)
      gsub(/^["\047]|["\047]$/, "", v); print v; exit }'
}

# m6_server: "<url> <bank>" of B's config, the server the documents are read on.
m6_server() {
  local cfg url bank
  cfg=$(m6_paths b | jq -r .config_file)
  cfg=$(m6_get b "$cfg") || fail "cannot read B's config"
  url=$(m6_toml server url <<<"$cfg")
  bank=$(m6_toml server bank <<<"$cfg")
  [ -n "$url" ] || fail "B's config names no server url"
  printf '%s %s\n' "${url%/}" "${bank:-claude-memory}"
}

# m6_auth: a curl config holding B's key, for `curl -K <(m6_auth)`: the
# key never sits in a traced argv. Only in a process substitution (it
# turns xtrace off for good in the shell it runs in).
m6_auth() {
  { set +x; } 2> /dev/null
  local sec
  sec=$(m6_paths b | jq -r .secrets_file)
  printf 'header = "Authorization: Bearer %s"\n' "$(m6_get b "$sec" | m6_toml '' api_key)"
}

# m6_urlencode <s>: s with every byte but [A-Za-z0-9._~-] %-encoded.
m6_urlencode() { jq -rn --arg s "$1" '$s | @uri'; }

# m6_doc <docid>: the document's JSON on B's server with B's key; empty on
# 404. Any other answer (401, 5xx, no server) fails: it never reads as
# "absent".
m6_doc() {
  local out code server
  server=$(m6_server) || exit 1
  out=$(mktemp "$M6_WORK/doc.XXXXXX")
  code=$(pl_untraced curl -sS -o "$out" -w '%{http_code}' -K <(m6_auth) \
    "${server% *}/v1/default/banks/${server#* }/documents/$(m6_urlencode "$1")") || code=000
  case $code in
    200) cat "$out" ;;
    404) ;;
    *)
      rm -f "$out"
      fail "reading document $1: HTTP $code"
      ;;
  esac
  rm -f "$out"
}

# m6_uuid_counts <sid>: "<uuid> <n>" for every [rec <uuid>] in the
# original_text of conversation:<sid> (every segment appended to it), one
# a line, sorted. Fails when the document is absent or has no
# original_text: an empty count must never read as "none sent twice".
m6_uuid_counts() {
  local doc
  doc=$(m6_doc "conversation:$1") || exit 1
  [ -n "$doc" ] || fail "conversation:$1 is not on the server"
  jq -e '.original_text | type == "string" and length > 0' > /dev/null <<<"$doc" ||
    fail "conversation:$1 has no original_text"
  jq -r .original_text <<<"$doc" | grep -o '\[rec [^]]*\]' | sed 's/^\[rec \(.*\)\]$/\1/' | sort | uniq -c | awk '{ print $2, $1 }'
}

# m6_queue_wait: waits until B's server has no pending or processing
# operation (async retains): what is read before may lag.
m6_queue_wait() {
  local server n total status _
  server=$(m6_server) || exit 1
  for _ in $(seq 1 100); do
    n=0
    for status in pending processing; do
      total=$(pl_untraced curl -fsS -K <(m6_auth) "${server% *}/v1/default/banks/${server#* }/operations?status=$status&limit=1" | jq -er .total) ||
        fail "cannot read the bank's $status operations"
      n=$((n + total))
    done
    [ "$n" -eq 0 ] && return 0
    sleep 3
  done
  fail "the bank's queue never drained ($n operation(s) left)"
}

# m6_tick <who>: `jobs tick --json` there, its envelope on stdout. A tick
# skipped because another runs (the scheduler's) is no tick: it is tried
# again every M6_TICK_EVERY s (5) for M6_TICK_WAIT s (120), then fails.
m6_tick() {
  local who=$1 out rc deadline=$((SECONDS + ${M6_TICK_WAIT:-120}))
  while :; do
    rc=0
    out=$(m6_on "$who" jobs tick --json < /dev/null) || rc=$?
    out=$(m6_json "jobs tick on $who (exit $rc)" "$out") || exit 1
    if [ "$(jq -r '.data.reason // ""' <<<"$out")" != "another tick is running" ]; then
      printf '%s\n' "$out"
      return 0
    fi
    [ "$SECONDS" -lt "$deadline" ] || fail "jobs tick on $who: another tick was still running after ${M6_TICK_WAIT:-120} s"
    sleep "${M6_TICK_EVERY:-5}"
  done
}

# m6_flush <who> <sid>: `retain flush --json --session <sid>` there, its
# envelope on stdout.
m6_flush() {
  local out rc=0
  out=$(m6_on "$1" retain flush --json --session "$2" < /dev/null) || rc=$?
  m6_json "retain flush of $2 on $1 (exit $rc)" "$out"
}

# m6_doctor <who>: `doctor --json` there (non-zero when it finds an error).
m6_doctor() {
  local out rc=0
  out=$(m6_on "$1" doctor --json < /dev/null) || rc=$?
  m6_json "doctor on $1 (exit $rc)" "$out"
}

# m6_doctor_status <who> <check id>: that check's status ("missing" when absent).
m6_doctor_status() {
  local out
  out=$(m6_doctor "$1") || exit 1
  jq -r --arg id "$2" '[.data.checks[]? | select(.id == $id) | .status][0] // "missing"' <<<"$out"
}

# m6_session <who> <sid> <project> <from> <to> [tag] [age_min]: appends
# records <sid>-<tag><from>…<sid>-<tag><to> (tag r) to
# <home>/.claude/projects/-bs-m6-<project>/<sid>.jsonl, in rc_session's
# format, each text carrying [rec <uuid>], the newest age_min (0) minutes
# old and each earlier one a second older; the file's mtime is the
# newest record's time. The records' cwd is <home>/work/<project>.
m6_session() {
  local who=$1 sid=$2 project=$3 from=$4 to=$5 tag=${6:-r} age=${7:-0} home dir cwd newest i ts uuid role content
  home=$(m6_home "$who")
  dir=$home/.claude/projects/-bs-m6-$project cwd=$home/work/$project
  newest=$(($(date +%s) - age * 60))
  for i in $(seq "$from" "$to"); do
    ts=$(date -u -d "@$((newest - (to - i)))" +%Y-%m-%dT%H:%M:%S.000Z)
    uuid=$sid-$tag$i
    if [ $((i % 2)) = 0 ]; then
      role=assistant content="[{\"type\":\"text\",\"text\":\"Noted for $sid: the export pages by 200. [rec $uuid]\"}]"
    else
      role=user content="\"$sid turn $i: the export job of the M6 bench failed at page $i. [rec $uuid]\""
    fi
    printf '{"type":"%s","uuid":"%s","sessionId":"%s","cwd":"%s","timestamp":"%s","message":{"role":"%s","content":%s}}\n' \
      "$role" "$uuid" "$sid" "$cwd" "$ts" "$role" "$content"
  done | m6_sh "$who" "mkdir -p $(m6_q "$dir" "$cwd") && cat >> $(m6_q "$dir/$sid.jsonl") && touch -d @$newest $(m6_q "$dir/$sid.jsonl")" ||
    fail "cannot write session $sid on $who"
}

# m6_transcript <who> <sid> <project>: the transcript's path there.
m6_transcript() { printf '%s/.claude/projects/-bs-m6-%s/%s.jsonl\n' "$(m6_home "$1")" "$3" "$2"; }

# m6_copy <from> <to> <sid> <project>: the transcript copied as is (its
# records' cwd too), its mtime kept.
m6_copy() {
  local src dst mtime
  src=$(m6_transcript "$1" "$3" "$4") dst=$(m6_transcript "$2" "$3" "$4")
  mtime=$(m6_sh "$1" "stat -c %Y $(m6_q "$src")") || fail "no transcript $3 on $1"
  m6_get "$1" "$src" | m6_sh "$2" "mkdir -p $(m6_q "${dst%/*}") && cat > $(m6_q "$dst") && touch -d @$mtime $(m6_q "$dst")" ||
    fail "cannot copy session $3 from $1 to $2"
}

# m6_hash <who> <path>: the file's sha256 there, or "absent".
m6_hash() {
  m6_sh "$1" "if [ -e $(m6_q "$2") ]; then sha256sum < $(m6_q "$2") | cut -d' ' -f1; else echo absent; fi" || fail "cannot hash $2 on $1"
}

# m6_wait_doc <sid>: waits (5 min) until conversation:<sid> is on B's
# server with its metadata; prints the document.
m6_wait_doc() {
  local doc _
  for _ in $(seq 1 100); do
    doc=$(m6_doc "conversation:$1") || exit 1
    if [ -n "$doc" ] && jq -e '(.document_metadata.last_uuid // "") != ""' > /dev/null <<<"$doc"; then
      printf '%s\n' "$doc"
      return 0
    fi
    sleep 3
  done
  fail "conversation:$1 never reached the server with its metadata"
}

# ---- The install chain: A installs the server over real ssh (VM bench).

# m6_install_args [plan]: A's install command, I, in M6_I; with plan, I
# --plan, which reads no key.
m6_install_args() {
  local h
  h=$(m6_home a)
  M6_I=(server install --ssh "bs@$M6_SERVER_IP" --source "$h/release" --provider mock --model mock-model)
  if [ "${1:-}" != plan ]; then M6_I+=(--llm-key-from-file "$h/mock-key"); fi
  M6_I+=(--answer consent=yes --answer project=bs-m6 --answer cleartext_lan=yes --answer retention=keep --answer backfill=no --non-interactive --json)
  if [ "${1:-}" = plan ]; then M6_I+=(--plan); fi
}

# m6_server_config: the path of the server's config.toml.
m6_server_config() { m6_paths srv | jq -r .config_file; }

# ssh-unknown-host-key: A has never connected to the server; the install
# refuses its unknown key, changes nothing anywhere, and names the ssh to
# run. Then the user, who compared the fingerprint, trusts it.
m6_check_ssh_unknown_host_key() {
  local h kh before after out rc=0 pub
  h=$(m6_home a) kh=$(m6_home a)/.ssh/known_hosts
  m6_sh srv 'test -d "$HOME" && test ! -e "$HOME/.local/share/backsight"' ||
    fail "the server already holds ~/.local/share/backsight (or cannot be read): the check would prove nothing"
  before=$(m6_hash a "$kh")
  # The user's login key, as ssh-copy-id installs it: what is refused below
  # is the host key alone.
  m6_get a "$h/.ssh/id_ed25519.pub" | m6_sh srv 'mkdir -p -m 700 ~/.ssh && cat >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys' ||
    fail "cannot authorize A's key for bs on the server"
  pl_untraced printf 'mock-not-a-key\n' | m6_put a "$h/mock-key" || fail "cannot write A's mock key"
  m6_install_args
  out=$(m6_on a "${M6_I[@]}" < /dev/null 2> "$M6_WORK/unknown-host.err") || rc=$?
  out=$(m6_json "the install on A (exit $rc)" "$out") || exit 1
  [ "$rc" = 10 ] || fail "the install with an unknown host key: exit $rc, want 10 ($(jq -r '.error.message // ""' <<<"$out"))"
  jq -e --arg c "ssh bs@$M6_SERVER_IP" '[.error.actions[]?.cmd] | index($c) != null' > /dev/null <<<"$out" ||
    fail "exit 10 does not name the action ssh bs@$M6_SERVER_IP: $(jq -c '.error.actions' <<<"$out")"
  after=$(m6_hash a "$kh")
  [ "$after" = "$before" ] || fail "A's known_hosts changed: $before, now $after"
  m6_sh srv 'test ! -e "$HOME/.local/share/backsight"' || fail "the refused install left ~/.local/share/backsight on the server"
  pub=$(m6_sh srv 'cut -d" " -f1,2 /etc/ssh/ssh_host_ed25519_key.pub') && [ -n "$pub" ] || fail "cannot read the server's host key"
  printf '%s %s\n' "$M6_SERVER_IP" "$pub" | m6_sh a 'mkdir -p -m 700 ~/.ssh && cat >> ~/.ssh/known_hosts && chmod 600 ~/.ssh/known_hosts' ||
    fail "cannot add the server's key to A's known_hosts"
  m6_result ssh-unknown-host-key PASS "exit 10 naming ssh bs@$M6_SERVER_IP; A's known_hosts $before before and after; nothing on the server" \
    "$(jq -cn --arg kh "$before" '{known_hosts_before: $kh}')"
}

# ssh-dropped-mid-install: A's link is cut for M6_DROP_S s (90; ssh's
# keep-alives give up after 60 s) while the host runs its setup. The
# install stops resumable (exit 40), and nothing it printed holds a key.
m6_check_ssh_dropped_mid_install() {
  local h d rc out seen=0 _ hits
  h=$(m6_home a) d=$(m6_home a)/m6-drop
  m6_install_args
  m6_sh a "rm -rf $(m6_q "$d") && mkdir -p $(m6_q "$d")" || fail "cannot make $d on A"
  printf 'cd %s || exit 1\n%s %s > out 2> err < /dev/null\necho $? > code\n' "$(m6_q "$d")" "$(m6_q "$h/backsight")" "$(m6_q "${M6_I[@]}")" |
    m6_put a "$d/run.sh" || fail "cannot write the detached install on A"
  m6_sh a "setsid nohup sh $(m6_q "$d/run.sh") > /dev/null 2>&1 < /dev/null &" || fail "cannot start the detached install on A"
  # The host's setup relays its steps: its server step (the image start
  # and health wait) is the longest.
  for _ in $(seq 1 300); do
    if m6_sh a "grep -q '\"step\":\"host:server\"' $(m6_q "$d/err")"; then
      seen=1
      break
    fi
    if m6_sh a "test -e $(m6_q "$d/code")"; then
      fail "the install ended (exit $(m6_get a "$d/code")) before the host's setup reached its server step"
    fi
    sleep 2
  done
  [ "$seen" = 1 ] || fail "the host's setup never reached its server step (10 min)"
  m6_link a down
  sleep "${M6_DROP_S:-90}"
  m6_link a up
  for _ in $(seq 1 100); do
    rc=$(m6_get a "$d/code" 2> /dev/null) && [ -n "$rc" ] && break
    rc=
    sleep 3
  done
  [ -n "$rc" ] || fail "the install on A did not end within 5 min of the link coming back"
  out=$(m6_get a "$d/out") || fail "cannot read the install's output"
  out=$(m6_json "the dropped install on A (exit $rc)" "$out") || exit 1
  [ "$rc" = 40 ] || fail "the dropped install: exit $rc, want 40 ($(jq -r '.error.message // ""' <<<"$out" | m6_scrub))"
  jq -e '.error.resume // "" | startswith("backsight server install --ssh bs@")' > /dev/null <<<"$out" ||
    fail "exit 40 does not resume with backsight server install --ssh: $(jq -r '.error.resume // "none"' <<<"$out")"
  jq -r .error.resume <<<"$out" > "$M6_WORK/resume"
  # Positive control: the reader sees what the install printed.
  m6_sh a "grep -q '\"event\":\"progress\"' $(m6_q "$d/err") && test -s $(m6_q "$d/out")" || fail "the install printed no progress line: the key check would prove nothing"
  hits=$(m6_sh a "cat $(m6_q "$d/out") $(m6_q "$d/err") | grep -c -e mock-not-a-key -e 'bst1\\.' || true")
  [ "$hits" = 0 ] || fail "the install's output holds the LLM key or a token ($hits line(s))"
  m6_result ssh-dropped-mid-install PASS "link down ${M6_DROP_S:-90} s during host:server; exit 40, resumable; no key in the output ($(jq -r .error.message <<<"$out" | cut -c1-160))"
}

# ssh-install: the resume, once the host's setup cut by the drop is gone,
# installs the server and joins it from A.
m6_check_ssh_install() {
  local h resume out rc=0 deadline state cfg url mode doc srvshell unit active at
  h=$(m6_home a)
  [ -s "$M6_WORK/resume" ] || fail "no resume command from ssh-dropped-mid-install"
  resume=$(cat "$M6_WORK/resume")
  deadline=$((SECONDS + 900))
  while m6_sh srv "pgrep -u bs -f 'setup --role server' > /dev/null"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "a backsight setup still runs on the server after 15 min"
    sleep 10
  done
  out=$(m6_sh a ". ~/.bs-m6-env 2> /dev/null; $(m6_q "$h/backsight") ${resume#backsight } --json" < /dev/null 2> "$M6_WORK/install.err") || rc=$?
  out=$(m6_json "the resumed install on A (exit $rc)" "$out") || exit 1
  [ "$rc" = 0 ] || fail "the resumed install: exit $rc ($(jq -r '.error.message // ""' <<<"$out" | m6_scrub))"
  [ "$(jq -r '[.data.steps[] | select(.name == "release")][0].status' <<<"$out")" = skipped ] ||
    fail "the resume did not skip the release step: $(jq -c '[.data.steps[] | select(.name == "release")]' <<<"$out")"
  # Positive control: the install ran through bs's tcsh.
  srvshell=$(m6_sh srv 'getent passwd bs')
  [ "${srvshell##*:}" = /usr/bin/tcsh ] || fail "bs's login shell on the server is ${srvshell##*:}, not /usr/bin/tcsh: the install did not go through a non-POSIX shell"
  [ "$(m6_paths srv | jq -r .server_role)" = server ] || fail "the server's backsight is not in the server role"
  m6_sh srv 'docker compose ls --format json' | jq -e 'any(.[]; .Name == "bs-m6")' > /dev/null || fail "docker compose ls on the server lists no bs-m6"
  state=$(m6_paths srv | jq -r .state)
  m6_sh srv "test -d $(m6_q "$state") && test ! -e $(m6_q "$state/ssh-handback-token")" || fail "the handback token is still in $state on the server (or no state dir)"
  cfg=$(m6_paths a | jq -r .config_file)
  cfg=$(m6_get a "$cfg") || fail "cannot read A's config"
  mode=$(m6_toml server mode <<<"$cfg") url=$(m6_toml server url <<<"$cfg")
  [ "$mode $url" = "remote http://$M6_SERVER_IP:8888" ] || fail "A's config: mode '$mode' url '$url', want remote http://$M6_SERVER_IP:8888"
  doc=$(m6_doctor a) || exit 1
  jq -e '(.data.checks | length > 0) and ([.data.checks[] | select(.status == "error")] | length == 0)' > /dev/null <<<"$doc" ||
    fail "doctor on A reports errors: $(jq -c '[.data.checks[]? | select(.status == "error") | {id, summary}]' <<<"$doc")"
  [ "$(jq -r '[.data.checks[] | select(.id == "server-id") | .status][0]' <<<"$doc")" = ok ] ||
    fail "doctor's server-id on A: $(jq -c '[.data.checks[] | select(.id == "server-id")]' <<<"$doc")"
  # server-reach is info at best (never ok): present, and no warning.
  case $(jq -r '[.data.checks[] | select(.id == "server-reach") | .status][0] // "missing"' <<<"$doc") in
    info | ok) ;;
    *) fail "doctor's server-reach on A: $(jq -c '[.data.checks[] | select(.id == "server-reach")]' <<<"$doc")" ;;
  esac
  # For timers: A's tick timer right after the install.
  unit=$(m6_sh a "systemctl --user list-unit-files --no-legend --plain '*-tick.timer' | awk '{ print \$1 }'")
  [ "$(printf '%s\n' "$unit" | grep -c .)" = 1 ] || fail "A has $(printf '%s\n' "$unit" | grep -c .) tick timer(s), want one: $unit"
  active=$(m6_sh a "systemctl --user is-active $(m6_q "$unit")" || true)
  at=$(m6_sh a 'date +%s')
  printf '%s %s %s\n' "$unit" "$active" "$at" > "$M6_WORK/timers"
  m6_result ssh-install PASS "resumed: exit 0, release skipped; server role, compose bs-m6, no handback token; A remote on $url, doctor clean, server-id ok; bs's shell tcsh" \
    "$(jq -cn --argjson steps "$(jq -c '[.data.steps[] | {name, status}]' <<<"$out")" '{steps: $steps}')"
}

# ssh-changed-host-key: the server's host key changes; A's install (plan)
# refuses it as changed, without telling to remove the old one, and
# changes nothing; with the old key back it plans again.
m6_check_ssh_changed_host_key() {
  local kh before cfg cfg_before out rc=0 msg trusted now
  kh=$(m6_home a)/.ssh/known_hosts
  before=$(m6_hash a "$kh")
  cfg=$(m6_server_config)
  cfg_before=$(m6_hash srv "$cfg")
  [ "$cfg_before" != absent ] || fail "the server has no config.toml: the check would prove nothing"
  m6_install_args plan
  m6_host_key new
  # Positive control: the server now shows another key than A trusts.
  trusted=$(m6_sh a "awk '\$1 == \"$M6_SERVER_IP\" { print \$3 }' ~/.ssh/known_hosts")
  now=$(m6_sh srv 'cut -d" " -f2 /etc/ssh/ssh_host_ed25519_key.pub' || true)
  out=$(m6_on a "${M6_I[@]}" < /dev/null 2> /dev/null) || rc=$?
  m6_host_key old
  [ -n "$trusted" ] && [ -n "$now" ] && [ "$trusted" != "$now" ] || fail "the server's key did not change (A trusts '$trusted', the server shows '$now')"
  out=$(m6_json "the plan on A with a changed key (exit $rc)" "$out") || exit 1
  msg=$(jq -r '.error.message // ""' <<<"$out")
  [ "$rc" = 20 ] || fail "the plan with a changed host key: exit $rc, want 20 ($msg)"
  case $msg in *changed*) ;; *) fail "the refusal does not say the key changed: $msg" ;; esac
  case $(jq -c .error <<<"$out") in *"ssh-keygen -R"*) fail "the refusal tells to remove the old key (ssh-keygen -R): $msg" ;; esac
  [ "$(m6_hash a "$kh")" = "$before" ] || fail "A's known_hosts changed"
  [ "$(m6_hash srv "$cfg")" = "$cfg_before" ] || fail "the server's config.toml changed"
  rc=0
  out=$(m6_on a "${M6_I[@]}" < /dev/null 2> /dev/null) || rc=$?
  [ "$rc" = 0 ] || fail "the plan with the old key back: exit $rc ($(jq -r '.error.message // ""' <<<"$out" 2> /dev/null))"
  m6_result ssh-changed-host-key PASS "exit 20, the key changed, no ssh-keygen -R; A's known_hosts and the server's config unchanged; the old key back: plan exit 0"
}

# login-shells: the remote steps under every login shell, on the server
# (Task 3's TestRemoteStepsUnderEveryLoginShell, each shell required).
m6_check_login_shells() {
  local dir out rc=0 passed shells=(sh dash bash zsh fish tcsh csh) s n=0
  (cd "$M6_ROOT" && CGO_ENABLED=0 go test -c -o "$M6_WORK/cli.test" ./internal/cli/) > "$M6_WORK/cli.test.log" 2>&1 ||
    fail "go test -c ./internal/cli/ failed: $(tail -n 3 "$M6_WORK/cli.test.log")"
  dir=$(m6_home srv)/m6-shells
  m6_sh srv "rm -rf $(m6_q "$dir") && mkdir -p $(m6_q "$dir")" || fail "cannot make $dir on the server"
  m6_put srv "$dir/cli.test" < "$M6_WORK/cli.test" || fail "cannot copy cli.test to the server"
  out=$(m6_sh srv "cd $(m6_q "$dir") && chmod 700 cli.test && BACKSIGHT_REQUIRE_SHELLS=sh,dash,bash,zsh,fish,tcsh,csh ./cli.test -test.run '^TestRemoteStepsUnderEveryLoginShell\$' -test.v 2>&1") || rc=$?
  printf '%s\n' "$out" > "$M6_WORK/login-shells.log"
  [ "$rc" = 0 ] || fail "TestRemoteStepsUnderEveryLoginShell on the server: exit $rc: $(grep -E -- '--- (FAIL|SKIP)' <<<"$out" | head -n 3 | paste -sd ' ')"
  for s in "${shells[@]}"; do
    if grep -qE -- "^ *--- PASS: TestRemoteStepsUnderEveryLoginShell/$s " <<<"$out"; then n=$((n + 1)); else passed+="$s "; fi
  done
  [ "$n" = 7 ] || fail "$n of 7 shells passed (missing: $passed)"
  grep -q -- '^--- PASS: TestRemoteStepsUnderEveryLoginShell ' <<<"$out" || fail "the test itself did not pass"
  m6_result login-shells PASS "7 of 7 login shells pass on the server (${shells[*]})"
}

# two-clients: B joins the server A installed with a token from its host;
# each sends a session: both on the server once, each with its sender's
# machine id, both clients bound to the same server id.
m6_check_two_clients() {
  local state tok out rc=0 who sid mid ids doc counts sa sb ia ib
  state=$(m6_paths srv | jq -r .state)
  out=$(m6_on srv server token --out "$state/bench-token" --json < /dev/null) || fail "server token on the server failed: $(jq -r '.error.message // ""' <<<"$out" 2> /dev/null)"
  tok=$(m6_home b)/token
  m6_get srv "$state/bench-token" | m6_put b "$tok" || fail "cannot copy the token to B"
  m6_sh srv "rm -f $(m6_q "$state/bench-token")" || fail "cannot remove the token on the server"
  [ "$(m6_sh b "stat -c %a $(m6_q "$tok")")" = 600 ] || fail "B's token file is not 0600"
  pl_untraced m6_sh b "grep -q '^bst1\\.' $(m6_q "$tok")" || fail "B's token file holds no token"
  out=$(m6_on b connect --token-file "$tok" --answer consent=yes --answer backfill=no --answer retention=keep --answer scheduler=off \
    --non-interactive --json < /dev/null 2> "$M6_WORK/connect-b.err") || rc=$?
  out=$(m6_json "connect on B (exit $rc)" "$out") || exit 1
  [ "$rc" = 0 ] && jq -e .ok > /dev/null <<<"$out" || fail "connect on B: exit $rc ($(jq -r '.error.message // ""' <<<"$out" | m6_scrub))"
  for who in a b; do
    sid=tc-$who
    m6_session "$who" "$sid" bs-m6-two 1 3 r 120
    out=$(m6_flush "$who" "$sid") || exit 1
    jq -e --arg s "$sid" '.ok and ([.data.results[] | select(.session == $s and .status == "sent")] | length == 1)' > /dev/null <<<"$out" ||
      fail "$who did not send $sid: $(jq -c '.data.results // .error' <<<"$out")"
  done
  m6_queue_wait
  for who in a b; do
    sid=tc-$who
    doc=$(m6_wait_doc "$sid") || exit 1
    mid=$(m6_get "$who" "$(m6_paths "$who" | jq -r .state)/machine_id") || fail "cannot read $who's machine_id"
    [ -n "$mid" ] && [ "$(jq -r '.document_metadata.machine_id // ""' <<<"$doc")" = "$mid" ] ||
      fail "conversation:$sid's machine_id is '$(jq -r '.document_metadata.machine_id // ""' <<<"$doc")', $who's is '$mid'"
    printf '%s\n' "$mid" >> "$M6_WORK/machine-ids"
    counts=$(m6_uuid_counts "$sid") || exit 1
    [ "$counts" = "$(printf '%s 1\n' "$sid-r1" "$sid-r2" "$sid-r3")" ] || fail "conversation:$sid holds the records $(paste -sd ' ' <<<"$counts"), want r1 r2 r3 once each"
  done
  # Positive control: the two machine ids differ, so each document names its own sender.
  ids=$(sort -u "$M6_WORK/machine-ids" | grep -c .)
  [ "$ids" = 2 ] || fail "A and B have the same machine id: the machine_id check would prove nothing"
  for who in a b; do
    doc=$(m6_doctor "$who") || exit 1
    jq -e '(.data.checks | length > 0) and ([.data.checks[] | select(.status == "error")] | length == 0)' > /dev/null <<<"$doc" ||
      fail "doctor on $who reports errors: $(jq -c '[.data.checks[]? | select(.status == "error") | {id, summary}]' <<<"$doc")"
    [ "$(jq -r '[.data.checks[] | select(.id == "server-id") | .status][0]' <<<"$doc")" = ok ] ||
      fail "doctor's server-id on $who: $(jq -c '[.data.checks[] | select(.id == "server-id")]' <<<"$doc")"
  done
  sa=$(m6_paths a | jq -r .state) sb=$(m6_paths b | jq -r .state)
  ia=$(m6_get a "$sa/state.json" | jq -r '.remote_server.id // ""') ib=$(m6_get b "$sb/state.json" | jq -r '.remote_server.id // ""')
  [ -n "$ia" ] && [ "$ia" = "$ib" ] || fail "the clients' remote_server.id differ: A '$ia', B '$ib'"
  m6_result two-clients PASS "B joined; tc-a and tc-b on the server once each, with their sender's machine id; both doctors clean, server-id ok; server $ia" \
    "$(jq -cn --arg id "$ia" '{server_id: $id, records: 6}')"
}

# timers (evaluated last): A's tick timer was active right after the
# install, and once 15 min have passed the timer has run the tick, which
# exited 0, after the install (real timers, decision 0073).
m6_check_timers() {
  local unit active at svc show status exit_at now deadline
  [ -s "$M6_WORK/timers" ] || fail "ssh-install recorded no timer"
  read -r unit active at < "$M6_WORK/timers"
  [ "$active" = active ] || fail "right after the install, $unit was $active"
  svc=${unit%.timer}.service
  deadline=$((at + 20 * 60))
  while :; do
    now=$(m6_sh a 'date +%s')
    show=$(m6_sh a "systemctl --user show $(m6_q "$svc") -p ExecMainStatus -p ExecMainExitTimestamp --timestamp=unix")
    status=$(sed -n 's/^ExecMainStatus=//p' <<<"$show") exit_at=$(sed -n 's/^ExecMainExitTimestamp=@//p' <<<"$show")
    if [ "$now" -ge $((at + 15 * 60)) ] && [ -n "$exit_at" ] && [ "$exit_at" -gt "$at" ]; then break; fi
    [ "$now" -lt "$deadline" ] || fail "$svc did not run after the install within 20 min ($(paste -sd ' ' <<<"$show"))"
    sleep 30
  done
  [ "$status" = 0 ] || fail "$svc's last run exited $status"
  m6_result timers PASS "$unit active after the install; $svc ran $((exit_at - at)) s after it, exit 0" \
    "$(jq -cn --arg u "$unit" --argjson after "$((exit_at - at))" '{unit: $u, ran_after_install_s: $after}')"
}
