package retain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// purge does what forget --purge does to a document of the fake server:
// its tombstone into state.json, the delete, then the mirror into the
// control bank.
func (e *env) purge(docs ...string) {
	e.t.Helper()
	var ts []state.Tombstone
	e.hs.mu.Lock()
	for _, d := range docs {
		meta, ok := e.hs.docs[d]
		if !ok {
			e.hs.mu.Unlock()
			e.t.Fatalf("no document %s to purge", d)
		}
		ts = append(ts, TombstoneOf("claude-memory", d, meta, e.clock))
		delete(e.hs.docs, d)
		delete(e.hs.content, d)
	}
	e.hs.mu.Unlock()
	var local []state.Tombstone
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.AddTombstones(ts...)
		local = s.TombstonesOf("claude-memory")
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := WriteTombstones(context.Background(), e.f.Client, "claude-memory", local); err != nil {
		e.t.Fatal(err)
	}
}

// sentText says whether any retain since from carried text.
func (e *env) sentText(from int, text string) bool {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	return slices.ContainsFunc(e.hs.retains[from:], func(it hindsight.RetainItem) bool { return strings.Contains(it.Content, text) })
}

// controlTombstones are the tombstones the fake control bank holds.
func (e *env) controlTombstones() []state.Tombstone {
	e.t.Helper()
	e.hs.mu.Lock()
	meta, ok := e.hs.docs[TombstonesDocument]
	e.hs.mu.Unlock()
	if !ok {
		return nil
	}
	ts, err := TombstonesFromMetadata("claude-memory", meta)
	if err != nil {
		e.t.Fatal(err)
	}
	return ts
}

