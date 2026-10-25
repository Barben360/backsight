package retain

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// snapshot is what a backup of the fake server holds, the operations it
// has seen included (they live in the same database).
type snapshot struct {
	docs    map[string]map[string]string
	content map[string]string
	ops     map[string]bool
}

func (h *fakeHS) backup() snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := snapshot{docs: map[string]map[string]string{}, content: map[string]string{}, ops: map[string]bool{}}
	for k, v := range h.seenOps {
		s.ops[k] = v
	}
	for k, v := range h.docs {
		s.docs[k] = v
	}
	for k, v := range h.content {
		s.content[k] = v
	}
	return s
}

// restore puts a backup back and writes the epoch the way `restore` does
// (decision 0044).
func (h *fakeHS) restore(s snapshot, epoch int, backupAt, restoredAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.docs, h.content, h.seenOps = s.docs, s.content, s.ops
	h.docs[EpochDocument] = map[string]string{"epoch": strconv.Itoa(epoch),
		"backup_created_at": backupAt.UTC().Format(time.RFC3339), "restored_at": restoredAt.UTC().Format(time.RFC3339)}
}

func clockAt(hour, minute int) time.Time {
	return time.Date(2026, 10, 24, hour, minute, 0, 0, time.UTC)
}

// restoredEnv: s1 and s3 go out at 12:00, the backup is taken at 12:30,
// then s1's new turn and the new session s2 go out at 13:00; the server
// is restored from that backup at 13:30 (epoch 1).
func restoredEnv(t *testing.T) *env {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "Why does the export fail?")
	e.record("s1", "a1", "assistant", 1, "The page size was 1000.")
	e.record("s3", "v1", "user", 10, "kept by the backup")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	expect(t, e.flush("s3", Options{}), Sent, 1, 1)
	snap := e.hs.backup()
	e.clock = clockAt(13, 0)
	e.record("s1", "u2", "user", 60, "sent after the backup")
	e.record("s2", "w1", "user", 30, "a session first sent after the backup")
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	expect(t, e.flush("s2", Options{}), Sent, 1, 1)
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	e.clock = clockAt(13, 30)
	return e
}

