package dashboard

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
)

// The seven Overview scenarios of the approved design (design/canvas:
// Overview.dc.html, Banner.dc.html, Tile.dc.html), as hand-built overview
// results and the texts the page must show for each. They reproduce the
// mockups' statuses as drawn, even where the engine's min_n would judge
// otherwise: they test rendering, not judging. Where two mockups give the
// same data two readings, the fixture follows the visible note (only
// healthy and latency judge Right on 28 days). No memory text, no real
// name or id: the ids and project names are the canvas's samples.

type scenario struct {
	Name   string                 // new | healthy | cantjudge | qualitywatch | latency | serverdown | broken
	Result func() *metrics.Result // a fresh hand-built overview result at each call
	Want   scenarioWant
}

type scenarioWant struct {
	Title     string       // the banner's title, without "(last 7 days)"
	Class     string       // v-ok | v-learn | v-cant | v-watch | v-bad
	Axes      [4][3]string // found, right, cost, flowing: name, word, note
	Reasons   []string     // the banner's reason texts, in order
	Tiles     [6]string    // status words of Found, Used, Reported wrong, Context, Speed, Flowing
	Attention []string     // the "What should I do?" item texts, in order
	Summary   string       // the summary line's visible text
}

// The banner and attention sentences, shared by the reasons, the items
// and the wants.
const (
	scLearnText    = "Learning: day 2 of about 7, 12 prompts so far."
	scJudged28Text = "Judged on the last 28 days: too few turns in 7."
	scCantText     = "Can't judge yet: Claude said what it used on only 64 % of turns."
	scFoundText    = "Fewer prompts get memory than usual: 14 % vs 27 % before."
	scLatencyText  = "Memory adds 2.3 s to the slowest prompts (limit 2.5 s)."
	scServerText   = "Memory server unreachable since 09:12: 14 notes and 3 sessions wait on this PC, nothing is lost."
	scFailedText   = "3 sessions could not be sent; 1 set aside after an error."
	scWaitedText   = "A session has waited on this PC for 26 h."
	scCrashedText  = "2 steps crashed in the last 24 h."
)

var scenarios = []scenario{
	{Name: "new", Result: scNew, Want: scenarioWant{
		Title: scLearnText, Class: "v-learn",
		Axes: [4][3]string{
			{"Found", "learning", "day 2 of about 7"}, {"Right", "learning", "day 2 of about 7"},
			{"Cost", "ok", "slowest prompts 1.4 s"}, {"Flowing", "ok", "4 of 4 sessions sent"},
		},
		Tiles:   [6]string{"too few", "learning", "too few", "ok", "ok", "ok"},
		Summary: "In the last 7 days: 3 prompts got memory, Claude used it in 0 turns, about 1,100 tokens added; extraction cost price not set (server, all PCs).",
	}},
	{Name: "healthy", Result: scHealthy, Want: scenarioWant{
		Title: "Memory is working.", Class: "v-ok",
		Axes: [4][3]string{
			{"Found", "ok", "28 % of prompts got memory"}, {"Right", "ok", scJudged28Text},
			{"Cost", "ok", "slowest prompts 1.6 s"}, {"Flowing", "ok", "239 of 239 sessions sent"},
		},
		Tiles:   [6]string{"ok", "ok", "too few", "ok", "ok", "ok"},
		Summary: "In the last 7 days: 46 prompts got memory, Claude used it in 10 turns, about 17 k tokens added; extraction cost $43.62 (server, all PCs).",
	}},
	{Name: "cantjudge", Result: scCantJudge, Want: scenarioWant{
		Title: "Right: can't judge yet.", Class: "v-cant",
		Axes: [4][3]string{
			{"Found", "ok", "27 % of prompts got memory"}, {"Right", "can't judge", "Claude said what it used on 16 of 25 turns"},
			{"Cost", "ok", "slowest prompts 1.5 s"}, {"Flowing", "ok", "212 of 212 sessions sent"},
		},
		Reasons:   []string{scCantText},
		Tiles:     [6]string{"ok", "unreliable", "too few", "ok", "ok", "ok"},
		Attention: []string{scCantText},
		Summary:   "In the last 7 days: 41 prompts got memory, Claude used it in 9 turns, about 15 k tokens added; extraction cost $39.80 (server, all PCs).",
	}},
	{Name: "qualitywatch", Result: scQualityWatch, Want: scenarioWant{
		Title: "Found needs attention.", Class: "v-watch",
		Axes: [4][3]string{
			{"Found", "attention", "14 % vs 27 % usual, two weeks running"}, {"Right", "ok", "31 % used, borderline"},
			{"Cost", "ok", "slowest prompts 1.7 s"}, {"Flowing", "ok", "226 of 226 sessions sent"},
		},
		Reasons:   []string{scFoundText},
		Tiles:     [6]string{"watch", "borderline", "ok", "ok", "ok", "ok"},
		Attention: []string{scFoundText},
		Summary:   "In the last 7 days: 21 prompts got memory, Claude used it in 8 turns, about 18 k tokens added; extraction cost $41.05 (server, all PCs).",
	}},
	{Name: "latency", Result: scLatency, Want: scenarioWant{
		Title: "Cost needs attention: slowest prompts take 2.3 s.", Class: "v-watch",
		Axes: [4][3]string{
			{"Found", "ok", "28 % of prompts got memory"}, {"Right", "ok", scJudged28Text},
			{"Cost", "attention", "slowest 1 in 20: 2.3 s, limit 2.5 s"}, {"Flowing", "ok", "239 of 239 sessions sent"},
		},
		Reasons:   []string{scLatencyText},
		Tiles:     [6]string{"ok", "ok", "too few", "ok", "watch", "ok"},
		Attention: []string{scLatencyText},
		Summary:   "In the last 7 days: 46 prompts got memory, Claude used it in 10 turns, about 17 k tokens added; extraction cost $43.62 (server, all PCs).",
	}},
	{Name: "serverdown", Result: scServerDown, Want: scenarioWant{
		Title: "Flowing: memory server unreachable since 09:12, nothing is lost.", Class: "v-watch",
		Axes: [4][3]string{
			{"Found", "ok", "9 %: prompts skipped while the server is unreachable"}, {"Right", "ok", "60 % used"},
			{"Cost", "ok", "slowest prompts 910 ms"}, {"Flowing", "attention", "14 notes and 3 sessions waiting"},
		},
		Reasons:   []string{scServerText},
		Tiles:     [6]string{"watch", "ok", "too few", "ok", "ok", "watch"},
		Attention: []string{scServerText},
		Summary:   "In the last 7 days: 12 prompts got memory, Claude used it in 9 turns, about 11 k tokens added; extraction cost unknown (the memory server is not answering).",
	}},
	{Name: "broken", Result: scBroken, Want: scenarioWant{
		Title: "Flowing needs attention (3 reasons).", Class: "v-bad",
		Axes: [4][3]string{
			{"Found", "ok", "25 % of prompts got memory"}, {"Right", "ok", "58 % used"},
			{"Cost", "ok", "slowest prompts 1.9 s"}, {"Flowing", "attention", "3 failed, oldest waiting 26 h"},
		},
		Reasons:   []string{scFailedText, scWaitedText, scCrashedText},
		Tiles:     [6]string{"ok", "ok", "ok", "ok", "ok", "attention"},
		Attention: []string{scFailedText, scWaitedText, scCrashedText},
		Summary:   "In the last 7 days: 40 prompts got memory, Claude used it in 11 turns, about 16 k tokens added; extraction cost $40.12 (server, all PCs).",
	}},
}

