package scripts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test driver of scripts/m6-proofs.sh: each machine is a directory of
// the test (its home), m6_sh runs sh there, m6_on runs the fake backsight
// <dir>/bin/backsight with WHO set, fail prints FAIL: <message> and exits.
const m6TestDriver = `
m6_home() { echo "$T/$1"; }
m6_sh() { (cd "$T/$1" && HOME="$T/$1" sh -c "$2"); }
m6_on() { local w=$1; shift; WHO=$w HOME="$T/$w" "$T/bin/backsight" "$@"; }
m6_put() { m6_sh "$1" "umask 077 && cat > '$2'"; }
m6_get() { m6_sh "$1" "cat '$2'"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
`

// m6Lib sources scripts/m6-proofs.sh under set -euo pipefail with the
// driver, the fakes dir first on PATH, T, M6_RESULTS (T/results.jsonl) and
// M6_WORK (T/work) set; it returns stdout, stderr and the exit code.
func m6Lib(t *testing.T, dir, driver, script string, env ...string) (stdout, stderr string, code int) {
	t.Helper()
	path := mustAbs(t, "m6-proofs.sh")
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"work", "bin", "a", "b", "srv"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . "$1"; `+driver+"\n"+script, "m6", path)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "T="+dir,
		"M6_RESULTS="+filepath.Join(dir, "results.jsonl"), "M6_WORK="+filepath.Join(dir, "work"), "M6_VERSION=0.940.0", "M6_COMMIT=abc1234")
	cmd.Env = append(cmd.Env, env...)
	cmd.Dir = dir
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return o.String(), e.String(), ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String(), 0
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// m6ServerFakes gives B a config and key, backsight a paths --json naming
// them, and curl a fake answering with the code in <T>/code ("refused":
// no connection) and the body in <T>/body, logging its argv to
// <T>/curl.calls and the config -K gave it to <T>/curl.config.
func m6ServerFakes(t *testing.T, dir string) {
	t.Helper()
	for _, d := range []string{"b", "bin"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeLines(t, filepath.Join(dir, "b", "config.toml"), `bank = "not-this-one"`, "[server]", `mode = "remote"`, `url = "http://10.0.0.9:8888/"`, `bank = "bk"`)
	writeLines(t, filepath.Join(dir, "b", "secrets.toml"), `api_key = "k-S3CR3T"`)
	writeExec(t, filepath.Join(dir, "bin", "backsight"), `#!/bin/sh
case "$*" in
  "paths --json") printf '{"ok":true,"data":{"config_file":"%s/config.toml","secrets_file":"%s/secrets.toml","state":"%s/state"}}\n' "$HOME" "$HOME" "$HOME" ;;
  *) echo '{"ok":false}'; exit 1 ;;
esac
`)
	writeExec(t, filepath.Join(dir, "bin", "curl"), `#!/bin/sh
t=$(dirname "$(dirname "$0")")
printf '%s\n' "$*" >> "$t/curl.calls"
out=
while [ $# -gt 0 ]; do
  case $1 in
    -o) out=$2; shift ;;
    -K) cat "$2" >> "$t/curl.config"; shift ;;
  esac
  shift
