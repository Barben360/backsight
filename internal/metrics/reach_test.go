package metrics

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/telemetry"
)

// since7 is the start of the fixture's default 7-day window.
var since7 = fxNow.Add(-7 * day)

// injecting makes the first k prompts of a batch inject one memory, the
// rest nothing.
func injecting(k int) func(int, *telemetry.Prompt) {
	return func(i int, p *telemetry.Prompt) {
		if i < k {
			p.Injected = 1
		}
	}
}

func close4(a, b float64) bool { return math.Abs(a-b) < 5e-5 }

func reachOf(x *fx, q Query) map[string]Metric {
	return x.metrics(reachIDs, q)
}

// hitRatePrompts writes TestHitRate's telemetry: 33 recalls of which the
// first 12 injected (chars characters each), 2 machine and 3 typed skips,
// and 20 recalls of the previous window of which 6 injected.
func hitRatePrompts(x *fx, chars int) {
	x.t.Helper()
	x.prompts("alpha", "cur", fxNow, 33, func(i int, p *telemetry.Prompt) {
		if i < 12 {
			p.Injected, p.Chars = 1, chars
		}
	})
	x.prompts("alpha", "mach", fxNow.Add(-40*time.Hour), 2, func(_ int, p *telemetry.Prompt) { p.Decision, p.Reason = "skip", "machine" })
	x.prompts("alpha", "skip", fxNow.Add(-50*time.Hour), 3, func(_ int, p *telemetry.Prompt) { p.Decision, p.Reason = "skip", "gap" })
	x.prompts("alpha", "prev", since7, 20, injecting(6))
}

// TestHitRate: 12 of 33 recalls injected; typed skips and machine prompts
// are not recalls; the previous window is 6 of 20.
func TestHitRate(t *testing.T) {
	x := newFx(t, fxNow)
	hitRatePrompts(x, 0)

	m := reachOf(x, x.query(7))["recall.hit_rate"]
	if m.Num == nil || *m.Num != 12 || m.Den == nil || *m.Den != 33 || m.Value == nil || !close4(*m.Value, 12.0/33) {
		t.Fatalf("hit rate = %+v, want 12/33", m)
	}
	if m.Status != StatusOK || m.Borderline {
		t.Errorf("status = %s borderline=%v, want ok", m.Status, m.Borderline)
	}
	if m.CI == nil || !close4(m.CI[0], 0.2219) || !close4(m.CI[1], 0.5338) {
		t.Errorf("ci = %v, want [0.2219 0.5338]", m.CI)
	}
	if m.MDC == nil || !close4(*m.MDC, 0.2413) {
		t.Errorf("mdc = %v, want 0.2413", m.MDC)
	}
	if m.Previous == nil || !close4(*m.Previous, 0.30) || m.Delta == nil || !close4(*m.Delta, 12.0/33-0.30) {
		t.Errorf("previous = %v delta = %v, want 0.30 and 0.0636", m.Previous, m.Delta)
	}
	if len(m.Series) != 8 {
		t.Fatalf("series has %d points (8 local days), want 8", len(m.Series))
	}
	var num, den int64
	for _, p := range m.Series {
		if p.Day == "" || p.Num == nil || p.Den == nil {
			t.Fatalf("point %+v lacks day, num or den", p)
		}
		num, den = num+*p.Num, den+*p.Den
	}
	if num != 12 || den != 33 {
		t.Errorf("series sums to %d/%d, want 12/33", num, den)
	}
}

// TestHitRateEmptyOneAndFew: the window's trials decide between too_few
// and a figure; a source with no row for the filter is no_data.
func TestHitRateEmptyOneAndFew(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(x *fx)
		status   Status
		reason   string
		den      int64
		value    *float64
		nilCount bool
	}{
		{"empty window, older events", func(x *fx) { x.prompts("alpha", "old", fxNow.Add(-60*day), 5, nil) }, StatusTooFew, "", 0, nil, false},
		{"one recall", func(x *fx) { x.prompts("alpha", "one", fxNow, 1, injecting(1)) }, StatusTooFew, "", 1, f(1), false},
		{"19 recalls", func(x *fx) { x.prompts("alpha", "nin", fxNow, 19, injecting(19)) }, StatusTooFew, "", 19, f(1), false},
		{"20 recalls", func(x *fx) { x.prompts("alpha", "twe", fxNow, 20, injecting(10)) }, StatusOK, "", 20, f(0.5), false},
		{"no prompt row ever", func(x *fx) {}, StatusNoData, ReasonNoEvents, 0, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := newFx(t, fxNow)
			c.setup(x)
			m := reachOf(x, x.query(7))["recall.hit_rate"]
			if m.Status != c.status || m.Reason != c.reason {
				t.Fatalf("status = %s %q, want %s %q", m.Status, m.Reason, c.status, c.reason)
			}
			if c.nilCount {
				if m.Den != nil || m.Num != nil || m.Value != nil {
					t.Errorf("no_data carries numbers: %+v", m)
				}
				return
			}
			if m.Den == nil || *m.Den != c.den {
				t.Errorf("den = %v, want %d", m.Den, c.den)
			}
			switch {
			case c.value == nil && m.Value != nil:
				t.Errorf("value = %v, want null", *m.Value)
			case c.value != nil && (m.Value == nil || !close4(*m.Value, *c.value)):
				t.Errorf("value = %v, want %v", m.Value, *c.value)
			}
		})
	}
}

