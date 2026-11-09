package metrics

import (
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// utcDays are day buckets for the UTC days from first to last of October
// 2026, day d holding d × 1,000 input tokens (so a sum names its days).
func utcDays(first, last int) []hindsight.LLMBucket {
	var out []hindsight.LLMBucket
	for d := first; d <= last; d++ {
		out = append(out, hindsight.LLMBucket{Trunc: "day", Time: time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC), Input: int64(d) * 1000})
	}
	return out
}

// TestExtractionCountsUTCDaysByTheirNoon: a UTC day counts in a window when
// its 12:00 UTC lies in it, so an N-day window counts exactly N days. The
// server's 30-day read holds whole UTC days: a custom 7-day window of local
// midnights (Paris, 2026-10-19T22:00Z to 2026-10-26T23:00Z) counts 10-20 to
// 10-26 (20 + … + 26 = 161 thousand, 7 days), not 10-19, whose 22 hours before
// the window the old rule kept. At the edges: a noon exactly at the start
// counts, one exactly at the end does not. A rolling window's first day,
// which the server cut at the window's start, and today, cut at now, count.
func TestExtractionCountsUTCDaysByTheirNoon(t *testing.T) {
	custom := func(x *fx, since, until time.Time) Query {
		w, err := NewWindow(x.now, 0, since, until, fxParis, "Europe/Paris")
		if err != nil {
			t.Fatal(err)
		}
		return Query{Window: w, Machines: "this"}
	}
	t.Run("local midnights", func(t *testing.T) {
		x := costFx(t)
		x.server.set(func(d *fakeData) { d.LLM = map[string][]hindsight.LLMBucket{"": utcDays(18, 28)} })
		q := custom(x, time.Date(2026, 10, 20, 0, 0, 0, 0, fxParis), time.Date(2026, 10, 27, 0, 0, 0, 0, fxParis))
		m := costOf(x, q)["extraction.tokens"]
		if val(m) != 161_000 || m.Extra["days_covered"] != 7 {
			t.Errorf("tokens %v over %v days, want 161,000 over 7", val(m), m.Extra["days_covered"])
		}
		if first := m.Series[0]; first.Day != "2026-10-20" || first.Value == nil || *first.Value != 20_000 {
			t.Errorf("first day %+v, want 2026-10-20 with 20,000 (10-19 left out)", first)
		}
	})
	t.Run("noon on the edges", func(t *testing.T) {
		x := costFx(t)
		x.server.set(func(d *fakeData) { d.LLM = map[string][]hindsight.LLMBucket{"": utcDays(18, 28)} })
		q := custom(x, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))
		m := costOf(x, q)["extraction.tokens"]
		if val(m) != 63_000 || m.Extra["days_covered"] != 3 {
			t.Errorf("tokens %v over %v days, want 63,000 (10-20, 21, 22) over 3", val(m), m.Extra["days_covered"])
		}
	})
	t.Run("a rolling window", func(t *testing.T) {
		x := costFx(t)
		x.server.set(func(d *fakeData) { d.LLM = map[string][]hindsight.LLMBucket{"": utcDays(23, 30)} })
		m := costOf(x, x.query(7))["extraction.tokens"]
		if val(m) != 212_000 || m.Extra["days_covered"] != 8 {
			t.Errorf("tokens %v over %v days, want 212,000 (10-23, cut at the start, to today, cut at now) over 8", val(m), m.Extra["days_covered"])
		}
	})
}
