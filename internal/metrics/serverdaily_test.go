package metrics

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/metricsfile"
)

// The fixtures of the server's daily record read past the server's reach.
// The clock is fxNow (2026-10-30T12:00+01:00). The server's series holds
// the UTC days 2026-08-02 to 2026-10-29; the record holds older days.

func sdDay(s string) time.Time { return mustDay(s) }

// sdSeries fills the fake's series with world 2 on every UTC day from first
// to 10-29 included.
func sdSeries(x *fx, first string) {
	var bs []hindsight.SeriesBucket
	for d := sdDay(first); !d.After(sdDay("2026-10-29")); d = d.AddDate(0, 0, 1) {
		bs = append(bs, hindsight.SeriesBucket{Time: d, World: 2})
	}
	x.server.set(func(f *fakeData) { f.Series = hindsight.Timeseries{Trunc: "day", Buckets: bs} })
}

func sdNew(x *fx, at time.Time, day string, k metricsfile.KindCounts) {
	x.t.Helper()
	x.mrec(at, metricsfile.ServerDay{Bucket: sdDay(day), New: &k})
}

// fv is a figure for a message: the value, or null.
func fv(p *float64) any {
	if p == nil {
		return "null"
	}
	return *p
}

func pointOn(t *testing.T, m Metric, day string) Point {
	t.Helper()
	for _, p := range m.Series {
		if p.Day == day {
			return p
		}
	}
	t.Fatalf("no point for %s", day)
	return Point{}
}

// Window 120 days (from 2026-07-02T12:00+02:00). The server holds 08-02 on
// (world 2 a day); the record, days before: 07-01 ends before the window
// (left out), 07-05 (world 3, experience 1), 07-10 (observation 4), 08-01
// (world 5), and 08-10 (world 99: the server holds it, so it is ignored).
// 07-05 is recorded twice; the later line (world 3) replaces the earlier
// (world 50). 07-06 has no line: none.
func TestGrowthReadsTheRecordPastNinetyDays(t *testing.T) {
	x := newFx(t, fxNow)
	sdSeries(x, "2026-08-02")
	sdNew(x, rcBase, "2026-07-01", metricsfile.KindCounts{World: 1000})
	sdNew(x, rcBase, "2026-07-05", metricsfile.KindCounts{World: 50})
	sdNew(x, rcBase.Add(time.Hour), "2026-07-05", metricsfile.KindCounts{World: 3, Experience: 1})
	sdNew(x, rcBase, "2026-07-10", metricsfile.KindCounts{Observation: 4})
	sdNew(x, rcBase, "2026-08-01", metricsfile.KindCounts{World: 5})
	sdNew(x, rcBase, "2026-08-10", metricsfile.KindCounts{World: 99})
	m := bankOf(x, x.query(120))["bank.growth"]

	for _, c := range []struct {
		day, source string
		value       *float64
	}{
		{"2026-07-05", "record", ptr(4.0)},
		{"2026-07-06", "none", nil},
		{"2026-07-10", "record", ptr(4.0)},
		{"2026-08-01", "record", ptr(5.0)},
		{"2026-08-02", "server", ptr(2.0)},
		{"2026-08-10", "server", ptr(2.0)},
		{"2026-10-29", "server", ptr(2.0)},
	} {
		p := pointOn(t, m, c.day)
		if p.Source != c.source {
			t.Errorf("%s source = %q, want %q", c.day, p.Source, c.source)
		}
		switch {
		case c.value == nil && p.Value != nil:
			t.Errorf("%s value = %v, want null", c.day, *p.Value)
		case c.value != nil && (p.Value == nil || *p.Value != *c.value):
			t.Errorf("%s value = %v, want %v", c.day, fv(p.Value), *c.value)
		}
	}
	if p := pointOn(t, m, "2026-07-05"); p.Parts["world"] != 3 || p.Parts["experience"] != 1 {
		t.Errorf("07-05 parts = %v, want world 3, experience 1 (the later line)", p.Parts)
	}
	if got := m.Extra["record_from"]; got == nil || *got.(*string) != "2026-07-01" {
		t.Errorf("record_from = %v, want 2026-07-01 (the first recorded day)", got)
	}
	// 4 + 4 + 5 from the record; the server's 89 days (08-02 to 10-29) and
	// the zero-padded 10-30 are not in the series: 89 x 2.
	if m.Extra["world"] != int64(3+5+89*2) || m.Extra["experience"] != int64(1) || m.Extra["observation"] != int64(4) {
		t.Errorf("kinds = %v %v %v", m.Extra["world"], m.Extra["experience"], m.Extra["observation"])
	}
	if m.Extra["days_covered"] != 89+3 {
		t.Errorf("days_covered = %v, want 92 (89 server days, 3 recorded)", m.Extra["days_covered"])
	}
}

