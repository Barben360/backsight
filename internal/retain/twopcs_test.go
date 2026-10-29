package retain

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
)

// otherPC is a second PC sending to e's server under machine id m2: its
// own state, sent index and transcripts, with e's directories and project
// rule, as a transcript copied between two PCs keeps its paths.
func (e *env) otherPC() *env {
	e.t.Helper()
	root := e.t.TempDir()
	d := paths.Dirs{State: filepath.Join(root, "state")}
	idx, err := LoadSentIndex(d.SentIndexDir())
	if err != nil {
		e.t.Fatal(err)
	}
	o := &env{t: e.t, hs: e.hs, dir: filepath.Join(root, "projects", "p"), cwd: e.cwd, clock: e.clock}
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	o.f = &Flusher{Client: e.f.Client, Bank: e.f.Bank, Dirs: d, Resolver: e.f.Resolver, MachineID: "m2", Version: "test",
		Now: func() time.Time { return o.clock }, Index: idx}
	return o
}

// copyTo copies session sp1's transcript from e to o.
func (e *env) copyTo(o *env) {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.dir, "sp1.jsonl"))
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.dir, "sp1.jsonl"), raw, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// turns records one user turn of sp1 per uuid, the n-th at minute
// from+n, with the text "turn <uuid>".
func (e *env) turns(from int, uuids ...string) {
	e.t.Helper()
	for i, u := range uuids {
		e.record("sp1", u, "user", from+i, "turn "+u)
	}
}

// sp1Cursor is this PC's cursor of sp1.
func (e *env) sp1Cursor() Cursor {
	e.t.Helper()
	c, ok, err := LoadCursor(e.f.Dirs.CursorsDir(), "sp1")
	if err != nil || !ok {
		e.t.Fatalf("cursor of sp1: %v (found %v)", err, ok)
	}
	return c
}

// readsOf counts the reads of document doc since the server started.
func (h *fakeHS) readsOf(doc string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, id := range h.reads {
		if id == doc {
			n++
		}
	}
	return n
}

// continuedOnTwoPCs: A (m1) sends sp1's r1-r3; sp1 is copied to B (m2),
// whose flush sends nothing; A appends a4 a5 (segment 2); B appends b4
// b5, not flushed yet. Both cursors are valid: neither PC resyncs.
func continuedOnTwoPCs(t *testing.T) (a, b *env) {
	a = newEnv(t)
	a.f.MachineID = "m1"
	a.turns(0, "r1", "r2", "r3")
	expect(t, a.flush("sp1", Options{}), Sent, 3, 1)
	b = a.otherPC()
	a.copyTo(b)
	expect(t, b.flush("sp1", Options{}), Nothing, 0, 0)
	if c := b.sp1Cursor(); c.Segments[c.Home] != 1 || c.LastUUIDs[c.Home] != "r3" {
		t.Fatalf("B's cursor after the copy: %+v", c)
	}
	a.turns(3, "a4", "a5")
	expect(t, a.flush("sp1", Options{}), Sent, 2, 2)
	b.turns(3, "b4", "b5")
	return a, b
}

// expectEachOnce fails unless every record of uuids is stored once in doc.
func (h *fakeHS) expectEachOnce(t *testing.T, doc string, uuids ...string) {
	t.Helper()
	for _, u := range uuids {
		if n := h.countIn(doc, "turn "+u); n != 1 {
			t.Errorf("%s stored %d times in %s", u, n, doc)
		}
	}
}

