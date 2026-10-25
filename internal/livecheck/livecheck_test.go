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
	return f.queued, nil
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

// transcript writes a session's transcript last written at lastWrite.
func (w *world) transcript(sid string, lastWrite time.Time) string {
	w.t.Helper()
	p := filepath.Join(w.dir, "projects", "-work-api", sid+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
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
	want(t, w.check(), Fail, "quarantined")
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
