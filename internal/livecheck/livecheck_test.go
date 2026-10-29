package livecheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/retain"
)

// fakeServer holds documents and memories in memory; every call is
// recorded so a test can tell nothing was written.
type fakeServer struct {
	docs     map[string]hindsight.Document
	memories []hindsight.Memory
	queued   int
	queueErr error
	docErr   error
	calls    []string
}

func (f *fakeServer) GetDocument(_ context.Context, bank, id string) (hindsight.Document, error) {
	f.calls = append(f.calls, "GET document "+bank+" "+id)
	if f.docErr != nil {
		return hindsight.Document{}, f.docErr
	}
	d, ok := f.docs[id]
	if !ok {
		return d, &hindsight.APIError{Status: 404, Kind: hindsight.NotFound}
	}
	return d, nil
}

func (f *fakeServer) ListMemories(_ context.Context, bank string, q hindsight.MemoryQuery, fn func(hindsight.Memory) error) error {
	f.calls = append(f.calls, "LIST "+bank+" "+q.Type+" "+q.DocumentID)
	for _, m := range f.memories {
		if (q.Type == "" || m.FactType == q.Type) && (q.DocumentID == "" || m.DocumentID == q.DocumentID) {
			if !q.UpdatedSince.IsZero() && m.UpdatedAt.Before(q.UpdatedSince) {
				continue
			}
			if err := fn(m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeServer) QueuedOperations(_ context.Context, bank string) (int, error) {
	f.calls = append(f.calls, "QUEUE "+bank)
	return f.queued, f.queueErr
}

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

type world struct {
	t       *testing.T
	dir     string
	cursors string
	files   []string
	srv     *fakeServer
}

func newWorld(t *testing.T) *world {
	dir := t.TempDir()
	return &world{t: t, dir: dir, cursors: filepath.Join(dir, "cursors"), srv: &fakeServer{docs: map[string]hindsight.Document{}}}
}

// prompt is one typed prompt: a record jobs tick sends.
const prompt = `{"type":"user","uuid":"u1","message":{"role":"user","content":"why does the export job stall?"}}` + "\n"

// transcript writes a session's transcript last written at lastWrite.
func (w *world) transcript(sid string, lastWrite time.Time) string {
	w.t.Helper()
	return w.transcriptOf(sid, prompt, lastWrite)
}

// transcriptOf writes a session's transcript holding body.
func (w *world) transcriptOf(sid, body string, lastWrite time.Time) string {
	w.t.Helper()
	p := filepath.Join(w.dir, "projects", "-work-api", sid+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		w.t.Fatal(err)
	}
	if err := os.Chtimes(p, lastWrite, lastWrite); err != nil {
		w.t.Fatal(err)
	}
	w.files = append(w.files, p)
	return p
}

// sent records that this machine sent segment seg of sid at sentAt, and
// gives the server the document, its facts (consolidated or not) and an
// observation from them.
func (w *world) sent(sid string, seg int, sentAt time.Time) {
	w.t.Helper()
	p := filepath.Join(w.dir, "projects", "-work-api", sid+".jsonl")
	fi, _ := os.Stat(p)
	c := retain.Cursor{Transcript: p, Offset: fi.Size(), LastUUID: sid + "-last", Segment: seg, Document: "conversation:" + sid, Bank: "b", SentAt: sentAt}
	if err := retain.SaveCursor(w.cursors, sid, c); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) stored(sid string, seg int) {
	w.srv.docs["conversation:"+sid] = hindsight.Document{ID: "conversation:" + sid, Metadata: map[string]string{"segment": strconv.Itoa(seg), "last_uuid": sid + "-last"}}
}

func (w *world) fact(sid, id string, consolidated bool) {
	m := hindsight.Memory{ID: id, FactType: "world", DocumentID: "conversation:" + sid, UpdatedAt: now.Add(-time.Hour)}
	if consolidated {
		m.ConsolidatedAt = "2026-10-25T11:00:00+00:00"
	}
	w.srv.memories = append(w.srv.memories, m)
}

func (w *world) observation(id string, updated time.Time, sources ...string) {
	w.srv.memories = append(w.srv.memories, hindsight.Memory{ID: id, FactType: "observation", UpdatedAt: updated, SourceMemoryIDs: sources})
}

func (w *world) check() Result {
	return Check(context.Background(), Input{Server: w.srv, Bank: "b", Transcripts: w.files, CursorsDir: w.cursors, Now: now})
}

func want(t *testing.T, r Result, status, inSummary string) {
	t.Helper()
	if r.Status != status || !strings.Contains(r.Summary, inSummary) {
		t.Fatalf("got %s %q (detail %q, fix %q), want %s with %q", r.Status, r.Summary, r.Detail, r.Fix, status, inSummary)
	}
}

func TestPassWhenTheLatestSettledSessionWasSentAndConsolidated(t *testing.T) {
	w := newWorld(t)
	w.transcript("old", now.Add(-5*time.Hour))
	w.sent("old", 1, now.Add(-4*time.Hour))
	w.transcript("s1", now.Add(-2*time.Hour))
	w.sent("s1", 2, now.Add(-90*time.Minute))
	w.stored("s1", 2)
	w.fact("s1", "f1", true)
	w.fact("s1", "f2", true)
	w.observation("o1", now.Add(-80*time.Minute), "f9", "f2")
	w.transcript("live", now.Add(-time.Minute)) // still active: not settled, not judged
	r := w.check()
	want(t, r, Pass, "s1")
	if r.Session != "s1" || r.Segment != 2 || r.ServerSegment != 2 || r.Facts != 2 || r.Observations != 1 || r.ObservedIn != "" || r.Fix != "" {
		t.Errorf("%+v", r)
	}
	for _, c := range w.srv.calls {
		if !strings.HasPrefix(c, "GET ") && !strings.HasPrefix(c, "LIST ") && !strings.HasPrefix(c, "QUEUE ") {
			t.Errorf("a write: %s", c)
		}
	}
}

func TestPendingWhileTheLatestSessionIsNotSettledOrNotSentYet(t *testing.T) {
	w := newWorld(t)
	w.transcript("live", now.Add(-10*time.Minute))
	r := w.check()
	want(t, r, Pending, "not settled")
	// Settles 20 min from now, then the next tick sends it.
	if r.WaitS < 20*60 || r.WaitS > 36*60 {
		t.Errorf("wait %d s", r.WaitS)
	}
	// Settled 10 min ago, no cursor yet: the next tick sends it.
	w = newWorld(t)
	w.transcript("s1", now.Add(-40*time.Minute))
	r = w.check()
	want(t, r, Pending, "not sent yet")
	if r.WaitS <= 0 || r.WaitS > 15*60 {
		t.Errorf("wait %d s", r.WaitS)
	}
}

func TestFailWhenASettledSessionWasNeverSent(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-3*time.Hour))
	r := w.check()
	want(t, r, Fail, "not sent")
	if !strings.Contains(r.Fix, "backsight jobs tick") || !strings.Contains(r.Fix, "backsight doctor") {
		t.Errorf("fix %q", r.Fix)
	}
}

func TestPendingWhileTheServerHasNotStoredOrConsolidatedIt(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-time.Hour))
	w.sent("s1", 1, now.Add(-10*time.Minute))
	w.srv.queued = 3
	want(t, w.check(), Pending, "not stored")

	w.stored("s1", 1)
	want(t, w.check(), Pending, "extract")

	w.fact("s1", "f1", true)
	w.fact("s1", "f2", false)
	r := w.check()
	want(t, r, Pending, "consolidat")
	if r.WaitS <= 0 || r.Facts != 2 {
		t.Errorf("%+v", r)
	}
	// Nothing queued but sent minutes ago: consolidation still gets time.
	w.srv.queued = 0
	want(t, w.check(), Pending, "consolidat")
}

