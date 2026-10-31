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

	"github.com/Barben360/backsight/internal/testtool"
)

// The CI chain re-runs the VM bench's dedup and key-rotation proofs on
// every pull request, on containers (scripts/m6-chain.sh). Scoped to the
// m6-chain job: the workflow runs on every pull_request (no paths or
// branches filter), the job has its own runs-on and no if: (nor
// continue-on-error), and one step with no if: runs scripts/m6-chain.sh in
// its run: script, not in a comment.
func TestM6ChainRunsOnEveryPullRequest(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	on := ""
	for _, b := range blocks(string(raw), regexp.MustCompile(`(?m)^[a-z]`)) {
		if strings.HasPrefix(b, "on:") {
			on = b
		}
	}
	if !regexp.MustCompile(`(?m)^  pull_request:\s*$`).MatchString(on) {
		t.Errorf("ci.yml's on: has no pull_request trigger:\n%s", on)
	}
	if regexp.MustCompile(`(?m)^  pull_request:\s*\n    `).MatchString(on) {
		t.Errorf("ci.yml's pull_request trigger is filtered, not every pull request:\n%s", on)
	}
	var j *job
	for _, w := range workflows(t) {
		if w.name != "ci.yml" {
			continue
		}
		for i := range w.jobs {
			if w.jobs[i].name == "m6-chain" {
				j = &w.jobs[i]
			}
		}
	}
	if j == nil {
		t.Fatal("ci.yml has no m6-chain job")
	}
	header, _, _ := strings.Cut(j.raw, "\n    steps:")
	if !regexp.MustCompile(`(?m)^    runs-on: \S`).MatchString(header) {
		t.Errorf("the m6-chain job has no runs-on of its own:\n%s", header)
	}
	for _, key := range []string{"if", "continue-on-error"} {
		if regexp.MustCompile(`(?m)^    ` + key + `:`).MatchString(header) {
			t.Errorf("the m6-chain job has an %s: (it may not run, or not fail):\n%s", key, header)
		}
	}
	runs := 0
	for _, step := range j.steps {
		if !slices.ContainsFunc(runScript(step), func(l string) bool { return regexp.MustCompile(`(^|[\s/])scripts/m6-chain\.sh(\s|$)`).MatchString(l) }) {
			continue
		}
		if regexp.MustCompile(`(?m)^        if:`).MatchString(step) {
			t.Errorf("the step running scripts/m6-chain.sh has an if::\n%s", step)
		}
		runs++
	}
	if runs != 1 {
		t.Errorf("%d steps of the m6-chain job run scripts/m6-chain.sh in their run: script, want 1", runs)
	}
}

// runScript: the lines of a step's run: script (inline or a block),
// comment lines left out; none for a step without run:.
func runScript(step string) []string {
	lines := strings.Split(step, "\n")
	re := regexp.MustCompile(`^(\s*)(- )?run:\s*(.*)$`)
	for i, l := range lines {
		m := re.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		indent := len(m[1]) + len(m[2])
		var out []string
		if v := strings.TrimSpace(m[3]); v != "|" && v != ">" && v != "|-" && v != ">-" {
			out = append(out, v)
		} else {
			for _, b := range lines[i+1:] {
				if strings.TrimSpace(b) != "" && len(b)-len(strings.TrimLeft(b, " ")) <= indent {
					break
				}
				out = append(out, b)
			}
		}
		return slices.DeleteFunc(out, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") })
	}
	return nil
}

func TestM6ChainRunsTheDedupAndRotationProofs(t *testing.T) {
	testtool.SkipWindows(t, "POSIX script, Linux only")
	cmd := exec.Command("bash", mustAbs(t, "m6-chain.sh"))
	cmd.Env = append(os.Environ(), "M6_DRY=1", "BACKSIGHT_CI_SLOT=1", "CI=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("M6_DRY=1: %v", err)
	}
	want := "two-clients dedup-copied dedup-concurrent dedup-split key-rotation"
	if got := strings.Join(strings.Fields(string(out)), " "); got != want {
		t.Errorf("checks listed: %q, want %q", got, want)
	}
}

func TestM6ChainRefusesTheMachinesOwnProject(t *testing.T) {
	testtool.SkipWindows(t, "POSIX script, Linux only")
	cmd := exec.Command("bash", mustAbs(t, "m6-chain.sh"))
	cmd.Env = append(os.Environ(), "M6_DRY=1", "BACKSIGHT_CI_SLOT=1", "PROJECT=backsight")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "PROJECT=backsight") {
		t.Errorf("PROJECT=backsight accepted: %v, %s", err, out)
	}
}

