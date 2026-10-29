package scripts

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The CI chain re-runs the VM bench's dedup and key-rotation proofs on
// every pull request, on containers (scripts/m6-chain.sh).
func TestM6ChainRunsOnEveryPullRequest(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^  pull_request:`).Match(raw) {
		t.Error("ci.yml does not run on pull_request")
	}
	found := false
	for _, w := range workflows(t) {
		if w.name != "ci.yml" {
			continue
		}
		for _, j := range w.jobs {
			if j.name != "m6-chain" {
				continue
			}
			for _, s := range j.steps {
				if strings.Contains(s, "scripts/m6-chain.sh") && regexp.MustCompile(`(?m)^\s*(- )?run:`).MatchString(s) {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("ci.yml has no m6-chain job with a step running scripts/m6-chain.sh")
	}
}

func TestM6ChainRunsTheDedupAndRotationProofs(t *testing.T) {
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