// scAt is a time on day of October 2026 in Paris; scenarios are generated
// at scAt(30, 15, 44), 2026-10-30T15:44:00+01:00.
func scAt(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, parisZone())
}

func scF(x float64) *float64 { return &x }
func scN(n int64) *int64     { return &n }

// scNull marks a day without a value in scDaily.
var scNull = math.NaN()

// scPart is one metric of a scenario.
type scPart struct {
	id string
	m  metrics.Metric
}

type scOpt func(*metrics.Metric)

// scMetric is m with its catalog unit, aggregation and drill (bare, as the
// engine emits it: the page's link adds the window and filters), and no tag
// as [], as the views return it.
func scMetric(id string, m metrics.Metric, opts []scOpt) scPart {
	d, _ := metrics.DefOf(id)
	m.Unit, m.Aggregation, m.Drill = d.Unit, d.Aggregation, d.Drill
	m.Tags = []string{}
	for _, o := range opts {
		o(&m)
	}
	return scPart{id, m}
}

// scRate is a ratio metric: num of den, read as st.
func scRate(id string, st metrics.Status, num, den int64, opts ...scOpt) scPart {
	m := metrics.Metric{Status: st, Num: scN(num), Den: scN(den)}
	if den > 0 {
		m.Value = scF(float64(num) / float64(den))
	}
	return scMetric(id, m, opts)
}

// scValue is a value over n events (none when n is 0), read as st.
func scValue(id string, st metrics.Status, v float64, n int64, opts ...scOpt) scPart {
	m := metrics.Metric{Status: st, Value: scF(v)}
	if n > 0 {
		m.Den = scN(n)
	}
	return scMetric(id, m, opts)
}

// scNoData is a metric with nothing to read, for reason.
func scNoData(id, reason string) scPart {
	return scMetric(id, metrics.Metric{Status: metrics.StatusNoData, Reason: reason}, nil)
}

func scCI(lo, hi float64) scOpt { return func(m *metrics.Metric) { m.CI = &[2]float64{lo, hi} } }
func scMDC(x float64) scOpt     { return func(m *metrics.Metric) { m.MDC = scF(x) } }
func scTags(tags ...string) scOpt {
	return func(m *metrics.Metric) { m.Tags = append(m.Tags, tags...) }
}
func scBorderline() scOpt            { return func(m *metrics.Metric) { m.Borderline = true } }
func scExtra(x map[string]any) scOpt { return func(m *metrics.Metric) { m.Extra = x } }
func scDelta(d float64) scOpt {
	return func(m *metrics.Metric) { m.Delta, m.Previous = scF(d), scF(*m.Value-d) }
}
func scDeltaFrom(prev float64) scOpt {
	return func(m *metrics.Metric) { m.Previous, m.Delta = scF(prev), scF(*m.Value-prev) }
}
func scSnapshot() scOpt { return scTags("snapshot") }

// scDaily is the tile's sparkline over the window's 8 day buckets,
// 2026-10-23 to 2026-10-30, the first and the last partial: vals are the
// canvas's 7 days, 24 to 30, each times scale; the partial head the canvas
// does not draw repeats its first day. scNull is a day without a value.
func scDaily(scale float64, vals ...float64) scOpt {
	return func(m *metrics.Metric) {
		vals = append([]float64{vals[0]}, vals...)
		for i, v := range vals {
			p := metrics.Point{Day: fmt.Sprintf("2026-10-%02d", 23+i), Partial: i == 0 || i == len(vals)-1}
			if !math.IsNaN(v) {
				p.Value = scF(v * scale)
			}
			m.Series = append(m.Series, p)
		}
	}
}

// scContext is context.per_session: the high value (p95) and the typical
// (p50) characters per session over sessions, and the high value in tokens.
func scContext(p95, p50 float64, tokensP95, sessions int64, opts ...scOpt) scPart {
	x := map[string]any{"p50": p50, "p95": p95, "tokens_p95": tokensP95}
	return scValue("context.per_session", metrics.StatusOK, p95, sessions, append([]scOpt{scExtra(x)}, opts...)...)
}

// scSpeed is recall.latency: the slowest 1 in 20 and the typical prompt,
// in ms, over prompts, read as st.
func scSpeed(st metrics.Status, p95, p50 float64, prompts int64, opts ...scOpt) scPart {
	x := map[string]any{"p50": p50, "p95": p95}
	return scValue("recall.latency", st, p95, prompts, append([]scOpt{scExtra(x)}, opts...)...)
}

// scFlow is pipeline.flow: sent of sessions, with what did not go.
func scFlow(st metrics.Status, sent, sessions, failed, setAside, waiting, oldestMS int64, opts ...scOpt) scPart {
	x := map[string]any{"ok": sent, "failed": failed, "set_aside": setAside, "waiting": waiting, "backlog_oldest_ms": oldestMS}
	return scRate("pipeline.flow", st, sent, sessions, append([]scOpt{scExtra(x)}, opts...)...)
}