func TestFailWhenTheServerLostOrDiffersFromWhatWasSent(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-3*time.Hour))
	w.sent("s1", 2, now.Add(-2*time.Hour))
	r := w.check()
	want(t, r, Fail, "no document")
	if !strings.Contains(r.Fix, "backsight doctor") {
		t.Errorf("fix %q", r.Fix)
	}
	w.stored("s1", 1)
	want(t, w.check(), Fail, "segment 1")
	w.stored("s1", 2)
	w.srv.docs["conversation:s1"].Metadata["last_uuid"] = "someone-else"
	want(t, w.check(), Fail, "does not match")
	w.srv.docErr = hindsight.ErrUnreachable
	r = w.check()
	want(t, r, Fail, "cannot read")
	if !strings.Contains(r.Fix, "backsight server status") {
		t.Errorf("fix %q", r.Fix)
	}
}

func TestFailWhenConsolidationFailedOrStalled(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-5*time.Hour))
	w.sent("s1", 1, now.Add(-4*time.Hour))
	w.stored("s1", 1)
	w.fact("s1", "f1", false)
	want(t, w.check(), Fail, "not consolidated")
	w.srv.memories[0].ConsolidationFailedAt = "2026-10-25T09:00:00+00:00"
	r := w.check()
	want(t, r, Fail, "consolidation failed")
	if !strings.Contains(r.Fix, "backsight server llm check") {
		t.Errorf("fix %q", r.Fix)
	}
}