// After a restore, what went out after the backup was taken is sent again;
// what the backup holds is not.
func TestRestoreResendsWhatTheBackupLacks(t *testing.T) {
	e := restoredEnv(t)
	sent := len(e.hs.retains)
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || rec.From != 0 || rec.To != 1 || !reflect.DeepEqual(rec.Resend, []string{"s1", "s2"}) {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	expect(t, e.flush("s2", Options{}), Sent, 1, 1)
	expect(t, e.flush("s3", Options{}), Nothing, 0, 0)
	again := e.hs.retains[sent:]
	if len(again) != 2 || !strings.Contains(again[0].Content, "sent after the backup") || strings.Contains(again[0].Content, "page size") ||
		again[0].UpdateMode != "append" || !strings.Contains(again[1].Content, "first sent after the backup") {
		t.Errorf("resent: %+v", again)
	}
	if !strings.Contains(e.hs.content["conversation:s1"], "page size") || !strings.Contains(e.hs.content["conversation:s1"], "sent after the backup") {
		t.Errorf("s1 on the server: %q", e.hs.content["conversation:s1"])
	}
	if st, _ := state.Load(e.f.Dirs.StateFile()); st.EpochOf("claude-memory") != 1 {
		t.Errorf("epoch not recorded: %+v", st.RestoreEpoch)
	}
	gets := e.hs.gets
	if rec, err := e.f.Reconcile(context.Background()); err != nil || rec.From != 1 || len(rec.Resend) != 0 || e.hs.gets != gets+1 {
		t.Errorf("a reconciled client reconciles again: %+v %v (%d reads)", rec, err, e.hs.gets-gets)
	}
}

// An epoch that cannot be read, or a reconcile that cannot run now, drops
// nothing and holds the flushes; the next run reconciles. A session being
// flushed is TestReconcileStopsAtASessionBeingFlushed.
func TestUnreadableEpochDropsNothing(t *testing.T) {
	for name, breakIt := range map[string]func(e *env){
		"server error": func(e *env) { e.hs.epochStatus = http.StatusInternalServerError },
		"server down": func(e *env) {
			e.f.Client, _ = hindsight.New("http://127.0.0.1:1", "k", time.Second)
		},
		"epoch not a number": func(e *env) { e.hs.docs[EpochDocument]["epoch"] = "two" },
		"backup time unreadable": func(e *env) {
			e.hs.docs[EpochDocument]["backup_created_at"] = "yesterday"
		},
		"maintenance": func(e *env) {
			_ = os.MkdirAll(filepath.Dir(e.f.Dirs.MaintenanceLock()), 0o700)
			_ = os.WriteFile(e.f.Dirs.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"command":"restore"}`), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := restoredEnv(t)
			breakIt(e)
			before, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
			sent := len(e.hs.retains)
			if rec, err := e.f.Reconcile(context.Background()); err == nil || e.f.Hold == "" {
				t.Fatalf("reconciled: %+v, hold %q", rec, e.f.Hold)
			}
			if cur, ok, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1"); !ok || cur != before {
				t.Errorf("cursor dropped or changed: %+v", cur)
			}
			if idx, _ := LoadSentIndex(e.f.Dirs.SentIndexDir()); !idx.Has("u2") || !idx.Has(sessionKey("claude-memory", "s2", "w1")) {
				t.Error("sent-index entries dropped")
			}
			if st, _ := state.Load(e.f.Dirs.StateFile()); st.RestoreEpoch != nil {
				t.Errorf("epoch recorded: %+v", st.RestoreEpoch)
			}
			e.record("s3", "v2", "user", 70, "a new turn")
			if r := e.flush("s3", Options{}); r.Status == Sent || len(e.hs.retains) != sent {
				t.Errorf("a held flusher sent: %+v", r)
			}
		})
	}
	e := restoredEnv(t)
	e.hs.epochStatus = http.StatusServiceUnavailable
	if _, err := e.f.Reconcile(context.Background()); err == nil {
		t.Fatal("reconciled")
	}
	e.hs.epochStatus = 0
	if rec, err := e.f.Reconcile(context.Background()); err != nil || e.f.Hold != "" || len(rec.Resend) != 2 {
		t.Fatalf("next run: %+v %v hold %q", rec, err, e.f.Hold)
	}
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
}

// No epoch document is epoch 0, which every install before M5 is on: an
// existing install keeps its cursors. A lower epoch than the one recorded
// (another server behind the same bank id) is taken without dropping.
func TestNoEpochDocumentIsEpochZero(t *testing.T) {
	e := restoredEnv(t)
	delete(e.hs.docs, EpochDocument)
	gets := e.hs.gets
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || rec.From != 0 || rec.To != 0 || len(rec.Resend) != 0 || e.hs.gets != gets+1 {
		t.Fatalf("%+v %v (%d reads)", rec, err, e.hs.gets-gets)
	}
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)

	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: "claude-memory", Epoch: 3}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec, err := e.f.Reconcile(context.Background()); err != nil || rec.From != 3 || rec.To != 0 || len(rec.Resend) != 0 {
		t.Errorf("lower epoch: %+v %v", rec, err)
	}
	if _, ok, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s2"); !ok {
		t.Error("a lower epoch dropped a cursor")
	}
	if st, _ := state.Load(e.f.Dirs.StateFile()); st.EpochOf("claude-memory") != 0 || st.RestoreEpoch == nil {
		t.Errorf("lower epoch not recorded: %+v", st.RestoreEpoch)
	}
}

// A session backfill sent after the backup goes back in the backfill
// queue: live retain leaves sessions older than retain_since to backfill.
func TestRestoreRequeuesBackfilledSessions(t *testing.T) {
	e := newEnv(t)
	e.f.RetainSince = clockAt(10, 0)
	e.record("old", "o1", "user", 0, "09:00, before live retain")
	snap := e.hs.backup()
	e.clock = clockAt(13, 0)
	expect(t, e.flush("old", Options{Force: true, IgnoreSince: true}), Sent, 1, 1) // backfill's flush
	path := filepath.Join(e.dir, "old.jsonl")
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.Backfill = &state.Backfill{Done: 1, Total: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	if rec, err := e.f.Reconcile(context.Background()); err != nil || !reflect.DeepEqual(rec.Resend, []string{"old"}) {
		t.Fatalf("%+v %v", rec, err)
	}
	st, _ := state.Load(e.f.Dirs.StateFile())
	if b := st.Backfill; b == nil || !reflect.DeepEqual(b.Sessions, []string{path}) || b.Total != 2 {
		t.Fatalf("backfill queue: %+v", b)
	}
	expect(t, e.flush("old", Options{Force: true, IgnoreSince: true}), Sent, 1, 1)
}

// A session older than retain_since that the user sent by hand (`retain
// flush --session`) after the backup, with no backfill queue to take it
// back, is sent again by the next live flush: nothing else would send it,
// and decision 0044 resends what the restore lost.
func TestRestoreResendsAnOldSessionWithoutBackfillQueue(t *testing.T) {
	e := newEnv(t)
	e.f.RetainSince = clockAt(10, 0)
	e.record("old", "o1", "user", 0, "09:00, before live retain")
	e.record("old", "o2", "assistant", 1, "an answer")
	snap := e.hs.backup()
	e.clock = clockAt(13, 0)
	expect(t, e.flush("old", Options{Force: true, IgnoreSince: true}), Sent, 2, 1) // retain flush --session old
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	e.clock = clockAt(13, 30)
	if rec, err := e.f.Reconcile(context.Background()); err != nil || !reflect.DeepEqual(rec.Resend, []string{"old"}) {
		t.Fatalf("%+v %v", rec, err)
	}
	if st, _ := state.Load(e.f.Dirs.StateFile()); st.Backfill != nil {
		t.Fatalf("a backfill queue was started: %+v", st.Backfill)
	}
	sent := len(e.hs.retains)
	expect(t, e.flush("old", Options{}), Sent, 2, 1) // the next jobs tick
	if len(e.hs.retains) != sent+1 || !strings.Contains(e.hs.content["conversation:old"], "before live retain") {
		t.Errorf("not resent whole: %+v", e.hs.retains[sent:])
	}
	expect(t, e.flush("old", Options{}), Nothing, 0, 0)
	if len(e.hs.retains) != sent+1 {
		t.Errorf("sent twice: %d", len(e.hs.retains)-sent)
	}
}
