package metrics

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The daily fixtures are in UTC: day 1 is 2026-11-01, day 2 2026-11-02, and Daily runs at 2026-11-03T10:00Z.

var dailyAt = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

func utc(d int, hh int) time.Time { return time.Date(2026, 11, d, hh, 0, 0, 0, time.UTC) }

// dailyPrompt writes a prompt line with only the fields given (a recall that carries no ms has no ms in the
// index, as a skipped one).
func dailyPrompt(x *fx, at time.Time, n int, session string, f map[string]any) {
	x.t.Helper()
	fields := map[string]any{"n": n, "plen": 80}
	for k, v := range f {
		fields[k] = v
	}
	x.tel(at, telemetry.Raw{Name: "prompt", Fields: fields}, session, "alpha")
}

func (x *fx) daily() []machines.Day {
	x.t.Helper()
	days, err := Daily(context.Background(), x.source(), dailyAt)
	if err != nil {
		x.t.Fatal(err)
	}
	return days
}

// dailyFixture is two UTC days.
//
// Day 1 (binary 0.1.18): session S has 10 prompt recalls, 4 of them injecting; the first carries 700 chars, the
// latencies are 40, 2000, 2001 and 6000 ms on four of them; S also has a briefing of 500 chars; 2 machine prompts
// (skips) do not count; 4 turns: T1 declared with 2 used credits, T2 declared with 1 wrong credit, T3 declared
// without credit, T4 undeclared; a document sent ok.
//
// Day 2: 4 recalls, 2 injecting; p1 (v9.0, session S) 500 chars; p3 (v9.1) 300 ms; turn T5 (v9.0) declared,
// turn T6 (v9.1) declared with a used credit.
func dailyFixture(t *testing.T) *fx {
	x := newFx(t, dailyAt)
	ms := map[int]int{0: 40, 1: 2000, 2: 2001, 3: 6000}
	for i := range 10 {
		f := map[string]any{"decision": "recall", "injected": 0, "empty": "no_candidates"}
		if i < 4 {
			f["injected"], f["empty"] = 1, ""
		}
		if v, ok := ms[i]; ok {
			f["ms"] = v
		}
		if i == 0 {
			f["chars"] = 700
		}
		dailyPrompt(x, utc(1, 8).Add(time.Duration(i)*time.Minute), i+1, "S", f)
	}
	for i := range 2 {
		dailyPrompt(x, utc(1, 9).Add(time.Duration(i)*time.Minute), 20+i, "S", map[string]any{"decision": "skip", "reason": "machine", "chars": 9000})
	}
	x.tel(utc(1, 7), telemetry.Briefing{Chars: 500, Source: "raw"}, "S", "alpha")
	x.declaredTurn(utc(1, 10), "T1", "declared", "used", "m1", "m2")
	x.declaredTurn(utc(1, 11), "T2", "declared", "wrong", "m3")
	x.declaredTurn(utc(1, 12), "T3", "declared", "", "m5")
	x.declaredTurn(utc(1, 13), "T4", "undeclared", "", "m6")
	x.retained(utc(1, 14), "doc1", "ok", 10)

	x.binary = "v9.0"
	dailyPrompt(x, utc(2, 8), 31, "S", map[string]any{"decision": "recall", "injected": 1, "chars": 500})
	dailyPrompt(x, utc(2, 8).Add(time.Minute), 32, "", map[string]any{"decision": "recall", "injected": 0, "empty": "no_candidates"})
	x.declaredTurn(utc(2, 10), "T5", "declared", "", "m7")
	x.binary = "v9.1"
	dailyPrompt(x, utc(2, 9), 33, "", map[string]any{"decision": "recall", "injected": 1, "ms": 300})
	dailyPrompt(x, utc(2, 9).Add(time.Minute), 34, "", map[string]any{"decision": "recall", "injected": 0, "empty": "no_candidates"})
	x.declaredTurn(utc(2, 11), "T6", "declared", "used", "m8")
	return x
}