// A session whose facts formed no new observation (transient, or already
// known) is not a failure while an earlier one of this machine's did.
func TestAnEarlierSessionCanProveConsolidation(t *testing.T) {
	w := newWorld(t)
	w.transcript("s0", now.Add(-6*time.Hour))
	w.sent("s0", 1, now.Add(-5*time.Hour))
	w.stored("s0", 1)
	w.fact("s0", "f0", true)
	w.observation("o0", now.Add(-5*time.Hour+time.Minute), "f0")
	w.transcript("s1", now.Add(-2*time.Hour))
	w.sent("s1", 1, now.Add(-90*time.Minute))
	w.stored("s1", 1)
	w.fact("s1", "f1", true)
	r := w.check()
	want(t, r, Pass, "s1")
	if r.ObservedIn != "s0" || !strings.Contains(r.Detail, "s0") {
		t.Errorf("%+v", r)
	}
	// None of them did: fail.
	w.srv.memories = nil
	w.fact("s0", "f0", true)
	w.fact("s1", "f1", true)
	r = w.check()
	want(t, r, Fail, "no observation")
	if !strings.Contains(r.Fix, "backsight") {
		t.Errorf("fix %q", r.Fix)
	}
}

func TestSessionsLeftToBackfillOrOptedOutAreSkipped(t *testing.T) {
	w := newWorld(t)
	w.transcript("s0", now.Add(-6*time.Hour))
	w.sent("s0", 1, now.Add(-5*time.Hour))
	w.stored("s0", 1)
	w.fact("s0", "f0", true)
	w.observation("o0", now.Add(-5*time.Hour+time.Minute), "f0")
	// A newer session opted out: its cursor moved to the end, nothing sent.
	p := w.transcript("private", now.Add(-2*time.Hour))
	fi, _ := os.Stat(p)
	_ = retain.SaveCursor(w.cursors, "private", retain.Cursor{Transcript: p, Offset: fi.Size(), Bank: "b"})
	// A newer one left to backfill.
	w.transcript("bf", now.Add(-time.Hour))
	_ = retain.SaveCursor(w.cursors, "bf", retain.Cursor{Bank: "b", Backfill: true})
	want(t, w.check(), Pass, "s0")
}

func TestAQuarantinedLatestSessionFails(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-2*time.Hour))
	_ = retain.SaveCursor(w.cursors, "s1", retain.Cursor{Bank: "b", Quarantined: true})
	r := w.check()
	want(t, r, Fail, "quarantined")
	// A later segment refused after earlier ones went out: still held.
	_ = retain.SaveCursor(w.cursors, "s1", retain.Cursor{Bank: "b", Segment: 2, Document: "conversation:s1", Quarantined: true})
	w.stored("s1", 2)
	r = w.check()
	want(t, r, Fail, "quarantined")
	if !strings.Contains(r.Fix, "backsight retain retry-quarantine") {
		t.Errorf("fix %q", r.Fix)
	}
}

