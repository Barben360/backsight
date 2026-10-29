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

// RF 2: a tick skipped because the scheduler's runs is retried, so a
// skipped tick never reads as "sent nothing"; one that stays skipped fails.
func TestM6TickRetriesASkippedTick(t *testing.T) {
	const fake = `#!/bin/sh
t=$(dirname "$(dirname "$0")")
n=$(($(cat "$t/calls" 2>/dev/null || echo 0) + 1))
echo "$n" > "$t/calls"
if [ "$n" -le "$(cat "$t/skips")" ]; then
  echo '{"ok":true,"data":{"status":"skipped","reason":"another tick is running"}}'
else
  echo '{"ok":true,"data":{"status":"done","sessions":{"sent":1}}}'
fi
`
	dir := t.TempDir()
	m6Lib(t, dir, m6TestDriver, "true")
	writeExec(t, filepath.Join(dir, "bin", "backsight"), fake)
	writeLines(t, filepath.Join(dir, "skips"), "2")
	stdout, stderr, code := m6Lib(t, dir, m6TestDriver, `m6_tick a`, "M6_TICK_EVERY=0")
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if code != 0 || strings.TrimSpace(stdout) != `{"ok":true,"data":{"status":"done","sessions":{"sent":1}}}` || strings.TrimSpace(string(calls)) != "3" {
		t.Errorf("exit %d, %s call(s), stdout %q (want the done tick alone, after 3 calls); stderr:\n%s", code, strings.TrimSpace(string(calls)), stdout, stderr)
	}

	_ = os.Remove(filepath.Join(dir, "calls"))
	writeLines(t, filepath.Join(dir, "skips"), "1000000")
	stdout, stderr, code = m6Lib(t, dir, m6TestDriver, `m6_tick a`, "M6_TICK_EVERY=0.2", "M6_TICK_WAIT=1")
	calls, _ = os.ReadFile(filepath.Join(dir, "calls"))
	if code == 0 || !strings.Contains(stderr, "FAIL: jobs tick on a: another tick was still running after 1 s") || stdout != "" {
		t.Errorf("always skipped: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
	if n := strings.TrimSpace(string(calls)); n == "1" || n == "" {
		t.Errorf("always skipped: %q call(s), want it tried again before failing", n)
	}
}

// Each result is one line and one JSONL record with its fields; no key or
// token passes through its values or detail.
func TestM6ResultWritesALineAndARecord(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, code := m6Lib(t, dir, m6TestDriver,
		`m6_result two-clients PASS "joined with bst1.eyJhIjoxfQ.sig" '{"records":6,"api_key":"k-S3CR3T","nested":{"token":"bst1.x","note":"got bst1.abc_DEF-1","llm_key":"m"}}'`)
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
	if string(vals) != `{"nested":{"note":"got [token]"},"records":6}` {
		t.Errorf("values = %s, want the counts kept and every key or token gone", vals)
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
// driver logs each call with its machine (and what it is given on stdin);
// a B call naming --ssh or server install is a violation.
func TestM6ProofsNeverRunSSHInstallOnB(t *testing.T) {
	const driver = `
log() { printf '%s\n' "$*" >> "$T/calls"; }
m6_home() { echo "/home/$1"; }
m6_on() {
  local w=$1; shift
  log "on $w $*"
  case "$w $*" in b*--ssh* | b*"server install"* | srv*"server install"*) echo VIOLATION >> "$T/violations" ;; esac
  case "$*" in
    "paths --json") echo '{"ok":true,"data":{"config_file":"/c/config.toml","secrets_file":"/c/secrets.toml","state":"/s","server_role":"server"}}' ;;
    "server token"*) echo '{"ok":true,"data":{}}' ;;
    *) echo '{"ok":false,"error":{"message":"fake"}}'; return 1 ;;
  esac
}
m6_sh() {
  local in
  in=$(cat)
  log "sh $1 $2 <<$(printf %s "$in" | tr '\n' ' ')"
  case "$1 $2 $in" in b*--ssh* | b*"server install"* | srv*"server install"*) echo VIOLATION >> "$T/violations" ;; esac
  case $2 in
    *pgrep*) return 1 ;;
    *code*) echo 1 ;;
    *sha256sum*) if [ "$1" = srv ]; then echo abc; else echo absent; fi ;;
    *"stat -c %a"*) echo 600 ;;
  esac
}
m6_put() { m6_sh "$1" "put $2"; }
m6_get() { m6_sh "$1" "get $2" < /dev/null; }
m6_link() { log "link $*"; }
m6_host_key() { log "host_key $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
M6_SERVER_IP=10.0.0.9 M6_DROP_S=0
`
	checks := []string{"ssh_unknown_host_key", "ssh_dropped_mid_install", "ssh_install", "ssh_changed_host_key", "two_clients"}
	for _, c := range checks {
		dir := t.TempDir()
		m6Lib(t, dir, driver, "true")
		writeLines(t, filepath.Join(dir, "work", "resume"), "backsight server install --ssh bs@10.0.0.9 --source /home/a/release --non-interactive")
		_, stderr, _ := m6Lib(t, dir, driver, `m6_check `+c+` m6_check_`+c+` < /dev/null`)
		raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
		calls := string(raw)
		if v, _ := os.ReadFile(filepath.Join(dir, "violations")); len(v) > 0 {
			t.Errorf("%s: a server install --ssh went through B or the server:\n%s", c, calls)
		}
		var onA bool
		for _, l := range strings.Split(calls, "\n") {
			if (strings.HasPrefix(l, "on a ") || strings.HasPrefix(l, "sh a ")) && strings.Contains(strings.ReplaceAll(l, "'", ""), "server install --ssh") {
				onA = true
			}
		}
		if c == "two_clients" {
			if !strings.Contains(calls, "on b connect --token-file") {
				t.Errorf("%s: B never ran connect (the check would prove nothing):\n%s\nstderr:\n%s", c, calls, stderr)
			}
			continue
		}
		if !onA {
			t.Errorf("%s: no server install --ssh went through A (the check would prove nothing):\n%s\nstderr:\n%s", c, calls, stderr)
		}
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
	_, stderr, code := vmBench(t, fakes, t.TempDir(), bench, `. "`+lib+`"; m6_on b server install --ssh bs@10.0.0.9 --json`,
		"CLAUDE_CONFIG_DIR=/home/u/.claude", "XDG_CONFIG_HOME=/home/u/.config", "SSH_AUTH_SOCK=/run/agent")
	if code == 0 || !strings.Contains(stderr, "client B never runs server install --ssh") {
		t.Errorf("m6_on b … --ssh: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(bench, "backsight.calls")); err == nil {
		t.Error("B's backsight ran a server install --ssh")
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
