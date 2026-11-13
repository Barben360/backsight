package retain

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// Review of decision 0090 (PR 341): what a part, a document of its own,
// changes when two PCs, a failed retain, a purge or the read budget meet.

// confirmed runs two ticks 15 minutes apart: the pending check of the
// session's last sends is looked at, then confirmed and dropped.
func (e *env) confirmed() {
	e.t.Helper()
	for range 2 {
		expect(e.t, e.tick(), Nothing, 0, 0)
	}
	if r := e.replaces(); len(r) != 0 {
		e.t.Fatalf("still pending after two looks: %+v", r)
	}
}

// Two PCs continue a copied session: A's part 2 is still queued (not
// readable) when B reads the chain at segment 1 and sends its own part 2
// under the same id, which replaces A's (a retain without update_mode
// replaces its document). Under appends both were kept; with parts, A's
// check of its sends finds its turn missing once the queue is clear, and
// sends it again after the chain's end: nothing is lost.
func TestTwoPCsSendingTheSamePartLoseNoTurn(t *testing.T) {
	a := newEnv(t)
	a.f.MachineID = "m1"
	a.turns(0, "r1")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	a.confirmed()
	b := a.otherPC()
	a.copyTo(b)
	expect(t, b.flush("sp1", Options{}), Nothing, 0, 0)

	const doc, part2 = "conversation:sp1", "conversation:sp1#part2"
	a.turns(1, "a2")
	a.clock = a.clock.Add(3 * time.Hour)
	expect(t, a.flush("sp1", Options{}), Sent, 1, 2)
	a.hs.hide = map[string]bool{part2: true}
	a.hs.opsList = []hindsight.Operation{{ID: "op-a2", Type: "retain", Status: "pending", DocumentID: part2}}
	b.turns(1, "b2")
	b.clock = a.clock
	expect(t, b.flush("sp1", Options{}), Sent, 1, 2)
	a.hs.hide = nil
	if n := a.hs.countIn(doc, "turn a2"); n != 0 || !isPart(a.hs.retains[len(a.hs.retains)-1], doc, 2) {
		t.Fatalf("B's part 2 did not replace A's: a2 stored %d times", n)
	}
	// While the queue names the part, A only waits.
	sent := len(a.hs.retains)
	a.tick()
	if len(a.hs.retains) != sent {
		t.Fatalf("A sent while the queue names %s: %+v", part2, a.hs.retains[sent:])
	}
	a.hs.opsList = nil
	r := a.tick()
	if r.Status != Sent || r.Records != 1 {
		t.Fatalf("A's repair: %+v", r)
	}
	a.hs.expectEachOnce(t, doc, "r1", "a2", "b2")
	for range 2 {
		a.tick()
		b.tick()
	}
	a.hs.expectEachOnce(t, doc, "r1", "a2", "b2")
	if len(a.replaces()) != 0 || len(b.replaces()) != 0 {
		t.Errorf("still pending: A %+v, B %+v", a.replaces(), b.replaces())
	}
}