func TestNothingToCheckIsPending(t *testing.T) {
	w := newWorld(t)
	r := w.check()
	want(t, r, Pending, "no session")
	if r.WaitS <= 0 {
		t.Errorf("%+v", r)
	}
	// Sessions older than retain_since belong to backfill.
	w.transcript("old", now.Add(-48*time.Hour))
	r = Check(context.Background(), Input{Server: w.srv, Bank: "b", Transcripts: w.files, CursorsDir: w.cursors, Now: now, RetainSince: now.Add(-24 * time.Hour)})
	want(t, r, Pending, "no session")
}

func TestServerErrorsWhileListingFail(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-2*time.Hour))
	w.sent("s1", 1, now.Add(-90*time.Minute))
	w.stored("s1", 1)
	srv := &erroringList{fakeServer: w.srv}
	r := Check(context.Background(), Input{Server: srv, Bank: "b", Transcripts: w.files, CursorsDir: w.cursors, Now: now})
	want(t, r, Fail, "cannot read")
}

type erroringList struct{ *fakeServer }

func (e *erroringList) ListMemories(context.Context, string, hindsight.MemoryQuery, func(hindsight.Memory) error) error {
	return errors.New("500 internal error")
}

// sentTwo records that this machine sent sid's records of two projects:
// alpha (the home, conversation:<sid>, segment 2) and beta (conversation:
// <sid>@beta, segment 1), each with its own last record; the server holds
// both as sent, with a consolidated fact each.
func (w *world) sentTwo(sid string, sentAt time.Time) {
	w.t.Helper()
	p := filepath.Join(w.dir, "projects", "-work-api", sid+".jsonl")
	fi, _ := os.Stat(p)
	c := retain.Cursor{Transcript: p, Offset: fi.Size(), LastUUID: "u9", Segment: 2, Document: "conversation:" + sid, Bank: "b", SentAt: sentAt,
		Home: "alpha", Segments: map[string]int{"alpha": 2, "beta": 1}, LastUUIDs: map[string]string{"alpha": "u3", "beta": "u9"}}
	if err := retain.SaveCursor(w.cursors, sid, c); err != nil {
		w.t.Fatal(err)
	}
	w.srv.docs["conversation:"+sid] = hindsight.Document{ID: "conversation:" + sid, Metadata: map[string]string{"segment": "2", "last_uuid": "u3"}}
	w.srv.docs["conversation:"+sid+"@beta"] = hindsight.Document{ID: "conversation:" + sid + "@beta", Metadata: map[string]string{"segment": "1", "last_uuid": "u9"}}
	w.srv.memories = append(w.srv.memories,
		hindsight.Memory{ID: "fa", FactType: "world", DocumentID: "conversation:" + sid, ConsolidatedAt: "2026-10-25T11:00:00+00:00"},
		hindsight.Memory{ID: "fb", FactType: "world", DocumentID: "conversation:" + sid + "@beta", ConsolidatedAt: "2026-10-25T11:00:00+00:00"})
}

// A session with one document per project passes when every document
// holds what this machine sent to it, and its facts are the documents'.
func TestPassWhenEveryDocumentOfTheSessionMatches(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-2*time.Hour))
	w.sentTwo("s1", now.Add(-90*time.Minute))
	w.observation("o1", now.Add(-80*time.Minute), "fb")
	r := w.check()
	want(t, r, Pass, "conversation:s1@beta")
	if r.Document != "conversation:s1" || r.Segment != 2 || r.ServerSegment != 2 || r.Facts != 2 || r.Observations != 1 {
		t.Errorf("%+v", r)
	}
}