// scBacklog is local.backlog: what waits on this PC.
func scBacklog(st metrics.Status, notes, sessions, setAside, oldestMS int64) scPart {
	x := map[string]any{"notes_queued": notes, "sessions_waiting": sessions, "quarantined": setAside, "oldest_ms": oldestMS}
	return scValue("local.backlog", st, float64(notes+sessions), 0, scExtra(x))
}

// scReachOK is server.reach for a server that answered a minute ago.
func scReachOK() scPart {
	x := map[string]any{"last_ok": scAt(30, 15, 43), "failing_since": nil, "breaker_open_until": nil, "skipped_prompts": int64(0)}
	return scValue("server.reach", metrics.StatusOK, 0, 0, scExtra(x))
}

// scBank is bank.size and bank.growth: the memories by kind (summaries,
// facts about you and your work, facts from a session), the documents, and
// the memories added in the window by kind.
func scBank(summaries, world, sessionFacts, documents int64, added [3]int64) []scPart {
	size := map[string]any{"world": world, "experience": sessionFacts, "observation": summaries, "documents": documents}
	growth := map[string]any{"world": added[0], "experience": added[1], "observation": added[2]}
	return []scPart{
		scValue("bank.size", metrics.StatusOK, float64(summaries+world+sessionFacts), 0, scExtra(size), scSnapshot()),
		scValue("bank.growth", metrics.StatusOK, float64(added[0]+added[1]+added[2])/7, 0, scExtra(growth), scSnapshot()),
	}
}

// scHealthyBank is the glance of every scenario but new.
func scHealthyBank() []scPart { return scBank(212, 431, 641, 286, [3]int64{15, 23, 8}) }

// scSession is one Lately row: the canvas's 8-character id with the
// canvas's sample tail, last active at last.
func scSession(id, project string, last time.Time, prompts, withMemory, shown, used, wrong, chars int64) metrics.SessionRow {
	return metrics.SessionRow{
		ID: id + "4e1b0c9d2f7a63e85b1d0c4a", Project: project, Start: last.Add(-40 * time.Minute), Last: last,
		Prompts: prompts, PromptsWithMemory: withMemory, Shown: shown, Used: used, Wrong: wrong, ContextChars: chars, Sent: "ok",
	}
}

// scLately is the Lately table of every scenario but new.
func scLately() []metrics.SessionRow {
	return []metrics.SessionRow{
		scSession("a41c09e2", "backsight", scAt(30, 15, 19), 6, 4, 10, 2, 1, 3200),
		scSession("5be2d7f0", "project-a", scAt(30, 13, 44), 3, 2, 4, 1, 0, 1500),
		scSession("93c1a4e8", "backsight", scAt(30, 10, 44), 5, 4, 9, 3, 0, 2800),
		scSession("d0f63b15", "project-c", scAt(29, 16, 20), 2, 1, 3, 0, 0, 900),
		scSession("2e8a91c7", "project-d-shared-frontend-tools", scAt(28, 11, 30), 4, 2, 5, 1, 0, 2100),
	}
}

// scItem is an attention item; its reason on the banner is scReason.
func scItem(metric string, sev metrics.Severity, axis, text, command, why string) metrics.Item {
	return metrics.Item{Metric: metric, Severity: sev, Axis: axis, Text: text, Why: why, Command: command}
}

func scReason(it metrics.Item) metrics.Reason {
	return metrics.Reason{Metric: it.Metric, Severity: it.Severity, Text: it.Text, Command: it.Command}
}

// scAxis is an axis judged on days with reasons.
func scAxis(state metrics.AxisState, days int, reasons ...metrics.Reason) metrics.Axis {
	return metrics.Axis{State: state, WindowDays: days, Reasons: append([]metrics.Reason{}, reasons...)}
}

// scAxes are the four axes, ok on 7 days unless given.
func scAxes(given map[string]metrics.Axis) map[string]metrics.Axis {
	out := map[string]metrics.Axis{}
	for _, a := range []string{"found", "right", "cost", "flowing"} {
		if ax, ok := given[a]; ok {
			out[a] = ax
		} else {
			out[a] = scAxis(metrics.AxisOK, 7)
		}
	}
	return out
}

// scRight28 is Right ok, judged on the last 28 days for want of turns.
func scRight28() metrics.Axis {
	return scAxis(metrics.AxisOK, 28, metrics.Reason{Metric: "used.turn_rate", Severity: metrics.SeverityInfo, Text: scJudged28Text})
}

// scSummary is the summary line's figures: cost nil with priced false for
// a price not set.
func scSummary(prompts, turns, tokens int64, cost *float64, priced bool) *metrics.Summary {
	s := &metrics.Summary{PromptsWithMemory: prompts, TurnsWithMemoryUsed: turns, ContextChars: tokens * 4, TokensApprox: tokens,
		ExtractionCost: cost, PriceSet: priced, ExtractionScope: "server"}
	if priced {
		s.Currency = "USD"
	}
	return s
}

// scResult is an overview generated at 15:44 on 2026-10-30 in Paris over
// the last 7 days, the server answering, with parts as its metrics.
func scResult(verdict *metrics.Verdict, summary *metrics.Summary, attention []metrics.Item, parts ...scPart) *metrics.Result {
	loc := parisZone()
	now := scAt(30, 15, 44)
	since := now.Add(-7 * 24 * time.Hour).In(loc)
	r := &metrics.Result{
		View: "overview", ViewSchema: metrics.ViewSchema, GeneratedAt: now,
		Window:   metrics.Window{Since: since, Until: now, Days: 7, TZ: "Europe/Paris", Loc: loc},
		Previous: metrics.Span{Since: since.Add(-7 * 24 * time.Hour), Until: since},
		Scope:    metrics.Scope{Machines: "this", Server: "127.0.0.1:18888", ServerOK: true},
		Server:   &metrics.ServerState{OK: true, LastOK: scT(scAt(30, 15, 43))},
		Verdict:  verdict, Summary: summary, Metrics: map[string]metrics.Metric{},
		Tables:    metrics.Tables{Lately: scLately()},
		Attention: append([]metrics.Item{}, attention...), Info: []metrics.InfoItem{},
	}
	r.Index.IngestedAt = now
	for _, p := range parts {
		r.Metrics[p.id] = p.m
	}
	return r
}

func scT(t time.Time) *time.Time { return &t }