// recordSave appends to s1 an assistant record that stores a note
// through memory_store, as Claude Code writes the tool call.
func (e *env) recordSave(uuid string, minute int, text string) {
	const sid = "s1"
	e.t.Helper()
	ts := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute).Format(time.RFC3339)
	line := fmt.Sprintf(`{"type":"assistant","uuid":%q,"sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"assistant","content":[`+
		`{"type":"text","text":"Saving it."},{"type":"tool_use","id":"tu-%s","name":"mcp__plugin_backsight_memory__memory_store","input":{"kind":"decision","text":%q}}]}}`+"\n",
		uuid, sid, e.cwd, ts, uuid, text)
	fh, err := os.OpenFile(filepath.Join(e.dir, sid+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = fh.WriteString(line)
	_ = fh.Close()
}

// purgedEnv: s3 goes out at 12:00, the backup is taken at 12:30; s1 goes
// out at 13:00 and is purged at 13:10 (a leaked secret); the server is
// restored from the backup at 13:30 (epoch 1), which brings back a control
// bank without that purge.
func purgedEnv(t *testing.T) *env {
	e := newEnv(t)
	e.record("s3", "v1", "user", 10, "kept by the backup")
	expect(t, e.flush("s3", Options{}), Sent, 1, 1)
	snap := e.hs.backup()
	e.clock = clockAt(13, 0)
	e.record("s1", "u1", "user", 30, "the deploy token is hunter2-leaked")
	e.record("s1", "a1", "assistant", 31, "Noted the hunter2 token.")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	e.clock = clockAt(13, 10)
	e.purge("conversation:s1")
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	e.clock = clockAt(13, 30)
	return e
}

// A purge made after the backup survives the restore: the reconcile does
// not take the purged session for lost, nothing of it goes out again, the
// control bank gets the purge back, and a later turn of that session is
// still sent.
func TestPurgeSurvivesARestore(t *testing.T) {
	e := purgedEnv(t)
	sent := len(e.hs.retains)
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || rec.To != 1 || slices.Contains(rec.Resend, "s1") {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)
	expect(t, e.flush("s3", Options{}), Nothing, 0, 0)
	if e.sentText(sent, "hunter2") {
		t.Fatalf("the purged turns went out again: %+v", e.hs.retains[sent:])
	}
	if ts := e.controlTombstones(); len(ts) != 1 || ts[0].Document != "conversation:s1" || ts[0].LastUUID != "a1" {
		t.Errorf("control bank after the reconcile: %+v", ts)
	}
	// later turns of the purged session still go, and only they
	e.clock = clockAt(14, 30)
	e.record("s1", "u2", "user", 60, "a later turn after the purge")
	r := e.flush("s1", Options{})
	if r.Status != Sent || r.Records != 1 || !e.sentText(sent, "a later turn after the purge") || e.sentText(sent, "hunter2") {
		t.Errorf("later turn: %+v, sent %+v", r, e.hs.retains[sent:])
	}
}

// The same restore seen by a client whose cursor is gone (its resync reads
// the server): the purged records are held, the later one goes out as a
// new document, under the successor id (never the purged one).
func TestPurgeHoldsWhenTheCursorIsGone(t *testing.T) {
	e := purgedEnv(t)
	if _, err := e.f.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, _ := cursorPath(e.f.Dirs.CursorsDir(), "s1")
	_ = os.Remove(p)
	sent := len(e.hs.retains)
	e.clock = clockAt(14, 30)
	e.record("s1", "u2", "user", 60, "a later turn after the purge")
	r := e.flush("s1", Options{})
	if r.Status != Sent || r.Records != 1 || r.Segment != 1 || e.sentText(sent, "hunter2") {
		t.Errorf("%+v, sent %+v", r, e.hs.retains[sent:])
	}
	if it := e.hs.retains[len(e.hs.retains)-1]; it.DocumentID != "conversation:s1#2" || it.UpdateMode != "" || !strings.Contains(it.Content, "REF-ID") {
		t.Errorf("not a new document: %+v", it)
	}
}

// A purged session whose cursor is lost waits for its document, then is
// quarantined, then released by retry-quarantine, which resends it whole:
// none of that may bring the purged records back.
func TestPurgeSurvivesRetryQuarantine(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "the deploy token is hunter2-leaked")
	e.record("s1", "a1", "assistant", 1, "Noted the hunter2 token.")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	e.purge("conversation:s1")
	p, _ := cursorPath(e.f.Dirs.CursorsDir(), "s1")
	_ = os.Remove(p)
	sent := len(e.hs.retains)
	for step, run := range []func() Result{
		func() Result { return e.flush("s1", Options{}) },
		func() Result { e.clock = e.clock.Add(PendingLimit); return e.flush("s1", Options{}) },
		func() Result {
			if _, err := ReleaseQuarantine(e.f.Dirs.CursorsDir(), "s1"); err != nil {
				t.Fatal(err)
			}
			return e.flush("s1", Options{})
		},
	} {
		if r := run(); r.Status == Quarantined || r.Status == Retry || e.sentText(sent, "hunter2") {
			t.Fatalf("step %d: %+v, sent %+v", step, r, e.hs.retains[sent:])
		}
	}
	// a session quarantined before the purge, released after it, too
	e.record("s1", "u2", "user", 2, "refused turn")
	e.hs.refuse = map[string]int{"conversation:s1#2": 400} // the purged id is never written again
	if r := e.flush("s1", Options{Force: true}); r.Status != Quarantined {
		t.Fatalf("%+v", r)
	}
	e.hs.refuse = nil
	if _, err := ReleaseQuarantine(e.f.Dirs.CursorsDir(), "s1"); err != nil {
		t.Fatal(err)
	}
	if r := e.flush("s1", Options{Force: true}); r.Status != Sent || r.Records != 1 || e.sentText(sent, "hunter2") || !e.sentText(sent, "refused turn") {
		t.Errorf("%+v, sent %+v", r, e.hs.retains[sent:])
	}
}

