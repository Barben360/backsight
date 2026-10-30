package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The CI proof of the native runtime and the switch (decision 0078,
// scripts/native-chain.sh), on every pull request: a job of its own with
// no if: (nor continue-on-error), one step with no if: running the script
// in its run: script, and the runner user's linger enabled by an earlier
// step (the units must outlive the job's session, and the chain's
// pre-flight refuses without it).
func TestNativeChainRunsOnEveryPullRequest(t *testing.T) {
	var j *job
	for _, w := range workflows(t) {
		if w.name != "ci.yml" {
			continue
		}
		for i := range w.jobs {
			if w.jobs[i].name == "native-chain" {
				j = &w.jobs[i]
			}
		}
	}
	if j == nil {
		t.Fatal("ci.yml has no native-chain job")
	}
	header, _, _ := strings.Cut(j.raw, "\n    steps:")
	if !regexp.MustCompile(`(?m)^    runs-on: ubuntu-latest\s*$`).MatchString(header) {
		t.Errorf("the native-chain job does not run on ubuntu-latest:\n%s", header)
	}
	for _, key := range []string{"if", "continue-on-error"} {
		if regexp.MustCompile(`(?m)^    ` + key + `:`).MatchString(header) {
			t.Errorf("the native-chain job has an %s: (it may not run, or not fail):\n%s", key, header)
		}
	}
	runs, script, linger := 0, -1, -1
	isScript := regexp.MustCompile(`(^|[\s/])scripts/native-chain\.sh(\s|$)`)
	isLinger := regexp.MustCompile(`(^|\s)sudo loginctl enable-linger "\$USER"(\s|$)`)
	for i, step := range j.steps {
		lines := runScript(step)
		if slices.ContainsFunc(lines, isLinger.MatchString) && linger < 0 {
			linger = i
		}
		if !slices.ContainsFunc(lines, isScript.MatchString) {
			continue
		}
		if regexp.MustCompile(`(?m)^        if:`).MatchString(step) {
			t.Errorf("the step running scripts/native-chain.sh has an if::\n%s", step)
		}
		runs++
		script = i
	}
	if runs != 1 {
		t.Fatalf("%d steps of the native-chain job run scripts/native-chain.sh in their run: script, want 1", runs)
	}
	if linger < 0 || linger > script {
		t.Errorf("no step before the script's runs sudo loginctl enable-linger \"$USER\" (linger step %d, script step %d)", linger, script)
	}
	// It waits for the user manager the linger starts.
	if linger >= 0 && !slices.ContainsFunc(runScript(j.steps[linger]), func(l string) bool { return strings.Contains(l, "systemctl --user show-environment") }) {
		t.Errorf("the linger step does not wait until systemctl --user show-environment answers:\n%s", j.steps[linger])
	}
}

// fakeTools is a PATH directory whose systemctl, docker, loginctl,
// journalctl, make and go record their argv in calls and succeed;
// systemctl --user show-environment names xdg as the manager's
// XDG_CONFIG_HOME, so a unit directory the script derives is a temporary
// one.
func fakeTools(t *testing.T, xdg string) (dir, calls string) {
	t.Helper()
	dir = t.TempDir()
	calls = filepath.Join(dir, "calls")
	for _, name := range []string{"systemctl", "docker", "loginctl", "journalctl", "make", "go"} {
		body := "#!/bin/sh\necho \"" + name + " $*\" >> " + calls + "\n"
		if name == "systemctl" {
			body += "case \"$*\" in *show-environment*) echo XDG_CONFIG_HOME=" + xdg + " ;; esac\n"
		}
		writeExec(t, filepath.Join(dir, name), body)
	}
	return dir, calls
}

// chainEnv is the script's environment in these tests: the fakes first
// on PATH, and HOME, XDG_CONFIG_HOME and XDG_RUNTIME_DIR temporary, so
// that even a regressed refusal, and its trap, reach no real unit
// directory.
func chainEnv(t *testing.T, bin, xdg string, extra ...string) []string {
	t.Helper()
	return append(append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+xdg,
		"XDG_RUNTIME_DIR="+t.TempDir(), "WORK="+t.TempDir(), "CI="), extra...)
}

// realUnitDir is the developer's own systemd user unit directory, which
// no test run may name.
func realUnitDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".config", "systemd", "user")
}

