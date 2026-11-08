package metrics

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/metricsfile"
)

// The fixtures of memory.yield. Weeks run Monday to Sunday; the week of
// 2026-09-21 (dates 09-21 to 09-27) is the one most tests read: its last
// day plus 29 days is 2026-10-26T00:00Z, before the window's end
// (2026-10-30T15:44+01:00). The week of 2026-09-28 ends on 10-04, plus 29
// days is 2026-11-02: not complete.

const (
	ydWeek  = "2026-09-21"
	ydNow   = "2026-10-30T15:44:00+01:00"
	ydToken = "memory.yield"
)

func ydFx(t *testing.T) *fx {
	t.Helper()
	now, err := time.Parse(time.RFC3339, ydNow)
	if err != nil {
		t.Fatal(err)
	}
	x := newFx(t, now)
	x.need = slices.Concat(x.need, YieldServerNeeds())
	return x
}

func ydDate(s string) time.Time { return mustDay(s) }

// ydDays records the server's new memories of the seven dates from first,
// n in total spread over the days (the first days get the remainder).
func (x *fx) ydDays(first string, n int64) {
	x.t.Helper()
	d := ydDate(first)
	for i := range int64(7) {
		c := n / 7
		if i < n%7 {
			c++
		}
		x.mrec(rcBase, metricsfile.ServerDay{Bucket: d.AddDate(0, 0, int(i)), New: &metricsfile.KindCounts{World: c}})
	}
}

// ydDay records one server day with its LLM use.
func (x *fx) ydDay(day string, world int64, llm map[string]metricsfile.LLMDay) {
	x.t.Helper()
	x.mrec(rcBase, metricsfile.ServerDay{Bucket: ydDate(day), New: &metricsfile.KindCounts{World: world}, LLM: llm})
}

// ydMemory is a memory created on day (a row of the day lists).
func (x *fx) ydMemory(id, day string) {
	x.t.Helper()
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: id, Outcome: "found", DocKind: "session", Document: "conversation:s-" + id,
		Session: "s-" + id, CreatedDay: day, FactType: "world"})
}

// ydObservation is an observation (no document) created on day.
func (x *fx) ydObservation(id, day string) {
	x.t.Helper()
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: id, Outcome: "found", DocKind: "none", CreatedDay: day, FactType: "observation"})
}

// ydUsed credits id used at at.
func (x *fx) ydUsed(id string, at time.Time, agent string) {
	x.t.Helper()
	x.credit(at, "cs-"+id, agent, "alpha", "used", id)
}

// ydUsedMemories makes n memories of the week used the day after their
// creation (named prefix-1 ... prefix-n, created over the week's days).
func (x *fx) ydUsedMemories(prefix string, n int) {
	x.t.Helper()
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		d := ydDate(ydWeek).AddDate(0, 0, i%7)
		x.ydMemory(id, d.Format("2006-01-02"))
		x.ydUsed(id, d.AddDate(0, 0, 1).Add(time.Hour), "")
	}
}

// ydListed marks the day lists as started: one day listed, not complete.
func (x *fx) ydListed() { x.mrec(rcBase, metricsfile.Listed{Day: "2026-09-21", N: 3}) }

func yieldOf(x *fx, q Query) Metric { return x.metrics([]string{ydToken}, q)[ydToken] }

func weekOf(t *testing.T, m Metric, week string) Point {
	t.Helper()
	for _, p := range m.Series {
		if p.Week == week {
			return p
		}
	}
	t.Fatalf("no point for the week of %s in %v", week, m.Series)
	return Point{}
}

func wantFloat(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got < want-1e-6 || *got > want+1e-6 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func wantInt(t *testing.T, what string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %d", what, got, want)
	}
}

func ydExtra(t *testing.T, m Metric, key string) int64 {
	t.Helper()
	v, ok := m.Extra[key].(int64)
	if !ok {
		t.Fatalf("extra.%s = %#v, want a count", key, m.Extra[key])
	}
	return v
}

