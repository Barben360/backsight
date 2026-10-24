package retain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/state"
)

// fakeHS stores retained documents like Hindsight: append concatenates
// content and replaces metadata (E21).
type fakeHS struct {
	mu      sync.Mutex
	docs    map[string]map[string]string
	content map[string]string
	retains []hindsight.RetainItem
	ops     []string
	status  int // non-zero: answer retains with this status
}

func (h *fakeHS) handler(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories"):
		if h.status != 0 {
			w.WriteHeader(h.status)
			_, _ = w.Write([]byte(`{"detail":"refused"}`))
			return
		}
		var body struct {
			Items       []hindsight.RetainItem `json:"items"`
			OperationID string                 `json:"operation_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, it := range body.Items {
			h.retains = append(h.retains, it)
			h.docs[it.DocumentID] = it.Metadata
			if it.UpdateMode == "append" {
				h.content[it.DocumentID] += "\n" + it.Content
			} else {
				h.content[it.DocumentID] = it.Content
			}
		}
		h.ops = append(h.ops, body.OperationID)
		_, _ = w.Write([]byte(`{"success":true}`))
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/documents/"):
		id, _ := url.PathUnescape(r.URL.EscapedPath()[strings.Index(r.URL.EscapedPath(), "/documents/")+len("/documents/"):])
		meta, ok := h.docs[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := json.Marshal(map[string]any{"id": id, "document_metadata": meta})
		_, _ = w.Write(raw)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type env struct {
	t     *testing.T
	hs    *fakeHS
	f     *Flusher
	dir   string // transcripts
	cwd   string
	clock time.Time
}

func newEnv(t *testing.T) *env {
	hs := &fakeHS{docs: map[string]map[string]string{}, content: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(hs.handler))
	t.Cleanup(srv.Close)
	c, _ := hindsight.New(srv.URL, "k", 2*time.Second)
	root := t.TempDir()
	d := paths.Dirs{State: filepath.Join(root, "state")}
	idx, _ := LoadSentIndex(d.SentIndexDir())
	e := &env{t: t, hs: hs, dir: filepath.Join(root, "projects", "p"), cwd: filepath.Join(root, "home", "work", "billing"),
		clock: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	_ = os.MkdirAll(e.dir, 0o700)
	_ = os.MkdirAll(e.cwd, 0o700)
	e.f = &Flusher{Client: c, Bank: "claude-memory", Dirs: d, Resolver: project.New(filepath.Join(root, "home"), []string{".claude"}),
		MachineID: "m1", Version: "test", Now: func() time.Time { return e.clock }, Index: idx}
	return e
}

// record appends a user or assistant record at minutes after 09:00.
func (e *env) record(sid, uuid, role string, minute int, text string) {
	e.t.Helper()
	e.recordIn(e.cwd, sid, uuid, role, minute, text)
}

func (e *env) recordIn(cwd, sid, uuid, role string, minute int, text string) {
	e.t.Helper()
	ts := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute).Format(time.RFC3339)
	content := fmt.Sprintf("%q", text)
	if role == "assistant" {
		content = fmt.Sprintf(`[{"type":"text","text":%q}]`, text)
	}
	line := fmt.Sprintf(`{"type":%q,"uuid":%q,"sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":%q,"content":%s}}`+"\n",
		role, uuid, sid, cwd, ts, role, content)
	fh, err := os.OpenFile(filepath.Join(e.dir, sid+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = fh.WriteString(line)
	_ = fh.Close()
}

func (e *env) flush(sid string, opt Options) Result {
	return e.f.FlushFile(context.Background(), filepath.Join(e.dir, sid+".jsonl"), opt)
}

func expect(t *testing.T, r Result, status string, records, segment int) {
	t.Helper()
	if r.Status != status || r.Records != records || r.Segment != segment {
		t.Errorf("got %s records=%d segment=%d (%s), want %s records=%d segment=%d", r.Status, r.Records, r.Segment, r.Detail, status, records, segment)
	}
}

func TestSettledSessionIsSentOnce(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "Why does the export fail?")
	e.record("s1", "a1", "assistant", 1, "The page size was 1000.")
	r := e.flush("s1", Options{})
	expect(t, r, Sent, 2, 1)
	if r.Project != "billing" || len(e.hs.retains) != 1 || e.hs.retains[0].Tags[0] != "project:billing" {
		t.Errorf("%+v %+v", r, e.hs.retains)
	}
	if !e.f.Index.Has("u1") || !e.f.Index.Has("a1") {
		t.Error("index not updated")
	}
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)
	if len(e.hs.retains) != 1 {
		t.Errorf("sent twice: %d", len(e.hs.retains))
	}
}

func TestRecentSessionWaits(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 170, "still talking") // 11:50, 10 min before the clock
	expect(t, e.flush("s1", Options{}), Waiting, 0, 0)
	if len(e.hs.retains) != 0 {
		t.Error("an unsettled session was sent")
	}
	expect(t, e.flush("s1", Options{Force: true}), Sent, 1, 1)
}

// An open session sends records older than 2 h and keeps the rest.
func TestLongOpenSessionSendsItsOldPart(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "old question")     // 09:00
	e.record("s1", "a1", "assistant", 5, "old answer")  // 09:05
	e.record("s1", "u2", "user", 170, "fresh question") // 11:50
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	if strings.Contains(e.hs.retains[0].Content, "fresh question") {
		t.Error("the recent turn was sent")
	}
	e.clock = e.clock.Add(time.Hour)
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	if e.hs.retains[1].UpdateMode != "append" || !strings.Contains(e.hs.retains[1].Content, "fresh question") {
		t.Errorf("%+v", e.hs.retains[1])
	}
}

// claude --resume appends to the same file: only the new turns go out.
func TestResumedSessionAppendsOnlyNewTurns(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	e.record("s1", "a1", "assistant", 1, "reply")
	e.flush("s1", Options{})
	e.record("s1", "u2", "user", 60, "resumed later")
	r := e.flush("s1", Options{})
	expect(t, r, Sent, 1, 2)
	if strings.Contains(e.hs.retains[1].Content, "first") || strings.Contains(e.hs.retains[1].Content, "REF-ID") {
		t.Errorf("resent old turns: %s", e.hs.retains[1].Content)
	}
}

// A new machine, a restored state or a lost cursor resumes from the server.
func TestLostCursorResumesFromServer(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	e.record("s1", "a1", "assistant", 1, "reply")
	e.flush("s1", Options{})
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	e.f.Index, _ = LoadSentIndex(t.TempDir()) // the other machine has no index either
	e.record("s1", "u2", "user", 60, "new turn")
	r := e.flush("s1", Options{})
	expect(t, r, Sent, 1, 2)
	if strings.Contains(e.hs.retains[1].Content, "first") {
		t.Error("a session was sent twice to a shared server")
	}
}

func TestUnknownServerPositionQuarantines(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	e.hs.docs["conversation:s1"] = map[string]string{"last_uuid": "not-in-this-file", "segment": "4"}
	r := e.flush("s1", Options{})
	if r.Status != Quarantined || len(e.hs.retains) != 0 {
		t.Fatalf("%+v", r)
	}
	if raw, err := os.ReadFile(filepath.Join(e.f.Dirs.QuarantineDir(), "s1.jsonl")); err != nil || !strings.Contains(string(raw), "not-in-this-file") {
		t.Errorf("quarantine file: %v %s", err, raw)
	}
	if r := e.flush("s1", Options{}); r.Status != Held {
		t.Errorf("a quarantined session must stay skipped (held): %+v", r)
	}
}

// claude --fork-session copies the parent's records with their uuids.
func TestForkSendsOnlyItsOwnTurns(t *testing.T) {
	e := newEnv(t)
	e.record("parent", "u1", "user", 0, "shared question")
	e.record("parent", "a1", "assistant", 1, "shared answer")
	e.flush("parent", Options{})
	e.record("fork", "u1", "user", 0, "shared question")
	e.record("fork", "a1", "assistant", 1, "shared answer")
	e.record("fork", "u9", "user", 30, "fork's own question")
	r := e.flush("fork", Options{})
	expect(t, r, Sent, 1, 1)
	got := e.hs.retains[1].Content
	if strings.Contains(got, "shared answer") || !strings.Contains(got, "fork's own question") || e.hs.retains[1].DocumentID != "conversation:fork" {
		t.Errorf("%s", got)
	}
}

func TestOptedOutSessionAdvancesWithoutSending(t *testing.T) {
	e := newEnv(t)
	e.f.Exclude = []string{e.cwd}
	e.record("s1", "u1", "user", 0, "private client work")
	if r := e.flush("s1", Options{}); r.Status != OptedOut || len(e.hs.retains) != 0 {
		t.Fatalf("%+v", r)
	}
	e.f.Exclude = nil
	e.record("s1", "u2", "user", 60, "later turn")
	r := e.flush("s1", Options{})
	if r.Status != Sent || strings.Contains(e.hs.retains[0].Content, "private client work") {
		t.Errorf("the opted-out part leaked: %+v %s", r, e.hs.retains)
	}
}

func TestPausedSessionIsNotSent(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(e.f.Dirs.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.SessionsDir(), "s1.json"), []byte(`{"paused":true}`), 0o600)
	e.record("s1", "u1", "user", 0, "paused")
	if r := e.flush("s1", Options{}); r.Status != OptedOut || len(e.hs.retains) != 0 {
		t.Errorf("%+v", r)
	}
}

func TestSessionsBeforeRetainSinceBelongToBackfill(t *testing.T) {
	e := newEnv(t)
	e.f.RetainSince = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	e.record("s1", "u1", "user", 0, "09:00, before live retain")
	if r := e.flush("s1", Options{}); r.Status != Backfill || len(e.hs.retains) != 0 {
		t.Errorf("%+v", r)
	}
	if r := e.flush("s1", Options{IgnoreSince: true}); r.Status != Sent {
		t.Errorf("an explicitly named session is sent: %+v", r)
	}
}

func TestServerErrors(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "q")
	e.hs.status = http.StatusServiceUnavailable
	if r := e.flush("s1", Options{}); r.Status != Retry {
		t.Errorf("503: %+v", r)
	}
	e.hs.status = http.StatusUnprocessableEntity
	if r := e.flush("s1", Options{}); r.Status != Quarantined || strings.Contains(r.Detail, "refused") || !strings.Contains(r.Detail, "HTTP 422") {
		t.Errorf("422: %+v (the server body must not be kept)", r)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.f.Dirs.QuarantineDir(), "s1.jsonl")); strings.Contains(string(raw), "refused") {
		t.Errorf("quarantine file kept the server body: %s", raw)
	}
	e.hs.status = 0
	if r := e.flush("s1", Options{}); r.Status != Held {
		t.Errorf("stays held: %+v", r)
	}
}

func TestRetryKeepsTheCursor(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "q")
	e.hs.status = http.StatusTooManyRequests
	e.flush("s1", Options{})
	e.hs.status = 0
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
}

func TestLocks(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "q")
	l, err := state.TryAcquire(filepath.Join(e.f.Dirs.LocksDir(), "session-s1.lock"), "other flusher")
	if err != nil {
		t.Fatal(err)
	}
	if r := e.flush("s1", Options{}); r.Status != Locked {
		t.Errorf("held: %+v", r)
	}
	e.clock = time.Now().Add(lockStaleness + time.Minute) // the lock is now older than 10 min
	if r := e.flush("s1", Options{Force: true}); r.Status != Sent {
		t.Errorf("a lock older than 10 min is taken over: %+v", r)
	}
	_ = l.Release()

	_ = os.MkdirAll(filepath.Dir(e.f.Dirs.MaintenanceLock()), 0o700)
	_ = os.WriteFile(e.f.Dirs.MaintenanceLock(), []byte(`{"pid":1,"command":"backup create"}`), 0o600)
	e.record("s1", "u2", "user", 60, "q2")
	if r := e.flush("s1", Options{Force: true}); r.Status != Maintenance {
		t.Errorf("maintenance: %+v", r)
	}
}

// A rewritten transcript (fingerprint mismatch) resyncs from the server.
func TestRewrittenTranscriptResyncs(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	e.flush("s1", Options{})
	p := filepath.Join(e.dir, "s1.jsonl")
	raw, _ := os.ReadFile(p)
	_ = os.WriteFile(p, append([]byte(`{"type":"mode","sessionId":"s1"}`+"\n"), raw...), 0o600)
	e.record("s1", "u2", "user", 60, "second")
	r := e.flush("s1", Options{})
	expect(t, r, Sent, 1, 2)
	if strings.Contains(e.hs.retains[1].Content, "first") {
		t.Error("resent after a rewrite")
	}
}

// A cd into an excluded path mid-session: those turns are never sent.
func TestCwdChangeIntoExcludedPathMidSession(t *testing.T) {
	e := newEnv(t)
	secret := filepath.Join(filepath.Dir(e.cwd), "secret-client")
	_ = os.MkdirAll(secret, 0o700)
	e.f.Exclude = []string{secret}
	e.record("s1", "u1", "user", 0, "billing work")
	e.recordIn(secret, "s1", "u2", "user", 1, "confidential client detail")
	e.record("s1", "u3", "user", 2, "back to billing")
	r := e.flush("s1", Options{})
	if r.Status != Sent || r.Records != 2 || len(e.hs.retains) != 2 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
	for _, it := range e.hs.retains {
		if strings.Contains(it.Content, "confidential") {
			t.Errorf("an excluded turn was sent: %s", it.Content)
		}
	}
	if e.hs.retains[1].UpdateMode != "append" || e.hs.retains[1].Metadata["segment"] != "2" {
		t.Errorf("the run after the excluded part must append: %+v", e.hs.retains[1])
	}
}

// A session moving to another project sends one segment per project.
func TestProjectChangeMidSessionSplitsSegments(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(filepath.Dir(e.cwd), "infra")
	_ = os.MkdirAll(other, 0o700)
	e.record("s1", "u1", "user", 0, "billing question")
	e.recordIn(other, "s1", "u2", "user", 1, "infra question")
	e.flush("s1", Options{})
	if len(e.hs.retains) != 2 || e.hs.retains[0].Tags[0] != "project:billing" || e.hs.retains[1].Tags[0] != "project:infra" {
		t.Errorf("%+v", e.hs.retains)
	}
}

func TestUnreadablePauseFileFailsClosed(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(e.f.Dirs.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.SessionsDir(), "s1.json"), []byte(`{"paused": tr`), 0o600)
	e.record("s1", "u1", "user", 0, "q")
	if r := e.flush("s1", Options{}); r.Status != OptedOut || len(e.hs.retains) != 0 {
		t.Errorf("%+v", r)
	}
}

// The document follows the file name even when records say otherwise.
func TestDocumentIDFollowsTheFile(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "q")
	raw, _ := os.ReadFile(filepath.Join(e.dir, "s1.jsonl"))
	_ = os.WriteFile(filepath.Join(e.dir, "s2.jsonl"), []byte(strings.Replace(string(raw), `"sessionId":"s1"`, `"sessionId":"other"`, 1)), 0o600)
	e.f.Index, _ = LoadSentIndex(t.TempDir())
	e.flush("s2", Options{})
	if got := e.hs.retains[len(e.hs.retains)-1].DocumentID; got != "conversation:s2" {
		t.Errorf("document %q", got)
	}
}

// Only a refusal of the document quarantines it; anything about the server
// or the setup stops the run and keeps the session for the next tick.
func TestErrorClasses(t *testing.T) {
	for status, want := range map[int]string{
		400: Quarantined, 413: Quarantined, 422: Quarantined,
		401: Retry, 403: Retry, 302: Retry, 404: Retry, 408: Retry, 409: Retry, 429: Retry, 500: Retry, 502: Retry, 503: Retry, 504: Retry,
	} {
		e := newEnv(t)
		e.record("s1", "u1", "user", 0, "q")
		e.hs.status = status
		r := e.flush("s1", Options{})
		if r.Status != want || r.Stop != (want == Retry) {
			t.Errorf("HTTP %d: %s stop=%v, want %s", status, r.Status, r.Stop, want)
		}
		if want == Retry {
			e.hs.status = 0
			if r := e.flush("s1", Options{}); r.Status != Sent {
				t.Errorf("HTTP %d then OK: %+v", status, r)
			}
		}
	}
}

func TestInvalidServerSegmentQuarantines(t *testing.T) {
	for _, seg := range []string{"", "abc", "0"} {
		e := newEnv(t)
		e.record("s1", "u1", "user", 0, "q")
		e.record("s1", "u2", "user", 1, "q2")
		e.hs.docs["conversation:s1"] = map[string]string{"last_uuid": "u1", "segment": seg}
		if r := e.flush("s1", Options{}); r.Status != Quarantined || len(e.hs.retains) != 0 {
			t.Errorf("segment %q: %+v", seg, r)
		}
	}
}

func TestReleaseQuarantineResyncs(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "q")
	e.hs.status = 422
	e.flush("s1", Options{})
	if held, _ := QuarantinedSessions(e.f.Dirs.CursorsDir()); len(held) != 1 {
		t.Fatalf("%v", held)
	}
	e.hs.status = 0
	released, err := ReleaseQuarantine(e.f.Dirs.CursorsDir(), "")
	if err != nil || len(released) != 1 {
		t.Fatalf("%v %v", released, err)
	}
	if r := e.flush("s1", Options{}); r.Status != Sent {
		t.Errorf("after release: %+v", r)
	}
}
