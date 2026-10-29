package retain

import (
	"context"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// tick is one jobs tick's flush of sp1 on this PC, a quarter of an hour
// after the last: a Flusher of its own, as every run builds one.
func (e *env) tick() Result {
	e.t.Helper()
	e.clock = e.clock.Add(15 * time.Minute)
	f := *e.f
	f.replaceReads, f.queue = 0, nil
	return f.FlushFile(context.Background(), filepath.Join(e.dir, "sp1.jsonl"), Options{})
}

// replaces is this PC's pending replacing parts of sp1, by project.
func (e *env) replaces() map[string]PendingReplace {
	e.t.Helper()
	return e.sp1Cursor().Replaces
}

// splitOn fails unless this PC recorded sp1 split in exactly docs.
func (e *env) splitOn(docs ...string) {
	e.t.Helper()
	splits, err := SplitSessions(e.f.Dirs.State)
	got := splits["sp1"].Documents
	if len(docs) == 0 && got == nil && err == nil {
		return
	}
	if err != nil || !reflect.DeepEqual(got, docs) {
		e.t.Errorf("split recorded: %v %v, want %v", err, got, docs)
	}
}

const betaDoc = "conversation:sp1@beta"

// betaReplacedByB: sp1 is sent from A (r1), copied to B; then each PC
// starts project beta, A with a2 a3, B with b2, and both send their
// segment 1 of conversation:sp1@beta before the server applies either
// (both read it absent). B's, applied last, replaces A's.
func betaReplacedByB(t *testing.T) (a, b *env) {
	a = newEnv(t)
	a.f.MachineID = "m1"
	beta := a.repo("beta")
	a.turns(0, "r1")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	b = a.otherPC()
	a.copyTo(b)
	expect(t, b.flush("sp1", Options{}), Nothing, 0, 0)
	a.hs.hide = map[string]bool{betaDoc: true}
	a.recordIn(beta, "sp1", "a2", "user", 1, "turn a2")
	a.recordIn(beta, "sp1", "a3", "user", 2, "turn a3")
	expect(t, a.flush("sp1", Options{}), Sent, 2, 1)
	b.recordIn(beta, "sp1", "b2", "user", 1, "turn b2")
	expect(t, b.flush("sp1", Options{}), Sent, 1, 1)
	a.hs.hide = nil
	if n := a.hs.countIn(betaDoc, "turn a2"); n != 0 {
		t.Fatalf("B's segment 1 did not replace A's: a2 stored %d times", n)
	}
	if p, ok := a.replaces()["beta"]; !ok || p.Document != betaDoc {
		t.Fatalf("A's replacing part not recorded: %+v", a.replaces())
	}
	return a, b
}

// The I-1 race on a new side document: A's tick, with no new record,
// sends A's records B's segment 1 replaced as the next segment, appended
// after B's; the split is recorded on A. B's ticks find B's own records
// there and send nothing. Every record of both PCs is stored once.
func TestAFirstSegmentAnotherPCReplacedIsSentAgainByTheTick(t *testing.T) {
	a, b := betaReplacedByB(t)
	sent := len(a.hs.retains)
	expect(t, a.tick(), Sent, 2, 2)
	if len(a.hs.retains) != sent+1 {
		t.Fatalf("%d items sent, want 1", len(a.hs.retains)-sent)
	}
	it := a.hs.retains[sent]
	if it.DocumentID != betaDoc || it.UpdateMode != "append" || it.Metadata["segment"] != "2" || it.Metadata["machine_id"] != "m1" {
		t.Errorf("A's repair: %s %q segment %s machine %s", it.DocumentID, it.UpdateMode, it.Metadata["segment"], it.Metadata["machine_id"])
	}
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
	a.splitOn(betaDoc)
	if _, ok := a.replaces()["beta"]; ok {
		t.Errorf("A's replacing part still pending: %+v", a.replaces())
	}
	if c := a.sp1Cursor(); c.Segments["beta"] != 2 || c.LastUUIDs["beta"] != "a3" {
		t.Errorf("A's cursor: segments %v last %v", c.Segments, c.LastUUIDs)
	}
	sent = len(a.hs.retains)
	for range 2 {
		expect(t, a.tick(), Nothing, 0, 0)
		expect(t, b.tick(), Nothing, 0, 0)
	}
	if len(a.hs.retains) != sent {
		t.Errorf("sent again: %+v", a.hs.retains[sent:])
	}
	if len(a.replaces()) != 0 || len(b.replaces()) != 0 {
		t.Errorf("still pending: A %+v, B %+v", a.replaces(), b.replaces())
	}
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")

	// Both PCs go on appending after the repair, each numbered after the
	// server's: nothing lost, nothing sent twice.
	a.recordIn(a.repo("beta"), "sp1", "a4", "user", 3, "turn a4")
	expect(t, a.tick(), Sent, 1, 3)
	b.recordIn(a.repo("beta"), "sp1", "b3", "user", 4, "turn b3")
	expect(t, b.tick(), Sent, 1, 4)
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "a4", "b2", "b3")
}

