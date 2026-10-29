package scripts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

// The keys the run learnt (m6_learn_key: B's, the server's old and new)
// are masked in a result's detail and values and in a check's fail
// message, on stderr and in its FAIL record; tokens in a fail message too.
// Learning a key never traces it, and a machine with no backsight adds
// nothing and fails nothing.
func TestM6ScrubMasksTheKeysTheRunKnows(t *testing.T) {
	const kb, ks1, ks2 = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0", "s1s1s1s1-s1s1s1s1_s1s1s1s1", "s2s2s2s2s2s2s2s2s2s2s2s2s2s2"
	dir := t.TempDir()
	m6ServerFakes(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "srv"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(dir, "b", "secrets.toml"), `api_key = "`+kb+`"`)
	writeLines(t, filepath.Join(dir, "srv", "secrets.toml"), `api_key = "`+ks1+`"`)
	if err := os.WriteFile(filepath.Join(dir, "new"), []byte(ks2), 0o600); err != nil { // the key after the rotation
		t.Fatal(err)
	}
	stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `mkdir -p "$M6_WORK"
set -x
m6_learn_key b; m6_learn_key srv
m6_learn_key nobody
set +x
printf 'api_key = "%s"\n' "$(cat "$T/new")" > "$T/srv/secrets.toml"
m6_learn_key srv
m6_result c1 PASS "B said '`+kb+`' and the server '`+ks1+`'" '{"note":"`+ks2+` rotated","n":2}'
c2() { fail "the flush printed `+ks2+` and bst1.abc_DEF"; }
m6_check c2 c2
stat -c %a "$M6_WORK/known-keys"`, "PS4=+ ")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	want := "bs-m6 c1: PASS B said '[key]' and the server '[key]'\nbs-m6 c2: FAIL the flush printed [key] and [token]\n600\n"
	if stdout != want {
		t.Errorf("stdout\n%s\nwant\n%s", stdout, want)
	}
	if !strings.Contains(stderr, "bs-m6 c2: the flush printed [key] and [token]\n") {
		t.Errorf("the fail line on stderr is not scrubbed:\n%s", stderr)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	if !strings.Contains(string(raw), `"values":{"note":"[key] rotated","n":2}`) {
		t.Errorf("values not scrubbed:\n%s", raw)
	}
	for _, k := range []string{kb, ks1, ks2} {
		if strings.Contains(stdout+stderr+string(raw), k) {
			t.Errorf("key %s shown:\nstdout %s\nstderr %s\nrecords %s", k, stdout, stderr, raw)
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
    *"get /home/a/m6-drop/err") echo '{"event":"progress","step":"host:server"}'; [ ! -e "$T/leak-drop" ] || cat "$T/leak-drop" ;;
    "a "*"server install"*)
      echo '{"event":"progress","step":"host:server"}' >&2
      [ ! -e "$T/leak-resume" ] || cat "$T/leak-resume" >&2
      echo '{"ok":true,"data":{"steps":[{"name":"release","status":"skipped"}]}}' ;;
    *"get /c/config.toml") printf '[server]\nmode = "remote"\nurl = "http://10.0.0.9:8888"\n' ;;
    *"get /c/secrets.toml") echo 'api_key = "k3y-0f-the-server-0123456789"' ;;
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

