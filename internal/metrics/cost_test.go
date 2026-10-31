package metrics

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The cost family's fixtures. Window: the 7 days before fxNow
// (2026-10-30T12:00+01:00), so [10-23T12:00+02, 10-30T12:00+01).

// costFx is a fixture whose server reads are the cost family's.
func costFx(t *testing.T) *fx {
	x := newFx(t, fxNow)
	x.need = CostServerNeeds()
	return x
}

func costOf(x *fx, q Query) map[string]Metric { return x.metrics(costIDs, q) }

// ago is fxNow - d, to the minute.
func ago(d time.Duration) time.Time { return fxNow.Add(-d) }

// pf reads a *float64 extra, ok false when null.
func pf(t *testing.T, m Metric, key string) (float64, bool) {
	t.Helper()
	p, isPtr := m.Extra[key].(*float64)
	if !isPtr {
		t.Fatalf("extra.%s = %#v, want a *float64", key, m.Extra[key])
	}
	if p == nil {
		return 0, false
	}
	return *p, true
}

// TestContextPerSession: 120 sessions, session i (1..120) with a briefing
// of 100 chars and a typed prompt of 100·i - 100, so 100·i chars in all:
// 100, 200, … 12,000. Nearest rank: p95 is the 114th (ceil(0.95·120)) =
// 11,400, p50 the 60th = 6,000, tokens_p95 = 11,400/4 = 2,850. The default
// budget 6,000 gives a watch at 9,000: 11,400 > 9,000, watch. A budget of
// 8,000 gives 12,000: ok. A machine prompt is not counted.
func TestContextPerSession(t *testing.T) {
	fill := func(x *fx, sessions int) {
		for i := 1; i <= sessions; i++ {
			s := fmt.Sprintf("s-%d", i)
			ts := ago(time.Duration(i) * time.Hour)
			x.tel(ts.Add(-time.Minute), telemetry.Briefing{Chars: 100, Source: "raw"}, s, "alpha")
			x.tel(ts, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Chars: 100*i - 100}, s, "alpha")
		}
		x.tel(ago(time.Hour), telemetry.Prompt{N: 2, Decision: "recall", Reason: "machine", Chars: 99999}, "s-1", "alpha")
	}
	t.Run("watch", func(t *testing.T) {
		x := costFx(t)
		fill(x, 120)
		m := costOf(x, x.query(7))["context.per_session"]
		if p, ok := pf(t, m, "p95"); !ok || p != 11400 || val(m) != 11400 {
			t.Errorf("p95 = %v (value %v), want 11,400", p, val(m))
		}
		if p, ok := pf(t, m, "p50"); !ok || p != 6000 {
			t.Errorf("p50 = %v, want 6,000", p)
		}
		if tp := m.Extra["tokens_p95"].(*int64); tp == nil || *tp != 2850 {
			t.Errorf("tokens_p95 = %v, want 2,850", tp)
		}
		if tp := m.Extra["tokens_p50"].(*int64); tp == nil || *tp != 1500 {
			t.Errorf("tokens_p50 = %v, want 1,500", tp)
		}
		if m.Status != StatusWatch || den(m) != 120 {
			t.Errorf("status %s, n %d, want watch, 120", m.Status, den(m))
		}
		h := m.Extra["histogram"].([]histBucket)
		// bucket k holds [1000k, 1000k+1000): k = 0 has 100..900 (9 sessions),
		// k = 1..11 ten each (1000k, 1000k+100, … +900), k = 12 only 12,000;
		// the open bucket from 20,000 is empty.
		if len(h) != 21 || h[0].N != 9 || h[1].N != 10 || h[11].N != 10 || h[12].N != 1 || h[13].N != 0 || h[20].Hi != nil || h[20].Lo != 20000 || h[0].Hi == nil || *h[0].Hi != 1000 {
			t.Errorf("histogram = %+v", h)
		}
	})
	t.Run("budget 8000 gives limit 12000", func(t *testing.T) {
		x := costFx(t)
		x.cfg = func(c *config.Config) { c.Recall.SessionBudgetChars = 8000 }
		fill(x, 120)
		if m := costOf(x, x.query(7))["context.per_session"]; m.Status != StatusOK {
			t.Errorf("status %s, want ok: 11,400 is under 1.5 × 8,000", m.Status)
		}
	})
	t.Run("99 sessions", func(t *testing.T) {
		x := costFx(t)
		fill(x, 99)
		m := costOf(x, x.query(7))["context.per_session"]
		// sessions 1..99: p50 = 50th = 5,000
		if m.Status != StatusTooFew || m.Value != nil {
			t.Errorf("status %s value %v, want too_few, null", m.Status, m.Value)
		}
		if p, ok := pf(t, m, "p50"); !ok || p != 5000 {
			t.Errorf("p50 = %v %v, want 5,000", p, ok)
		}
		if _, ok := pf(t, m, "p95"); ok {
			t.Error("p95 set under 100 sessions")
		}
	})
}