// The same race on the home document of a session copied to B before
// either PC sent it, then continued on both: A's own turn a4 goes again,
// the shared r1-r3 do not.
func TestAHomeDocumentAnotherPCReplacedIsSentAgainByTheTick(t *testing.T) {
	a := newEnv(t)
	a.f.MachineID = "m1"
	a.turns(0, "r1", "r2", "r3")
	b := a.otherPC()
	a.copyTo(b)
	a.turns(3, "a4")
	b.turns(3, "b4")
	a.hs.hide = map[string]bool{"conversation:sp1": true}
	expect(t, a.flush("sp1", Options{}), Sent, 4, 1)
	expect(t, b.flush("sp1", Options{}), Sent, 4, 1)
	a.hs.hide = nil
	if n := a.hs.countIn("conversation:sp1", "turn a4"); n != 0 {
		t.Fatalf("B's segment 1 did not replace A's: a4 stored %d times", n)
	}
	expect(t, a.tick(), Sent, 1, 2)
	a.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3", "a4", "b4")
	a.splitOn("conversation:sp1")
	if len(a.replaces()) != 0 {
		t.Errorf("A's replacing part still pending: %+v", a.replaces())
	}
	sent := len(a.hs.retains)
	expect(t, b.tick(), Nothing, 0, 0)
	expect(t, b.tick(), Nothing, 0, 0)
	expect(t, a.tick(), Nothing, 0, 0)
	if len(a.hs.retains) != sent {
		t.Errorf("sent again: %+v", a.hs.retains[sent:])
	}
	a.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3", "a4", "b4")
}

// dedup-concurrent: the same copied session, unchanged, sent by both PCs
// at once. Each PC finds its records on the server: confirmed on both, no
// part sent again, no split, every record once.
func TestIdenticalFirstSegmentsFromTwoPCsAreConfirmedOnBoth(t *testing.T) {
	a := newEnv(t)
	a.f.MachineID = "m1"
	a.turns(0, "r1", "r2", "r3")
	b := a.otherPC()
	a.copyTo(b)
	a.hs.hide = map[string]bool{"conversation:sp1": true}
	expect(t, a.flush("sp1", Options{}), Sent, 3, 1)
	expect(t, b.flush("sp1", Options{}), Sent, 3, 1)
	a.hs.hide = nil
	sent := len(a.hs.retains)
	for range 2 {
		expect(t, a.tick(), Nothing, 0, 0)
		expect(t, b.tick(), Nothing, 0, 0)
	}
	if len(a.hs.retains) != sent {
		t.Errorf("sent again: %+v", a.hs.retains[sent:])
	}
	if len(a.replaces()) != 0 || len(b.replaces()) != 0 {
		t.Errorf("still pending: A %+v, B %+v", a.replaces(), b.replaces())
	}
	a.splitOn()
	b.splitOn()
	a.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3")
}

// This PC alone: its segment 1 is on the server. The first tick sees it,
// the next confirms it, and no tick reads the document again.
func TestOwnFirstSegmentIsConfirmedThenNeverReadAgain(t *testing.T) {
	e := newEnv(t)
	e.turns(0, "r1", "r2", "r3")
	expect(t, e.flush("sp1", Options{}), Sent, 3, 1)
	reads := e.hs.readsOf("conversation:sp1")
	expect(t, e.tick(), Nothing, 0, 0)
	if n := e.hs.readsOf("conversation:sp1") - reads; n != 1 {
		t.Errorf("first tick read the document %d times, want once", n)
	}
	if p := e.replaces()["billing"]; p.SeenAt.IsZero() {
		t.Errorf("first look not recorded: %+v", e.replaces())
	}
	expect(t, e.tick(), Nothing, 0, 0)
	if len(e.replaces()) != 0 {
		t.Errorf("not confirmed: %+v", e.replaces())
	}
	after := e.hs.readsOf("conversation:sp1")
	expect(t, e.tick(), Nothing, 0, 0)
	expect(t, e.tick(), Nothing, 0, 0)
	if n := e.hs.readsOf("conversation:sp1") - after; n != 0 {
		t.Errorf("read %d times after its confirmation", n)
	}
	if len(e.hs.retains) != 1 {
		t.Errorf("%d items sent", len(e.hs.retains))
	}
}

// A purge of the document (its tombstone), even with the server still
// holding it: never written again, and the pending mark goes.
func TestAPurgedReplacedSegmentIsNeverSentAgain(t *testing.T) {
	a, _ := betaReplacedByB(t)
	addTombstone(t, a, state.Tombstone{Bank: a.f.Bank, Document: betaDoc, Session: "sp1", Project: "beta", PurgedAt: a.clock})
	sent := len(a.hs.retains)
	a.tick()
	a.tick()
	for _, it := range a.hs.retains[sent:] {
		t.Errorf("sent after the purge: %s segment %s", it.DocumentID, it.Metadata["segment"])
	}
	if _, ok := a.replaces()["beta"]; ok {
		t.Errorf("still pending: %+v", a.replaces())
	}
}

// A restore moved the bank's epoch since the send, without a reconcile
// that judged this session (its cursor predates the backup): the mark
// stays, and is looked at against the restored document.
func TestAnEpochChangeKeepsThePendingMark(t *testing.T) {
	a, _ := betaReplacedByB(t)
	if err := state.Update(a.f.Dirs, "test", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: a.f.Bank, Epoch: 1, ReconciledAt: a.clock}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, a.tick(), Sent, 2, 2)
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
	a.splitOn(betaDoc)
}

// A mark seen before a restore is not confirmed by that first look: the
// two looks start over on the restored server.
func TestARestoreStartsTheLooksOver(t *testing.T) {
	e := newEnv(t)
	e.turns(0, "r1")
	expect(t, e.flush("sp1", Options{}), Sent, 1, 1)
	expect(t, e.tick(), Nothing, 0, 0)
	if p := e.replaces()["billing"]; p.SeenAt.IsZero() {
		t.Fatalf("first look not recorded: %+v", e.replaces())
	}
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: e.f.Bank, Epoch: 1, ReconciledAt: e.clock}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, e.tick(), Nothing, 0, 0)
	if p, ok := e.replaces()["billing"]; !ok || p.Epoch != 1 || !p.SeenAt.Equal(e.clock) {
		t.Fatalf("after the restore: %+v, want seen again now at epoch 1", e.replaces())
	}
	expect(t, e.tick(), Nothing, 0, 0)
	if len(e.replaces()) != 0 {
		t.Errorf("not confirmed: %+v", e.replaces())
	}
}