// A document that is not the last one sent but does not match fails,
// naming it, as does a missing one with nothing queued.
func TestFailWhenAnyDocumentOfTheSessionDiffers(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-5*time.Hour))
	w.sentTwo("s1", now.Add(-4*time.Hour))
	w.observation("o1", now.Add(-3*time.Hour), "fa")
	w.srv.docs["conversation:s1"].Metadata["last_uuid"] = "u2"
	r := w.check()
	want(t, r, Fail, "conversation:s1 does not match")
	if r.Document != "conversation:s1" {
		t.Errorf("%+v", r)
	}
	w.srv.docs["conversation:s1"].Metadata["last_uuid"] = "u3"
	delete(w.srv.docs, "conversation:s1@beta")
	if r = w.check(); r.Status != Fail || !strings.Contains(r.Summary, "no document conversation:s1@beta") || r.Document != "conversation:s1@beta" {
		t.Errorf("%+v", r)
	}
}

// called reports whether the server was asked for its queue.
func (f *fakeServer) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// Sent long ago (no grace left): the server's queue decides between
// pending and fail.
func TestTheServersQueueDecidesOnceTheSendIsOld(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-3*time.Hour))
	w.sent("s1", 2, now.Add(-2*time.Hour))
	w.srv.queued = 3
	want(t, w.check(), Pending, "not stored")
	if !w.srv.called("QUEUE b") {
		t.Errorf("queue not asked: %q", w.srv.calls)
	}
	w.srv.queued = 0
	want(t, w.check(), Fail, "no document")

	w.srv.queued = 3
	w.stored("s1", 1) // the server is still on an earlier segment
	want(t, w.check(), Pending, "segment 2 is still being stored")
	w.srv.queued = 0
	want(t, w.check(), Fail, "this machine sent segment 2")

	w.srv.queued = 3
	w.stored("s1", 2)
	want(t, w.check(), Pending, "extract")
	w.fact("s1", "f1", false)
	want(t, w.check(), Pending, "not consolidated yet")
	w.srv.queued = 0
	r := w.check()
	want(t, r, Fail, "not consolidated")
	if !strings.Contains(r.Detail, "nothing is queued") {
		t.Errorf("detail %q", r.Detail)
	}
}

// Sent minutes ago: a server segment behind is still on its way, one
// ahead of what this machine sent is not.
func TestASegmentBehindIsPendingOnlyWhileRecent(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-time.Hour))
	w.sent("s1", 2, now.Add(-10*time.Minute))
	w.stored("s1", 1)
	want(t, w.check(), Pending, "segment 2 is still being stored")
	w.stored("s1", 3)
	want(t, w.check(), Fail, "server holds segment 3")
}

