package retain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/testtool"
)

// history encodes epoch/backup pairs as the epoch document stores them.
func history(t *testing.T, pairs ...any) string {
	t.Helper()
	var out []EpochEntry
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, EpochEntry{Epoch: pairs[i].(int), BackupCreatedAt: pairs[i+1].(time.Time)})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func cursorOf(t *testing.T, e *env, sid string) (Cursor, bool) {
	t.Helper()
	c, ok, err := LoadCursor(e.f.Dirs.CursorsDir(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return c, ok
}

// Only cursors sent after the backup (minus the clock skew allowance) are
// compared with the server: a session sent at 10:00 whose document went
// missing for another reason is left alone; one sent at 12:00, inside the
// skew hour before a 12:30 backup, is re-sent.
func TestReconcileTakesOnlySessionsSentAfterTheBackup(t *testing.T) {
	e := newEnv(t)
	e.clock = clockAt(10, 0)
	e.record("early", "x1", "user", 0, "sent at 10:00")
	expect(t, e.flush("early", Options{}), Sent, 1, 1)
	e.clock = clockAt(12, 0)
	e.record("skew", "k1", "user", 10, "sent at 12:00")
	expect(t, e.flush("skew", Options{}), Sent, 1, 1)
	snap := e.hs.backup()
	delete(snap.docs, "conversation:early")
	delete(snap.docs, "conversation:skew")
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	e.clock = clockAt(13, 30)
	before, _ := cursorOf(t, e, "early")
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || !reflect.DeepEqual(rec.Resend, []string{"skew"}) {
		t.Fatalf("%+v %v", rec, err)
	}
	if c, ok := cursorOf(t, e, "early"); !ok || !reflect.DeepEqual(c, before) {
		t.Errorf("the 10:00 cursor changed: %+v", c)
	}
	expect(t, e.flush("early", Options{}), Nothing, 0, 0)
	expect(t, e.flush("skew", Options{}), Sent, 1, 1)
}

// A session locked by another flusher stops the reconcile: that session
// keeps its cursor and its index entries, the epoch is not recorded and
// nothing is sent; the next run finishes, and every lost session goes out
// once, whichever had been dropped before the stop.
func TestReconcileStopsAtASessionBeingFlushed(t *testing.T) {
	e := restoredEnv(t)
	l, err := state.TryAcquire(filepath.Join(e.f.Dirs.LocksDir(), "session-s2.lock"), "other flusher")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := cursorOf(t, e, "s2")
	sent := len(e.hs.retains)
	if _, err := e.f.Reconcile(context.Background()); err == nil || e.f.Hold == "" {
		t.Fatalf("reconciled while s2 is locked: hold %q", e.f.Hold)
	}
	if c, ok := cursorOf(t, e, "s2"); !ok || !reflect.DeepEqual(c, before) {
		t.Errorf("s2's cursor dropped or changed: %+v", c)
	}
	if idx, _ := LoadSentIndex(e.f.Dirs.SentIndexDir()); !idx.Has(sessionKey("claude-memory", "s2", "w1")) {
		t.Error("s2's index entry dropped")
	}
	if st, _ := state.Load(e.f.Dirs.StateFile()); st.RestoreEpoch != nil {
		t.Errorf("epoch recorded: %+v", st.RestoreEpoch)
	}
	e.record("s3", "v2", "user", 70, "a new turn")
	if r := e.flush("s3", Options{}); r.Status == Sent || len(e.hs.retains) != sent {
		t.Errorf("a held flusher sent: %+v", r)
	}
	_ = l.Release()
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || e.f.Hold != "" || !slicesContain(rec.Resend, "s2") {
		t.Fatalf("next run: %+v %v hold %q", rec, err, e.f.Hold)
	}
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	expect(t, e.flush("s2", Options{}), Sent, 1, 1)
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)
	expect(t, e.flush("s2", Options{}), Nothing, 0, 0)
}

func slicesContain(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// A cursor of another bank is not this bank's restore: it is left alone.
func TestReconcileSkipsAnotherBanksCursor(t *testing.T) {
	e := restoredEnv(t)
	c, _ := cursorOf(t, e, "s2")
	c.Bank = "other-bank"
	if err := SaveCursor(e.f.Dirs.CursorsDir(), "s2", c); err != nil {
		t.Fatal(err)
	}
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || !reflect.DeepEqual(rec.Resend, []string{"s1"}) {
		t.Fatalf("%+v %v", rec, err)
	}
	if got, ok := cursorOf(t, e, "s2"); !ok || !reflect.DeepEqual(got, c) {
		t.Errorf("the other bank's cursor changed: %+v", got)
	}
}

// A server last_uuid the transcript lacks leaves the sent index as it is
// (the resync quarantines); a transcript gone keeps its cursor: there is
// nothing to resend from.
func TestReconcileLeavesWhatItCannotPlace(t *testing.T) {
	e := restoredEnv(t)
	e.hs.docs["conversation:s1"] = map[string]string{"segment": "1", "records": "2", "last_uuid": "not-in-the-transcript"}
	if err := os.Remove(filepath.Join(e.dir, "s2.jsonl")); err != nil {
		t.Fatal(err)
	}
	s2, _ := cursorOf(t, e, "s2")
	if _, err := e.f.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	idx, _ := LoadSentIndex(e.f.Dirs.SentIndexDir())
	for _, k := range []string{"u1", "a1", "u2", sessionKey("claude-memory", "s1", "u2"), sessionKey("claude-memory", "s2", "w1")} {
		if !idx.Has(k) {
			t.Errorf("index entry %q dropped", k)
		}
	}
	if c, ok := cursorOf(t, e, "s2"); !ok || !reflect.DeepEqual(c, s2) {
		t.Errorf("s2's cursor (transcript gone) dropped: %+v", c)
	}
	if r := e.flush("s1", Options{}); r.Status != Quarantined {
		t.Errorf("s1: %+v", r)
	}
}

// forkEnv: a parent and its fork (which copies the parent's two records)
// both go out at 13:00, after the 12:30 backup; the restore loses both.
func forkEnv(t *testing.T) *env {
	e := newEnv(t)
	snap := e.hs.backup()
	e.clock = clockAt(13, 0)
	e.record("parent", "p1", "user", 0, "the parent's question")
	e.record("parent", "p2", "assistant", 1, "the parent's answer")
	expect(t, e.flush("parent", Options{}), Sent, 2, 1)
	e.recordIn(e.cwd, "fork", "p1", "user", 0, "the parent's question")
	e.recordIn(e.cwd, "fork", "p2", "assistant", 1, "the parent's answer")
	e.record("fork", "f1", "user", 30, "the fork's own question")
	expect(t, e.flush("fork", Options{}), Sent, 1, 1)
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	e.clock = clockAt(13, 30)
	return e
}

// After a restore that lost a parent and its fork, each is rebuilt with
// its own records only, whichever flushes first: the fork never takes the
// parent's records, the parent is never skipped as a copy.
func TestRestoreRebuildsAParentAndItsForkInEitherOrder(t *testing.T) {
	for _, order := range [][]string{{"fork", "parent"}, {"parent", "fork"}} {
		t.Run(strings.Join(order, " then "), func(t *testing.T) {
			e := forkEnv(t)
			if _, err := e.f.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if idx, _ := LoadSentIndex(e.f.Dirs.SentIndexDir()); !idx.Has("p1") || !idx.Has("p2") {
				t.Error("the fork guard lost the parent's records")
			}
			want := map[string]int{"parent": 2, "fork": 1}
			for _, sid := range order {
				expect(t, e.flush(sid, Options{}), Sent, want[sid], 1)
			}
			if c := e.hs.content["conversation:fork"]; strings.Contains(c, "parent's") || !strings.Contains(c, "fork's own") {
				t.Errorf("fork on the server: %q", c)
			}
			if c := e.hs.content["conversation:parent"]; !strings.Contains(c, "parent's question") || !strings.Contains(c, "parent's answer") {
				t.Errorf("parent on the server: %q", c)
			}
			for _, sid := range order {
				expect(t, e.flush(sid, Options{}), Nothing, 0, 0)
			}
		})
	}
}

// A client two restores behind reconciles against the earliest backup of
// the epochs above its own (the epoch history), not only the last one.
func TestReconcileTwoEpochsBehindUsesTheEarliestBackup(t *testing.T) {
	e := newEnv(t)
	snap := e.hs.backup()
	e.clock = clockAt(11, 0)
	e.record("a", "a1", "user", 0, "sent at 11:00, lost by the first restore")
	expect(t, e.flush("a", Options{}), Sent, 1, 1)
	e.clock = clockAt(13, 0)
	e.record("b", "b1", "user", 30, "sent at 13:00, lost by the second restore")
	expect(t, e.flush("b", Options{}), Sent, 1, 1)
	e.hs.restore(snap, 2, clockAt(12, 30), clockAt(13, 30))
	e.hs.docs[EpochDocument]["history"] = history(t, 1, clockAt(10, 30), 2, clockAt(12, 30))
	e.clock = clockAt(13, 30)
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || rec.To != 2 || !reflect.DeepEqual(rec.Resend, []string{"a", "b"}) {
		t.Fatalf("%+v %v", rec, err)
	}
	expect(t, e.flush("a", Options{}), Sent, 1, 1)
	expect(t, e.flush("b", Options{}), Sent, 1, 1)
}

// A client older than the history (it holds the last 10 epochs, all above
// the client's) compares every cursor sent after its own reconcile.
func TestReconcileOlderThanTheHistoryUsesItsOwnReconcileTime(t *testing.T) {
	e := newEnv(t)
	snap := e.hs.backup()
	e.clock = clockAt(9, 30)
	e.record("before", "o1", "user", 0, "sent at 09:30, before this client's last reconcile")
	expect(t, e.flush("before", Options{}), Sent, 1, 1)
	e.clock = clockAt(10, 30)
	e.record("after", "n1", "user", 5, "sent at 10:30, after it")
	expect(t, e.flush("after", Options{}), Sent, 1, 1)
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: "claude-memory", Epoch: 1, ReconciledAt: clockAt(10, 0)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var pairs []any
	for i := 0; i < 10; i++ { // epochs 3..12, backups from 12:00 on
		pairs = append(pairs, 3+i, clockAt(12, 0).Add(time.Duration(i)*time.Minute))
	}
	e.hs.restore(snap, 12, clockAt(12, 9), clockAt(13, 30))
	e.hs.docs[EpochDocument]["history"] = history(t, pairs...)
	e.clock = clockAt(13, 30)
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || rec.From != 1 || rec.To != 12 || !reflect.DeepEqual(rec.Resend, []string{"after"}) {
		t.Fatalf("%+v %v", rec, err)
	}
	expect(t, e.flush("after", Options{}), Sent, 1, 1)
	expect(t, e.flush("before", Options{}), Nothing, 0, 0)
}

// While restore-incomplete.json exists, nothing is sent and nothing is
// reconciled, even when the maintenance lock's owner is gone (a restore
// killed half way): the server may be the old one, the restored one or
// none.
func TestARestoreMarkerHoldsEverySend(t *testing.T) {
	testtool.SkipWindows(t, "Task 3b: state.processAlive treats an exited process as alive on Windows (a dead pid is not reliably dead)")
	e := restoredEnv(t)
	_ = os.WriteFile(e.f.Dirs.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(deadPID(t))+`,"command":"restore"}`), 0o600)
	if err := state.WriteRestoreMarker(e.f.Dirs, state.RestoreMarker{Command: "restore", Safety: "/b/safety.tar.gz.age", Archive: "/b/a.tar.gz.age", StartedAt: clockAt(13, 20)}); err != nil {
		t.Fatal(err)
	}
	sent := len(e.hs.retains)
	before, _ := cursorOf(t, e, "s1")
	e.record("s3", "v2", "user", 70, "a new turn")
	if r := e.flush("s3", Options{}); r.Status != Retry || !r.Stop || len(e.hs.retains) != sent {
		t.Errorf("flushed without reconcile while the marker exists: %+v", r)
	}
	if _, err := e.f.Reconcile(context.Background()); err == nil || !strings.Contains(e.f.Hold, "restore") {
		t.Fatalf("reconciled while the marker exists: hold %q", e.f.Hold)
	}
	if c, ok := cursorOf(t, e, "s1"); !ok || !reflect.DeepEqual(c, before) {
		t.Errorf("cursor dropped while held: %+v", c)
	}
	if r := e.flush("s3", Options{}); r.Status == Sent || len(e.hs.retains) != sent {
		t.Errorf("a held flusher sent: %+v", r)
	}
	_ = state.ClearRestoreMarker(e.f.Dirs)
	if rec, err := e.f.Reconcile(context.Background()); err != nil || len(rec.Resend) != 2 {
		t.Fatalf("after the marker: %+v %v", rec, err)
	}
	expect(t, e.flush("s3", Options{}), Sent, 1, 2)
}

// deadPID is the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	p, err := os.StartProcess("/bin/true", []string{"true"}, &os.ProcAttr{})
	if err != nil {
		t.Skip("no /bin/true:", err)
	}
	_, _ = p.Wait()
	return p.Pid
}