func scNew() *metrics.Result {
	learn := func(metric string) metrics.Axis {
		return scAxis(metrics.AxisLearning, 7, metrics.Reason{Metric: metric, Severity: metrics.SeverityInfo, Text: scLearnText})
	}
	v := &metrics.Verdict{WindowDays: 7, Learning: true, Axes: scAxes(map[string]metrics.Axis{
		"found": learn("recall.hit_rate"), "right": learn("used.turn_rate"),
	})}
	r := scResult(v, scSummary(3, 0, 1100, nil, false), nil, append([]scPart{
		scRate("recall.hit_rate", metrics.StatusTooFew, 3, 9, scDaily(0.01, scNull, scNull, scNull, scNull, scNull, 33, 33)),
		scNoData("used.turn_rate", metrics.ReasonNoEvents),
		scRate("verdict.wrong_rate", metrics.StatusTooFew, 0, 0, scDaily(0.01, scNull, scNull, scNull, scNull, scNull, 0, 0)),
		scContext(1100, 600, 280, 4, scDaily(1, scNull, scNull, scNull, scNull, scNull, 900, 1100)),
		scSpeed(metrics.StatusOK, 1400, 620, 12, scDaily(1000, scNull, scNull, scNull, scNull, scNull, 1.3, 1.4)),
		scFlow(metrics.StatusOK, 4, 4, 0, 0, 0, 0, scDaily(0.01, scNull, scNull, scNull, scNull, scNull, 100, 100)),
		scRate("declare.coverage", metrics.StatusTooFew, 1, 1),
		scBacklog(metrics.StatusOK, 0, 0, 0, 0),
		scReachOK(),
	}, scBank(0, 12, 26, 4, [3]int64{12, 26, 0})...)...)
	r.Tables.Lately = []metrics.SessionRow{
		scSession("c7d24e60", "backsight", scAt(30, 15, 4), 2, 2, 2, 0, 0, 1100),
		scSession("81f0b3a9", "backsight", scAt(29, 17, 10), 1, 1, 1, 0, 0, 1000),
	}
	return r
}

// scHealthyParts are the healthy scenario's metrics; latency changes only
// its speed.
func scHealthyParts(speed scPart) []scPart {
	return append([]scPart{
		scRate("recall.hit_rate", metrics.StatusOK, 46, 167, scCI(0.21, 0.35), scDelta(0.02), scMDC(0.09),
			scDaily(0.01, 24, 31, 27, 22, 30, 29, 28)),
		scRate("used.turn_rate", metrics.StatusOK, 38, 61, scCI(0.50, 0.73), scDelta(0.04), scMDC(0.22), scTags("judged_28d"),
			scDaily(0.01, 60, 64, 58, 66, 61, 62, 63)),
		scRate("verdict.wrong_rate", metrics.StatusTooFew, 1, 14, scDaily(0.01, 0, 0, 1, 0, 0, 0, 0)),
		scContext(3600, 2100, 900, 34, scDelta(-200), scMDC(700), scDaily(1, 3400, 3700, 3500, 3900, 3600, 3300, 3600)),
		speed,
		scFlow(metrics.StatusOK, 239, 239, 0, 0, 0, 0, scDelta(0), scDaily(0.01, 100, 100, 100, 100, 100, 100, 100)),
		scRate("declare.coverage", metrics.StatusOK, 61, 61),
		scBacklog(metrics.StatusOK, 0, 0, 0, 0),
		scReachOK(),
	}, scHealthyBank()...)
}

func scHealthy() *metrics.Result {
	v := &metrics.Verdict{WindowDays: 7, Axes: scAxes(map[string]metrics.Axis{"right": scRight28()})}
	speed := scSpeed(metrics.StatusOK, 1600, 840, 167, scDelta(100), scMDC(300), scDaily(1000, 1.4, 1.5, 1.7, 1.5, 1.6, 1.6, 1.6))
	return scResult(v, scSummary(46, 10, 17000, scF(43.62), true), nil, scHealthyParts(speed)...)
}

func scCantJudge() *metrics.Result {
	it := scItem("declare.coverage", metrics.SeverityWatch, "right", scCantText, "backsight stats sessions --has undeclared",
		"Whether memory is right depends on Claude saying which memories it used. Under 80 % of turns, the Used and Reported wrong numbers are not reliable. The sessions listed show the turns where it said nothing.")
	v := &metrics.Verdict{WindowDays: 7, Axes: scAxes(map[string]metrics.Axis{"right": scAxis(metrics.AxisCantJudge, 7, scReason(it))})}
	return scResult(v, scSummary(41, 9, 15000, scF(39.80), true), []metrics.Item{it}, append([]scPart{
		scRate("recall.hit_rate", metrics.StatusOK, 41, 152, scCI(0.20, 0.34), scDelta(-0.01), scMDC(0.09),
			scDaily(0.01, 29, 26, 28, 25, 27, 28, 27)),
		scRate("used.turn_rate", metrics.StatusOK, 9, 16, scCI(0.33, 0.77), scMDC(0.30), scTags("unreliable"),
			scDaily(0.01, 50, 62, 48, 60, 55, 58, 56)),
		scRate("verdict.wrong_rate", metrics.StatusTooFew, 0, 9, scDaily(0.01, 0, 0, 0, 0, 0, 0, 0)),
		scContext(3400, 2000, 850, 31, scDelta(-100), scMDC(700), scDaily(1, 3300, 3500, 3400, 3600, 3300, 3400, 3400)),
		scSpeed(metrics.StatusOK, 1500, 800, 152, scDelta(0), scMDC(300), scDaily(1000, 1.5, 1.4, 1.6, 1.5, 1.5, 1.5, 1.5)),
		scFlow(metrics.StatusOK, 212, 212, 0, 0, 0, 0, scDelta(0), scDaily(0.01, 100, 100, 100, 100, 100, 100, 100)),
		scRate("declare.coverage", metrics.StatusWatch, 16, 25),
		scBacklog(metrics.StatusOK, 0, 0, 0, 0),
		scReachOK(),
	}, scHealthyBank()...)...)
}