func TestYieldCohorts(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	// tokens: retain 150 000 and consolidation 23 600 over the week
	x.ydDay("2026-09-22", 7, map[string]metricsfile.LLMDay{
		"retain":        {Input: 100_000, Output: 50_000},
		"consolidation": {Input: 20_000, Output: 3_600},
		"reflect":       {Input: 999_999, Output: 999_999}, // not extraction of memory
	})
	x.ydListed()
	for i := 1; i <= 14; i++ {
		id := fmt.Sprintf("u%d", i)
		day := ydDate(ydWeek).AddDate(0, 0, i%7)
		switch {
		case i <= 2:
			x.ydObservation(id, day.Format("2006-01-02"))
		default:
			x.ydMemory(id, day.Format("2006-01-02"))
		}
		at := day.AddDate(0, 0, 1)
		agent := ""
		if i%3 == 0 {
			agent = "worker" // a sub-agent's credit counts as well
		}
		x.ydUsed(id, at, agent)
	}
	// the last day it counts: 28 days and 23:59 after its day began
	x.ydMemory("edge-in", "2026-09-23")
	x.ydUsed("edge-in", ydDate("2026-09-23").AddDate(0, 0, 28).Add(23*time.Hour+59*time.Minute), "")
	// ... and the first it does not: exactly 29 days after its day began
	x.ydMemory("edge-out", "2026-09-23")
	x.ydUsed("edge-out", ydDate("2026-09-23").AddDate(0, 0, 29), "")
	// a memory credited twice counts once; one only reported wrong, not at all
	x.ydMemory("twice", "2026-09-24")
	x.ydUsed("twice", ydDate("2026-09-25"), "")
	x.ydUsed("twice", ydDate("2026-09-26"), "")
	x.ydMemory("wrong", "2026-09-24")
	x.credit(ydDate("2026-09-25"), "cs-wrong", "", "alpha", "wrong", "wrong")
	// a memory of the next week is in that week
	x.ydMemory("next", "2026-09-28")
	x.ydUsed("next", ydDate("2026-09-29"), "")

	m := yieldOf(x, x.query(90))
	// 14 + edge-in + twice = 16 of 45
	p := weekOf(t, m, ydWeek)
	wantInt(t, "num", p.Num, 16)
	wantInt(t, "den", p.Den, 45)
	wantFloat(t, "week value", p.Value, 16.0/45)
	if p.Complete == nil || !*p.Complete {
		t.Errorf("week complete = %v, want true", p.Complete)
	}
	wantInt(t, "week tokens", p.Tokens, 173_600)
	wantFloat(t, "tokens per used", p.TokensPerUsed, 173_600.0/16)
	if p.Source != "record" {
		t.Errorf("source %q, want record", p.Source)
	}
	// the metric is the latest complete week's
	wantFloat(t, "value", m.Value, 16.0/45)
	wantInt(t, "metric num", m.Num, 16)
	wantInt(t, "metric den", m.Den, 45)
	if m.CI == nil {
		t.Error("no Wilson interval")
	}
	if m.Status != StatusOK {
		t.Errorf("status %q, want ok", m.Status)
	}
	if got := m.Extra["tokens"]; got == nil {
		t.Error("extra.tokens missing")
	}
	// the weeks of the window come as points, Monday dates, oldest first
	var weeks []string
	for _, p := range m.Series {
		weeks = append(weeks, p.Week)
	}
	if !slices.IsSorted(weeks) || weeks[0] != "2026-07-27" || weeks[len(weeks)-1] != "2026-10-26" {
		t.Errorf("weeks %v, want Mondays from 2026-07-27 to 2026-10-26", weeks)
	}
}