// TestHitRateRelativeAndFloor: a rate far under its four previous weeks
// is watched; with one prior week only, the absolute floor shows the
// borderline case without a relative verdict.
func TestHitRateRelativeAndFloor(t *testing.T) {
	filled := func(weeks int) *fx {
		x := newFx(t, fxNow)
		inj := []int{8, 9, 10, 7}
		for k := 1; k <= weeks; k++ {
			x.prompts("alpha", fmt.Sprintf("w%d", k), fxNow.Add(-time.Duration(7*k)*day), 20, injecting(inj[k-1]))
		}
		x.prompts("alpha", "cur", fxNow, 40, injecting(2))
		return x
	}
	x4 := filled(4)
	if m := reachOf(x4, x4.query(7))["recall.hit_rate"]; m.Status != StatusWatch {
		t.Errorf("four prior weeks: %s, want watch (the relative rule)", m.Status)
	}
	x1 := filled(1)
	m1 := reachOf(x1, x1.query(7))["recall.hit_rate"]
	if m1.Status != StatusOK || !m1.Borderline {
		t.Errorf("one prior week: %s borderline=%v, want ok, borderline", m1.Status, m1.Borderline)
	}
	if m1.CI == nil || !close4(m1.CI[1], 0.165) {
		t.Errorf("ci = %v, want upper bound 0.165", m1.CI)
	}
}

// TestEmptyReasons: injected-nothing recalls by their reason (unknown on
// older lines), typed skips by theirs.
func TestEmptyReasons(t *testing.T) {
	x := newFx(t, fxNow)
	reasons := []string{"no_candidates", "no_candidates", "below_floor", "all_seen", "capped", "error", ""}
	day1 := fxNow.Add(-30 * time.Hour)
	x.prompts("alpha", "e", day1, len(reasons), func(i int, p *telemetry.Prompt) { p.Empty = reasons[i] })
	x.prompts("alpha", "inj", fxNow.Add(-2*time.Hour), 2, injecting(2))
	skip := func(prefix, reason string, n int) {
		x.prompts("alpha", prefix, fxNow.Add(-60*time.Hour), n, func(_ int, p *telemetry.Prompt) { p.Decision, p.Reason = "skip", reason })
	}
	skip("g", "gap", 2)
	skip("t", "too_short", 1)
	skip("m", "machine", 4) // machine messages are not typed prompts

	m := reachOf(x, x.query(7))["recall.empty_reasons"]
	if m.Value == nil || *m.Value != 7 {
		t.Fatalf("value = %v, want 7", m.Value)
	}
	if got, want := m.Extra["empty"], map[string]int64{"no_candidates": 2, "below_floor": 1, "all_seen": 1, "capped": 1, "error": 1, "unknown": 1}; !sameMap(got, want) {
		t.Errorf("empty = %v, want %v", got, want)
	}
	if got, want := m.Extra["skips"], map[string]int64{"gap": 2, "too_short": 1}; !sameMap(got, want) {
		t.Errorf("skips = %v, want %v", got, want)
	}
	var parts int64
	for _, p := range m.Series {
		for _, n := range p.Parts {
			parts += n
		}
		if p.Value == nil {
			t.Fatalf("point %+v has no value", p)
		}
		var sum int64
		for _, n := range p.Parts {
			sum += n
		}
		if int64(*p.Value) != sum {
			t.Errorf("day %s: value %v but parts %v", p.Day, *p.Value, p.Parts)
		}
	}
	if parts != 7 {
		t.Errorf("series parts sum to %d, want 7", parts)
	}
}

