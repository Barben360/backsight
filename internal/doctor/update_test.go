package doctor

import (
	"encoding/json"
	"os"
	"os/exec"
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
	if c := checkUpdate(d, "0.1.0", now); c.Status != Info || !strings.Contains(c.Summary, "last update check failed") || !strings.Contains(c.Detail, "404") || c.Fix == "" || strings.Contains(c.Detail, "previous check") {
		t.Errorf("failed: %+v", c)
	}
	// A failed check that kept the release an earlier one found names it.
	record(state.UpdateCheck{CheckedAt: now.Add(-3 * time.Hour), Latest: "0.2.0", Error: "answered 503"})
	if c := checkUpdate(d, "0.1.0", now); !strings.Contains(c.Detail, "answered 503; the previous check found 0.2.0") {
		t.Errorf("failed after a found release: %+v", c)
	}
	if c := checkUpdate(d, "0.2.0", now); strings.Contains(c.Detail, "previous check") {
		t.Errorf("a release no newer than the running one is not named: %+v", c)
	}
	record(state.UpdateCheck{CheckedAt: now.Add(-3 * time.Hour), Latest: "0.2.0", Summary: "✨ Add update check"})
	// The version only: release notes are unsigned text (decision 0067
	// block 2 review), shown by backsight changelog, never by doctor.
	if c := checkUpdate(d, "0.1.0", now); c.Status != Info || c.Summary != "update 0.2.0 available" || c.Fix != "backsight update apply" ||
		!strings.Contains(c.Detail, "backsight changelog") || strings.Contains(c.Summary+c.Detail+c.Fix, "Add update check") {
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

// update-incomplete: an update apply that is running is information; one
// whose process is gone is an error naming the resume and the way back,
// or the resume only once it passed its commit point.
func TestCheckUpdateIncomplete(t *testing.T) {
	f := newFixture(t)
	d := f.env.Dirs
	if c := checkUpdateIncomplete(d); c.Status != OK {
		t.Errorf("no marker: %+v", c)
	}
	m := state.UpdateMarker{From: "0.1.0", To: "0.2.0", Previous: "/d/bin/0.1.0-aaaaaaaaaaaa/backsight", Binary: "/d/bin/0.2.0-bbbbbbbbbbbb/backsight",
		WayBack: "/d/bin/0.1.0-aaaaaaaaaaaa/backsight client install --config-dir /p", StartedAt: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
	if err := state.WriteUpdateMarker(d, m); err != nil {
		t.Fatal(err)
	}
	c := checkUpdateIncomplete(d)
	if c.Status != Error || !strings.Contains(c.Fix, m.Binary+" update apply") || !strings.Contains(c.Fix, m.WayBack+" (back to 0.1.0)") {
		t.Errorf("unfinished: %+v", c)
	}
	// Running now: update apply holds the maintenance lock.
	sleep := exec.Command("sleep", "600")
	if err := sleep.Start(); err != nil {
		t.Skip("no sleep:", err)
	}
	t.Cleanup(func() { _ = sleep.Process.Kill(); _ = sleep.Wait() })
	raw, _ := json.Marshal(state.Owner{PID: sleep.Process.Pid, Command: "update apply", Since: time.Now().UTC()})
	_ = os.MkdirAll(d.State, 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), raw, 0o600)
	if c := checkUpdateIncomplete(d); c.Status != Info || !strings.Contains(c.Summary, "update from 0.1.0 to 0.2.0 in progress") {
		t.Errorf("in progress: %+v", c)
	}
	_ = os.Remove(d.MaintenanceLock())
	// Committed: never undone, only resumed.
	m.Committed = true
	_ = state.WriteUpdateMarker(d, m)
	if c := checkUpdateIncomplete(d); c.Status != Error || !strings.Contains(c.Fix, m.Binary+" update apply") || strings.Contains(c.Fix, "client install") {
		t.Errorf("committed: %+v", c)
	}
	// A marker that cannot be read is still an unfinished update.
	_ = os.WriteFile(d.UpdateMarker(), []byte("{garbled"), 0o600)
	if c := checkUpdateIncomplete(d); c.Status != Error || c.Detail == "" || !strings.Contains(c.Fix, "client install") {
		t.Errorf("corrupt: %+v", c)
	}
}
