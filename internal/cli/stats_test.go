package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/telemetry"
)

func TestStatsReportsTelemetryAndExtraction(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	llmInput.Store(2_000_000)
	tw := telemetry.New(pathsOf(home).State, telemetry.Common{Session: "s1"})
	tw.Emit(telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, MS: 1200, Injected: 1, Lang: "fr", Scopes: map[string]telemetry.ScopeResult{"project": {OK: true}}})
	tw.Emit(telemetry.Raw{Name: "tool_use", Fields: map[string]any{"name": "mcp__plugin_backsight_memory__memory_search", "n": 1}})
	tw.Emit(telemetry.Outcome{N: 1, IDs: []string{"m1"}, Searched: true})
	code, out, _ := run(t, "stats", "3")
	if code != output.OK || !strings.Contains(out, "last 3 day(s)") || !strings.Contains(out, "1 typed") || !strings.Contains(out, "p50 1200 ms") ||
		!strings.Contains(out, "followed by a search 100%") || !strings.Contains(out, "2000000 input") ||
		!strings.Contains(out, "French 1 recalls (100% injected), English 0 (-)") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if code, out, _ := run(t, "stats", "--json"); code != output.OK || !strings.Contains(out, `"days": 7`) || !strings.Contains(out, `"recalls": 1`) {
		t.Errorf("default window: exit %d\n%s", code, out)
	}
	for _, bad := range []string{"0", "366", "x"} {
		if code, _, _ := run(t, "stats", bad); code != output.Usage {
			t.Errorf("stats %s: exit %d", bad, code)
		}
	}
	// the server down leaves the local report
	statsDown.Store(true)
	if code, out, _ := run(t, "stats"); code != output.OK || !strings.Contains(out, "Extraction   unavailable") || !strings.Contains(out, "1 typed") {
		t.Errorf("server down: exit %d\n%s", code, out)
	}
}