func sameMap(got any, want map[string]int64) bool {
	g, ok := got.(map[string]int64)
	if !ok || len(g) != len(want) {
		return false
	}
	for k, v := range want {
		if g[k] != v {
			return false
		}
	}
	return true
}

// TestSearchRate: typed prompts of at least 40 characters whose turn has
// a main-agent memory_search or memory_read; a store is not a search (the
// rule of stats' Searches line and of the injections followed by a
// search); subagents, other tools, other sessions' turns, short prompts
// and machine messages do not count.
func TestSearchRate(t *testing.T) {
	x := newFx(t, fxNow)
	sess := x.prompts("alpha", "long", fxNow, 25, func(_ int, p *telemetry.Prompt) { p.Plen = 40 })
	at := fxNow.Add(-time.Minute)
	for _, s := range sess[:10] {
		x.toolUse(at, s, "alpha", 1, "mcp__plugin_backsight_memory__memory_search", false)
	}
	x.toolUse(at, sess[10], "alpha", 1, "memory_read", false)
	x.toolUse(at, sess[11], "alpha", 1, "memory_search", true)  // a subagent's
	x.toolUse(at, sess[12], "alpha", 1, "memory_search", true)  // a subagent's
	x.toolUse(at, sess[13], "alpha", 1, "memory_store", false)  // a store is no search
	x.toolUse(at, sess[14], "alpha", 1, "Read", false)          // not a memory tool
	x.toolUse(at, sess[15], "alpha", 2, "memory_search", false) // another turn
	x.toolUse(at, "elsewhere", "alpha", 1, "memory_search", false)
	// A 39-character prompt with a search, and a long machine message.
	short := x.prompts("alpha", "short", fxNow.Add(-5*time.Hour), 1, func(_ int, p *telemetry.Prompt) { p.Plen = 39 })
	x.toolUse(at, short[0], "alpha", 1, "memory_search", false)
	mach := x.prompts("alpha", "mach", fxNow.Add(-6*time.Hour), 1, func(_ int, p *telemetry.Prompt) { p.Decision, p.Reason = "skip", "machine" })
	x.toolUse(at, mach[0], "alpha", 1, "memory_search", false)

	// A tool use 4 h after its prompt still belongs to the prompt's turn; one
	// of a prompt outside the window counts for nothing.
	late := x.prompts("alpha", "late", fxNow.Add(-10*time.Hour), 1, nil)
	x.toolUse(fxNow.Add(-6*time.Hour), late[0], "alpha", 1, "memory_search", false)
	old := x.prompts("alpha", "old", fxNow.Add(-20*day), 1, nil)
	x.toolUse(fxNow.Add(-time.Hour), old[0], "alpha", 1, "memory_search", false)

	m := reachOf(x, x.query(7))["search.rate"]
	// 10 main-agent searches + 1 memory_read = 11 prompts, plus the late one.
	if m.Num == nil || *m.Num != 12 || m.Den == nil || *m.Den != 26 {
		t.Fatalf("search rate = %v/%v, want 12/26 (value %v)", m.Num, m.Den, m.Value)
	}
	if m.Value == nil || !close4(*m.Value, 12.0/26) {
		t.Errorf("value = %v, want 12/26", m.Value)
	}
}