// A session continued on two PCs whose cursors are both valid (decision
// 0073, "A session continued on two PCs"): B's append follows the server's
// segment 2 (A's), never repeats its number, records the split, and sends
// only B's own turns. Once A appends again, A finds B's segment the same
// way. Nothing is lost, nothing this PC sent goes twice.
func TestAppendAfterAnotherPCMovedTheDocument(t *testing.T) {
	a, b := continuedOnTwoPCs(t)
	expect(t, b.flush("sp1", Options{}), Sent, 2, 3)
	it := a.hs.retains[len(a.hs.retains)-1]
	if it.DocumentID != "conversation:sp1" || it.UpdateMode != "append" || it.Metadata["segment"] != "3" || it.Metadata["machine_id"] != "m2" {
		t.Errorf("B's segment: %s %q segment %s machine %s", it.DocumentID, it.UpdateMode, it.Metadata["segment"], it.Metadata["machine_id"])
	}
	a.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3", "a4", "a5", "b4", "b5")
	splits, err := SplitSessions(b.f.Dirs.State)
	if s, ok := splits["sp1"]; err != nil || !ok || !reflect.DeepEqual(s.Documents, []string{"conversation:sp1"}) {
		t.Errorf("B's split record: %v %+v", err, splits)
	}
	if c := b.sp1Cursor(); c.Segments[c.Home] != 3 || c.LastUUIDs[c.Home] != "b5" || c.Segment != 3 {
		t.Errorf("B's cursor: segments %v last %v segment %d", c.Segments, c.LastUUIDs, c.Segment)
	}
	sent := len(a.hs.retains)
	if r := b.flush("sp1", Options{}); r.Status != Nothing || len(a.hs.retains) != sent {
		t.Errorf("B flushed again: %+v", r)
	}

	a.turns(5, "a6")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 4)
	a.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3", "a4", "a5", "b4", "b5", "a6")
	if splits, err := SplitSessions(a.f.Dirs.State); err != nil || len(splits["sp1"].Documents) != 1 {
		t.Errorf("A's split record: %v %+v", err, splits)
	}
}

// After its split, B's own segment 3 is not applied yet (async retain):
// the server still names A's segment 2, the position B's resync rebuilt
// from. That is B's own lag, not a new move: B's next segment is 4, not a
// second resync sending 3 again.
func TestAnAppendAfterASplitWhileTheServerLagsFollowsThisPCsSegment(t *testing.T) {
	a, b := continuedOnTwoPCs(t)
	a.hs.stale = true // nothing B sends from now on is applied to the metadata
	expect(t, b.flush("sp1", Options{}), Sent, 2, 3)
	b.turns(5, "b6")
	expect(t, b.flush("sp1", Options{}), Sent, 1, 4)
	var segs []string
	for _, it := range a.hs.itemsOf("conversation:sp1") {
		segs = append(segs, it.Metadata["segment"])
	}
	if !reflect.DeepEqual(segs, []string{"1", "2", "3", "4"}) {
		t.Errorf("segments sent %v, want 1 2 3 4", segs)
	}
	a.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3", "a4", "a5", "b4", "b5", "b6")
	if c := b.sp1Cursor(); c.Segments[c.Home] != 4 || c.LastUUIDs[c.Home] != "b6" {
		t.Errorf("B's cursor: segments %v last %v", c.Segments, c.LastUUIDs)
	}
}

// The server still holds what this PC's cursor says: the append goes out
// as before, one read of its document, no split; a flush with nothing to
// send reads nothing.
func TestAppendWhenTheServerIsUnchangedSendsAsBefore(t *testing.T) {
	e := newEnv(t)
	e.turns(0, "r1", "r2", "r3")
	expect(t, e.flush("sp1", Options{}), Sent, 3, 1)
	reads := e.hs.readsOf("conversation:sp1")
	expect(t, e.flush("sp1", Options{}), Nothing, 0, 0)
	if n := e.hs.readsOf("conversation:sp1") - reads; n != 0 {
		t.Errorf("a flush with nothing to send read the document %d times", n)
	}
	e.turns(3, "a4", "a5")
	expect(t, e.flush("sp1", Options{}), Sent, 2, 2)
	if len(e.hs.retains) != 2 {
		t.Fatalf("%d items: %+v", len(e.hs.retains), e.hs.retains)
	}
	if it := e.hs.retains[1]; it.UpdateMode != "append" || it.Metadata["segment"] != "2" {
		t.Errorf("%q segment %s", it.UpdateMode, it.Metadata["segment"])
	}
	if n := e.hs.readsOf("conversation:sp1") - reads; n != 1 {
		t.Errorf("the append read its document %d times, want once", n)
	}
	if splits, err := SplitSessions(e.f.Dirs.State); err != nil || len(splits) != 0 {
		t.Errorf("split recorded: %v %+v", err, splits)
	}
	e.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3", "a4", "a5")
}