// The window applies to local telemetry and to the server's daily buckets
// (asked for 30d, the only long period Hindsight knows); thoughts are billed
// as output; 1 and 365 are accepted.
func TestStatsWindowPeriodAndCost(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	llmInput.Store(2_000_000)
	llmThoughts.Store(1_000_000)
	llmOldInput.Store(5_000_000) // 10 days ago
	old := time.Now().Add(-5 * 24 * time.Hour).UTC()
	dir := pathsOf(home).TelemetryDir()
	_ = os.MkdirAll(dir, 0o700)
	line := `{"ts":"` + old.Format("2006-01-02T15:04:05.000Z") + `","event":"prompt","decision":"recall","plen":80,"ms":700,"session":"old"}` + "\n"
	_ = os.WriteFile(filepath.Join(dir, "events-"+old.Format("2006-01")+".jsonl"), []byte(line), 0o600)
	if _, out, _ := run(t, "stats", "3"); !strings.Contains(out, "0 typed") || llmPeriod != "30d" || !strings.Contains(out, "2000000 input") {
		t.Errorf("3 days: period %q\n%s", llmPeriod, out)
	}
	_, out, _ := run(t, "stats", "7")
	if !strings.Contains(out, "1 typed") || !strings.Contains(out, "2000000 input") || !strings.Contains(out, "1000000 output tokens: 1.80 USD") {
		t.Errorf("7 days:\n%s", out)
	}
	// the day 10 days ago is in a 14-day window, not in a 7-day one
	if _, out, _ := run(t, "stats", "14"); !strings.Contains(out, "7000000 input") || strings.Contains(out, "server keeps no more") {
		t.Errorf("14 days:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "90"); !strings.Contains(out, "7000000 input") || !strings.Contains(out, "last 30 days: the server keeps no more") {
		t.Errorf("90 days:\n%s", out)
	}
	for _, ok := range []string{"1", "365"} {
		if code, _, _ := run(t, "stats", ok); code != output.OK {
			t.Errorf("stats %s: exit %d", ok, code)
		}
	}
}

// Producers to reader: what the hooks, the flush and the tick write is what
// stats counts (a renamed field on either side fails here).
func TestStatsReadsWhatTheProducersWrite(t *testing.T) {
	home := hookHome(t)
	recallAnswer = "[]"
	withClock(t, time.Now().Add(-3*time.Hour))
	run(t, "jobs", "tick") // retain_since
	withClock(t, time.Now())
	search := func(sid, agent string) {
		in := map[string]any{"session_id": sid, "tool_name": "mcp__plugin_backsight_memory__memory_search",
			"tool_response": "[billing] fact (2d ago, 0b4c2a52-0000-4000-8000-000000000001)"}
		if agent != "" {
			in["agent_id"] = agent
		}
		fire(t, "post-tool-use", in)
	}
	prompt(t, home, "s1", longPrompt)
	search("s1", "")
	fire(t, "stop", map[string]any{"session_id": "s1", "last_assistant_message": "> 🧠 **From Hindsight memory (billing)** — PostgreSQL 16."})
	prompt(t, home, "s1", longPrompt+" And the replica?")
	search("s1", "") // turn 2
	prompt(t, home, "s2", longPrompt)
	search("s2", "agent-1") // a subagent's: s2 stays unsearched
	credit := "> 🧠 **From Hindsight memory (billing)** — PostgreSQL 16."
	fire(t, "stop", map[string]any{"session_id": "s2", "last_assistant_message": credit}) // only a subagent searched
	prompt(t, home, "s1", "A third question, not searched, about the billing replica lag.")
	fire(t, "stop", map[string]any{"session_id": "s1", "last_assistant_message": credit}) // the search was turns ago
	fire(t, "stop", map[string]any{"session_id": "s9", "last_assistant_message": "no prompt was seen"})
	writeTranscript(t, home, "settled", time.Now().Add(-2*time.Hour))
	p := filepath.Join(home, ".claude", "projects", "-home-u-billing", "settled.jsonl")
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString(`{"type":"brand-new-kind","uuid":"z"}` + "\n")
	_ = f.Close()
	writeTranscript(t, home, "fresh", time.Now().Add(-time.Minute)) // waiting: read, not counted
	f, _ = os.OpenFile(filepath.Join(home, ".claude", "projects", "-home-u-billing", "fresh.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString(`{"type":"other-new-kind","uuid":"w"}` + "\n")
	_ = f.Close()
	_, _ = notesOutbox(pathsOf(home)).Append(newTestNote())
	run(t, "jobs", "tick")
	run(t, "jobs", "tick")                    // a second read of the waiting session counts nothing either
	withClock(t, time.Now().Add(time.Minute)) // the window ends after the events
	_, out, _ := run(t, "stats", "1", "--json")
	var env struct {
		Data statsResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	r := env.Data
	if r.Turns.Long != 4 || r.Turns.Searched != 2 || r.Turns.Credit["exact"] != 1 || len(r.Turns.Credit) != 1 {
		t.Errorf("turns %+v", r.Turns)
	}
	if r.Unknown["brand-new-kind"] != 1 || r.Unknown["other-new-kind"] != 0 || r.Retains["ok"] == 0 {
		t.Errorf("unknown %+v retains %+v", r.Unknown, r.Retains)
	}
	tel := telemetryText(t, home)
	if strings.Count(tel, `"event":"reply"`) != 3 || !strings.Contains(tel, `"notes_sent":1`) || !strings.Contains(tel, `"subagent":true`) {
		t.Errorf("telemetry:\n%s", tel)
	}
}

// --since/--until count only the events in [since, until), whenever stats
// runs; --parity adds the C12 comparison (decision 0065).
func TestStatsAbsoluteWindowAndParity(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dir := pathsOf(home).TelemetryDir()
	_ = os.MkdirAll(dir, 0o700)
	var lines strings.Builder
	for _, ts := range []string{"2026-09-30T23:59:59.000Z", "2026-10-01T00:00:00.000Z", "2026-10-01T12:00:00.000Z", "2026-10-02T00:00:00.000Z"} {
		lines.WriteString(`{"ts":"` + ts + `","event":"prompt","decision":"recall","plen":80,"ms":700,"injected":1,"session":"w"}` + "\n")
	}
	_ = os.WriteFile(filepath.Join(dir, "events-2026-09.jsonl"), []byte(strings.SplitAfter(lines.String(), "\n")[0]), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "events-2026-10.jsonl"), []byte(strings.Join(strings.SplitAfter(lines.String(), "\n")[1:], "")), 0o600)

	code, out, _ := run(t, "stats", "--since", "2026-10-01", "--until", "2026-10-02T00:00Z")
	if code != output.OK || !strings.Contains(out, "Memory from 2026-10-01 00:00 → 2026-10-02 00:00 (UTC)") || !strings.Contains(out, "2 typed") || strings.Contains(out, "Parity") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	_, out, _ = run(t, "stats", "--since", "2026-09-30T23:00:00Z", "--until", "2026-10-02T00:00Z", "--parity", "--json")
	var env struct {
		Data statsResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if r := env.Data; r.Prompts.Human != 3 || !r.Window || r.Days != 2 || len(r.Parity) != 6 || r.Parity[0].Verdict != "too few" || r.Parity[0].N != 3 {
		t.Errorf("%+v", r)
	}
	if _, out, _ := run(t, "stats", "--since", "2026-10-01", "--until", "2026-10-02", "--parity"); !strings.Contains(out, "recalls that injected something") || !strings.Contains(out, "26% (16/61)") || !strings.Contains(out, "Parity       not yet") {
		t.Errorf("parity text:\n%s", out)
	}
	for _, bad := range [][]string{
		{"7", "--since", "2026-10-01"},                     // both forms
		{"--until", "2026-10-02"},                          // until alone
		{"--since", "yesterday"},                           // not a time
		{"--since", "2026-10-02", "--until", "2026-10-01"}, // reversed
		{"--since", "2020-01-01", "--until", "2026-10-01"}, // over 366 days
	} {
		if code, _, _ := run(t, append([]string{"stats"}, bad...)...); code != output.Usage {
			t.Errorf("stats %v: exit %d", bad, code)
		}
	}
}

// The server's buckets follow the same [since, until) window: an until at
// midnight leaves its day out, and a window older than the server's 30
// days says none were counted instead of showing 0 as a measure.
func TestStatsAbsoluteWindowExtraction(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	llmInput.Store(2_000_000)    // today
	llmOldInput.Store(5_000_000) // 10 days ago
	day := func(back int) string { return time.Now().UTC().AddDate(0, 0, -back).Format("2006-01-02") }
	if _, out, _ := run(t, "stats", "--since", day(10), "--until", day(9)); !strings.Contains(out, "5000000 input") {
		t.Errorf("the day 10 days ago alone:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "--since", day(11), "--until", day(10)); !strings.Contains(out, " 0 input") {
		t.Errorf("until at midnight leaves its day out:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "--since", day(10)); !strings.Contains(out, "7000000 input") || strings.Contains(out, "server keeps") {
		t.Errorf("since alone runs to now:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "--since", day(60), "--until", day(50)); !strings.Contains(out, "none counted: the server keeps only the last 30 days") {
		t.Errorf("a window older than the server's buckets:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "--since", day(40), "--until", day(9)); !strings.Contains(out, "5000000 input") || !strings.Contains(out, "(last 20 days: the server keeps no more)") {
		t.Errorf("a window cut by the server's 30 days:\n%s", out)
	}
}