done
code=$(cat "$t/code")
if [ "$code" = refused ]; then printf 000; exit 7; fi
[ -z "$out" ] || cat "$t/body" > "$out"
printf %s "$code"
`)
}

// RF 2: a document read is found (the body) or absent (404, empty); any
// other answer fails naming its status, never reads as absent. The key
// reaches curl only through -K, never its argv nor the trace.
func TestM6DocRefusesAnythingButFoundOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		code, want string
		fails      bool
	}{
		{"200", `{"id":"conversation:s1"}`, false},
		{"404", "", false},
		{"401", "HTTP 401", true},
		{"500", "HTTP 500", true},
		{"refused", "HTTP 000", true},
	} {
		dir := t.TempDir()
		m6ServerFakes(t, dir)
		writeLines(t, filepath.Join(dir, "code"), tc.code)
		writeLines(t, filepath.Join(dir, "body"), `{"id":"conversation:s1"}`)
		stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `set -x; doc=$(m6_doc conversation:s1); printf 'DOC[%s]\n' "$doc"`)
		if tc.fails {
			if code == 0 || !strings.Contains(stderr, "FAIL: reading document conversation:s1: "+tc.want) || strings.Contains(stdout, "DOC[") {
				t.Errorf("%s: exit %d, stdout %q, want a fail naming %s; stderr:\n%s", tc.code, code, stdout, tc.want, stderr)
			}
		} else if code != 0 || !strings.Contains(stdout, "DOC["+tc.want+"]") {
			t.Errorf("%s: exit %d, stdout %q, want DOC[%s]; stderr:\n%s", tc.code, code, stdout, tc.want, stderr)
		}
		calls, _ := os.ReadFile(filepath.Join(dir, "curl.calls"))
		if !strings.Contains(string(calls), "http://10.0.0.9:8888/v1/default/banks/bk/documents/conversation%3As1") {
			t.Errorf("%s: curl was called with %q, want the document of bank bk on B's server", tc.code, calls)
		}
		cfg, _ := os.ReadFile(filepath.Join(dir, "curl.config"))
		if !strings.Contains(string(cfg), `header = "Authorization: Bearer k-S3CR3T"`) {
			t.Errorf("%s: curl's -K config is %q, want B's key in its header", tc.code, cfg)
		}
		if strings.Contains(string(calls), "k-S3CR3T") || strings.Contains(stderr, "k-S3CR3T") {
			t.Errorf("%s: the key is in curl's argv or the trace", tc.code)
		}
	}
}

// RF 2: the counts read every segment appended to original_text, not a
// field holding the first one only, and a record sent twice counts 2. An
// absent document or one without original_text fails.
func TestM6UUIDCountsSeeEverySegment(t *testing.T) {
	dir := t.TempDir()
	m6ServerFakes(t, dir)
	writeLines(t, filepath.Join(dir, "code"), "200")
	doc, _ := json.Marshal(map[string]any{
		"id":      "conversation:sp1",
		"content": "user: sp1 turn 1 [rec sp1-r1]",
		"original_text": "user: sp1 turn 1 [rec sp1-r1]\nassistant: noted [rec sp1-r2]\n\n" +
			"user: sp1 turn 4 [rec sp1-a4]\nassistant: noted [rec sp1-r2]\n\nuser: sp1 turn 5 [rec sp1-b5]",
	})
	if err := os.WriteFile(filepath.Join(dir, "body"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_uuid_counts sp1`)
	want := "sp1-a4 1\nsp1-b5 1\nsp1-r1 1\nsp1-r2 2\n"
	if code != 0 || stdout != want {
		t.Errorf("exit %d, counts\n%s\nwant\n%s\nstderr:\n%s", code, stdout, want, stderr)
	}

	writeLines(t, filepath.Join(dir, "code"), "404")
	if _, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_uuid_counts sp1`); code == 0 || !strings.Contains(stderr, "conversation:sp1 is not on the server") {
		t.Errorf("absent document: exit %d, stderr %q", code, stderr)
	}
	writeLines(t, filepath.Join(dir, "code"), "200")
	writeLines(t, filepath.Join(dir, "body"), `{"id":"conversation:sp1","content":"[rec sp1-r1]"}`)
	if _, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_uuid_counts sp1`); code == 0 || !strings.Contains(stderr, "has no original_text") {
		t.Errorf("no original_text: exit %d, stderr %q", code, stderr)
	}
}

