package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/telemetry"
)

// clientTelemetry writes Claude Code's two sessions (two recalls, one that
// injected, one used credit) and Codex's one (a recall that injected
// nothing, a wrong verdict), a minute before the views read them: an event
// of the view's own millisecond would fall at the window's open end.
func clientTelemetry(t *testing.T, home string) {
	t.Helper()
	d := pathsOf(home)
	at := now().Add(-time.Minute)
	clock := func() time.Time { return at }
	for i, s := range []string{"cc-1", "cc-2"} {
		tw := telemetry.New(d.State, telemetry.Common{Session: s, Client: harnessid.ClaudeCode}).WithClock(clock)
		tw.Emit(telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Injected: 1 - i})
		if i == 0 {
			tw.Emit(telemetry.Credit{Verdict: "used", IDs: 1, OK: true})
		}
	}
	tw := telemetry.New(d.State, telemetry.Common{Session: "cx-1", Client: harnessid.Codex}).WithClock(clock)
	tw.Emit(telemetry.Prompt{N: 1, Decision: "recall", Plen: 80})
	tw.Emit(telemetry.Credit{Verdict: "wrong", IDs: 1, OK: true})
}

// stats [days]: combined by default, with a line splitting it by tool;
// --client counts one tool, says so, and drops the split; a tool it does
// not know is a usage error, and stats index refuses the flag.
func TestStatsByClient(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	clientTelemetry(t, home)
	code, out, _ := run(t, "stats", "1")
	if code != output.OK || !strings.Contains(out, "3 typed") ||
		!strings.Contains(out, "By tool      Claude Code 2 sessions, 2 recalls (50% injected something), 1 used; Codex 1 session, 1 recalls (0% injected something), 0 used (--client for one)\n") ||
		strings.Contains(out, "Coding tool") {
		t.Fatalf("combined: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "stats", "1", "--client", "codex")
	if code != output.OK || !strings.Contains(out, "Coding tool  Codex only") || !strings.Contains(out, "1 typed") || strings.Contains(out, "By tool") ||
		!strings.Contains(out, "used 0, none 0, wrong 1") {
		t.Errorf("--client codex: exit %d\n%s", code, out)
	}
	_, out, _ = run(t, "stats", "1", "--client", "claude-code", "--json")
	var res struct {
		Data struct {
			Client   string `json:"client"`
			ByClient []any  `json:"by_client"`
			Prompts  struct {
				Human int `json:"human"`
			} `json:"prompts"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Data.Client != "claude-code" || res.Data.Prompts.Human != 2 || res.Data.ByClient != nil {
		t.Errorf("json: %v %+v\n%s", err, res.Data, out)
	}
	if code, _, _ := run(t, "stats", "1", "--client", "cursor"); code != output.Usage {
		t.Errorf("an unknown tool: exit %d", code)
	}
	if code, _, _ := run(t, "stats", "index", "--client", "codex"); code != output.Usage {
		t.Errorf("stats index --client: exit %d", code)
	}
}

// The views take --client as the dashboard takes client=: the overview's
// scope names the tool, and unfiltered it splits by tool; an unknown tool
// is refused.
func TestStatsViewsByClient(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	clientTelemetry(t, home)
	view := func(args ...string) metrics.Result {
		t.Helper()
		code, out, errOut := run(t, append([]string{"stats", "overview", "--days", "1", "--json"}, args...)...)
		if code != output.OK {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		var r metrics.Result
		jsonData(t, out, &r)
		return r
	}
	r := view()
	if len(r.Tables.ByClient) != 2 || r.Tables.ByClient[0].Client != "claude-code" || r.Tables.ByClient[0].Sessions != 2 || r.Tables.ByClient[1].Recalls != 1 {
		t.Errorf("split %+v", r.Tables.ByClient)
	}
	if r = view("--client", "codex"); r.Scope.Client != "codex" || r.Tables.ByClient != nil || r.Tables.Activity == nil || r.Tables.Activity.Recalls != 1 {
		t.Errorf("filtered: scope %+v activity %+v", r.Scope, r.Tables.Activity)
	}
	_, out, _ := run(t, "stats", "overview", "--days", "1", "--client", "codex")
	if !strings.Contains(out, "Coding tool: Codex") {
		t.Errorf("text:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "overview", "--days", "1"); !strings.Contains(out, "By coding tool (this PC)") || !strings.Contains(out, "Claude Code") {
		t.Errorf("split text:\n%s", out)
	}
	if code, _, _ := run(t, "stats", "overview", "--client", "cursor"); code != output.Usage {
		t.Errorf("an unknown tool: exit %d", code)
	}
}
