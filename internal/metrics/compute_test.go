package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/telemetry"
)

// countingReader counts the read transactions of a view.
type countingReader struct {
	Reader
	n atomic.Int32
}

func (c *countingReader) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	c.n.Add(1)
	return c.Reader.ReadTx(ctx, fn)
}

func same(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestComputeOneTransaction: two windows, each with its previous window
// and baselines, read from the fixture's index in one transaction; an id
// no family registered reads no_data/no_events.
func TestComputeOneTransaction(t *testing.T) {
	x := newFx(t, fxNow)
	// 7-day window [10-23 12:00, 10-30 12:00): 40 recalls, 2 with memory.
	// Each of the four 7-day windows before it: 20 recalls, 10 with memory.
	recall := func(at time.Time, injected int) {
		x.tel(at, telemetry.Prompt{N: 1, Decision: "recall", Injected: injected}, "s1", "alpha")
	}
	for i := range 40 {
		recall(fxNow.Add(-time.Duration(i+1)*time.Hour), min(1, max(0, 2-i)))
	}
	for k := 1; k <= 4; k++ {
		start := fxNow.Add(-time.Duration(7*k) * day)
		for i := range 20 {
			x.tel(start.Add(-time.Duration(i+1)*time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Injected: i % 2},
				fmt.Sprintf("s%d", k+1), "beta")
		}
	}

	var calls []time.Time
	var txs []*sql.Tx
	reg := &registry{byID: map[string]*family{}}
	reg.register("reach", []string{"recall.hit_rate", "recall.empty_reasons"}, func(e env, w Window) (map[string]Observation, map[string]Metric, error) {
		calls, txs = append(calls, w.Since), append(txs, e.tx)
		var den, num int64
		if err := e.tx.QueryRowContext(e.ctx, `SELECT count(*), coalesce(sum(injected > 0), 0) FROM tel_prompt
			WHERE decision = 'recall' AND ts >= ? AND ts < ?`, w.Since.UnixMilli(), w.Until.UnixMilli()).Scan(&den, &num); err != nil {
			return nil, nil, err
		}
		rate := Metric{Num: &num, Den: &den}
		if den > 0 {
			v := float64(num) / float64(den)
			rate.Value = &v
		}
		empty := float64(den - num)
		return map[string]Observation{
			"recall.hit_rate":      {Num: num, Den: den},
			"recall.empty_reasons": {Den: den},
		}, map[string]Metric{
			"recall.hit_rate":      rate,
			"recall.empty_reasons": {Value: &empty},
		}, nil
	})

	src := x.source()
	cr := &countingReader{Reader: src.Index}
	src.Index = cr
	w7, w28 := x.window(7), x.window(28)
	ids := []string{"recall.hit_rate", "recall.empty_reasons", "bank.size"}
	var m7, m28 map[string]Metric
	err := withTx(context.Background(), src, Query{Window: w7}, nil, func(e env) error {
		var err error
		if m7, err = reg.compute(e, w7, ids); err != nil {
			return err
		}
		m28, err = reg.compute(e, w28, ids)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := cr.n.Load(); n != 1 {
		t.Fatalf("%d read transactions, want 1", n)
	}
	// Each window is run once per transaction: the 7-day window's previous
	// window is its first baseline, and its last baseline (35 to 28 days
	// back) is the 28-day window's first, so 2 × 6 windows are 10 runs.
	if len(txs) != 10 || slices.ContainsFunc(txs, func(tx *sql.Tx) bool { return tx != txs[0] }) {
		t.Fatalf("%d family runs, want 10 (2 windows × itself, previous, 4 baselines; each window once) in one transaction", len(txs))
	}
	var want []time.Time
	var seen []Span
	for _, w := range []Window{w7, w28} {
		for _, r := range append([]Window{w, w.Previous()}, w.Baselines()...) {
			if span := (Span{r.Since, r.Until}); !slices.ContainsFunc(seen, func(s Span) bool {
				return s.Since.Equal(span.Since) && s.Until.Equal(span.Until)
			}) {
				seen = append(seen, span)
				want = append(want, r.Since)
			}
		}
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("windows run = %v, want %v", calls, want)
	}

	// 2/40 against a baseline median of 0.5: the upper bound 0.1650 is
	// under 0.7 × 0.5 (relative watch) and over 0.10 (no absolute watch)
	// while the point 0.05 is under it (borderline).
	hr := m7["recall.hit_rate"]
	lo, hi := Wilson(2, 40)
	if hr.Status != StatusWatch || !hr.Borderline || hr.CI == nil || !same(hr.CI[0], lo) || !same(hr.CI[1], hi) {
		t.Errorf("hit_rate: status %s, borderline %v, ci %v; want watch, borderline, [%.4f, %.4f]", hr.Status, hr.Borderline, hr.CI, lo, hi)
	}
	if hr.MDC == nil || !same(*hr.MDC, MDC(40)) {
		t.Errorf("hit_rate mdc = %v, want %.4f", hr.MDC, MDC(40))
	}
	if hr.Value == nil || !same(*hr.Value, 0.05) || hr.Previous == nil || !same(*hr.Previous, 0.5) || hr.Delta == nil || !same(*hr.Delta, -0.45) {
		t.Errorf("hit_rate value %v, previous %v, delta %v; want 0.05, 0.5, -0.45", hr.Value, hr.Previous, hr.Delta)
	}
	d := mustDef(t, "recall.hit_rate")
	if hr.Unit != "ratio" || hr.Aggregation != d.Aggregation || hr.Drill != d.Drill || hr.Tags == nil || len(hr.Tags) != 0 {
		t.Errorf("hit_rate unit %q, aggregation %q, drill %q, tags %v", hr.Unit, hr.Aggregation, hr.Drill, hr.Tags)
	}
	// 28 days: 40 + 20 + 20 + 20 recalls, 2 + 30 with memory.
	if h := m28["recall.hit_rate"]; h.Den == nil || *h.Den != 100 || *h.Num != 32 {
		t.Errorf("28-day hit_rate num %v den %v, want 32/100", h.Num, h.Den)
	}
	if er := m7["recall.empty_reasons"]; er.Status != StatusOK || er.MDC != nil || er.Previous == nil || *er.Previous != 10 {
		t.Errorf("empty_reasons: status %s, mdc %v, previous %v; want ok, none (a count), 10", er.Status, er.MDC, er.Previous)
	}
	bs := m7["bank.size"]
	if bs.Status != StatusNoData || bs.Reason != ReasonNoEvents || bs.Value != nil || bs.Unit != "count" || bs.Drill != mustDef(t, "bank.size").Drill {
		t.Errorf("bank.size (no family): %+v, want no_data no_events with its unit and drill", bs)
	}
}

// fixedFamily answers fixed observations and metrics by window start
// (the zero key: any other window), counting its runs.
type fixedFamily struct {
	obs   map[time.Time]map[string]Observation
	met   map[time.Time]map[string]Metric
	err   error
	calls int
}

func (f *fixedFamily) run(_ env, w Window) (map[string]Observation, map[string]Metric, error) {
	f.calls++
	if f.err != nil {
		return nil, nil, f.err
	}
	o, ok := f.obs[w.Since]
	if !ok {
		o = f.obs[time.Time{}]
	}
	m, ok := f.met[w.Since]
	if !ok {
		m = f.met[time.Time{}]
	}
	return o, m, nil
}

func TestComputeFillsEveryMetric(t *testing.T) {
	x := newFx(t, fxNow)
	w := x.window(7)
	prev := w.Previous().Since
	fam := &fixedFamily{
		obs: map[time.Time]map[string]Observation{
			w.Since: {
				"declare.coverage": {Num: 10, Den: 19},
				"recall.latency":   {Den: 150, P95: fp(1200)},
				"bank.size":        {Den: 0},
			},
			prev: {"declare.coverage": {Num: 1, Den: 2}, "recall.latency": {Den: 120}, "bank.size": {}},
		},
		met: map[time.Time]map[string]Metric{
			w.Since: {
				"declare.coverage": {Value: fp(10.0 / 19)},
				"recall.latency":   {Value: fp(1200)},
				"bank.size":        {Value: fp(100), Tags: []string{"unreliable"}},
			},
			prev: {"declare.coverage": {}, "recall.latency": {Value: fp(900)}, "bank.size": {Value: fp(90)}},
		},
	}
	reg := &registry{byID: map[string]*family{}}
	reg.register("mixed", []string{"declare.coverage", "recall.latency", "bank.size"}, fam.run)

	// The project filter of the query reaches the families and the tags.
	var got map[string]Metric
	err := withTx(context.Background(), x.source(), Query{Window: w, Project: "alpha"}, nil, func(e env) error {
		if e.q.Project != "alpha" || e.q.Window != w {
			t.Errorf("env query %+v, want the window and project alpha", e.q)
		}
		var err error
		got, err = reg.compute(e, w, []string{"declare.coverage", "recall.latency", "bank.size"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if fam.calls != 2 {
		t.Errorf("%d runs, want 2 (no relative id: the window and its previous)", fam.calls)
	}
	dc := got["declare.coverage"]
	if dc.Status != StatusTooFew || dc.MDC != nil || dc.CI == nil || dc.Previous != nil || dc.Delta != nil {
		t.Errorf("coverage 10/19: status %s, mdc %v, ci %v, previous %v, delta %v; want too_few, no mdc, an interval, no previous", dc.Status, dc.MDC, dc.CI, dc.Previous, dc.Delta)
	}
	rl := got["recall.latency"]
	if rl.Status != StatusOK || rl.MDC != nil || rl.CI != nil || rl.Previous == nil || *rl.Previous != 900 || rl.Delta == nil || *rl.Delta != 300 {
		t.Errorf("latency: status %s, mdc %v, ci %v, previous %v, delta %v; want ok, no mdc (not a rate), 900, +300", rl.Status, rl.MDC, rl.CI, rl.Previous, rl.Delta)
	}
	if !slices.Equal(rl.Tags, []string{}) {
		t.Errorf("latency (this PC) tags %v under a project filter, want none", rl.Tags)
	}
	if bs := got["bank.size"]; !slices.Equal(bs.Tags, []string{"unreliable", "not_filtered"}) {
		t.Errorf("bank.size (server) tags %v, want the family's and not_filtered", bs.Tags)
	}
}

func TestComputeRefuses(t *testing.T) {
	x := newFx(t, fxNow)
	src := x.source()
	w := x.window(7)
	run := func(reg *registry, ids []string) error {
		return withTx(context.Background(), src, Query{Window: w}, nil, func(e env) error {
			_, err := reg.compute(e, w, ids)
			return err
		})
	}
	empty := &registry{byID: map[string]*family{}}
	if err := run(empty, []string{"nope"}); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown id: %v", err)
	}
	boom := errors.New("boom")
	reg := &registry{byID: map[string]*family{}}
	reg.register("broken", []string{"bank.size"}, (&fixedFamily{err: boom}).run)
	if err := run(reg, []string{"bank.size"}); !errors.Is(err, boom) {
		t.Errorf("family error: %v, want boom", err)
	}
	reg = &registry{byID: map[string]*family{}}
	reg.register("forgetful", []string{"bank.size", "bank.growth"}, (&fixedFamily{
		obs: map[time.Time]map[string]Observation{{}: {"bank.size": {}}}}).run)
	if err := run(reg, []string{"bank.growth"}); err == nil || !strings.Contains(err.Error(), "bank.growth") {
		t.Errorf("an id the family did not compute: %v", err)
	}
	if err := withTx(context.Background(), Source{}, Query{Window: w}, nil, func(env) error { return nil }); !errors.Is(err, errNoIndex) {
		t.Errorf("no index: %v", err)
	}
}

func TestRegisterRefuses(t *testing.T) {
	panics := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: no panic", name)
			}
		}()
		f()
	}
	noop := func(env, Window) (map[string]Observation, map[string]Metric, error) { return nil, nil, nil }
	reg := &registry{byID: map[string]*family{}}
	reg.register("a", []string{"bank.size"}, noop)
	panics("registered twice", func() { reg.register("b", []string{"bank.size"}, noop) })
	panics("not in the catalog", func() { reg.register("c", []string{"nope"}, noop) })
}

// TestFixtureMetrics: fx.metrics runs the registered families through
// withTx and computeMetrics, with the fake server's data; a read that
// blocks past ServerTimeout reads no_data server_down.
func TestFixtureMetrics(t *testing.T) {
	saved := families.byID
	families.byID = map[string]*family{}
	t.Cleanup(func() { families.byID = saved })
	register("fixture", []string{"bank.size"}, func(e env, _ Window) (map[string]Observation, map[string]Metric, error) {
		if err := e.srv.Err["stats"]; err != nil {
			_, reason := serverError(err)
			return map[string]Observation{"bank.size": {Absent: true, Reason: reason}}, nil, nil
		}
		v := float64(e.srv.Stats.TotalNodes)
		return map[string]Observation{"bank.size": {}}, map[string]Metric{"bank.size": {Value: &v}}, nil
	})

	x := newFx(t, fxNow)
	x.server.set(func(d *fakeData) { d.Stats.TotalNodes = 120 })
	q := x.query(7)
	q.Project = "alpha"
	m := x.metrics([]string{"bank.size", "recall.hit_rate"}, q)
	if bs := m["bank.size"]; bs.Status != StatusOK || bs.Value == nil || *bs.Value != 120 || !slices.Equal(bs.Tags, []string{"not_filtered"}) {
		t.Errorf("bank.size = %+v, want ok 120 not_filtered", bs)
	}
	if hr := m["recall.hit_rate"]; hr.Status != StatusNoData || hr.Reason != ReasonNoEvents {
		t.Errorf("recall.hit_rate (no family) = %s %s, want no_data no_events", hr.Status, hr.Reason)
	}

	lowerTimeouts(t, 10*time.Millisecond, time.Minute)
	x.server.block("BankStats")
	if bs := x.metrics([]string{"bank.size"}, x.query(28))["bank.size"]; bs.Status != StatusNoData || bs.Reason != ReasonServerDown {
		t.Errorf("stats blocked: bank.size %s %s, want no_data server_down", bs.Status, bs.Reason)
	}
}

// TestPreviousAndDeltaNeedMinN: the previous value shows only when the
// previous window has min_n trials; the delta only when both windows do.
func TestPreviousAndDeltaNeedMinN(t *testing.T) {
	x := newFx(t, fxNow)
	src := x.source()
	w := x.window(7)
	prev := w.Previous().Since
	for _, c := range []struct {
		name               string
		id                 string
		cur, old           Observation
		noOld              bool // the family computes nothing for the previous window
		wantPrev, wantDelt bool
	}{
		{name: "both at min_n", id: "recall.hit_rate", cur: Observation{Num: 5, Den: 20}, old: Observation{Num: 4, Den: 20}, wantPrev: true, wantDelt: true},
		{name: "previous under min_n", id: "recall.hit_rate", cur: Observation{Num: 5, Den: 20}, old: Observation{Num: 4, Den: 19}},
		{name: "current under min_n", id: "recall.hit_rate", cur: Observation{Num: 5, Den: 19}, old: Observation{Num: 4, Den: 20}, wantPrev: true},
		{name: "both under min_n", id: "recall.hit_rate", cur: Observation{Num: 5, Den: 19}, old: Observation{Num: 4, Den: 19}},
		{name: "previous absent", id: "recall.hit_rate", cur: Observation{Num: 5, Den: 20}, old: Observation{Absent: true, Den: 40}},
		{name: "previous not computed", id: "recall.hit_rate", cur: Observation{Num: 5, Den: 20}, noOld: true},
		{name: "min_n 0, previous computed", id: "bank.size", cur: Observation{}, old: Observation{}, wantPrev: true, wantDelt: true},
		{name: "min_n 0, previous not computed", id: "bank.size", cur: Observation{}, noOld: true},
		{name: "percentile n at 100", id: "recall.latency", cur: Observation{Den: 100, P95: fp(800)}, old: Observation{Den: 100}, wantPrev: true, wantDelt: true},
		{name: "percentile previous n 99", id: "recall.latency", cur: Observation{Den: 100, P95: fp(800)}, old: Observation{Den: 99}},
		{name: "percentile current n 99", id: "recall.latency", cur: Observation{Den: 99, P95: fp(800)}, old: Observation{Den: 100}, wantPrev: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			oldObs := map[string]Observation{c.id: c.old}
			if c.noOld {
				oldObs = map[string]Observation{}
			}
			fam := &fixedFamily{
				obs: map[time.Time]map[string]Observation{w.Since: {c.id: c.cur}, prev: oldObs, {}: {}},
				met: map[time.Time]map[string]Metric{w.Since: {c.id: {Value: fp(0.25)}}, prev: {c.id: {Value: fp(0.2)}}},
			}
			reg := &registry{byID: map[string]*family{}}
			reg.register("one", []string{c.id}, fam.run)
			var m Metric
			if err := withTx(context.Background(), src, Query{Window: w}, nil, func(e env) error {
				got, err := reg.compute(e, w, []string{c.id})
				m = got[c.id]
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if (m.Previous != nil) != c.wantPrev || (m.Previous != nil && *m.Previous != 0.2) {
				t.Errorf("previous = %v, want set %v (0.2)", m.Previous, c.wantPrev)
			}
			if (m.Delta != nil) != c.wantDelt || (m.Delta != nil && !same(*m.Delta, 0.05)) {
				t.Errorf("delta = %v, want set %v (0.05)", m.Delta, c.wantDelt)
			}
		})
	}
}
