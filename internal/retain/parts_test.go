package retain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// Decision 0090: a session's later segments are documents of their own.

// The second segment creates the document's part 2 and leaves the first
// document as it was: no append (which makes Hindsight chunk the whole
// document again and extract its tail chunk anew), the first document's
// text and metadata untouched, the part's metadata naming the segment.
func TestALaterSegmentLeavesTheFirstDocumentAlone(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "why does the export fail")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	text, meta := e.hs.content["conversation:s1"], e.hs.docs["conversation:s1"]
	e.record("s1", "u2", "user", 60, "the page size was 1000")
	r := e.flush("s1", Options{})
	expect(t, r, Sent, 1, 2)
	if it := e.hs.retains[1]; !isPart(it, "conversation:s1", 2) || strings.Contains(it.Content, "export fail") || strings.Contains(it.Content, "REF-ID") {
		t.Errorf("the second segment: %+v", it)
	}
	if e.hs.content["conversation:s1"] != text || e.hs.docs["conversation:s1"]["segment"] != "1" || e.hs.docs["conversation:s1"]["last_uuid"] != meta["last_uuid"] {
		t.Errorf("the first document changed: %q %v", e.hs.content["conversation:s1"], e.hs.docs["conversation:s1"])
	}
	if m := e.hs.docs["conversation:s1#part2"]; m["segment"] != "2" || m["last_uuid"] != "u2" || m["session_id"] != "s1" || m["project"] != "billing" {
		t.Errorf("part 2's metadata %v", m)
	}
	if !slices.Equal(r.Documents, []string{"conversation:s1#part2"}) {
		t.Errorf("documents %v: the telemetry names the document the server holds", r.Documents)
	}
	if cur, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1"); cur.Segments["billing"] != 2 || cur.Document != "conversation:s1" || cur.docOf("s1", "billing") != "conversation:s1" {
		t.Errorf("cursor %+v", cur)
	}
}

// A session an older build appended to (segments 1-3 in one document,
// its cursor at segment 3) goes on in parts after the update: segment 4
// is the document's part 4, the appended document stays as it is, and a
// resync with no cursor finds the position at the part's end.
func TestASessionAnOlderBuildAppendedToGoesOnInParts(t *testing.T) {
	e := newEnv(t)
	olderBuild = true
	t.Cleanup(func() { olderBuild = false })
	for i := range 3 {
		e.record("s1", "u"+strconv.Itoa(i+1), "user", i*60, "turn "+strconv.Itoa(i+1))
		e.clock = e.clock.Add(3 * time.Hour)
		expect(t, e.flush("s1", Options{}), Sent, 1, i+1)
	}
	if m := e.hs.docs["conversation:s1"]; m["segment"] != "3" || e.hs.countIn("conversation:s1", "turn 3") != 1 {
		t.Fatalf("not an older build's appended document: %v", m)
	}
	olderBuild = false
	appended := e.hs.content["conversation:s1"]
	e.record("s1", "u4", "user", 600, "turn 4")
	e.clock = e.clock.Add(3 * time.Hour)
	expect(t, e.flush("s1", Options{}), Sent, 1, 4)
	if it := e.hs.retains[len(e.hs.retains)-1]; !isPart(it, "conversation:s1", 4) || strings.Contains(it.Content, "turn 3") {
		t.Errorf("segment 4: %+v", it)
	}
	if e.hs.content["conversation:s1"] != appended || e.hs.docs["conversation:s1"]["segment"] != "3" {
		t.Error("the appended document was written again")
	}
	// No cursor (a new machine's restore, a lost state): the resync reads
	// the appended document, then its parts.
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	sent := len(e.hs.retains)
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)
	cur, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	if cur.Segments["billing"] != 4 || cur.LastUUIDs["billing"] != "u4" || len(e.hs.retains) != sent {
		t.Fatalf("resynced cursor %+v, %d sent", cur, len(e.hs.retains)-sent)
	}
	e.record("s1", "u5", "user", 700, "turn 5")
	e.clock = e.clock.Add(3 * time.Hour)
	expect(t, e.flush("s1", Options{}), Sent, 1, 5)
	if it := e.hs.retains[len(e.hs.retains)-1]; !isPart(it, "conversation:s1", 5) {
		t.Errorf("segment 5: %+v", it)
	}
	for i := 1; i <= 5; i++ {
		if n := e.hs.countIn("conversation:s1", "turn "+strconv.Itoa(i)); n != 1 {
			t.Errorf("turn %d stored %d times", i, n)
		}
	}
}