// The hand-computed case of the brief: 14 of 45, 173 600 tokens.
func TestYieldFourteenOfFortyFive(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	x.ydDay("2026-09-22", 7, map[string]metricsfile.LLMDay{
		"retain": {Input: 100_000, Output: 50_000}, "consolidation": {Input: 20_000, Output: 3_600}})
	x.ydListed()
	x.ydUsedMemories("f", 14)
	m := yieldOf(x, x.query(90))
	wantFloat(t, "value", m.Value, 14.0/45)
	p := weekOf(t, m, ydWeek)
	wantFloat(t, "tokens_per_used", p.TokensPerUsed, 12_400)
	// Wilson 95 % of 14/45: p = 0.31111, z = 1.96, z^2/n = 0.085369,
	// centre = (0.31111 + 0.042684) / 1.085369 = 0.325968,
	// half = 1.96 * sqrt(0.0047627 + 0.00047428) / 1.085369 = 0.130683,
	// so [0.195285, 0.456651].
	if p.CI == nil {
		t.Fatal("the week has no interval")
	}
	wantFloat(t, "week ci low", &p.CI[0], 0.195285)
	wantFloat(t, "week ci high", &p.CI[1], 0.456651)
	if m.CI == nil || *m.CI != *p.CI {
		t.Errorf("metric ci %v, want the latest week's %v", m.CI, p.CI)
	}
	if q := weekOf(t, m, "2026-09-14"); q.CI != nil {
		t.Errorf("a week with no share carries an interval %v", q.CI)
	}
}

// TestYieldShareIsAtMostOne: a memory the server's series no longer counts
// (forgotten) can still be credited, so the week's numerator can pass its
// denominator: the share stops at 100 %, as its interval does.
func TestYieldShareIsAtMostOne(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 35)
	x.ydListed()
	x.ydUsedMemories("o", 40)
	m := yieldOf(x, x.query(90))
	p := weekOf(t, m, ydWeek)
	if p.Num == nil || p.Den == nil || *p.Num != 40 || *p.Den != 35 {
		t.Fatalf("week %v/%v, want 40/35", p.Num, p.Den)
	}
	wantFloat(t, "week value", p.Value, 1)
	wantFloat(t, "value", m.Value, 1)
	if p.CI == nil || p.CI[1] > 1 {
		t.Errorf("interval %v", p.CI)
	}
}

func TestYieldWeekWithNoMemories(t *testing.T) {
	x := ydFx(t)
	x.ydDays("2026-09-14", 0)
	x.ydListed()
	p := weekOf(t, yieldOf(x, x.query(90)), "2026-09-14")
	if p.Value != nil {
		t.Errorf("value %v, want none for a week with no memories", *p.Value)
	}
	wantInt(t, "den", p.Den, 0)
	wantInt(t, "num", p.Num, 0)
	if p.Tokens != nil || p.TokensPerUsed != nil {
		t.Errorf("tokens %v per used %v, want none", p.Tokens, p.TokensPerUsed)
	}
}

func TestYieldIncompleteWeeks(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	x.ydDays("2026-09-28", 40)
	x.ydDays("2026-10-26", 30)
	x.ydListed()
	x.ydUsedMemories("a", 25)
	x.ydMemory("b", "2026-09-29")
	x.ydUsed("b", ydDate("2026-09-30"), "")
	m := yieldOf(x, x.query(90))
	for _, wk := range []string{"2026-09-28", "2026-10-26"} {
		p := weekOf(t, m, wk)
		if p.Complete == nil || *p.Complete || p.Value != nil {
			t.Errorf("week %s: complete %v value %v, want incomplete and no value", wk, p.Complete, p.Value)
		}
	}
	if p := weekOf(t, m, ydWeek); p.Complete == nil || !*p.Complete {
		t.Errorf("week %s complete = %v, want true", ydWeek, p.Complete)
	}
	wantFloat(t, "metric value (the latest complete week's)", m.Value, 25.0/45)
}