// The race happened before the nightly backup: A sent its segment 1 at
// 12:00 (B's replaced it), then went offline; the backup at 15:00 holds
// B's segment 1 only, and the server is restored from it at 16:00. The
// reconcile does not look at A's cursor (sent before the backup), so the
// mark is what repairs: A's tick re-sends a2 a3 after B's segment, and
// records the split. Before, the epoch change dropped the mark: lost,
// silently.
func TestARaceBeforeARestoredBackupIsRepairedAfterTheRestore(t *testing.T) {
	a, _ := betaReplacedByB(t)
	snap := a.hs.backup()
	a.hs.restore(snap, 1, clockAt(15, 0), clockAt(16, 0))
	a.clock = clockAt(16, 0)
	rec, err := a.f.Reconcile(context.Background())
	if err != nil || rec.To != 1 || len(rec.Resend) != 0 {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	sent := len(a.hs.retains)
	expect(t, a.tick(), Sent, 2, 2)
	if len(a.hs.retains) != sent+1 {
		t.Fatalf("%d items sent, want 1", len(a.hs.retains)-sent)
	}
	if it := a.hs.retains[sent]; it.DocumentID != betaDoc || it.UpdateMode != "append" || it.Metadata["segment"] != "2" {
		t.Errorf("A's repair: %s %q segment %s", it.DocumentID, it.UpdateMode, it.Metadata["segment"])
	}
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
	a.splitOn(betaDoc)
	if _, ok := a.replaces()["beta"]; ok {
		t.Errorf("still pending: %+v", a.replaces())
	}
	for range 2 {
		expect(t, a.tick(), Nothing, 0, 0)
	}
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
}

// The race of the test above with A offline 30 h: its mark is past the
// 24 h limit at the restore. The restore gives it a full window again, so
// a queue still busy at the first looks after it (every client is
// re-sending what the backup lacked) keeps the mark, and a later look
// with the queue clear re-sends a2 a3.
func TestARestoreGivesAnExpiredMarkAFullWindow(t *testing.T) {
	a, _ := betaReplacedByB(t)
	snap := a.hs.backup()
	restoredAt := a.clock.Add(30 * time.Hour)
	a.hs.restore(snap, 1, a.clock.Add(29*time.Hour), restoredAt)
	a.clock = restoredAt
	if rec, err := a.f.Reconcile(context.Background()); err != nil || len(rec.Resend) != 0 {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	a.hs.queued = 1
	for range 2 {
		if r := a.tick(); len(r.Unconfirmed) != 0 {
			t.Fatalf("given up at a busy look: %+v", r.Unconfirmed)
		}
	}
	if _, ok := a.replaces()["beta"]; !ok {
		t.Fatalf("mark dropped: %+v", a.replaces())
	}
	a.hs.queued = 0
	expect(t, a.tick(), Sent, 2, 2)
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
}

// The race of the tests above, restored twice from the same backup: once
// 6 h after the send, then 40 h after it on a fresh host, which may
// write the epoch number A already holds. Either restore is another one
// (its restored_at tells), so each gives the mark a full window: a queue
// busy at the first look after the second keeps the mark, and a look
// with the queue clear re-sends a2 a3, once. Before, a restore under the
// same number left the window running from the first one: given up.
func TestASecondRestoreGivesTheMarkAFullWindow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		epoch int
	}{
		{"same number", 1},
		{"higher number", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := betaReplacedByB(t)
			t0 := a.clock
			snap := a.hs.backup()
			restore := func(epoch int, at time.Time) {
				t.Helper()
				a.hs.restore(snap, epoch, t0.Add(5*time.Hour), at)
				a.clock = at
				a.hs.queued = 0
				if rec, err := a.f.Reconcile(context.Background()); err != nil || len(rec.Resend) != 0 {
					t.Fatalf("reconcile: %+v %v", rec, err)
				}
				a.hs.queued = 1
				if r := a.tick(); len(r.Unconfirmed) != 0 {
					t.Fatalf("given up at the first busy look after a restore to epoch %d: %+v", epoch, r.Unconfirmed)
				}
				if _, ok := a.replaces()["beta"]; !ok {
					t.Fatalf("mark dropped after a restore to epoch %d: %+v", epoch, a.replaces())
				}
			}
			restore(1, t0.Add(6*time.Hour))
			restore(tc.epoch, t0.Add(40*time.Hour))
			if p := a.replaces()["beta"]; p.Epoch != tc.epoch || !p.RestoredAt.Equal(a.clock) {
				t.Errorf("mark after the second restore: %+v, want epoch %d, restored at %v", p, tc.epoch, a.clock)
			}
			a.hs.queued = 0
			expect(t, a.tick(), Sent, 2, 2)
			a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
			a.splitOn(betaDoc)
		})
	}
}

