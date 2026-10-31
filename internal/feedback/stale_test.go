package feedback

import (
	"slices"
	"testing"
	"time"
)

// staleAt is the time TestFeedbackStaleIsMemoryStale lists at.
var staleAt = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// TestFeedbackStaleIsMemoryStale: the three lists of memory stale, each
// memory in one list at most, most shown first then by id. With 10 shows
// as the bar: n-a (15 live shows), n-b and n-c (12 each, by id) are never
// used; imp (30 shown, 25 of them imported: 5 live) and used (credited)
// are not; h-old (hidden 8 days) is listed, h-new (2 days) waits; r-x
// and r-y are reported (r-y shown more, so first), r-y also hidden for
// 1 hour: still reported, not hidden.
func TestFeedbackStaleIsMemoryStale(t *testing.T) {
	l := Ledger{Memories: map[string]Entry{
		"n-b":   {Shown: 12},
		"n-a":   {Shown: 15},
		"n-c":   {Shown: 12},
		"imp":   {Shown: 30, Imported: Imported{Shown: 25}},
		"used":  {Shown: 40, Credits: 0.5},
		"h-old": {Shown: 3, Hidden: &Hidden{Since: staleAt.Add(-8 * 24 * time.Hour)}},
		"h-new": {Shown: 3, Hidden: &Hidden{Since: staleAt.Add(-2 * 24 * time.Hour)}},
		"r-x":   {Shown: 2, Wrong: 1},
		"r-y":   {Shown: 4, Wrong: 2},
	}}
	got := Stale(l, 10, staleAt)
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"never used", got.NeverUsed, []string{"n-a", "n-b", "n-c"}},
		{"hidden", got.Hidden, []string{"h-old"}},
		{"reported", got.Reported, []string{"r-y", "r-x"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestStaleHiddenBoundary: a hiding is listed from exactly StaleHidden on,
// and a hidden memory that is also reported or never used waits with it.
func TestStaleHiddenBoundary(t *testing.T) {
	l := Ledger{Memories: map[string]Entry{
		"exact": {Shown: 1, Hidden: &Hidden{Since: staleAt.Add(-StaleHidden)}},
		"short": {Shown: 20, Wrong: 1, Hidden: &Hidden{Since: staleAt.Add(-StaleHidden + time.Second)}},
	}}
	got := Stale(l, 10, staleAt)
	if !slices.Equal(got.Hidden, []string{"exact"}) || len(got.Reported) != 0 || len(got.NeverUsed) != 0 {
		t.Errorf("lists = %+v, want only exact hidden", got)
	}
}
