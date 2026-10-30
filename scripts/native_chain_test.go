package scripts

import (
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
// journalctl, make and go record their argv in calls and succeed.
func fakeTools(t *testing.T) (dir, calls string) {
	t.Helper()
	dir = t.TempDir()
	calls = filepath.Join(dir, "calls")
	for _, name := range []string{"systemctl", "docker", "loginctl", "journalctl", "make", "go"} {
		writeExec(t, filepath.Join(dir, name), "#!/bin/sh\necho \""+name+" $*\" >> "+calls+"\n")
	}
	return dir, calls
}

// The live install's units are backsight-*.service and its Compose
// project is backsight: the chain refuses that project and any project
// its trap's $PROJECT-* would match them with, before it runs anything.
func TestNativeChainNeverTouchesTheLiveInstall(t *testing.T) {
	for _, project := range []string{"backsight", "backsight-dev", "backsightx"} {
		t.Run(project, func(t *testing.T) {
			bin, calls := fakeTools(t)
			cmd := exec.Command("bash", mustAbs(t, "native-chain.sh"))
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PROJECT="+project, "WORK="+t.TempDir(), "NATIVE_DRY=", "CI=")
			out, err := cmd.CombinedOutput()
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
	bin, calls := fakeTools(t)
	if out, err := exec.Command(filepath.Join(bin, "systemctl"), "--user", "probe").CombinedOutput(); err != nil {
		t.Fatalf("fake systemctl: %v %s", err, out)
	}
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "systemctl --user probe") {
		t.Fatalf("the fake systemctl records nothing (%q): the refusal test would prove nothing", b)
	}
	cmd := exec.Command("bash", mustAbs(t, "native-chain.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PROJECT=bs-native-test", "NATIVE_DRY=1", "CI=")
	if out, err := cmd.CombinedOutput(); err != nil || strings.Contains(string(out), "never the live install") {
		t.Errorf("PROJECT=bs-native-test refused: %v\n%s", err, out)
	}
}

func TestNativeChainListsItsLegs(t *testing.T) {
	cmd := exec.Command("bash", mustAbs(t, "native-chain.sh"))
	cmd.Env = append(os.Environ(), "NATIVE_DRY=1", "CI=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("NATIVE_DRY=1: %v", err)
	}
	want := "fresh store to-docker to-native refusals upgrade-rollback switch-rollback lock"
	if got := strings.Join(strings.Fields(string(out)), " "); got != want {
		t.Errorf("legs listed: %q, want %q", got, want)
	}
}