// A queue that cannot be read never reads as "nothing is queued": the
// check stays pending and says why (0067 block 4 review rules).
func TestAnUnreadableQueueIsPendingNeverNothingQueued(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-5*time.Hour))
	w.sent("s1", 1, now.Add(-4*time.Hour))
	w.srv.queueErr = errors.New("502 bad gateway")
	for _, c := range []struct {
		name  string
		setup func()
	}{
		{"document missing", func() {}},
		{"facts unconsolidated", func() {
			w.stored("s1", 1)
			w.fact("s1", "f1", false)
		}},
	} {
		name := c.name
		c.setup()
		r := w.check()
		want(t, r, Pending, "could not read the server's queue")
		if strings.Contains(r.Summary+r.Detail, "nothing is queued") || strings.Contains(r.Summary, "0 operation") || !strings.Contains(r.Detail, "502") || r.WaitS <= 0 {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// No fact extracted yet, queue unknown: pending too, not a verdict on
	// consolidation.
	w.srv.memories = nil
	want(t, w.check(), Pending, "could not read the server's queue")
}

// A transcript with nothing jobs tick would send (only slash or ! commands)
// is skipped: flush saves no cursor for it, so it is never "not sent".
func TestATranscriptWithNothingToSendIsSkipped(t *testing.T) {
	commands := `{"type":"user","uuid":"c1","message":{"role":"user","content":"<command-name>/clear</command-name>\n<command-message>clear</command-message>"}}` + "\n" +
		`{"type":"user","uuid":"c2","message":{"role":"user","content":"<bash-input>ls</bash-input>"}}` + "\n" +
		`{"type":"user","uuid":"c3","message":{"role":"user","content":"<bash-stdout>a b</bash-stdout><bash-stderr></bash-stderr>"}}` + "\n"
	w := newWorld(t)
	w.transcript("s0", now.Add(-6*time.Hour))
	w.sent("s0", 1, now.Add(-5*time.Hour))
	w.stored("s0", 1)
	w.fact("s0", "f0", true)
	w.observation("o0", now.Add(-5*time.Hour+time.Minute), "f0")
	w.transcriptOf("cmds", commands, now.Add(-3*time.Hour)) // settled, newer, no cursor
	w.transcriptOf("live-cmds", commands, now.Add(-time.Minute))
	r := w.check()
	want(t, r, Pass, "s0")
	// Alone, it leaves nothing to check.
	w = newWorld(t)
	w.transcriptOf("cmds", commands, now.Add(-3*time.Hour))
	want(t, w.check(), Pending, "no session")
	// Nor is a live one of only commands the session to wait for.
	w.transcriptOf("live-cmds", commands, now.Add(-time.Minute))
	want(t, w.check(), Pending, "no session")
	w.transcript("live", now.Add(-2*time.Minute))
	want(t, w.check(), Pending, "the latest session live is not settled")
}

// At most 5 earlier sessions may prove consolidation.
func TestTheEarlierProofLooksBackFiveSessions(t *testing.T) {
	build := func(withObservationIn int) *world {
		w := newWorld(t)
		for i := 0; i <= 6; i++ { // s0 newest … s6 oldest
			sid := "s" + strconv.Itoa(i)
			at := now.Add(-time.Duration(2+i) * time.Hour)
			w.transcript(sid, at)
			w.sent(sid, 1, at.Add(10*time.Minute))
			w.stored(sid, 1)
			w.fact(sid, "f"+sid, true)
			if i == withObservationIn {
				w.observation("o"+sid, at.Add(20*time.Minute), "f"+sid)
			}
		}
		return w
	}
	r := build(5).check()
	want(t, r, Pass, "s0")
	if r.ObservedIn != "s5" {
		t.Errorf("%+v", r)
	}
	r = build(6).check()
	want(t, r, Fail, "last 6 sent session(s)")
}

// A cursor that cannot be read: the latest settled session's fails the
// check; an earlier one's is only left out of the proof.
func TestACorruptCursor(t *testing.T) {
	w := newWorld(t)
	w.transcript("s1", now.Add(-2*time.Hour))
	if err := os.MkdirAll(w.cursors, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(w.cursors, "s1.json"), []byte("{not json"), 0o600)
	r := w.check()
	want(t, r, Fail, "cannot read the cursor of session s1")
	if r.Fix != "backsight doctor" || r.Detail == "" {
		t.Errorf("%+v", r)
	}
	w.transcript("s2", now.Add(-time.Hour))
	w.sent("s2", 1, now.Add(-50*time.Minute))
	w.stored("s2", 1)
	w.fact("s2", "f2", true)
	w.observation("o2", now.Add(-40*time.Minute), "f2")
	want(t, w.check(), Pass, "s2")
}

// A session sent to another bank was not sent to this one: once the
// grace is over, it fails as not sent.
func TestASessionSentToAnotherBankIsNotSentHere(t *testing.T) {
	w := newWorld(t)
	p := w.transcript("s1", now.Add(-3*time.Hour))
	fi, _ := os.Stat(p)
	_ = retain.SaveCursor(w.cursors, "s1", retain.Cursor{Transcript: p, Offset: fi.Size(), LastUUID: "u1", Segment: 1, Document: "conversation:s1", Bank: "other", SentAt: now.Add(-2 * time.Hour)})
	w.stored("s1", 1)
	want(t, w.check(), Fail, "not sent")
	if w.srv.called("GET ") {
		t.Errorf("read another bank's document: %q", w.srv.calls)
	}
}

// continued: this PC sent segment 2 of s ending at s-a5; the server holds
// the segment and machine id of doc, with s's facts consolidated and one
// observation from them.
func (w *world) continued(doc map[string]string) {
	w.t.Helper()
	p := w.transcript("s", now.Add(-2*time.Hour))
	fi, _ := os.Stat(p)
	c := retain.Cursor{Transcript: p, Offset: fi.Size(), LastUUID: "s-a5", Segment: 2, Document: "conversation:s", Bank: "b", SentAt: now.Add(-90 * time.Minute)}
	if err := retain.SaveCursor(w.cursors, "s", c); err != nil {
		w.t.Fatal(err)
	}
	w.srv.docs["conversation:s"] = hindsight.Document{ID: "conversation:s", Metadata: doc}
	w.fact("s", "f1", true)
	w.observation("o1", now.Add(-80*time.Minute), "f1")
}

func (w *world) checkAs(machineID string) Result {
	return Check(context.Background(), Input{Server: w.srv, Bank: "b", Transcripts: w.files, CursorsDir: w.cursors, Now: now, MachineID: machineID})
}

// A session copied to another PC and continued there: the other PC sent
// segment 3, which this PC's cursor does not know of. The document
// matches, and the check goes on to its facts.
func TestLiveCheckPassesADocumentAnotherPCContinued(t *testing.T) {
	w := newWorld(t)
	w.continued(map[string]string{"segment": "3", "last_uuid": "s-b5", "machine_id": "other"})
	r := w.checkAs("this-pc")
	want(t, r, Pass, "continued on another PC: conversation:s")
	if len(r.ContinuedElsewhere) != 1 || r.ContinuedElsewhere[0] != "conversation:s" {
		t.Errorf("continued elsewhere %q", r.ContinuedElsewhere)
	}
	if r.Segment != 2 || r.ServerSegment != 3 || r.Facts != 1 || r.Observations != 1 {
		t.Errorf("%+v", r)
	}
	// The facts are still judged: unconsolidated long after the send fails.
	w.srv.memories[0].ConsolidatedAt = ""
	want(t, w.checkAs("this-pc"), Fail, "not consolidated")
}

// Everything but a later segment another PC wrote still fails, with the
// summary naming the segment or the last record as before.
func TestLiveCheckStillFailsWhatThisPCWrote(t *testing.T) {
	for _, tc := range []struct {
		name, machineID, inSummary string
		doc                        map[string]string
	}{
		{"a later segment this PC wrote (a lost cursor)", "this-pc", "the server holds segment 3 of conversation:s, this machine sent segment 2",
			map[string]string{"segment": "3", "last_uuid": "s-b5", "machine_id": "this-pc"}},
		{"a later segment without machine_id", "this-pc", "the server holds segment 3 of conversation:s, this machine sent segment 2",
			map[string]string{"segment": "3", "last_uuid": "s-b5"}},
		{"this PC's machine id unknown", "", "the server holds segment 3 of conversation:s, this machine sent segment 2",
			map[string]string{"segment": "3", "last_uuid": "s-b5", "machine_id": "other"}},
		{"an earlier segment from another PC", "this-pc", "the server holds segment 1 of conversation:s, this machine sent segment 2",
			map[string]string{"segment": "1", "last_uuid": "s-b5", "machine_id": "other"}},
		{"the same segment from another PC ending elsewhere", "this-pc", "does not match what this machine sent",
			map[string]string{"segment": "2", "last_uuid": "s-b5", "machine_id": "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.continued(tc.doc)
			r := w.checkAs(tc.machineID)
			want(t, r, Fail, tc.inSummary)
			if len(r.ContinuedElsewhere) != 0 || strings.Contains(r.Summary, "another PC") {
				t.Errorf("%+v", r)
			}
		})
	}
}