func TestDailyFigures(t *testing.T) {
	days := dailyFixture(t).daily()
	if len(days) != 2 || days[0].Day != "2026-11-01" || days[1].Day != "2026-11-02" {
		t.Fatalf("days = %+v, want 2026-11-01 and 2026-11-02", days)
	}
	d1 := days[0]
	want1 := machines.Figures{
		Prompts: 10, Hit: machines.Pair{4, 10}, Used: machines.Pair{1, 3}, Coverage: machines.Pair{3, 4},
		Wrong: machines.Pair{1, 3}, Context: machines.Pair{1200, 1}, Latency: machines.Latency{1, 0, 0, 0, 0, 1, 1, 0, 1}}
	if d1.Figures != want1 {
		t.Errorf("day 1 = %+v, want %+v", d1.Figures, want1)
	}
	if d1.Flow != (machines.Pair{1, 1}) {
		t.Errorf("day 1 flow = %v, want [1 1]", d1.Flow)
	}
	if len(d1.Versions) != 1 || d1.Versions["0.1.18"] != want1 {
		t.Errorf("day 1 versions = %+v, want only 0.1.18 with the day's figures", d1.Versions)
	}

	d2 := days[1]
	want2 := machines.Figures{
		Prompts: 4, Hit: machines.Pair{2, 4}, Used: machines.Pair{1, 2}, Coverage: machines.Pair{2, 2},
		Wrong: machines.Pair{0, 1}, Context: machines.Pair{500, 0}, Latency: machines.Latency{0, 0, 0, 1, 0, 0, 0, 0, 0}}
	if d2.Figures != want2 {
		t.Errorf("day 2 = %+v, want %+v", d2.Figures, want2)
	}
	if d2.Flow != (machines.Pair{}) {
		t.Errorf("day 2 flow = %v, want [0 0]", d2.Flow)
	}
	wantV := map[string]machines.Figures{
		"v9.0": {Prompts: 2, Hit: machines.Pair{1, 2}, Coverage: machines.Pair{1, 1}, Used: machines.Pair{0, 1}, Context: machines.Pair{500, 0}},
		"v9.1": {Prompts: 2, Hit: machines.Pair{1, 2}, Coverage: machines.Pair{1, 1}, Used: machines.Pair{1, 1}, Wrong: machines.Pair{0, 1},
			Latency: machines.Latency{0, 0, 0, 1, 0, 0, 0, 0, 0}},
	}
	if len(d2.Versions) != 2 || d2.Versions["v9.0"] != wantV["v9.0"] || d2.Versions["v9.1"] != wantV["v9.1"] {
		t.Errorf("day 2 versions = %+v, want %+v", d2.Versions, wantV)
	}
}

// metricOf is id over [from, to) (UTC) through the views' own path.
func metricOf(x *fx, id string, from, to time.Time) Metric {
	x.t.Helper()
	w, err := NewWindow(to, 0, from, to, time.UTC, "UTC")
	if err != nil {
		x.t.Fatal(err)
	}
	return x.metrics([]string{id}, Query{Window: w, Machines: "this"})[id]
}

func TestDailyAgreesWithTheViews(t *testing.T) {
	x := dailyFixture(t)
	days := x.daily()
	pairOf := func(d machines.Day, id string) machines.Pair {
		switch id {
		case "recall.hit_rate":
			return d.Hit
		case "used.turn_rate":
			return d.Used
		case "declare.coverage":
			return d.Coverage
		case "verdict.wrong_rate":
			return d.Wrong
		}
		return d.Flow
	}
	ids := []string{"recall.hit_rate", "used.turn_rate", "declare.coverage", "verdict.wrong_rate", "pipeline.flow"}
	for _, id := range ids {
		var sum machines.Pair
		for i, d := range days {
			from := utc(1+i, 0)
			m := metricOf(x, id, from, from.AddDate(0, 0, 1))
			got, want := pairOf(d, id), machines.Pair{*m.Num, *m.Den}
			if got != want {
				t.Errorf("%s on %s: daily %v, view %v", id, d.Day, got, want)
			}
			sum[0] += got[0]
			sum[1] += got[1]
		}
		m := metricOf(x, id, utc(1, 0), utc(3, 0))
		if want := (machines.Pair{*m.Num, *m.Den}); sum != want {
			t.Errorf("%s over the range: daily sums %v, view %v", id, sum, want)
		}
	}
	var n int64
	for _, d := range days {
		for _, c := range d.Latency {
			n += c
		}
	}
	if m := metricOf(x, "recall.latency", utc(1, 0), utc(3, 0)); n != 5 || *m.Den != n {
		t.Errorf("latency counts sum to %d, the view's n is %d, want 5 and 5", n, *m.Den)
	}
}