// TestSearchRateToolUseAfterWindow: a tool use belongs to its prompt's turn
// however long after the window's end it was logged; the join has no time
// bound.
func TestSearchRateToolUseAfterWindow(t *testing.T) {
	x := newFx(t, fxNow)
	until := fxNow.Add(-2 * day)
	w, err := NewWindow(fxNow, 0, until.Add(-day), until, fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// The prompt is inside the window; its search comes 5 h after Until.
	p := x.prompts("alpha", "edge", until, 1, func(_ int, p *telemetry.Prompt) { p.Plen = 40 })
	x.toolUse(until.Add(5*time.Hour), p[0], "alpha", 1, "memory_search", false)

	m := reachOf(x, Query{Window: w, Machines: "this"})["search.rate"]
	if m.Num == nil || *m.Num != 1 || m.Den == nil || *m.Den != 1 {
		t.Fatalf("search rate = %v/%v, want 1/1", m.Num, m.Den)
	}
}

// TestSearchEmptyRateExcludesFailures: failed calls are in neither the
// numerator nor the denominator; other tools are ignored.
func TestSearchEmptyRateExcludesFailures(t *testing.T) {
	x := newFx(t, fxNow)
	for i := range 29 {
		var ev telemetry.Tool
		switch {
		case i < 5:
			ev = telemetry.Tool{Name: "memory_search", OK: true, Hits: 0}
		case i < 20:
			ev = telemetry.Tool{Name: "memory_search", OK: true, Hits: 3}
		case i < 24:
			ev = telemetry.Tool{Name: "memory_search", OK: false}
		default:
			ev = telemetry.Tool{Name: "memory_read", OK: true, Hits: 0}
		}
		x.tel(fxNow.Add(-time.Duration(i+1)*time.Hour), ev, "s", "alpha")
	}
	m := reachOf(x, x.query(7))["search.empty_rate"]
	if m.Num == nil || *m.Num != 5 || m.Den == nil || *m.Den != 20 || m.Value == nil || !close4(*m.Value, 0.25) {
		t.Fatalf("empty rate = %v/%v (%v), want 5/20 = 0.25", m.Num, m.Den, m.Value)
	}
	if m.Extra["failed"] != int64(4) {
		t.Errorf("failed = %v, want 4", m.Extra["failed"])
	}
	if m.Status != StatusOK {
		t.Errorf("status = %s, want ok", m.Status)
	}
}

// Beside the rate: the queries the successful searches asked, how many
// found none of the hits returned, and the characters a search returned
// on average. A call written before those fields counts in none of them;
// a failed one is counted apart.
func TestSearchEmptyRateCountsQueriesAndAnswerSize(t *testing.T) {
	x := newFx(t, fxNow)
	at := func(h int) time.Time { return fxNow.Add(-time.Duration(h) * time.Hour) }
	x.tel(at(1), telemetry.Tool{Name: "memory_search", OK: true, Hits: 2, QueriesN: 3, QueryHits: []int{2, 0, 0}, ResultChars: 900}, "s", "alpha")
	x.tel(at(2), telemetry.Tool{Name: "memory_search", OK: true, QueriesN: 1, QueryHits: []int{0}, ResultChars: 100}, "s", "alpha")
	x.tel(at(3), telemetry.Raw{Name: "tool", Fields: map[string]any{"name": "memory_search", "ok": true, "hits": 1}}, "s", "alpha")
	x.tel(at(4), telemetry.Tool{Name: "memory_search", QueriesN: 2, QueryHits: []int{0, 0}, ResultChars: 50}, "s", "alpha")
	x.tel(at(5), telemetry.Tool{Name: "memory_read", OK: true, ResultChars: 4000}, "s", "alpha")
	m := reachOf(x, x.query(7))["search.empty_rate"]
	if m.Extra["queries"] != int64(4) || m.Extra["queries_none_returned"] != int64(3) || m.Extra["chars_per_search"] != int64(500) || m.Extra["failed"] != int64(1) {
		t.Errorf("extra %v, want 4 queries, 3 empty, 500 chars per search, 1 failed", m.Extra)
	}
	y := newFx(t, fxNow)
	y.tel(at(1), telemetry.Raw{Name: "tool", Fields: map[string]any{"name": "memory_search", "ok": true, "hits": 1}}, "s", "alpha")
	if m := reachOf(y, y.query(7))["search.empty_rate"]; m.Extra["queries"] != int64(0) || m.Extra["chars_per_search"] != nil {
		t.Errorf("before the fields: extra %v, want 0 queries and no average", m.Extra)
	}
}

// TestByLang: each language is judged on its own trials; a missing
// language is "unknown".
func TestByLang(t *testing.T) {
	x := newFx(t, fxNow)
	lang := func(l string, n, inj int) {
		x.prompts("alpha", "l"+l, fxNow.Add(-time.Duration(len(l))*time.Hour), n, func(i int, p *telemetry.Prompt) {
			p.Lang = l
			if i < inj {
				p.Injected = 1
			}
		})
	}
	lang("fr", 20, 9)
	lang("en", 10, 3)
	x.prompts("alpha", "nolang", fxNow.Add(-9*time.Hour), 1, nil)

	m := reachOf(x, x.query(7))["recall.by_lang"]
	bl, ok := m.Extra["by_lang"].(map[string]langStat)
	if !ok {
		t.Fatalf("by_lang = %T", m.Extra["by_lang"])
	}
	fr, en, un := bl["fr"], bl["en"], bl[unknownKey]
	if fr.Num != 9 || fr.Den != 20 || !close4(fr.Value, 0.45) || fr.Status != StatusOK || fr.CI == nil {
		t.Errorf("fr = %+v, want 9/20 0.45 ok with an interval", fr)
	}
	if en.Num != 3 || en.Den != 10 || en.Status != StatusTooFew {
		t.Errorf("en = %+v, want 3/10 too_few", en)
	}
	if un.Den != 1 || un.Status != StatusTooFew || len(bl) != 3 {
		t.Errorf("unknown = %+v of %d languages, want 1 prompt too_few of 3", un, len(bl))
	}
}

// TestProjectFilterUsesTheSessionsProject: activity metrics count only
// the rows of the asked project, and carry no not_filtered tag.
func TestProjectFilterUsesTheSessionsProject(t *testing.T) {
	x := newFx(t, fxNow)
	as := x.prompts("alpha", "a", fxNow, 25, injecting(5))
	bs := x.prompts("beta", "b", fxNow, 30, injecting(30))
	x.toolUse(fxNow, as[0], "alpha", 1, "memory_search", false)
	x.toolUse(fxNow, as[1], "alpha", 1, "memory_search", false)
	for _, s := range bs[:3] {
		x.toolUse(fxNow, s, "beta", 1, "memory_search", false)
	}
	for i := range 21 {
		x.tel(fxNow.Add(-time.Duration(i+1)*time.Hour), telemetry.Tool{Name: "memory_search", OK: true, Hits: i % 2}, "sa", "alpha")
		x.tel(fxNow.Add(-time.Duration(i+1)*time.Hour), telemetry.Tool{Name: "memory_search", OK: true, Hits: 1}, "sb", "beta")
	}
	q := x.query(7)
	q.Project = "alpha"
	m := reachOf(x, q)
	if hr := m["recall.hit_rate"]; *hr.Num != 5 || *hr.Den != 25 {
		t.Errorf("alpha hit rate = %d/%d, want 5/25", *hr.Num, *hr.Den)
	}
	if sr := m["search.rate"]; *sr.Num != 2 || *sr.Den != 25 {
		t.Errorf("alpha search rate = %d/%d, want 2/25", *sr.Num, *sr.Den)
	}
	if er := m["search.empty_rate"]; *er.Num != 11 || *er.Den != 21 {
		t.Errorf("alpha empty rate = %d/%d, want 11/21", *er.Num, *er.Den)
	}
	for id, mm := range m {
		if len(mm.Tags) != 0 {
			t.Errorf("%s tags = %v, want none", id, mm.Tags)
		}
	}
	q.Project = "gamma"
	if hr := reachOf(x, q)["recall.hit_rate"]; hr.Status != StatusNoData || hr.Reason != ReasonNoEvents {
		t.Errorf("a project without events: %s %q, want no_data no_events", hr.Status, hr.Reason)
	}
	if all := reachOf(x, x.query(7))["recall.hit_rate"]; *all.Den != 55 {
		t.Errorf("no filter: den = %d, want 55", *all.Den)
	}
}

// TestReachNoData: a server-only host reads server_only_host; a table
// with no row reads no_events, apart from the other.
func TestReachNoData(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "a", fxNow, 25, injecting(5))
	m := reachOf(x, x.query(7))
	if s := m["search.empty_rate"]; s.Status != StatusNoData || s.Reason != ReasonNoEvents {
		t.Errorf("search.empty_rate without tool rows: %s %q, want no_data no_events", s.Status, s.Reason)
	}
	if s := m["recall.hit_rate"]; s.Status == StatusNoData {
		t.Errorf("recall.hit_rate = no_data, prompts exist")
	}

	z := newFx(t, fxNow)
	z.prompts("alpha", "a", fxNow, 25, injecting(5))
	z.tel(fxNow.Add(-time.Hour), telemetry.Tool{Name: "memory_read", OK: true}, "s", "alpha")
	if s := reachOf(z, z.query(7))["search.empty_rate"]; s.Status != StatusTooFew || s.Den == nil || *s.Den != 0 {
		t.Errorf("search.empty_rate with only memory_read rows: %s den %v, want too_few, den 0", s.Status, s.Den)
	}

	y := newFx(t, fxNow)
	y.prompts("alpha", "a", fxNow, 25, injecting(5))
	y.tel(fxNow.Add(-time.Hour), telemetry.Tool{Name: "memory_search", OK: true}, "s", "alpha")
	y.cfg = func(c *config.Config) { c.Server.Role = config.RoleServer }
	for id, mm := range reachOf(y, y.query(7)) {
		if mm.Status != StatusNoData || mm.Reason != ReasonServerOnlyHost {
			t.Errorf("%s on a server-only host: %s %q, want no_data server_only_host", id, mm.Status, mm.Reason)
		}
	}
}

