package cli

import (
	"strings"
	"testing"
	"time"
)

// `backsight stats` counts a UTC day's extraction when the day's noon lies in
// the window, as the dashboard does: a window starting at 22:00 UTC (a local
// midnight east of UTC) leaves out the UTC day it starts on, of which it
// holds 2 hours; the old rule kept that day whole.
func TestStatsExtractionCountsUTCDaysByTheirNoon(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	llmInput.Store(2_000_000)    // today
	llmOldInput.Store(5_000_000) // 10 days ago
	at := func(back, hour int) string {
		return time.Now().UTC().AddDate(0, 0, -back).Format("2006-01-02") + "T" + time.Date(0, 1, 1, hour, 0, 0, 0, time.UTC).Format("15:04") + "Z"
	}
	_, out, _ := run(t, "stats", "--since", at(10, 22), "--until", at(8, 22))
	if !strings.Contains(out, " 0 input") {
		t.Errorf("the day 10 days ago holds 2 h of the window: left out\n%s", out)
	}
	if _, out, _ := run(t, "stats", "--since", at(10, 12), "--until", at(8, 22)); !strings.Contains(out, "5000000 input") {
		t.Errorf("a noon exactly at the start counts\n%s", out)
	}
	if _, out, _ := run(t, "stats", "--since", at(12, 22), "--until", at(10, 12)); !strings.Contains(out, " 0 input") {
		t.Errorf("a noon exactly at the end does not count\n%s", out)
	}
}

// The old report reads the server as the dashboard's views do: the same
// period for the same window (a rolling 3 or 7 days: 7d, cut by the server
// at the window's start; 14: 30d), so the two give the same extraction
// figures on every window.
func TestStatsAsksTheViewsPeriod(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	for _, c := range []struct{ days, period string }{{"3", "7d"}, {"7", "7d"}, {"14", "30d"}} {
		run(t, "stats", c.days)
		if llmPeriod != c.period {
			t.Errorf("stats %s asked %q, want %q (the views')", c.days, llmPeriod, c.period)
		}
	}
}
