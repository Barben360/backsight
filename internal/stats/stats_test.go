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
	s1.Emit(telemetry.Prompt{N: 1, Decision: "recall", Plen: long, MS: 900, Injected: 2, Chars: 300, Lang: "fr",
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
	s2.Emit(telemetry.Prompt{N: 1, Decision: "recall", Plen: long, MS: 3000, Chars: 100, Detector: ptr("approval"), Lang: "en",
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
	if r.Prompts.ByLang["fr"] != [2]int{1, 1} || r.Prompts.ByLang["en"] != [2]int{1, 0} || len(r.Prompts.ByLang) != 2 {
		t.Errorf("by language %+v", r.Prompts.ByLang)
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

func write(t *testing.T, dir, month, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "events-"+month+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// [from, to): an event exactly at `to` is out; lines out of order come back
// sorted; a line over 1 MiB is skipped, not fatal; December → January.
func TestLoadEdgesOrderLongLinesAndYearRollover(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "2026-12", `{"ts":"2026-12-31T23:00:00.000Z","event":"gate","decision":"b"}
{"ts":"2026-12-31T22:00:00.000Z","event":"gate","decision":"a"}
{"ts":"2026-12-31T21:00:00.000Z","event":"gate","decision":"`+strings.Repeat("x", 2<<20)+`"}
`)
	write(t, dir, "2027-01", `{"ts":"2027-01-01T01:00:00.000Z","event":"gate","decision":"c"}
{"ts":"2027-01-01T02:00:00.000Z","event":"gate","decision":"at-to"}`) // no final newline
	from, to := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 2, 0, 0, 0, time.UTC)
	ev, err := Load(dir, from, to)
	var got []string
	for _, e := range ev {
		got = append(got, e.Decision)
	}
	if err != nil || strings.Join(got, ",") != "a,b,c" {
		t.Errorf("%v %v", got, err)
	}
	ev, _ = Load(dir, from, to.Add(time.Second))
	if len(ev) != 4 || ev[3].Decision != "at-to" {
		t.Errorf("a last line without newline: %d events", len(ev))
	}
}

func TestComputeBranches(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	cr := func(s string) *string { return &s }
	rule := cr("rule")
	ev := []Event{
		// a memory returned before its briefing is not briefing use
		{TS: at(0), Event: "tool_use", Session: "s1", Name: "x__memory_search", N: 0, MemoryIDs: []string{"b1"}},
		{TS: at(1), Event: "briefing", Session: "s1", IDs: []string{"b1"}, Source: "observations"},
		{TS: at(1), Event: "briefing", Session: "s2", Source: "empty"},
		{TS: at(1), Event: "briefing", Session: "s5", Source: "model"},
		// silent and free replies after a search; a reply without a search,
		// or after a store only, is ignored; a turn's second reply (a
		// machine message) keeps the best credit, once
		{TS: at(2), Event: "tool_use", Session: "s1", Name: "x__memory_read", N: 1},
		{TS: at(2), Event: "reply", Session: "s1", N: 1, Searched: true},
		{TS: at(3), Event: "tool_use", Session: "s1", Name: "x__memory_search", N: 2},
		{TS: at(3), Event: "reply", Session: "s1", N: 2, Searched: true, Credit: "free"},
		{TS: at(3), Event: "reply", Session: "s1", N: 2, Searched: true},
		{TS: at(3), Event: "reply", Session: "s1", N: 3, Searched: true, Credit: "exact"},
		{TS: at(3), Event: "tool_use", Session: "s4", Name: "x__memory_store", N: 1},
		{TS: at(3), Event: "reply", Session: "s4", N: 1, Searched: true, Credit: "exact"},
		// outcomes: free counts as credited, empty does not; a recall that
		// injected nothing is no injection; searched follows the turn's tools
		{TS: at(4), Event: "outcome", Session: "s1", N: 1, IDs: []string{"m"}, Credited: cr("free")},
		{TS: at(4), Event: "outcome", Session: "s1", N: 3, IDs: []string{"m"}, Credited: cr("empty"), Searched: true},
		{TS: at(4), Event: "outcome", Session: "s1", N: 4, Credited: cr("exact")},
		// C12's counts for parity: turn 1 was searched by a read only, turn
		// 2 by a memory_search; only an exact credit counts
		{TS: at(4), Event: "outcome", Session: "s1", N: 2, IDs: []string{"m"}, Credited: cr("exact")},
		// detector: a store in another session, or two turns later, is no follow-up
		{TS: at(5), Event: "prompt", Session: "s1", N: 5, Decision: "skip", Plen: 80, Detector: rule},
		{TS: at(6), Event: "tool_use", Session: "s2", Name: "x__memory_store", N: 5},
		{TS: at(7), Event: "tool_use", Session: "s1", Name: "x__memory_store", N: 7},
		// a subagent's search on a long turn does not make it searched
		{TS: at(8), Event: "prompt", Session: "s3", N: 1, Decision: "skip", Plen: 80},
		{TS: at(8), Event: "tool_use", Session: "s3", Name: "x__memory_search", N: 1, Subagent: true},
		// a recall written before prompts carried lang is in no language
		{TS: at(8), Event: "prompt", Session: "s3", N: 2, Decision: "recall", Plen: 10},
		// unknown records sum over events
		{TS: at(9), Event: "transcript_unknown", Types: map[string]int{"k": 2}},
		{TS: at(9), Event: "transcript_unknown", Types: map[string]int{"k": 3}},
	}
	r := Compute(ev, t0, at(60))
	if r.Briefings.N != 3 || r.Briefings.Empty != 1 || r.Briefings.Model != 1 || r.Briefings.Used != 0 {
		t.Errorf("briefings %+v", r.Briefings)
	}
	if r.Turns.Credit["silent"] != 1 || r.Turns.Credit["free"] != 1 || r.Turns.Credit["exact"] != 0 || len(r.Turns.Credit) != 2 {
		t.Errorf("credit %+v", r.Turns.Credit)
	}
	if i := r.Injections; i.N != 3 || i.Credited != 2 || i.Searched != 2 || i.SearchedBySearch != 1 || i.Exact != 1 {
		t.Errorf("injections %+v", r.Injections)
	}
	if r.Detector.Fired != 1 || r.Detector.Saved != 0 {
		t.Errorf("detector %+v", r.Detector)
	}
	if r.Turns.Long != 2 || r.Turns.Searched != 0 {
		t.Errorf("turns %+v", r.Turns)
	}
	if len(r.Prompts.ByLang) != 0 {
		t.Errorf("by language %+v", r.Prompts.ByLang)
	}
	if r.Unknown["k"] != 5 {
		t.Errorf("unknown %+v", r.Unknown)
	}
}

func TestPercentileOnTwentyValuesLeavesTheInput(t *testing.T) {
	xs := make([]int, 20)
	for i := range xs {
		xs[i] = 20 - i // 20 … 1
	}
	if p := Percentile(xs, 95); p != 19 {
		t.Errorf("p95 of 1..20 = %d, want 19", p)
	}
	if xs[0] != 20 {
		t.Error("the input was sorted in place")
	}
}

// feedbackEvents writes one window of the feedback telemetry: end-of-turn
// checks, memory_credit calls and two settles.
func feedbackEvents(tw *telemetry.Writer) {
	for _, c := range []telemetry.CreditCheck{
		{Outcome: "declared", Shown: 2, Flags: []string{"credit_without_overlap"}},
		{Outcome: "declared", Shown: 1},
		{Outcome: "declared", Shown: 3, Flags: []string{"none_with_overlap"}, AfterBlock: true},
		{Outcome: "blocked", Shown: 1},
		{Outcome: "blocked", Shown: 2},
		{Outcome: "undeclared", Shown: 2, AfterBlock: true},
		{Outcome: "check_skipped", Shown: 1},
	} {
		tw.Emit(c)
	}
	for _, c := range []telemetry.Credit{
		{Verdict: "used", IDs: 2, OK: true}, {Verdict: "used", IDs: 1, OK: true}, {Verdict: "none", OK: true},
		{Verdict: "used", IDs: 1, OK: false}, {Verdict: "invalid", OK: false},
	} {
		tw.Emit(c)
	}
	tw.Emit(telemetry.FeedbackSettle{Shows: 6, Credits: 1, Misses: 2, Unknown: 1, Late: 1})
	tw.Emit(telemetry.FeedbackSettle{Shows: 4, Credits: 1, Misses: 1})
}

// The feedback counts, from events the real writer wrote: checks by
// outcome, accepted credits by verdict and the refused ones, the settled
// shows summed, the late credits and the honesty flags.
func TestComputeCountsFeedback(t *testing.T) {
	state := t.TempDir()
	feedbackEvents(telemetry.New(state, telemetry.Common{Session: "s1"}))
	now := time.Now()
	ev, err := Load(filepath.Join(state, "telemetry"), now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := Compute(ev, now.Add(-time.Hour), now.Add(time.Hour)).Feedback
	if f.Checks["declared"] != 3 || f.Checks["blocked"] != 2 || f.Checks["undeclared"] != 1 || f.Checks["check_skipped"] != 1 || len(f.Checks) != 4 || f.DeclaredAfterBlock != 1 {
		t.Errorf("checks %v, declared after a block %d", f.Checks, f.DeclaredAfterBlock)
	}
	if f.Verdicts["used"] != 2 || f.Verdicts["none"] != 1 || len(f.Verdicts) != 2 || f.Refused != 2 {
		t.Errorf("verdicts %v, refused %d", f.Verdicts, f.Refused)
	}
	if s := f.Settled; s.Shows != 10 || s.Credits != 2 || s.Misses != 3 || s.Unknown != 1 || f.Late != 1 {
		t.Errorf("settled %+v, late %d", s, f.Late)
	}
	if f.Flags["credit_without_overlap"] != 1 || f.Flags["none_with_overlap"] != 1 || len(f.Flags) != 2 {
		t.Errorf("flags %v", f.Flags)
	}
}

// The verdicts that retire: wrong and outdated counted with the other
// accepted credits, the memories invalidated (partly refused calls
// included) and hidden summed from the retire events, the partly refused
// calls counted; a Sources answer retires nothing.
func TestComputeCountsVerdicts(t *testing.T) {
	state := t.TempDir()
	tw := telemetry.New(state, telemetry.Common{Session: "s1"})
	for _, c := range []telemetry.Credit{{Verdict: "wrong", IDs: 2, OK: true}, {Verdict: "outdated", IDs: 1, OK: true}, {Verdict: "wrong", IDs: 1, OK: true}, {Verdict: "outdated", IDs: 1}} {
		tw.Emit(c)
	}
	for _, r := range []telemetry.Retire{{Action: "retired", IDs: 2}, {Action: "hidden", IDs: 1}, {Action: "partly", IDs: 1}, {Action: "hidden", IDs: 2}, {Action: "sources", IDs: 3}} {
		tw.Emit(r)
	}
	now := time.Now()
	ev, err := Load(filepath.Join(state, "telemetry"), now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := Compute(ev, now.Add(-time.Hour), now.Add(time.Hour)).Feedback
	if f.Verdicts["wrong"] != 2 || f.Verdicts["outdated"] != 1 || f.Refused != 1 {
		t.Errorf("verdicts %v, refused %d", f.Verdicts, f.Refused)
	}
	if f.Retired != 3 || f.Hidden != 3 || f.Partly != 1 {
		t.Errorf("retired %d, hidden %d, partly %d; want 3, 3, 1", f.Retired, f.Hidden, f.Partly)
	}
}
