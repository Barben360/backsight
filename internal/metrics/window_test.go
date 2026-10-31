package metrics

import (
	"testing"
	"time"
	_ "time/tzdata" // the DST tests name Europe/Paris whatever the host has
)

func parisLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func local(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02T15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustWindow(t *testing.T, now time.Time, days int, since, until time.Time, loc *time.Location) Window {
	t.Helper()
	w, err := NewWindow(now, days, since, until, loc, loc.String())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWindowFromDays(t *testing.T) {
	paris := parisLoc(t)
	now := at(t, "2026-10-30T15:44:00+01:00")
	w := mustWindow(t, now, 7, time.Time{}, time.Time{}, paris)
	// 168 h before, across the DST change of 25 October: the wall clock
	// reads 16:44, not 15:44.
	wantSince := at(t, "2026-10-23T16:44:00+02:00")
	if !w.Since.Equal(wantSince) || !w.Until.Equal(now) {
		t.Fatalf("window = [%v, %v), want [%v, %v)", w.Since, w.Until, wantSince, now)
	}
	if w.Days != 7 || w.Length() != 168*time.Hour {
		t.Errorf("days %d, length %v", w.Days, w.Length())
	}
	if w.Since.Location() != paris || w.Until.Location() != paris || w.TZ != "Europe/Paris" || w.Loc != paris {
		t.Errorf("zone not kept: %v %v %q", w.Since.Location(), w.Until.Location(), w.TZ)
	}
	p := w.Previous()
	if !p.Since.Equal(at(t, "2026-10-16T16:44:00+02:00")) || !p.Until.Equal(w.Since) {
		t.Errorf("previous = [%v, %v)", p.Since, p.Until)
	}
	if p.Days != 7 || p.TZ != w.TZ || p.Loc != paris {
		t.Errorf("previous days %d tz %q", p.Days, p.TZ)
	}
}

func TestWindowDefaultsToUTC(t *testing.T) {
	now := at(t, "2026-10-30T15:44:00+01:00")
	w, err := NewWindow(now, 1, time.Time{}, time.Time{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Loc != time.UTC || w.TZ != "UTC" || w.Since.Location() != time.UTC {
		t.Errorf("zone: %v %q", w.Loc, w.TZ)
	}
	if !w.Since.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("since %v", w.Since)
	}
}

func TestBaselinesAreAlwaysSevenDays(t *testing.T) {
	paris := parisLoc(t)
	now := at(t, "2026-10-30T15:44:00+01:00")
	for _, days := range []int{7, 30, 28, 1} {
		w := mustWindow(t, now, days, time.Time{}, time.Time{}, paris)
		bs := w.Baselines()
		if len(bs) != 4 {
			t.Fatalf("%d days: %d baselines", days, len(bs))
		}
		if !bs[0].Until.Equal(w.Since) {
			t.Errorf("%d days: first baseline ends %v, want since %v", days, bs[0].Until, w.Since)
		}
		for k, b := range bs {
			if b.Length() != 168*time.Hour || b.Days != 7 {
				t.Errorf("%d days: baseline %d lasts %v (%d days)", days, k, b.Length(), b.Days)
			}
			wantSince := w.Since.Add(-time.Duration(168*(k+1)) * time.Hour)
			if !b.Since.Equal(wantSince) {
				t.Errorf("%d days: baseline %d starts %v, want %v", days, k, b.Since, wantSince)
			}
			if k > 0 && !b.Until.Equal(bs[k-1].Since) {
				t.Errorf("%d days: baseline %d does not abut the newer one", days, k)
			}
		}
		if got := w.Since.Sub(bs[3].Since); got != 672*time.Hour {
			t.Errorf("%d days: last baseline starts %v before since, want 672h", days, got)
		}
	}
}

func TestWindowLast(t *testing.T) {
	paris := parisLoc(t)
	w := mustWindow(t, at(t, "2026-10-30T15:44:00+01:00"), 30, time.Time{}, time.Time{}, paris)
	l := w.Last(7 * 24 * time.Hour)
	if !l.Until.Equal(w.Until) || l.Length() != 168*time.Hour || l.Days != 7 || l.Loc != paris {
		t.Errorf("last = [%v, %v) days %d", l.Since, l.Until, l.Days)
	}
	if l := w.Last(28 * 24 * time.Hour); l.Days != 28 {
		t.Errorf("last 28d days = %d", l.Days)
	}
}

func TestWindowFromSince(t *testing.T) {
	paris := parisLoc(t)
	since := local(t, paris, "2026-10-01T00:00")
	until := local(t, paris, "2026-10-08T12:00")
	now := at(t, "2026-10-30T15:44:00+01:00")
	w := mustWindow(t, now, 0, since, until, paris)
	if !w.Since.Equal(since) || !w.Until.Equal(until) || w.Days != 8 {
		t.Errorf("window [%v, %v) days %d, want days 8 (7.5 rounded up)", w.Since, w.Until, w.Days)
	}
	// Without an end the window runs to now.
	w = mustWindow(t, now, 0, since, time.Time{}, paris)
	if !w.Until.Equal(now) {
		t.Errorf("until = %v, want now", w.Until)
	}
	// Exact multiples do not round up.
	w = mustWindow(t, now, 0, at(t, "2026-10-01T00:00:00Z"), at(t, "2026-10-08T00:00:00Z"), time.UTC)
	if w.Days != 7 {
		t.Errorf("exactly 7×24h: days %d", w.Days)
	}

	for name, c := range map[string]struct {
		days         int
		since, until time.Time
	}{
		"until before since": {0, until, since},
		"until equals since": {0, since, since},
		"days and since":     {7, since, until},
		"neither":            {0, time.Time{}, time.Time{}},
		"negative days":      {-1, time.Time{}, time.Time{}},
	} {
		if w, err := NewWindow(now, c.days, c.since, c.until, paris, "Europe/Paris"); err == nil {
			t.Errorf("%s: no error, window %v", name, w)
		}
	}
}

func keys(bs []Bucket) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Key
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDayBucketsAcrossDST(t *testing.T) {
	paris := parisLoc(t)

	// Autumn: 48 wall-clock hours plus the repeated one.
	w := mustWindow(t, time.Time{}, 0, local(t, paris, "2026-10-24T00:00"), local(t, paris, "2026-10-27T00:00"), paris)
	bs := w.Buckets()
	if want := []string{"2026-10-24", "2026-10-25", "2026-10-26"}; !equalStrings(keys(bs), want) {
		t.Fatalf("keys %v, want %v", keys(bs), want)
	}
	for i, want := range []time.Duration{24, 25, 24} {
		if got := bs[i].Until.Sub(bs[i].Since); got != want*time.Hour {
			t.Errorf("%s lasts %v, want %vh", bs[i].Key, got, int(want))
		}
	}
	// An event at 02:30+01:00 (the second 02:30 of the night) lands in one
	// bucket, the 25th; so does the first 02:30 (+02:00).
	for _, ev := range []time.Time{at(t, "2026-10-25T02:30:00+01:00"), at(t, "2026-10-25T02:30:00+02:00")} {
		var in []string
		for _, b := range bs {
			if !ev.Before(b.Since) && ev.Before(b.Until) {
				in = append(in, b.Key)
			}
		}
		if !equalStrings(in, []string{"2026-10-25"}) {
			t.Errorf("event %v lands in %v, want [2026-10-25] once", ev, in)
		}
	}
	// Half-open edges: midnight belongs to the day it starts.
	if !bs[0].Until.Equal(bs[1].Since) || !bs[1].Until.Equal(bs[2].Since) {
		t.Errorf("buckets do not abut")
	}

	// Spring: the 29th of March has 23 hours.
	w = mustWindow(t, time.Time{}, 0, local(t, paris, "2026-03-28T00:00"), local(t, paris, "2026-03-31T00:00"), paris)
	bs = w.Buckets()
	if want := []string{"2026-03-28", "2026-03-29", "2026-03-30"}; !equalStrings(keys(bs), want) {
		t.Fatalf("keys %v, want %v", keys(bs), want)
	}
	for i, want := range []time.Duration{24, 23, 24} {
		if got := bs[i].Until.Sub(bs[i].Since); got != want*time.Hour {
			t.Errorf("%s lasts %v, want %vh", bs[i].Key, got, int(want))
		}
	}
}

func TestBucketsTileTheWindow(t *testing.T) {
	paris := parisLoc(t)
	// A rolling 7-day window across the autumn change: the first and last
	// day are partial, and the buckets still add up to the window.
	now := at(t, "2026-10-30T15:44:00+01:00")
	for _, w := range []Window{
		mustWindow(t, now, 7, time.Time{}, time.Time{}, paris),
		mustWindow(t, now, 2, time.Time{}, time.Time{}, paris), // 48 h: hourly
		mustWindow(t, time.Time{}, 0, local(t, paris, "2026-10-25T10:30"), local(t, paris, "2026-10-25T12:30"), paris),
		mustWindow(t, now, 90, time.Time{}, time.Time{}, time.UTC),
	} {
		bs := w.Buckets()
		if len(bs) == 0 {
			t.Fatalf("%v: no buckets", w)
		}
		if !bs[0].Since.Equal(w.Since) || !bs[len(bs)-1].Until.Equal(w.Until) {
			t.Errorf("%v: buckets span [%v, %v)", w.Length(), bs[0].Since, bs[len(bs)-1].Until)
		}
		var total time.Duration
		for i, b := range bs {
			total += b.Until.Sub(b.Since)
			if b.Until.Sub(b.Since) <= 0 {
				t.Errorf("empty bucket %s", b.Key)
			}
			if i > 0 && !b.Since.Equal(bs[i-1].Until) {
				t.Errorf("gap or overlap before %s", b.Key)
			}
		}
		if total != w.Length() {
			t.Errorf("buckets add up to %v, window is %v", total, w.Length())
		}
	}
	// The 7-day window: 8 local days, first partial (16:44 to midnight).
	w := mustWindow(t, now, 7, time.Time{}, time.Time{}, paris)
	bs := w.Buckets()
	if len(bs) != 8 || bs[0].Key != "2026-10-23" || bs[7].Key != "2026-10-30" {
		t.Errorf("7-day keys %v", keys(bs))
	}
	if got := bs[0].Until.Sub(bs[0].Since); got != 7*time.Hour+16*time.Minute {
		t.Errorf("first day clipped to %v, want 7h16m", got)
	}
}

func TestHourlyBuckets(t *testing.T) {
	paris := parisLoc(t)
	// 48 h exactly (elapsed) over the autumn change: 48 hourly buckets, the
	// repeated 02:00 hour told apart by its offset.
	now := at(t, "2026-10-26T12:00:00+01:00")
	w := mustWindow(t, now, 2, time.Time{}, time.Time{}, paris)
	bs := w.Buckets()
	if len(bs) != 48 {
		t.Fatalf("%d buckets, want 48", len(bs))
	}
	if bs[0].Key != "2026-10-24T13:00+02:00" || bs[47].Key != "2026-10-26T11:00+01:00" {
		t.Errorf("first %q last %q", bs[0].Key, bs[47].Key)
	}
	seen := map[string]bool{}
	for _, b := range bs {
		if seen[b.Key] {
			t.Errorf("key %s twice", b.Key)
		}
		seen[b.Key] = true
		if b.Until.Sub(b.Since) != time.Hour {
			t.Errorf("%s lasts %v", b.Key, b.Until.Sub(b.Since))
		}
	}
	if !seen["2026-10-25T02:00+02:00"] || !seen["2026-10-25T02:00+01:00"] {
		t.Errorf("the repeated 02:00 hour is missing: %v", keys(bs))
	}

	// 49 h: daily.
	w = mustWindow(t, time.Time{}, 0, local(t, paris, "2026-10-24T14:00"), local(t, paris, "2026-10-26T14:00"), paris)
	if w.Length() != 49*time.Hour { // 48 wall-clock hours plus the repeated one
		t.Fatalf("setup: %v", w.Length())
	}
	if want := []string{"2026-10-24", "2026-10-25", "2026-10-26"}; !equalStrings(keys(w.Buckets()), want) {
		t.Errorf("49 h keys %v, want %v", keys(w.Buckets()), want)
	}

	// UTC, hourly, 24 h.
	w = mustWindow(t, at(t, "2026-10-24T10:00:00Z"), 1, time.Time{}, time.Time{}, time.UTC)
	if bs := w.Buckets(); len(bs) != 24 || bs[0].Key != "2026-10-23T10:00+00:00" {
		t.Errorf("UTC: %d buckets, first %q", len(bs), bs[0].Key)
	}
}

func TestBucketsInUTC(t *testing.T) {
	// --tz UTC: days are UTC days, whatever the host zone.
	w := mustWindow(t, time.Time{}, 0, at(t, "2026-10-24T00:00:00Z"), at(t, "2026-10-27T00:00:00Z"), time.UTC)
	bs := w.Buckets()
	if want := []string{"2026-10-24", "2026-10-25", "2026-10-26"}; !equalStrings(keys(bs), want) {
		t.Fatalf("keys %v", keys(bs))
	}
	for _, b := range bs {
		if b.Until.Sub(b.Since) != 24*time.Hour {
			t.Errorf("%s lasts %v in UTC", b.Key, b.Until.Sub(b.Since))
		}
	}
	// An event at 23:30 Paris on the 24th is 21:30Z the 24th, but 00:30
	// Paris on the 25th is 22:30Z the 24th: the same instant lands on
	// different days in the two zones.
	ev := at(t, "2026-10-24T22:30:00Z")
	dayOf := func(w Window) string {
		for _, b := range w.Buckets() {
			if !ev.Before(b.Since) && ev.Before(b.Until) {
				return b.Key
			}
		}
		return ""
	}
	paris := parisLoc(t)
	wp := mustWindow(t, time.Time{}, 0, local(t, paris, "2026-10-24T00:00"), local(t, paris, "2026-10-27T00:00"), paris)
	if dayOf(w) != "2026-10-24" || dayOf(wp) != "2026-10-25" {
		t.Errorf("UTC day %q, Paris day %q", dayOf(w), dayOf(wp))
	}
}