// A part the server accepted but never stored (its retain failed in the
// worker, or a restore lost it) while the next one is stored: the chain
// steps over the hole, so the session's check of its sends finds the
// part's records missing and sends them again, after the chain's end.
func TestAPartTheServerNeverStoredIsSentAgain(t *testing.T) {
	e := newEnv(t)
	e.turns(0, "r1")
	expect(t, e.flush("sp1", Options{}), Sent, 1, 1)
	e.confirmed()
	const doc, part2 = "conversation:sp1", "conversation:sp1#part2"
	e.turns(1, "r2")
	e.clock = e.clock.Add(3 * time.Hour)
	expect(t, e.flush("sp1", Options{}), Sent, 1, 2)
	e.hs.mu.Lock()
	delete(e.hs.docs, part2)
	delete(e.hs.content, part2)
	e.hs.mu.Unlock()
	// Part 3 goes out a minute later, before any look: it is stored.
	e.turns(2, "r3")
	e.clock = e.clock.Add(time.Minute)
	expect(t, e.flush("sp1", Options{}), Sent, 1, 3)
	if _, ok := e.hs.docs["conversation:sp1#part3"]; !ok {
		t.Fatal("part 3 not stored")
	}
	r := e.tick()
	if r.Status != Sent || r.Records != 1 || !isPart(e.hs.retains[len(e.hs.retains)-1], doc, 4) || !e.sentText(len(e.hs.retains)-1, "turn r2") {
		t.Fatalf("the repair: %+v, last item %+v", r, e.hs.retains[len(e.hs.retains)-1])
	}
	e.hs.expectEachOnce(t, doc, "r1", "r2", "r3")
	sent := len(e.hs.retains)
	for range 3 {
		e.tick()
	}
	if len(e.hs.retains) != sent || len(e.replaces()) != 0 {
		t.Errorf("after the repair: %d sent, pending %+v", len(e.hs.retains)-sent, e.replaces())
	}
}

// A look reads the documents its sends wrote, and never more than the
// run's budget: a check whose parts outnumber what is left is not looked
// at in this run (never a partial read); one that can never fit a run is
// given up at its limit, reported.
func TestALookNeverReadsPastTheRunsBudget(t *testing.T) {
	old := maxReplaceReads
	maxReplaceReads = 2
	t.Cleanup(func() { maxReplaceReads = old })
	e := newEnv(t)
	for i, u := range []string{"r1", "r2", "r3"} {
		e.turns(i, u)
		e.clock = e.clock.Add(3 * time.Hour)
		expect(t, e.flush("sp1", Options{}), Sent, 1, i+1)
	}
	reads := e.hs.gets
	e.tick()
	if n := e.hs.gets - reads; n > maxReplaceReads {
		t.Errorf("one run read %d documents (%v), the budget is %d", n, e.hs.reads[reads:], maxReplaceReads)
	}
	if _, ok := e.replaces()["billing"]; !ok {
		t.Fatalf("the check was dropped unread: %+v", e.replaces())
	}
	e.clock = e.clock.Add(PendingLimit)
	r := e.tick()
	if _, ok := e.replaces()["billing"]; ok || len(r.Unconfirmed) != 1 {
		t.Errorf("past its limit: pending %+v, unconfirmed %+v", e.replaces(), r.Unconfirmed)
	}
}

// A purge's successor goes on in parts of its own (conversation:s1#2,
// then conversation:s1#2#part2), and a tombstone of one of those parts
// moves the session to the next successor.
func TestASuccessorGoesOnInPartsOfItsOwn(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "turn 1")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.purge("conversation:s1")
	e.record("s1", "u2", "user", 60, "turn 2")
	e.clock = e.clock.Add(6 * time.Hour)
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.record("s1", "u3", "user", 120, "turn 3")
	e.clock = e.clock.Add(6 * time.Hour)
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	if it := e.hs.retains[len(e.hs.retains)-1]; !isPart(it, "conversation:s1#2", 2) || it.DocumentID != "conversation:s1#2#part2" {
		t.Fatalf("the successor's segment 2: %+v", it)
	}
	if BaseDocumentID("conversation:s1#2#part2") != "conversation:s1" || chainOf("conversation:s1#2#part2") != "conversation:s1#2" {
		t.Fatal("the successor's part is not read as one")
	}
	e.purge("conversation:s1#2#part2")
	if got := LiveDocumentID("s1", "billing", "billing", e.tombs()); got != "conversation:s1#3" {
		t.Fatalf("live document %s", got)
	}
	e.record("s1", "u4", "user", 180, "turn 4")
	e.clock = e.clock.Add(6 * time.Hour)
	sent := len(e.hs.retains)
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	if it := e.hs.retains[len(e.hs.retains)-1]; !isItem(it, "conversation:s1#3", "", "1") || e.sentText(sent, "turn 3") {
		t.Errorf("after the part's purge: %+v", e.hs.retains[sent:])
	}
}