// A week is complete when the window's end is at or after its last date +
// 29 days at 00:00 UTC: to the minute.
func TestYieldCompleteAtTheExactMoment(t *testing.T) {
	for _, c := range []struct {
		now  string
		want bool
	}{{"2026-10-26T00:00:00Z", true}, {"2026-10-25T23:59:00Z", false}} {
		now, _ := time.Parse(time.RFC3339, c.now)
		x := newFx(t, now)
		x.ydDays(ydWeek, 45)
		x.ydListed()
		x.ydUsedMemories("a", 25)
		p := weekOf(t, yieldOf(x, x.query(90)), ydWeek)
		if p.Complete == nil || *p.Complete != c.want {
			t.Errorf("at %s: complete = %v, want %v", c.now, p.Complete, c.want)
		}
		if (p.Value != nil) != c.want {
			t.Errorf("at %s: value %v, want one only when complete", c.now, p.Value)
		}
	}
}

func TestYieldTooFew(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 12)
	x.ydListed()
	x.ydUsedMemories("a", 5)
	m := yieldOf(x, x.query(90))
	p := weekOf(t, m, ydWeek)
	if p.Value != nil {
		t.Errorf("value %v, want none under 20 new memories", *p.Value)
	}
	wantInt(t, "num", p.Num, 5)
	wantInt(t, "den", p.Den, 12)
	if m.Status != StatusTooFew || m.Value != nil {
		t.Errorf("status %q value %v, want too_few and no value", m.Status, m.Value)
	}
	wantInt(t, "metric num", m.Num, 5)
	wantInt(t, "metric den", m.Den, 12)
}

func TestYieldNoRecord(t *testing.T) {
	x := ydFx(t)
	x.ydDays("2026-09-14", 30) // the week before is recorded, the one read is not
	x.ydListed()
	x.ydUsedMemories("a", 25)
	m := yieldOf(x, x.query(90))
	p := weekOf(t, m, ydWeek)
	if p.Source != "none" || p.Value != nil || p.Den != nil {
		t.Errorf("source %q value %v den %v, want none and no figures", p.Source, p.Value, p.Den)
	}
	if m.Status != StatusNoData || m.Reason != ReasonNoRecord || m.Value != nil {
		t.Errorf("status %q reason %q, want no_data, no_record for a latest complete week nobody recorded", m.Status, m.Reason)
	}
	if got := weekOf(t, m, "2026-09-14"); got.Source != "record" {
		t.Errorf("recorded week source %q, want record", got.Source)
	}
	// one missing day makes the week no record
	y := ydFx(t)
	for i := range 6 {
		y.ydDay(ydDate(ydWeek).AddDate(0, 0, i).Format("2006-01-02"), 8, nil)
	}
	y.ydListed()
	if p := weekOf(t, yieldOf(y, y.query(90)), ydWeek); p.Source != "none" || p.Den != nil {
		t.Errorf("six days of seven: source %q den %v, want none", p.Source, p.Den)
	}
}

// The server's series is read first, for the days it holds.
func TestYieldServerSeriesFirst(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 700) // the record says 100 a day
	x.ydListed()
	x.ydUsedMemories("a", 25)
	var bs []hindsight.SeriesBucket
	for i := range 7 {
		bs = append(bs, hindsight.SeriesBucket{Time: ydDate(ydWeek).AddDate(0, 0, i), World: 4, Experience: 3, Observation: 3})
	}
	x.server.set(func(d *fakeData) { d.Series = hindsight.Timeseries{Period: "90d", Trunc: "day", Buckets: bs} })
	p := weekOf(t, yieldOf(x, x.query(90)), ydWeek)
	wantInt(t, "den", p.Den, 70)
	if p.Source != "server" {
		t.Errorf("source %q, want server", p.Source)
	}
	// only 4 days held by the series: the others come from the record
	x.server.set(func(d *fakeData) { d.Series.Buckets = bs[:4] })
	p = weekOf(t, yieldOf(x, x.query(90)), ydWeek)
	wantInt(t, "mixed den", p.Den, 4*10+3*100)
	if p.Source != "record" {
		t.Errorf("mixed source %q, want record", p.Source)
	}
}