func scQualityWatch() *metrics.Result {
	it := scItem("recall.hit_rate", metrics.SeverityWatch, "found", scFoundText, "backsight stats quality --days 14",
		"With less memory, Claude starts each prompt with less of your context. The Quality page shows why prompts got nothing: often a project with little memory yet, which importing its past sessions fixes.")
	v := &metrics.Verdict{WindowDays: 7, Axes: scAxes(map[string]metrics.Axis{"found": scAxis(metrics.AxisAttention, 7, scReason(it))})}
	return scResult(v, scSummary(21, 8, 18000, scF(41.05), true), []metrics.Item{it}, append([]scPart{
		scRate("recall.hit_rate", metrics.StatusWatch, 21, 150, scCI(0.09, 0.20), scDeltaFrom(0.27), scMDC(0.08),
			scDaily(0.01, 26, 22, 17, 13, 12, 15, 14)),
		scRate("used.turn_rate", metrics.StatusOK, 14, 45, scCI(0.19, 0.46), scBorderline(), scDelta(-0.09), scMDC(0.25),
			scDaily(0.01, 40, 35, 30, 28, 33, 30, 31)),
		scRate("verdict.wrong_rate", metrics.StatusOK, 2, 23, scCI(0.02, 0.27), scDelta(0.03), scMDC(0.20),
			scDaily(0.01, 0, 5, 0, 10, 0, 9, 9)),
		scContext(3900, 2300, 980, 33, scDelta(300), scMDC(700), scDaily(1, 3600, 3700, 3900, 4000, 3800, 3900, 3900)),
		scSpeed(metrics.StatusOK, 1700, 880, 150, scDelta(100), scMDC(300), scDaily(1000, 1.6, 1.6, 1.7, 1.8, 1.7, 1.7, 1.7)),
		scFlow(metrics.StatusOK, 226, 226, 0, 0, 0, 0, scDelta(0), scDaily(0.01, 100, 100, 100, 100, 100, 100, 100)),
		scRate("declare.coverage", metrics.StatusOK, 43, 45),
		scBacklog(metrics.StatusOK, 0, 0, 0, 0),
		scReachOK(),
	}, scHealthyBank()...)...)
}

func scLatency() *metrics.Result {
	it := scItem("recall.latency", metrics.SeverityWatch, "cost", scLatencyText, "backsight doctor",
		"Every prompt waits for memory before Claude sees it. Past 2.5 s the wait becomes noticeable. Doctor checks the server and its search index and names the fix.")
	v := &metrics.Verdict{WindowDays: 7, Axes: scAxes(map[string]metrics.Axis{
		"right": scRight28(), "cost": scAxis(metrics.AxisAttention, 7, scReason(it)),
	})}
	speed := scSpeed(metrics.StatusWatch, 2300, 1100, 167, scDelta(700), scMDC(300), scDaily(1000, 1.6, 1.7, 1.9, 2.1, 2.2, 2.4, 2.3))
	return scResult(v, scSummary(46, 10, 17000, scF(43.62), true), []metrics.Item{it}, scHealthyParts(speed)...)
}

func scServerDown() *metrics.Result {
	since := scAt(30, 9, 12)
	waited := scAt(30, 15, 44).Sub(since).Milliseconds()
	it := scItem("server.reach", metrics.SeverityWatch, "flowing", scServerText, "backsight doctor",
		"While the server is unreachable, prompts get no memory and sessions wait on this PC. They are sent on their own once it answers. After 24 h this becomes urgent.")
	v := &metrics.Verdict{WindowDays: 7, Axes: scAxes(map[string]metrics.Axis{"flowing": scAxis(metrics.AxisAttention, 7, scReason(it))})}
	reach := map[string]any{"last_ok": since, "failing_since": since, "breaker_open_until": scAt(30, 15, 49), "skipped_prompts": int64(96)}
	r := scResult(v, scSummary(12, 9, 11000, nil, true), []metrics.Item{it},
		scRate("recall.hit_rate", metrics.StatusWatch, 12, 133, scCI(0.05, 0.15), scDelta(-0.19), scMDC(0.09),
			scDaily(0.01, 27, 29, 28, 26, 28, 24, 9)),
		scRate("used.turn_rate", metrics.StatusOK, 27, 45, scCI(0.45, 0.74), scDelta(-0.02), scMDC(0.22),
			scDaily(0.01, 61, 63, 60, 62, 59, 61, 60)),
		scRate("verdict.wrong_rate", metrics.StatusTooFew, 0, 11, scDaily(0.01, 0, 0, 0, 0, 0, 0, 0)),
		scContext(2100, 1200, 530, 29, scDelta(-1500), scMDC(700), scDaily(1, 3500, 3600, 3400, 3700, 3500, 3300, 2100)),
		scSpeed(metrics.StatusOK, 910, 380, 133, scDeltaFrom(1600), scMDC(300), scDaily(1000, 1.6, 1.5, 1.6, 1.7, 1.6, 1.5, 0.91)),
		scFlow(metrics.StatusWatch, 14, 17, 0, 0, 3, waited, scDaily(0.01, 100, 100, 100, 100, 100, 100, 82)),
		scRate("declare.coverage", metrics.StatusOK, 44, 45),
		scBacklog(metrics.StatusWatch, 14, 3, 0, waited),
		scValue("server.reach", metrics.StatusWatch, 96, 0, scExtra(reach)),
		scNoData("bank.size", metrics.ReasonServerDown),
		scNoData("bank.growth", metrics.ReasonServerDown),
	)
	r.Scope.ServerOK = false
	r.Server = &metrics.ServerState{OK: false, Error: "unreachable", LastOK: &since}
	return r
}