// TestContextPerSessionSeries: two days, 10-28 holds session a (briefing
// 300, prompt 200), 10-29 holds sessions a (prompt 50) and b (briefing 70):
// parts per day {briefing, prompts}, n = sessions active that day.
func TestContextPerSessionSeries(t *testing.T) {
	x := costFx(t)
	d1 := time.Date(2026, 10, 28, 10, 0, 0, 0, fxParis)
	d2 := time.Date(2026, 10, 29, 10, 0, 0, 0, fxParis)
	x.tel(d1, telemetry.Briefing{Chars: 300, Source: "raw"}, "a", "alpha")
	x.tel(d1.Add(time.Minute), telemetry.Prompt{N: 1, Decision: "recall", Chars: 200}, "a", "alpha")
	x.tel(d2, telemetry.Prompt{N: 2, Decision: "recall", Chars: 50}, "a", "alpha")
	x.tel(d2, telemetry.Briefing{Chars: 70, Source: "raw"}, "b", "alpha")
	m := costOf(x, x.query(7))["context.per_session"]
	got := map[string]Point{}
	for _, p := range m.Series {
		got[p.Day] = p
	}
	if p := got["2026-10-28"]; p.Parts["briefing"] != 300 || p.Parts["prompts"] != 200 || *p.N != 1 {
		t.Errorf("10-28 = %+v, want briefing 300, prompts 200, n 1", p)
	}
	if p := got["2026-10-29"]; p.Parts["briefing"] != 70 || p.Parts["prompts"] != 50 || *p.N != 2 {
		t.Errorf("10-29 = %+v, want briefing 70, prompts 50, n 2", p)
	}
	if len(m.Series) != 8 {
		t.Errorf("%d points, want 8 days", len(m.Series))
	}
}

// TestContextPerPrompt: 40 recalls that injected, chars 100, 200, … 4,000
// (p50 = the 20th = 2,000), 10 recalls that injected nothing (chars 0, not
// counted). n 40 < 100: p95 null. 19 recalls: too_few.
func TestContextPerPrompt(t *testing.T) {
	fill := func(x *fx, hits int) {
		for i := 1; i <= hits; i++ {
			x.tel(ago(time.Duration(i)*time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Chars: 100 * i}, fmt.Sprintf("s-%d", i), "alpha")
		}
		for i := range 10 {
			x.tel(ago(time.Duration(i+1)*time.Hour+time.Minute), telemetry.Prompt{N: 2, Decision: "recall", Chars: 0}, fmt.Sprintf("e-%d", i), "alpha")
		}
	}
	x := costFx(t)
	fill(x, 40)
	m := costOf(x, x.query(7))["context.per_prompt"]
	if val(m) != 2000 || den(m) != 40 || m.Status != StatusOK {
		t.Errorf("value %v den %d status %s, want 2,000, 40, ok", val(m), den(m), m.Status)
	}
	if p, ok := pf(t, m, "p50"); !ok || p != 2000 {
		t.Errorf("p50 = %v", p)
	}
	if _, ok := pf(t, m, "p95"); ok {
		t.Error("p95 set under 100 recalls")
	}
	x = costFx(t)
	fill(x, 19)
	if m := costOf(x, x.query(7))["context.per_prompt"]; m.Status != StatusTooFew || m.Value != nil {
		t.Errorf("19 recalls: status %s value %v, want too_few, null", m.Status, m.Value)
	}
}