func TestYieldWithholdsWhileDaysCatchUp(t *testing.T) {
	setup := func(t *testing.T, resolved int) *fx {
		x := ydFx(t)
		x.ydDays(ydWeek, 45)
		x.ydUsedMemories("r", resolved)
		// a memory credited used with no row, shown once on 2026-10-05
		x.shows(time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), "v", "t1", "", "alpha", "prompt", "", "lost")
		x.ydUsed("lost", time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC), "")
		return x
	}
	t.Run("15 resolved and 1 unresolved", func(t *testing.T) {
		x := setup(t, 15)
		x.ydListed()
		m := yieldOf(x, x.query(90))
		if m.Value != nil {
			t.Errorf("value %v, want it withheld at 1 in 16", *m.Value)
		}
		p := weekOf(t, m, ydWeek)
		if p.Value != nil {
			t.Errorf("week value %v, want it withheld", *p.Value)
		}
		wantInt(t, "kept num", p.Num, 15)
		wantInt(t, "kept den", p.Den, 45)
		wantInt(t, "metric num", m.Num, 15)
		if m.Extra["withheld"] != "record_catching_up" {
			t.Errorf("withheld = %v, want record_catching_up", m.Extra["withheld"])
		}
		if ydExtra(t, m, "unresolved") != 1 || ydExtra(t, m, "resolved_used") != 15 {
			t.Errorf("unresolved %v resolved_used %v, want 1 and 15", m.Extra["unresolved"], m.Extra["resolved_used"])
		}
		if !slices.Contains(m.Tags, tagPartial) {
			t.Errorf("tags %v, want partial", m.Tags)
		}
		if m.CI != nil || p.CI != nil {
			t.Errorf("CI %v / %v on a withheld share", m.CI, p.CI)
		}
		if m.Status != StatusNoData || m.Reason != ReasonRecordCatchingUp {
			t.Errorf("status %q reason %q, want no_data, record_catching_up (not too_few)", m.Status, m.Reason)
		}
	})
	t.Run("20 resolved and 1 unresolved", func(t *testing.T) {
		x := setup(t, 20)
		x.ydListed()
		m := yieldOf(x, x.query(90))
		wantFloat(t, "value", m.Value, 20.0/45)
		if m.Extra["withheld"] != nil {
			t.Errorf("withheld = %v, want none at 1 in 21", m.Extra["withheld"])
		}
		if !slices.Contains(m.Tags, tagPartial) {
			t.Errorf("tags %v, want partial", m.Tags)
		}
	})
	t.Run("nothing unresolved", func(t *testing.T) {
		x := ydFx(t)
		x.ydDays(ydWeek, 45)
		x.ydUsedMemories("r", 15)
		x.ydListed()
		m := yieldOf(x, x.query(90))
		wantFloat(t, "value", m.Value, 15.0/45)
		if slices.Contains(m.Tags, tagPartial) {
			t.Errorf("tags %v, want no partial", m.Tags)
		}
	})
	t.Run("made after the listed days: pending", func(t *testing.T) {
		x := ydFx(t)
		x.ydDays(ydWeek, 45)
		x.ydUsedMemories("r", 15)
		coverage(x, "2026-08-01", "2026-10-29") // covered until yesterday (UTC)
		x.shows(time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC), "v", "t1", "", "alpha", "prompt", "", "fresh")
		x.ydUsed("fresh", time.Date(2026, 10, 30, 8, 30, 0, 0, time.UTC), "")
		m := yieldOf(x, x.query(90))
		if ydExtra(t, m, "pending") != 1 || ydExtra(t, m, "unresolved") != 0 {
			t.Errorf("pending %v unresolved %v, want 1 and 0", m.Extra["pending"], m.Extra["unresolved"])
		}
		wantFloat(t, "value", m.Value, 15.0/45)
		if slices.Contains(m.Tags, tagPartial) {
			t.Errorf("tags %v: a pending memory is no reason for partial", m.Tags)
		}
	})
}