// RF 2: a skipped tick (another tick running, a maintenance) is retried,
// so it never reads as "sent nothing"; one that stays skipped fails, and
// so does a tick that did not succeed.
func TestM6TickRetriesASkippedTick(t *testing.T) {
	const fake = `#!/bin/sh
t=$(dirname "$(dirname "$0")")
n=$(($(cat "$t/calls" 2>/dev/null || echo 0) + 1))
echo "$n" > "$t/calls"
if [ "$n" -le "$(cat "$t/skips")" ]; then
  printf '{"ok":true,"data":{"status":"skipped","reason":"%s"}}\n' "$(cat "$t/reason")"
else
  cat "$t/final"
  exit "$(cat "$t/final-code")"
fi
`
	const done = `{"ok":true,"data":{"status":"done","sessions":{"sent":1}}}`
	run := func(skips, reason, final, finalCode string, env ...string) (stdout, stderr, calls string, code int) {
		dir := t.TempDir()
		m6Lib(t, dir, m6TestDriver, "true")
		writeExec(t, filepath.Join(dir, "bin", "backsight"), fake)
		writeLines(t, filepath.Join(dir, "skips"), skips)
		writeLines(t, filepath.Join(dir, "reason"), reason)
		writeLines(t, filepath.Join(dir, "final"), final)
		writeLines(t, filepath.Join(dir, "final-code"), finalCode)
		stdout, stderr, code = m6Lib(t, dir, m6TestDriver, `m6_tick a`, env...)
		raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return stdout, stderr, strings.TrimSpace(string(raw)), code
	}
	for _, reason := range []string{"another tick is running", "maintenance in progress"} {
		stdout, stderr, calls, code := run("2", reason, done, "0", "M6_TICK_EVERY=0")
		if code != 0 || strings.TrimSpace(stdout) != done || calls != "3" {
			t.Errorf("%s twice: exit %d, %s call(s), stdout %q (want the done tick alone, after 3 calls); stderr:\n%s", reason, code, calls, stdout, stderr)
		}
		stdout, stderr, calls, code = run("1000000", reason, done, "0", "M6_TICK_EVERY=0.2", "M6_TICK_WAIT=1")
		if code == 0 || !strings.Contains(stderr, "FAIL: jobs tick on a: still skipped ("+reason+") after 1 s") || stdout != "" {
			t.Errorf("always skipped (%s): exit %d, stdout %q, stderr:\n%s", reason, code, stdout, stderr)
		}
		if calls == "1" || calls == "" {
			t.Errorf("always skipped (%s): %q call(s), want it tried again before failing", reason, calls)
		}
	}
	stdout, stderr, _, code := run("0", "", `{"ok":false,"data":{"status":"partial"},"error":{"code":40,"message":"some notes stay queued"}}`, "40", "M6_TICK_EVERY=0")
	if code == 0 || !strings.Contains(stderr, "FAIL: jobs tick on a did not succeed (exit 40): some notes stay queued") || stdout != "" {
		t.Errorf("a failed tick: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
}

// Each result is one line and one JSONL record with its fields; no key or
// token passes through its values or detail.
func TestM6ResultWritesALineAndARecord(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, code := m6Lib(t, dir, m6TestDriver,
		`m6_result two-clients PASS "joined with bst1.eyJhIjoxfQ.sig" '{"records":6,"api_key":"k-S3CR3T","tokens":1200,"nested":{"token":"bst1.x","note":"got bst1.abc_DEF-1","llm_key":"m","max_tokens":9,"bearer_token":"t","client_secret":"s"}}'`)
	if code != 0 || stdout != "bs-m6 two-clients: PASS joined with [token]\n" {
		t.Fatalf("exit %d, stdout %q; stderr:\n%s", code, stdout, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d records, want 1:\n%s", len(lines), raw)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]any{"check": "two-clients", "status": "PASS", "detail": "joined with [token]", "version": "0.940.0", "commit": "abc1234"} {
		if rec[k] != v {
			t.Errorf("record %s = %v, want %v", k, rec[k], v)
		}
	}
	if at, _ := rec["at"].(string); len(at) != len("2026-10-29T10:00:00Z") || !strings.HasSuffix(at, "Z") {
		t.Errorf("record at = %q, want a UTC timestamp", rec["at"])
	}
	vals, _ := json.Marshal(rec["values"])
	if string(vals) != `{"nested":{"max_tokens":9,"note":"got [token]"},"records":6,"tokens":1200}` {
		t.Errorf("values = %s, want the counts (tokens, max_tokens) kept and every key or token gone", vals)
	}
	for _, s := range []string{"api_key", "bst1.", "k-S3CR3T"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("the record holds %q:\n%s", s, raw)
		}
	}
}

// A run stops at the first FAIL: the checks after it are NOT RUN, and do
// not run. A fail inside a command substitution, or a failing command,
// ends the check; one that records nothing fails too; m6_check refuses to
// run in a condition, where set -e would be off.
func TestM6CheckStopsTheRunAtTheFirstFail(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `
c1() { m6_result c1 PASS "one"; }
c2() { x=$(fail "boom in a substitution"); echo "after $x"; m6_result c2 PASS "never"; }
c3() { touch "$T/c3-ran"; m6_result c3 PASS "three"; }
m6_check c1 c1
echo "stopped after c1: [$M6_STOPPED]"
m6_check c2 c2
echo "stopped after c2: [$M6_STOPPED]"
m6_check c3 c3
`)
	want := "bs-m6 c1: PASS one\nstopped after c1: []\nbs-m6 c2: FAIL boom in a substitution\nstopped after c2: [c2]\nbs-m6 c3: NOT RUN after the FAIL of c2\n"
	if code != 0 || stdout != want {
		t.Errorf("exit %d, stdout\n%s\nwant\n%s\nstderr:\n%s", code, stdout, want, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "c3-ran")); err == nil {
		t.Error("c3 ran after the FAIL of c2")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	var statuses []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r struct{ Check, Status string }
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, r.Check+" "+r.Status)
	}
	if strings.Join(statuses, ", ") != "c1 PASS, c2 FAIL, c3 NOT RUN" {
		t.Errorf("records: %s", strings.Join(statuses, ", "))
	}

	dir = t.TempDir()
	stdout, _, _ = m6Lib(t, dir, m6TestDriver, `quiet() { :; }; failing() { x=$(false; echo leaked); m6_result failing PASS "$x"; }
m6_check quiet quiet; M6_STOPPED=; m6_check failing failing`)
	if stdout != "bs-m6 quiet: FAIL the check recorded no result\nbs-m6 failing: FAIL exited 1 without a message\n" {
		t.Errorf("a silent or erroring check: stdout\n%s", stdout)
	}
	// A fail swallowed by its caller still fails the check, whatever it
	// recorded after.
	dir = t.TempDir()
	stdout, _, _ = m6Lib(t, dir, m6TestDriver, `swallowed() { x=$(fail "inner read failed") || true; m6_result swallowed PASS "ok $x"; }
m6_check swallowed swallowed; echo "stopped: [$M6_STOPPED]"`)
	if !strings.HasSuffix(stdout, "bs-m6 swallowed: FAIL inner read failed\nstopped: [swallowed]\n") {
		t.Errorf("a swallowed fail: stdout\n%s", stdout)
	}
	dir = t.TempDir()
	stdout, stderr, code = m6Lib(t, dir, m6TestDriver, `c() { touch "$T/c-ran"; m6_result c PASS x; }; m6_check c c || true`)
	if code == 0 || !strings.Contains(stderr, "FAIL: m6_check c was called in a condition") || stdout != "" {
		t.Errorf("m6_check in a condition: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "c-ran")); err == nil {
		t.Error("the check ran in a condition")
	}
}

// RF 1: every server install --ssh the proofs issue goes through A; none
// through B (OpenSSH would use the user's own ~/.ssh) nor the server. The
// driver logs each call with its machine (and what it is given on stdin),
// answers each step as the real machines do, so that every check runs to
// its end (PASS) and each of its installs is seen; a B or server call
// naming --ssh or server install is a violation.
// m6ChainDriver answers each step of the install chain and two-clients
// as the real machines do, logging every call to <T>/calls; a B or server
// call naming --ssh or server install goes to <T>/violations. <T>/reach
// and <T>/plan-msg, when present, replace doctor's server-reach summary
// and the changed-key refusal.
const m6ChainDriver = `
log() { printf '%s\n' "$*" >> "$T/calls"; }
violation() { echo VIOLATION >> "$T/violations"; }
m6_home() { echo "/home/$1"; }
m6_on() {
  local w=$1; shift
  log "on $w $*"
  case "$w $*" in b*--ssh* | b*"server install"* | srv*"server install"*) violation ;; esac
  case "$w $*" in
    *"paths --json") echo '{"ok":true,"data":{"config_file":"/c/config.toml","secrets_file":"/c/secrets.toml","state":"/s","server_role":"server"}}' ;;
    "srv server token"*) echo '{"ok":true,"data":{}}' ;;
    "b connect"*) echo '{"ok":true,"data":{}}' ;;
    "a server install"*--plan*)
      if [ "$(cat "$T/hostkey" 2>/dev/null)" = new ]; then
        msg="the host key of bs@10.0.0.9 changed since you last connected"
        [ ! -e "$T/plan-msg" ] || msg=$(cat "$T/plan-msg")
        printf '{"ok":false,"error":{"code":20,"message":"%s"}}\n' "$msg"
        return 20
      fi
      echo '{"ok":true,"data":{}}' ;;
    "a server install"*)
      echo '{"ok":false,"error":{"code":10,"message":"unknown host key","actions":[{"kind":"run_as_user","cmd":"ssh bs@10.0.0.9"}]}}'
      return 10 ;;
    *"retain flush"*) printf '{"ok":true,"data":{"results":[{"session":"%s","status":"sent"}]}}\n' "${!#}" ;;
    *"doctor --json")
      reach="last answer now"
      [ ! -e "$T/reach" ] || reach=$(cat "$T/reach")
      printf '{"ok":true,"data":{"checks":[{"id":"server-id","status":"ok"},{"id":"server-reach","status":"info","summary":"%s"}]}}\n' "$reach" ;;
    *) echo '{"ok":false,"error":{"message":"fake"}}'; return 1 ;;
  esac
}
m6_sh() {
  local in
  in=$(cat)
  log "sh $1 $2 <<$(printf %s "$in" | tr '\n' ' ')"
  case "$1 $2 $in" in b*--ssh* | b*"server install"* | srv*"server install"*) violation ;; esac
  case "$1 $2" in
    *pgrep*) return 1 ;;
    *"get /home/a/m6-drop/code") echo 40 ;;
    *"get /home/a/m6-drop/out") echo '{"ok":false,"error":{"code":40,"message":"dropped","resume":"backsight server install --ssh bs@10.0.0.9 --non-interactive"}}' ;;
    "a "*"server install"*) echo '{"ok":true,"data":{"steps":[{"name":"release","status":"skipped"}]}}' ;;
    *"get /c/config.toml") printf '[server]\nmode = "remote"\nurl = "http://10.0.0.9:8888"\n' ;;
    *"get /c/secrets.toml") echo 'api_key = "k"' ;;
    *"get /s/machine_id") echo "mid-$1" ;;
    *"get /s/state.json") echo '{"remote_server":{"id":"srv-1"}}' ;;
    *"get /s/bench-token") echo bst1.fake ;;
    *sha256sum*) if [ "$1" = srv ]; then echo abc; else echo absent; fi ;;
    *"stat -c %a"*) echo 600 ;;
    *"grep -c"*) echo 0 ;;
    *"getent passwd bs"*) echo 'bs:x:1001:1001::/home/bs:/usr/bin/tcsh' ;;
    *"docker compose ls"*) echo '[{"Name":"bs-m6"}]' ;;
    *list-unit-files*) echo backsight-tick.timer ;;
    *is-active*) echo active ;;
    *"date +%s"*) echo 1000 ;;
    *"-f2 /etc/ssh/ssh_host_ed25519_key.pub"*) echo KEYNEW ;;
    *"/etc/ssh/ssh_host_ed25519_key.pub"*) echo 'ssh-ed25519 KEYOLD' ;;
    *known_hosts*awk* | *awk*known_hosts*) echo KEYOLD ;;
  esac
}
m6_put() { m6_sh "$1" "put $2"; }
m6_get() { m6_sh "$1" "get $2" < /dev/null; }
m6_link() { log "link $*"; }
m6_host_key() { log "host_key $*"; echo "$1" > "$T/hostkey"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
M6_SERVER_IP=10.0.0.9 M6_DROP_S=0
`

// m6ChainCurl serves B's server: no pending operation, and each tc-<who>
// document with its three records and machine id mid-<who>.
const m6ChainCurl = `#!/bin/sh
out=
while [ $# -gt 1 ]; do
  case $1 in -o) out=$2; shift ;; -K) cat "$2" > /dev/null; shift ;; esac
  shift
done
case $1 in *operations*) echo '{"total":0}'; exit 0 ;; esac
sid=${1##*conversation%3A}
printf '{"id":"conversation:%s","document_metadata":{"last_uuid":"%s-r3","machine_id":"mid-%s"},"original_text":"[rec %s-r1] [rec %s-r2] [rec %s-r3]"}' \
  "$sid" "$sid" "${sid#tc-}" "$sid" "$sid" "$sid" > "$out"
printf 200
`

// m6Chain runs one check against m6ChainDriver; it returns stdout, stderr
// and the calls.
func m6Chain(t *testing.T, dir, check string) (stdout, stderr, calls string) {
	t.Helper()
	m6Lib(t, dir, m6ChainDriver, "true")
	writeExec(t, filepath.Join(dir, "bin", "curl"), m6ChainCurl)
	writeLines(t, filepath.Join(dir, "work", "resume"), "backsight server install --ssh bs@10.0.0.9 --source /home/a/release --non-interactive")
	stdout, stderr, _ = m6Lib(t, dir, m6ChainDriver, `m6_check `+check+` m6_check_`+strings.ReplaceAll(check, "-", "_")+` < /dev/null`)
	raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
	return stdout, stderr, string(raw)
}

func TestM6ProofsNeverRunSSHInstallOnB(t *testing.T) {
	for _, c := range []string{"ssh-unknown-host-key", "ssh-dropped-mid-install", "ssh-install", "ssh-changed-host-key", "two-clients"} {
		dir := t.TempDir()
		stdout, stderr, calls := m6Chain(t, dir, c)
		if !strings.HasPrefix(stdout, "bs-m6 "+c+": PASS ") {
			t.Errorf("%s did not run to its end against the driver: %s\nstderr:\n%s\ncalls:\n%s", c, stdout, stderr, calls)
		}
		if v, _ := os.ReadFile(filepath.Join(dir, "violations")); len(v) > 0 {
			t.Errorf("%s: a server install --ssh went through B or the server:\n%s", c, calls)
		}
		installs := 0
		for _, l := range strings.Split(calls, "\n") {
			if (strings.HasPrefix(l, "on a ") || strings.HasPrefix(l, "sh a ")) && strings.Contains(strings.ReplaceAll(l, "'", ""), "server install --ssh") {
				installs++
			}
		}
		want := map[string]int{"ssh-unknown-host-key": 1, "ssh-dropped-mid-install": 1, "ssh-install": 1, "ssh-changed-host-key": 2, "two-clients": 0}[c]
		if installs != want {
			t.Errorf("%s: %d server install --ssh through A, want %d:\n%s", c, installs, want, calls)
		}
		if c == "two-clients" && !strings.Contains(calls, "on b connect --token-file") {
			t.Errorf("%s: B never ran connect (the check would prove nothing):\n%s", c, calls)
		}
	}
}

// The install chain's reads refuse look-alikes: a server-reach doctor
// skipped (no config read) is not a reach, and a revoked host key's
// refusal ("… Nothing was changed") is not a changed one.
func TestM6InstallChainRefusesLookalikes(t *testing.T) {
	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "reach"), "skipped: no backsight config")
	if stdout, stderr, _ := m6Chain(t, dir, "ssh-install"); !strings.HasPrefix(stdout, "bs-m6 ssh-install: FAIL doctor's server-reach on A was skipped") {
		t.Errorf("a skipped server-reach: stdout %q, stderr:\n%s", stdout, stderr)
	}
	dir = t.TempDir()
	writeLines(t, filepath.Join(dir, "plan-msg"), "the host key of bs@10.0.0.9 is marked revoked in your ssh configuration. Nothing was changed")
	if stdout, stderr, _ := m6Chain(t, dir, "ssh-changed-host-key"); !strings.HasPrefix(stdout, "bs-m6 ssh-changed-host-key: FAIL the refusal does not say the key changed") {
		t.Errorf("a revoked key's refusal: stdout %q, stderr:\n%s", stdout, stderr)
	}
}