// TestRecallLatency: 100 recalls, ms 30, 60, … 3,000; the first 60 (30..
// 1,800) on 10-28, the other 40 (1,830..3,000) on 10-29. Overall p95 = the
// 95th = 2,850 (bad: over 2,500), p50 = the 50th = 1,500. 10-28: p50 = the
// 30th = 900, p95 = the 57th = 1,710, n 60; 10-29: p50 = the 20th (value
// 1,800 + 30·20) = 2,400, p95 = the 38th = 2,940, n 40. A skip with a huge
// ms is ignored.
func TestRecallLatency(t *testing.T) {
	x := costFx(t)
	d1 := time.Date(2026, 10, 28, 8, 0, 0, 0, fxParis)
	d2 := time.Date(2026, 10, 29, 8, 0, 0, 0, fxParis)
	for i := 1; i <= 100; i++ {
		ts := d1.Add(time.Duration(i) * time.Minute)
		if i > 60 {
			ts = d2.Add(time.Duration(i-60) * time.Minute)
		}
		x.tel(ts, telemetry.Prompt{N: i, Decision: "recall", MS: 30 * i}, "s", "alpha")
	}
	x.tel(d1, telemetry.Prompt{N: 200, Decision: "skip", Reason: "short", MS: 99999}, "s", "alpha")
	m := costOf(x, x.query(7))["recall.latency"]
	if val(m) != 2850 || m.Status != StatusBad || den(m) != 100 {
		t.Fatalf("value %v status %s n %d, want 2,850 bad 100", val(m), m.Status, den(m))
	}
	if p, _ := pf(t, m, "p50"); p != 1500 {
		t.Errorf("p50 = %v, want 1,500", p)
	}
	byDay := map[string]Point{}
	for _, p := range m.Series {
		byDay[p.Day] = p
	}
	for day, want := range map[string][3]float64{"2026-10-28": {900, 1710, 60}, "2026-10-29": {2400, 2940, 40}} {
		p := byDay[day]
		if p.P50 == nil || p.P95 == nil || *p.P50 != want[0] || *p.P95 != want[1] || *p.N != int64(want[2]) {
			t.Errorf("%s = %+v, want p50 %v p95 %v n %v", day, p, want[0], want[1], want[2])
		}
	}
	if p := byDay["2026-10-27"]; p.N == nil || *p.N != 0 || p.P50 != nil || p.P95 != nil {
		t.Errorf("empty day = %+v, want n 0 and no percentiles", p)
	}
	if len(m.Series) != 8 {
		t.Errorf("%d points, want 8", len(m.Series))
	}
}

// TestSearchLatencyOkOnly: 100 ok searches at 1,000 ms, 5 failed at
// 10,000 ms and a memory_read at 50,000 ms: p95 = 1,000, ok (watch above
// 3,000).
func TestSearchLatencyOkOnly(t *testing.T) {
	x := costFx(t)
	for i := range 100 {
		x.tel(ago(time.Duration(i+1)*time.Hour), telemetry.Tool{Name: "memory_search", MS: 1000, OK: true, Hits: 1}, "s", "alpha")
	}
	for i := range 5 {
		x.tel(ago(time.Duration(i+1)*time.Hour+time.Minute), telemetry.Tool{Name: "memory_search", MS: 10000, OK: false}, "s", "alpha")
	}
	x.tel(ago(time.Hour), telemetry.Tool{Name: "memory_read", MS: 50000, OK: true}, "s", "alpha")
	m := costOf(x, x.query(7))["search.latency"]
	if val(m) != 1000 || m.Status != StatusOK || den(m) != 100 {
		t.Errorf("value %v status %s n %d, want 1,000 ok 100", val(m), m.Status, den(m))
	}
}

// pricesSet sets the [llm] prices 0.27 / 0.07 / 1.10 per million (USD).
func pricesSet(x *fx) {
	x.cfg = func(c *config.Config) {
		c.LLM.PriceInput, c.LLM.PriceCached, c.LLM.PriceOutput, c.LLM.Currency = 0.27, 0.07, 1.10, "USD"
	}
}