// The server's API key (A's once installed) is looked for in the dropped
// install's output by ssh-dropped-mid-install, and in both the dropped and
// the resumed install's output by ssh-install; a hit fails the check,
// without showing the key.
func TestM6InstallOutputHoldsNoServerKey(t *testing.T) {
	const key = "k3y-0f-the-server-0123456789"
	for _, c := range []struct{ check, leak, want string }{
		{"ssh-dropped-mid-install", "leak-drop", "the install's output holds the LLM key, a token or the server's API key (1 line(s))"},
		{"ssh-install", "leak-drop", "the dropped or the resumed install's output holds the server's API key, the LLM key or a token (1 line(s))"},
		{"ssh-install", "leak-resume", "the dropped or the resumed install's output holds the server's API key, the LLM key or a token (1 line(s))"},
	} {
		dir := t.TempDir()
		writeLines(t, filepath.Join(dir, c.leak), `{"event":"progress","detail":"key `+key+`"}`)
		stdout, stderr, _ := m6Chain(t, dir, c.check)
		if want := "bs-m6 " + c.check + ": FAIL " + c.want + "\n"; stdout != want {
			t.Errorf("%s with the key in %s: stdout %q, want %q\nstderr:\n%s", c.check, c.leak, stdout, want, stderr)
		}
		if strings.Contains(stdout+stderr, key) {
			t.Errorf("%s shows the key:\n%s%s", c.check, stdout, stderr)
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

// m6Record is one line of a synthetic transcript m6_session writes.
type m6Record struct {
	Type, UUID, SessionID, Cwd, Timestamp string
	Message                               struct {
		Role    string
		Content json.RawMessage
	}
}

func m6Transcript(t *testing.T, path string) []m6Record {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recs []m6Record
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r m6Record
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		recs = append(recs, r)
	}
	return recs
}

// A session continued (here, or on the PC it was copied to) goes on after
// its last record, whatever age the caller gives: the shared records r and
// this PC's own (a), in order, each text naming its uuid; the file's mtime
// is its newest record's time.
func TestM6SessionWritesSharedAndOwnRecords(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	_, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_session a sp1 bs-m6-dedup 1 3 r 90
m6_session a sp1 bs-m6-dedup 4 4 a 90
m6_session a sp1 bs-m6-dedup 5 5 a 120`)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	path := filepath.Join(dir, "a", ".claude", "projects", "-bs-m6-bs-m6-dedup", "sp1.jsonl")
	recs := m6Transcript(t, path)
	var uuids []string
	var prev time.Time
	for i, r := range recs {
		uuids = append(uuids, r.UUID)
		if r.SessionID != "sp1" || r.Cwd != filepath.Join(dir, "a", "work", "bs-m6-dedup") || !strings.Contains(string(r.Message.Content), "[rec "+r.UUID+"]") {
			t.Errorf("record %d: session %q, cwd %q, content %s", i, r.SessionID, r.Cwd, r.Message.Content)
		}
		ts, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && !ts.After(prev) {
			t.Errorf("record %s at %s is not after the one before it (%s)", r.UUID, r.Timestamp, prev.UTC().Format(time.RFC3339))
		}
		prev = ts
	}
	if strings.Join(uuids, " ") != "sp1-r1 sp1-r2 sp1-r3 sp1-a4 sp1-a5" {
		t.Errorf("records %v, want sp1-r1 sp1-r2 sp1-r3 sp1-a4 sp1-a5", uuids)
	}
	r3, _ := time.Parse(time.RFC3339Nano, recs[2].Timestamp)
	if want := start.Add(-90 * time.Minute); r3.Before(want.Add(-5*time.Second)) || r3.After(time.Now().Add(-90*time.Minute).Add(5*time.Second)) {
		t.Errorf("sp1-r3 at %s, want 90 min ago (%s)", recs[2].Timestamp, want.UTC().Format(time.RFC3339))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().Unix() != prev.Unix() {
		t.Errorf("mtime %s, want the newest record's time %s", fi.ModTime().UTC(), prev.UTC())
	}
}

// The copy is the transcript as it is, with its mtime: a session written
// 120 min ago stays 120 min old on the other PC.
func TestM6CopyKeepsTheMtime(t *testing.T) {
	dir := t.TempDir()
	_, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_session a cp1 bs-m6-dedup 1 3 r 120; m6_copy a b cp1 bs-m6-dedup`)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	rel := filepath.Join(".claude", "projects", "-bs-m6-bs-m6-dedup", "cp1.jsonl")
	src, _ := os.ReadFile(filepath.Join(dir, "a", rel))
	dst, err := os.ReadFile(filepath.Join(dir, "b", rel))
	if err != nil || !bytes.Equal(src, dst) || len(src) == 0 {
		t.Fatalf("B's copy differs from A's transcript (%v):\n%s\n---\n%s", err, src, dst)
	}
	fa, _ := os.Stat(filepath.Join(dir, "a", rel))
	fb, _ := os.Stat(filepath.Join(dir, "b", rel))
	if !fa.ModTime().Equal(fb.ModTime()) {
		t.Errorf("B's copy has mtime %s, A's %s", fb.ModTime(), fa.ModTime())
	}
	if age := time.Since(fb.ModTime()); age < 119*time.Minute || age > 121*time.Minute {
		t.Errorf("B's copy is %s old, want 120 min", age.Round(time.Second))
	}
}

// m6TickFake answers jobs tick: skipped <T>/skips times, then <T>/final
// with exit <T>/final-code; it counts its calls in <T>/calls.
const m6TickFake = `#!/bin/sh
t=$(dirname "$(dirname "$0")")
n=$(($(cat "$t/calls" 2>/dev/null || echo 0) + 1))
echo "$n" > "$t/calls"
if [ "$n" -le "$(cat "$t/skips")" ]; then
  echo '{"ok":true,"data":{"status":"skipped","reason":"another tick is running"}}'
else
  cat "$t/final"
  exit "$(cat "$t/final-code")"
fi
`

// The old key's tick (key-rotation) is partial: m6_tick takes it only when
// told to, still retries a skipped tick first, and fails any other failure.
func TestM6TickTakesAPartialTickOnlyWhenAsked(t *testing.T) {
	const partial = `{"ok":false,"data":{"status":"partial","sessions":{"retry":1},"notes":{"sent":0,"kept":1}},"error":{"code":20,"message":"the server refused backsight (401): run backsight doctor"}}`
	run := func(skips, final, finalCode, script string) (stdout, stderr, calls string, code int) {
		dir := t.TempDir()
		m6Lib(t, dir, m6TestDriver, "true")
		writeExec(t, filepath.Join(dir, "bin", "backsight"), m6TickFake)
		writeLines(t, filepath.Join(dir, "skips"), skips)
		writeLines(t, filepath.Join(dir, "final"), final)
		writeLines(t, filepath.Join(dir, "final-code"), finalCode)
		stdout, stderr, code = m6Lib(t, dir, m6TestDriver, script, "M6_TICK_EVERY=0")
		raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return stdout, stderr, strings.TrimSpace(string(raw)), code
	}
	if stdout, stderr, _, code := run("0", partial, "20", `m6_tick a`); code == 0 || stdout != "" || !strings.Contains(stderr, "FAIL: jobs tick on a did not succeed (exit 20)") {
		t.Errorf("a partial tick, not asked for: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
	if stdout, stderr, calls, code := run("2", partial, "20", `m6_tick a partial`); code != 0 || strings.TrimSpace(stdout) != partial || calls != "3" {
		t.Errorf("a partial tick, asked for, after two skips: exit %d, %s call(s), stdout %q, stderr:\n%s", code, calls, stdout, stderr)
	}
	const usage = `{"ok":false,"error":{"code":2,"message":"unknown flag"}}`
	if stdout, stderr, _, code := run("0", usage, "2", `m6_tick a partial`); code == 0 || stdout != "" || !strings.Contains(stderr, "FAIL: jobs tick on a did not succeed (exit 2): unknown flag") {
		t.Errorf("a failed tick that is not partial, partial asked for: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
}

// m6StateFake answers backsight paths --json with the state dir
// <HOME>/state, and retain flush --json from <T>/flush.<n> (n: the call),
// counted in <T>/flushes.
const m6StateFake = `#!/bin/sh
t=$(dirname "$(dirname "$0")")
case "$*" in
  "paths --json") printf '{"ok":true,"data":{"state":"%s/state"}}\n' "$HOME" ;;
  "retain flush --json --session"*)
    n=$(($(cat "$t/flushes" 2>/dev/null || echo 0) + 1))
    echo "$n" > "$t/flushes"
    cat "$t/flush.$n" ;;
  *) echo '{"ok":false}'; exit 1 ;;
esac
`

// m6_live_since moves state.json's retain_since back, never forward,
// keeping every other field and the file's mode, under the state lock it
// takes and releases; a lock someone else holds is waited for, then the
// helper fails with state.json and that lock left as they were.
func TestM6LiveSinceOnlyMovesRetainSinceBack(t *testing.T) {
	setup := func(retainSince string) string {
		dir := t.TempDir()
		m6Lib(t, dir, m6TestDriver, "true")
		writeExec(t, filepath.Join(dir, "bin", "backsight"), m6StateFake)
		if err := os.MkdirAll(filepath.Join(dir, "a", "state", "locks"), 0o700); err != nil {
			t.Fatal(err)
		}
		st := `{"schema_version":1,"remote_server":{"id":"s1"}` + retainSince + `}`
		if err := os.WriteFile(filepath.Join(dir, "a", "state", "state.json"), []byte(st), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	read := func(dir string) (since time.Time, raw []byte) {
		raw, err := os.ReadFile(filepath.Join(dir, "a", "state", "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			SchemaVersion int                 `json:"schema_version"`
			RemoteServer  struct{ ID string } `json:"remote_server"`
			RetainSince   time.Time           `json:"retain_since"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		if st.SchemaVersion != 1 || st.RemoteServer.ID != "s1" {
			t.Errorf("state.json lost its other fields: %s", raw)
		}
		return st.RetainSince, raw
	}
	near := func(got, want time.Time) bool { d := got.Sub(want); return d > -time.Minute && d < time.Minute }
	for _, tc := range []struct{ name, field string }{
		{"absent", ""},
		{"later", `,"retain_since":"` + time.Now().UTC().Format(time.RFC3339) + `"`},
	} {
		dir := setup(tc.field)
		if _, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_live_since a 150`); code != 0 {
			t.Fatalf("%s: exit %d, stderr:\n%s", tc.name, code, stderr)
		}
		if since, raw := read(dir); !near(since, time.Now().Add(-150*time.Minute)) {
			t.Errorf("%s: retain_since %s, want 150 min ago: %s", tc.name, since, raw)
		}
		fi, _ := os.Stat(filepath.Join(dir, "a", "state", "state.json"))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: state.json is %v, want 0600", tc.name, fi.Mode().Perm())
		}
		left, _ := filepath.Glob(filepath.Join(dir, "a", "state", "locks", "*"))
		tmp, _ := filepath.Glob(filepath.Join(dir, "a", "state", "state.json?*"))
		if len(left)+len(tmp) > 0 {
			t.Errorf("%s: left behind %v %v", tc.name, left, tmp)
		}
	}
	// state.json that jq cannot read: the helper fails and leaves neither
	// a temp file nor its lock.
	dir := setup("")
	writeLines(t, filepath.Join(dir, "a", "state", "state.json"), "{not json")
	if _, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_live_since a 150`); code == 0 || !strings.Contains(stderr, "FAIL: cannot move a's retain_since back") {
		t.Errorf("unreadable state.json: exit %d, stderr:\n%s", code, stderr)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "a", "state", "locks", "*")); len(left) > 0 {
		t.Errorf("unreadable state.json: the lock stayed: %v", left)
	}
	if tmp, _ := filepath.Glob(filepath.Join(dir, "a", "state", "state.json?*")); len(tmp) > 0 {
		t.Errorf("unreadable state.json: a temp file stayed: %v", tmp)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "a", "state", "state.json")); string(raw) != "{not json\n" {
		t.Errorf("unreadable state.json was replaced: %q", raw)
	}
	dir = setup(`,"retain_since":"2020-01-02T03:04:05Z"`)
	if _, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_live_since a 150`); code != 0 {
		t.Fatalf("earlier: exit %d, stderr:\n%s", code, stderr)
	}
	if since, raw := read(dir); !since.Equal(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("an earlier retain_since moved forward: %s", raw)
	}
	dir = setup(`,"retain_since":"2026-10-29T02:45:00Z"`)
	lock := filepath.Join(dir, "a", "state", "locks", "state.lock")
	writeLines(t, lock, `{"pid":1,"command":"jobs tick","since":"2026-10-29T02:45:00Z"}`)
	_, before := read(dir)
	if _, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_live_since a 150`, "M6_LOCK_WAIT=3"); code == 0 || !strings.Contains(stderr, "FAIL: cannot move a's retain_since back") {
		t.Errorf("a held state lock: exit %d, stderr:\n%s", code, stderr)
	}
	if _, after := read(dir); !bytes.Equal(before, after) {
		t.Errorf("state.json changed under another process's lock:\n%s\n%s", before, after)
	}
	if raw, err := os.ReadFile(lock); err != nil || !strings.Contains(string(raw), "jobs tick") {
		t.Errorf("the other process's lock was removed or changed (%v): %s", err, raw)
	}
}

// m6_flush_sent: the flush sent the session, or a tick (A's timer) did
// before it, which only the cursor at the wanted record proves; another
// flusher holding the session is waited for; anything else fails.
func TestM6FlushSentNeedsTheCursorToProveATicksSend(t *testing.T) {
	const sent = `{"ok":true,"data":{"results":[{"session":"sp1","status":"sent","segment":2}],"counts":{"sent":1}}}`
	const nothing = `{"ok":true,"data":{"results":[],"counts":{"nothing":1}}}`
	const locked = `{"ok":true,"data":{"results":[{"session":"sp1","status":"locked"}],"counts":{"locked":1}}}`
	const retry = `{"ok":false,"data":{"results":[{"session":"sp1","status":"retry","detail":"server busy"}],"counts":{"retry":1}},"error":{"code":40,"message":"some sessions could not be sent now"}}`
	for _, tc := range []struct {
		name    string
		answers []string
		cursor  string // last_uuid of the cursor; "" none
		want    string // stdout, or "FAIL: …" in stderr
		flushes string
	}{
		{"sent", []string{sent}, "", "sent", "1"},
		{"locked then sent", []string{locked, sent}, "", "sent", "2"},
		{"sent by a tick", []string{nothing}, "sp1-a5", "tick", "1"},
		{"nothing, cursor elsewhere", []string{nothing}, "sp1-r3", "FAIL: the flush of sp1 on a sent nothing, and its cursor ends at sp1-r3, not sp1-a5", "1"},
		{"nothing, no cursor", []string{nothing}, "", "FAIL: the flush of sp1 on a sent nothing, and its cursor ends at none, not sp1-a5", "1"},
		{"retry", []string{retry}, "", "FAIL: the flush of sp1 on a: retry (exit", "1"},
	} {
		dir := t.TempDir()
		m6Lib(t, dir, m6TestDriver, "true")
		writeExec(t, filepath.Join(dir, "bin", "backsight"), m6StateFake)
		for i, a := range tc.answers {
			writeLines(t, filepath.Join(dir, "flush."+strconv.Itoa(i+1)), a)
		}
		if tc.cursor != "" {
			if err := os.MkdirAll(filepath.Join(dir, "a", "state", "cursors"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeLines(t, filepath.Join(dir, "a", "state", "cursors", "sp1.json"), `{"last_uuid":"`+tc.cursor+`","segment":2}`)
		}
		stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_flush_sent a sp1 sp1-a5`, "M6_TICK_EVERY=0")
		flushes, _ := os.ReadFile(filepath.Join(dir, "flushes"))
		if strings.TrimSpace(string(flushes)) != tc.flushes {
			t.Errorf("%s: %s flush(es), want %s", tc.name, flushes, tc.flushes)
		}
		if strings.HasPrefix(tc.want, "FAIL: ") {
			if code == 0 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Errorf("%s: exit %d, stdout %q, stderr:\n%s", tc.name, code, stdout, stderr)
			}
			continue
		}
		if code != 0 || strings.TrimSpace(stdout) != tc.want {
			t.Errorf("%s: exit %d, stdout %q, want %s; stderr:\n%s", tc.name, code, stdout, tc.want, stderr)
		}
	}
}

// timers: a tick that ran before key-rotation's new key (and failed on the
// old one) is not the run the check judges: it waits for one after both
// the install and the key change.
func TestM6TimersJudgesARunAfterTheKeyChange(t *testing.T) {
	const driver = `
m6_home() { echo "$T/$1"; }
m6_sh() {
  case $2 in
    "date +%s") n=$(($(cat "$T/now") + 300)); echo "$n" > "$T/now"; echo "$n" ;;
    *"systemctl --user show"*)
      if [ "$(cat "$T/now")" -lt 2100 ]; then printf 'ExecMainStatus=1\nExecMainExitTimestamp=@1500\n'
      else printf 'ExecMainStatus=0\nExecMainExitTimestamp=@2100\n'; fi ;;
  esac
}
fail() { echo "FAIL: $*" >&2; exit 1; }
`
	for _, tc := range []struct {
		name, after, want string
	}{
		{"a key change at 2000", "2000", "bs-m6 timers: PASS backsight-tick.timer active after the install; backsight-tick.service ran 1100 s after the install and 100 s after key-rotation's new key, exit 0"},
		{"no key change", "", "bs-m6 timers: FAIL backsight-tick.service's last run exited 1"},
	} {
		dir := t.TempDir()
		m6Lib(t, dir, driver, "true")
		writeLines(t, filepath.Join(dir, "work", "timers"), "backsight-tick.timer active 1000")
		writeLines(t, filepath.Join(dir, "now"), "1660")
		if tc.after != "" {
			writeLines(t, filepath.Join(dir, "work", "timers-after"), tc.after)
		}
		stdout, stderr, _ := m6Lib(t, dir, driver, `m6_check timers m6_check_timers`, "M6_TIMERS_EVERY=0")
		if !strings.HasPrefix(stdout, tc.want) {
			t.Errorf("%s: stdout %q, want %q; stderr:\n%s", tc.name, stdout, tc.want, stderr)
		}
	}
}

// However a run ends, no token stays in client B's home: two-clients'
// (the old one once the key rotated) and key-rotation's new one.
func TestVMBenchRunEndRemovesBsTokens(t *testing.T) {
	fakes := newBenchFakes(t)
	bench := filepath.Join(t.TempDir(), "bs-m6")
	if err := os.MkdirAll(filepath.Join(bench, "client-b"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"token", "token2", "keep.txt"} {
		writeLines(t, filepath.Join(bench, "client-b", n), "x")
	}
	if _, stderr, code := vmBench(t, fakes, t.TempDir(), bench, `vb_run_end`); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, n := range []string{"token", "token2"} {
		if _, err := os.Stat(filepath.Join(bench, "client-b", n)); err == nil {
			t.Errorf("client-b/%s is still there", n)
		}
	}
	if _, err := os.Stat(filepath.Join(bench, "client-b", "keep.txt")); err != nil {
		t.Errorf("the run's end removed another file of B's: %v", err)
	}
	if calls := benchCalls(t, fakes, "virsh"); !strings.Contains(strings.Join(calls, "\n"), "domif-setlink bs-m6-client") {
		t.Errorf("the run's end did not bring A's link up: virsh calls %q", calls)
	}
	// The tokens go first: a later step that stops the trap leaves none.
	for _, n := range []string{"token", "token2"} {
		writeLines(t, filepath.Join(bench, "client-b", n), "x")
	}
	vmBench(t, fakes, t.TempDir(), bench, `vb_untrust_new_key() { false; }; vb_run_end`)
	for _, n := range []string{"token", "token2"} {
		if _, err := os.Stat(filepath.Join(bench, "client-b", n)); err == nil {
			t.Errorf("client-b/%s stayed when a later step of the run's end failed", n)
		}
	}
}

// m6KeyRotationFake is each machine's backsight for key-rotation, by WHO:
// paths under its home, the server's rotation writing the new token, a
// queued note, the old key's partial tick, the old token refused, and the
// new token's connect ok (same-server) but on the machine <T>/fails names.
// It logs every call to <T>/calls.
const m6KeyRotationFake = `#!/bin/sh
t=$(dirname "$(dirname "$0")")
echo "$WHO $*" >> "$t/calls"
case "$*" in
  "paths --json") printf '{"ok":true,"data":{"state":"%s/state","config_file":"%s/config.toml","secrets_file":"%s/secrets.toml"}}\n' "$HOME" "$HOME" "$HOME" ;;
  "server token --rotate --out "*) out=${4#--out }; out=$5; umask 077; echo bst1.new > "$out"; echo '{"ok":true,"data":{}}' ;;
  store*) echo '{"ok":true,"data":{"status":"queued","document":"note:1"}}' ;;
  "jobs tick --json") echo '{"ok":false,"data":{"status":"partial","sessions":{"retry":1},"notes":{"sent":0,"kept":1}},"error":{"code":20,"message":"401"}}'; exit 20 ;;
  "connect --token-file "*/token\ *) echo '{"ok":false,"error":{"code":20,"message":"the server rejected this token'"'"'s key (rotated?): ask the host"}}'; exit 20 ;;
  "connect --token-file "*/token2\ *)
    if [ "$WHO" != "$(cat "$t/fails")" ]; then echo '{"ok":true,"data":{"situation":"same-server"}}'; exit 0; fi
    echo '{"ok":false,"error":{"code":20,"message":"'"$WHO"' could not connect"}}'; exit 20 ;;
  *) echo '{"ok":false}'; exit 1 ;;
esac
`

// m6KeyRotationCurl: the banks list answers 200 once, then 401 (the
// rotated key), or 200 always when <T>/old-key-kept exists; every document
// is absent.
const m6KeyRotationCurl = `#!/bin/sh
t=$(dirname "$(dirname "$0")")
for a in "$@"; do last=$a; done
case $last in
  */banks) n=$(($(cat "$t/banks" 2>/dev/null || echo 0) + 1)); echo "$n" > "$t/banks"
    if [ "$n" = 1 ] || [ -e "$t/old-key-kept" ]; then printf 200; else printf 401; fi ;;
  *) printf 404 ;;
esac
`

// key-rotation reconnects each PC with the scheduler answer its own setup
// recorded (A on, B off: the CI chain's homes answer off), never one
// tied to the driver; whatever step fails once the rotation wrote the new
// token, it is removed on every machine. Each copy the trap removes is
// one a case needs: the server's (a fail before the token is handed out),
// A's (B fails first) and B's (a fail before B's connect).
func TestM6KeyRotationKeepsEachSchedulerAndRemovesTheNewToken(t *testing.T) {
	run := func(fails string, opts ...string) (stdout, stderr, calls, dir string) {
		dir = t.TempDir()
		m6Lib(t, dir, m6TestDriver, "true")
		writeExec(t, filepath.Join(dir, "bin", "backsight"), m6KeyRotationFake)
		writeExec(t, filepath.Join(dir, "bin", "curl"), m6KeyRotationCurl)
		writeLines(t, filepath.Join(dir, "fails"), fails)
		for _, o := range opts {
			if o == "old-key-kept" {
				writeLines(t, filepath.Join(dir, "old-key-kept"), "x")
			}
		}
		for who, sched := range map[string]string{"a": "on", "b": "off", "srv": ""} {
			if err := os.MkdirAll(filepath.Join(dir, who, "state", "cursors"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeLines(t, filepath.Join(dir, who, "state", "state.json"), `{"schema_version":1}`)
			if who != "b" || !slices.Contains(opts, "b-no-cursor") {
				writeLines(t, filepath.Join(dir, who, "state", "cursors", "tc-"+who+".json"), `{"last_uuid":"tc-r3"}`)
			}
			if sched != "" {
				writeLines(t, filepath.Join(dir, who, "state", "setup.json"), `{"answers":{"scheduler":"`+sched+`","consent":"yes"}}`)
			}
		}
		writeLines(t, filepath.Join(dir, "b", "config.toml"), "[server]", `url = "http://10.0.0.9:8888"`, `bank = "bk"`)
		writeLines(t, filepath.Join(dir, "b", "secrets.toml"), `api_key = "old"`)
		if err := os.WriteFile(filepath.Join(dir, "b", "token"), []byte("bst1.old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, _ = m6Lib(t, dir, m6TestDriver, `m6_check key-rotation m6_check_key_rotation`, "M6_TICK_EVERY=0", "M6_LOCK_WAIT=3")
		raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return stdout, stderr, string(raw), dir
	}
	tokensGone := func(name, dir string) {
		for _, f := range []string{filepath.Join("a", "token2"), filepath.Join("b", "token2"), filepath.Join("srv", "state", "bench-token2")} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				t.Errorf("%s: %s stayed after the check failed", name, f)
			}
		}
	}
	// A fails: both connects ran, each with its machine's own answer.
	stdout, stderr, calls, dir := run("a")
	if !strings.HasPrefix(stdout, "bs-m6 key-rotation: FAIL connect on a with the new token: exit 20 (a could not connect)") {
		t.Fatalf("A failing: stdout %q; stderr:\n%s", stdout, stderr)
	}
	var connects []string
	for _, l := range strings.Split(calls, "\n") {
		if strings.Contains(l, "connect --token-file") && strings.Contains(l, "/token2 ") {
			connects = append(connects, l)
		}
	}
	if len(connects) != 2 || !strings.HasPrefix(connects[0], "b ") || !strings.Contains(connects[0], "--answer scheduler=off") ||
		!strings.HasPrefix(connects[1], "a ") || !strings.Contains(connects[1], "--answer scheduler=on") {
		t.Errorf("the new token's connects, want B with scheduler=off then A with scheduler=on:\n%s", strings.Join(connects, "\n"))
	}
	tokensGone("A failing", dir)
	// B fails first: A's copy of the new token was never used, and goes too.
	stdout, stderr, _, dir = run("b")
	if !strings.HasPrefix(stdout, "bs-m6 key-rotation: FAIL connect on b with the new token: exit 20 (b could not connect)") {
		t.Fatalf("B failing: stdout %q; stderr:\n%s", stdout, stderr)
	}
	tokensGone("B failing", dir)
	// A fail before B's connect: B's copy (and A's) were never used.
	stdout, stderr, _, dir = run("none", "b-no-cursor")
	if !strings.HasPrefix(stdout, "bs-m6 key-rotation: FAIL b has no session cursor") {
		t.Fatalf("no cursor on B: stdout %q; stderr:\n%s", stdout, stderr)
	}
	tokensGone("no cursor on B", dir)
	// A fail between the rotation and the old token's connect (the old key
	// still accepted): the server's new token goes.
	stdout, stderr, calls, dir = run("none", "old-key-kept")
	if !strings.HasPrefix(stdout, "bs-m6 key-rotation: FAIL the banks list with the old key after the rotation: HTTP 200, want 401") {
		t.Fatalf("old key kept: stdout %q; stderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(calls, "srv server token --rotate --out ") {
		t.Fatalf("old key kept: the rotation never ran (the case would prove nothing):\n%s", calls)
	}
	tokensGone("old key kept", dir)
}

// m6_scheduler reads the scheduler answer that machine's setup recorded;
// none recorded fails rather than guessing.
func TestM6SchedulerIsTheMachinesOwnAnswer(t *testing.T) {
	for _, tc := range []struct{ setup, want string }{
		{`{"answers":{"scheduler":"on"}}`, "on"},
		{`{"answers":{"scheduler":"off"}}`, "off"},
		{`{"answers":{"consent":"yes"}}`, "FAIL: a's setup recorded no scheduler answer"},
		{"", "FAIL: a's setup recorded no scheduler answer"},
	} {
		dir := t.TempDir()
		m6Lib(t, dir, m6TestDriver, "true")
		writeExec(t, filepath.Join(dir, "bin", "backsight"), m6StateFake)
		if err := os.MkdirAll(filepath.Join(dir, "a", "state"), 0o700); err != nil {
			t.Fatal(err)
		}
		if tc.setup != "" {
			writeLines(t, filepath.Join(dir, "a", "state", "setup.json"), tc.setup)
		}
		stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `s=$(m6_scheduler a); echo "S[$s]"`)
		if strings.HasPrefix(tc.want, "FAIL: ") {
			if code == 0 || strings.Contains(stdout, "S[") || !strings.Contains(stderr, tc.want) {
				t.Errorf("%q: exit %d, stdout %q, stderr:\n%s", tc.setup, code, stdout, stderr)
			}
		} else if code != 0 || strings.TrimSpace(stdout) != "S["+tc.want+"]" {
			t.Errorf("%q: exit %d, stdout %q, stderr:\n%s", tc.setup, code, stdout, stderr)
		}
	}
}

// RF 2: a document's facts are counted from the server's memory list of
// that document (observations aside); a page that does not hold them all,
// or any answer but 200, fails rather than reading as fewer facts.
func TestM6FactsCountsADocumentsFacts(t *testing.T) {
	for _, tc := range []struct {
		name, code, body, want string
	}{
		{"facts", "200", `{"total":3,"items":[{"fact_type":"world","document_id":"conversation:cc1"},{"fact_type":"experience","document_id":"conversation:cc1"},{"fact_type":"observation","document_id":"conversation:cc1"}]}`, "2"},
		{"the filter ignored", "200", `{"total":2,"items":[{"fact_type":"world","document_id":"conversation:cc1"},{"fact_type":"world","document_id":"conversation:tc-a"}]}`, "FAIL: the memories listed for conversation:cc1 include another document's (conversation:tc-a)"},
		{"none", "200", `{"total":0,"items":[]}`, "0"},
		{"a short page", "200", `{"total":600,"items":[{"fact_type":"world","document_id":"conversation:cc1"}]}`, "FAIL: the memories of conversation:cc1: 1 of 600 read"},
		{"401", "401", `{}`, "FAIL: reading the memories of conversation:cc1: HTTP 401"},
		{"refused", "refused", `{}`, "FAIL: reading the memories of conversation:cc1: HTTP 000"},
	} {
		dir := t.TempDir()
		m6ServerFakes(t, dir)
		writeLines(t, filepath.Join(dir, "code"), tc.code)
		writeLines(t, filepath.Join(dir, "body"), tc.body)
		stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `n=$(m6_facts conversation:cc1); echo "N[$n]"`)
		if strings.HasPrefix(tc.want, "FAIL: ") {
			if code == 0 || strings.Contains(stdout, "N[") || !strings.Contains(stderr, tc.want) {
				t.Errorf("%s: exit %d, stdout %q, stderr:\n%s", tc.name, code, stdout, stderr)
			}
		} else if code != 0 || strings.TrimSpace(stdout) != "N["+tc.want+"]" {
			t.Errorf("%s: exit %d, stdout %q, want N[%s]; stderr:\n%s", tc.name, code, stdout, tc.want, stderr)
		}
		calls, _ := os.ReadFile(filepath.Join(dir, "curl.calls"))
		if !strings.Contains(string(calls), "http://10.0.0.9:8888/v1/default/banks/bk/memories/list?document_id=conversation%3Acc1&limit=500") {
			t.Errorf("%s: curl was called with %q", tc.name, calls)
		}
	}
}

// Every run tests the tree it runs from: after the reset and before the
// first check, run builds the binary and its signed release afresh, gives
// them to A (~/backsight, ~/release) and is B's binary (VB_DIR/bin).
func TestVMBenchRunRebuildsBeforeTheFirstCheck(t *testing.T) {
	fakes := newBenchFakes(t)
	order := filepath.Join(fakes, "order")
	// make: the build writes OUT with this run's content; ssh logs its host
	// and command, keeps what A's ~/backsight receives, and answers A's
	// version check.
	writeExec(t, filepath.Join(fakes, "make"), `#!/bin/sh
d=$(dirname "$0")
echo "make $*" >> "$d/order"
[ ! -e "$d/make-fails" ] || exit 2
for a in "$@"; do case $a in OUT=*) printf 'built-this-run\n' > "${a#OUT=}" ;; esac; done
`)
	writeExec(t, filepath.Join(fakes, "ssh"), `#!/bin/sh
d=$(dirname "$0")
host=$3
shift 3
echo "ssh $host $*" >> "$d/order"
case "$*" in
  *"cat > ~/backsight"*) cat > "$d/a-backsight" ;;
  *"tar -xf"*) cat > "$d/a-release.tar" ;;
  *"version --json"*) echo '{"data":{"version":"0.940.0"}}' ;;
esac
exit 0
`)
	bench := filepath.Join(t.TempDir(), "bs-m6")
	root := t.TempDir()
	for _, d := range []string{filepath.Join(bench, "bin"), filepath.Join(root, "scripts")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeLines(t, filepath.Join(bench, "bin", "backsight"), "built-at-up")
	writeLines(t, filepath.Join(bench, "ssh_config"), "Host bs-m6-server", "  HostName 10.0.0.9")
	writeLines(t, filepath.Join(bench, "minisign.key"), "k")
	writeLines(t, filepath.Join(bench, "minisign.pub"), "untrusted comment", "RWPUB")
	writeLines(t, filepath.Join(root, "LICENSE"), "l")
	writeLines(t, filepath.Join(root, "README.md"), "r")
	writeLines(t, filepath.Join(root, "scripts", "m6-proofs.sh"), `m6_check() { echo "check $1" >> "$ORDER"; }`)
	script := `VB_ROOT=` + root + `
vb_is_up() { :; }
vb_reset() { echo reset >> "$ORDER"; }
vb_b_claude() { :; }
vb_authorize_a() { :; }
vb_run two-clients`
	_, stderr, code := vmBench(t, fakes, t.TempDir(), bench, script, "ORDER="+order)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	raw, _ := os.ReadFile(order)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	idx := func(prefix string) int {
		for i, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return i
			}
		}
		return -1
	}
	reset, build, bin, rel, check := idx("reset"), idx("make"), idx("ssh bs-m6-client-bs cat > ~/backsight"), idx("ssh bs-m6-client-bs rm -rf ~/release"), idx("check ")
	if reset < 0 || build <= reset || bin <= build || rel <= build || check <= bin || check <= rel || lines[check] != "check ssh-unknown-host-key" {
		t.Errorf("want reset, build, A's binary and release, then the first check; got:\n%s", raw)
	}
	for _, f := range []string{filepath.Join(bench, "bin", "backsight"), filepath.Join(fakes, "a-backsight"), filepath.Join(bench, "release", "v0.940.0", "backsight_0.940.0_linux_amd64")} {
		if b, err := os.ReadFile(f); err != nil || string(b) != "built-this-run\n" {
			t.Errorf("%s holds %q (%v), want this run's build", f, b, err)
		}
	}
	if tarball, _ := os.ReadFile(filepath.Join(fakes, "a-release.tar")); !bytes.Contains(tarball, []byte("release/v0.940.0/checksums.txt")) {
		t.Error("A did not receive the new release with its checksums")
	}
	if calls := strings.Join(benchCalls(t, fakes, "minisign"), "\n"); !strings.Contains(calls, "-S ") {
		t.Errorf("the release was not signed: minisign calls %q", calls)
	}
	// The build trusts the bench's minisign key (a testkey build).
	if build < 0 || !strings.Contains(lines[build], "TEST_KEY=RWPUB") || !strings.Contains(lines[build], "VERSION=0.940.0") {
		t.Errorf("the build does not trust the bench's key: %q", lines[max(build, 0)])
	}
	// A failing build stops the run before any check.
	writeLines(t, filepath.Join(fakes, "make-fails"), "x")
	if err := os.Remove(order); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := vmBench(t, fakes, t.TempDir(), bench, script, "ORDER="+order); code == 0 || !strings.Contains(stderr, "make build failed") {
		t.Errorf("a failing build: exit %d, stderr %q", code, stderr)
	}
	if raw, _ := os.ReadFile(order); strings.Contains(string(raw), "check ") {
		t.Errorf("a check ran after a failing build:\n%s", raw)
	}
}

// RF 2: the reconciles of a PC are read from its telemetry, the product's
// record of every tick, the VM timer's too: one line per tick since the
// time given (its epoch line, "-" for none), not a skipped tick, another
// job or an earlier tick, across the months' files. No telemetry is no
// tick: an empty answer, which the check's own tick must then fill.
func TestM6ReconcilesReadsEveryTickSince(t *testing.T) {
	dir := t.TempDir()
	m6ServerFakes(t, dir)
	tel := filepath.Join(dir, "a", "state", "telemetry")
	since := "2026-10-29T10:00:00.000Z"
	stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_reconciles a `+since+`; echo END`)
	if code != 0 || stdout != "END\n" {
		t.Errorf("no telemetry: exit %d, stdout %q, want nothing; stderr:\n%s", code, stdout, stderr)
	}
	if err := os.MkdirAll(tel, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(tel, "events-2026-09.jsonl"),
		`{"ts":"2026-09-30T10:00:00.000Z","event":"job","name":"tick","status":"done","details":{"epoch":"restore epoch 0 -> 1: 1 session(s) resume from the server"}}`)
	writeLines(t, filepath.Join(tel, "events-2026-10.jsonl"),
		`{"ts":"2026-10-29T09:59:59.999Z","event":"job","name":"tick","status":"done","details":{"epoch":"restore epoch 1 -> 2: 3 session(s) resume from the server"}}`,
		`{"ts":"2026-10-29T10:00:01.000Z","event":"job","name":"tick","status":"skipped","details":{"reason":"maintenance in progress"}}`,
		`{"ts":"2026-10-29T10:00:02.000Z","event":"job","name":"maintain","status":"done","details":{"epoch":"restore epoch 9 -> 9"}}`,
		`{"ts":"2026-10-29T10:00:03.000Z","event":"hook","name":"tick","details":{"epoch":"restore epoch 8 -> 8"}}`,
		`not json`,
		`{"ts":"2026-10-29T10:00:04.000Z","event":"job","name":"tick","status":"partial","details":{"epoch":"held: restore epoch not reconciled: maintenance"}}`,
		`{"ts":"2026-10-29T10:01:00.000Z","event":"job","name":"tick","status":"done","details":{"epoch":"restore epoch 2 -> 3: 1 session(s) resume from the server"}}`,
		`{"ts":"2026-10-29T10:02:00.000Z","event":"job","name":"tick","status":"done","details":{"sessions":{"nothing":2}}}`)
	writeLines(t, filepath.Join(tel, "events-2026-11.jsonl"),
		`{"ts":"2026-11-01T00:00:00.000Z","event":"job","name":"tick","status":"done","details":{"epoch":""}}`)
	stdout, stderr, code = m6Lib(t, dir, m6TestDriver, `m6_reconciles a `+since)
	want := "held: restore epoch not reconciled: maintenance\nrestore epoch 2 -> 3: 1 session(s) resume from the server\n-\n-\n"
	if code != 0 || stdout != want {
		t.Errorf("exit %d, lines\n%s\nwant\n%s\nstderr:\n%s", code, stdout, want, stderr)
	}
	// m6_ticks: the same ticks as whole events, so a timer's reconcile
	// tells what it sent.
	stdout, stderr, code = m6Lib(t, dir, m6TestDriver, `m6_ticks a `+since+` | jq -r '"\(.ts) \(.details.sessions.sent // "-")"'`)
	if want := "2026-10-29T10:00:04.000Z -\n2026-10-29T10:01:00.000Z -\n2026-10-29T10:02:00.000Z -\n2026-11-01T00:00:00.000Z -\n"; code != 0 || stdout != want {
		t.Errorf("m6_ticks: exit %d, stdout\n%s\nwant\n%s\nstderr:\n%s", code, stdout, want, stderr)
	}
	writeLines(t, filepath.Join(tel, "events-2026-12.jsonl"),
		`{"ts":"2026-12-01T00:00:00.000Z","event":"job","name":"tick","status":"done","details":{"epoch":"restore epoch 3 -> 4: 1 session(s) resume from the server","sessions":{"sent":1}}}`)
	stdout, stderr, code = m6Lib(t, dir, m6TestDriver, `m6_ticks a 2026-12-01T00:00:00.000Z | jq -r '.details.sessions.sent'`)
	if code != 0 || stdout != "1\n" {
		t.Errorf("m6_ticks sent: exit %d, stdout %q; stderr:\n%s", code, stdout, stderr)
	}
	// A telemetry that cannot be read fails: never "no tick".
	stdout, stderr, code = m6Lib(t, dir, m6TestDriver, `m6_sh() { return 3; }; m6_reconciles a `+since+`; echo END`)
	if code == 0 || strings.Contains(stdout, "END") || !strings.Contains(stderr, "FAIL: cannot read a's telemetry") {
		t.Errorf("unreadable telemetry: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
}

// RF 2 and the order of checks (the CI chain runs another): before a check
// counts what each tick sends, both PCs are settled: each ticks, the queue
// drains, until both ticks send nothing (sessions or notes); a PC that
// keeps sending fails the check after M6_SETTLE_ROUNDS rounds, never
// passing on a count an earlier check's session would spoil.
func TestM6SettleTicksUntilNothingIsSent(t *testing.T) {
	const fake = `#!/bin/sh
t=$(dirname "$(dirname "$0")")
case "$*" in
  "jobs tick --json")
    n=$(($(cat "$t/calls-$WHO" 2>/dev/null || echo 0) + 1))
    echo "$n" > "$t/calls-$WHO"
    line=$(sed -n "${n}p" "$t/ticks-$WHO")
    [ -n "$line" ] || line=$(tail -n 1 "$t/ticks-$WHO")
    echo "$line" ;;
  "paths --json") printf '{"ok":true,"data":{"config_file":"%s/config.toml","secrets_file":"%s/secrets.toml","state":"%s/state"}}\n' "$HOME" "$HOME" "$HOME" ;;
  *) echo '{"ok":false}'; exit 1 ;;
esac
`
	const curl = `#!/bin/sh
t=$(dirname "$(dirname "$0")")
echo x >> "$t/queue.calls"
echo '{"total":0}'
`
	const zero = `{"ok":true,"data":{"status":"done","sessions":{"nothing":2},"notes":{"sent":0}}}`
	const one = `{"ok":true,"data":{"status":"done","sessions":{"sent":1},"notes":{"sent":0}}}`
	const note = `{"ok":true,"data":{"status":"done","sessions":{"nothing":2},"notes":{"sent":1}}}`
	for _, tc := range []struct {
		name         string
		a, b         []string
		calls, fails string
	}{
		{"settled at once", []string{zero}, []string{zero}, "1 1", ""},
		{"a sends then nothing", []string{one, zero}, []string{zero}, "2 2", ""},
		{"b's note then nothing", []string{zero}, []string{note, zero}, "2 2", ""},
		{"a keeps sending", []string{one}, []string{zero}, "3 3", "a session from an earlier check was still pending after 3 settle round(s): a sent {\"sent\":1} (notes 0)"},
	} {
		dir := t.TempDir()
		m6ServerFakes(t, dir)
		writeExec(t, filepath.Join(dir, "bin", "backsight"), fake)
		writeExec(t, filepath.Join(dir, "bin", "curl"), curl)
		writeLines(t, filepath.Join(dir, "ticks-a"), tc.a...)
		writeLines(t, filepath.Join(dir, "ticks-b"), tc.b...)
		stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_settle a b; echo SETTLED`, "M6_SETTLE_ROUNDS=3", "M6_TICK_EVERY=0")
		ca, _ := os.ReadFile(filepath.Join(dir, "calls-a"))
		cb, _ := os.ReadFile(filepath.Join(dir, "calls-b"))
		if got := strings.TrimSpace(string(ca)) + " " + strings.TrimSpace(string(cb)); got != tc.calls {
			t.Errorf("%s: ticks a b %q, want %q", tc.name, got, tc.calls)
		}
		if q, _ := os.ReadFile(filepath.Join(dir, "queue.calls")); len(q) == 0 {
			t.Errorf("%s: the queue was never waited for", tc.name)
		}
		if tc.fails == "" {
			if code != 0 || stdout != "SETTLED\n" {
				t.Errorf("%s: exit %d, stdout %q; stderr:\n%s", tc.name, code, stdout, stderr)
			}
		} else if code == 0 || strings.Contains(stdout, "SETTLED") || !strings.Contains(stderr, "FAIL: "+tc.fails) {
			t.Errorf("%s: exit %d, stdout %q, want FAIL: %s; stderr:\n%s", tc.name, code, stdout, tc.fails, stderr)
		}
	}
}

