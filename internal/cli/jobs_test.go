package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

func TestJobsTickSendsSettledSessionsAndNotes(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // first tick: retain_since = 08:00
	writeTranscript(t, home, "settled", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	writeTranscript(t, home, "fresh", time.Date(2026, 10, 24, 9, 50, 0, 0, time.UTC))
	_, _ = notesOutbox(pathsOf(home)).Append(newTestNote())
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	before := retained.Load()
	code, out, errOut := run(t, "jobs", "tick")
	if code != output.OK || !strings.Contains(out, "sent:1") || !strings.Contains(out, "waiting:1") || !strings.Contains(out, "notes sent 1") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if retained.Load() != before+2 {
		t.Errorf("want 1 session + 1 note retained, got %d", retained.Load()-before)
	}
	raw, _ := os.ReadFile(filepath.Join(pathsOf(home).TelemetryDir(), "events-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if !strings.Contains(string(raw), `"event":"job"`) || !strings.Contains(string(raw), `"name":"tick"`) {
		t.Errorf("telemetry: %s", raw)
	}
}

func TestJobsTickSkipsWhenAnotherRuns(t *testing.T) {
	home := devInstall(t, true)
	l, err := state.TryAcquire(filepath.Join(pathsOf(home).LocksDir(), "job-tick.lock"), "jobs tick")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "another tick") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestJobsTickSkipsDuringMaintenance(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	_ = os.MkdirAll(filepath.Dir(d.MaintenanceLock()), 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":1}`), 0o600)
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "maintenance") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestJobsTickServerDownIsPartial(t *testing.T) {
	home := devInstall(t, true) // closed port
	_, _ = notesOutbox(pathsOf(home)).Append(newTestNote())
	if code, out, _ := run(t, "jobs", "tick"); code != output.Partial || !strings.Contains(out, "queued 1") {
		t.Errorf("exit %d\n%s", code, out)
	}
}