// extractionServer fills the fake. All operations: day A (10-28) input
// 900,000, cached 400,000, output 100,000, thoughts 50,000; day B (10-29)
// input 600,000, output 200,000; day C (10-20, outside the 7-day window)
// input 5,000,000. retain_extract_facts: day A as above, day B input
// 300,000, output 100,000. consolidation: day B input 200,000, output
// 60,000. Nothing for reflect. What is left of day B, 100,000 input and
// 40,000 output, is "other".
func extractionServer(x *fx) {
	a := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	b := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	out := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	x.server.set(func(d *fakeData) {
		d.LLM = map[string][]hindsight.LLMBucket{
			"": {{Time: out, Input: 5_000_000}, {Time: a, Input: 900_000, Cached: 400_000, Output: 100_000, Thoughts: 50_000},
				{Time: b, Input: 600_000, Output: 200_000}},
			"retain_extract_facts": {{Time: a, Input: 900_000, Cached: 400_000, Output: 100_000, Thoughts: 50_000},
				{Time: b, Input: 300_000, Output: 100_000}},
			"consolidation": {{Time: b, Input: 200_000, Output: 60_000}},
		}
	})
}

// TestExtractionTokensAndCost. In the window (days A and B; C is out), all
// operations: input 900,000 + 600,000 = 1,500,000, cached 400,000, output
// 100,000 + 50,000 (thoughts, billed as output) + 200,000 = 350,000;
// tokens = input + output = 1,500,000 + 350,000 = 1,850,000 (the input
// includes the 400,000 cached, which is a subset, never added).
// Cost, all: (1,500,000 - 400,000) × 0.27 = 0.297; 400,000 × 0.07 = 0.028;
// 350,000 × 1.10 = 0.385; total 0.710.
// retain_extract_facts: input 1,200,000, cached 400,000, output 250,000:
// 800,000 × 0.27 = 0.216 + 0.028 + 250,000 × 1.10 = 0.275 = 0.519.
// consolidation: input 200,000, output 60,000: 0.054 + 0.066 = 0.120.
// other = all - named: input 100,000, cached 0, output 40,000: 0.027 +
// 0.044 = 0.071 (0.519 + 0.120 + 0.071 = 0.710). reflect: zeros.
// Series: day A 900,000 + 150,000 = 1,050,000, day B 600,000 + 200,000 =
// 800,000;
// cost on day B 600,000 × 0.27 + 200,000 × 1.10 = 0.162 + 0.22 = 0.382.
func TestExtractionTokensAndCost(t *testing.T) {
	x := costFx(t)
	pricesSet(x)
	extractionServer(x)
	m := costOf(x, x.query(7))
	tk, cs := m["extraction.tokens"], m["extraction.cost"]
	if val(tk) != 1_850_000 {
		t.Errorf("tokens = %v, want 1,850,000 (the day outside the window dropped)", val(tk))
	}
	for k, want := range map[string]int64{"input": 1_500_000, "cached": 400_000, "output": 350_000} {
		if tk.Extra[k] != want {
			t.Errorf("tokens extra.%s = %v, want %d", k, tk.Extra[k], want)
		}
	}
	if tk.Extra["days_covered"] != 2 {
		t.Errorf("days_covered = %v, want 2", tk.Extra["days_covered"])
	}
	if math.Abs(val(cs)-0.710) > 1e-9 || cs.Extra["price_set"] != true || cs.Extra["currency"] != "USD" {
		t.Errorf("cost = %v (%v %v), want 0.710 USD", val(cs), cs.Extra["price_set"], cs.Extra["currency"])
	}
	ops := tk.Extra["by_operation"].(map[string]any)
	for op, want := range map[string][4]float64{
		"retain_extract_facts": {1_200_000, 400_000, 250_000, 0.519},
		"consolidation":        {200_000, 0, 60_000, 0.120},
		"other":                {100_000, 0, 40_000, 0.071},
		"reflect":              {0, 0, 0, 0},
	} {
		o := ops[op].(map[string]any)
		if o["input"] != int64(want[0]) || o["cached"] != int64(want[1]) || o["output"] != int64(want[2]) || math.Abs(*o["cost"].(*float64)-want[3]) > 1e-9 {
			t.Errorf("by_operation.%s = %v, want %v", op, o, want)
		}
	}
	byDay, costDay := map[string]float64{}, map[string]float64{}
	for _, p := range tk.Series {
		byDay[p.Day] = *p.Value
	}
	for _, p := range cs.Series {
		costDay[p.Day] = *p.Value
	}
	if byDay["2026-10-28"] != 1_050_000 || byDay["2026-10-29"] != 800_000 || byDay["2026-10-27"] != 0 || len(tk.Series) != 8 {
		t.Errorf("series = %v", byDay)
	}
	if math.Abs(costDay["2026-10-29"]-0.382) > 1e-9 {
		t.Errorf("cost on 10-29 = %v, want 0.382", costDay["2026-10-29"])
	}
	if !slices.Contains(x.server.called(), "LLMStats?operation=consolidation&period=7d") {
		t.Errorf("calls %v: no 7d read of consolidation", x.server.called())
	}
}