// Another PC with no cursor and no sent index of this session (a copied
// transcript) reads the position past a part the server has not stored
// yet (parts are documents of their own, stored in any order): it sends
// only what follows the chain's end, never the records of a later part.
func TestAResyncStepsOverOneMissingPart(t *testing.T) {
	a := newEnv(t)
	a.turns(0, "r1")
	expect(t, a.flush("sp1", Options{}), Sent, 1, 1)
	a.turns(60, "r2")
	a.clock = a.clock.Add(3 * time.Hour)
	expect(t, a.flush("sp1", Options{}), Sent, 1, 2)
	a.turns(120, "r3")
	a.clock = a.clock.Add(3 * time.Hour)
	expect(t, a.flush("sp1", Options{}), Sent, 1, 3)
	b := a.otherPC()
	a.copyTo(b)
	b.turns(180, "b4")
	b.clock = a.clock.Add(3 * time.Hour)
	b.hs.hide = map[string]bool{"conversation:sp1#part2": true}
	expect(t, b.flush("sp1", Options{}), Sent, 1, 4)
	if it := b.hs.retains[len(b.hs.retains)-1]; !isPart(it, "conversation:sp1", 4) || strings.Contains(it.Content, "turn r") {
		t.Errorf("B's part: %+v", it)
	}
	// Two missing in a row end the chain: what follows is unknown, and
	// sending is safer than losing (decision 0073, "never lose").
	c := a.otherPC()
	a.copyTo(c)
	c.hs.hide = map[string]bool{"conversation:sp1#part2": true, "conversation:sp1#part3": true}
	c.clock = b.clock
	expect(t, c.flush("sp1", Options{}), Sent, 2, 2)
}

// A crash between the send and the cursor's save: the next flush reads
// the part it sent (or, not stored yet, sends it again with the same
// operation id, which the server collapses). Nothing is stored twice,
// across an update too: a segment an older build appended is resent by
// this build under the appended segment's operation id.
func TestACrashBeforeTheCheckpointSendsNothingTwice(t *testing.T) {
	for _, c := range []struct {
		name          string
		older, hidden bool
	}{
		{"part stored", false, false},
		{"part not stored yet", false, true},
		{"an older build's append, then the update", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.record("s1", "u1", "user", 0, "turn 1")
			expect(t, e.flush("s1", Options{}), Sent, 1, 1)
			cursor := filepath.Join(e.f.Dirs.CursorsDir(), "s1.json")
			index := filepath.Join(e.f.Dirs.SentIndexDir(), "index.bin")
			savedCursor, _ := os.ReadFile(cursor)
			savedIndex, _ := os.ReadFile(index)
			e.record("s1", "u2", "user", 60, "turn 2")
			e.clock = e.clock.Add(3 * time.Hour)
			olderBuild = c.older
			expect(t, e.flush("s1", Options{}), Sent, 1, 2)
			olderBuild = false
			// The crash: neither the cursor nor the sent index were saved.
			_ = os.WriteFile(cursor, savedCursor, 0o600)
			_ = os.WriteFile(index, savedIndex, 0o600)
			idx, err := LoadSentIndex(e.f.Dirs.SentIndexDir())
			if err != nil {
				t.Fatal(err)
			}
			e.f.Index = idx
			if c.hidden {
				e.hs.hide = map[string]bool{"conversation:s1#part2": true}
			}
			e.flush("s1", Options{})
			e.hs.hide = nil
			if n := e.hs.countIn("conversation:s1", "turn 2"); n != 1 {
				t.Errorf("turn 2 stored %d times", n)
			}
			e.record("s1", "u3", "user", 120, "turn 3")
			e.clock = e.clock.Add(3 * time.Hour)
			expect(t, e.flush("s1", Options{}), Sent, 1, 3)
			if it := e.hs.retains[len(e.hs.retains)-1]; !isPart(it, "conversation:s1", 3) {
				t.Errorf("segment 3: %+v", it)
			}
			for i := 1; i <= 3; i++ {
				if n := e.hs.countIn("conversation:s1", "turn "+strconv.Itoa(i)); n != 1 {
					t.Errorf("turn %d stored %d times", i, n)
				}
			}
		})
	}
}

// A tombstone of one part (an older binary's purge deletes the one
// document a fact came from) purges the whole document: the session's
// later turns go to its successor, never to a purged chain.
func TestATombstonedPartMovesTheSessionToItsSuccessor(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "turn 1")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.record("s1", "u2", "user", 60, "turn 2 hunter2")
	e.clock = e.clock.Add(3 * time.Hour)
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	e.purge("conversation:s1#part2")
	if got := LiveDocumentID("s1", "billing", "billing", e.tombs()); got != "conversation:s1#2" {
		t.Fatalf("live document %s", got)
	}
	e.record("s1", "u3", "user", 400, "turn 3")
	e.clock = e.clock.Add(6 * time.Hour)
	sent := len(e.hs.retains)
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	if it := e.hs.retains[len(e.hs.retains)-1]; !isItem(it, "conversation:s1#2", "", "1") || e.sentText(sent, "hunter2") || e.sentText(sent, "turn 1") {
		t.Errorf("after the purge: %+v", e.hs.retains[sent:])
	}
	if _, ok := e.hs.docs["conversation:s1#part3"]; ok {
		t.Error("the purged document's chain was written again")
	}
}