// The tombstones a purge recorded are read from the server's control bank
// (<bank>-control) with B's key: the document is listed, or the read
// fails, never passes on an absent record, another document's tombstone,
// metadata that does not parse or an answer but 200.
func TestM6TombstonedNeedsTheControlBanksRecord(t *testing.T) {
	const listed = `{"id":"backsight:tombstones","document_metadata":{"tombstones":"[{\"bank\":\"bk\",\"document\":\"conversation:pg1\"}]"}}`
	for _, tc := range []struct {
		name, code, body, fail string
	}{
		{"listed", "200", listed, ""},
		{"another document's", "200", `{"document_metadata":{"tombstones":"[{\"document\":\"conversation:pg2\"}]"}}`, "the control bank's tombstones do not name conversation:pg1"},
		{"no tombstones field", "200", `{"document_metadata":{}}`, "the control bank's tombstones do not name conversation:pg1"},
		{"not a list", "200", `{"document_metadata":{"tombstones":"{oops"}}`, "the control bank's tombstones do not name conversation:pg1"},
		{"no document", "404", `{}`, "the control bank holds no tombstones document"},
		{"401", "401", listed, "reading document backsight:tombstones: HTTP 401"},
		{"refused", "refused", listed, "reading document backsight:tombstones: HTTP 000"},
	} {
		dir := t.TempDir()
		m6ServerFakes(t, dir)
		writeLines(t, filepath.Join(dir, "code"), tc.code)
		writeLines(t, filepath.Join(dir, "body"), tc.body)
		stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_tombstoned conversation:pg1; echo OK`)
		if tc.fail == "" {
			if code != 0 || stdout != "OK\n" {
				t.Errorf("%s: exit %d, stdout %q; stderr:\n%s", tc.name, code, stdout, stderr)
			}
		} else if code == 0 || strings.Contains(stdout, "OK") || !strings.Contains(stderr, "FAIL: "+tc.fail) {
			t.Errorf("%s: exit %d, stdout %q, want FAIL: %s; stderr:\n%s", tc.name, code, stdout, tc.fail, stderr)
		}
		calls, _ := os.ReadFile(filepath.Join(dir, "curl.calls"))
		if !strings.Contains(string(calls), "http://10.0.0.9:8888/v1/default/banks/bk-control/documents/backsight%3Atombstones") {
			t.Errorf("%s: curl was called with %q, want the control bank's tombstones on B's server", tc.name, calls)
		}
	}
}

// The restore epoch a PC reconciled with is its state.json's, 0 when it
// never reconciled; an unreadable state fails rather than reading as 0.
func TestM6EpochIsThePCsOwn(t *testing.T) {
	dir := t.TempDir()
	m6ServerFakes(t, dir)
	st := filepath.Join(dir, "a", "state")
	if err := os.MkdirAll(st, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ state, want string }{
		{`{"restore_epoch":{"bank":"bk","epoch":3}}`, "3"},
		{`{"retain_since":"2026-10-29T10:00:00Z"}`, "0"},
	} {
		writeLines(t, filepath.Join(st, "state.json"), tc.state)
		if stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `e=$(m6_epoch a); echo "E[$e]"`); code != 0 || stdout != "E["+tc.want+"]\n" {
			t.Errorf("%s: exit %d, stdout %q, want E[%s]; stderr:\n%s", tc.state, code, stdout, tc.want, stderr)
		}
	}
	if err := os.Remove(filepath.Join(st, "state.json")); err != nil {
		t.Fatal(err)
	}
	if stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `e=$(m6_epoch a); echo "E[$e]"`); code == 0 || strings.Contains(stdout, "E[") || !strings.Contains(stderr, "FAIL: cannot read a's state.json") {
		t.Errorf("no state.json: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
}