// A reinstall: no state, no cursor, no sent index, only the control bank's
// tombstones. The reconcile before backfill learns them; backfill then
// sends none of the purged records, replays none of their saves, and still
// sends the session's later records and saves.
func TestBackfillAfterAReinstallKeepsThePurge(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "the deploy token is hunter2-leaked")
	e.recordSave("a1", 1, "Decision: the deploy uses hunter2 for now.")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	e.purge("conversation:s1")
	e.record("s1", "u2", "user", 2, "a later turn after the purge")
	e.recordSave("a2", 3, "Decision: rotate the deploy token weekly.")
	// the reinstall
	if err := os.RemoveAll(e.f.Dirs.State); err != nil {
		t.Fatal(err)
	}
	e.f.Index, _ = LoadSentIndex(e.f.Dirs.SentIndexDir())
	path := filepath.Join(e.dir, "s1.jsonl")
	whole, _ := e.f.Survey(path) // before the reconcile: nothing known of the purge
	if _, err := e.f.Reconcile(context.Background()); err != nil || e.f.Hold != "" {
		t.Fatalf("reconcile: %v, hold %q", err, e.f.Hold)
	}
	if st, _ := state.Load(e.f.Dirs.StateFile()); len(st.TombstonesOf("claude-memory")) != 1 {
		t.Fatalf("tombstones not learned from the control bank: %+v", st.Tombstones)
	}
	if c, _ := e.f.Survey(path); c.Status != "send" || whole.Notes != 2 || c.Notes != 1 || c.Chars >= whole.Chars {
		t.Errorf("survey counts the purged records or saves: %+v, whole %+v", c, whole)
	}
	sent := len(e.hs.retains)
	r := e.f.FlushFile(context.Background(), path, Options{Force: true, IgnoreSince: true})
	if r.Status != Sent || r.Records != 2 || e.sentText(sent, "hunter2") || !e.sentText(sent, "a later turn after the purge") {
		t.Errorf("%+v, sent %+v", r, e.hs.retains[sent:])
	}
	_, recs := e.f.Survey(path)
	notes := e.f.Notes("s1", recs)
	if len(notes) != 1 || !strings.Contains(notes[0].Content, "rotate the deploy token") {
		t.Errorf("replayed saves: %+v", notes)
	}
}

// A replayed save whose own note document was purged is not replayed.
func TestPurgedNoteIsNotReplayed(t *testing.T) {
	e := newEnv(t)
	e.recordSave("a1", 0, "Decision: the deploy uses hunter2 for now.")
	e.recordSave("a2", 1, "Decision: rotate the deploy token weekly.")
	path := filepath.Join(e.dir, "s1.jsonl")
	_, recs := e.f.Survey(path)
	notes := e.f.Notes("s1", recs)
	if len(notes) != 2 {
		t.Fatalf("notes %+v", notes)
	}
	e.hs.docs[notes[0].DocumentID] = map[string]string{"source": "note"}
	e.purge(notes[0].DocumentID)
	if again := e.f.Notes("s1", recs); len(again) != 1 || again[0].DocumentID != notes[1].DocumentID {
		t.Errorf("replayed after the purge: %+v", again)
	}
	// the second save went out live, under the id store gives it; its
	// purge hashes its text, which the replay matches
	live := TombstoneOf("claude-memory", NewNote("claude-memory", "decision", "billing", notes[1].Content, e.clock).DocumentID, nil, e.clock)
	live.ContentSHA256 = ContentHash(notes[1].Content)
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error { s.AddTombstones(live); return nil }); err != nil {
		t.Fatal(err)
	}
	if again := e.f.Notes("s1", recs); len(again) != 0 {
		t.Errorf("a save purged live is replayed: %+v", again)
	}
}

