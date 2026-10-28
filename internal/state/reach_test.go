package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

// Reach remembers the first failure of a run of failures and forgets it on
// the next answer; an answer within a minute of the last one writes nothing.
func TestRecordReachSetsAndClearsFailingSince(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	if r := LoadReach(d); !r.LastOK.IsZero() || !r.FailingSince.IsZero() || r.SessionsWaiting != 0 {
		t.Fatalf("absent file: %+v", r)
	}
	t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	RecordReach(d, true, t0)
	RecordReach(d, false, t0.Add(10*time.Minute))
	RecordReach(d, false, t0.Add(20*time.Minute))
	if r := LoadReach(d); !r.FailingSince.Equal(t0.Add(10*time.Minute)) || !r.LastOK.Equal(t0) {
		t.Fatalf("after two failures: %+v", r)
	}
	RecordReach(d, true, t0.Add(30*time.Minute))
	if r := LoadReach(d); !r.FailingSince.IsZero() || !r.LastOK.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("after an answer: %+v", r)
	}
	p := filepath.Join(d.State, "reach.json")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	RecordReach(d, true, t0.Add(30*time.Minute+30*time.Second))
	if after, _ := os.Stat(p); !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("an answer within a minute rewrote the file (%s -> %s)", before.ModTime(), after.ModTime())
	}
	RecordReach(d, true, t0.Add(32*time.Minute))
	if r := LoadReach(d); !r.LastOK.Equal(t0.Add(32 * time.Minute)) {
		t.Errorf("an answer a minute later is not recorded: %+v", r)
	}
	RecordWaiting(d, 3)
	if r := LoadReach(d); r.SessionsWaiting != 3 || !r.LastOK.Equal(t0.Add(32*time.Minute)) {
		t.Errorf("waiting: %+v", r)
	}
}

// An unreadable reach.json reads as nothing known, never an error.
func TestLoadReachUnreadableIsZero(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	_ = os.MkdirAll(d.State, 0o700)
	_ = os.WriteFile(filepath.Join(d.State, "reach.json"), []byte("{not json"), 0o600)
	if r := LoadReach(d); !r.LastOK.IsZero() || !r.FailingSince.IsZero() {
		t.Errorf("unreadable: %+v", r)
	}
}

// The breaker keeps the file and JSON the hooks always wrote.
func TestLoadBreakerReadsBreakerJSON(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	_ = os.MkdirAll(d.State, 0o700)
	_ = os.WriteFile(filepath.Join(d.State, "breaker.json"), []byte(`{"failures":2,"open_until":"2026-10-28T10:00:00Z"}`), 0o600)
	if b := LoadBreaker(d); b.Failures != 2 || !b.OpenUntil.Equal(time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("breaker %+v", b)
	}
}