// Window 90 days (from 2026-08-01T12:00+02:00): the LLM stats reach 30 days,
// so the server holds 10-01 to 10-29 (all: input 100, output 50; retain:
// input 60, output 30) and the record the days before. Recorded: 08-15 (all
// 1000 in, 400 of them cached, 200 out, 100 thoughts; retain 700 in, 400
// cached, 100 out; consolidation 300 in, 100 out, 100 thoughts), twice (the
// later line wins, the earlier has 5000 in), 09-30 (all 10 in), 10-10 (the
// server holds it: ignored).
func TestTokensReadTheRecordPastThirtyDays(t *testing.T) {
	x := costFx(t)
	pricesSet(x)
	var all, retain []hindsight.LLMBucket
	for d := sdDay("2026-10-01"); !d.After(sdDay("2026-10-29")); d = d.AddDate(0, 0, 1) {
		all = append(all, hindsight.LLMBucket{Time: d, Trunc: "day", Input: 100, Output: 50})
		retain = append(retain, hindsight.LLMBucket{Time: d, Trunc: "day", Input: 60, Output: 30})
	}
	x.server.set(func(f *fakeData) {
		f.LLM = map[string][]hindsight.LLMBucket{"": all, "retain": retain, "consolidation": {}, "refresh_mental_model": {}}
	})
	x.mrec(rcBase, metricsfile.ServerDay{Bucket: sdDay("2026-08-15"), LLM: map[string]metricsfile.LLMDay{"all": {Input: 5000}}})
	x.mrec(rcBase.Add(time.Hour), metricsfile.ServerDay{Bucket: sdDay("2026-08-15"), LLM: map[string]metricsfile.LLMDay{
		"all":           {Input: 1000, Cached: 400, Output: 200, Thoughts: 100},
		"retain":        {Input: 700, Cached: 400, Output: 100},
		"consolidation": {Input: 300, Output: 100, Thoughts: 100}}})
	x.mrec(rcBase, metricsfile.ServerDay{Bucket: sdDay("2026-09-30"), LLM: map[string]metricsfile.LLMDay{"all": {Input: 10}}})
	x.mrec(rcBase, metricsfile.ServerDay{Bucket: sdDay("2026-10-10"), LLM: map[string]metricsfile.LLMDay{"all": {Input: 9999}}})
	q := x.query(90)
	m := costOf(x, q)
	tk, cs := m["extraction.tokens"], m["extraction.cost"]

	for _, c := range []struct {
		day, source string
		value       *float64
	}{
		{"2026-08-15", "record", ptr(1300.0)},
		{"2026-08-16", "none", nil},
		{"2026-09-30", "record", ptr(10.0)},
		{"2026-10-01", "server", ptr(150.0)},
		{"2026-10-10", "server", ptr(150.0)},
	} {
		p := pointOn(t, tk, c.day)
		if p.Source != c.source {
			t.Errorf("%s source = %q, want %q", c.day, p.Source, c.source)
		}
		if (c.value == nil) != (p.Value == nil) || (c.value != nil && *p.Value != *c.value) {
			t.Errorf("%s value = %v, want %v", c.day, fv(p.Value), fv(c.value))
		}
	}
	if v := val(tk); v != 29*150+1300+10 {
		t.Errorf("tokens = %v, want %d", v, 29*150+1300+10)
	}
	for k, want := range map[string]int64{"input": 29*100 + 1000 + 10, "cached": 400, "output": 29*50 + 300} {
		if tk.Extra[k] != want {
			t.Errorf("extra.%s = %v, want %d", k, tk.Extra[k], want)
		}
	}
	ops := tk.Extra["by_operation"].(map[string]any)
	for op, want := range map[string][3]int64{
		"retain":        {29*60 + 700, 400, 29*30 + 100},
		"consolidation": {300, 0, 200},
		"other":         {3910 - (29*60 + 700) - 300, 0, 1750 - (29*30 + 100) - 200},
	} {
		o := ops[op].(map[string]any)
		if o["input"] != want[0] || o["cached"] != want[1] || o["output"] != want[2] {
			t.Errorf("by_operation.%s = %v, want %v", op, o, want)
		}
	}
	if tk.Extra["days_covered"] != 29+2 {
		t.Errorf("days_covered = %v, want 31", tk.Extra["days_covered"])
	}
	if got := tk.Extra["record_from"]; got == nil || *got.(*string) != "2026-08-15" {
		t.Errorf("record_from = %v, want 2026-08-15", got)
	}
	// the cost of 08-15 is priced from the recorded tokens: (1000-400) x 0.27
	// + 400 x 0.07 + 300 x 1.10, per million
	p := pointOn(t, cs, "2026-08-15")
	if want := (600*0.27 + 400*0.07 + 300*1.10) / 1e6; p.Source != "record" || p.Value == nil || math.Abs(*p.Value-want) > 1e-12 {
		t.Errorf("08-15 cost = %+v, want %v from the record", p, want)
	}
	if p := pointOn(t, cs, "2026-08-16"); p.Source != "none" || p.Value != nil {
		t.Errorf("08-16 cost = %+v, want none, null", p)
	}
	// the table reads the record too
	var got *ExtractionRow
	for _, r := range x.run("cost", q).Tables.Extraction {
		if r.Day == "2026-08-15" && r.Operation == "retain" {
			got = &r
		}
	}
	if got == nil || got.Input != 700 || got.Cached != 400 || got.Output != 100 {
		t.Errorf("table row for 08-15 retain = %+v, want input 700, cached 400, output 100", got)
	}
}