func scBroken() *metrics.Result {
	const day26h = int64(26 * 3600 * 1000) // 93 600 000 ms
	failed := scItem("pipeline.flow", metrics.SeverityBad, "flowing", scFailedText, "backsight retain retry-quarantine",
		"Sessions that are not sent never become memories. The one set aside failed in a way that a retry may fix; the other two are retried on their own.")
	waited := scItem("local.backlog", metrics.SeverityBad, "flowing", scWaitedText, "backsight doctor",
		"Sessions normally reach the server within minutes. A day-old backlog means sending is stuck; doctor names the step that fails.")
	crashed := scItem("hooks.failures", metrics.SeverityWatch, "flowing", scCrashedText, "backsight report",
		"A step that crashes skips its work for that prompt or session. Two in a day is worth a bug report: it collects the logs without any memory or prompt text.")
	v := &metrics.Verdict{WindowDays: 7, Axes: scAxes(map[string]metrics.Axis{
		"flowing": scAxis(metrics.AxisAttention, 7, scReason(failed), scReason(waited), scReason(crashed)),
	})}
	crashes := map[string]any{"by_kind": map[string]int64{"panic": 2}, "last_24h": map[string]int64{"panic": 2}}
	return scResult(v, scSummary(40, 11, 16000, scF(40.12), true), []metrics.Item{failed, waited, crashed}, append([]scPart{
		scRate("recall.hit_rate", metrics.StatusOK, 40, 160, scCI(0.19, 0.32), scDelta(-0.03), scMDC(0.09),
			scDaily(0.01, 28, 27, 26, 24, 25, 25, 25)),
		scRate("used.turn_rate", metrics.StatusOK, 33, 57, scCI(0.45, 0.70), scDelta(-0.04), scMDC(0.22),
			scDaily(0.01, 62, 60, 59, 57, 58, 58, 58)),
		scRate("verdict.wrong_rate", metrics.StatusOK, 1, 20, scCI(0.01, 0.24), scDelta(0), scMDC(0.20),
			scDaily(0.01, 0, 0, 5, 0, 0, 5, 5)),
		scContext(3500, 2100, 880, 32, scDelta(-100), scMDC(700), scDaily(1, 3500, 3600, 3400, 3600, 3500, 3500, 3500)),
		scSpeed(metrics.StatusOK, 1900, 950, 160, scDelta(300), scMDC(300), scDaily(1000, 1.6, 1.7, 1.8, 1.8, 1.9, 1.9, 1.9)),
		scFlow(metrics.StatusBad, 188, 192, 3, 1, 0, day26h, scDeltaFrom(1), scDaily(0.01, 100, 100, 100, 99, 98, 97, 96)),
		scRate("declare.coverage", metrics.StatusOK, 55, 57),
		scBacklog(metrics.StatusBad, 0, 4, 1, day26h),
		scValue("hooks.failures", metrics.StatusWatch, 2, 0, scExtra(crashes)),
		scReachOK(),
	}, scHealthyBank()...)...)
}

// scenarioAxes are the verdict's axes in the banner's order, with their
// names on the page.
var scenarioAxes = [4][2]string{{"found", "Found"}, {"right", "Right"}, {"cost", "Cost"}, {"flowing", "Flowing"}}

// scenarioTiles are the Overview's six tiles in the page's order: the
// metric each shows and the axis it belongs to.
var scenarioTiles = [6][2]string{
	{"recall.hit_rate", "found"}, {"used.turn_rate", "right"}, {"verdict.wrong_rate", "right"},
	{"context.per_session", "cost"}, {"recall.latency", "cost"}, {"pipeline.flow", "flowing"},
}

// scenarioMetrics are the metrics every scenario carries: the tiles', and
// those the banner, the summary and the glance read.
var scenarioMetrics = []string{"declare.coverage", "local.backlog", "server.reach", "bank.size", "bank.growth"}

var scenarioAxisWords = map[metrics.AxisState]string{
	metrics.AxisOK: "ok", metrics.AxisAttention: "attention", metrics.AxisTooEarly: "too few",
	metrics.AxisCantJudge: "can't judge", metrics.AxisLearning: "learning",
}

var scenarioStatusWords = map[metrics.Status]string{
	metrics.StatusOK: "ok", metrics.StatusWatch: "watch", metrics.StatusBad: "attention", metrics.StatusTooFew: "too few",
}

// scenarioTileWord is the status word a tile shows for m, on an axis that
// is learning or not; "" for a reading no tile word covers.
func scenarioTileWord(m metrics.Metric, learning bool) string {
	switch {
	case m.Status == metrics.StatusNoData && learning:
		return "learning"
	case slices.Contains(m.Tags, "unreliable"):
		return "unreliable"
	case m.Borderline:
		return "borderline"
	}
	return scenarioStatusWords[m.Status]
}

// The seven Overview scenarios are what the later page tests render: each
// fixture must be a Result the views could return (it survives JSON, names
// only catalog metrics with their catalog drills) and must read, through
// the page's status and axis words, as its expected texts say.
func TestScenarioFixturesAreConsistent(t *testing.T) {
	// The shared sentences feed the fixtures and the wants alike, so they
	// are pinned here to the brief's wording, typed out again.
	for _, c := range [][2]string{
		{scLearnText, "Learning: day 2 of about 7, 12 prompts so far."},
		{scJudged28Text, "Judged on the last 28 days: too few turns in 7."},
		{scCantText, "Can't judge yet: Claude said what it used on only 64 % of turns."},
		{scFoundText, "Fewer prompts get memory than usual: 14 % vs 27 % before."},
		{scLatencyText, "Memory adds 2.3 s to the slowest prompts (limit 2.5 s)."},
		{scServerText, "Memory server unreachable since 09:12: 14 notes and 3 sessions wait on this PC, nothing is lost."},
		{scFailedText, "3 sessions could not be sent; 1 set aside after an error."},
		{scWaitedText, "A session has waited on this PC for 26 h."},
		{scCrashedText, "2 steps crashed in the last 24 h."},
	} {
		if c[0] != c[1] {
			t.Errorf("sentence %q, want %q", c[0], c[1])
		}
	}
	var names []string
	for _, s := range scenarios {
		names = append(names, s.Name)
		t.Run(s.Name, func(t *testing.T) {
			r := s.Result()
			if again := s.Result(); again == r || !reflect.DeepEqual(again, r) {
				t.Fatal("Result must build an equal but fresh Result at each call")
			}
			checkScenarioFrame(t, r)
			checkScenarioRoundTrip(t, r)
			checkScenarioMetrics(t, r)
			checkScenarioVerdict(t, r, s.Want)
			checkScenarioTiles(t, r, s.Want)
			checkScenarioAttention(t, r, s.Want)
			if !slices.Contains([]string{"v-ok", "v-learn", "v-cant", "v-watch", "v-bad"}, s.Want.Class) {
				t.Errorf("class %q is not a banner class", s.Want.Class)
			}
			if s.Want.Title == "" || strings.Contains(s.Want.Title, "last 7 days") {
				t.Errorf("title %q: want the banner title without its window", s.Want.Title)
			}
		})
	}
	want := []string{"new", "healthy", "cantjudge", "qualitywatch", "latency", "serverdown", "broken"}
	if !slices.Equal(names, want) {
		t.Errorf("scenarios %v, want %v (the canvas's order)", names, want)
	}
}