// TestExtractionPriceNotSet: no price and a model without a price: the
// cost is null with price_set false, the tokens are still there.
func TestExtractionPriceNotSet(t *testing.T) {
	x := costFx(t)
	x.cfg = func(c *config.Config) { c.LLM.Model = "no-such-model" }
	extractionServer(x)
	m := costOf(x, x.query(7))
	cs := m["extraction.cost"]
	if cs.Value != nil || cs.Extra["price_set"] != false {
		t.Errorf("cost = %v price_set %v, want null, false", cs.Value, cs.Extra["price_set"])
	}
	if val(m["extraction.tokens"]) != 1_850_000 {
		t.Errorf("tokens = %v, want 1,850,000", val(m["extraction.tokens"]))
	}
	for _, p := range cs.Series {
		if p.Value != nil {
			t.Errorf("cost series point %+v has a value without a price", p)
		}
	}
	if c := cs.Extra["by_operation"].(map[string]any)["consolidation"].(map[string]any)["cost"].(*float64); c != nil {
		t.Errorf("per-operation cost = %v, want null", *c)
	}
}

// TestExtractionNothingInWindow: the server answers with days outside the
// window only: no_data, no_events (not a zero).
func TestExtractionNothingInWindow(t *testing.T) {
	x := costFx(t)
	x.server.set(func(d *fakeData) {
		d.LLM = map[string][]hindsight.LLMBucket{"": {{Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Input: 1}}}
	})
	for _, id := range []string{"extraction.tokens", "extraction.cost"} {
		if m := costOf(x, x.query(7))[id]; m.Status != StatusNoData || m.Reason != ReasonNoEvents {
			t.Errorf("%s: %s/%s, want no_data/no_events", id, m.Status, m.Reason)
		}
	}
}

// TestExtractionProjectFilter: server numbers are not filtered by project.
func TestExtractionProjectFilter(t *testing.T) {
	x := costFx(t)
	extractionServer(x)
	q := x.query(7)
	q.Project = "alpha"
	m := costOf(x, q)
	for _, id := range []string{"extraction.tokens", "extraction.cost", "extraction.per_session"} {
		if !slices.Contains(m[id].Tags, tagNotFiltered) {
			t.Errorf("%s tags %v, want not_filtered", id, m[id].Tags)
		}
	}
	if val(m["extraction.tokens"]) != 1_850_000 {
		t.Errorf("tokens = %v: the project filter must not change a server figure", val(m["extraction.tokens"]))
	}
}

// retained writes a retain of document with status at ts.
func retained(x *fx, ts time.Time, doc, status string) {
	x.tel(ts, telemetry.Retain{Document: doc, Items: 1, Status: status, MS: 10}, "s-"+doc, "alpha")
}