// A window inside the server's reach reads the server alone: a day the
// server's series lacks stays 0 whatever the record holds, no point has a
// source and no extra names the record.
func TestInsideTheReachNothingChanges(t *testing.T) {
	build := func(withRecord bool) *fx {
		x := costFx(t)
		x.need = append(x.need, "tags", "series") // bank.growth is read through the cost fixture
		pricesSet(x)
		extractionServer(x)
		x.server.set(func(f *fakeData) {
			f.Series = hindsight.Timeseries{Trunc: "day", Buckets: []hindsight.SeriesBucket{
				{Time: utcDay(26), World: 3}, {Time: utcDay(28), World: 4}}} // 10-27 is missing
		})
		if withRecord {
			sdNew(x, rcBase, "2026-10-27", metricsfile.KindCounts{World: 77})
			sdNew(x, rcBase, "2026-10-28", metricsfile.KindCounts{World: 88})
			x.mrec(rcBase, metricsfile.ServerDay{Bucket: sdDay("2026-10-27"), LLM: map[string]metricsfile.LLMDay{"all": {Input: 55555}}})
		}
		return x
	}
	with, without := build(true), build(false)
	q := 7
	for _, id := range []string{"bank.growth", "extraction.tokens", "extraction.cost"} {
		a := with.metrics([]string{id}, with.query(q))[id]
		b := without.metrics([]string{id}, without.query(q))[id]
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Errorf("%s with the record = %s\nwithout = %s", id, ja, jb)
		}
		for _, p := range a.Series {
			if p.Source != "" {
				t.Errorf("%s: point %s has source %q inside the reach", id, p.Day, p.Source)
			}
		}
		if _, ok := a.Extra["record_from"]; ok {
			t.Errorf("%s: record_from inside the reach", id)
		}
	}
	// 10-27 stays 0 in the growth series
	if p := pointOn(t, with.metrics([]string{"bank.growth"}, with.query(7))["bank.growth"], "2026-10-27"); p.Value == nil || *p.Value != 0 {
		t.Errorf("10-27 = %v, want 0 (the record is not read inside the reach)", p.Value)
	}
}