// A mark written before it kept its epoch's restored_at, at epoch 1,
// restored 3 h before the send. Another restore under that number later
// is told by the times (the mark synced to its epoch before it): a full
// window, as above, once. With no other restore, nothing starts over:
// the 24 h run from the send, and a mark never confirmed is given up.
func TestAMarkWithoutItsEpochsIdentity(t *testing.T) {
	legacy := func(t *testing.T) (*env, time.Time) {
		a, _ := betaReplacedByB(t)
		t0 := a.clock
		if err := state.Update(a.f.Dirs, "test", func(s *state.State) error {
			s.RestoreEpoch = &state.RestoreEpoch{Bank: a.f.Bank, Epoch: 1, ReconciledAt: t0.Add(-2 * time.Hour), RestoredAt: t0.Add(-3 * time.Hour)}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		cur := a.sp1Cursor()
		for p, pr := range cur.Replaces {
			pr.Epoch, pr.EpochRestoredAt = 1, time.Time{}
			cur.Replaces[p] = pr
		}
		if err := SaveCursor(a.f.Dirs.CursorsDir(), "sp1", cur); err != nil {
			t.Fatal(err)
		}
		return a, t0
	}
	t.Run("another restore under its number", func(t *testing.T) {
		a, t0 := legacy(t)
		a.hs.restore(a.hs.backup(), 1, t0.Add(5*time.Hour), t0.Add(40*time.Hour))
		a.clock = t0.Add(40 * time.Hour)
		if rec, err := a.f.Reconcile(context.Background()); err != nil || len(rec.Resend) != 0 {
			t.Fatalf("reconcile: %+v %v", rec, err)
		}
		a.hs.queued = 1
		for range 2 {
			if r := a.tick(); len(r.Unconfirmed) != 0 {
				t.Fatalf("given up at a busy look: %+v", r.Unconfirmed)
			}
		}
		if p := a.replaces()["beta"]; !p.RestoredAt.Equal(a.clock.Add(-15*time.Minute)) || !p.EpochRestoredAt.Equal(t0.Add(40*time.Hour)) {
			t.Fatalf("mark: %+v, want started over once, at the first look after the restore", p)
		}
		a.hs.queued = 0
		expect(t, a.tick(), Sent, 2, 2)
		a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
	})
	t.Run("no other restore", func(t *testing.T) {
		a, t0 := legacy(t)
		a.hs.queued = 1
		a.tick()
		if p := a.replaces()["beta"]; !p.RestoredAt.IsZero() {
			t.Fatalf("started over with no restore: %+v", p)
		}
		a.clock = t0.Add(PendingLimit)
		r := a.tick()
		if !slices.ContainsFunc(r.Unconfirmed, func(u Unconfirmed) bool { return u.Document == betaDoc }) {
			t.Fatalf("not given up %v after the send: %+v, pending %+v", PendingLimit, r.Unconfirmed, a.replaces())
		}
	})
}

// The same race, with A's send after the backup (12:00, the backup at
// 11:30): the reconcile judges A's cursor and drops it with its mark; the
// resync sends what the server lacks once, and the mark sends nothing
// more.
func TestARaceAfterARestoredBackupIsTheReconcilesAlone(t *testing.T) {
	a, _ := betaReplacedByB(t)
	snap := a.hs.backup()
	a.hs.restore(snap, 1, clockAt(11, 30), clockAt(13, 0))
	a.clock = clockAt(13, 0)
	rec, err := a.f.Reconcile(context.Background())
	if err != nil || !reflect.DeepEqual(rec.Resend, []string{"sp1"}) {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	for range 3 {
		a.tick()
	}
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
	if _, ok := a.replaces()["beta"]; ok {
		t.Errorf("still pending: %+v", a.replaces())
	}
}

// The document cannot be read: nothing goes to it and the mark stays as
// it was, but the check never holds the session or the run: the flush
// sends the session's new turn as usual. A later tick repairs.
func TestAFailedReadOfAReplacedSegmentSendsNothingThere(t *testing.T) {
	a, _ := betaReplacedByB(t)
	before := a.replaces()
	a.hs.failGet = map[string]bool{betaDoc: true}
	a.gammaTurn("r9")
	sent := len(a.hs.retains)
	r := a.tick()
	expect(t, r, Sent, 1, 1)
	if r.Stop || len(a.hs.retains) != sent+1 || a.hs.retains[sent].DocumentID != "conversation:sp1@gamma" {
		t.Fatalf("%+v, items %+v", r, a.hs.retains[sent:])
	}
	if !reflect.DeepEqual(a.replaces()["beta"], before["beta"]) {
		t.Errorf("mark changed: %+v, was %+v", a.replaces(), before)
	}
	a.hs.failGet = nil
	expect(t, a.tick(), Sent, 2, 2)
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
	a.hs.expectEachOnce(t, "conversation:sp1@gamma", "r9")
}

// The bank's queue decides when the document is read: a retain naming it
// (or naming no single document) may be either PC's segment 1 not applied
// yet, so the mark waits; an empty queue, or one naming only other
// documents, lets the tick look.
func TestAReplacedSegmentWaitsForAQueueNamingIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		ops  []hindsight.Operation
		look bool
	}{
		{"this document", []hindsight.Operation{{ID: "o1", Type: "retain", Status: "pending", DocumentID: betaDoc}}, false},
		{"no single document", []hindsight.Operation{{ID: "o1", Type: "batch_retain", Status: "pending"}}, false},
		{"another document", []hindsight.Operation{{ID: "o1", Type: "retain", Status: "pending", DocumentID: "conversation:other"}}, true},
		{"an empty queue", nil, true},
		{"only consolidations", []hindsight.Operation{{ID: "o1", Type: "consolidation", Status: "pending"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := betaReplacedByB(t)
			a.hs.opsList = tc.ops
			reads, sent := a.hs.readsOf(betaDoc), len(a.hs.retains)
			r := a.tick()
			read := a.hs.readsOf(betaDoc) > reads
			if read != tc.look {
				t.Errorf("read the document: %v, want %v", read, tc.look)
			}
			if tc.look {
				expect(t, r, Sent, 2, 2)
				return
			}
			if len(a.hs.retains) != sent {
				t.Errorf("sent %+v while the queue names it", a.hs.retains[sent:])
			}
			if _, ok := a.replaces()["beta"]; !ok {
				t.Errorf("mark dropped: %+v", a.replaces())
			}
			a.hs.opsList = nil
			expect(t, a.tick(), Sent, 2, 2)
		})
	}
}

// A queue that cannot be read, or is longer than read: no look, the mark
// stays, and the session's new turn still goes out (never held by it).
func TestAnUnreadableQueueLetsTheFlushGoOn(t *testing.T) {
	for name, set := range map[string]func(h *fakeHS){
		"unreadable": func(h *fakeHS) { h.opsStatus = 500 },
		"too long":   func(h *fakeHS) { h.queued = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := betaReplacedByB(t)
			set(a.hs)
			a.gammaTurn("r9")
			reads := a.hs.readsOf(betaDoc)
			r := a.tick()
			expect(t, r, Sent, 1, 1)
			if r.Stop || a.hs.readsOf(betaDoc) != reads {
				t.Errorf("%+v, %d reads of beta", r, a.hs.readsOf(betaDoc)-reads)
			}
			if _, ok := a.replaces()["beta"]; !ok {
				t.Errorf("mark dropped: %+v", a.replaces())
			}
		})
	}
}

// A connect started this PC's sends over between the send and the tick
// (the tick's config is the old server's), or while the tick reads the
// document: nothing goes to the old server.
func TestAConnectBeforeTheRepairSendsNothing(t *testing.T) {
	connect := func(e *env) {
		_ = state.Update(e.f.Dirs, "connect", func(s *state.State) error { s.SendsGeneration++; return nil })
	}
	t.Run("before the tick", func(t *testing.T) {
		a, _ := betaReplacedByB(t)
		connect(a)
		sent := len(a.hs.retains)
		if r := a.tick(); r.Status != Retry || len(a.hs.retains) != sent {
			t.Errorf("%+v, %d items sent", r, len(a.hs.retains)-sent)
		}
	})
	t.Run("while the tick reads", func(t *testing.T) {
		a, _ := betaReplacedByB(t)
		a.hs.onDocument = func(id string) {
			if id == betaDoc {
				connect(a)
			}
		}
		sent := len(a.hs.retains)
		r := a.tick()
		if r.Status != Retry || len(a.hs.retains) != sent {
			t.Errorf("%+v, %d items sent", r, len(a.hs.retains)-sent)
		}
		if _, ok := a.replaces()["beta"]; !ok {
			t.Errorf("mark dropped: %+v", a.replaces())
		}
	})
}

// One run reads at most maxReplaceReads documents to confirm: the others
// wait for the next run.
func TestReplaceChecksAreBoundedPerRun(t *testing.T) {
	old := maxReplaceReads
	maxReplaceReads = 1
	t.Cleanup(func() { maxReplaceReads = old })
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "one")
	e.record("s2", "u2", "user", 0, "two")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	expect(t, e.flush("s2", Options{}), Sent, 1, 1)
	e.clock = e.clock.Add(15 * time.Minute)
	f := *e.f
	f.replaceReads, f.queue = 0, nil
	reads := e.hs.gets
	for _, sid := range []string{"s1", "s2"} {
		f.FlushFile(context.Background(), filepath.Join(e.dir, sid+".jsonl"), Options{})
	}
	if n := e.hs.gets - reads; n != 1 {
		t.Errorf("one run read %d documents, want 1", n)
	}
	if last := e.hs.reads[len(e.hs.reads)-1]; !slices.Contains([]string{"conversation:s1", "conversation:s2"}, last) {
		t.Errorf("read %s", last)
	}
}