func TestYieldGoneIsNotUnresolved(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	x.ydUsedMemories("r", 14)
	coverage(x, "2026-08-01", "2026-10-29")
	for i := 1; i <= 3; i++ { // forgotten before their day was listed
		id := fmt.Sprintf("g%d", i)
		x.shows(time.Date(2026, 10, 1, 10, i, 0, 0, time.UTC), "v", "t1", "", "alpha", "prompt", "", id)
		x.ydUsed(id, time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), "")
	}
	m := yieldOf(x, x.query(90))
	wantFloat(t, "value", m.Value, 14.0/45)
	wantInt(t, "num", m.Num, 14)
	if ydExtra(t, m, "gone") != 3 || ydExtra(t, m, "unresolved") != 0 || ydExtra(t, m, "resolved_used") != 14 {
		t.Errorf("gone %v unresolved %v resolved_used %v, want 3, 0 and 14", m.Extra["gone"], m.Extra["unresolved"], m.Extra["resolved_used"])
	}
	if slices.Contains(m.Tags, tagPartial) || m.Extra["withheld"] != nil {
		t.Errorf("tags %v withheld %v, want neither", m.Tags, m.Extra["withheld"])
	}

	// 15 resolved + 1 memory without a row: withheld until the record
	// covers the day it was first shown, then gone.
	y := ydFx(t)
	y.ydDays(ydWeek, 45)
	y.ydUsedMemories("r", 15)
	y.mrec(rcBase, metricsfile.Listed{Day: "2026-09-21", N: 3, Complete: true}) // complete, but the days after are not listed
	y.shows(time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), "v", "t1", "", "alpha", "prompt", "", "lost")
	y.ydUsed("lost", time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC), "")
	m = yieldOf(y, y.query(90))
	if m.Value != nil || ydExtra(t, m, "unresolved") != 1 || ydExtra(t, m, "gone") != 0 {
		t.Errorf("before the days are listed: value %v unresolved %v gone %v, want withheld, 1 and 0", m.Value, m.Extra["unresolved"], m.Extra["gone"])
	}
	for d := ydDate("2026-09-22"); !d.After(ydDate("2026-10-05")); d = d.AddDate(0, 0, 1) {
		y.mrec(rcBase, metricsfile.Listed{Day: d.Format("2006-01-02"), N: 3})
	}
	m = yieldOf(y, y.query(90))
	wantFloat(t, "value once its day is covered", m.Value, 15.0/45)
	if ydExtra(t, m, "gone") != 1 || ydExtra(t, m, "unresolved") != 0 || slices.Contains(m.Tags, tagPartial) {
		t.Errorf("gone %v unresolved %v tags %v, want 1, 0 and no partial", m.Extra["gone"], m.Extra["unresolved"], m.Tags)
	}
}

func TestYieldNeedsTheDayRecord(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	x.ydUsedMemories("r", 25)
	m := yieldOf(x, x.query(90))
	if m.Status != StatusNoData || m.Reason != ReasonNoEvents || m.Value != nil {
		t.Errorf("no day listed: status %q reason %q value %v, want no_data, no_events", m.Status, m.Reason, m.Value)
	}
	// one listed day and no complete line: computed, the unrowed memory unresolved
	x.ydListed()
	x.shows(time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), "v", "t1", "", "alpha", "prompt", "", "lost")
	x.ydUsed("lost", time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC), "")
	m = yieldOf(x, x.query(90))
	if m.Status == StatusNoData {
		t.Fatalf("status no_data once a day is listed")
	}
	if ydExtra(t, m, "unresolved") != 1 {
		t.Errorf("unresolved %v, want 1", m.Extra["unresolved"])
	}
	wantFloat(t, "value", m.Value, 25.0/45)
}

