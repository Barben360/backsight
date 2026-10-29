package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
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

// The two feedback lines: the end-of-turn checks and credit calls, then
// the settled shows, with the confident misses as a share of the shows
// left uncredited.
func TestStatsCountsFeedback(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	tw := telemetry.New(pathsOf(home).State, telemetry.Common{Session: "s1"})
	for _, c := range []telemetry.CreditCheck{{Outcome: "declared", Flags: []string{"credit_without_overlap"}}, {Outcome: "declared"},
		{Outcome: "declared", Flags: []string{"none_with_overlap"}, AfterBlock: true}, {Outcome: "blocked"}, {Outcome: "blocked"}, {Outcome: "undeclared", AfterBlock: true},
		{Outcome: "check_skipped"}} {
		tw.Emit(c)
	}
	for _, c := range []telemetry.Credit{{Verdict: "used", IDs: 2, OK: true}, {Verdict: "used", IDs: 1, OK: true}, {Verdict: "none", OK: true},
		{Verdict: "used", IDs: 1}, {Verdict: "invalid"}} {
		tw.Emit(c)
	}
	tw.Emit(telemetry.FeedbackSettle{Shows: 6, Credits: 1, Misses: 2, Unknown: 1, Late: 1})
	tw.Emit(telemetry.FeedbackSettle{Shows: 4, Credits: 1, Misses: 1})
	_, out, _ := run(t, "stats", "1")
	for _, want := range []string{
		"\nCredits      turns with memories shown: declared 2, after a block 1, undeclared 1, skipped 1; used 2, none 1, wrong 0, outdated 0, refused 2\n",
		"\nLedger       settled 10 live shows: 2 credits, 3 confident misses (38% of uncredited), 1 unknown, 1 late; imported 0 shows of earlier history (0 credits); honesty flags: credit_without_overlap 1, none_with_overlap 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	// No feedback events: zeros, no share, no flags.
	empty := devInstall(t, true)
	pointAt(t, empty, fakeHindsight(t))
	if _, out, _ := run(t, "stats", "1"); !strings.Contains(out, "\nLedger       settled 0 live shows: 0 credits, 0 confident misses (- of uncredited), 0 unknown, 0 late; imported 0 shows of earlier history (0 credits); honesty flags: none\n") {
		t.Errorf("empty window:\n%s", out)
	}
}

// The verdicts that retire: wrong and outdated on the Credits line, and
// what they retired on their own line.
func TestStatsCountsVerdicts(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	tw := telemetry.New(pathsOf(home).State, telemetry.Common{Session: "s1"})
	for _, c := range []telemetry.Credit{{Verdict: "used", IDs: 1, OK: true}, {Verdict: "wrong", IDs: 2, OK: true}, {Verdict: "outdated", IDs: 1, OK: true},
		{Verdict: "wrong", IDs: 1, OK: true}, {Verdict: "outdated", IDs: 1}} {
		tw.Emit(c)
	}
	for _, r := range []telemetry.Retire{{Action: "retired", IDs: 2}, {Action: "partly", IDs: 1}, {Action: "hidden", IDs: 1}, {Action: "sources", IDs: 2}} {
		tw.Emit(r)
	}
	_, out, _ := run(t, "stats", "1")
	for _, want := range []string{
		"; used 1, none 0, wrong 2, outdated 1, refused 1\n",
		"\nRetirements  3 raw fact(s) invalidated on the server (1 verdict(s) the server refused halfway), 1 observation(s) hidden on this machine\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if code, out, _ := run(t, "stats", "1", "--json"); code != output.OK || !strings.Contains(out, `"retired": 3`) || !strings.Contains(out, `"hidden": 1`) || !strings.Contains(out, `"partly": 1`) {
		t.Errorf("json: exit %d\n%s", code, out)
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
	fire(t, "stop", map[string]any{"session_id": "s1", "last_assistant_message": "> 🧠 **From memory (billing)** — PostgreSQL 16."})
	prompt(t, home, "s1", longPrompt+" And the replica?")
	search("s1", "") // turn 2
	prompt(t, home, "s2", longPrompt)
	search("s2", "agent-1") // a subagent's: s2 stays unsearched
	credit := "> 🧠 **From memory (billing)** — PostgreSQL 16."
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

// The Ledger line names how many other machines' counts recall weighs
// (those in use: neither this machine nor one silent for 30 days), and
// only when there are any and sharing is on.
func TestStatsNamesSharedMachines(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	self, err := retain.MachineID(pathsOf(home).State)
	if err != nil {
		t.Fatal(err)
	}
	gone := otherMachine("pc-old", nil)
	gone.At = gone.At.Add(-31 * 24 * time.Hour)
	writeSharedCache(t, home, otherMachine("pc-b", nil), otherMachine("pc-c", nil), otherMachine("pc-c", nil), otherMachine(self, nil), gone)
	line := "\nLedger       settled 0 live shows: 0 credits, 0 confident misses (- of uncredited), 0 unknown, 0 late; imported 0 shows of earlier history (0 credits); honesty flags: none"
	if _, out, _ := run(t, "stats", "1"); !strings.Contains(out, line+"; shared with 2 other machines\n") {
		t.Errorf("two other machines:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "1", "--json"); !strings.Contains(out, `"shared_machines": 2`) {
		t.Errorf("json:\n%s", out)
	}
	writeSharedCache(t, home, otherMachine("pc-b", nil))
	if _, out, _ := run(t, "stats", "1"); !strings.Contains(out, line+"; shared with 1 other machine\n") {
		t.Errorf("one other machine:\n%s", out)
	}
	// No machine id yet: every document counts, and stats creates none.
	idFile := filepath.Join(pathsOf(home).State, "machine_id")
	if err := os.Remove(idFile); err != nil {
		t.Fatal(err)
	}
	writeSharedCache(t, home, otherMachine("pc-b", nil), otherMachine(self, nil))
	if _, out, _ := run(t, "stats", "1"); !strings.Contains(out, line+"; shared with 2 other machines\n") {
		t.Errorf("no machine id:\n%s", out)
	}
	if _, err := os.Stat(idFile); !os.IsNotExist(err) {
		t.Errorf("stats created the machine id: %v", err)
	}
	appendConfig(t, home, "[feedback]\nshare_ledger = false")
	if _, out, _ := run(t, "stats", "1"); !strings.Contains(out, line+"\n") {
		t.Errorf("sharing off:\n%s", out)
	}
}

// journalIn appends events to home's journal, each at its own TS.
func journalIn(t *testing.T, home string, events ...feedback.Event) {
	t.Helper()
	j := feedback.NewJournal(pathsOf(home).FeedbackDir(), feedback.Common{Session: "s1"})
	for _, e := range events {
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

// Under the feedback lines, the share of the window's prompt recalls and
// searches whose shown set the boost changed, from the journal: a gate
// show, a search's shows (its audit counts) and what lies outside the
// window are not counted, nor is an order changed alone.
func TestStatsBoostChangedShare(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	hour, old := time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(-3*24*time.Hour)
	items := []feedback.Item{{ID: "m1", Rank: 1}}
	set, order := []string{feedback.FlagSetChanged}, []string{feedback.FlagOrderChanged}
	journalIn(t, home,
		feedback.Event{Kind: "shows", Path: "prompt", TS: hour, Items: items, Flags: set},
		feedback.Event{Kind: "shows", Path: "prompt", TS: hour, Items: items},
		feedback.Event{Kind: "shows", Path: "prompt", TS: hour, Items: items},
		feedback.Event{Kind: "shows", Path: "prompt", TS: old, Items: items, Flags: set},
		feedback.Event{Kind: "shows", Path: "gate", TS: hour, Items: items, Flags: set},
		feedback.Event{Kind: "shows", Path: "search", TS: hour, Items: items},
		feedback.Event{Kind: feedback.KindRecall, Path: "search", TS: hour, Items: items, Flags: []string{feedback.FlagSetChanged, feedback.FlagOrderChanged}},
		feedback.Event{Kind: feedback.KindRecall, Path: "search", TS: hour, Items: items, Flags: order},
	)
	if _, out, _ := run(t, "stats", "1"); !strings.Contains(out, "\nBoost        changed the shown set of 1 of 3 prompt recalls (33%) and 1 of 2 searches (50%)\n") {
		t.Errorf("1 day:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "7"); !strings.Contains(out, "\nBoost        changed the shown set of 2 of 4 prompt recalls (50%) and 1 of 2 searches (50%)\n") {
		t.Errorf("7 days:\n%s", out)
	}
	_, raw, _ := run(t, "stats", "1", "--json")
	var env struct {
		Data struct {
			Feedback struct {
				Boost map[string]int `json:"boost"`
			} `json:"feedback"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	if b := env.Data.Feedback.Boost; b["prompts"] != 3 || b["prompts_set_changed"] != 1 || b["searches"] != 2 || b["searches_set_changed"] != 1 {
		t.Errorf("json boost %v", b)
	}
	empty := devInstall(t, true)
	pointAt(t, empty, fakeHindsight(t))
	if _, out, _ := run(t, "stats", "1"); !strings.Contains(out, "\nBoost        changed the shown set of 0 of 0 prompt recalls (-) and 0 of 0 searches (-)\n") {
		t.Errorf("empty journal:\n%s", out)
	}
}

// The Ledger line splits the shows counted into the live ones settled in
// the window and those imported from earlier history in it (by when the
// import journaled them, not when they were shown).
func TestStatsLedgerSplitsImported(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	telemetry.New(pathsOf(home).State, telemetry.Common{Session: "s1"}).Emit(telemetry.FeedbackSettle{Shows: 4, Credits: 1, Misses: 1})
	hour, old := time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(-3*24*time.Hour)
	shown := func(outcomes ...string) []feedback.Settled { // shows of earlier history
		var out []feedback.Settled
		for i, o := range outcomes {
			out = append(out, feedback.Settled{Key: "k" + itoa(i), ID: "m" + itoa(i), At: histStart, Outcome: o})
		}
		return out
	}
	journalIn(t, home,
		feedback.Event{Kind: feedback.KindImport, Source: "telemetry", TS: hour, Settled: shown(feedback.OutcomeShown, feedback.OutcomeCredit, feedback.OutcomeShown)},
		feedback.Event{Kind: feedback.KindImport, Source: "transcripts", TS: old, Settled: shown(feedback.OutcomeShown, feedback.OutcomeShown)},
		feedback.Event{Kind: feedback.KindImport, Outcome: feedback.ImportComplete, TS: hour},
	)
	if _, out, _ := run(t, "stats", "1"); !strings.Contains(out, "\nLedger       settled 4 live shows: 1 credits, 1 confident misses (33% of uncredited), 0 unknown, 0 late; imported 3 shows of earlier history (1 credits); honesty flags: none\n") {
		t.Errorf("1 day:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "7"); !strings.Contains(out, "; imported 5 shows of earlier history (1 credits);") {
		t.Errorf("7 days:\n%s", out)
	}
	if _, raw, _ := run(t, "stats", "1", "--json"); !strings.Contains(raw, `"imported": {`) || !strings.Contains(raw, `"shows": 3,`) {
		t.Errorf("json:\n%s", raw)
	}
}

// statsAt fixes the clock and points this PC, a client of a shared server
// at 127.0.0.1, at a port where nothing answers. The URL carries a fake
// secret in its query, which stats must never show.
func statsAt(t *testing.T, at time.Time) string {
	t.Helper()
	home := devInstall(t, true)
	withClock(t, at)
	cfg := pathsOf(home).ConfigFile()
	if err := os.WriteFile(cfg, []byte("[server]\nmode = \"remote\"\nurl = \"http://127.0.0.1:1/?key=fakevalue\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// stateFile writes one of <state>'s JSON files.
func stateFile(t *testing.T, home, name, body string) {
	t.Helper()
	_ = os.MkdirAll(pathsOf(home).State, 0o700)
	if err := os.WriteFile(filepath.Join(pathsOf(home).State, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// serverLine is stats' Server line.
func serverLine(t *testing.T, out string) string {
	t.Helper()
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, "Server ") {
			return l
		}
	}
	t.Fatalf("no Server line:\n%s", out)
	return ""
}

// A client whose server is silent says so from what this PC recorded, with
// no call of its own: since when, the breaker, what waits here, and the
// server's host (never its key).
func TestStatsShowsAnUnreachableServer(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	home := statsAt(t, at)
	stateFile(t, home, "reach.json", `{"last_ok":"2026-10-28T09:30:00Z","failing_since":"2026-10-28T10:00:00Z","sessions_waiting":3}`)
	stateFile(t, home, "breaker.json", `{"failures":3,"open_until":"2026-10-28T12:03:00Z"}`)
	ob := filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl")
	_ = os.MkdirAll(filepath.Dir(ob), 0o700)
	if err := os.WriteFile(ob, []byte(`{"operation_id":"a","content":"x"}`+"\n"+`{"operation_id":"b","content":"y"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "stats")
	want := "Server       127.0.0.1 silent since 2026-10-28T10:00:00Z (2h): recall paused until 2026-10-28T12:03:00Z; 2 notes queued, 3 sessions not sent yet"
	if l := serverLine(t, out); code != output.OK || l != want {
		t.Errorf("exit %d\n got %q\nwant %q", code, l, want)
	}
	if l := serverLine(t, out); strings.Contains(l, "fakevalue") || strings.Contains(l, "127.0.0.1:1") {
		t.Errorf("the line names more than the host: %q", l)
	}
	code, out, _ = run(t, "stats", "--json")
	var env struct {
		Data struct {
			Server map[string]any `json:"server"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); code != output.OK || err != nil {
		t.Fatalf("exit %d, %v\n%s", code, err, out)
	}
	if raw, _ := json.Marshal(env.Data.Server); strings.Contains(string(raw), "fakevalue") {
		t.Errorf("json server names the secret: %s", raw)
	}
	s := env.Data.Server
	if s["failing_since"] != "2026-10-28T10:00:00Z" || s["last_ok"] != "2026-10-28T09:30:00Z" || s["breaker_open_until"] != "2026-10-28T12:03:00Z" ||
		s["notes_queued"] != 2.0 || s["sessions_waiting"] != 3.0 || s["host"] != "127.0.0.1" {
		t.Errorf("json server: %v", s)
	}
	// the breaker closed again: recall goes on while the server is silent
	stateFile(t, home, "breaker.json", `{"failures":0,"open_until":"2026-10-28T11:00:00Z"}`)
	if _, out, _ := run(t, "stats"); !strings.Contains(serverLine(t, out), "(2h): recall on; 2 notes queued") {
		t.Errorf("breaker closed:\n%s", out)
	}
	if _, out, _ := run(t, "stats", "--json"); strings.Contains(out, "breaker_open_until") {
		t.Errorf("a closed breaker shows a time:\n%s", out)
	}
}

// A server that answers: when it last did, and no failure time.
func TestStatsShowsAnAnsweringServer(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	home := statsAt(t, at)
	pointAt(t, home, 1) // a PC with its own server: no host named
	stateFile(t, home, "reach.json", `{"last_ok":"2026-10-28T11:59:00Z","sessions_waiting":0}`)
	_, out, _ := run(t, "stats")
	if l := serverLine(t, out); l != "Server       answering (last answer 1m ago)" {
		t.Errorf("%q", l)
	}
	if _, out, _ := run(t, "stats", "--json"); strings.Contains(out, "failing_since") || !strings.Contains(out, `"last_ok": "2026-10-28T11:59:00Z"`) {
		t.Errorf("json:\n%s", out)
	}
}

// Nothing recorded yet (a fresh install): stats says so.
func TestStatsWithoutReachRecord(t *testing.T) {
	statsAt(t, time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
	_, out, _ := run(t, "stats")
	if l := serverLine(t, out); l != "Server       127.0.0.1 not recorded yet" {
		t.Errorf("%q", l)
	}
	if _, out, _ := run(t, "stats", "--json"); strings.Contains(out, "last_ok") || !strings.Contains(out, `"server": {`) {
		t.Errorf("json:\n%s", out)
	}
}

// Another server answers at the joined address: the line says that every
// write is held, since when, and what waits here, never "answering"
// (that server answers, the joined one does not); --json names it too.
func TestStatsShowsAHeldServer(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	home := statsAt(t, at)
	stateFile(t, home, "reach.json", `{"last_ok":"2026-10-28T08:00:00Z","sessions_waiting":3}`)
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.RemoteServer = &state.RemoteServer{ID: "a", URL: "http://127.0.0.1:1", Mismatch: "b", MismatchAt: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "stats")
	want := "Server       a different server answers at 127.0.0.1 (server b, joined a) since 2026-10-28T10:00:00Z (2h): writes held; 0 notes queued, 3 sessions not sent yet"
	if l := serverLine(t, out); code != output.OK || l != want {
		t.Errorf("exit %d\n got %q\nwant %q", code, l, want)
	}
	code, out, _ = run(t, "stats", "--json")
	var env struct {
		Data struct {
			Server map[string]any `json:"server"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); code != output.OK || err != nil {
		t.Fatalf("exit %d, %v\n%s", code, err, out)
	}
	if s := env.Data.Server; s["other_server"] != "b" || s["held_since"] != "2026-10-28T10:00:00Z" {
		t.Errorf("json server: %v", s)
	}
}