func checkScenarioFrame(t *testing.T, r *metrics.Result) {
	t.Helper()
	gen := time.Date(2026, 10, 30, 15, 44, 0, 0, parisZone())
	if r.View != "overview" || r.ViewSchema != metrics.ViewSchema {
		t.Errorf("view %q schema %d, want overview %d", r.View, r.ViewSchema, metrics.ViewSchema)
	}
	if !r.GeneratedAt.Equal(gen) || r.GeneratedAt.Format(time.RFC3339) != "2026-10-30T15:44:00+01:00" {
		t.Errorf("generated at %s, want 2026-10-30T15:44:00+01:00", r.GeneratedAt.Format(time.RFC3339))
	}
	w := r.Window
	if w.TZ != "Europe/Paris" || w.Loc == nil || w.Loc.String() != "Europe/Paris" || w.Days != 7 ||
		!w.Until.Equal(gen) || w.Length() != 7*24*time.Hour {
		t.Errorf("window %+v, want the 7 days up to %s in Europe/Paris", w, gen)
	}
	if !r.Previous.Until.Equal(w.Since) || r.Previous.Until.Sub(r.Previous.Since) != 7*24*time.Hour {
		t.Errorf("previous %+v, want the 7 days before the window", r.Previous)
	}
	if r.Scope.Machines != "this" || r.Scope.Server != "127.0.0.1:18888" {
		t.Errorf("scope %+v, want this PC on 127.0.0.1:18888", r.Scope)
	}
	if r.Server == nil || r.Server.OK != r.Scope.ServerOK {
		t.Errorf("server %+v disagrees with the scope's server_ok %v", r.Server, r.Scope.ServerOK)
	}
	if r.Summary == nil || r.Verdict == nil || len(r.Tables.Lately) == 0 {
		t.Error("an overview carries a summary, a verdict and the Lately rows")
	}
}

// checkScenarioRoundTrip: what the pages read must be what the API
// returns, so the fixture must survive JSON (as goldenResult checks).
func checkScenarioRoundTrip(t *testing.T, r *metrics.Result) {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back metrics.Result
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(&back)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if json.Unmarshal(raw, &want) != nil || json.Unmarshal(again, &got) != nil || !reflect.DeepEqual(want, got) {
		t.Errorf("the fixture does not survive a round trip through metrics.Result:\n%s\n%s", raw, again)
	}
}

func checkScenarioMetrics(t *testing.T, r *metrics.Result) {
	t.Helper()
	for _, id := range append(scenarioMetrics, scenarioTiles[0][0], scenarioTiles[1][0], scenarioTiles[2][0],
		scenarioTiles[3][0], scenarioTiles[4][0], scenarioTiles[5][0]) {
		if _, ok := r.Metrics[id]; !ok {
			t.Errorf("metric %s is missing", id)
		}
	}
	for id, m := range r.Metrics {
		d, ok := metrics.DefOf(id)
		if !ok {
			t.Errorf("metric %s is not in the catalog", id)
			continue
		}
		if m.Drill != d.Drill || m.Unit != d.Unit || m.Aggregation != d.Aggregation {
			t.Errorf("metric %s: drill %q unit %q aggregation %q, want the catalog's %q %q %q", id, m.Drill, m.Unit, m.Aggregation,
				d.Drill, d.Unit, d.Aggregation)
		}
		if m.Series != nil {
			checkScenarioSeries(t, id, m.Series, r.Window.Buckets())
		}
		for k := range m.Extra {
			if !slices.Contains(d.Extra, k) {
				t.Errorf("metric %s: extra %q is not one the catalog declares", id, k)
			}
		}
		if m.Status == metrics.StatusNoData && (m.Reason == "" || m.Value != nil) {
			t.Errorf("metric %s: no data needs a reason and no value", id)
		}
	}
	// Every metric an axis reason or an attention item names is one of the
	// result's, so a page can link it.
	for _, ax := range r.Verdict.Axes {
		for _, re := range ax.Reasons {
			if _, ok := r.Metrics[re.Metric]; !ok {
				t.Errorf("reason %q names metric %q, not in the result", re.Text, re.Metric)
			}
		}
	}
	for _, it := range r.Attention {
		if _, ok := r.Metrics[it.Metric]; !ok {
			t.Errorf("item %q names metric %q, not in the result", it.Text, it.Metric)
		}
	}
}

func checkScenarioVerdict(t *testing.T, r *metrics.Result, want scenarioWant) {
	t.Helper()
	v := r.Verdict
	if len(v.Axes) != 4 || v.WindowDays != 7 {
		t.Fatalf("verdict %+v, want four axes over 7 days", v)
	}
	learning := false
	var attention, cantJudge []string
	for i, a := range scenarioAxes {
		ax, ok := v.Axes[a[0]]
		if !ok {
			t.Errorf("axis %s is missing", a[0])
			continue
		}
		learning = learning || ax.State == metrics.AxisLearning
		if got := [2]string{a[1], scenarioAxisWords[ax.State]}; got != [2]string{want.Axes[i][0], want.Axes[i][1]} {
			t.Errorf("axis %d reads %v, want %v", i, got, want.Axes[i][:2])
		}
		checkScenarioNote(t, r, a[0], want.Axes[i][2])
		if ax.Reasons == nil {
			t.Errorf("axis %s: reasons are [] when there are none, as the views return them", a[0])
		}
		for _, re := range ax.Reasons {
			switch ax.State {
			case metrics.AxisAttention:
				attention = append(attention, re.Text)
			case metrics.AxisCantJudge:
				cantJudge = append(cantJudge, re.Text)
			}
		}
	}
	if v.Learning != learning {
		t.Errorf("verdict learning %v, want %v (an axis is learning)", v.Learning, learning)
	}
	if got := slices.Concat(attention, cantJudge); !slices.Equal(got, want.Reasons) {
		t.Errorf("banner reasons %q, want %q", got, want.Reasons)
	}
	s := r.Summary
	head := fmt.Sprintf("In the last 7 days: %d prompts got memory, Claude used it in %d turns, ", s.PromptsWithMemory, s.TurnsWithMemoryUsed)
	if !strings.HasPrefix(want.Summary, head) {
		t.Errorf("summary %q does not start with the fixture's counts %q", want.Summary, head)
	}
	switch {
	case s.ExtractionCost != nil:
		if !strings.Contains(want.Summary, fmt.Sprintf("extraction cost $%.2f ", *s.ExtractionCost)) {
			t.Errorf("summary %q does not show the fixture's cost %.2f", want.Summary, *s.ExtractionCost)
		}
	case s.PriceSet: // a price but no cost: the server did not say
		if !strings.Contains(want.Summary, "extraction cost unknown") {
			t.Errorf("summary %q: a cost the server did not give reads unknown", want.Summary)
		}
	default:
		if !strings.Contains(want.Summary, "extraction cost price not set") {
			t.Errorf("summary %q: no price reads price not set", want.Summary)
		}
	}
}