// The live install's units are backsight-*.service and its Compose
// project is backsight: the chain refuses that project and any project
// its trap's $PROJECT-* would match them with, before it runs anything.
func TestNativeChainNeverTouchesTheLiveInstall(t *testing.T) {
	real := realUnitDir(t)
	for _, project := range []string{"backsight", "backsight-dev", "backsightx"} {
		t.Run(project, func(t *testing.T) {
			xdg := t.TempDir()
			bin, calls := fakeTools(t, xdg)
			cmd := exec.Command("bash", mustAbs(t, "native-chain.sh"))
			cmd.Env = chainEnv(t, bin, xdg, "PROJECT="+project, "NATIVE_DRY=")
			out, err := cmd.CombinedOutput()
			if b, _ := os.ReadFile(calls); strings.Contains(string(b)+string(out), real) {
				t.Errorf("PROJECT=%s named the real unit directory %s:\n%s%s", project, real, b, out)
			}
			if err == nil {
				t.Fatalf("PROJECT=%s accepted:\n%s", project, out)
			}
			if !strings.Contains(string(out), "never the live install") {
				t.Errorf("PROJECT=%s refused without saying why:\n%s", project, out)
			}
			if b, _ := os.ReadFile(calls); len(b) > 0 {
				t.Errorf("PROJECT=%s ran commands before refusing:\n%s", project, b)
			}
		})
	}
	// Positive control: the fakes do record, and a bs- project gets past
	// the refusal (NATIVE_DRY stops it before any command).
	xdg := t.TempDir()
	bin, calls := fakeTools(t, xdg)
	out, err := exec.Command(filepath.Join(bin, "systemctl"), "--user", "show-environment").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "XDG_CONFIG_HOME="+xdg) {
		t.Fatalf("fake systemctl: %v %s", err, out)
	}
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "systemctl --user show-environment") {
		t.Fatalf("the fake systemctl records nothing (%q): the refusal test would prove nothing", b)
	}
	cmd := exec.Command("bash", mustAbs(t, "native-chain.sh"))
	cmd.Env = chainEnv(t, bin, xdg, "PROJECT=bs-native-test", "NATIVE_DRY=1")
	if out, err := cmd.CombinedOutput(); err != nil || strings.Contains(string(out), "never the live install") || strings.Contains(string(out), real) {
		t.Errorf("PROJECT=bs-native-test refused, or named %s: %v\n%s", real, err, out)
	}
}

func TestNativeChainListsItsLegs(t *testing.T) {
	xdg := t.TempDir()
	bin, _ := fakeTools(t, xdg)
	cmd := exec.Command("bash", mustAbs(t, "native-chain.sh"))
	cmd.Env = chainEnv(t, bin, xdg, "NATIVE_DRY=1", "PROJECT=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("NATIVE_DRY=1: %v", err)
	}
	want := "fresh store to-docker to-native refusals upgrade-rollback switch-rollback lock"
	if got := strings.Join(strings.Fields(string(out)), " "); got != want {
		t.Errorf("legs listed: %q, want %q", got, want)
	}
}