// Client homes only ever run client commands: a server command, or an ssh
// install, belongs to the srv home.
func TestM6ChainClientsNeverRunAServerCommand(t *testing.T) {
	testtool.SkipWindows(t, "POSIX script, Linux only")
	for _, c := range []struct{ who, args string }{
		{"b", "server token --out /x"}, {"a", "--json server install"}, {"b", "connect --ssh=bs@h"}, {"a", "connect --ssh bs@h"},
	} {
		cmd := exec.Command("bash", "-c", `set -euo pipefail; . "$1"; fail() { echo "FAIL: $*" >&2; exit 7; }; work=/nonexistent; m6_on `+c.who+` `+c.args, "x", mustAbs(t, "m6-chain.sh"))
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 7 || !strings.Contains(string(out), "never runs") {
			t.Errorf("m6_on %s %s: %v, %s", c.who, c.args, err, out)
		}
	}
}

// A failure of the chain's setup or A's connect shows what the command
// printed through m6_scrub only (tokens and the server's API key masked),
// never a raw cat of the run's files.
func TestM6ChainShowsItsFilesScrubbed(t *testing.T) {
	raw, err := os.ReadFile(mustAbs(t, "m6-chain.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if l := regexp.MustCompile(`(?m)^.*cat "\$work/.*$`).Find(raw); l != nil {
		t.Errorf("a file of the run is shown unscrubbed: %s", l)
	}
	for _, f := range []string{"setup.json", "setup.err", "expose.json", "connect-a.json", "connect-a.err"} {
		if !strings.Contains(string(raw), `m6_scrub < "$work/`+f+`" >&2`) {
			t.Errorf("%s is not shown through m6_scrub on failure", f)
		}
	}
}

// With no private IPv4 on the default route (a fake ip that finds none),
// the chain fails on CI (CI or GITHUB_ACTIONS set: a skip would be a green
// job with no coverage) and is skipped elsewhere, exit 0 with a skip line.
func TestM6ChainWithoutAPrivateIPFailsOnCIAndSkipsElsewhere(t *testing.T) {
	testtool.SkipWindows(t, "POSIX script, Linux only")
	for _, c := range []struct {
		name string
		env  []string
		code int
		says string
	}{
		{"CI", []string{"CI=true"}, 1, "the chain cannot run on CI"},
		{"GitHub Actions", []string{"GITHUB_ACTIONS=true"}, 1, "the chain cannot run on CI"},
		{"elsewhere", nil, 0, "m6-chain: skipped: no private IPv4 on the default route"},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			calls := filepath.Join(bin, "ip.calls")
			// A default route by a public address only.
			writeExec(t, filepath.Join(bin, "ip"), "#!/bin/sh\necho \"$*\" >> "+calls+"\necho '1.1.1.1 via 203.0.113.1 dev eth0 src 203.0.113.7 uid 1000'\n")
			cmd := exec.Command("bash", mustAbs(t, "m6-chain.sh"))
			for _, kv := range os.Environ() {
				switch strings.SplitN(kv, "=", 2)[0] {
				case "CI", "GITHUB_ACTIONS", "PATH", "M6_DRY", "PROJECT":
					continue
				}
				cmd.Env = append(cmd.Env, kv)
			}
			cmd.Env = append(cmd.Env, append([]string{"PATH=" + bin + ":" + os.Getenv("PATH"), "BACKSIGHT_CI_SLOT=1"}, c.env...)...)
			out, err := cmd.CombinedOutput()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != c.code || !strings.Contains(string(out), c.says) {
				t.Errorf("exit %d, want %d saying %q:\n%s", code, c.code, c.says, out)
			}
			// Positive control: the address was looked up, and found public.
			if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "-4 route get") {
				t.Errorf("the default route was never looked up (ip calls %q): the test would prove nothing", b)
			}
		})
	}
}