// Positions are per document: a purge of one project's document holds
// that project's records only; a legacy document (no project in its
// metadata) held every project's, and its purge holds them all; a
// tombstone whose last record the transcript lacks holds its whole
// project.
func TestPurgedOfFollowsTheDocuments(t *testing.T) {
	e := newEnv(t)
	for i, u := range []string{"r0", "r1", "r2", "r3"} {
		e.record("s1", u, "user", i, "turn "+u)
	}
	_, recs := e.f.Survey(filepath.Join(e.dir, "s1.jsonl"))
	tomb := func(doc string, meta map[string]string) state.Tombstone { return TombstoneOf("b", doc, meta, e.clock) }
	for name, c := range map[string]struct {
		tombs []state.Tombstone
		held  map[string][]bool // project → held, record by record
	}{
		"other project's document": {[]state.Tombstone{tomb("conversation:s1@infra", map[string]string{"project": "infra", "last_uuid": "r1"})},
			map[string][]bool{"billing": {false, false, false, false}, "infra": {true, true, false, false}}},
		"home document": {[]state.Tombstone{tomb("conversation:s1", map[string]string{"project": "billing", "last_uuid": "r2"})},
			map[string][]bool{"billing": {true, true, true, false}, "infra": {false, false, false, false}}},
		"legacy document": {[]state.Tombstone{tomb("conversation:s1", map[string]string{"last_uuid": "r1"})},
			map[string][]bool{"billing": {true, true, false, false}, "infra": {true, true, false, false}}},
		"home document with a legacy part": {[]state.Tombstone{tomb("conversation:s1", map[string]string{"project": "billing", "last_uuid": "r2", "legacy_last_uuid": "r0"})},
			map[string][]bool{"billing": {true, true, true, false}, "infra": {true, false, false, false}}},
		"last record not in the transcript": {[]state.Tombstone{tomb("conversation:s1@infra", map[string]string{"project": "infra", "last_uuid": "gone"})},
			map[string][]bool{"billing": {false, false, false, false}, "infra": {true, true, true, true}}},
		"another session's document": {[]state.Tombstone{tomb("conversation:s10", map[string]string{"project": "billing", "last_uuid": "r3"})},
			map[string][]bool{"billing": {false, false, false, false}}},
	} {
		p := purgedOf("s1", recs, c.tombs)
		for project, want := range c.held {
			for i, w := range want {
				if got := p.holds(i, project); got != w {
					t.Errorf("%s: record %d of %s held %v, want %v", name, i, project, got, w)
				}
			}
		}
	}
}

// The control bank's tombstones cannot be read (a server error): the
// reconcile holds every send, as for an unreadable epoch; once they can,
// sends resume. A client in step with the control bank writes nothing.
func TestUnreadableTombstonesHoldTheSends(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first turn")
	e.hs.tombStatus = 500
	if _, err := e.f.Reconcile(context.Background()); err == nil || !strings.Contains(e.f.Hold, "purge tombstones") {
		t.Fatalf("hold %q, err %v", e.f.Hold, err)
	}
	if r := e.flush("s1", Options{}); r.Status != Retry || len(e.hs.retains) != 0 {
		t.Fatalf("sent while held: %+v", r)
	}
	e.hs.tombStatus = 0
	if _, err := e.f.Reconcile(context.Background()); err != nil || e.f.Hold != "" {
		t.Fatalf("hold %q, err %v", e.f.Hold, err)
	}
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.purge("conversation:s1")
	writes := len(e.hs.control)
	if _, err := e.f.Reconcile(context.Background()); err != nil || len(e.hs.control) != writes {
		t.Errorf("a client in step wrote to the control bank: %v, %d writes", err, len(e.hs.control)-writes)
	}
}