// The document cannot be read before an append: nothing goes out with a
// segment number that may collide, the cursor stays, and the records are
// sent by the next flush.
func TestAppendWhenTheServerReadFailsSendsNothing(t *testing.T) {
	e := newEnv(t)
	e.turns(0, "r1", "r2", "r3")
	expect(t, e.flush("sp1", Options{}), Sent, 3, 1)
	before := e.sp1Cursor()
	e.turns(3, "a4", "a5")
	e.hs.failGet = map[string]bool{"conversation:sp1": true}
	r := e.flush("sp1", Options{})
	if r.Status != Retry || !r.Stop || len(e.hs.retains) != 1 {
		t.Fatalf("%+v, %d items", r, len(e.hs.retains))
	}
	if after := e.sp1Cursor(); !reflect.DeepEqual(after, before) {
		t.Errorf("cursor moved: %+v, was %+v", after, before)
	}
	e.hs.failGet = nil
	expect(t, e.flush("sp1", Options{}), Sent, 2, 2)
	e.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3", "a4", "a5")
}

// The server has not applied this PC's own latest segment yet (async
// retain): it still names an earlier segment of this PC's. That is no
// other PC's move: the next segment follows this PC's cursor as before.
func TestAppendWhileTheServerLagsThisPCsOwnSegmentSendsAsBefore(t *testing.T) {
	e := newEnv(t)
	e.turns(0, "r1")
	expect(t, e.flush("sp1", Options{}), Sent, 1, 1)
	e.hs.stale = true
	e.turns(1, "r2")
	expect(t, e.flush("sp1", Options{}), Sent, 1, 2)
	e.turns(2, "r3")
	expect(t, e.flush("sp1", Options{}), Sent, 1, 3)
	if splits, err := SplitSessions(e.f.Dirs.State); err != nil || len(splits) != 0 {
		t.Errorf("split recorded: %v %+v", err, splits)
	}
	e.hs.expectEachOnce(t, "conversation:sp1", "r1", "r2", "r3")
}

// A document this PC never wrote to, which another PC started after the
// copy: this PC's first segment of it would replace that PC's. It follows
// the server's segment instead, and the split is recorded.
func TestAFirstSegmentOfADocumentAnotherPCStartedFollowsIt(t *testing.T) {
	a := newEnv(t)
	beta := a.repo("beta")
	a.turns(0, "r1")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	b := a.otherPC()
	a.copyTo(b)
	expect(t, b.flush("sp1", Options{}), Nothing, 0, 0)
	a.recordIn(beta, "sp1", "a2", "user", 1, "turn a2")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	b.recordIn(beta, "sp1", "b2", "user", 1, "turn b2")
	expect(t, b.flush("sp1", Options{}), Sent, 1, 2)
	if it := a.hs.retains[len(a.hs.retains)-1]; it.DocumentID != "conversation:sp1@beta" || it.UpdateMode != "append" || it.Metadata["segment"] != "2" {
		t.Errorf("B's segment: %s %q segment %s", it.DocumentID, it.UpdateMode, it.Metadata["segment"])
	}
	a.hs.expectEachOnce(t, "conversation:sp1@beta", "a2", "b2")
	if splits, err := SplitSessions(b.f.Dirs.State); err != nil || !reflect.DeepEqual(splits["sp1"].Documents, []string{"conversation:sp1@beta"}) {
		t.Errorf("B's split record: %v %+v", err, splits)
	}
}

// ContinuedElsewhere is A's side once B appended its segment 3 after A's
// segment 2 (TestAppendAfterAnotherPCMovedTheDocument's first part), for
// verify --live's test: the server both PCs sent to, A's bank, machine id,
// transcript and cursors directory, and when A sent.
type ContinuedElsewhere struct {
	Server             *hindsight.Client
	Bank, MachineID    string
	Transcript, Cursor string
	SentAt             time.Time
}

// ContinueElsewhere plays sp1 continued on A and B, B flushing last, and
// returns A's side (ContinuedElsewhere).
func ContinueElsewhere(t *testing.T) ContinuedElsewhere {
	a, b := continuedOnTwoPCs(t)
	expect(t, b.flush("sp1", Options{}), Sent, 2, 3)
	return ContinuedElsewhere{Server: a.f.Client, Bank: a.f.Bank, MachineID: a.f.MachineID,
		Transcript: filepath.Join(a.dir, "sp1.jsonl"), Cursor: a.f.Dirs.CursorsDir(), SentAt: a.clock}
}