// A's next turn of beta goes out before the queue lets the tick look:
// the flush finds B's segment 1 there and resyncs (a split). The pending
// replace survives that resync, and the next tick still sends a2 a3.
func TestAPendingReplaceSurvivesAResync(t *testing.T) {
	a, _ := betaReplacedByB(t)
	a.hs.queued = 1
	a.recordIn(a.repo("beta"), "sp1", "a4", "user", 3, "turn a4")
	expect(t, a.tick(), Sent, 1, 2)
	if _, ok := a.replaces()["beta"]; !ok {
		t.Fatalf("the resync dropped the pending replace: %+v", a.replaces())
	}
	a.hs.queued = 0
	expect(t, a.tick(), Sent, 2, 3)
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "a4", "b2")
	a.splitOn(betaDoc)
}

// A appends to beta before B's segment 1 is applied (A1, A2, then B1):
// B's replace takes A's append with it. Both parts go again.
func TestAnAppendBeforeTheOtherPCsReplaceIsSentAgainToo(t *testing.T) {
	a := newEnv(t)
	a.f.MachineID = "m1"
	beta := a.repo("beta")
	a.turns(0, "r1")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	b := a.otherPC()
	a.copyTo(b)
	expect(t, b.flush("sp1", Options{}), Nothing, 0, 0)
	a.hs.hide = map[string]bool{betaDoc: true}
	a.recordIn(beta, "sp1", "a2", "user", 1, "turn a2")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	a.recordIn(beta, "sp1", "a3", "user", 2, "turn a3")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 2)
	b.recordIn(beta, "sp1", "b2", "user", 1, "turn b2")
	expect(t, b.flush("sp1", Options{}), Sent, 1, 1)
	a.hs.hide = nil
	if n := a.hs.countIn(betaDoc, "turn a3"); n != 0 {
		t.Fatalf("B's segment 1 did not replace A's append: a3 stored %d times", n)
	}
	expect(t, a.tick(), Sent, 2, 2)
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
}