// tombs are this PC's tombstones of the bank.
func (e *env) tombs() []state.Tombstone {
	e.t.Helper()
	st, err := state.Load(e.f.Dirs.StateFile())
	if err != nil {
		e.t.Fatal(err)
	}
	return st.TombstonesOf(e.f.Bank)
}

// ChainEnd reads the chain from the document, or from the part a cursor
// names, steps over one missing part and stops at two, and keeps an
// appended document's own segments.
func TestChainEnd(t *testing.T) {
	docs := map[string]string{} // id → segment
	var reads []string
	get := func(_ context.Context, id string) (hindsight.Document, error) {
		reads = append(reads, id)
		if id == "conversation:boom#part2" {
			return hindsight.Document{}, errors.New("unreachable")
		}
		seg, ok := docs[id]
		if !ok {
			return hindsight.Document{}, &hindsight.APIError{Status: 404, Kind: hindsight.NotFound}
		}
		return hindsight.Document{ID: id, Metadata: map[string]string{"segment": seg}}, nil
	}
	for _, c := range []struct {
		name    string
		docs    map[string]string
		doc     string
		from    int
		end     string
		found   bool
		reads   []string
		failing bool
	}{
		{"one document", map[string]string{"conversation:a": "1"}, "conversation:a", 0, "conversation:a", true,
			[]string{"conversation:a", "conversation:a#part2", "conversation:a#part3"}, false},
		{"one part missing", map[string]string{"conversation:a": "1", "conversation:a#part3": "3"}, "conversation:a", 0, "conversation:a#part3", true,
			[]string{"conversation:a", "conversation:a#part2", "conversation:a#part3", "conversation:a#part4", "conversation:a#part5"}, false},
		{"two missing end it", map[string]string{"conversation:a": "1", "conversation:a#part4": "4"}, "conversation:a", 0, "conversation:a", true,
			[]string{"conversation:a", "conversation:a#part2", "conversation:a#part3"}, false},
		{"an appended document", map[string]string{"conversation:a": "5", "conversation:a#part6": "6"}, "conversation:a", 0, "conversation:a#part6", true,
			[]string{"conversation:a", "conversation:a#part6", "conversation:a#part7", "conversation:a#part8"}, false},
		{"from the cursor's part", map[string]string{"conversation:a": "1", "conversation:a#part2": "2", "conversation:a#part3": "3"}, "conversation:a", 3, "conversation:a#part3", true,
			[]string{"conversation:a#part3", "conversation:a#part4", "conversation:a#part5"}, false},
		{"the cursor's part not stored yet", map[string]string{"conversation:a": "1", "conversation:a#part2": "2"}, "conversation:a", 3, "conversation:a#part2", true,
			[]string{"conversation:a#part3", "conversation:a", "conversation:a#part2", "conversation:a#part3", "conversation:a#part4"}, false},
		{"no document", map[string]string{"conversation:a#part2": "2"}, "conversation:a", 0, "conversation:a", false,
			[]string{"conversation:a"}, false},
		{"a segment that is no number", map[string]string{"conversation:a": "x", "conversation:a#part2": "2"}, "conversation:a", 0, "conversation:a", true,
			[]string{"conversation:a"}, false},
		{"a failed read", map[string]string{"conversation:boom": "1"}, "conversation:boom", 0, "", false, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			docs, reads = c.docs, nil
			end, id, found, err := ChainEnd(context.Background(), get, c.doc, c.from)
			if c.failing {
				if err == nil {
					t.Errorf("no error: %s %v", id, found)
				}
				return
			}
			if err != nil || id != c.end || found != c.found || (found && end.ID != id) || !slices.Equal(reads, c.reads) {
				t.Errorf("%s found=%v %v, reads %v; want %s found=%v, reads %v", id, found, err, reads, c.end, c.found, c.reads)
			}
		})
	}
}

// A purge takes a session's document whole: every part of it the bank
// lists, whichever one the purged fact came from; nothing of another
// session, another project's document, or a purge's successor.
func TestWholeChains(t *testing.T) {
	listed := []hindsight.DocumentItem{{ID: "conversation:s1"}, {ID: "conversation:s1#part2"}, {ID: "conversation:s1#part4"},
		{ID: "conversation:s1@beta"}, {ID: "conversation:s1@beta#part2"}, {ID: "conversation:s1#2"}, {ID: "conversation:s1#2#part2"},
		{ID: "conversation:s10#part2"}, {ID: "note:x"}, {ID: "import:a/b#part2"}}
	got := WholeChains([]string{"conversation:s1#part4", "note:y"}, listed)
	want := []string{"conversation:s1#part4", "note:y", "conversation:s1", "conversation:s1#part2"}
	if !slices.Equal(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
	if got := WholeChains([]string{"conversation:s1@beta"}, listed); !slices.Equal(got, []string{"conversation:s1@beta", "conversation:s1@beta#part2"}) {
		t.Errorf("beta: %v", got)
	}
	if HasConversation([]string{"note:x", "import:a"}) || !HasConversation([]string{"note:x", "conversation:s1"}) {
		t.Error("HasConversation")
	}
}