// The trap stops and removes the units this run's install makes, named
// exactly: another project sharing the prefix (bs-x-db's units are
// bs-x-db-db.service, …) is never matched.
func TestNativeChainTrapMatchesItsOwnUnitsOnly(t *testing.T) {
	src := `. "$1"; project=bs-x; shift; for u; do if nc_own_unit "$u"; then echo "own $u"; else echo "other $u"; fi; done`
	own := []string{"bs-x-db.service", "bs-x-server.service", "bs-x-db-init.service", "bs-x-pg-unpack.service", "bs-x-verify-0123456789ab.service",
		"bs-x-tick.service", "bs-x-tick.timer", "bs-x-maintain.service", "bs-x-maintain.timer"}
	other := []string{"bs-x-db-db.service", "bs-x-db-server.service", "bs-x-verify-db.service", "bs-x-verify-0a-db.service",
		"bs-x-other.service", "bs-xy-db.service", "backsight-db.service", "backsight-tick.timer", "bs-x-db.service.d", "bs-x-pg-unpack-db.service"}
	out, err := exec.Command("bash", append([]string{"-c", src, "x", mustAbs(t, "native-chain.sh")}, append(own, other...)...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, u := range own {
		if !strings.Contains(string(out), "own "+u+"\n") {
			t.Errorf("the trap would leave %s running", u)
		}
	}
	for _, u := range other {
		if !strings.Contains(string(out), "other "+u+"\n") {
			t.Errorf("the trap would stop %s, not this run's", u)
		}
	}
}

// A run's exit status survives its cleanup, even when every cleanup step
// fails (here systemctl, docker and journalctl all exit 1).
func TestNativeChainTrapKeepsTheRunsStatus(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"systemctl", "docker", "journalctl", "pgrep"} {
		writeExec(t, filepath.Join(bin, name), "#!/bin/sh\necho \"$0 $*\" >&2\nexit 1\n")
	}
	units := t.TempDir()
	work := t.TempDir()
	for _, f := range []string{"bs-x-db.service", "bs-x-server.service", "bs-other-db.service"} {
		if err := os.WriteFile(filepath.Join(units, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := `set -euo pipefail; . "$1"; project=bs-x; work=$2; unitdir=$3; BACKSIGHT_HOME=$2/b; trap nc_cleanup EXIT; exit 7`
	cmd := exec.Command("bash", "-c", src, "x", mustAbs(t, "native-chain.sh"), work, units)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "KEEP=")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("exit %v, want the run's 7:\n%s", err, out)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("the run's directory is still there: %v", err)
	}
	for f, want := range map[string]bool{"bs-x-db.service": false, "bs-x-server.service": false, "bs-other-db.service": true} {
		if _, err := os.Stat(filepath.Join(units, f)); (err == nil) != want {
			t.Errorf("%s present %v after the trap, want %v", f, err == nil, want)
		}
	}
}

// The same when removing the files fails too (a fake rm exiting 1): the
// run still exits with its own status.
func TestNativeChainTrapKeepsTheRunsStatusWhenRemovingFails(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"systemctl", "docker", "journalctl", "pgrep", "rm", "chmod"} {
		writeExec(t, filepath.Join(bin, name), "#!/bin/sh\nexit 1\n")
	}
	src := `set -euo pipefail; . "$1"; project=bs-x; work=$2; unitdir=$3; trap nc_cleanup EXIT; exit 7`
	cmd := exec.Command("bash", "-c", src, "x", mustAbs(t, "native-chain.sh"), t.TempDir(), t.TempDir())
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "KEEP=")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("exit %v, want the run's 7:\n%s", err, out)
	}
}

// The round trip's way back starts from an install that never had native
// (the v0.1.16-m7 bug: pg0's Postgres missing at the switch's verify):
// to-docker ends with server runtime clean, which removes server/native
// whole, and to-native refuses to start while it is there.
func TestNativeChainToNativeStartsFromNoNative(t *testing.T) {
	dir := t.TempDir()
	native, calls := filepath.Join(dir, "native"), filepath.Join(dir, "calls")
	if err := os.MkdirAll(filepath.Join(native, "home", ".pg0", "installation"), 0o700); err != nil {
		t.Fatal(err)
	}
	// backsight: records its argv; clean removes server/native.
	writeExec(t, filepath.Join(dir, "backsight"), `#!/bin/sh
echo "$*" >> `+calls+`
case "$*" in "server runtime clean"*) rm -rf `+native+`; echo '{"ok":true,"data":{"runtime":"native","deleted":true}}' ;; esac
`)
	if err := os.WriteFile(filepath.Join(dir, "to-docker.json"), []byte(`{"data":{"probes":{"overlap_at_10":1,"queries":20,"own_at_rank_1":20}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `set -euo pipefail; . "$1"; bin=$2/backsight work=$2 native=$3 project=bs-x
nc_switch() { echo "switch $1" >> "$work/calls"; }
hfc_seed() { :; }; hfc_save() { :; }; nc_units() { [ "$2" = no ]; }; nc_docker_running() { echo c1; }
nc_leg_to_docker
[ ! -e "$native" ] || { echo "native still there" >&2; exit 4; }
mkdir -p "$native"
if (nc_leg_to_native) 2> "$work/to-native.err"; then echo "to-native ran with native there" >&2; exit 5; fi
grep -q "never had native" "$work/to-native.err" || { cat "$work/to-native.err" >&2; exit 6; }`
	out, err := exec.Command("bash", "-c", src, "x", mustAbs(t, "native-chain.sh"), dir, native).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if want := []string{"switch docker", "server runtime clean --answer consent=yes --non-interactive --json"}; !slices.Equal(lines, want) {
		t.Errorf("to-docker ran %q, want %q", lines, want)
	}
	if !strings.Contains(string(out), "native cleaned") {
		t.Errorf("to-docker's line does not say native was cleaned:\n%s", out)
	}
}
