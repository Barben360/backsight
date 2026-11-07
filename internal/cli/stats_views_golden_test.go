package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/metricsfile"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/telemetry"
)

// goldenViews are the views whose text and JSON are pinned.
var goldenViews = []string{"overview", "quality", "cost", "bank", "pipeline", "system", "sessions", "days", "defs"}

// fixedDoctor makes the health checks deterministic (the real ones quote the
// temporary directory of the run).
func fixedDoctor(t *testing.T) {
	t.Helper()
	old := doctorRun
	doctorRun = func(context.Context, doctor.Env) doctor.Report {
		return doctor.Report{Checks: []doctor.Check{
			{ID: "config", Status: doctor.OK},
			{ID: "server", Status: doctor.OK},
			{ID: "backup", Status: doctor.Warn, Fix: "backsight backup create"},
			{ID: "bank", Status: doctor.Error, Fix: "backsight doctor"},
		}}
	}
	t.Cleanup(func() { doctorRun = old })
}

// viewGolden runs a view and compares its output to testdata/<name>.golden.
func viewGolden(t *testing.T, name string, args ...string) {
	t.Helper()
	old := viewGOOS
	viewGOOS = "linux" // scope.pcs names this PC's OS: the goldens are the same on every runner
	t.Cleanup(func() { viewGOOS = old })
	code, out, errOut := run(t, args...)
	if code != output.OK {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	cliGolden(t, name, []byte(goldenPort.ReplaceAllString(out, "127.0.0.1:PORT")))
}

// Every view, as text and as JSON, on the fixed fixture (frozen clock,
// Europe/Paris, the fake server up); a diff fails. -update rewrites them.
func TestStatsViewsGolden(t *testing.T) {
	viewsFixture(t)
	fixedDoctor(t)
	for _, view := range goldenViews {
		t.Run(view, func(t *testing.T) {
			viewGolden(t, "stats-view-"+view+".golden", "stats", view, "--tz", "Europe/Paris")
			viewGolden(t, "stats-view-"+view+"-json.golden", "stats", view, "--tz", "Europe/Paris", "--json")
		})
	}
	t.Run("developer metrics", func(t *testing.T) {
		viewGolden(t, "stats-view-quality-dev.golden", "stats", "quality", "--tz", "Europe/Paris", "--dev")
	})
	t.Run("too few", func(t *testing.T) { // a day of data: nothing is judged
		viewGolden(t, "stats-view-overview-too-few.golden", "stats", "overview", "--tz", "Europe/Paris", "--days", "1")
	})
}

// browseFixture adds to viewsFixture what the browse views show: two releases
// (the older one in p2), a ledger of the memories and one declared turn of
// sess-long-1 that showed mem-long-1 at its third prompt.
func browseFixture(t *testing.T) {
	t.Helper()
	home, _ := os.UserHomeDir()
	d := pathsOf(home)
	// two releases ran: a prompt of each, and a ledger of the memories
	f, err := os.OpenFile(filepath.Join(d.TelemetryDir(), "events-2026-10.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range []string{"v0.1.17", "v0.1.18"} { // the older one ran in another project, p2
		line := telLineFor(t, goldenNow.Add(-time.Duration(50-i*40)*time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Plen: 90, MS: 300, Injected: 1}, "rel-"+v, []string{"p2", "p1"}[i])
		line = strings.Replace(line, `{`, `{"binary_version":"`+v+`",`, 1)
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	ledger := `{"schema":1,"memories":{"mem-long-1":{"shown":4,"credits":1.5,"credits_at":"2026-10-08T03:01:00Z","last_shown":"2026-10-08T03:00:01Z"},` +
		`"mem-long-2":{"shown":12,"last_shown":"2026-10-07T09:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(d.FeedbackDir(), "ledger.json"), []byte(ledger), 0o600); err != nil {
		t.Fatal(err)
	}
	at := goldenNow.Add(-9 * time.Hour)
	journalIn(t, home,
		feedback.Event{Kind: "turn", TS: at, Session: "sess-long-1", Turn: "t3", Project: "p1", N: 3},
		feedback.Event{Kind: "shows", Path: "prompt", TS: at.Add(time.Second), Session: "sess-long-1", Turn: "t3", Project: "p1",
			Items: []feedback.Item{{ID: "mem-long-1", Rank: 1}}},
		feedback.Event{Kind: "credit", TS: at.Add(time.Minute), Session: "sess-long-1", Turn: "t3", Project: "p1", Verdict: "used", IDs: []string{"mem-long-1"}},
		feedback.Event{Kind: "stop", TS: at.Add(2 * time.Minute), Session: "sess-long-1", Turn: "t3", Outcome: "declared"},
	)
}

// The browse views and the id pages, as text and as JSON, on the same
// fixture plus one declared turn of sess-long-1 that showed mem-long-1 at
// its third prompt (credited used), two releases (the older one in p2) and
// a ledger: the projects list and p1's page, the
// sessions page with its turn rail, the memories list and mem-long-1's
// page (its showings by session and prompt), a day's page and the
// releases.
func TestStatsBrowseViewsGolden(t *testing.T) {
	viewsFixture(t)
	fixedDoctor(t)
	browseFixture(t)
	for _, c := range []struct{ name, view, id string }{
		{"projects", "projects", ""}, {"project-page", "projects", "p1"},
		{"session-page", "sessions", "sess-long-1"},
		{"memories", "memories", ""}, {"memory-page", "memories", "mem-long-1"},
		{"day-page", "days", "2026-10-08"},
		{"releases", "releases", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"stats", c.view}
			if c.id != "" {
				args = append(args, c.id)
			}
			args = append(args, "--tz", "Europe/Paris")
			viewGolden(t, "stats-view-"+c.name+".golden", args...)
			viewGolden(t, "stats-view-"+c.name+"-json.golden", append(args, "--json")...)
		})
	}
}

// With the server down, its numbers read "no data" with the reason, the
// local ones stay.
func TestStatsViewsGoldenServerDown(t *testing.T) {
	viewsFixture(t)
	fixedDoctor(t)
	home, _ := os.UserHomeDir()
	pointAt(t, home, closedPort(t))
	stateFile(t, home, "reach.json", `{"last_ok":"2026-10-08T09:00:00Z","failing_since":"2026-10-08T10:00:00Z","sessions_waiting":0}`)
	viewGolden(t, "stats-view-overview-server-down.golden", "stats", "overview", "--tz", "Europe/Paris")
	viewGolden(t, "stats-view-bank-server-down.golden", "stats", "bank", "--tz", "Europe/Paris")
}

// The header and the "answering" line come from one source: they never
// disagree, with the server up or down.
func TestServerLinesAgree(t *testing.T) {
	viewsFixture(t)
	fixedDoctor(t)
	answering := func() (string, string) {
		_, out, _ := run(t, "stats", "overview", "--tz", "Europe/Paris")
		return out, lineOf(t, out, "Memory server answering")
	}
	out, line := answering()
	if !strings.Contains(line, " yes ") || strings.Contains(out, "Memory server: ") {
		t.Errorf("server up:\n%s", out)
	}
	home, _ := os.UserHomeDir()
	pointAt(t, home, closedPort(t))
	stateFile(t, home, "reach.json", `{"last_ok":"2026-10-08T09:00:00Z","failing_since":"2026-10-08T10:00:00Z","sessions_waiting":0}`)
	out, line = answering()
	if !strings.Contains(line, " no ") || !strings.Contains(out, "Memory server: unreachable") {
		t.Errorf("server down:\n%s", out)
	}
}

// The cases the fixture has no data for, built by hand: a borderline
// status, a metric judged on 28 days, a capped failure count, a partial
// cost, a snapshot and a partial day.
func TestStatsViewsGoldenEdgeCases(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	i64 := func(v int64) *int64 { return &v }
	r := result("overview", "recall.hit_rate", metrics.Metric{})
	r.Metrics = map[string]metrics.Metric{
		"recall.hit_rate":  {Value: ptr(0.09), Num: i64(9), Den: i64(100), Unit: "ratio", Status: metrics.StatusWatch, Borderline: true, CI: &[2]float64{0.05, 0.16}, Delta: ptr(-0.04)},
		"declare.coverage": {Value: ptr(0.8), Num: i64(24), Den: i64(30), Unit: "ratio", Status: metrics.StatusOK, Borderline: true, CI: &[2]float64{0.63, 0.91}},
		"used.turn_rate": {Value: ptr(0.5), Num: i64(8), Den: i64(16), Unit: "ratio", Status: metrics.StatusOK, CI: &[2]float64{0.28, 0.72}, Delta: ptr(0.1),
			Tags: []string{"judged_28d", "unreliable"}, Extra: map[string]any{"worst_case": ptr(0.4), "pending": int64(1)}},
		"extraction.tokens": {Value: ptr(52000), Tags: []string{"partial"}, Extra: map[string]any{"failed_operations": int64(2)}},
		"extraction.cost":   {Tags: []string{"partial"}, Extra: map[string]any{"currency": "USD", "price_set": false, "failed_operations": int64(2)}},
		"server.queue": {Value: ptr(3), Status: metrics.StatusWatch, Extra: map[string]any{"pending": int64(2), "processing": int64(1),
			"failed": int64(500), "failed_total": int64(731), "failed_capped": true}},
		"bank.size":     {Value: ptr(1241), Status: metrics.StatusOK, Tags: []string{"snapshot"}},
		"jobs.last_run": {Value: ptr(645_800), Status: metrics.StatusOK}, // the last tick 10 min 45.8 s ago
	}
	r.Tables.OutcomeMix = []metrics.OutcomeDay{{Day: "2026-10-01", Used: 1, Partial: true}, {Day: "2026-10-02", Used: 4}}
	cliGolden(t, "stats-view-edge-cases.golden", []byte(viewText(r, false)))
}

func TestFormatValue(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	ci := func(lo, hi float64) *[2]float64 { return &[2]float64{lo, hi} }
	def := func(id string) metrics.Def {
		d, ok := metrics.DefOf(id)
		if !ok {
			t.Fatalf("no metric %s", id)
		}
		return d
	}
	usd := map[string]any{"currency": "USD"}
	for _, c := range []struct {
		name string
		d    metrics.Def
		m    metrics.Metric
		want string
	}{
		{"ratio with interval", def("used.turn_rate"), metrics.Metric{Value: ptr(12.0 / 33), CI: ci(0.22, 0.53)}, "36 % (22–53 %)"},
		{"ratio under 1 %", def("used.turn_rate"), metrics.Metric{Value: ptr(0.004)}, "0.4 %"},
		{"ratio zero", def("used.turn_rate"), metrics.Metric{Value: ptr(0)}, "0 %"},
		{"chars under 10,000", def("context.per_session"), metrics.Metric{Value: ptr(9999)}, "9,999 chars"},
		{"chars from 10,000", def("context.per_session"), metrics.Metric{Value: ptr(12400)}, "12.4 k chars"},
		{"tokens", def("extraction.tokens"), metrics.Metric{Value: ptr(1241)}, "1,241 tokens"},
		{"ms under a second", def("recall.latency"), metrics.Metric{Value: ptr(850)}, "850 ms"},
		{"ms above a second", def("recall.latency"), metrics.Metric{Value: ptr(2850)}, "2.9 s"},
		{"money", def("extraction.cost"), metrics.Metric{Value: ptr(0.4), Extra: usd}, "0.40 USD"},
		{"money without prices", def("extraction.cost"), metrics.Metric{}, "price not set"},
		{"count", def("bank.size"), metrics.Metric{Value: ptr(1241)}, "1,241"},
		{"no value", def("bank.size"), metrics.Metric{}, "—"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := formatValue(c.d, c.m); got != c.want {
				t.Errorf("formatValue = %q, want %q", got, c.want)
			}
		})
	}
	if got := deltaText(def("used.turn_rate"), metrics.Metric{Status: metrics.StatusOK, Delta: ptr(0.0636)}); got != "+6 pts vs previous" {
		t.Errorf("delta = %q", got)
	}
}

// result is a Result of one metric, for the line tests.
func result(view string, id string, m metrics.Metric) metrics.Result {
	loc := time.UTC
	w, _ := metrics.NewWindow(time.Date(2026, 10, 8, 12, 0, 0, 0, loc), 7, time.Time{}, time.Time{}, loc, "UTC")
	return metrics.Result{View: view, Window: w, Scope: metrics.Scope{Machines: "this", Server: "srv:1"}, Metrics: map[string]metrics.Metric{id: m}}
}

// lineOf is the text line that starts with label.
func lineOf(t *testing.T, text, label string) string {
	t.Helper()
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, label) {
			return l
		}
	}
	t.Fatalf("no line %q in:\n%s", label, text)
	return ""
}

func TestViewTextLines(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	i64 := func(v int64) *int64 { return &v }
	hit := metrics.Metric{Value: ptr(12.0 / 33), Num: i64(12), Den: i64(33), Status: metrics.StatusOK, CI: &[2]float64{0.22, 0.53}, Delta: ptr(0.0636)}
	t.Run("a judged rate", func(t *testing.T) {
		text := viewText(result("overview", "recall.hit_rate", hit), false)
		re := regexp.MustCompile(`^Prompts that got memory +36 % \(22–53 %\) +\(n=33, ok, \+6 pts vs previous\)$`)
		if l := lineOf(t, text, "Prompts that got memory"); !re.MatchString(l) {
			t.Errorf("line %q", l)
		}
		if first := strings.SplitN(text, "\n", 2)[0]; first != "Is my memory working? (2026-10-01 12:00 → 2026-10-08 12:00, UTC)" {
			t.Errorf("header %q", first)
		}
		want := "Activity on this PC · memory on srv:1 · --json for every field"
		if lines := strings.Split(strings.TrimSpace(text), "\n"); lines[len(lines)-1] != want {
			t.Errorf("footer %q", lines[len(lines)-1])
		}
	})
	t.Run("too few says how many more", func(t *testing.T) {
		m := metrics.Metric{Value: ptr(0.8), Num: i64(4), Den: i64(5), Status: metrics.StatusTooFew, CI: &[2]float64{0.4, 0.96}}
		l := lineOf(t, viewText(result("overview", "recall.hit_rate", m), false), "Prompts that got memory")
		if !strings.Contains(l, "4 of 5") || !strings.Contains(l, "n=5, too few, 15 more needed") || strings.Contains(l, "80 %") {
			t.Errorf("line %q", l)
		}
	})
	t.Run("borderline never contradicts the status", func(t *testing.T) {
		for _, c := range []struct {
			st   metrics.Status
			want string
		}{{metrics.StatusWatch, "watch — close to the line"}, {metrics.StatusOK, "ok — close to the line"}} {
			m := hit
			m.Status, m.Borderline = c.st, true
			if l := lineOf(t, viewText(result("overview", "recall.hit_rate", m), false), "Prompts that got memory"); !strings.Contains(l, "n=33, "+c.want+",") {
				t.Errorf("%s: line %q", c.st, l)
			}
		}
	})
	t.Run("no data gives its reason", func(t *testing.T) {
		m := metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonServerDown}
		l := lineOf(t, viewText(result("bank", "bank.size", m), false), "Memories")
		if !strings.Contains(l, "no data: memory server not answering") || strings.Contains(l, "0") {
			t.Errorf("line %q", l)
		}
	})
	t.Run("a snapshot has no history", func(t *testing.T) {
		m := metrics.Metric{Value: ptr(41), Status: metrics.StatusOK, Tags: []string{"snapshot"}}
		l := lineOf(t, viewText(result("bank", "bank.size", m), false), "Memories")
		if !strings.Contains(l, "no history") || strings.Contains(l, "no change") {
			t.Errorf("line %q", l)
		}
	})
	t.Run("a zero change against a window is no change", func(t *testing.T) {
		m := metrics.Metric{Value: ptr(0), Status: metrics.StatusOK, Delta: ptr(0)}
		if l := lineOf(t, viewText(result("pipeline", "hooks.failures", m), false), "Steps that crashed"); !strings.Contains(l, "no change vs previous") {
			t.Errorf("line %q", l)
		}
	})
	t.Run("judged on 28 days", func(t *testing.T) {
		m := hit
		m.Tags = []string{"judged_28d"}
		if l := lineOf(t, viewText(result("overview", "used.turn_rate", m), false), "Turns where Claude used memory"); !strings.Contains(l, "judged on the last 28 days") || !strings.Contains(l, "vs previous 28 days") {
			t.Errorf("line %q", l)
		}
	})
	t.Run("unreliable", func(t *testing.T) {
		m := hit
		m.Tags = []string{"unreliable"}
		if l := lineOf(t, viewText(result("overview", "used.turn_rate", m), false), "Turns where Claude used memory"); !strings.Contains(l, ", unreliable,") {
			t.Errorf("line %q", l)
		}
	})
	t.Run("a capped failure count is a lower bound", func(t *testing.T) {
		m := metrics.Metric{Value: ptr(0), Status: metrics.StatusWatch, Extra: map[string]any{
			"pending": int64(0), "processing": int64(0), "failed": int64(500), "failed_total": int64(731), "failed_capped": true}}
		text := viewText(result("pipeline", "server.queue", m), false)
		if !strings.Contains(text, "failed in the last 7 days 500+ (a lower bound") {
			t.Errorf("text:\n%s", text)
		}
		m.Extra["failed_capped"] = false
		if text := viewText(result("pipeline", "server.queue", m), false); strings.Contains(text, "500+") {
			t.Errorf("an exact count shown as a bound:\n%s", text)
		}
	})
	t.Run("a partial cost says operations could not be read", func(t *testing.T) {
		m := metrics.Metric{Value: ptr(0.4), Tags: []string{"partial"}, Status: metrics.StatusOK,
			Extra: map[string]any{"currency": "USD", "failed_operations": int64(2)}}
		text := viewText(result("cost", "extraction.cost", m), false)
		if !strings.Contains(text, "0.40 USD") || !strings.Contains(text, "2 server tasks could not be read, so this is a lower bound") {
			t.Errorf("text:\n%s", text)
		}
	})
	t.Run("partial days are not dips", func(t *testing.T) {
		r := result("quality", "recall.hit_rate", hit)
		r.Tables.OutcomeMix = []metrics.OutcomeDay{{Day: "2026-10-01", Used: 1, Partial: true}, {Day: "2026-10-02", Used: 4}}
		text := viewText(r, false)
		if !strings.Contains(lineOf(t, text, "2026-10-01"), "partial day") || strings.Contains(lineOf(t, text, "2026-10-02"), "partial") {
			t.Errorf("text:\n%s", text)
		}
	})
	t.Run("developer metrics only with --dev", func(t *testing.T) {
		m := metrics.Metric{Value: ptr(0.9), Den: i64(40), Status: metrics.StatusOK}
		r := result("quality", "used.rubber_stamp", m)
		if text := viewText(r, false); strings.Contains(text, "Turns where every memory was marked used") {
			t.Errorf("developer metric without --dev:\n%s", text)
		}
		if text := viewText(r, true); !strings.Contains(text, "Turns where every memory was marked used") {
			t.Errorf("developer metric missing with --dev:\n%s", text)
		}
	})
	t.Run("the verdict, the attention list and its commands", func(t *testing.T) {
		r := result("overview", "recall.hit_rate", hit)
		r.Verdict = &metrics.Verdict{WindowDays: 7, Axes: map[string]metrics.Axis{
			"found":   {State: metrics.AxisOK},
			"right":   {State: metrics.AxisCantJudge, Reasons: []metrics.Reason{{Text: "Can't judge yet: Claude said what it used on only 64 % of turns."}, {Text: "second"}}},
			"flowing": {State: metrics.AxisAttention, Reasons: []metrics.Reason{{Text: "Health check bank failed."}}},
		}}
		r.Attention = []metrics.Item{{Text: "Health check bank failed.", Command: "backsight doctor"}, {Text: "Two."}}
		r.AttentionMore = 3
		text := viewText(r, false)
		for _, want := range []string{
			"Found    ok\n", "Right    can't judge — Can't judge yet: Claude said what it used on only 64 % of turns.\n",
			"Flowing  attention — Health check bank failed.\n", "1. Health check bank failed.\n   Run: backsight doctor\n", "2. Two.\n", "3 more: backsight stats system",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q in:\n%s", want, text)
			}
		}
		if strings.Contains(text, "second") {
			t.Errorf("only an axis's first reason is printed:\n%s", text)
		}
	})
	t.Run("Right judged on 28 days gives its cause, then the note", func(t *testing.T) {
		r := result("overview", "recall.hit_rate", hit)
		r.Verdict = &metrics.Verdict{WindowDays: 7, Axes: map[string]metrics.Axis{
			"right": {State: metrics.AxisAttention, WindowDays: 28, Reasons: []metrics.Reason{
				{Metric: "verdict.wrong_rate", Severity: metrics.SeverityBad, Text: "10 memories reported wrong or out of date in the last 28 days."},
				{Metric: "used.turn_rate", Severity: metrics.SeverityInfo, Text: "Judged on the last 28 days: too few turns in 7."}}},
		}}
		want := "Right    attention — 10 memories reported wrong or out of date in the last 28 days. Judged on the last 28 days: too few turns in 7.\n"
		if text := viewText(r, false); !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	})
	t.Run("ids and projects are cut", func(t *testing.T) {
		r := result("sessions", "recall.hit_rate", hit)
		r.Tables.Sessions = []metrics.SessionRow{{ID: "abcdefghijklmnop", Project: strings.Repeat("p", 30), Start: r.Window.Since}}
		text := viewText(r, false)
		if !strings.Contains(text, "abcdefgh ") || strings.Contains(text, "abcdefghi") || !strings.Contains(text, strings.Repeat("p", 23)+"…") || strings.Contains(text, strings.Repeat("p", 24)) {
			t.Errorf("text:\n%s", text)
		}
	})
	t.Run("control characters never reach the terminal", func(t *testing.T) {
		r := result("sessions", "recall.hit_rate", hit)
		r.Scope.Project = "evil\x1b[31mred\x07"
		r.Tables.Sessions = []metrics.SessionRow{{ID: "\x1b[2Jabcdefgh", Project: "p\x1b[31m\u009b1", Start: r.Window.Since}}
		r.Text = &metrics.MemoryText{ID: "m1", Text: "line one\n\x1b[31mline\ttwo\x1b[0m"}
		text := viewText(r, false)
		if strings.ContainsAny(text, "\x1b\x07\u009b") {
			t.Errorf("control characters printed:\n%q", text)
		}
		for _, want := range []string{"Project: evil[31mred\n", "[2Jabcde ", "p[31m1", "line one\n[31mline\ttwo[0m\n"} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q in:\n%q", want, text)
			}
		}
	})
	t.Run("a memory's text the server did not give says why", func(t *testing.T) {
		r := result("memories", "recall.hit_rate", hit)
		r.Text = &metrics.MemoryText{ID: "m1", Reason: metrics.ReasonServerDown}
		if text := viewText(r, false); !strings.Contains(text, "Memory m1: its text is not available (memory server not answering).") {
			t.Errorf("text:\n%s", text)
		}
	})
	t.Run("the server notice", func(t *testing.T) {
		r := result("bank", "bank.size", metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonServerDown})
		r.Server = &metrics.ServerState{OK: false, Error: "unreachable"}
		if text := viewText(r, false); !strings.Contains(text, "Memory server: unreachable. The numbers that come from it are missing.") {
			t.Errorf("text:\n%s", text)
		}
	})
}

// No line of any view carries a decision, block or spec number, nor the old
// word for the memory.
func TestViewTextIsPublic(t *testing.T) {
	viewsFixture(t)
	fixedDoctor(t)
	banned := regexp.MustCompile(`(?i)\b(bank|block \d|decision|0079|spec)\b`)
	for _, view := range goldenViews {
		_, out, _ := run(t, "stats", view, "--dev", "--tz", "Europe/Paris")
		for _, l := range strings.Split(out, "\n") {
			if view == "defs" { // the first column holds the metric ids, the argument of `stats defs <id>`
				l = strings.TrimSpace(strings.TrimPrefix(l, strings.Fields(l + " x")[0]))
			}
			if banned.MatchString(l) {
				t.Errorf("%s: %q", view, l)
			}
		}
	}
}

// With no memory server set up (no config file at all), the server's rows
// read "no memory server set up" with no status of their own, never "not
// answering", and their JSON reason is not_configured.
func TestStatsViewsWithoutAServer(t *testing.T) {
	viewsFixture(t)
	fixedDoctor(t)
	d, _, err := machine()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(d.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"overview", "bank", "pipeline"} {
		_, out, _ := run(t, "stats", view, "--tz", "Europe/Paris")
		if strings.Contains(out, "not answering") {
			t.Errorf("%s says the server is not answering:\n%s", view, out)
		}
		rows := map[string][]string{"overview": {"Memories", "Memory server answering"}, "bank": {"Memories"}, "pipeline": {"Waiting on the server", "Memory server answering"}}[view]
		for _, label := range rows {
			if l := lineOf(t, out, label+"  "); !strings.HasSuffix(l, "(no memory server set up)") {
				t.Errorf("%s: %q, want only (no memory server set up)", view, l)
			}
		}
		if !strings.Contains(out, "Memory server: none set up.") {
			t.Errorf("%s: no header saying no server is set up:\n%s", view, out)
		}
		v := runView(t, "stats", view)
		ms, _ := at(v.env.Data, "metrics").(map[string]any)
		for id, m := range ms {
			def, _ := metrics.DefOf(id)
			if (def.Scope != "server" || def.Source != "server") && id != "server.reach" { // the scan record is local
				continue
			}
			if at(m, "status") != "no_data" || at(m, "reason") != "not_configured" {
				t.Errorf("%s: %s = %v/%v, want no_data/not_configured", view, id, at(m, "status"), at(m, "reason"))
			}
		}
		if at(v.env.Data, "server", "error") != "not_configured" {
			t.Errorf("%s: server = %v", view, at(v.env.Data, "server"))
		}
	}
}

// blockFixture adds to viewsFixture the records of the daily collection
// and the telemetry that go with them: sess-long-1 and sess-long-2 were sent
// (30 minutes after their last prompt), the first was turned into two
// memories 10 minutes later (mem-long-1, from it, shown again an hour and
// twenty minutes after; mem-long-2 is a note), the second's operation
// failed; a scan found one possible secret, the checks ran on three days
// (the backup check warns from the second, the memory check fails on the
// third) and the settings changed once.
func blockFixture(t *testing.T, home string) {
	t.Helper()
	d := pathsOf(home)
	hr := func(h float64) time.Time { return goldenNow.Add(-time.Duration(h * float64(time.Hour))) }
	var b strings.Builder
	for _, s := range []struct {
		id string
		at time.Time
	}{{"sess-long-1", hr(10.5)}, {"sess-long-2", hr(11.5)}} {
		b.WriteString(telLineFor(t, s.at, telemetry.Retain{Document: "conversation:" + s.id, Items: 2, Status: "ok", MS: 300}, s.id, "p1"))
	}
	// two sessions sent two days ago: one the server never made memories of
	// (no operation), one whose operation failed twice
	for _, id := range []string{"sess-never-1", "sess-never-2"} {
		b.WriteString(telLineFor(t, hr(52), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, MS: 500, Injected: 1}, id, "p1"))
		b.WriteString(telLineFor(t, hr(51.5), telemetry.Retain{Document: "conversation:" + id, Items: 2, Status: "ok", MS: 300}, id, "p1"))
	}
	for _, c := range []struct {
		at    time.Time
		sha   string
		score float64
	}{{hr(60), strings.Repeat("a", 64), 0.5}, {hr(20), strings.Repeat("b", 64), 0.6}} {
		b.WriteString(telLineFor(t, c.at, telemetry.Config{SHA256: c.sha, Values: map[string]float64{"recall.min_score": c.score}}, "", ""))
	}
	f, err := os.OpenFile(filepath.Join(d.TelemetryDir(), "events-2026-10.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	rec := func(ts time.Time, recs ...metricsfile.Record) {
		w := metricsfile.New(d.MetricsDir(), "v0.1.17")
		w.Now = func() time.Time { return ts }
		if err := w.Append(recs...); err != nil {
			t.Fatal(err)
		}
	}
	units := int64(2)
	rec(hr(8),
		metricsfile.MemoryDoc{Memory: "mem-long-1", Outcome: "found", DocKind: "session", Document: "conversation:sess-long-1", Session: "sess-long-1", CreatedDay: "2026-10-08", FactType: "world"},
		metricsfile.MemoryDoc{Memory: "mem-long-2", Outcome: "found", DocKind: "note", CreatedDay: "2026-10-08", FactType: "world"},
		metricsfile.Listed{Day: "2026-10-08", N: 2},
		metricsfile.DocOp{Operation: "op-1", Document: "conversation:sess-long-1", Session: "sess-long-1", Status: "completed",
			CreatedAt: hr(10.49), UpdatedAt: hr(10.5 - 10.0/60), Units: &units},
		metricsfile.DocOp{Operation: "op-2", Document: "conversation:sess-long-2", Session: "sess-long-2", Status: "failed",
			CreatedAt: hr(11.49), UpdatedAt: hr(11.4), Retries: 3})
	// the operations record starts before those sends (a session of an
	// earlier day), so a send without a memory is never_extracted, not "before the record"
	rec(hr(60), metricsfile.DocOp{Operation: "op-0", Document: "conversation:sess-old", Session: "sess-old", Status: "completed",
		CreatedAt: hr(60.1), UpdatedAt: hr(60.05), Units: &units})
	rec(hr(51), metricsfile.DocOp{Operation: "op-n2", Document: "conversation:sess-never-2", Session: "sess-never-2", Status: "failed",
		CreatedAt: hr(51.49), UpdatedAt: hr(51.4), Retries: 2})
	// the yield of one complete week (31 August to 6 September): 21 new
	// memories a day record gives, three of the four listed on 1 September
	// used within 28 days, and the tokens the server spent those days
	for day := 0; day < 7; day++ {
		b := time.Date(2026, 8, 31+day, 0, 0, 0, 0, time.UTC)
		rec(b.Add(25*time.Hour), metricsfile.ServerDay{Bucket: b, New: &metricsfile.KindCounts{World: 3},
			LLM: map[string]metricsfile.LLMDay{"all": {Input: 1000, Output: 500, Requests: 2}, "retain": {Input: 1000, Output: 500, Requests: 2}}})
	}
	rec(time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC),
		metricsfile.MemoryDoc{Memory: "mem-y1", Outcome: "found", DocKind: "note", CreatedDay: "2026-09-01", FactType: "world"},
		metricsfile.MemoryDoc{Memory: "mem-y2", Outcome: "found", DocKind: "note", CreatedDay: "2026-09-01", FactType: "world"},
		metricsfile.MemoryDoc{Memory: "mem-y3", Outcome: "found", DocKind: "note", CreatedDay: "2026-09-01", FactType: "world"},
		metricsfile.MemoryDoc{Memory: "mem-y4", Outcome: "found", DocKind: "note", CreatedDay: "2026-09-01", FactType: "world"},
		metricsfile.Listed{Day: "2026-09-01", N: 4})
	journalIn(t, home, feedback.Event{Kind: "credit", TS: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), Session: "sess-y", Turn: "ty", Project: "p1",
		Verdict: "used", IDs: []string{"mem-y1", "mem-y2", "mem-y3"}})
	rec(hr(48), metricsfile.Scan{Count: 1, Live: 1, Forgotten: 0, Pages: 3, MS: 1200})
	rec(hr(50), metricsfile.DoctorRun{Checks: map[string]string{"config": "ok", "backup": "ok", "memory": "ok"}})
	rec(hr(26), metricsfile.DoctorRun{Checks: map[string]string{"config": "ok", "backup": "warn", "memory": "ok"}})
	rec(hr(2), metricsfile.DoctorRun{Checks: map[string]string{"config": "ok", "backup": "warn", "memory": "error"}})
}

// blockViews is every view whose text the block's fixture pins.
var blockViews = []struct {
	name string
	args []string
}{
	{"overview", []string{"stats", "overview"}},
	{"quality-dev", []string{"stats", "quality", "--dev"}},
	{"bank-dev", []string{"stats", "bank", "--dev"}},
	{"pipeline", []string{"stats", "pipeline"}},
	{"system", []string{"stats", "system"}},
	{"days", []string{"stats", "days"}},
	{"day-page", []string{"stats", "days", "2026-10-08"}},
	{"memory-page", []string{"stats", "memories", "mem-long-1"}},
	{"cost", []string{"stats", "cost"}},
	{"cost-yield", []string{"stats", "cost", "--days", "60"}},
	{"sessions", []string{"stats", "sessions"}},
	{"sessions-never", []string{"stats", "sessions", "--has", "never_extracted"}},
	{"releases", []string{"stats", "releases"}},
	{"releases-compare", []string{"stats", "releases", "--compare", "v0.1.17,v0.1.18"}},
}

// blockSetup is the extended fixture: the views' fixture, the browse one and
// the block's records.
func blockSetup(t *testing.T) {
	t.Helper()
	viewsFixture(t)
	fixedDoctor(t)
	browseFixture(t)
	home, _ := os.UserHomeDir()
	blockFixture(t, home)
}

// The text of every view on the extended fixture, pinned (-update rewrites).
func TestStatsBlockViewsGolden(t *testing.T) {
	blockSetup(t)
	for _, v := range blockViews {
		t.Run(v.name, func(t *testing.T) {
			viewGolden(t, "stats-block-"+v.name+".golden", append(v.args, "--tz", "Europe/Paris")...)
		})
	}
}

// squashed is the text's lines with every run of blanks one space, so a
// line is compared by its words and not by the width of its columns.
func squashed(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		out = append(out, strings.Join(strings.Fields(l), " "))
	}
	return out
}

// wantLines fails for each line of want that text has no line equal to.
func wantLines(t *testing.T, what, text string, want ...string) {
	t.Helper()
	have := squashed(text)
	for _, w := range want {
		if !slices.Contains(have, w) {
			t.Errorf("%s: no line %q in:\n%s", what, w, text)
		}
	}
}

// The numbers of the extended fixture, worked out by hand, as the text
// says them: the funnel and the hops of time to recall (sess-long-1 sent 30
// minutes after its last prompt, turned into memories 10 minutes later and
// recalled 1 h 20 min after that; sess-long-2 sent 30 minutes after its last
// prompt, its operation failed, so it is still counted on the server; the
// other four sessions never sent), the scan, the checks' history, the
// calendar, the memory that helped, where it came from and what changed.
func TestStatsBlockText(t *testing.T) {
	blockSetup(t)
	text := func(args ...string) string {
		t.Helper()
		code, out, errOut := run(t, append(append([]string{"stats"}, args...), "--tz", "Europe/Paris")...)
		if code != output.OK {
			t.Fatalf("%v: exit %d: %s %s", args, code, out, errOut)
		}
		return out
	}

	wantLines(t, "pipeline", text("pipeline"),
		"session ended 8 → sent 4 → turned into memories 1 → shown again 1",
		"not sent 4, never turned into memories 2, still on the server 1, shown, origin not known yet 1",
		"last turn to sent: 4 sessions, too few for a typical time",
		// the two sessions sent two days ago: none made memories (one no operation, one a failed one)
		"Sessions sent that gave no memories",
		"Session Project Sent What happened Retries",
		"sess-nev p1 2026-10-06 10:30 no trace of it on the server 0",
		"sess-nev p1 2026-10-06 10:30 the server failed on it 2",
		"sent to turned into memories: 1 session, too few for a typical time",
		"turned into memories to shown again: 1 session, too few for a typical time",
		"the whole way: 1 session, too few for a typical time",
	)

	// the same two sessions, filtered
	if sn := text("sessions", "--has", "never_extracted"); strings.Count(sn, "sess-nev") != 2 || strings.Contains(sn, "s1 ") {
		t.Errorf("sessions --has never_extracted:\n%s", sn)
	}
	// yield of the one complete week (3 of the 21 new memories used, 10,500
	// tokens spent, 3,500 a useful memory), and where the record starts
	wantLines(t, "cost", text("cost", "--days", "60"),
		"week of 2026-08-31: 3 of 21 (14 %, 5 to 35 %), 10,500 tokens, 3,500 tokens per useful memory",
		"older days come from the days this PC recorded, from 2026-08-31",
		"week of 2026-09-07: not complete yet",
		"week of 2026-08-24: the new memories of that week were not recorded",
	)

	wantLines(t, "system", text("system"),
		"Possible secrets in memory 1 (watch)",
		"last scan 2026-10-06 14:00, 2 days ago (3 pages read)",
		"3 runs in the window, 5 days without a run",
		"Check Now Days ok Days with a warning Days with a problem Last change",
		"backup warning 1 2 0 2026-10-07, ok → warning",
		"memory problem 2 0 1 2026-10-08, ok → problem",
		"settings ok 3 0 0 —",
	)

	// the calendar line agrees with the rows the JSON carries, and with the
	// days the fixture has prompts on (6, 7 and 8 October, in Paris)
	days := text("days")
	var env struct {
		Data struct {
			Tables struct {
				Calendar []struct {
					Recorded bool `json:"recorded"`
				} `json:"calendar"`
			} `json:"tables"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(runView(t, "stats", "days", "--tz", "Europe/Paris").raw), &env); err != nil {
		t.Fatal(err)
	}
	rec := 0
	for _, d := range env.Data.Tables.Calendar {
		if d.Recorded {
			rec++
		}
	}
	if rec != 3 || len(env.Data.Tables.Calendar) != 396 {
		t.Fatalf("calendar: %d recorded of %d, want 3 of 396 (Monday 2025-09-08 to 2026-10-08)", rec, len(env.Data.Tables.Calendar))
	}
	wantLines(t, "days", days, "calendar: 3 days recorded of 396")

	wantLines(t, "overview", text("overview"),
		"Helped most",
		"Memory Project Kind Used Shown",
		idText("mem-long-1")+" p1 facts about you and your work 1 2",
	)

	wantLines(t, "memory page", text("memories", "mem-long-1"),
		"Came from a session on this PC (sess-lon, p1)",
		"Session ended 2026-10-08 03:00, sent 2026-10-08 03:30, turned into memories 2026-10-08 03:40, first shown 2026-10-08 05:00",
	)

	rel := text("releases")
	wantLines(t, "releases", rel, "What changed", "When What")
	if !strings.Contains(rel, "recall.min_score 0.5 → 0.6") || !strings.Contains(rel, "new version v0.1.18") {
		t.Errorf("releases: no marker for the version and the setting:\n%s", rel)
	}
	// the markers of a day are on its page: the setting changed on the 7th
	// (18:00 in Paris), the version first seen on the 8th (04:00)
	if p := text("days", "2026-10-07"); !strings.Contains(p, "recall.min_score 0.5 → 0.6") || strings.Contains(p, "new version") {
		t.Errorf("the page of the 7th:\n%s", p)
	}
	if p := text("days", "2026-10-08"); !strings.Contains(p, "new version v0.1.18 (after v0.1.17)") || strings.Contains(p, "recall.min_score") {
		t.Errorf("the page of the 8th:\n%s", p)
	}

	cmp := text("releases", "--compare", "v0.1.17,v0.1.18")
	if !strings.Contains(cmp, "Comparison of v0.1.17 and v0.1.18 on Turns where Claude used memory") ||
		!strings.Contains(cmp, "too few to compare: v0.1.17 has 0 active days and 0 turns in its best group; not enough active days") {
		t.Errorf("a comparison with too few days:\n%s", cmp)
	}
}

// A comparison that reads, and the shapes the fixture has no data for, built
// by hand: the lines the text gives for a matched difference, an unmatched
// one that disagrees (Simpson's paradox), the groups, the settings, the
// weeks of yield, an old scan and the origins of a memory.
func TestStatsBlockTextShapes(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	i := func(v int64) *int64 { return &v }
	r := result("releases", "used.turn_rate", metrics.Metric{})
	r.Tables.Compare = &metrics.Comparison{
		A: "v0.1.16-m7", B: "v0.1.17-m7", Measure: "used.turn_rate", Reading: "differs",
		Diff: f(0.114625), CI: &[2]float64{0.024783, 0.204466}, PooledA: f(0.489723), PooledB: f(0.604348),
		RawA: metrics.Rate{Num: 187, Den: 326}, RawB: metrics.Rate{Num: 154, Den: 295}, RawDisagrees: true,
		SessionsA: 120, SessionsB: 85,
		Strata: []metrics.StratumRow{
			{Project: "backsight", Path: "prompt", A: metrics.StratumSide{Num: 140, Den: 200, Days: 21}, B: metrics.StratumSide{Num: 46, Den: 60, Days: 9},
				Weight: f(46.1538), Diff: f(0.066667), CI: &[2]float64{-0.058021, 0.191355}, Reading: "no_clear_difference", Eligible: true},
			{Project: "project-b", Path: "unknown", A: metrics.StratumSide{Num: 9, Den: 20, Days: 4}, B: metrics.StratumSide{Num: 15, Den: 33, Days: 8}, Why: "days"},
		},
		SettingsDiffer: []metrics.SettingDiff{{Key: "recall.min_score", A: f(0.5), B: f(0.6), ChangedAt: &goldenNow}},
	}
	out := viewText(r, false)
	wantLines(t, "comparison", out,
		"Comparison of v0.1.16-m7 and v0.1.17-m7 on Turns where Claude used memory",
		"matched difference +11 pts (+2 to +20), differs",
		"unmatched −5 pts (57 % → 52 %): read the matched figure",
		"120 sessions in v0.1.16-m7, 85 in v0.1.17-m7",
		"Project Shown How v0.1.16-m7 did How v0.1.17-m7 did Difference",
		"backsight added to the prompt 140 of 200 (21 days) 46 of 60 (9 days) +7 pts (−6 to +19), no clear difference",
		"project-b not shown to Claude itself 9 of 20 (4 days) 15 of 33 (8 days) not compared: not enough active days",
		"Settings that differ: recall.min_score 0.5 → 0.6 (changed 2026-10-08 12:00)",
	)
	// the unmatched line appears only when it disagrees
	r.Tables.Compare.RawDisagrees = false
	if strings.Contains(viewText(r, false), "unmatched") {
		t.Errorf("an unmatched line without a disagreement")
	}
	r.Tables.Compare.Reading = "no_clear_difference"
	r.Tables.Compare.CI = &[2]float64{-0.02, 0.2}
	r.Tables.Compare.SettingsDiffer, r.Tables.Compare.SettingsUnknown = nil, true
	wantLines(t, "no clear difference", viewText(r, false),
		"matched difference +11 pts (−2 to +20), no clear difference",
		"Settings of a version are not known: nothing was recorded before its first turn.")

	// time to recall with everything the funnel can say, a yield of four weeks
	// (one short, one not complete, one not recorded), an old scan
	y := result("cost", "memory.yield", metrics.Metric{Extra: map[string]any{"withheld": "record_catching_up", "unresolved": int64(3)}, Series: []metrics.Point{
		{Week: "2026-09-21", Complete: &[]bool{true}[0], Num: i(12), Den: i(40), Value: f(0.3), CI: &[2]float64{0.18, 0.45}, Tokens: i(52000), TokensPerUsed: f(4333.33)},
		{Week: "2026-09-28", Complete: &[]bool{true}[0], Num: i(3), Den: i(15)},
		{Week: "2026-10-05", Complete: &[]bool{false}[0]},
		{Week: "2026-09-14", Complete: &[]bool{true}[0]},
	}})
	y.Metrics["memory.yield"] = metrics.Metric{Value: f(0.3), Num: i(12), Den: i(40), Status: metrics.StatusOK, Extra: y.Metrics["memory.yield"].Extra, Series: y.Metrics["memory.yield"].Series}
	wantLines(t, "yield", viewText(y, false),
		"week of 2026-09-21: 12 of 40 (30 %, 18 to 45 %), 52,000 tokens, 4,333 tokens per useful memory",
		"week of 2026-09-28: 3 of 15, too few for a share",
		"week of 2026-10-05: not complete yet",
		"week of 2026-09-14: the new memories of that week were not recorded",
		"shares withheld: the list of which memories are new is still catching up (3 not placed yet)",
	)

	// a typical time from 20 sessions, the slowest 1 in 20 from 100
	ms := func(min float64) *float64 { return f(min * 60000) }
	ttr := result("pipeline", "pipeline.time_to_recall", metrics.Metric{Value: ms(125), Num: i(20), Den: i(31), Status: metrics.StatusOK, Extra: map[string]any{
		"funnel": map[string]int64{"ended": 40, "sent": 31, "extracted": 28, "recalled": 20, "waiting_or_failed": 9, "never": 2, "on_server": 1, "not_recalled": 8,
			"unresolved": 0, "before_record": 3, "gone": 0},
		"hops": []metrics.Hop{{Name: "last_to_sent", P50: ms(0.5), N: 31}, {Name: "sent_to_extracted", P50: ms(7), P95: ms(95), N: 120},
			{Name: "extracted_to_recalled", P50: ms(60 * 30), P95: ms(60 * 24 * 3), N: 20}, {Name: "whole_way", P50: ms(125), N: 20}},
	}})
	wantLines(t, "time to recall", viewText(ttr, false),
		"session ended 40 → sent 31 → turned into memories 28 → shown again 20",
		"not sent 9, never turned into memories 2, still on the server 1, not shown again 8, sent before this was recorded 3",
		"last turn to sent: typical under 1 min (31 sessions)",
		"sent to turned into memories: typical 7 min, slowest 1 in 20: 95 min (120 sessions)",
		"turned into memories to shown again: typical 30 h, slowest 1 in 20: 3 days (20 sessions)",
		"the whole way: typical 2 h (20 sessions)",
	)

	s := result("system", "secrets.suspects", metrics.Metric{Value: f(2), Status: metrics.StatusWatch, Tags: []string{"old_scan"},
		Extra: map[string]any{"age_days": int64(45), "pages": int64(3)}})
	wantLines(t, "old scan", viewText(s, false), "Possible secrets in memory 2 (watch, the last scan is more than 30 days old)")

	// where a memory came from, in each state
	for state, want := range map[string]string{
		metrics.OriginOtherPC:   "Came from a session on another PC, made 2026-09-30",
		metrics.OriginNoSession: "Came from a note or an import",
		metrics.OriginUnknown:   "Came from a source not known yet",
	} {
		created := "2026-09-30"
		o := &metrics.Origin{State: state, Created: &created}
		m := result("memories", "used.turn_rate", metrics.Metric{})
		m.Tables.Memory = &metrics.MemoryPage{Origin: o}
		if state != metrics.OriginOtherPC {
			o.Created = nil
		}
		wantLines(t, "origin "+state, viewText(m, false), want)
	}
}

// The two other PCs of the all-PCs fixtures.
const (
	otherPCFresh = "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	otherPCStale = "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// otherDays are the days of one other PC: the same figures each day, so the
// sums are easy to check by hand. A day holds 40 prompts, 20 of 36 got
// memory, 6 of 12 turns used it, 10 of 12 declared, 1 of 12 wrong, 4,600
// characters over 2 sessions, 80 recalls timed (2 under 100 ms, 10 under 250,
// 20 under 500, 30 under a second, 16 under 2 s, 2 under 2.5 s) and 9 of 10
// sends that worked.
func otherDays(from, to string, version string) []machines.Day {
	f := machines.Figures{Prompts: 40, Hit: machines.Pair{20, 36}, Used: machines.Pair{6, 12}, Coverage: machines.Pair{10, 12},
		Wrong: machines.Pair{1, 12}, Context: machines.Pair{4600, 2}, Latency: machines.Latency{0, 2, 10, 20, 30, 16, 2, 0, 0}}
	var out []machines.Day
	for d, _ := time.Parse("2006-01-02", from); d.Format("2006-01-02") <= to; d = d.AddDate(0, 0, 1) {
		out = append(out, machines.Day{Day: d.Format("2006-01-02"), Figures: f, Flow: machines.Pair{9, 10},
			Versions: map[string]machines.Figures{version: f}})
	}
	return out
}

// fixturePC is the document of another PC last sent at at, covering from to
// until; it runs the release version.
func fixturePC(id, os, first string, at time.Time, from, until, version string) machines.Doc {
	return machines.Doc{Machine: id, At: at, OS: os, FirstSeen: first, BinaryVersion: version, Until: until,
		Days: otherDays(from, until, version)}
}

// otherPCsFixture writes the cache of two other PCs of the goldens' clock:
// one that sent 3 hours ago (days 3 to 7 October, darwin, release v0.1.18)
// and one that went quiet 3 days ago (days 2 to 4 October, windows, v0.1.17),
// and one document set aside.
func otherPCsFixture(t *testing.T) {
	t.Helper()
	d, _, err := machine()
	if err != nil {
		t.Fatal(err)
	}
	fresh, stale := fixturePC(otherPCFresh, "darwin", "2026-09-20", goldenNow.Add(-3*time.Hour), "2026-10-03", "2026-10-07", "v0.1.18"),
		fixturePC(otherPCStale, "windows", "2026-09-25", goldenNow.Add(-72*time.Hour), "2026-10-02", "2026-10-04", "v0.1.17")
	c := machines.Cache{FetchedAt: goldenNow.Add(-time.Hour), Ignored: []machines.Ignored{{Machine: "deadbeef", Reason: machines.UnknownVersion}}}
	for _, doc := range []machines.Doc{fresh, stale} {
		raw, err := doc.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		c.Docs = append(c.Docs, machines.CachedDoc{Name: machines.DocPrefix + doc.Machine, Raw: raw})
	}
	if err := os.MkdirAll(d.MetricsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := machines.SaveCache(d.MetricsDir(), c); err != nil {
		t.Fatal(err)
	}
	// this PC has a name and has published once
	if err := os.WriteFile(filepath.Join(d.State, "machine_id"), []byte("3f9c2a17-1b2c-4d3e-8f40-0123456789ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := machines.SavePublished(d.MetricsDir(), machines.Published{At: goldenNow.Add(-5 * time.Hour), Until: "2026-10-07", FirstSeen: "2026-09-15", Bytes: 900}); err != nil {
		t.Fatal(err)
	}
}

// allPCsViews are the views pinned with --machines all.
var allPCsViews = []struct {
	name string
	args []string
}{
	{"overview", []string{"stats", "overview", "--machines", "all"}},
	{"overview-filter", []string{"stats", "overview", "--machines", "all", "--project", "p1"}},
	{"quality", []string{"stats", "quality", "--machines", "all"}},
	{"cost", []string{"stats", "cost", "--machines", "all"}},
	{"days", []string{"stats", "days", "--machines", "all"}},
	{"day-page", []string{"stats", "days", "2026-10-07", "--machines", "all"}},
	{"releases", []string{"stats", "releases", "--machines", "all"}},
	{"system", []string{"stats", "system", "--machines", "all"}},
}

// With --machines all the text names the PCs counted, the day the other PCs'
// figures reach, the documents set aside and, under a project filter, that
// the other PCs are not filtered; the speed is a range read from the
// buckets, the context an average over the sessions of every PC, and
// System lists the PCs. Each view is pinned (-update rewrites them).
func TestViewTextAllPCs(t *testing.T) {
	blockSetup(t)
	otherPCsFixture(t)
	for _, v := range allPCsViews {
		t.Run(v.name, func(t *testing.T) {
			viewGolden(t, "stats-all-pcs-"+v.name+".golden", append(v.args, "--tz", "Europe/Paris")...)
		})
	}
	viewGolden(t, "stats-all-pcs-overview-json.golden", "stats", "overview", "--machines", "all", "--tz", "Europe/Paris", "--json")
	viewGolden(t, "stats-all-pcs-system-json.golden", "stats", "system", "--machines", "all", "--tz", "Europe/Paris", "--json")

	text := func(args ...string) string {
		t.Helper()
		code, out, errOut := run(t, append(append([]string{"stats"}, args...), "--tz", "Europe/Paris")...)
		if code != output.OK {
			t.Fatalf("%v: exit %d: %s %s", args, code, out, errOut)
		}
		return out
	}
	foot := "Activity on all PCs on this server (3) · other PCs up to 2026-10-04 · 1 document ignored · memory on 127.0.0.1:"
	if out := text("cost", "--machines", "all"); !strings.Contains(out, foot) {
		t.Errorf("no footer %q in:\n%s", foot, out)
	}
	if out := text("overview", "--machines", "all", "--project", "p1"); !strings.Contains(out, "1 document ignored · other PCs not filtered · memory on") {
		t.Errorf("a project filter does not say the other PCs are not filtered:\n%s", out)
	}
	if out := text("overview", "--project", "p1"); strings.Contains(out, "other PCs") || !strings.Contains(out, "Activity on this PC · memory on") {
		t.Errorf("this PC alone names the other PCs:\n%s", out)
	}
	// the speed of the 649 recalls: 8 days of 80 from the other PCs and
	// this PC's 9; the slowest 1 in 20 is rank 617, in (1000, 2000], and
	// the typical one rank 325, in (500, 1000]
	wantLines(t, "cost", text("cost", "--machines", "all"),
		"Time memory adds to a prompt 1.0–2.0 s (approximate, n=649, ok)", "all PCs: typical 500 ms–1.0 s")
	// the sessions: this PC's 5 sent and the 8 days of 10 of the others, 9 of 10 ok
	wantLines(t, "overview", text("overview", "--machines", "all"), "Sessions sent 91 % (83–95 %) (n=85, status on this PC: ok)")
	sys := text("system", "--machines", "all")
	wantLines(t, "system", sys,
		"PCs (fetched 2026-10-08 13:00)",
		"3f9c2a17 (this PC) linux 2026-09-15 2026-10-08 09:00 dev 2026-10-07 active",
		"11111111 darwin 2026-09-20 2026-10-08 11:00 v0.1.18 2026-10-07 active",
		"22222222 windows 2026-09-25 2026-10-05 14:00 v0.1.17 2026-10-04 stopped sending",
		"deadbeef written by a newer backsight: update this PC")
}

// The speed and the context of all PCs, as lines: a range read from the
// buckets, an average over the sessions, each with what it stands on.
func TestViewTextMergedLines(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	i64 := func(v int64) *int64 { return &v }
	t.Run("speed is a range", func(t *testing.T) {
		m := metrics.Metric{Value: ptr(2000), Den: i64(1468), Status: metrics.StatusOK, Tags: []string{"approximate"},
			Extra: map[string]any{"range_ms": &[2]*int64{i64(1000), i64(2000)}, "p50_range_ms": &[2]*int64{i64(250), i64(500)}, "p50": 420.0, "p95": 1900.0}}
		text := viewText(result("cost", "recall.latency", m), false)
		wantLines(t, "speed", text, "Time memory adds to a prompt 1.0–2.0 s (approximate, n=1,468, ok)",
			"all PCs: typical 250–500 ms", "this PC alone: typical 420 ms, slowest 1 in 20 1.9 s")
	})
	t.Run("the open bucket and the edges", func(t *testing.T) {
		for _, c := range []struct {
			lo   int64
			hi   *int64
			want string
		}{{0, i64(50), "0–50 ms"}, {500, i64(1000), "500 ms–1.0 s"}, {2500, i64(5000), "2.5–5.0 s"}, {5000, nil, "over 5.0 s"}} {
			m := metrics.Metric{Value: ptr(5000), Den: i64(200), Status: metrics.StatusBad, Tags: []string{"approximate"},
				Extra: map[string]any{"range_ms": &[2]*int64{i64(c.lo), c.hi}}}
			if l := lineOf(t, viewText(result("cost", "recall.latency", m), false), "Time memory adds to a prompt"); !strings.Contains(l, " "+c.want+" ") {
				t.Errorf("%s: line %q", c.want, l)
			}
		}
	})
	t.Run("context is an average", func(t *testing.T) {
		m := metrics.Metric{Value: ptr(2300), Num: i64(715300), Den: i64(311), Status: metrics.StatusOK, Tags: []string{"status_this_pc", "average"},
			Extra: map[string]any{"p50": 2000.0, "p95": 5000.0, "average": 2300.0}}
		text := viewText(result("cost", "context.per_session", m), false)
		wantLines(t, "context", text, "Context added per session 2,300 chars on average (n=311 sessions, status on this PC: ok)",
			"this PC alone: typical 2,000 chars, high 5,000 chars")
	})
	t.Run("this PC alone keeps its lines", func(t *testing.T) {
		m := metrics.Metric{Value: ptr(2300), Den: i64(311), Status: metrics.StatusOK, Extra: map[string]any{"p50": 2000.0, "p95": 5000.0}}
		text := viewText(result("cost", "context.per_session", m), false)
		wantLines(t, "context", text, "Context added per session 2,300 chars (n=311, ok)", "typical 2,000 chars, high 5,000 chars")
	})
}
