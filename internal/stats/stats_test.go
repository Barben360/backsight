package stats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/telemetry"
)

func ptr(s string) *string { return &s }

// The events are written by the real telemetry writer, so a renamed field
// on either side fails here.
func TestComputeFromRealEvents(t *testing.T) {
	state := t.TempDir()
	s1 := telemetry.New(state, telemetry.Common{Session: "s1"})
	s2 := telemetry.New(state, telemetry.Common{Session: "s2"})
	long := 60
	// s1: briefing of b1, b2; prompt 1 recalls (one scope fails) and is
	// searched; the detector fires on prompt 2 and a store follows in turn 3.
	s1.Emit(telemetry.Briefing{IDs: []string{"b1", "b2"}, Source: "observations"})
	s1.Emit(telemetry.Prompt{N: 1, Decision: "recall", Plen: long, MS: 900, Injected: 2, Chars: 300,
		Scopes: map[string]telemetry.ScopeResult{"project": {OK: true}, "global": {OK: false}}})
	s1.Emit(telemetry.Raw{Name: "tool_use", Fields: map[string]any{"name": "mcp__plugin_backsight_memory__memory_search", "ids": 2, "memory_ids": []string{"b2", "x"}, "n": 1}})
	s1.Emit(telemetry.Reply{N: 1, Credit: "exact", Searched: true})
	s1.Emit(telemetry.Prompt{N: 2, Decision: "skip", Reason: "same_topic", Plen: long, Detector: ptr("rule")})
	s1.Emit(telemetry.Outcome{N: 1, IDs: []string{"m1", "m2"}, Searched: true, Credited: ptr("exact"), Contradicted: true})
	s1.Emit(telemetry.Reply{N: 2, Credit: ""})
	s1.Emit(telemetry.Prompt{N: 3, Decision: "skip", Reason: "too_short", Plen: 10})
	s1.Emit(telemetry.Raw{Name: "tool_use", Fields: map[string]any{"name": "mcp__plugin_backsight_memory__memory_store", "ids": 0, "memory_ids": nil, "n": 3}})
	// a subagent's search counts for nothing
	s1.Emit(telemetry.Raw{Name: "tool_use", Fields: map[string]any{"name": "mcp__plugin_backsight_memory__memory_search", "memory_ids": []string{"b1"}, "n": 3, "subagent": true}})
	// s2: a machine message, a long prompt never searched, a slow recall
	s2.Emit(telemetry.Prompt{Decision: "skip", Reason: "machine"})
	s2.Emit(telemetry.Prompt{N: 1, Decision: "recall", Plen: long, MS: 3000, Chars: 100, Detector: ptr("approval"),
		Scopes: map[string]telemetry.ScopeResult{"project": {OK: true}}})
	s2.Emit(telemetry.Tool{Name: "memory_search", Live: "none"})
	s2.Emit(telemetry.Gate{Decision: "deny"})
	s2.Emit(telemetry.Retain{Status: "ok"})
	s2.Emit(telemetry.HookFailure{Kind: "timeout"})
	s2.Emit(telemetry.Raw{Name: "transcript_unknown", Fields: map[string]any{"types": map[string]int{"new-kind": 3}}})

	now := time.Now()
	ev, err := Load(filepath.Join(state, "telemetry"), now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r := Compute(ev, now.Add(-time.Hour), now.Add(time.Hour))
	p := r.Prompts
	if p.Human != 4 || p.Machine != 1 || p.Recalls != 2 || p.WithHits != 1 || p.Skips["same_topic"] != 1 || p.Skips["too_short"] != 1 ||
		p.P50MS != 900 || p.P95MS != 3000 || p.ScopeCalls != 3 || p.ScopeFailures != 1 {
		t.Errorf("prompts %+v", p)
	}
	if r.Turns.Long != 3 || r.Turns.Searched != 1 || r.Turns.Credit["exact"] != 1 || len(r.Turns.Credit) != 1 {
		t.Errorf("turns %+v", r.Turns)
	}
	if i := r.Injections; i.N != 1 || i.Searched != 1 || i.Credited != 1 || i.Contradicted != 1 {
		t.Errorf("injections %+v", i)
	}
	if b := r.Briefings; b.N != 1 || b.IDs != 2 || b.Used != 1 || b.Empty != 0 {
		t.Errorf("briefings %+v (b2 was returned later, b1 only to a subagent)", b)
	}
	if d := r.Detector; d.Fired != 2 || d.Saved != 1 {
		t.Errorf("detector %+v", d)
	}
	if r.Budget.Sessions != 2 || r.Budget.P95Chars != 300 {
		t.Errorf("budget %+v", r.Budget)
	}
	if r.Gate["deny"] != 1 || r.Tools["memory_search"] != 1 || r.Live["none"] != 1 || r.Retains["ok"] != 1 || r.HookFailures["timeout"] != 1 || r.Unknown["new-kind"] != 3 {
		t.Errorf("counts %+v %+v %+v %+v %+v %+v", r.Gate, r.Tools, r.Live, r.Retains, r.HookFailures, r.Unknown)
	}
}

func TestLoadKeepsTheWindowAndSkipsBrokenLines(t *testing.T) {
	dir := t.TempDir()
	lines := `{"ts":"2026-09-30T23:59:59.000Z","event":"gate","decision":"deny"}
{"ts":"2026-10-01T00:00:00.000Z","event":"gate","decision":"allow"}
{"ts":"2026-10-01T00:00:01.000Z","event":"gate","deci
`
	_ = os.WriteFile(filepath.Join(dir, "events-2026-09.jsonl"), []byte(lines[:strings.Index(lines, "\n")+1]), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "events-2026-10.jsonl"), []byte(lines[strings.Index(lines, "\n")+1:]), 0o600)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ev, err := Load(dir, from, from.Add(24*time.Hour))
	if err != nil || len(ev) != 1 || ev[0].Decision != "allow" {
		t.Errorf("%+v %v", ev, err)
	}
	ev, _ = Load(dir, from.Add(-time.Hour), from.Add(24*time.Hour))
	if len(ev) != 2 || ev[0].Decision != "deny" {
		t.Errorf("across months: %+v", ev)
	}
}

func TestPercentile(t *testing.T) {
	xs := []int{5, 1, 4, 2, 3, 6, 7, 8, 9, 10}
	if Percentile(xs, 50) != 5 || Percentile(xs, 95) != 10 || Percentile(nil, 95) != 0 || Percentile([]int{7}, 95) != 7 {
		t.Errorf("%d %d", Percentile(xs, 50), Percentile(xs, 95))
	}
}
