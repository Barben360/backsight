package scripts

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// proofLib runs a script after sourcing proof-lib.sh, under bash with the
// given flags; it returns stderr and the exit code.
func proofLib(t *testing.T, flags, script string) (stderr string, code int) {
	t.Helper()
	root, _ := filepath.Abs("..")
	args := []string{"-c", `. "$1/scripts/proof-lib.sh"; ` + script, "pl", root}
	if flags != "" {
		args = append([]string{flags}, args...)
	}
	cmd := exec.Command("bash", args...)
	cmd.Dir = t.TempDir()
	var e bytes.Buffer
	cmd.Stderr = &e
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return e.String(), ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return e.String(), 0
}

func TestProofLibSecretIsNeverTracedAndTraceResumes(t *testing.T) {
	// The secret comes out of a function, not the argv (a command line is
	// traced before it runs), and is checked untraced for the same reason.
	const script = `emit() { printf s3cr3t; }; check() { [ "$k" = s3cr3t ]; }; pl_secret k emit; echo after; pl_untraced check`
	stderr, code := proofLib(t, "-x", script)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "s3cr3t") {
		t.Errorf("the secret was traced:\n%s", stderr)
	}
	if !strings.Contains(stderr, "+ echo after") {
		t.Errorf("trace did not resume after pl_secret:\n%s", stderr)
	}
	stderr, code = proofLib(t, "", script)
	if code != 0 || strings.Contains(stderr, "+ echo after") {
		t.Errorf("untraced caller: exit %d, stderr %q (want 0, no trace turned on)", code, stderr)
	}
}

