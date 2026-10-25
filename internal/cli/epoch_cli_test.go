package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

// retain flush reconciles before sending: an unreadable epoch fails the run
// and sends nothing; a readable higher one is reported and the session goes.
func TestRetainFlushHoldsWhileTheEpochIsUnreadable(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // retain_since 08:00
	writeTranscript(t, home, "live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	epochStatus.Store(500)
	before := retained.Load()
	if code, out, errOut := run(t, "retain", "flush"); code == output.OK || !strings.Contains(out+errOut, "restore epoch not reconciled") || retained.Load() != before {
		t.Fatalf("held flush: exit %d, %d retained\n%s\n%s", code, retained.Load()-before, out, errOut)
	}
	epochStatus.Store(0)
	epochDoc = `{"epoch":"2","restored_at":"2026-10-24T09:50:00Z","backup_created_at":"2026-10-24T03:00:00Z"}`
	if code, out, errOut := run(t, "retain", "flush"); code != output.OK || !strings.Contains(out, "restore epoch 0 -> 2") || retained.Load() != before+1 {
		t.Fatalf("exit %d, %d retained\n%s\n%s", code, retained.Load()-before, out, errOut)
	}
}

// retain backfill reconciles before sending, and holds when it cannot;
// --plan and --queue send nothing, so they do not need the epoch.
func TestRetainBackfillHoldsWhileTheEpochIsUnreadable(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), true)
	epochStatus.Store(500)
	before := retained.Load()
	if code, out, errOut := run(t, "retain", "backfill"); code != output.Partial || !strings.Contains(out+errOut, "restore epoch not reconciled") || retained.Load() != before {
		t.Fatalf("held backfill: exit %d, %d retained\n%s\n%s", code, retained.Load()-before, out, errOut)
	}
	if code, out, errOut := run(t, "retain", "backfill", "--plan"); code != output.OK || retained.Load() != before {
		t.Errorf("--plan: exit %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := run(t, "retain", "backfill", "--queue"); code != output.OK || retained.Load() != before {
		t.Errorf("--queue: exit %d\n%s\n%s", code, out, errOut)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.Backfill == nil || len(st.Backfill.Sessions) != 1 {
		t.Errorf("--queue did not queue: %+v", st.Backfill)
	}
}

// queueBackfill puts sessions in a running backfill queue.
func queueBackfill(t *testing.T, home string, sids ...string) []string {
	t.Helper()
	var queue []string
	for i, sid := range sids {
		oldSession(t, home, sid, time.Date(2026, 9, 20+i, 9, 0, 0, 0, time.UTC), false)
		queue = append(queue, filepath.Join(home, ".claude", "projects", "-home-u-billing", sid+".jsonl"))
	}
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.Backfill = &state.Backfill{Total: len(queue), MaxCost: 10, Currency: "USD", StartDay: time.Now().UTC().Format("2006-01-02"), Sessions: queue}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return queue
}

type tickEnvelope struct {
	OK   bool       `json:"ok"`
	Data tickResult `json:"data"`
}

// A held tick runs no backfill: the queue is left as it was.
func TestJobsTickHoldsTheBackfillWhileTheEpochIsUnreadable(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	queue := queueBackfill(t, home, "x1")
	epochStatus.Store(500)
	code, out, _ := run(t, "jobs", "tick", "--json")
	var env tickEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if code == output.OK || !strings.HasPrefix(env.Data.Epoch, "held: ") || env.Data.Backfill != "" || retained.Load() != 0 {
		t.Fatalf("exit %d, %d retained\n%s", code, retained.Load(), out)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.Backfill == nil || !reflect.DeepEqual(st.Backfill.Sessions, queue) || st.Backfill.Done != 0 || st.Backfill.Paused != "" {
		t.Errorf("queue changed: %+v", st.Backfill)
	}
}

// writeDeadMaintenanceLock leaves the maintenance lock of a restore killed
// half way: its owner is gone, so it no longer stops hooks and jobs.
func writeDeadMaintenanceLock(t *testing.T, home string) {
	t.Helper()
	p, err := os.StartProcess("/bin/true", []string{"true"}, &os.ProcAttr{})
	if err != nil {
		t.Skip("no /bin/true:", err)
	}
	_, _ = p.Wait()
	d := pathsOf(home)
	_ = os.MkdirAll(d.State, 0o700)
	if err := os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(p.Pid)+`,"command":"restore","since":"2026-10-24T09:55:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// While restore-incomplete.json exists, a tick sends no session and runs no
// backfill, although the restore's maintenance lock has a dead owner; once
// it is gone, the tick sends.
func TestJobsTickHoldsWhileARestoreIsIncomplete(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick")
	writeTranscript(t, home, "live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	queue := queueBackfill(t, home, "x1")
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	writeDeadMaintenanceLock(t, home)
	d := pathsOf(home)
	if err := state.WriteRestoreMarker(d, state.RestoreMarker{Command: "restore", Safety: "/b/safety.tar.gz.age", Archive: "/b/a.tar.gz.age"}); err != nil {
		t.Fatal(err)
	}
	before := retained.Load()
	code, out, _ := run(t, "jobs", "tick", "--json")
	var env tickEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if code == output.OK || !strings.Contains(env.Data.Epoch, "restore is in progress") || env.Data.Backfill != "" || retained.Load() != before {
		t.Fatalf("exit %d, %d retained\n%s", code, retained.Load()-before, out)
	}
	if st, _ := state.Load(d.StateFile()); st.Backfill == nil || !reflect.DeepEqual(st.Backfill.Sessions, queue) {
		t.Errorf("queue changed: %+v", st.Backfill)
	}
	if code, out, errOut := run(t, "retain", "flush"); code == output.OK || retained.Load() != before {
		t.Errorf("retain flush sent while the marker exists: exit %d\n%s\n%s", code, out, errOut)
	}
	_ = state.ClearRestoreMarker(d)
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || retained.Load() == before {
		t.Errorf("after the marker: exit %d, %d retained\n%s", code, retained.Load()-before, out)
	}
}

// While update-incomplete.json exists (an update killed half way, or one
// whose rollback failed), sessions and the backfill hold as they do for a
// restore: the shim may run a binary whose state is not its own.
func TestJobsTickHoldsWhileAnUpdateIsIncomplete(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick")
	writeTranscript(t, home, "live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	queue := queueBackfill(t, home, "x1")
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	d := pathsOf(home)
	if err := state.WriteUpdateMarker(d, state.UpdateMarker{From: "0.1.0", To: "0.2.0", Previous: "/b/0.1.0/backsight", Binary: "/b/0.2.0/backsight"}); err != nil {
		t.Fatal(err)
	}
	before := retained.Load()
	code, out, _ := run(t, "jobs", "tick", "--json")
	var env tickEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if code == output.OK || !strings.Contains(env.Data.Epoch, "update is in progress") || env.Data.Backfill != "" || retained.Load() != before {
		t.Fatalf("exit %d, %d retained\n%s", code, retained.Load()-before, out)
	}
	if st, _ := state.Load(d.StateFile()); st.Backfill == nil || !reflect.DeepEqual(st.Backfill.Sessions, queue) {
		t.Errorf("queue changed: %+v", st.Backfill)
	}
	if code, out, errOut := run(t, "retain", "flush"); code == output.OK || retained.Load() != before {
		t.Errorf("retain flush sent while the marker exists: exit %d\n%s\n%s", code, out, errOut)
	}
	_ = state.ClearUpdateMarker(d)
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || retained.Load() == before {
		t.Errorf("after the marker: exit %d, %d retained\n%s", code, retained.Load()-before, out)
	}
}