func TestDailyCountsADocumentOnce(t *testing.T) {
	x := newFx(t, dailyAt)
	x.retained(utc(1, 10), "doc", "queued", 0)
	x.retained(utc(1, 11), "other", "ok", 5)
	x.retained(utc(2, 10), "doc", "failed", 0)
	x.retained(utc(2, 11), "doc", "ok", 5) // the last send, on day 2
	days := x.daily()
	if len(days) != 2 {
		t.Fatalf("days = %+v, want 2", days)
	}
	// day 1: "other" (ok); day 2: "doc" (ok); "doc" is not counted again on day 1
	if days[0].Flow != (machines.Pair{1, 1}) || days[1].Flow != (machines.Pair{1, 1}) {
		t.Errorf("flow = %v and %v, want [1 1] each: a document counts on the day of its last send only", days[0].Flow, days[1].Flow)
	}
}

func TestDailyCountsASessionOnce(t *testing.T) {
	x := newFx(t, dailyAt)
	dailyPrompt(x, utc(1, 10), 1, "S", map[string]any{"decision": "recall", "injected": 1, "chars": 300})
	dailyPrompt(x, utc(2, 10), 2, "S", map[string]any{"decision": "recall", "injected": 1, "chars": 200})
	x.tel(utc(2, 11), telemetry.Briefing{Chars: 50, Source: "raw"}, "S", "alpha")
	days := x.daily()
	if len(days) != 2 || days[0].Context != (machines.Pair{300, 1}) || days[1].Context != (machines.Pair{250, 0}) {
		t.Errorf("context = %+v, want [300 1] then [250 0]", days)
	}
}

func TestDailyKeepsEightVersions(t *testing.T) {
	x := newFx(t, dailyAt)
	// versions w0..w9: wN has N+1 prompts, except w0 which has 2 like w1
	for v := range 10 {
		x.binary = fmt.Sprintf("w%d", v)
		for i := range max(v+1, 2) {
			dailyPrompt(x, utc(1, 8).Add(time.Duration(v*20+i)*time.Minute), v*20+i+1, "", map[string]any{"decision": "skip", "reason": "x"})
		}
	}
	days := x.daily()
	if len(days) != 1 || days[0].Prompts != 2+2+3+4+5+6+7+8+9+10 {
		t.Fatalf("days = %+v, want one day keeping all 10 versions' prompts (56)", days)
	}
	// the 8 with the most prompts are w2..w9; w0 and w1 (2 each) are the two fewest
	if len(days[0].Versions) != 8 {
		t.Fatalf("%d versions, want 8: %v", len(days[0].Versions), days[0].Versions)
	}
	for _, k := range []string{"w0", "w1"} {
		if _, ok := days[0].Versions[k]; ok {
			t.Errorf("version %s kept; the 8 with the most prompts are w2..w9", k)
		}
	}
}

func TestDailyTiesByName(t *testing.T) {
	x := newFx(t, dailyAt)
	for v := range 9 { // nine versions, one prompt each: the first 8 by name stay
		x.binary = fmt.Sprintf("t%d", v)
		dailyPrompt(x, utc(1, 8).Add(time.Duration(v)*time.Minute), v+1, "", map[string]any{"decision": "skip", "reason": "x"})
	}
	days := x.daily()
	if len(days) != 1 {
		t.Fatalf("days = %+v, want 1", days)
	}
	if _, ok := days[0].Versions["t8"]; ok || len(days[0].Versions) != 8 || days[0].Prompts != 9 {
		t.Errorf("versions = %v (day prompts %d), want t0..t7", days[0].Versions, days[0].Prompts)
	}
}

func TestDailyStopsAt400Days(t *testing.T) {
	x := newFx(t, dailyAt)
	start := time.Date(2025, 8, 10, 8, 0, 0, 0, time.UTC) // 450 days before 2026-11-03
	for i := range 451 {                                  // the last one is on `at`'s own day
		dailyPrompt(x, start.AddDate(0, 0, i), i+1, "", map[string]any{"decision": "skip", "reason": "x"})
	}
	days := x.daily()
	if len(days) == 0 {
		t.Fatal("no days")
	}
	if len(days) != machines.MaxDays {
		t.Fatalf("%d days, want %d", len(days), machines.MaxDays)
	}
	if last := days[len(days)-1].Day; last != "2026-11-02" {
		t.Errorf("last day %s, want 2026-11-02 (the day before at)", last)
	}
	if first := days[0].Day; first != "2025-09-29" {
		t.Errorf("first day %s, want 2025-09-29 (400 days ending 2026-11-02)", first)
	}
}