func TestProofLibSecretKeepsTheStatus(t *testing.T) {
	stderr, code := proofLib(t, "-x", `set -e; pl_secret k false || echo "rc=$?"; echo after`)
	if code != 0 || !strings.Contains(stderr, "+ echo rc=1") || !strings.Contains(stderr, "+ echo after") {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestProofLibUntracedKeepsTheStatus(t *testing.T) {
	stderr, code := proofLib(t, "-x", `pl_untraced false; rc=$?; echo "rc=$rc"; exit "$rc"`)
	if code != 1 {
		t.Errorf("exit %d, want 1 (pl_untraced false); stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "+ echo rc=1") {
		t.Errorf("trace did not resume after pl_untraced:\n%s", stderr)
	}
}

func TestProofLibUntracedHidesWhatItRuns(t *testing.T) {
	stderr, code := proofLib(t, "-x", `inner() { echo hidden-step >/dev/null; }; pl_untraced inner; echo after`)
	if code != 0 || strings.Contains(stderr, "hidden-step") || !strings.Contains(stderr, "+ echo after") {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestProofLibHashNamesTheMissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "present"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `fail() { echo "FAIL: $*" >&2; }; cd ` + dir + `; set -e; pl_hash present missing`
	stderr, code := proofLib(t, "", script)
	if code != 1 {
		t.Errorf("exit %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "FAIL: cannot hash missing") || strings.Contains(stderr, "sha256sum:") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

func TestProofLibHashPrintsEachFile(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("bash", "-c", `. "$1/scripts/proof-lib.sh"; fail() { echo "FAIL: $*" >&2; exit 9; }; cd "$2"; echo a > a; echo b > b; pl_hash a b`, "pl", mustAbs(t, ".."), dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(out), "\n"); got != 2 || !strings.Contains(string(out), "  a\n") || !strings.Contains(string(out), "  b\n") {
		t.Errorf("output:\n%s", out)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestProofLibUntracedLeavesTraceOffForAnUntracedCaller(t *testing.T) {
	stderr, code := proofLib(t, "", `pl_untraced true; case $- in *x*) echo "x is on" >&2; exit 3 ;; esac`)
	if code != 0 {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestProofLibSecretLeavesTraceOffForAnUntracedCaller(t *testing.T) {
	stderr, code := proofLib(t, "", `emit() { printf s; }; pl_secret k emit; case $- in *x*) exit 3 ;; esac`)
	if code != 0 {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestProofLibNonemptyAndDifferTakeNamesAndNeverTraceValues(t *testing.T) {
	const pre = `emit1() { printf s3cr3t-one; }; emit2() { printf s3cr3t-two; }; pl_secret a emit1; pl_secret b emit2; `
	stderr, code := proofLib(t, "-x", pre+`pl_nonempty a && pl_differ a b && echo ok`)
	if code != 0 || !strings.Contains(stderr, "+ echo ok") {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "s3cr3t") {
		t.Errorf("a value was traced:\n%s", stderr)
	}
	// The refusals: empty, unset, equal.
	for name, script := range map[string]string{
		"empty": `e=; pl_nonempty e`,
		"unset": `pl_nonempty nope`,
		"equal": pre + `pl_secret c emit1; pl_differ a c`,
	} {
		if stderr, code := proofLib(t, "-x", script); code != 1 || strings.Contains(stderr, "s3cr3t") {
			t.Errorf("%s: exit %d (want 1), stderr:\n%s", name, code, stderr)
		}
	}
	// Trace resumes afterwards.
	stderr, _ = proofLib(t, "-x", pre+`pl_nonempty a; pl_differ a b; echo after`)
	if !strings.Contains(stderr, "+ echo after") {
		t.Errorf("trace did not resume:\n%s", stderr)
	}
}

func TestRestoreChainAuthConfigIsNeverTraced(t *testing.T) {
	root, _ := filepath.Abs("..")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "secrets.toml"), []byte("api_key = \"s3cr3t-key\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := t.TempDir()
	// A curl that reads its -K config like the real one and shows the header on stdout.
	if err := os.WriteFile(filepath.Join(fake, "curl"), []byte("#!/bin/sh\ncat \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-x", "-c", `. "$1/scripts/restore-chain.sh"; curl -K <(rc_auth); echo after`, "rc", root)
	cmd.Env = append(os.Environ(), "BACKSIGHT_HOME="+home, "PATH="+fake+":"+os.Getenv("PATH"))
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v\n%s", err, e.String())
	}
	if !strings.Contains(o.String(), "Authorization: Bearer s3cr3t-key") {
		t.Errorf("curl did not get the header:\n%s", o.String())
	}
	if strings.Contains(e.String(), "s3cr3t-key") || !strings.Contains(e.String(), "+ echo after") {
		t.Errorf("stderr:\n%s", e.String())
	}
}

// fresh-install.sh is not sourceable (it runs on load), so this reads it:
// no curl gets the API key in its argv (ps-visible): no Authorization
// header passed with -H, and each authenticated call (llm_calls,
// banks_status_raw, the tombstones and stats reads) takes its key from a
// curl config in a process substitution.
func TestFreshInstallKeepsTheKeyOffCurlsArgv(t *testing.T) {
	raw, err := os.ReadFile(mustAbs(t, "fresh-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	k := 0
	for i, l := range strings.Split(string(raw), "\n") {
		if strings.Contains(l, "Authorization") && !strings.Contains(l, "printf 'header = ") {
			t.Errorf("fresh-install.sh:%d puts the key outside a curl config: %s", i+1, strings.TrimSpace(l))
		}
		if strings.Contains(l, "curl ") && (strings.Contains(l, "-K <(rc_auth)") || strings.Contains(l, `-K <(auth_of "$1")`)) {
			k++
		}
	}
	if k != 4 {
		t.Errorf("%d curl calls take the key from -K, want 4 (llm_calls, the tombstones read, banks_status_raw, the stats read)", k)
	}
}

// The restore chain compares what was sent of a session (its cursor), not
// the pending first-segment checks a tick drops on a restore epoch change
// without sending anything (fresh-install failed on that since e32b2fc).
func TestRestoreChainCursorIgnoresPendingChecks(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "state", "cursors")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"marked":  `{"offset":120,"last_uuid":"r3","replaces":{"":{"document":"conversation:s1","epoch":0}}}`,
		"dropped": `{"last_uuid":"r3","offset":120}`,
		"moved":   `{"last_uuid":"r4","offset":180}`,
	} {
		writeLines(t, filepath.Join(dir, name+".json"), body)
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . "$1"; rc_cursor marked; rc_cursor dropped; rc_cursor moved`, "x", mustAbs(t, "restore-chain.sh"))
	cmd.Env = append(os.Environ(), "BACKSIGHT_HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	l := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(l) != 3 || l[0] != l[1] || l[1] == l[2] {
		t.Errorf("cursors %q: want marked = dropped (the check is not a send), moved apart", l)
	}
}