// A server whose documents give no original_text: this PC's lone
// segment 1, still the last part applied (its metadata), is confirmed;
// one another PC's segment replaced cannot be compared, so nothing is
// sent and the mark waits (dropped after PendingLimit).
func TestWithoutOriginalTextOnlyTheMetadataConfirms(t *testing.T) {
	t.Run("own", func(t *testing.T) {
		e := newEnv(t)
		e.hs.noText = true
		e.turns(0, "r1", "r2")
		expect(t, e.flush("sp1", Options{}), Sent, 2, 1)
		e.tick()
		e.tick()
		if len(e.replaces()) != 0 {
			t.Errorf("not confirmed: %+v", e.replaces())
		}
	})
	t.Run("replaced", func(t *testing.T) {
		a, _ := betaReplacedByB(t)
		a.hs.noText = true
		sent := len(a.hs.retains)
		a.tick()
		a.tick()
		if len(a.hs.retains) != sent {
			t.Errorf("sent: %+v", a.hs.retains[sent:])
		}
		if _, ok := a.replaces()["beta"]; !ok {
			t.Errorf("mark dropped: %+v", a.replaces())
		}
		a.clock = a.clock.Add(PendingLimit)
		r := a.tick()
		if _, ok := a.replaces()["beta"]; ok || len(a.hs.retains) != sent {
			t.Errorf("after PendingLimit: %+v, %d sent", a.replaces(), len(a.hs.retains)-sent)
		}
		expectUnconfirmed(t, r, "no original_text")
	})
}

// The document is gone with no tombstone or restore to explain it: it is
// never recreated. The mark waits, then goes after PendingLimit.
func TestAnAbsentReplacedDocumentIsNeverRecreated(t *testing.T) {
	a, _ := betaReplacedByB(t)
	delete(a.hs.docs, betaDoc)
	delete(a.hs.content, betaDoc)
	sent := len(a.hs.retains)
	a.tick()
	if _, ok := a.replaces()["beta"]; !ok || len(a.hs.retains) != sent {
		t.Errorf("mark %+v, %d sent", a.replaces(), len(a.hs.retains)-sent)
	}
	a.clock = a.clock.Add(PendingLimit)
	r := a.tick()
	if _, ok := a.replaces()["beta"]; ok || len(a.hs.retains) != sent {
		t.Errorf("after PendingLimit: %+v, %d sent", a.replaces(), len(a.hs.retains)-sent)
	}
	if _, ok := a.hs.docs[betaDoc]; ok {
		t.Error("recreated")
	}
	expectUnconfirmed(t, r, "not on the server")
}

// Only A's append is missing (the shared r1-r3 are in B's segment 1): the
// part sent again holds the same records as that append, and must not
// carry its operation id, which the server collapses as a repeat.
func TestAResentAppendIsNotCollapsedWithTheOriginal(t *testing.T) {
	a := newEnv(t)
	a.f.MachineID = "m1"
	a.turns(0, "r1", "r2", "r3")
	b := a.otherPC()
	a.copyTo(b)
	a.hs.hide = map[string]bool{"conversation:sp1": true}
	expect(t, a.flush("sp1", Options{}), Sent, 3, 1)
	a.turns(3, "a4")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 2)
	b.turns(3, "b4")
	expect(t, b.flush("sp1", Options{}), Sent, 4, 1)
	a.hs.hide = nil
	if n := a.hs.countIn("conversation:sp1", "turn a4"); n != 0 {
		t.Fatalf("B's segment 1 did not replace A's append: a4 stored %d times", n)
	}
	expect(t, a.tick(), Sent, 1, 2)
	a.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3", "a4", "b4")
}

// expectUnconfirmed fails unless r reports beta's document given up, for
// a reason containing why.
func expectUnconfirmed(t *testing.T, r Result, why string) {
	t.Helper()
	if !slices.ContainsFunc(r.Unconfirmed, func(u Unconfirmed) bool { return u.Document == betaDoc && strings.Contains(u.Reason, why) }) {
		t.Errorf("unconfirmed %+v, want %s (%s)", r.Unconfirmed, betaDoc, why)
	}
}

// gammaTurn records a new turn of sp1 in a third project, gamma: a send
// to another document than beta's.
func (e *env) gammaTurn(uuid string) {
	e.t.Helper()
	e.recordIn(e.repo("gamma"), "sp1", uuid, "user", 5, "turn "+uuid)
}

// betaReplacedWith is betaReplacedByB with A's beta turns and B's own as
// given: text a2 at minute 1, "turn a3" at 2, B's text b2 at minute 3.
func betaReplacedWith(t *testing.T, a2, b2 string) (a, b *env) {
	a = newEnv(t)
	a.f.MachineID = "m1"
	beta := a.repo("beta")
	a.turns(0, "r1")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	b = a.otherPC()
	a.copyTo(b)
	expect(t, b.flush("sp1", Options{}), Nothing, 0, 0)
	a.hs.hide = map[string]bool{betaDoc: true}
	a.recordIn(beta, "sp1", "a2", "user", 1, a2)
	a.recordIn(beta, "sp1", "a3", "user", 2, "turn a3")
	expect(t, a.flush("sp1", Options{}), Sent, 2, 1)
	b.recordIn(beta, "sp1", "b2", "user", 3, b2)
	expect(t, b.flush("sp1", Options{}), Sent, 1, 1)
	a.hs.hide = nil
	return a, b
}

