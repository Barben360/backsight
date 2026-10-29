package retain

import (
	"context"
	"errors"
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
	gets := e.hs.gets // the purge tombstones, then the epoch: no session's document
	if rec, err := e.f.Reconcile(context.Background()); err != nil || rec.From != 1 || len(rec.Resend) != 0 || e.hs.gets != gets+2 {
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
			if cur, ok, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1"); !ok || !reflect.DeepEqual(cur, before) {
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
	gets := e.hs.gets // the purge tombstones, then the epoch
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || rec.From != 0 || rec.To != 0 || len(rec.Resend) != 0 || e.hs.gets != gets+2 {
		t.Fatalf("%+v %v (%d reads)", rec, err, e.hs.gets-gets)
	}
	// The cursor is kept: no resync. The server lost u2 with no epoch to
	// say so; s1's segment 1, not confirmed yet, finds it missing there
	// and sends it again after the server's segment (checkReplaces).
	kept, ok, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	if !ok {
		t.Fatal("s1's cursor dropped")
	}
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	after, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	if after.Offset != kept.Offset || after.LastUUID != kept.LastUUID || after.Fingerprint != kept.Fingerprint {
		t.Errorf("cursor moved: %+v, was %+v", after, kept)
	}
	// A resync would read s1's document again; the kept cursor reads none.
	gets = e.hs.gets
	if r := e.flush("s1", Options{}); r.Status != Nothing || e.hs.gets != gets {
		t.Errorf("%+v, %d reads", r, e.hs.gets-gets)
	}

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

// twoDocEnv: s1 sends alpha's u1 and beta's u2 (two documents) at 12:00,
// the backup is taken at 12:30; lost, if set, then sends alpha's u3 at
// 13:00. The server is restored from the backup at 13:30.
func twoDocEnv(t *testing.T, lost bool) *env {
	e := newEnv(t)
	alpha, beta := e.repo("alpha"), e.repo("beta")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha before the backup")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta before the backup")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	snap := e.hs.backup()
	if lost {
		e.clock = clockAt(13, 0)
		e.recordIn(alpha, "s1", "u3", "user", 60, "alpha after the backup")
		expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	}
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	e.clock = clockAt(13, 30)
	return e
}

// The reconcile compares the cursor with the furthest record any of the
// session's documents holds: here beta's, the latest send the backup kept.
func TestReconcileAcrossTwoDocumentsDropsOnlyWhatFollowsTheFurthest(t *testing.T) {
	e := twoDocEnv(t, true)
	sent := len(e.hs.retains)
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || !reflect.DeepEqual(rec.Resend, []string{"s1"}) {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	if !e.f.Index.Has(sessionKey("claude-memory", "s1", "u2")) || !e.f.Index.Has(sessionKey("claude-memory", "s1", "u1")) {
		t.Error("records the server still holds (beta's u2, alpha's u1) lost their sent mark")
	}
	if e.f.Index.Has(sessionKey("claude-memory", "s1", "u3")) {
		t.Error("u3, lost by the restore, is still marked sent")
	}
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	again := e.hs.retains[sent:]
	if len(again) != 1 || again[0].DocumentID != "conversation:s1" || again[0].UpdateMode != "append" ||
		!strings.Contains(again[0].Content, "alpha after the backup") || strings.Contains(again[0].Content, "before the backup") {
		t.Errorf("resent: %+v", again)
	}
}

// Nothing lost: the cursor's last record is the side document's last_uuid,
// although the home document's differs.
func TestReconcileKeepsASessionWhoseLatestSendIsInASideDocument(t *testing.T) {
	e := twoDocEnv(t, false)
	before, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || len(rec.Resend) != 0 {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	if cur, ok, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1"); !ok || !reflect.DeepEqual(cur, before) {
		t.Errorf("cursor dropped or changed: %+v", cur)
	}
}

// A client bound to server a sends nothing while server b answers at its
// address, and records b for doctor, the banner and store; once a answers
// again it sends and the record goes. A failed read of the id changes
// nothing: a recorded mismatch still holds, none lets the run go on.
func TestFlushHoldsOnServerIDMismatch(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "Why does the export fail?")
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.RemoteServer = &state.RemoteServer{ID: "a", URL: "http://100.64.0.1:8888", ConnectedAt: clockAt(8, 0)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.f.ServerID = "a"
	e.hs.docs[ServerDocument] = map[string]string{"server_id": "b"}
	const text = "a different server answers at this address (server b, joined a): run backsight connect with its token"
	var mm *ServerMismatch
	if _, err := e.f.Reconcile(context.Background()); !errors.As(err, &mm) || e.f.Hold != text {
		t.Fatalf("reconcile: %v, hold %q", err, e.f.Hold)
	}
	if r := e.flush("s1", Options{}); r.Status != Retry || r.Detail != text || len(e.hs.retains) != 0 {
		t.Errorf("held flush: %+v, %d retain(s)", r, len(e.hs.retains))
	}
	raw, _ := os.ReadFile(e.f.Dirs.StateFile())
	st, _ := state.Load(e.f.Dirs.StateFile())
	if !strings.Contains(string(raw), `"mismatch": "b"`) || st.RemoteServer == nil || st.RemoteServer.ID != "a" || !st.RemoteServer.MismatchAt.Equal(clockAt(12, 0)) {
		t.Errorf("state.json:\n%s", raw)
	}

	e.hs.failGet = map[string]bool{ServerDocument: true}
	if _, err := e.f.Reconcile(context.Background()); !errors.As(err, &mm) || e.f.Hold != text {
		t.Errorf("unreadable id with a mismatch recorded: %v, hold %q", err, e.f.Hold)
	}
	if st, _ := state.Load(e.f.Dirs.StateFile()); st.RemoteServer.Mismatch != "b" {
		t.Errorf("an unreadable id changed the record: %+v", st.RemoteServer)
	}

	e.hs.failGet = nil
	e.hs.docs[ServerDocument] = map[string]string{"server_id": "a"}
	if _, err := e.f.Reconcile(context.Background()); err != nil || e.f.Hold != "" {
		t.Fatalf("the joined server answers again: %v, hold %q", err, e.f.Hold)
	}
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	if st, _ := state.Load(e.f.Dirs.StateFile()); st.RemoteServer.Mismatch != "" || !st.RemoteServer.MismatchAt.IsZero() || st.RemoteServer.ID != "a" {
		t.Errorf("mismatch not cleared: %+v", st.RemoteServer)
	}

	e.hs.failGet = map[string]bool{ServerDocument: true}
	if _, err := e.f.Reconcile(context.Background()); err != nil || e.f.Hold != "" {
		t.Errorf("unreadable id, no mismatch recorded: %v, hold %q", err, e.f.Hold)
	}
}

// A restore to a backup taken before this PC's part of a split session
// went out: the reconcile cannot tell this PC's records from the server's
// last_uuid (the other PC's), and resends them rather than lose them
// (decision 0073, "never lose").
func TestRestoreResendsThisPCsPartOfASplitSession(t *testing.T) {
	e := splitSession(t)
	snap := e.hs.backup()
	expect(t, e.flush("s1", Options{}), Sent, 5, 5)
	e.hs.restore(snap, 1, clockAt(11, 0), clockAt(12, 30))
	e.clock = clockAt(12, 30)
	if rec, err := e.f.Reconcile(context.Background()); err != nil || !reflect.DeepEqual(rec.Resend, []string{"s1"}) {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	r := e.flush("s1", Options{})
	if r.Status != Sent || len(e.hs.retains) != 2 {
		t.Fatalf("%+v, %d items", r, len(e.hs.retains))
	}
	for _, u := range []string{"u3", "b1", "b2"} {
		if !strings.Contains(e.hs.content["conversation:s1"], "turn "+u) {
			t.Errorf("%s lost: %q", u, e.hs.content["conversation:s1"])
		}
	}
}

// A connect between the reconcile's first look and its epoch.lock (here
// while the epoch document is read) started this PC's sends over: the
// reconcile drops no cursor, records no epoch and deletes nothing.
func TestReconcileSpanningConnectDropsNothing(t *testing.T) {
	e := restoredEnv(t)
	cursor := filepath.Join(e.f.Dirs.CursorsDir(), "s1.json")
	before, err := os.ReadFile(cursor)
	if err != nil {
		t.Fatal(err)
	}
	stBefore, _ := state.Load(e.f.Dirs.StateFile())
	e.hs.onDocument = func(id string) {
		if id == EpochDocument {
			_ = state.Update(e.f.Dirs, "connect", func(s *state.State) error { s.SendsGeneration++; return nil })
		}
	}
	if _, err := e.f.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "started over") {
		t.Fatalf("reconcile: %v", err)
	}
	if after, _ := os.ReadFile(cursor); string(after) != string(before) {
		t.Errorf("s1's cursor changed:\n%s\nwas\n%s", after, before)
	}
	if st, _ := state.Load(e.f.Dirs.StateFile()); !reflect.DeepEqual(st.RestoreEpoch, stBefore.RestoreEpoch) {
		t.Errorf("restore epoch recorded: %+v, was %+v", st.RestoreEpoch, stBefore.RestoreEpoch)
	}
	if len(e.hs.deleted) != 0 {
		t.Errorf("deleted %v", e.hs.deleted)
	}
}