// TestMonthCrossingWindow: a window over two telemetry month files counts
// each prompt once.
func TestMonthCrossingWindow(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "sep", time.Date(2026, 10, 1, 0, 0, 0, 0, fxParis), 12, injecting(6)) // 09-30, September's file
	x.prompts("alpha", "oct", time.Date(2026, 10, 4, 0, 0, 0, 0, fxParis), 8, injecting(2))  // October's file
	x.prompts("alpha", "out", time.Date(2026, 10, 7, 0, 0, 0, 0, fxParis), 5, injecting(5))  // after the window
	for _, f := range []string{"09", "10"} {
		if _, err := os.Stat(filepath.Join(x.dirs.TelemetryDir(), "events-2026-"+f+".jsonl")); err != nil {
			t.Fatalf("fixture lacks month %s: %v", f, err)
		}
	}
	w, err := NewWindow(x.now, 0, time.Date(2026, 9, 28, 0, 0, 0, 0, fxParis), time.Date(2026, 10, 5, 0, 0, 0, 0, fxParis), fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	m := reachOf(x, Query{Window: w, Machines: "this"})["recall.hit_rate"]
	if m.Num == nil || *m.Num != 8 || *m.Den != 20 {
		t.Errorf("hit rate over the month change = %v/%v, want 8/20", m.Num, m.Den)
	}
}