// The cost view carries it with the server's series.
func TestYieldInTheCostView(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	x.ydListed()
	x.ydUsedMemories("r", 25)
	r := x.run("cost", x.query(90))
	if m, ok := r.Metrics[ydToken]; !ok || m.Value == nil {
		t.Fatalf("cost view: %+v", m)
	}
	if !slices.Contains(CostServerNeeds(), yieldSeriesKey) {
		t.Error("the cost view does not read the series")
	}
}

// The weeks are those of the 90 days ending at the window's end, whatever
// the window: a 7- or 30-day view reads the same cohort.
func TestYieldWeeksIgnoreTheViewWindow(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	x.ydListed()
	x.ydUsedMemories("f", 14)
	var want Metric
	for _, days := range []int{90, 30, 7} {
		m := yieldOf(x, x.query(days))
		if days == 90 {
			want = m
		}
		wantFloat(t, fmt.Sprintf("%d days: value", days), m.Value, 14.0/45)
		wantInt(t, fmt.Sprintf("%d days: num", days), m.Num, 14)
		wantInt(t, fmt.Sprintf("%d days: den", days), m.Den, 45)
		if len(m.Series) != len(want.Series) {
			t.Errorf("%d days: %d weeks, want %d", days, len(m.Series), len(want.Series))
		}
	}
}

// With no span old enough, nothing is complete.
func TestYieldNoCompleteWeek(t *testing.T) {
	old := yieldSpan
	yieldSpan = 3 * 24 * time.Hour
	t.Cleanup(func() { yieldSpan = old })
	x := ydFx(t)
	x.ydDays("2026-10-26", 45)
	x.ydListed()
	m := yieldOf(x, x.query(90))
	if m.Status != StatusNoData || m.Reason != ReasonNoCompleteWeek || m.Value != nil {
		t.Errorf("status %q reason %q value %v, want no_data, no_complete_week", m.Status, m.Reason, m.Value)
	}
}

// A memory listed on two days (the later line wins) is in the week of the
// later line only; a day recorded twice counts its latest line, with that
// line's tokens.
func TestYieldReadsTheLatestLine(t *testing.T) {
	x := ydFx(t)
	x.ydDays("2026-09-14", 30)
	x.ydDays(ydWeek, 45)
	x.ydListed()
	x.ydMemory("moved", "2026-09-16") // week of 09-14 ...
	x.mrec(rcBase.Add(time.Hour), metricsfile.MemoryDoc{Memory: "moved", Outcome: "found", DocKind: "session", Document: "conversation:s-moved",
		Session: "s-moved", CreatedDay: "2026-09-22", FactType: "world"}) // ... listed again later: week of 09-21
	x.ydUsed("moved", ydDate("2026-09-17"), "")
	// 2026-09-22 recorded twice: the earlier line is replaced whole
	x.mrec(rcBase, metricsfile.ServerDay{Bucket: ydDate("2026-09-22"), New: &metricsfile.KindCounts{World: 100},
		LLM: map[string]metricsfile.LLMDay{"retain": {Input: 1000, Output: 1000}}})
	x.mrec(rcBase.Add(time.Hour), metricsfile.ServerDay{Bucket: ydDate("2026-09-22"), New: &metricsfile.KindCounts{World: 20},
		LLM: map[string]metricsfile.LLMDay{"retain": {Input: 200, Output: 100}}})
	m := yieldOf(x, x.query(90))
	a, b := weekOf(t, m, "2026-09-14"), weekOf(t, m, ydWeek)
	wantInt(t, "earlier week num", a.Num, 0)
	wantInt(t, "later week num", b.Num, 1)
	// 45 over the week, the 09-22 share (7) replaced by 20
	wantInt(t, "den", b.Den, 45-7+20)
	wantInt(t, "tokens", b.Tokens, 300)
}