// A purged document's id is never written again: Hindsight 1.1.0 recreates
// a deleted document on append (measured for issue 285), and a restore
// deletes a tombstoned id again. The session's later turns go to
// conversation:<sid>#2, as a new document, then append there; once that
// one is purged too, to #3. A resync reads the live document.
func TestAPurgedDocumentIsNeverWrittenAgain(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "the deploy token is hunter2-leaked")
	e.record("s1", "a1", "assistant", 1, "Noted the hunter2 token.")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	e.purge("conversation:s1")
	sent := len(e.hs.retains)
	e.record("s1", "u2", "user", 10, "a later turn after the purge")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	it := e.hs.retains[len(e.hs.retains)-1]
	if it.DocumentID != "conversation:s1#2" || it.UpdateMode != "" || !strings.Contains(it.Content, "REF-ID: conversation:s1#2") || e.sentText(sent, "hunter2") {
		t.Fatalf("after the purge: %+v", it)
	}
	e.record("s1", "u3", "user", 20, "one more turn")
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	if it := e.hs.retains[len(e.hs.retains)-1]; it.DocumentID != "conversation:s1#2" || it.UpdateMode != "append" {
		t.Errorf("the next turn: %+v", it)
	}
	// A lost cursor resumes from the live document: nothing is sent twice.
	p, _ := cursorPath(e.f.Dirs.CursorsDir(), "s1")
	_ = os.Remove(p)
	n := len(e.hs.retains)
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)
	if len(e.hs.retains) != n {
		t.Errorf("resent after a resync: %+v", e.hs.retains[n:])
	}
	e.purge("conversation:s1#2")
	e.record("s1", "u4", "user", 30, "after the second purge")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	if it := e.hs.retains[len(e.hs.retains)-1]; it.DocumentID != "conversation:s1#3" || e.sentText(n, "one more turn") {
		t.Errorf("after the second purge: %+v", it)
	}
	for _, it := range e.hs.retains[sent:] {
		if it.DocumentID == "conversation:s1" {
			t.Errorf("the purged id was written again: %+v", it)
		}
	}
}

// LiveDocumentID skips every id a tombstone names; BaseDocumentID undoes
// it, and leaves a project whose name ends in #<word> alone.
func TestLiveDocumentID(t *testing.T) {
	tombs := []state.Tombstone{{Document: "conversation:s1@infra"}, {Document: "conversation:s1@infra#2"}}
	if got := LiveDocumentID("s1", "billing", "infra", tombs); got != "conversation:s1@infra#3" {
		t.Errorf("live %s", got)
	}
	if got := LiveDocumentID("s1", "billing", "billing", tombs); got != "conversation:s1" {
		t.Errorf("home %s", got)
	}
	for in, want := range map[string]string{"conversation:s1@infra#3": "conversation:s1@infra", "conversation:s1#2": "conversation:s1",
		"conversation:s1@c#sharp": "conversation:s1@c#sharp", "conversation:s1#1": "conversation:s1#1", "conversation:s1": "conversation:s1"} {
		if got := BaseDocumentID(in); got != want {
			t.Errorf("BaseDocumentID(%s) = %s, want %s", in, got, want)
		}
	}
}

// A reconcile on a higher epoch purges again the tombstoned documents the
// server still holds: one a queued retain recreated after the restore, or
// one a machine purged while the restore ran, whose tombstone reached the
// control bank late (issue 285 review M1). Not on an epoch already
// reconciled.
func TestReconcilePurgesAgainWhatTheServerStillHolds(t *testing.T) {
	e := purgedEnv(t)
	// The archive held conversation:s1 (a queued retain recreated it).
	e.hs.mu.Lock()
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "a1", "segment": "1", "project": "billing"}
	e.hs.mu.Unlock()
	rec, err := e.f.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.Purged, []string{"conversation:s1"}) || !slices.Equal(e.hs.deleted, []string{"conversation:s1"}) {
		t.Errorf("purged %v, deleted %v", rec.Purged, e.hs.deleted)
	}
	e.hs.mu.Lock()
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1"}
	e.hs.mu.Unlock()
	if rec, err := e.f.Reconcile(context.Background()); err != nil || len(rec.Purged) != 0 {
		t.Errorf("an epoch already reconciled purges again: %+v %v", rec, err)
	}
}
