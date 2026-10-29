package retain

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

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

// A restore moved the bank's epoch since the send: what the server holds
// is the backup's, the reconcile's to compare. The mark goes, nothing is
// sent.
func TestAnEpochChangeDropsThePendingMark(t *testing.T) {
	a, _ := betaReplacedByB(t)
	if err := state.Update(a.f.Dirs, "test", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: a.f.Bank, Epoch: 1, ReconciledAt: a.clock}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sent := len(a.hs.retains)
	expect(t, a.tick(), Nothing, 0, 0)
	if len(a.hs.retains) != sent {
		t.Errorf("sent after the epoch change: %+v", a.hs.retains[sent:])
	}
	if len(a.replaces()) != 0 {
		t.Errorf("still pending: %+v", a.replaces())
	}
}

// The document cannot be read: nothing goes out, the run stops, the mark
// stays as it was; a later tick repairs.
func TestAFailedReadOfAReplacedSegmentSendsNothing(t *testing.T) {
	a, _ := betaReplacedByB(t)
	before := a.replaces()
	a.hs.failGet = map[string]bool{betaDoc: true}
	sent := len(a.hs.retains)
	r := a.tick()
	if r.Status != Retry || !r.Stop || len(a.hs.retains) != sent {
		t.Fatalf("%+v, %d items sent", r, len(a.hs.retains)-sent)
	}
	if !reflect.DeepEqual(a.replaces(), before) {
		t.Errorf("mark changed: %+v, was %+v", a.replaces(), before)
	}
	a.hs.failGet = nil
	expect(t, a.tick(), Sent, 2, 2)
	a.hs.expectEachOnce(t, betaDoc, "a2", "a3", "b2")
}

// The bank's queue still holds retains: the server may not have applied
// either segment 1. Nothing is read or sent until it is done.
func TestAReplacedSegmentWaitsForTheQueue(t *testing.T) {
	a, _ := betaReplacedByB(t)
	a.hs.queued = 1
	reads, sent := a.hs.readsOf(betaDoc), len(a.hs.retains)
	expect(t, a.tick(), Nothing, 0, 0)
	if n := a.hs.readsOf(betaDoc) - reads; n != 0 || len(a.hs.retains) != sent {
		t.Errorf("read %d times, sent %d items while the queue is busy", n, len(a.hs.retains)-sent)
	}
	if _, ok := a.replaces()["beta"]; !ok {
		t.Errorf("mark dropped: %+v", a.replaces())
	}
	a.hs.queued = 0
	expect(t, a.tick(), Sent, 2, 2)
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
		a.tick()
		if _, ok := a.replaces()["beta"]; ok || len(a.hs.retains) != sent {
			t.Errorf("after PendingLimit: %+v, %d sent", a.replaces(), len(a.hs.retains)-sent)
		}
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
	a.tick()
	if _, ok := a.replaces()["beta"]; ok || len(a.hs.retains) != sent {
		t.Errorf("after PendingLimit: %+v, %d sent", a.replaces(), len(a.hs.retains)-sent)
	}
	if _, ok := a.hs.docs[betaDoc]; ok {
		t.Error("recreated")
	}
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