// An unrowed memory first shown after the days listed stays unresolved when
// the listing has not reached the latest complete week.
func TestYieldPendingNeedsTheListingToReachTheWeek(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	x.ydUsedMemories("r", 15)
	coverage(x, "2026-08-01", "2026-09-20") // covered until before the week's last day (09-27)
	x.shows(time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC), "v", "t1", "", "alpha", "prompt", "", "fresh")
	x.ydUsed("fresh", time.Date(2026, 9, 26, 8, 30, 0, 0, time.UTC), "")
	m := yieldOf(x, x.query(90))
	if ydExtra(t, m, "unresolved") != 1 || ydExtra(t, m, "pending") != 0 {
		t.Errorf("unresolved %v pending %v, want 1 and 0", m.Extra["unresolved"], m.Extra["pending"])
	}
}

// The server is read over the yield's own 90 days, not the view's: a day
// the record lacks and the server still holds gives the same week at 7, 30
// and 90 days.
func TestYieldServerReadSpansTheNinetyDays(t *testing.T) {
	x := ydFx(t)
	for i := range 7 {
		if i != 3 { // the record has a hole on 09-24
			x.mrec(rcBase, metricsfile.ServerDay{Bucket: ydDate(ydWeek).AddDate(0, 0, i), New: &metricsfile.KindCounts{World: 5}})
		}
	}
	x.ydListed()
	x.ydUsedMemories("s", 14)
	var bs []hindsight.SeriesBucket
	for i := range 7 {
		bs = append(bs, hindsight.SeriesBucket{Time: ydDate(ydWeek).AddDate(0, 0, i), World: 6, Experience: 4})
	}
	x.server.set(func(d *fakeData) { d.Series = hindsight.Timeseries{Period: "90d", Trunc: "day", Buckets: bs} })
	for _, days := range []int{90, 30, 7} {
		m := yieldOf(x, x.query(days))
		p := weekOf(t, m, ydWeek)
		wantInt(t, fmt.Sprintf("%d days: den", days), p.Den, 70)
		wantFloat(t, fmt.Sprintf("%d days: value", days), m.Value, 14.0/70)
		if p.Source != "server" {
			t.Errorf("%d days: source %q, want server", days, p.Source)
		}
	}
}

// The weeks count the whole bank, whatever project the view is filtered to:
// the figure says so; without a project it does not.
func TestYieldIsNotFilteredByProject(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	x.ydListed()
	x.ydUsedMemories("p", 14)
	q := x.query(90)
	if m := yieldOf(x, q); slices.Contains(m.Tags, tagNotFiltered) {
		t.Errorf("no project: tags %v", m.Tags)
	}
	q.Project = "alpha"
	m := yieldOf(x, q)
	if !slices.Contains(m.Tags, tagNotFiltered) {
		t.Errorf("project alpha: tags %v, want %s", m.Tags, tagNotFiltered)
	}
	wantInt(t, "den", m.Den, 45) // the figure itself is unchanged
}

// A briefing page credited used is no memory: it has no day-list row and
// is never unresolved, so it cannot withhold the yield (decision 0083 C8).
func TestYieldIgnoresBriefingPages(t *testing.T) {
	x := ydFx(t)
	x.ydDays(ydWeek, 45)
	x.ydListed()
	x.ydUsedMemories("f", 14)
	page := feedback.BriefingPagePrefix + "alpha-1a2b3c4d"
	x.shows(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC), "v", "", "", "alpha", "briefing", "alpha", page)
	x.ydUsed(page, time.Date(2026, 9, 25, 10, 5, 0, 0, time.UTC), "")
	m := yieldOf(x, x.query(90))
	wantFloat(t, "value", m.Value, 14.0/45)
	if ydExtra(t, m, "unresolved") != 0 || ydExtra(t, m, "gone") != 0 || ydExtra(t, m, "resolved_used") != 14 {
		t.Errorf("unresolved %v gone %v resolved_used %v, want 0, 0 and 14", m.Extra["unresolved"], m.Extra["gone"], m.Extra["resolved_used"])
	}
}