// TestExtractionPerSession: LLMStats("30d") with tokens: 10-28 700,000
// (input 500,000, of which cached 100,000, + output 200,000) and 10-10 500,000
// (input 300,000, output 200,000), both in the last 28 days (since
// 10-02T11:00Z); 10-01 300,000, day 29, out. 1,200,000 over the 4
// documents last ok in the last 28 days = 300,000. Not counted: a failed
// document, a document ok then failed (last status failed), a document ok
// 40 days ago. The same under --days 7 (the 30-day read serves it).
func TestExtractionPerSession(t *testing.T) {
	x := costFx(t)
	x.server.set(func(d *fakeData) {
		d.LLM = map[string][]hindsight.LLMBucket{"": {
			{Time: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Input: 500_000, Cached: 100_000, Output: 200_000},
			{Time: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), Input: 300_000, Output: 200_000},
			{Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Input: 300_000},
		}}
	})
	for i := range 4 {
		retained(x, ago(time.Duration(i+1)*24*time.Hour), fmt.Sprintf("ok%d", i), "ok")
	}
	retained(x, ago(2*time.Hour), "failed", "failed")
	retained(x, ago(10*24*time.Hour), "flip", "ok")
	retained(x, ago(9*24*time.Hour), "flip", "failed")
	retained(x, ago(40*24*time.Hour), "old", "ok")
	for _, days := range []int{7, 28} {
		m := costOf(x, x.query(days))["extraction.per_session"]
		if val(m) != 300_000 || m.Extra["sessions_sent"] != int64(4) || m.Extra["window_days"] != 28 {
			t.Errorf("--days %d: value %v extra %v, want 300,000, 4 sessions, 28 days", days, val(m), m.Extra)
		}
	}
	if !slices.Contains(x.server.called(), "LLMStats?period=30d") {
		t.Errorf("calls %v: no 30d read", x.server.called())
	}
}

// TestExtractionPerSessionNoDocument: no document sent: no_data, no_events.
func TestExtractionPerSessionNoDocument(t *testing.T) {
	x := costFx(t)
	x.server.set(func(d *fakeData) {
		d.LLM = map[string][]hindsight.LLMBucket{"": {{Time: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Input: 1}}}
	})
	if m := costOf(x, x.query(7))["extraction.per_session"]; m.Status != StatusNoData || m.Reason != ReasonNoEvents {
		t.Errorf("%s/%s, want no_data/no_events", m.Status, m.Reason)
	}
}

// TestExtractionServerDown: an unreachable server: the three server
// metrics read no_data/server_down; a 404 reads server_unsupported.
func TestExtractionServerDown(t *testing.T) {
	ids := []string{"extraction.tokens", "extraction.cost", "extraction.per_session"}
	for name, c := range map[string]struct {
		err    error
		reason string
	}{
		"unreachable": {hindsight.ErrUnreachable, ReasonServerDown},
		"404":         {&hindsight.APIError{Kind: hindsight.NotFound, Status: 404}, ReasonServerUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			x := costFx(t)
			extractionServer(x)
			retained(x, ago(time.Hour), "d", "ok")
			x.server.fail("LLMStats", c.err)
			m := costOf(x, x.query(7))
			for _, id := range ids {
				if m[id].Status != StatusNoData || m[id].Reason != c.reason {
					t.Errorf("%s: %s/%s, want no_data/%s", id, m[id].Status, m[id].Reason, c.reason)
				}
			}
		})
	}
}

// TestCostNoTelemetry: with no telemetry at all the machine metrics read
// no_data, no_events.
func TestCostNoTelemetry(t *testing.T) {
	x := costFx(t)
	m := costOf(x, x.query(7))
	for _, id := range []string{"context.per_session", "context.per_prompt", "recall.latency", "search.latency"} {
		if m[id].Status != StatusNoData || m[id].Reason != ReasonNoEvents {
			t.Errorf("%s: %s/%s, want no_data/no_events", id, m[id].Status, m[id].Reason)
		}
	}
}