// vb_authorize_a, run's preparation: A's public login key, read on A,
// appended to bs's authorized_keys on the server, as ssh-copy-id would.
func TestVMBenchRunAuthorizesAsKey(t *testing.T) {
	fakes := newBenchFakes(t)
	// The fake ssh decodes the remote script (vb_remote) and logs it with
	// its host and stdin; on the client it prints a public key.
	writeExec(t, filepath.Join(fakes, "ssh"), `#!/bin/sh
d=$(dirname "$0")
host=$3
script=$(printf '%s' "$4" | sed -n 's/.*printf %s \([A-Za-z0-9+\/=]*\) | base64 -d.*/\1/p' | base64 -d)
in=$(cat)
printf '%s|%s|%s\n' "$host" "$script" "$in" >> "$d/ssh.calls"
case $host in bs-m6-client-bs) echo 'ssh-ed25519 AAAAPUB bs@bs-m6-client' ;; esac
`)
	bench := filepath.Join(t.TempDir(), "bs-m6")
	if err := os.MkdirAll(bench, 0o700); err != nil {
		t.Fatal(err)
	}
	lib := mustAbs(t, "m6-proofs.sh")
	_, stderr, code := vmBench(t, fakes, t.TempDir(), bench, `. "`+lib+`"; vb_authorize_a`)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	calls := benchCalls(t, fakes, "ssh")
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "bs-m6-client-bs|") || !strings.Contains(calls[0], "cat '/home/bs/.ssh/id_ed25519.pub'") ||
		!strings.HasPrefix(calls[1], "bs-m6-server-bs|") || !strings.Contains(calls[1], ">> ~/.ssh/authorized_keys") ||
		!strings.HasSuffix(calls[1], "|ssh-ed25519 AAAAPUB bs@bs-m6-client") {
		t.Errorf("ssh calls:\n%s", strings.Join(calls, "\n"))
	}
}

