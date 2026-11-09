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

// The days counted: exactly 7 in a 7-day window of local midnights (Paris,
// 2026-11-02 to 2026-11-09), today counted up to the time of the read.
func TestExtractionDaysOfAWindow(t *testing.T) {
	readAt := time.Date(2026, 11, 9, 17, 11, 0, 0, time.UTC)
	days := extractionDays(time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC), readAt)
	if len(days) != 7 || days["2026-11-01"] || !days["2026-11-02"] || !days["2026-11-08"] {
		t.Errorf("days %v, want 2026-11-02 to 2026-11-08", days)
	}
	days = extractionDays(time.Date(2026, 11, 2, 17, 11, 0, 0, time.UTC), readAt, readAt.Add(-time.Hour))
	if !days["2026-11-09"] {
		t.Errorf("days %v: today, read at 17:11, counts", days)
	}
}