// A short or recurring turn of A's, lost under B's segment 1 whose text
// holds the same words (B's own turn, the role, a timestamp's digits), is
// still missing: matched by its whole line, it goes again.
func TestAShortTurnLostUnderTheSameWordsIsSentAgain(t *testing.T) {
	for _, text := range []string{"yes", "2", "user"} {
		t.Run(text, func(t *testing.T) {
			a, _ := betaReplacedWith(t, text, text)
			sent := len(a.hs.retains)
			expect(t, a.tick(), Sent, 2, 2)
			if len(a.hs.retains) != sent+1 {
				t.Fatalf("%d items sent", len(a.hs.retains)-sent)
			}
			line := `{"role":"user","content":` + strconv.Quote(text) + `,"timestamp":"2026-10-24T09:01:00Z"}`
			if n := a.hs.countIn(betaDoc, line); n != 1 {
				t.Errorf("A's %q (09:01) stored %d times:\n%s", text, n, a.hs.content[betaDoc])
			}
		})
	}
}

// A record whose time was in the future when sent was given the send's
// time (clampTime), which a later read gives otherwise: matched without
// its time, it is found on the server, not sent twice.
func TestARecordClampedAtItsSendIsFoundWithoutItsTime(t *testing.T) {
	e := newEnv(t)
	e.turns(0, "r1")
	expect(t, e.flush("sp1", Options{}), Sent, 1, 1)
	e.record("sp1", "f2", "user", 60*24*400, "turn from a clock ahead")
	expect(t, e.flush("sp1", Options{Force: true}), Sent, 1, 2)
	e.tick()
	e.tick()
	if len(e.hs.retains) != 2 {
		t.Errorf("sent again: %+v", e.hs.retains[2:])
	}
	if len(e.replaces()) != 0 {
		t.Errorf("not confirmed: %+v", e.replaces())
	}
}

// A server segment that is not a number, with records missing: the
// session is quarantined as a send would quarantine it (movedElsewhere),
// never retried at every tick.
func TestAnInvalidServerSegmentQuarantinesTheRepair(t *testing.T) {
	a, _ := betaReplacedByB(t)
	a.hs.docs[betaDoc]["segment"] = "x"
	sent := len(a.hs.retains)
	r := a.tick()
	if r.Status != Quarantined || len(a.hs.retains) != sent {
		t.Fatalf("%+v, %d sent", r, len(a.hs.retains)-sent)
	}
	if r := a.tick(); r.Status != Held {
		t.Errorf("next tick: %+v", r)
	}
}

// A mark always ends 24 hours after its send. Past that, a tick that can
// still look does once (here it confirms, nothing to report); one that
// cannot drops it and reports the document given up.
func TestAMarkEndsAfterPendingLimit(t *testing.T) {
	t.Run("looked at once more", func(t *testing.T) {
		e := newEnv(t)
		e.turns(0, "r1")
		expect(t, e.flush("sp1", Options{}), Sent, 1, 1)
		e.clock = e.clock.Add(PendingLimit)
		r := e.tick()
		if len(e.replaces()) != 0 || len(r.Unconfirmed) != 0 {
			t.Errorf("%+v, unconfirmed %+v", e.replaces(), r.Unconfirmed)
		}
	})
	t.Run("queue busy", func(t *testing.T) {
		a, _ := betaReplacedByB(t)
		a.hs.queued = 1
		a.tick()
		a.clock = a.clock.Add(PendingLimit)
		a.gammaTurn("r9")
		r := a.tick()
		expect(t, r, Sent, 1, 1)
		if _, ok := a.replaces()["beta"]; ok {
			t.Errorf("mark kept: %+v", a.replaces())
		}
		expectUnconfirmed(t, r, "queue")
	})
	// The limit is 24 hours, not a symbol the test reads: a minute short of
	// it, a mark the busy queue keeps from looking at stays, unreported;
	// at 24 hours it is given up.
	t.Run("queue busy at 23 h 59 min, then 24 h", func(t *testing.T) {
		a, _ := betaReplacedByB(t)
		a.hs.queued = 1
		sent := a.replaces()["beta"].SentAt
		a.clock = sent.Add(24*time.Hour - time.Minute - 15*time.Minute) // tick adds 15 min
		if r := a.tick(); len(r.Unconfirmed) != 0 {
			t.Errorf("given up at 23 h 59 min: %+v", r.Unconfirmed)
		}
		if p, ok := a.replaces()["beta"]; !ok || !p.SentAt.Equal(sent) {
			t.Fatalf("mark at 23 h 59 min: %+v, want kept", a.replaces())
		}
		a.clock = sent.Add(24*time.Hour - 15*time.Minute)
		r := a.tick()
		if _, ok := a.replaces()["beta"]; ok {
			t.Errorf("mark kept at 24 h: %+v", a.replaces())
		}
		expectUnconfirmed(t, r, "queue")
	})
	t.Run("read failing", func(t *testing.T) {
		a, _ := betaReplacedByB(t)
		a.hs.failGet = map[string]bool{betaDoc: true}
		a.clock = a.clock.Add(PendingLimit)
		r := a.tick()
		if _, ok := a.replaces()["beta"]; ok || r.Stop {
			t.Errorf("mark kept: %+v, %+v", a.replaces(), r)
		}
		expectUnconfirmed(t, r, "HTTP 500")
	})
}

// A run whose reads are spent leaves its marks pending, even past
// PendingLimit: they are not given up unlooked. The next run with reads
// looks, and re-sends what B's segment 1 replaced.
func TestAMarkPastPendingLimitWaitsForARunWithReads(t *testing.T) {
	a, _ := betaReplacedByB(t)
	before := slices.Sorted(maps.Keys(a.replaces()))
	if len(before) < 2 {
		t.Fatalf("want A's home and beta marks, got %v", before)
	}
	old := maxReplaceReads
	maxReplaceReads = 0
	t.Cleanup(func() { maxReplaceReads = old })
	a.clock = a.clock.Add(PendingLimit)
	sent := len(a.hs.retains)
	r := a.tick()
	if got := slices.Sorted(maps.Keys(a.replaces())); !slices.Equal(got, before) {
		t.Errorf("marks after a run without reads: %v, want %v kept", got, before)
	}
	if len(r.Unconfirmed) != 0 || len(a.hs.retains) != sent {
		t.Errorf("a run without reads gave up %+v, sent %d", r.Unconfirmed, len(a.hs.retains)-sent)
	}
	maxReplaceReads = old
	expect(t, a.tick(), Sent, 2, 2)
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
}