// TestRotatedFileKeepsItsRows: removing a month's file after it was
// ingested does not remove its rows from the metrics.
func TestRotatedFileKeepsItsRows(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "oct", fxNow, 24, injecting(12))
	if m := reachOf(x, x.query(7))["recall.hit_rate"]; *m.Den != 24 {
		t.Fatalf("before rotation: den = %d, want 24", *m.Den)
	}
	if err := os.Remove(filepath.Join(x.dirs.TelemetryDir(), "events-2026-10.jsonl")); err != nil {
		t.Fatal(err)
	}
	m := reachOf(x, x.query(7))["recall.hit_rate"]
	if m.Num == nil || *m.Num != 12 || *m.Den != 24 {
		t.Errorf("after rotation: %v/%v, want 12/24", m.Num, m.Den)
	}
}

// TestPromptsReadOnceKeepWindowBoundaries: one read of the prompts serves
// the window and the one before it, each prompt in exactly one window
// (Since included, Until excluded), a search logged in the current window
// still crediting a prompt of the previous one.
func TestPromptsReadOnceKeepWindowBoundaries(t *testing.T) {
	x := newFx(t, fxNow)
	w, err := NewWindow(fxNow, 7, time.Time{}, time.Time{}, fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	prompt := func(at time.Time, session string, injected int) {
		x.tel(at, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Injected: injected}, session, "alpha")
	}
	prompt(w.Since, "at-since", 1)                    // current
	prompt(w.Since.Add(-time.Millisecond), "prev", 0) // previous
	prompt(w.Since.Add(time.Hour), "inside", 0)       // current
	prompt(w.Until, "at-until", 1)                    // neither
	x.toolUse(w.Since.Add(2*time.Hour), "prev", "alpha", 1, "memory_search", false)
	x.toolUse(w.Since.Add(3*time.Hour), "inside", "alpha", 1, "memory_search", false)

	var cur, prev map[string]Metric
	err = withTx(context.Background(), x.source(), Query{Window: w, Machines: "this"}, x.need, func(e env) error {
		var err error
		if _, cur, err = reach(e, w); err != nil {
			return err
		}
		_, prev, err = reach(e, w.Previous()) // served by the first read
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if h, p := cur["recall.hit_rate"], prev["recall.hit_rate"]; num(h) != 1 || den(h) != 2 || num(p) != 0 || den(p) != 1 {
		t.Errorf("hit rate = %d/%d, previous %d/%d; want 1/2 and 0/1 (Since in, Until out)", num(h), den(h), num(p), den(p))
	}
	if s, p := cur["search.rate"], prev["search.rate"]; num(s) != 1 || den(s) != 2 || num(p) != 1 || den(p) != 1 {
		t.Errorf("search rate = %d/%d, previous %d/%d; want 1/2 and 1/1 (the search logged after the previous window's end)", num(s), den(s), num(p), den(p))
	}
}