func TestDailyEmptyAndServerOnly(t *testing.T) {
	x := newFx(t, dailyAt)
	if days := x.daily(); days != nil {
		t.Errorf("an empty index gave %+v, want nil", days)
	}
	y := newFx(t, dailyAt)
	dailyPrompt(y, utc(1, 8), 1, "", map[string]any{"decision": "recall", "injected": 1})
	if days := y.daily(); len(days) != 1 {
		t.Fatalf("days = %+v, want 1", days)
	}
	y.cfg = func(c *config.Config) { c.Server.Role = config.RoleServer }
	if days := y.daily(); days != nil {
		t.Errorf("a server-only host gave %+v, want nil", days)
	}
}

func TestDailyDropsInvalidVersions(t *testing.T) {
	x := newFx(t, dailyAt)
	x.binary = "v1 CANARY"
	dailyPrompt(x, utc(1, 8), 1, "", map[string]any{"decision": "recall", "injected": 1})
	dailyPrompt(x, utc(1, 9), 2, "", map[string]any{"decision": "recall", "injected": 0, "empty": "no_candidates"})
	x.declaredTurn(utc(1, 10), "T1", "declared", "used", "m1")
	x.binary = "v2"
	dailyPrompt(x, utc(1, 11), 3, "", map[string]any{"decision": "recall", "injected": 1})
	days := x.daily()
	if len(days) != 1 {
		t.Fatalf("days = %+v", days)
	}
	d := days[0]
	if d.Prompts != 3 || d.Hit != (machines.Pair{2, 3}) || d.Used != (machines.Pair{1, 1}) {
		t.Errorf("day = %+v, want the invalid version's rows counted: 3 prompts, hit [2 3], used [1 1]", d.Figures)
	}
	if len(d.Versions) != 1 || d.Versions["v2"].Prompts != 1 || d.Versions["v2"].Hit != (machines.Pair{1, 1}) {
		t.Errorf("versions = %+v, want only v2 with 1 prompt", d.Versions)
	}
}

func TestRankMatchesPercentile(t *testing.T) {
	rng := rand.New(rand.NewSource(20261106))
	for set := range 200 {
		n := 1 + rng.Intn(500)
		samples := make([]int64, n)
		var counts [9]int64
		for i := range samples {
			samples[i] = int64(1 + rng.Intn(9000))
			counts[machines.Bucket(samples[i])]++
		}
		for _, p := range []int{50, 95} {
			got, ok := machines.Rank(counts, p)
			want := machines.Bucket(int64(Percentile(samples, p)))
			if !ok || got != want {
				t.Fatalf("set %d (n=%d), p%d: Rank = %d (%v), Percentile's bucket = %d", set, n, p, got, ok, want)
			}
		}
	}
}

// A prompt belongs to the UTC day it fell in: 23:59:59.999Z is that day's,
// 00:00:00Z the next one's (prompts, hit, and latency alike).
func TestDailyAssignsDaysAtTheUTCBoundary(t *testing.T) {
	x := newFx(t, dailyAt)
	dailyPrompt(x, utc(1, 23).Add(59*time.Minute+59*time.Second+999*time.Millisecond), 1, "S",
		map[string]any{"decision": "recall", "injected": 1, "ms": 40})
	dailyPrompt(x, utc(2, 0), 2, "S", map[string]any{"decision": "recall", "injected": 0, "empty": "no_candidates", "ms": 6000})
	days := x.daily()
	if len(days) != 2 || days[0].Day != "2026-11-01" || days[1].Day != "2026-11-02" {
		t.Fatalf("days = %+v, want 2026-11-01 and 2026-11-02", days)
	}
	if d := days[0]; d.Prompts != 1 || d.Hit != (machines.Pair{1, 1}) || d.Latency != (machines.Latency{1, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Errorf("23:59:59.999Z: %+v, want 1 prompt, hit [1 1], the 40 ms bucket", d.Figures)
	}
	if d := days[1]; d.Prompts != 1 || d.Hit != (machines.Pair{0, 1}) || d.Latency != (machines.Latency{0, 0, 0, 0, 0, 0, 0, 0, 1}) {
		t.Errorf("00:00:00Z: %+v, want 1 prompt, hit [0 1], the 6000 ms bucket", d.Figures)
	}
}
