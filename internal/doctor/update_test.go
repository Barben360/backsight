package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

// The update check is information only: never a warning, even when the
// last check failed (the repository is private until M7).
func TestCheckUpdate(t *testing.T) {
	f := newFixture(t)
	d := f.env.Dirs
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	record := func(uc state.UpdateCheck) {
		t.Helper()
		if err := state.Update(d, "test", func(s *state.State) error { s.UpdateCheck = &uc; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if c := checkUpdate(d, "0.1.0", now); c.ID != "update" || c.Status != Info || !strings.Contains(c.Summary, "no update check yet") {
		t.Errorf("none: %+v", c)
	}
	record(state.UpdateCheck{CheckedAt: now.Add(-3 * time.Hour), Source: "github:Barben360/backsight", Error: "release source github:Barben360/backsight unreadable: answered 404"})
	if c := checkUpdate(d, "0.1.0", now); c.Status != Info || !strings.Contains(c.Summary, "last update check failed") || !strings.Contains(c.Detail, "404") || c.Fix == "" {
		t.Errorf("failed: %+v", c)
	}
	record(state.UpdateCheck{CheckedAt: now.Add(-3 * time.Hour), Latest: "0.2.0", Summary: "✨ Add update check"})
	if c := checkUpdate(d, "0.1.0", now); c.Status != Info || c.Summary != "update 0.2.0 available (✨ Add update check)" {
		t.Errorf("available: %+v", c)
	}
	if c := checkUpdate(d, "0.2.0", now); c.Status != OK || !strings.Contains(c.Summary, "up to date") {
		t.Errorf("since updated: %+v", c)
	}
	record(state.UpdateCheck{CheckedAt: now.Add(-3 * time.Hour)})
	if c := checkUpdate(d, "0.1.0", now); c.Status != OK || !strings.Contains(c.Summary, "up to date (checked 2026-10-25 09:00 UTC)") {
		t.Errorf("up to date: %+v", c)
	}
}