func checkScenarioTiles(t *testing.T, r *metrics.Result, want scenarioWant) {
	t.Helper()
	for i, tile := range scenarioTiles {
		m := r.Metrics[tile[0]]
		learning := r.Verdict.Axes[tile[1]].State == metrics.AxisLearning
		if got := scenarioTileWord(m, learning); got != want.Tiles[i] {
			t.Errorf("tile %d (%s) reads %q, want %q", i, tile[0], got, want.Tiles[i])
		}
	}
}

func checkScenarioAttention(t *testing.T, r *metrics.Result, want scenarioWant) {
	t.Helper()
	var texts []string
	for _, it := range r.Attention {
		texts = append(texts, it.Text)
		if it.Why == "" || it.Command == "" || it.Axis == "" {
			t.Errorf("item %q: want its why, command and axis", it.Text)
		}
	}
	if !slices.Equal(texts, want.Attention) {
		t.Errorf("attention %q, want %q", texts, want.Attention)
	}
	// Each reason of an axis in attention or can't judge is also an item,
	// with the same text, metric, severity and command, on that axis.
	for _, a := range scenarioAxes {
		ax := r.Verdict.Axes[a[0]]
		if ax.State != metrics.AxisAttention && ax.State != metrics.AxisCantJudge {
			continue
		}
		for _, re := range ax.Reasons {
			found := slices.ContainsFunc(r.Attention, func(it metrics.Item) bool {
				return it.Text == re.Text && it.Metric == re.Metric && it.Severity == re.Severity &&
					it.Command == re.Command && it.Axis == a[0]
			})
			if !found {
				t.Errorf("axis %s reason %q has no matching attention item", a[0], re.Text)
			}
		}
	}
}

// checkScenarioSeries: a series has one point per bucket of the window,
// with the bucket's key and partial flag, as the views return it.
func checkScenarioSeries(t *testing.T, id string, series []metrics.Point, buckets []metrics.Bucket) {
	t.Helper()
	if len(series) != len(buckets) {
		t.Errorf("metric %s: %d points for the window's %d buckets", id, len(series), len(buckets))
		return
	}
	for i, p := range series {
		if p.Day != buckets[i].Key || p.Partial != buckets[i].Partial {
			t.Errorf("metric %s point %d: day %s partial %v, want %s %v", id, i, p.Day, p.Partial, buckets[i].Key, buckets[i].Partial)
		}
	}
}

// checkScenarioNote: every figure an axis note states is the fixture's.
func checkScenarioNote(t *testing.T, r *metrics.Result, axis, note string) {
	t.Helper()
	figures, ok := scenarioNoteFigures(r, axis, note)
	if !ok {
		t.Errorf("axis %s note %q: no form of note this check reads", axis, note)
		return
	}
	for _, f := range figures {
		if !strings.Contains(note, f) {
			t.Errorf("axis %s note %q does not state the fixture's %q", axis, note, f)
		}
	}
}

// scenarioNoteFigures are the figures, read from r, that an axis note of
// note's form must state; false for a note no form covers.
func scenarioNoteFigures(r *metrics.Result, axis, note string) ([]string, bool) {
	m := r.Metrics
	pct := func(x *float64) string { return fmt.Sprintf("%d %%", int(math.Round(*x*100))) }
	ax := r.Verdict.Axes[axis]
	switch {
	case note == "day 2 of about 7": // cut from the learning sentence
		return []string{"day 2 of about 7"}, ax.State == metrics.AxisLearning && strings.Contains(ax.Reasons[0].Text, note)
	case note == scJudged28Text:
		return nil, axis == "right" && ax.WindowDays == 28 && slices.Contains(m["used.turn_rate"].Tags, "judged_28d")
	case axis == "found" && strings.Contains(note, " vs "):
		f := m["recall.hit_rate"]
		return []string{pct(f.Value) + " vs " + pct(f.Previous) + " usual"}, true
	case axis == "found":
		return []string{pct(m["recall.hit_rate"].Value)}, true
	case axis == "right" && strings.HasSuffix(note, " turns"):
		c := m["declare.coverage"]
		return []string{fmt.Sprintf("%d of %d turns", *c.Num, *c.Den)}, true
	case axis == "right":
		return []string{pct(m["used.turn_rate"].Value) + " used"}, true
	case axis == "cost":
		v := *m["recall.latency"].Value
		if v < 1000 {
			return []string{fmt.Sprintf("%d ms", int(v))}, true
		}
		return []string{fmt.Sprintf("%.1f s", v/1000)}, true
	case axis == "flowing" && strings.HasSuffix(note, "sessions sent"):
		f := m["pipeline.flow"]
		return []string{fmt.Sprintf("%d of %d sessions sent", *f.Num, *f.Den)}, true
	case axis == "flowing" && strings.Contains(note, " notes and "):
		x := m["local.backlog"].Extra
		return []string{fmt.Sprintf("%d notes and %d sessions waiting", scInt(x["notes_queued"]), scInt(x["sessions_waiting"]))}, true
	case axis == "flowing" && strings.Contains(note, " failed"):
		x := m["pipeline.flow"].Extra
		hours := scInt(x["backlog_oldest_ms"]) / int64(time.Hour/time.Millisecond)
		return []string{fmt.Sprintf("%d failed, oldest waiting %d h", scInt(x["failed"]), hours)}, true
	}
	return nil, false
}

// scInt is an extra's count as the fixtures hold it.
func scInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -1
}
