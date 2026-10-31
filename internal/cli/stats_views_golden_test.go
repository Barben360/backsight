package cli

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
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
	t.Run("ids and projects are cut", func(t *testing.T) {
		r := result("sessions", "recall.hit_rate", hit)
		r.Tables.Sessions = []metrics.SessionRow{{ID: "abcdefghijklmnop", Project: strings.Repeat("p", 30), Start: r.Window.Since}}
		text := viewText(r, false)
		if !strings.Contains(text, "abcdefgh ") || strings.Contains(text, "abcdefghi") || !strings.Contains(text, strings.Repeat("p", 23)+"…") || strings.Contains(text, strings.Repeat("p", 24)) {
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