// TestExtractionPerSessionNoData: a value of 0 is never made up. Documents
// were sent but the server holds no bucket in the 28 days: no_data,
// no_events; the view did not request the 30-day read: no_data,
// server_unsupported.
func TestExtractionPerSessionNoData(t *testing.T) {
	t.Run("no bucket in 28 days", func(t *testing.T) {
		x := costFx(t)
		retained(x, ago(time.Hour), "d", "ok")
		x.server.set(func(d *fakeData) {
			d.LLM = map[string][]hindsight.LLMBucket{"": {{Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Input: 5}}}
		})
		m := costOf(x, x.query(7))["extraction.per_session"]
		if m.Status != StatusNoData || m.Reason != ReasonNoEvents || m.Value != nil {
			t.Errorf("%s/%s value %v, want no_data/no_events, null", m.Status, m.Reason, m.Value)
		}
	})
	t.Run("empty answer", func(t *testing.T) {
		x := costFx(t)
		retained(x, ago(time.Hour), "d", "ok")
		m := costOf(x, x.query(7))["extraction.per_session"]
		if m.Status != StatusNoData || m.Reason != ReasonNoEvents {
			t.Errorf("%s/%s, want no_data/no_events", m.Status, m.Reason)
		}
	})
	t.Run("read not requested", func(t *testing.T) {
		x := costFx(t)
		x.need = slices.DeleteFunc(slices.Clone(x.need), func(k string) bool { return k == llm30dKey })
		retained(x, ago(time.Hour), "d", "ok")
		extractionServer(x)
		m := costOf(x, x.query(7))["extraction.per_session"]
		if m.Status != StatusNoData || m.Reason != ReasonServerUnsupported {
			t.Errorf("%s/%s, want no_data/server_unsupported", m.Status, m.Reason)
		}
	})
}

// TestExtractionBucketOverlap: a bucket that starts before the window but
// overlaps it is kept whole (as extractionOver keeps whole UTC days). The
// window starts 10-23T11:00Z: the 10-23 day bucket (200) overlaps it, the
// 10-22 one (100, ending 10-23T00:00Z) does not; 10-28 has 400. Tokens
// = 200 + 400 = 600, over 2 days.
func TestExtractionBucketOverlap(t *testing.T) {
	x := costFx(t)
	x.server.set(func(d *fakeData) {
		d.LLM = map[string][]hindsight.LLMBucket{"": {
			{Time: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), Input: 100},
			{Time: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), Input: 200},
			{Time: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Input: 400},
		}}
	})
	m := costOf(x, x.query(7))["extraction.tokens"]
	if val(m) != 600 || m.Extra["days_covered"] != 2 {
		t.Errorf("tokens %v over %v days, want 600 over 2", val(m), m.Extra["days_covered"])
	}
}

// TestExtractionOneOperationFails: the read of consolidation fails (404):
// the total is still shown with the operations that answered, tagged
// partial with one failed operation, and without an "other" that would
// hide the missing one. All three costs stay 0.710 (the total's).
func TestExtractionOneOperationFails(t *testing.T) {
	x := costFx(t)
	pricesSet(x)
	extractionServer(x)
	x.server.fail("LLMStats:consolidation", &hindsight.APIError{Kind: hindsight.NotFound, Status: 404})
	m := costOf(x, x.query(7))
	for _, id := range []string{"extraction.tokens", "extraction.cost"} {
		if !slices.Contains(m[id].Tags, tagPartial) || m[id].Extra["failed_operations"] != 1 || m[id].Status == StatusNoData {
			t.Errorf("%s: tags %v extra %v status %s, want partial, 1 failed, not no_data", id, m[id].Tags, m[id].Extra, m[id].Status)
		}
	}
	if val(m["extraction.tokens"]) != 1_850_000 || math.Abs(val(m["extraction.cost"])-0.710) > 1e-9 {
		t.Errorf("tokens %v cost %v, want 1,850,000 and 0.710", val(m["extraction.tokens"]), val(m["extraction.cost"]))
	}
	ops := m["extraction.tokens"].Extra["by_operation"].(map[string]any)
	if _, ok := ops["consolidation"]; ok {
		t.Error("consolidation present although its read failed")
	}
	if _, ok := ops["other"]; ok {
		t.Error("other present although an operation is missing")
	}
	if _, ok := ops["retain_extract_facts"]; !ok {
		t.Error("an operation that answered is missing")
	}
	// all healthy: not partial
	y := costFx(t)
	extractionServer(y)
	if m := costOf(y, y.query(7))["extraction.tokens"]; slices.Contains(m.Tags, tagPartial) || m.Extra["failed_operations"] != nil {
		t.Errorf("healthy: tags %v extra %v, want no partial", m.Tags, m.Extra)
	}
}