// The bench's own driver refuses --ssh on B before running anything, and
// runs B's backsight in VB_DIR/client-b with no profile, XDG dir or agent
// of the user's.
func TestVMBenchClientBIsIsolated(t *testing.T) {
	fakes := newBenchFakes(t)
	bench := filepath.Join(t.TempDir(), "bs-m6")
	for _, d := range []string{"bin", "client-b"} {
		if err := os.MkdirAll(filepath.Join(bench, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(bench, "bin", "backsight"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+bench+"/backsight.calls\"\nenv > \""+bench+"/backsight.env\"\npwd > \""+bench+"/backsight.pwd\"\n")
	lib := mustAbs(t, "m6-proofs.sh")
	var stderr string
	var code int
	for _, words := range []string{"server install --ssh bs@10.0.0.9 --json", "server install --ssh=bs@10.0.0.9 --json", "--json server install --ssh bs@10.0.0.9",
		"server install --provider mock --json", "server start", "--json server start", "--config-dir x server start", "connect --ssh=bs@10.0.0.9", "version --ssh"} {
		_, stderr, code = vmBench(t, fakes, t.TempDir(), bench, `. "`+lib+`"; m6_on b `+words,
			"CLAUDE_CONFIG_DIR=/home/u/.claude", "XDG_CONFIG_HOME=/home/u/.config", "SSH_AUTH_SOCK=/run/agent")
		if code == 0 || !strings.Contains(stderr, "client B never runs") {
			t.Errorf("m6_on b %s: exit %d, stderr %q", words, code, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(bench, "backsight.calls")); err == nil {
		t.Error("B's backsight ran a server command or a server install --ssh")
	}
	// Positive control: another command runs B's backsight, isolated.
	_, stderr, code = vmBench(t, fakes, t.TempDir(), bench, `. "`+lib+`"; m6_on b version --json`,
		"CLAUDE_CONFIG_DIR=/home/u/.claude", "XDG_CONFIG_HOME=/home/u/.config", "XDG_STATE_HOME=/home/u/.state", "SSH_AUTH_SOCK=/run/agent")
	if code != 0 {
		t.Fatalf("m6_on b version: exit %d, stderr %q", code, stderr)
	}
	env, _ := os.ReadFile(filepath.Join(bench, "backsight.env"))
	vars := map[string]string{}
	for _, l := range strings.Split(string(env), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			vars[k] = v
		}
	}
	if vars["HOME"] != filepath.Join(bench, "client-b") || vars["BACKSIGHT_HOME"] != filepath.Join(bench, "client-b", "backsight") {
		t.Errorf("B runs with HOME %q, BACKSIGHT_HOME %q", vars["HOME"], vars["BACKSIGHT_HOME"])
	}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "SSH_AUTH_SOCK"} {
		if v, ok := vars[k]; ok {
			t.Errorf("B runs with %s=%s", k, v)
		}
	}
	if !strings.HasPrefix(vars["PATH"], filepath.Join(bench, "cache", "claude-code-")) || strings.Contains(vars["PATH"], "/home/") {
		t.Errorf("B's PATH is %q, want its own Claude Code first and no user directory", vars["PATH"])
	}
	if pwd, _ := os.ReadFile(filepath.Join(bench, "backsight.pwd")); strings.TrimSpace(string(pwd)) != filepath.Join(bench, "client-b") {
		t.Errorf("B runs in %q", pwd)
	}
}
