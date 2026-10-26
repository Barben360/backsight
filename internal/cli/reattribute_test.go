package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
)

func TestReattributeRefusesUnknownAnswers(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	for _, a := range []string{"resend=maybe", "backup=yes", "confirm=yes"} {
		if code, out, _ := run(t, "retain", "reattribute", "--answer", a); code != output.Usage {
			t.Errorf("--answer %s: exit %d\n%s", a, code, out)
		}
	}
}

// --plan reads and writes nothing; --json carries the plan.
func TestReattributePlanChangesNothing(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dk := withBackupDocker(t)
	code, out, _ := run(t, "retain", "reattribute", "--plan", "--json")
	var env struct {
		Data reattributeResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); code != output.OK || err != nil || env.Data.Action != "planned" || env.Data.Bank != "claude-memory" {
		t.Fatalf("exit %d %v\n%s", code, err, out)
	}
	if len(dk.calls) != 0 {
		t.Errorf("a plan took a backup: %q", dk.calls)
	}
	if code, out, _ := run(t, "retain", "reattribute"); code != output.OK || !strings.Contains(out, "nothing to move") {
		t.Errorf("nothing to do: exit %d\n%s", code, out)
	}
	if len(dk.calls) != 0 {
		t.Errorf("nothing to move, yet a backup was taken: %q", dk.calls)
	}
}

// The exit-code contract: 40 when a write failed (the same command
// finishes), 10 while re-sends wait for an answer, 0 otherwise.
func TestReattributeExitCodes(t *testing.T) {
	resends := retain.ReattributionPlan{Resends: []retain.Resend{{Session: "s1"}}}
	cases := []struct {
		name    string
		res     reattributeResult
		answers map[string]string
		opt     reattributeOptions
		code    int
		action  string
	}{
		{"nothing", reattributeResult{}, nil, reattributeOptions{}, output.OK, "nothing"},
		{"moved", reattributeResult{Retagged: []string{"d"}}, nil, reattributeOptions{}, output.OK, "done"},
		{"failed", reattributeResult{Retagged: []string{"d"}, Failed: []string{"e: HTTP 500"}}, nil, reattributeOptions{}, output.Partial, "partial"},
		{"re-sends unanswered", reattributeResult{Plan: resends}, nil, reattributeOptions{}, output.NeedsUser, "resends_pending"},
		{"re-sends declined", reattributeResult{Plan: resends}, map[string]string{"resend": "no"}, reattributeOptions{}, output.OK, "resends_pending"},
		{"re-sends left by the daily job", reattributeResult{Plan: resends}, nil, reattributeOptions{freeOnly: true}, output.OK, "resends_pending"},
		{"re-sends queued", reattributeResult{Plan: resends, Queued: []string{"s1"}}, map[string]string{"resend": "yes"}, reattributeOptions{}, output.OK, "done"},
	}
	for _, c := range cases {
		res, err := finishReattribute(c.res, reattributeFlags{}, c.opt, c.answers)
		if output.CodeOf(err) != c.code || res.Action != c.action {
			t.Errorf("%s: exit %d %q, want %d %q (%v)", c.name, output.CodeOf(err), res.Action, c.code, c.action, err)
		}
	}
}