// TestContextWatchBoundary: 100 sessions, 5 of 10,000 chars and 95 of
// 9,000 (the 95th, so p95 = 9,000 = 1.5 × the default budget of 6,000):
// ok, the rule is strict. With 9,001 for the 95: watch.
func TestContextWatchBoundary(t *testing.T) {
	for chars, want := range map[int]Status{9000: StatusOK, 9001: StatusWatch} {
		x := costFx(t)
		for i := range 100 {
			c := chars
			if i < 5 {
				c = 10000
			}
			x.tel(ago(time.Duration(i+1)*time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Chars: c}, fmt.Sprintf("s-%d", i), "alpha")
		}
		m := costOf(x, x.query(7))["context.per_session"]
		if val(m) != float64(chars) || m.Status != want {
			t.Errorf("p95 %v status %s, want %d %s", val(m), m.Status, chars, want)
		}
	}
}

// TestExtractionBucketWidthFromPeriod: the width comes from the period
// asked, not from the gaps. On a 1-day view (hourly buckets, window from
// 10-29T11:00Z) a lone bucket at 10:00Z ends at 11:00Z, outside the window;
// a lone one at 11:00Z is inside.
func TestExtractionBucketWidthFromPeriod(t *testing.T) {
	for name, c := range map[string]struct {
		at   time.Time
		want string
	}{
		"hour ending at the start": {time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC), ReasonNoEvents},
		"hour inside":              {time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC), ""},
	} {
		t.Run(name, func(t *testing.T) {
			x := costFx(t)
			x.server.set(func(d *fakeData) { d.LLM = map[string][]hindsight.LLMBucket{"": {{Time: c.at, Input: 7}}} })
			m := costOf(x, x.query(1))["extraction.tokens"]
			if c.want != "" && (m.Status != StatusNoData || m.Reason != c.want) {
				t.Errorf("%s/%s, want no_data/%s", m.Status, m.Reason, c.want)
			}
			if c.want == "" && val(m) != 7 {
				t.Errorf("tokens %v, want 7", val(m))
			}
		})
	}
}

// TestExtractionSparseOperation: the total has buckets 10-23 (200, straddling
// the window start) and 10-28 (400); consolidation only 10-23 (100). Both
// keep the edge bucket: other = 600 - 100 = 500. An operation bigger than
// the total (1,000 > 600) leaves other at 0, never negative.
func TestExtractionSparseOperation(t *testing.T) {
	for name, c := range map[string]struct{ op, other int64 }{"sparse": {100, 500}, "bigger than total": {1000, 0}} {
		t.Run(name, func(t *testing.T) {
			x := costFx(t)
			x.server.set(func(d *fakeData) {
				d.LLM = map[string][]hindsight.LLMBucket{
					"":              {{Time: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), Input: 200}, {Time: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), Input: 400}},
					"consolidation": {{Time: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), Input: c.op}},
				}
			})
			ops := costOf(x, x.query(7))["extraction.tokens"].Extra["by_operation"].(map[string]any)
			if got := ops["consolidation"].(map[string]any)["input"]; got != c.op {
				t.Errorf("consolidation input = %v, want %d", got, c.op)
			}
			if got := ops["other"].(map[string]any)["input"]; got != c.other {
				t.Errorf("other input = %v, want %d", got, c.other)
			}
		})
	}
}

// TestExtractionEveryOperationFails: the 3 operation reads fail, the total
// is kept: no_data only when the total's own read fails.
func TestExtractionEveryOperationFails(t *testing.T) {
	x := costFx(t)
	extractionServer(x)
	for _, op := range extractionOps {
		x.server.fail("LLMStats:"+op, &hindsight.APIError{Kind: hindsight.NotFound, Status: 404})
	}
	for _, id := range []string{"extraction.tokens", "extraction.cost"} {
		m := costOf(x, x.query(7))[id]
		if m.Status == StatusNoData || !slices.Contains(m.Tags, tagPartial) || m.Extra["failed_operations"] != 3 {
			t.Errorf("%s: %s tags %v extra %v, want kept, partial, 3 failed", id, m.Status, m.Tags, m.Extra)
		}
		if ops := m.Extra["by_operation"].(map[string]any); len(ops) != 0 {
			t.Errorf("%s by_operation = %v, want empty", id, ops)
		}
	}
	if v := val(costOf(x, x.query(7))["extraction.tokens"]); v != 1_850_000 {
		t.Errorf("tokens = %v, want 1,850,000", v)
	}
}