// A part the server refuses (422) quarantines the session without moving
// its cursor; released, the session sends the part again, as a part.
func TestARefusedPartIsSentAgainOnceReleased(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "turn 1")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.record("s1", "u2", "user", 60, "turn 2")
	e.clock = e.clock.Add(3 * time.Hour)
	e.hs.refuse = map[string]int{"conversation:s1#part2": http.StatusUnprocessableEntity}
	before := e.sp1CursorOf("s1")
	if r := e.flush("s1", Options{}); r.Status != Quarantined || r.FailedDocument != "conversation:s1#part2" {
		t.Fatalf("%+v", r)
	}
	if c := e.sp1CursorOf("s1"); c.Offset != before.Offset || c.Segments["billing"] != 1 || c.LastUUID != "u1" {
		t.Errorf("the refused part moved the cursor: %+v", c)
	}
	e.hs.refuse = nil
	if _, err := ReleaseQuarantine(e.f.Dirs.CursorsDir(), "s1"); err != nil {
		t.Fatal(err)
	}
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	if it := e.hs.retains[len(e.hs.retains)-1]; !isPart(it, "conversation:s1", 2) {
		t.Errorf("released: %+v", it)
	}
	if n := e.hs.countIn("conversation:s1", "turn 2"); n != 1 {
		t.Errorf("turn 2 stored %d times", n)
	}
}

// sp1CursorOf is this PC's cursor of session sid.
func (e *env) sp1CursorOf(sid string) Cursor {
	e.t.Helper()
	c, ok, err := LoadCursor(e.f.Dirs.CursorsDir(), sid)
	if err != nil || !ok {
		e.t.Fatalf("cursor of %s: %v (found %v)", sid, err, ok)
	}
	return c
}

// An older binary on another PC reads the document alone: it appends its
// segment 2 to it while this PC's part 2 is there (the document's segment
// now 2, past this PC's part). Neither loses a turn: this PC goes on
// after its own part, and a resync from the document finds this PC's
// part after the appended segment; at worst a turn is stored twice.
func TestAnOlderBinarysAppendPastThisPCsPartsLosesNothing(t *testing.T) {
	a := newEnv(t)
	a.f.MachineID = "m1"
	a.turns(0, "r1")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	const doc = "conversation:sp1"
	a.turns(1, "a2")
	a.clock = a.clock.Add(3 * time.Hour)
	expect(t, a.flush("sp1", Options{}), Sent, 1, 2)
	// The older binary's append on B: D's text grows, its metadata names
	// B's segment 2.
	a.hs.mu.Lock()
	a.hs.content[doc] += "\n" + `{"role":"user","text":"turn b2"} [rec b2]`
	a.hs.docs[doc] = map[string]string{"segment": "2", "last_uuid": "b2", "machine_id": "m2", "session_id": "sp1", "project": "billing"}
	a.hs.mu.Unlock()
	a.turns(2, "a3")
	a.clock = a.clock.Add(3 * time.Hour)
	for range 3 {
		a.clock = a.clock.Add(15 * time.Minute)
		a.flush("sp1", Options{})
	}
	if it := a.hs.retains[len(a.hs.retains)-1]; it.UpdateMode != "" || chainOf(it.DocumentID) != doc || it.DocumentID == doc {
		t.Errorf("A after the older binary's append: %+v", it)
	}
	for _, u := range []string{"r1", "a2", "a3", "b2"} {
		if n := a.hs.countIn(doc, "turn "+u); n < 1 {
			t.Errorf("%s lost (stored %d times)", u, n)
		}
	}
	end, _, found, err := ChainEnd(context.Background(), func(ctx context.Context, id string) (hindsight.Document, error) {
		return a.f.Client.GetDocument(ctx, a.f.Bank, id)
	}, doc, 0)
	if err != nil || !found || end.Metadata["last_uuid"] != "a3" {
		t.Errorf("the chain read from the document ends at %v (%v %v)", end.Metadata, found, err)
	}
}