// The parts a pending replace remembers stay few: consecutive segments
// sent to its document (a long first send, in chunks) are one range.
func TestConsecutiveSegmentsAreOnePart(t *testing.T) {
	old := maxSegmentRecords
	maxSegmentRecords = 2
	t.Cleanup(func() { maxSegmentRecords = old })
	e := newEnv(t)
	e.turns(0, "r1", "r2", "r3", "r4", "r5")
	expect(t, e.flush("sp1", Options{}), Sent, 5, 3)
	e.turns(5, "r6")
	expect(t, e.flush("sp1", Options{}), Sent, 1, 4)
	if p := e.replaces()["billing"]; !reflect.DeepEqual(p.Parts, []RecordRange{{First: "r1", Last: "r6"}}) {
		t.Errorf("parts %+v", p.Parts)
	}
}

// The second look confirms only replaceConfirmGap (5 min) after the
// first: a run a minute later (a detached flush after a hook, then a tick)
// leaves the mark as the first look left it; the run 5 min after confirms.
func TestTheSecondLookWaitsFiveMinutes(t *testing.T) {
	e := newEnv(t)
	e.turns(0, "r1")
	expect(t, e.flush("sp1", Options{}), Sent, 1, 1)
	e.tick()
	seen := e.replaces()["billing"].SeenAt
	if seen.IsZero() {
		t.Fatalf("first look not recorded: %+v", e.replaces())
	}
	e.clock = seen.Add(4*time.Minute + 59*time.Second - 15*time.Minute) // tick adds 15 min
	e.tick()
	if p, ok := e.replaces()["billing"]; !ok || !p.SeenAt.Equal(seen) {
		t.Fatalf("a look 4 min 59 s after the first: %+v, want the mark kept, seen at %s", e.replaces(), seen)
	}
	e.clock = seen.Add(5*time.Minute - 15*time.Minute)
	e.tick()
	if p, ok := e.replaces()["billing"]; ok {
		t.Errorf("a look 5 min after the first did not confirm: %+v", p)
	}
}

// A's first look finds its segment 1 (B has not sent yet); B, which read
// the document absent before A's was applied, replaces it just after. The
// second look, not a confirmation, finds A's records gone and repairs.
func TestTheSecondLookCatchesALateReplace(t *testing.T) {
	a := newEnv(t)
	a.f.MachineID = "m1"
	beta := a.repo("beta")
	a.turns(0, "r1")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	b := a.otherPC()
	a.copyTo(b)
	expect(t, b.flush("sp1", Options{}), Nothing, 0, 0)
	a.recordIn(beta, "sp1", "a2", "user", 1, "turn a2")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	expect(t, a.tick(), Nothing, 0, 0)
	if p := a.replaces()["beta"]; p.SeenAt.IsZero() {
		t.Fatalf("first look not recorded: %+v", a.replaces())
	}
	a.hs.hide = map[string]bool{betaDoc: true} // B read it before A's was applied
	b.recordIn(beta, "sp1", "b2", "user", 2, "turn b2")
	expect(t, b.flush("sp1", Options{}), Sent, 1, 1)
	a.hs.hide = nil
	expect(t, a.tick(), Sent, 1, 2)
	a.hs.expectEachOnce(t, betaDoc, "a2", "b2")
}

// Both PCs look in the same window. B looks first (its segment 1 won, by
// its metadata), A repairs, then B's second look reads A's append on top:
// B's records are still there, by the text. Nothing B sent goes twice.
func TestTheWinnerConfirmsByTextAfterTheLosersRepair(t *testing.T) {
	a, b := betaReplacedByB(t)
	expect(t, b.tick(), Nothing, 0, 0)
	expect(t, a.tick(), Sent, 2, 2)
	b.clock = b.clock.Add(time.Minute)
	expect(t, b.tick(), Nothing, 0, 0)
	if len(b.replaces()) != 0 {
		t.Errorf("B not confirmed: %+v", b.replaces())
	}
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
}

// Out-of-order workers leave each PC's records partly missing (B's append
// applied before its own segment 1, A's segment 1 under B's): each PC
// sends again only its own missing records; the part both share (the
// copied r1-r3) is not sent again by either.
func TestBothPCsRepairingSendOnlyTheirOwn(t *testing.T) {
	a := newEnv(t)
	a.f.MachineID = "m1"
	a.turns(0, "r1", "r2", "r3")
	b := a.otherPC()
	a.copyTo(b)
	a.turns(3, "a4")
	b.turns(3, "b4")
	a.hs.hide = map[string]bool{"conversation:sp1": true}
	expect(t, a.flush("sp1", Options{}), Sent, 4, 1)
	expect(t, b.flush("sp1", Options{}), Sent, 4, 1)
	b.turns(4, "b5")
	expect(t, b.flush("sp1", Options{}), Sent, 1, 2)
	a.hs.hide = nil
	// What the server holds: A's segment 1 replaced by B's, which itself
	// arrived after B's append b5 (lost with A's).
	items := a.hs.itemsOf("conversation:sp1")
	a.hs.content["conversation:sp1"] = items[1].Content
	a.hs.docs["conversation:sp1"] = items[1].Metadata
	expect(t, a.tick(), Sent, 1, 2)
	expect(t, b.tick(), Sent, 1, 3)
	a.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3", "a4", "b4", "b5")
}