// A restored server's queue lags its documents' metadata: reconcile waits
// until the bank has no queued operation, dropping nothing meanwhile.
func TestReconcileWaitsForTheBanksQueue(t *testing.T) {
	e := restoredEnv(t)
	e.hs.queued = 3
	before, _ := cursorOf(t, e, "s1")
	if _, err := e.f.Reconcile(context.Background()); err == nil || !strings.Contains(e.f.Hold, "queued") {
		t.Fatalf("reconciled with a queue: hold %q", e.f.Hold)
	}
	if c, ok := cursorOf(t, e, "s1"); !ok || !reflect.DeepEqual(c, before) {
		t.Errorf("cursor dropped while waiting: %+v", c)
	}
	e.hs.queued = 0
	if rec, err := e.f.Reconcile(context.Background()); err != nil || len(rec.Resend) != 2 {
		t.Fatalf("queue empty: %+v %v", rec, err)
	}
}

// A backfilled session goes back in the backfill queue before its cursor
// goes: when the queue cannot be written, the cursor stays, and the next
// run queues it.
func TestRestoreQueuesABackfillSessionBeforeDroppingItsCursor(t *testing.T) {
	e := newEnv(t)
	e.f.RetainSince = clockAt(10, 0)
	e.record("old", "o1", "user", 0, "09:00, before live retain")
	snap := e.hs.backup()
	e.clock = clockAt(13, 0)
	expect(t, e.flush("old", Options{Force: true, IgnoreSince: true}), Sent, 1, 1)
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.Backfill = &state.Backfill{Done: 1, Total: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	l, err := state.TryAcquire(filepath.Join(e.f.Dirs.LocksDir(), "state.lock"), "another writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.f.Reconcile(context.Background()); err == nil {
		t.Fatal("reconciled without writing the queue")
	}
	if _, ok := cursorOf(t, e, "old"); !ok {
		t.Fatal("cursor dropped before the session was queued")
	}
	_ = l.Release()
	if _, err := e.f.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(e.f.Dirs.StateFile())
	if b := st.Backfill; b == nil || !reflect.DeepEqual(b.Sessions, []string{filepath.Join(e.dir, "old.jsonl")}) || b.Total != 2 {
		t.Fatalf("backfill queue: %+v", b)
	}
	if _, ok := cursorOf(t, e, "old"); ok {
		t.Error("cursor kept once queued")
	}
}
