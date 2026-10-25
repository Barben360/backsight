package retain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/transcript"
)

// fakeHS stores retained documents like Hindsight: append concatenates
// content and replaces metadata (E21).
type fakeHS struct {
	mu      sync.Mutex
	docs    map[string]map[string]string
	content map[string]string
	retains []hindsight.RetainItem
	ops     []string
	status  int             // non-zero: answer retains with this status
	stale   bool            // metadata not updated (async retain still running)
	hidden  bool            // new documents not readable yet (their first async retain still running)
	hide    map[string]bool // these documents not readable yet
	refuse  map[string]int  // retains into these documents answer with this status
	// onRetain sees each item before it is stored.
	onRetain func(hindsight.RetainItem)
	gets     int      // document reads
	reads    []string // the documents read, in order
	seenOps  map[string]bool
	// epochStatus non-zero: reads of the restore epoch answer with it.
	epochStatus int
	queued      int // pending operations the bank reports
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
		if h.seenOps == nil {
			h.seenOps = map[string]bool{}
		}
		if body.OperationID != "" && h.seenOps[body.OperationID] {
			_, _ = w.Write([]byte(`{"success":true}`)) // E21: a repeated operation collapses
			return
		}
		for _, it := range body.Items {
			if h.onRetain != nil {
				h.onRetain(it)
			}
			if st := h.refuse[it.DocumentID]; st != 0 {
				w.WriteHeader(st)
				return
			}
		}
		h.seenOps[body.OperationID] = true
		for _, it := range body.Items {
			h.retains = append(h.retains, it)
			if !h.stale || h.docs[it.DocumentID] == nil {
				h.docs[it.DocumentID] = it.Metadata
			}
			if it.UpdateMode == "append" {
				h.content[it.DocumentID] += "\n" + it.Content
			} else {
				h.content[it.DocumentID] = it.Content
			}
		}
		h.ops = append(h.ops, body.OperationID)
		_, _ = w.Write([]byte(`{"success":true}`))
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/documents/"):
		h.gets++
		id, _ := url.PathUnescape(r.URL.EscapedPath()[strings.Index(r.URL.EscapedPath(), "/documents/")+len("/documents/"):])
		h.reads = append(h.reads, id)
		if id == EpochDocument && h.epochStatus != 0 {
			w.WriteHeader(h.epochStatus)
			return
		}
		meta, ok := h.docs[id]
		if !ok || h.hidden || h.hide[id] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := json.Marshal(map[string]any{"id": id, "document_metadata": meta})
		_, _ = w.Write(raw)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/operations"):
		n := 0
		if r.URL.Query().Get("status") == "pending" {
			n = h.queued
		}
		_, _ = fmt.Fprintf(w, `{"total":%d,"operations":[]}`, n)
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

// A session moving to another repository sends that project's records to
// its own document: an append to one document relabels all of it (E30 V2).
func TestProjectChangeMidSessionSplitsSegments(t *testing.T) {
	e := newEnv(t)
	infra := e.repo("infra")
	e.record("s1", "u1", "user", 0, "billing question")
	e.recordIn(infra, "s1", "u2", "user", 1, "infra question")
	e.flush("s1", Options{})
	if len(e.hs.retains) != 2 || e.hs.retains[0].Tags[0] != "project:billing" || e.hs.retains[1].Tags[0] != "project:infra" ||
		e.hs.retains[0].DocumentID != "conversation:s1" || e.hs.retains[1].DocumentID != "conversation:s1@infra" || e.hs.retains[1].UpdateMode != "" {
		t.Errorf("%+v", e.hs.retains)
	}
}

// repo creates a git repository named name next to the session's default
// directory: a strong project name (project.Resolver.Named).
func (e *env) repo(name string) string {
	e.t.Helper()
	dir := filepath.Join(filepath.Dir(e.cwd), name)
	_ = os.MkdirAll(dir, 0o700)
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		e.t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// itemsOf lists the retained items of document doc.
func (h *fakeHS) itemsOf(doc string) []hindsight.RetainItem {
	var out []hindsight.RetainItem
	for _, it := range h.retains {
		if it.DocumentID == doc {
			out = append(out, it)
		}
	}
	return out
}

// A→B→A: each project's records go to its own document with its own
// segments and metadata; B is never appended to A's document.
func TestEachProjectGetsItsOwnDocument(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.repo("alpha"), e.repo("beta")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha question")
	e.recordIn(alpha, "s1", "a1", "assistant", 1, "alpha answer")
	e.recordIn(beta, "s1", "u2", "user", 2, "beta question")
	e.recordIn(beta, "s1", "a2", "assistant", 3, "beta answer")
	e.recordIn(alpha, "s1", "u3", "user", 4, "back in alpha")
	r := e.flush("s1", Options{})
	if r.Status != Sent || r.Records != 5 || !reflect.DeepEqual(r.Documents, []string{"conversation:s1", "conversation:s1@beta"}) {
		t.Fatalf("%+v", r)
	}
	home, side := e.hs.itemsOf("conversation:s1"), e.hs.itemsOf("conversation:s1@beta")
	if len(home) != 2 || len(side) != 1 || len(e.hs.retains) != 3 {
		t.Fatalf("home %d, side %d, all %d", len(home), len(side), len(e.hs.retains))
	}
	for _, it := range home {
		if strings.Contains(it.Content, "beta") || it.Tags[0] != "project:alpha" || it.ObservationScopes[0][0] != "project:alpha" {
			t.Errorf("alpha's document got %+v", it)
		}
	}
	if home[0].UpdateMode != "" || !strings.Contains(home[0].Content, "REF-ID: conversation:s1\"") ||
		home[1].UpdateMode != "append" || home[1].Metadata["segment"] != "2" || !strings.Contains(home[1].Content, "back in alpha") {
		t.Errorf("alpha's segments: %+v", home)
	}
	if s := side[0]; s.UpdateMode != "" || s.Tags[0] != "project:beta" || !strings.Contains(s.Content, "REF-ID: conversation:s1@beta") ||
		strings.Contains(s.Content, "alpha") || s.Metadata["segment"] != "1" {
		t.Errorf("beta's segment: %+v", s)
	}
	if m := e.hs.docs["conversation:s1"]; m["last_uuid"] != "u3" || m["segment"] != "2" {
		t.Errorf("alpha's metadata %v", m)
	}
	if m := e.hs.docs["conversation:s1@beta"]; m["last_uuid"] != "a2" || m["segment"] != "1" {
		t.Errorf("beta's metadata %v", m)
	}
	cur, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	if cur.Home != "alpha" || !reflect.DeepEqual(cur.Segments, map[string]int{"alpha": 2, "beta": 1}) || cur.Segment != 2 ||
		cur.Document != "conversation:s1" || cur.Project != "alpha" {
		t.Errorf("cursor %+v", cur)
	}
	// A later flush, starting mid-file, appends to beta's own document.
	e.recordIn(beta, "s1", "u4", "user", 60, "beta again")
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	if side = e.hs.itemsOf("conversation:s1@beta"); len(side) != 2 || side[1].UpdateMode != "append" || side[1].Metadata["segment"] != "2" ||
		side[1].Metadata["last_uuid"] != "u4" || len(e.hs.itemsOf("conversation:s1")) != 2 {
		t.Errorf("%+v", e.hs.retains)
	}
}

// A directory outside any repository does not start a project mid-session:
// a session started in ~ that cds into ~/.npm-global/.../dist stays
// transverse (the live project:build), one in a repository stays in it.
// Only a session's first directory may name a project by its basename.
func TestANonRepoDirectoryKeepsThePreviousProject(t *testing.T) {
	e := newEnv(t)
	home := filepath.Dir(filepath.Dir(e.cwd))
	dist := filepath.Join(home, ".npm-global", "lib", "node_modules", "crawler", "dist")
	_ = os.MkdirAll(dist, 0o700)
	e.recordIn(home, "s1", "u1", "user", 0, "in the home directory")
	e.recordIn(dist, "s1", "u2", "user", 1, "reading the bundle")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	if len(e.hs.retains) != 1 || e.hs.retains[0].Tags[0] != "project:.claude" || e.hs.retains[0].DocumentID != "conversation:s1" {
		t.Errorf("%+v", e.hs.retains)
	}

	// Started in a plain directory: its basename names the session; a
	// repository then starts its own project, which a later plain
	// directory keeps, also across flushes (the cursor carries it).
	e = newEnv(t)
	welcome, beta, tmp := filepath.Join(home, "Documents", "Welcome"), e.repo("beta"), filepath.Join(home, "tmp")
	_ = os.MkdirAll(welcome, 0o700)
	_ = os.MkdirAll(tmp, 0o700)
	e.recordIn(welcome, "s1", "u1", "user", 0, "welcome notes")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta work")
	e.flush("s1", Options{})
	e.recordIn(tmp, "s1", "u3", "user", 60, "beta scratch file in tmp")
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	want := []struct{ doc, tag string }{{"conversation:s1", "project:Welcome"}, {"conversation:s1@beta", "project:beta"}, {"conversation:s1@beta", "project:beta"}}
	if len(e.hs.retains) != len(want) {
		t.Fatalf("%+v", e.hs.retains)
	}
	for i, w := range want {
		if it := e.hs.retains[i]; it.DocumentID != w.doc || it.Tags[0] != w.tag {
			t.Errorf("item %d: %s %v, want %s %s", i, it.DocumentID, it.Tags, w.doc, w.tag)
		}
	}
}

// With no cursor (new machine, lost state), each document resumes after its
// own position: a record goes out unless its project's document holds it.
func TestResyncResumesEachDocumentAfterItsOwnPosition(t *testing.T) {
	type item struct{ doc, text, mode, segment string }
	for _, c := range []struct {
		name string
		docs map[string]map[string]string
		want []item
	}{
		{"only the side document", map[string]map[string]string{"conversation:s1@beta": {"last_uuid": "u2", "segment": "1", "project": "beta"}},
			[]item{{"conversation:s1", "alpha 1", "", "1"}, {"conversation:s1", "alpha 3", "append", "2"}, {"conversation:s1@beta", "beta 4", "append", "2"}}},
		{"side further than home", map[string]map[string]string{"conversation:s1": {"last_uuid": "u1", "segment": "1", "project": "alpha"}, "conversation:s1@beta": {"last_uuid": "u2", "segment": "1", "project": "beta"}},
			[]item{{"conversation:s1", "alpha 3", "append", "2"}, {"conversation:s1@beta", "beta 4", "append", "2"}}},
		{"home further than side", map[string]map[string]string{"conversation:s1": {"last_uuid": "u3", "segment": "2", "project": "alpha"}, "conversation:s1@beta": {"last_uuid": "u2", "segment": "1", "project": "beta"}},
			[]item{{"conversation:s1@beta", "beta 4", "append", "2"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			alpha, beta := e.repo("alpha"), e.repo("beta")
			e.recordIn(alpha, "s1", "u1", "user", 0, "alpha 1")
			e.recordIn(beta, "s1", "u2", "user", 1, "beta 2")
			e.recordIn(alpha, "s1", "u3", "user", 2, "alpha 3")
			e.recordIn(beta, "s1", "u4", "user", 3, "beta 4")
			e.hs.docs = c.docs // sent from another machine: no cursor, no index here
			if r := e.flush("s1", Options{}); r.Status != Sent {
				t.Fatalf("%+v", r)
			}
			if len(e.hs.retains) != len(c.want) {
				t.Fatalf("%d items: %+v", len(e.hs.retains), e.hs.retains)
			}
			for i, w := range c.want {
				it := e.hs.retains[i]
				if it.DocumentID != w.doc || it.UpdateMode != w.mode || it.Metadata["segment"] != w.segment || !strings.Contains(it.Content, w.text) {
					t.Errorf("item %d: %s %q segment %s %s", i, it.DocumentID, it.UpdateMode, it.Metadata["segment"], it.Content)
				}
			}
			for _, text := range []string{"alpha 1", "beta 2", "alpha 3", "beta 4"} {
				n, want := 0, 0
				for _, it := range e.hs.retains {
					n += strings.Count(it.Content, `"`+text+`"`)
				}
				for _, w := range c.want {
					if w.text == text {
						want = 1
					}
				}
				if n != want {
					t.Errorf("%q sent %d times, want %d", text, n, want)
				}
			}
		})
	}
}

// A last_uuid this transcript lacks in any of the session's documents makes
// the position unknown: quarantine.
func TestUnknownPositionInASideDocumentQuarantines(t *testing.T) {
	e := newEnv(t)
	beta := e.repo("beta")
	e.record("s1", "u1", "user", 0, "billing")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta")
	e.hs.docs["conversation:s1"] = map[string]string{"last_uuid": "u1", "segment": "1"}
	e.hs.docs["conversation:s1@beta"] = map[string]string{"last_uuid": "elsewhere", "segment": "1"}
	if r := e.flush("s1", Options{}); r.Status != Quarantined || len(e.hs.retains) != 0 || !strings.Contains(r.Detail, "elsewhere") {
		t.Errorf("%+v", r)
	}
}

// A cursor written before per-project documents (no home) resyncs once from
// the server, appends to the home document, then flushes as usual.
func TestCursorWithoutHomeResyncsOnce(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	e.flush("s1", Options{})
	cur, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	cur.Home, cur.Segments, cur.Project = "", nil, "" // as an older build wrote it
	if err := SaveCursor(e.f.Dirs.CursorsDir(), "s1", cur); err != nil {
		t.Fatal(err)
	}
	e.record("s1", "u2", "user", 60, "second")
	gets := e.hs.gets
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	if e.hs.gets != gets+1 {
		t.Errorf("%d document reads, want one resync", e.hs.gets-gets)
	}
	if it := e.hs.retains[1]; it.DocumentID != "conversation:s1" || it.UpdateMode != "append" || strings.Contains(it.Content, "first") {
		t.Errorf("%+v", it)
	}
	if cur, _, _ = LoadCursor(e.f.Dirs.CursorsDir(), "s1"); cur.Home != "billing" || cur.Segments["billing"] != 2 {
		t.Errorf("cursor %+v", cur)
	}
	e.record("s1", "u3", "user", 120, "third")
	gets = e.hs.gets
	expect(t, e.flush("s1", Options{}), Sent, 1, 3)
	if e.hs.gets != gets {
		t.Errorf("resynced again: %d reads", e.hs.gets-gets)
	}
}

// Issue 101 for a side document: its first segment is not readable yet and
// the cursor is lost; a new segment 1 would replace it, so the flush waits.
func TestASideDocumentsPendingFirstSegmentWaits(t *testing.T) {
	e := newEnv(t)
	beta := e.repo("beta")
	e.record("s1", "u1", "user", 0, "billing")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta first")
	e.hs.hide = map[string]bool{"conversation:s1@beta": true}
	e.flush("s1", Options{})
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	e.recordIn(beta, "s1", "u3", "user", 60, "beta second")
	if r := e.flush("s1", Options{}); r.Status != Retry || len(e.hs.retains) != 2 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
	e.hs.hide = nil // the first retain lands
	if r := e.flush("s1", Options{}); r.Status != Sent || len(e.hs.retains) != 3 {
		t.Fatalf("%+v", r)
	}
	if it := e.hs.retains[2]; it.DocumentID != "conversation:s1@beta" || it.UpdateMode != "append" || it.Metadata["segment"] != "2" ||
		strings.Contains(it.Content, "beta first") {
		t.Errorf("%+v", it)
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

// countLines counts how many stored lines of session s1's document contain text.
func (h *fakeHS) countLines(text string) int {
	n := 0
	for _, l := range strings.Split(h.content["conversation:s1"], "\n") {
		if strings.Contains(l, text) {
			n++
		}
	}
	return n
}

func TestLargeBacklogGoesOutInCappedSegments(t *testing.T) {
	old := maxSegmentRecords
	maxSegmentRecords = 2
	t.Cleanup(func() { maxSegmentRecords = old })
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.record("s1", fmt.Sprintf("u%d", i), "user", i, fmt.Sprintf("turn %d", i))
	}
	r := e.flush("s1", Options{})
	if r.Status != Sent || r.Records != 5 || r.Segment != 3 || len(e.hs.retains) != 3 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
	if e.hs.retains[0].UpdateMode != "" || e.hs.retains[1].UpdateMode != "append" || e.hs.retains[2].Metadata["segment"] != "3" {
		t.Errorf("%+v", e.hs.retains)
	}
	for i := 0; i < 5; i++ {
		if n := e.hs.countLines(fmt.Sprintf("turn %d", i)); n != 1 {
			t.Errorf("turn %d stored %d times", i, n)
		}
	}
}

// Server metadata lagging an async retain, plus a lost cursor: the sent
// index prevents a duplicate.
func TestStaleServerMetadataDoesNotDuplicate(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	e.flush("s1", Options{})
	e.hs.stale = true
	e.record("s1", "u2", "user", 30, "second")
	e.flush("s1", Options{})
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	e.record("s1", "u3", "user", 60, "third")
	r := e.flush("s1", Options{})
	if r.Status != Sent || r.Records != 1 {
		t.Fatalf("%+v", r)
	}
	for _, text := range []string{"first", "second", "third"} {
		if n := e.hs.countLines(text); n != 1 {
			t.Errorf("%q stored %d times", text, n)
		}
	}
}

// A cursor that cannot be saved after a send must not cause a resend.
func TestCursorSaveFailureDoesNotDuplicate(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	_ = os.MkdirAll(e.f.Dirs.CursorsDir(), 0o700)
	_ = os.Chmod(e.f.Dirs.CursorsDir(), 0o500) // writes fail
	r := e.flush("s1", Options{})
	_ = os.Chmod(e.f.Dirs.CursorsDir(), 0o700)
	if r.Status != Sent || !strings.Contains(r.Detail, "cursor was not saved") {
		t.Fatalf("%+v", r)
	}
	r = e.flush("s1", Options{})
	if r.Status == Sent || e.hs.countLines("first") != 1 || len(e.hs.retains) != 1 {
		t.Errorf("resent after a failed cursor save: %+v, %d retains", r, len(e.hs.retains))
	}
}

// A backfill session is remembered: later ticks neither re-read nor query it.
func TestBackfillSessionIsNotRequeriedEveryTick(t *testing.T) {
	e := newEnv(t)
	e.f.RetainSince = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	e.record("s1", "u1", "user", 0, "old")
	e.flush("s1", Options{})
	gets := e.hs.gets
	for i := 0; i < 3; i++ {
		if r := e.flush("s1", Options{}); r.Status != Backfill {
			t.Fatalf("%+v", r)
		}
	}
	if e.hs.gets != gets {
		t.Errorf("%d extra document reads for a backfill session", e.hs.gets-gets)
	}
	if r := e.flush("s1", Options{IgnoreSince: true}); r.Status != Sent {
		t.Errorf("an explicit flush still sends it: %+v", r)
	}
}

func TestTruncatedTranscriptResyncs(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	e.record("s1", "u2", "user", 1, "second")
	e.flush("s1", Options{})
	p := filepath.Join(e.dir, "s1.jsonl")
	_ = os.WriteFile(p, nil, 0o600) // rewritten shorter than the cursor offset
	e.record("s1", "u1", "user", 0, "first")
	e.record("s1", "u2", "user", 1, "second")
	e.record("s1", "u3", "user", 60, "third")
	r := e.flush("s1", Options{})
	if r.Status != Sent || r.Records != 1 || e.hs.countLines("third") != 1 {
		t.Errorf("%+v", r)
	}
}

// A clock-skewed future timestamp is clamped: the session waits, then goes.
func TestFutureTimestampIsClamped(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 600, "from the future")               // 19:00, the clock says 12:00
	_ = os.Chtimes(filepath.Join(e.dir, "s1.jsonl"), e.clock, e.clock) // written now
	if r := e.flush("s1", Options{}); r.Status != Waiting {
		t.Fatalf("%+v", r)
	}
	e.clock = e.clock.Add(31 * time.Minute)
	if r := e.flush("s1", Options{}); r.Status != Sent || e.hs.retains[0].Timestamp.After(e.clock) {
		t.Errorf("%+v %v", r, e.hs.retains)
	}
}

func TestSettleBoundaries(t *testing.T) {
	for _, c := range []struct {
		age  time.Duration
		want string
	}{{29*time.Minute + 59*time.Second, Waiting}, {30 * time.Minute, Sent}} {
		e := newEnv(t)
		e.record("s1", "u1", "user", 0, "q")
		e.clock = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC).Add(c.age)
		if r := e.flush("s1", Options{}); r.Status != c.want {
			t.Errorf("age %s: %s, want %s", c.age, r.Status, c.want)
		}
	}
}

// In an open session, a turn exactly 2 h old goes; 1 h 59 stays (decision
// 0050). The clock is 12:00.
func TestOpenSessionTwoHourBoundary(t *testing.T) {
	for _, c := range []struct {
		offset int // minutes after 09:00
		want   string
	}{{61, Waiting}, {60, Sent}} {
		e := newEnv(t)
		e.record("s1", "u1", "user", c.offset, "older turn")
		e.record("s1", "u2", "user", 170, "fresh turn") // keeps the session open
		if r := e.flush("s1", Options{}); r.Status != c.want {
			t.Errorf("turn at +%d min: %s, want %s", c.offset, r.Status, c.want)
		}
	}
}

// Issue 101: the first segment's async retain has not landed and the
// cursor is lost. The document reads as missing, but this session's records
// were sent: a new segment 1 would replace them, so the flush waits.
func TestLostCursorBeforeTheFirstSegmentLandsWaits(t *testing.T) {
	e := newEnv(t)
	e.hs.hidden = true
	e.record("s1", "u1", "user", 0, "first")
	e.flush("s1", Options{})
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	e.record("s1", "u2", "user", 30, "second")
	r := e.flush("s1", Options{})
	if r.Status != Retry || r.Stop || len(e.hs.retains) != 1 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
	e.hs.hidden = false // the first retain lands
	r = e.flush("s1", Options{})
	if r.Status != Sent || len(e.hs.retains) != 2 || e.hs.retains[1].UpdateMode != "append" {
		t.Fatalf("%+v, %+v", r, e.hs.retains)
	}
	for _, text := range []string{"first", "second"} {
		if n := e.hs.countLines(text); n != 1 {
			t.Errorf("%q stored %d times", text, n)
		}
	}
}

// A fork's copied records were sent under the parent's session: the fork's
// own document is created normally.
func TestForkStillCreatesItsDocument(t *testing.T) {
	e := newEnv(t)
	e.record("parent", "u1", "user", 0, "shared turn")
	e.flush("parent", Options{})
	e.record("fork", "u1", "user", 0, "shared turn")
	e.record("fork", "u9", "user", 30, "fork turn")
	r := e.flush("fork", Options{})
	if r.Status != Sent || r.Records != 1 || e.hs.retains[len(e.hs.retains)-1].UpdateMode == "append" {
		t.Fatalf("%+v %+v", r, e.hs.retains)
	}
}

// A first segment the server never stores is not waited for forever: after
// PendingLimit the session is quarantined, and releasing it resends it.
func TestAFirstSegmentThatNeverLandsIsQuarantined(t *testing.T) {
	e := newEnv(t)
	e.hs.hidden = true
	e.record("s1", "u1", "user", 0, "first")
	e.flush("s1", Options{})
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	e.record("s1", "u2", "user", 30, "second")
	if r := e.flush("s1", Options{}); r.Status != Retry {
		t.Fatalf("%+v", r)
	}
	e.clock = e.clock.Add(PendingLimit)
	if r := e.flush("s1", Options{}); r.Status != Quarantined || len(e.hs.retains) != 1 {
		t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
	e.hs.retains, e.hs.content = nil, map[string]string{}
	e.hs.hidden = false
	e.hs.docs = map[string]map[string]string{} // the server really lost it
	if _, err := ReleaseQuarantine(e.f.Dirs.CursorsDir(), "s1"); err != nil {
		t.Fatal(err)
	}
	r := e.flush("s1", Options{})
	if r.Status != Sent || len(e.hs.retains) != 1 || e.hs.retains[0].UpdateMode == "append" || e.hs.countLines("first") != 1 || e.hs.countLines("second") != 1 {
		t.Errorf("%+v %+v", r, e.hs.retains)
	}
	if _, err := os.Stat(filepath.Join(e.f.Dirs.CursorsDir(), "s1.pending")); !os.IsNotExist(err) {
		t.Error("the pending marker outlived the resend")
	}
}

// A bank switch is not a pending first segment: the sent index's session
// keys are per bank, so the new bank gets the session.
func TestBankSwitchSendsTheSessionToTheNewBank(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	e.flush("s1", Options{})
	e.hs.docs = map[string]map[string]string{} // the new bank holds nothing
	e.f.Bank = "other-bank"
	e.record("s1", "u2", "user", 30, "second")
	r := e.flush("s1", Options{})
	if r.Status != Sent || e.hs.retains[len(e.hs.retains)-1].UpdateMode == "append" {
		t.Fatalf("%+v %+v", r, e.hs.retains)
	}
}

// pause → private talk → resume → flush: the paused stretch stays out even
// though the session is no longer paused when the flush runs.
func TestAPausedStretchStaysOutAfterAResume(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(e.f.Dirs.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.SessionsDir(), "s1.json"),
		[]byte(`{"paused":false,"paused_spans":[{"from":"2026-10-24T09:05:00Z","to":"2026-10-24T09:15:00Z"}]}`), 0o600)
	e.record("s1", "u1", "user", 0, "before the pause")
	e.record("s1", "u2", "user", 10, "private matter")
	e.record("s1", "a2", "assistant", 11, "private answer")
	e.record("s1", "u3", "user", 20, "after the resume")
	e.flush("s1", Options{Force: true})
	var sent []string
	for _, it := range e.hs.retains {
		sent = append(sent, it.Content)
	}
	all := strings.Join(sent, "\n")
	if strings.Contains(all, "private") || !strings.Contains(all, "before the pause") || !strings.Contains(all, "after the resume") {
		t.Errorf("sent:\n%s", all)
	}
}

// A session paused now (open stretch) still sends what was typed before the
// pause: only the stretch stays out. A pause with no stretch (older state)
// keeps everything out, as TestPausedSessionIsNotSent checks.
func TestAnOpenPauseKeepsOnlyItsStretchOut(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(e.f.Dirs.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.SessionsDir(), "s1.json"),
		[]byte(`{"paused":true,"paused_spans":[{"from":"2026-10-24T09:05:00Z"}]}`), 0o600)
	e.record("s1", "u1", "user", 0, "before the pause")
	e.record("s1", "u2", "user", 10, "private matter")
	e.flush("s1", Options{Force: true})
	var sent []string
	for _, it := range e.hs.retains {
		sent = append(sent, it.Content)
	}
	if all := strings.Join(sent, "\n"); !strings.Contains(all, "before the pause") || strings.Contains(all, "private") {
		t.Errorf("sent:\n%s", all)
	}
	// later turns, still paused, stay out too
	e.record("s1", "u3", "user", 20, "private again")
	e.flush("s1", Options{Force: true})
	for _, it := range e.hs.retains {
		if strings.Contains(it.Content, "private") {
			t.Errorf("sent while paused: %s", it.Content)
		}
	}
}

// A fork copies its parent's records, with the parent's session id: the
// parent's paused stretch stays out of the copy, and the records the parent
// already sent are not sent again.
func TestAForkKeepsItsParentsPausedStretchOut(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(e.f.Dirs.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.SessionsDir(), "s1.json"),
		[]byte(`{"paused":false,"paused_spans":[{"from":"2026-10-24T09:05:00Z","to":"2026-10-24T09:15:00Z"}]}`), 0o600)
	e.record("s1", "u1", "user", 0, "before the pause")
	e.record("s1", "u2", "user", 10, "private matter")
	e.record("s1", "u3", "user", 20, "after the resume")
	e.flush("s1", Options{Force: true})
	sentByParent := len(e.hs.retains)
	// the fork: the same records (parent's session id), then its own turn
	raw, _ := os.ReadFile(filepath.Join(e.dir, "s1.jsonl"))
	_ = os.WriteFile(filepath.Join(e.dir, "fork1.jsonl"), raw, 0o600)
	e.recordIn(e.cwd, "fork1", "u4", "user", 30, "the fork's own turn")
	e.flush("fork1", Options{Force: true})
	var fork []string
	for _, it := range e.hs.retains[sentByParent:] {
		fork = append(fork, it.Content)
	}
	got := strings.Join(fork, "\n")
	if strings.Contains(got, "private") || strings.Contains(got, "before the pause") || strings.Contains(got, "after the resume") || !strings.Contains(got, "the fork's own turn") {
		t.Errorf("fork sent:\n%s", got)
	}
}

// A fork taken while the parent is still paused: the copied stretch stays
// out even though nothing was flushed for the parent yet.
func TestAForkOfAPausedSessionKeepsTheStretchOut(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(e.f.Dirs.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.SessionsDir(), "s1.json"),
		[]byte(`{"paused":true,"paused_spans":[{"from":"2026-10-24T09:05:00Z"}]}`), 0o600)
	e.record("s1", "u1", "user", 0, "before the pause")
	e.record("s1", "u2", "user", 10, "private matter")
	raw, _ := os.ReadFile(filepath.Join(e.dir, "s1.jsonl"))
	_ = os.WriteFile(filepath.Join(e.dir, "fork2.jsonl"), raw, 0o600)
	e.recordIn(e.cwd, "fork2", "u3", "user", 30, "the fork's own turn")
	e.flush("fork2", Options{Force: true})
	var got []string
	for _, it := range e.hs.retains {
		got = append(got, it.Content)
	}
	if all := strings.Join(got, "\n"); strings.Contains(all, "private") || !strings.Contains(all, "the fork's own turn") {
		t.Errorf("sent:\n%s", all)
	}
}

// recordFrom is record with the entrypoint Claude Code writes on each record.
func (e *env) recordFrom(entrypoint, uuid string, minute int, text string) {
	const sid = "s1"
	e.t.Helper()
	ts := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute).Format(time.RFC3339)
	line := fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":%q,"cwd":%q,"entrypoint":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
		uuid, sid, e.cwd, entrypoint, ts, text)
	fh, err := os.OpenFile(filepath.Join(e.dir, sid+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = fh.WriteString(line)
	_ = fh.Close()
}

func skipSDK(ep string) bool { return strings.HasPrefix(ep, "sdk-") }

func TestHeadlessSessionIsNeverSent(t *testing.T) {
	e := newEnv(t)
	e.f.Headless = skipSDK
	e.recordFrom("sdk-ts", "u1", 0, "observer: summarise a meeting")
	e.recordFrom("sdk-ts", "u2", 1, "observer: list open tasks")
	r := e.flush("s1", Options{})
	if r.Status != OptedOut || !strings.Contains(r.Detail, "sdk-ts") || len(e.hs.retains) != 0 {
		t.Fatalf("%+v, retains %d", r, len(e.hs.retains))
	}
	// Handled for good: a later flush does not read them again.
	if r := e.flush("s1", Options{}); r.Status != Nothing || len(e.hs.retains) != 0 {
		t.Errorf("second flush %+v", r)
	}
	if !e.f.Index.Has("u1") || !e.f.Index.Has("u2") {
		t.Error("headless records not marked in the sent index")
	}
}

func TestInteractiveEntrypointsAreSentWithTheirEntrypoint(t *testing.T) {
	for _, ep := range []string{"cli", "claude-desktop", ""} {
		e := newEnv(t)
		e.f.Headless = skipSDK
		e.recordFrom(ep, "u1", 0, "we decided to keep PostgreSQL 16")
		if r := e.flush("s1", Options{}); r.Status != Sent || len(e.hs.retains) != 1 {
			t.Fatalf("entrypoint %q: %+v", ep, r)
		}
		if got := e.hs.retains[0].Metadata["entrypoint"]; got != ep {
			t.Errorf("entrypoint %q: metadata entrypoint = %q", ep, got)
		}
	}
}

func TestHeadlessStretchOfAFileIsLeftOut(t *testing.T) {
	e := newEnv(t)
	e.f.Headless = skipSDK
	e.recordFrom("cli", "u1", 0, "interactive question")
	e.recordFrom("sdk-cli", "u2", 1, "cron run log line")
	e.recordFrom("cli", "u3", 2, "interactive follow-up")
	if r := e.flush("s1", Options{}); r.Status != Sent {
		t.Fatalf("%+v", r)
	}
	var all string
	for _, it := range e.hs.retains {
		all += it.Content
	}
	if strings.Contains(all, "cron run log") || !strings.Contains(all, "interactive question") || !strings.Contains(all, "interactive follow-up") {
		t.Errorf("sent:\n%s", all)
	}
}

func TestSurveyCountsHeadlessSessionsAsOptedOut(t *testing.T) {
	e := newEnv(t)
	e.f.Headless = skipSDK
	e.recordFrom("sdk-cli", "u1", 0, "cron run log line")
	c, _ := e.f.Survey(filepath.Join(e.dir, "s1.jsonl"))
	if c.Status != "opted_out" || c.Chars != 0 || !strings.Contains(c.Reason, "skip_entrypoints") {
		t.Errorf("%+v", c)
	}
}

// releaseAndResend: the server lost the documents named, and the
// operations that wrote them; the session is released from quarantine and
// flushed again.
func (e *env) releaseAndResend(lost ...string) Result {
	e.t.Helper()
	e.hs.hide, e.hs.seenOps = nil, nil
	for _, d := range lost {
		delete(e.hs.docs, d)
	}
	if _, err := ReleaseQuarantine(e.f.Dirs.CursorsDir(), "s1"); err != nil {
		e.t.Fatal(err)
	}
	return e.flush("s1", Options{})
}

// waitThenQuarantine: a resync finds this session's first segment of a
// document missing; it waits, then quarantines after PendingLimit.
func (e *env) waitThenQuarantine(sent int) {
	e.t.Helper()
	if r := e.flush("s1", Options{}); r.Status != Retry || len(e.hs.retains) != sent {
		e.t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
	e.clock = e.clock.Add(PendingLimit)
	if r := e.flush("s1", Options{}); r.Status != Quarantined || len(e.hs.retains) != sent {
		e.t.Fatalf("%+v, %d retains", r, len(e.hs.retains))
	}
}

// expectItems checks the items sent since from: document, update mode,
// segment and the one text each holds.
func (e *env) expectItems(from int, want ...[4]string) {
	e.t.Helper()
	got := e.hs.retains[from:]
	if len(got) != len(want) {
		e.t.Fatalf("%d items, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		it := got[i]
		if it.DocumentID != w[0] || it.UpdateMode != w[1] || it.Metadata["segment"] != w[2] || !strings.Contains(it.Content, w[3]) {
			e.t.Errorf("item %d: %s %q segment %s\n%s\nwant %v", i, it.DocumentID, it.UpdateMode, it.Metadata["segment"], it.Content, w)
		}
	}
}

// Issue 101 for the home document while a side document landed: positions
// are per document, so the side document's further position does not let
// a new segment 1 replace the pending home document; once taken as lost
// and released, the home records go out again from its segment 1.
func TestAPendingHomeDocumentIsNotReplacedByAFurtherSideDocument(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.repo("alpha"), e.repo("beta")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	e.hs.hide = map[string]bool{"conversation:s1": true}
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	e.recordIn(alpha, "s1", "u3", "user", 60, "alpha three")
	e.waitThenQuarantine(2)
	if r := e.releaseAndResend("conversation:s1"); r.Status != Sent {
		t.Fatalf("%+v", r)
	}
	e.expectItems(2, [4]string{"conversation:s1", "", "1", "alpha one"}, [4]string{"conversation:s1", "append", "2", "alpha three"})
	if strings.Contains(e.hs.retains[2].Content, "alpha three") || strings.Contains(e.hs.retains[3].Content, "beta") {
		t.Errorf("%+v", e.hs.retains[2:])
	}
}

// A side document lost for good while the home document is further ahead:
// the session waits, is quarantined, and its release resends that project's
// records from the side document's segment 1, and nothing of the home's.
func TestALostSideDocumentIsResentFromItsFirstSegment(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.repo("alpha"), e.repo("beta")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	e.recordIn(alpha, "s1", "u3", "user", 2, "alpha three")
	expect(t, e.flush("s1", Options{}), Sent, 3, 2)
	delete(e.hs.docs, "conversation:s1@beta")
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	e.recordIn(alpha, "s1", "u4", "user", 60, "alpha four")
	e.waitThenQuarantine(3)
	if r := e.releaseAndResend(); r.Status != Sent {
		t.Fatalf("%+v", r)
	}
	e.expectItems(3, [4]string{"conversation:s1@beta", "", "1", "beta two"}, [4]string{"conversation:s1", "append", "3", "alpha four"})
	if strings.Contains(e.hs.retains[4].Content, "alpha three") {
		t.Errorf("resent: %s", e.hs.retains[4].Content)
	}
}

// noSecret fails when a retained item names the excluded project "secret"
// anywhere.
func (e *env) noSecret() {
	e.t.Helper()
	for _, it := range e.hs.retains {
		if strings.Contains(it.DocumentID, "secret") || strings.Contains(strings.Join(it.Tags, ","), "secret") || strings.Contains(fmt.Sprint(it.Metadata, it.ObservationScopes), "secret") {
			e.t.Errorf("secret leaked: %+v", it)
		}
	}
}

// A plain directory after an excluded repository keeps its project, so it
// is kept out too: never sent, never labelled with the excluded name, until
// a retained repository names the project again. Also when the session
// starts in the excluded repository.
func TestAPlainDirectoryAfterAnExcludedRepositoryStaysOut(t *testing.T) {
	e := newEnv(t)
	alpha, secret := e.repo("alpha"), e.repo("secret")
	scratch := filepath.Join(filepath.Dir(e.cwd), "scratch")
	_ = os.MkdirAll(scratch, 0o700)
	e.f.Exclude = []string{secret}
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(secret, "s1", "u2", "user", 1, "confidential repo")
	e.recordIn(scratch, "s1", "u3", "user", 2, "confidential scratch")
	e.recordIn(alpha, "s1", "u4", "user", 3, "alpha again")
	expect(t, e.flush("s1", Options{}), Sent, 2, 2)
	e.expectItems(0, [4]string{"conversation:s1", "", "1", "alpha one"}, [4]string{"conversation:s1", "append", "2", "alpha again"})
	e.noSecret()
	for _, id := range e.hs.reads {
		if strings.Contains(id, "secret") {
			t.Errorf("a kept-out project's document was read: %s", id)
		}
	}
	for _, it := range e.hs.retains {
		if strings.Contains(it.Content, "confidential") {
			t.Errorf("sent: %s", it.Content)
		}
	}

	// started in the excluded repository
	e.recordIn(secret, "s2", "v1", "user", 0, "confidential repo")
	e.recordIn(scratch, "s2", "v2", "user", 1, "confidential scratch")
	e.recordIn(alpha, "s2", "v3", "user", 2, "alpha work")
	from := len(e.hs.retains)
	if r := e.flush("s2", Options{}); r.Status != Sent || r.Records != 1 {
		t.Fatalf("%+v", r)
	}
	if it := e.hs.retains[from]; it.DocumentID != "conversation:s2@alpha" || it.Tags[0] != "project:alpha" || strings.Contains(it.Content, "confidential") {
		t.Errorf("%+v", it)
	}
	e.noSecret()
}

// An opt-out run last in a flush: the next flush, starting mid-file, still
// knows the plain directory after it inherits a kept-out project.
func TestAnOptOutRunLastCarriesToTheNextFlush(t *testing.T) {
	e := newEnv(t)
	alpha, secret := e.repo("alpha"), e.repo("secret")
	scratch := filepath.Join(filepath.Dir(e.cwd), "scratch")
	_ = os.MkdirAll(scratch, 0o700)
	e.f.Exclude = []string{secret}
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(secret, "s1", "u2", "user", 1, "confidential repo")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.recordIn(scratch, "s1", "u3", "user", 60, "confidential scratch")
	if r := e.flush("s1", Options{}); r.Status != OptedOut || len(e.hs.retains) != 1 {
		t.Errorf("%+v, %d retains", r, len(e.hs.retains))
	}
	e.noSecret()
}

// A resync with nothing left to send still records the last record's
// project: a later plain directory keeps it.
func TestAResyncWithNothingToSendKeepsTheLastProject(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.repo("alpha"), e.repo("beta")
	scratch := filepath.Join(filepath.Dir(e.cwd), "scratch")
	_ = os.MkdirAll(scratch, 0o700)
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	e.flush("s1", Options{})
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)
	e.recordIn(scratch, "s1", "u3", "user", 60, "beta scratch")
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	e.expectItems(2, [4]string{"conversation:s1@beta", "append", "2", "beta scratch"})
}

// A fork copies parent records of two projects: it sends only its own
// turn, to its own document of the project that turn keeps.
func TestAForkSpanningTwoProjectsSendsOnlyItsOwnTurns(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.repo("alpha"), e.repo("beta")
	scratch := filepath.Join(filepath.Dir(e.cwd), "scratch")
	_ = os.MkdirAll(scratch, 0o700)
	e.recordIn(alpha, "parent", "u1", "user", 0, "alpha shared")
	e.recordIn(beta, "parent", "u2", "user", 1, "beta shared")
	e.flush("parent", Options{})
	raw, _ := os.ReadFile(filepath.Join(e.dir, "parent.jsonl"))
	_ = os.WriteFile(filepath.Join(e.dir, "fork.jsonl"), raw, 0o600)
	e.recordIn(scratch, "fork", "u9", "user", 30, "the fork's own turn")
	if r := e.flush("fork", Options{}); r.Status != Sent || r.Records != 1 {
		t.Fatalf("%+v", r)
	}
	e.expectItems(2, [4]string{"conversation:fork@beta", "", "1", "the fork's own turn"})
	if it := e.hs.retains[2]; strings.Contains(it.Content, "shared") || it.Tags[0] != "project:beta" {
		t.Errorf("%+v", it)
	}
}

// legacySession is what a build from before per-project documents left:
// alpha's u1 and beta's u2 both in conversation:s1 (segment 2, no project
// in its metadata), sent from this machine, and its cursor.
func legacySession(t *testing.T) (e *env, alpha, beta string) {
	e = newEnv(t)
	alpha, beta = e.repo("alpha"), e.repo("beta")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	path := filepath.Join(e.dir, "s1.jsonl")
	fi, _ := os.Stat(path)
	fp, _ := Fingerprint(path, fi.Size())
	e.hs.docs["conversation:s1"] = map[string]string{"last_uuid": "u2", "segment": "2", "session_id": "s1"}
	_ = e.f.Index.Add("u1", "u2", sessionKey("claude-memory", "s1", "u1"), sessionKey("claude-memory", "s1", "u2"))
	if err := SaveCursor(e.f.Dirs.CursorsDir(), "s1", Cursor{Transcript: path, Offset: fi.Size(), Fingerprint: fp, LastUUID: "u2", Records: 2,
		Segment: 2, Document: "conversation:s1", Bank: "claude-memory", SentAt: e.clock}); err != nil {
		t.Fatal(err)
	}
	return e, alpha, beta
}

// A cursor from an older build, whose conversation:s1 mixes projects: the
// records it holds are not resent, a new beta record starts beta's own
// document, and the legacy position survives a later lost cursor (beta's
// u2 has no document of its own to be found in).
func TestALegacyMixedDocumentIsNotResent(t *testing.T) {
	e, _, beta := legacySession(t)
	e.recordIn(beta, "s1", "u3", "user", 60, "beta three")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.expectItems(0, [4]string{"conversation:s1@beta", "", "1", "beta three"})
	if c := e.hs.retains[0].Content; strings.Contains(c, "two") || strings.Contains(c, "one") {
		t.Errorf("resent: %s", c)
	}

	e, alpha, _ := legacySession(t)
	e.recordIn(alpha, "s1", "u3", "user", 60, "alpha three")
	expect(t, e.flush("s1", Options{}), Sent, 1, 3)
	e.expectItems(0, [4]string{"conversation:s1", "append", "3", "alpha three"})
	if m := e.hs.retains[0].Metadata; m["legacy_last_uuid"] != "u2" || m["project"] != "alpha" {
		t.Errorf("metadata %v", m)
	}
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	e.recordIn(alpha, "s1", "u4", "user", 120, "alpha four")
	expect(t, e.flush("s1", Options{}), Sent, 1, 4)
	e.expectItems(1, [4]string{"conversation:s1", "append", "4", "alpha four"})
}

// The cursor is saved after each segment: when the second document's send
// starts, the cursor on disk is already past the first document's records.
func TestTheCursorIsSavedAfterEachSegment(t *testing.T) {
	e := newEnv(t)
	beta := e.repo("beta")
	e.record("s1", "u1", "user", 0, "billing one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	var seen Cursor
	e.hs.onRetain = func(it hindsight.RetainItem) {
		if it.DocumentID == "conversation:s1@beta" {
			seen, _, _ = LoadCursor(e.f.Dirs.CursorsDir(), "s1")
		}
	}
	e.hs.refuse = map[string]int{"conversation:s1@beta": http.StatusServiceUnavailable}
	if r := e.flush("s1", Options{}); r.Status != Sent || r.Records != 1 || r.FailedDocument != "conversation:s1@beta" {
		t.Fatalf("%+v", r)
	}
	if seen.LastUUID != "u1" || seen.Segments["billing"] != 1 || seen.Segment != 1 {
		t.Errorf("cursor when beta's send started: %+v", seen)
	}
}

// Replayed explicit saves take their record's project by the session's
// rule: a save made from a plain directory mid-session gets the session's
// project, one kept out (after an excluded repository) is not replayed.
func TestReplayedSavesFollowTheSessionsProjectRule(t *testing.T) {
	e := newEnv(t)
	alpha, secret := e.repo("alpha"), e.repo("secret")
	scratch := filepath.Join(filepath.Dir(e.cwd), "scratch")
	e.f.Exclude = []string{secret}
	store := func(id, text string) []transcript.ToolUse {
		return []transcript.ToolUse{{ID: id, Name: "mcp__plugin_backsight_memory__memory_store", Input: map[string]json.RawMessage{"text": json.RawMessage(strconv.Quote(text))}}}
	}
	ts := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	recs := []transcript.Record{
		{UUID: "u1", CWD: alpha, Timestamp: ts, End: 1},
		{UUID: "a1", CWD: scratch, Timestamp: ts, End: 2, Tools: store("t1", "We keep the export page at 200.")},
		{UUID: "u2", CWD: secret, Timestamp: ts, End: 3},
		{UUID: "a2", CWD: scratch, Timestamp: ts, End: 4, Tools: store("t2", "confidential note")},
	}
	notes := e.f.Notes("s1", recs)
	if len(notes) != 1 || notes[0].Project != "alpha" || !strings.Contains(notes[0].Content, "export page") {
		t.Errorf("%+v", notes)
	}
}

// A plain directory named like an excluded repository (~/copies/secret
// after ~/work/secret) does not take the excluded project back: only the
// excluded directory itself, or a pause, could.
func TestANamesakeOfAnExcludedRepositoryStaysOut(t *testing.T) {
	e := newEnv(t)
	alpha, secret := e.repo("alpha"), e.repo("secret")
	copies := filepath.Join(filepath.Dir(filepath.Dir(e.cwd)), "copies", "secret")
	_ = os.MkdirAll(copies, 0o700)
	e.f.Exclude = []string{secret}
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(secret, "s1", "u2", "user", 1, "confidential repo")
	e.recordIn(copies, "s1", "u3", "user", 2, "confidential copy")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.noSecret()
	for _, it := range e.hs.retains {
		if strings.Contains(it.Content, "confidential") {
			t.Errorf("sent: %s", it.Content)
		}
	}
}

// The checkpoint between two segments cannot be saved: the cursor file is
// removed, so a crash before the end resyncs instead of trusting it.
func TestAFailedCheckpointRemovesTheCursor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("renaming onto a directory differs on Windows")
	}
	e := newEnv(t)
	beta := e.repo("beta")
	e.record("s1", "u1", "user", 0, "billing one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	p := filepath.Join(e.f.Dirs.CursorsDir(), "s1.json")
	present := true
	e.hs.onRetain = func(it hindsight.RetainItem) {
		switch it.DocumentID {
		case "conversation:s1":
			_ = os.MkdirAll(p, 0o700) // the checkpoint's rename onto it fails
		case "conversation:s1@beta":
			_, err := os.Stat(p)
			present = err == nil
		}
	}
	if r := e.flush("s1", Options{}); r.Status != Sent || r.Records != 2 {
		t.Fatalf("%+v", r)
	}
	if present {
		t.Error("the cursor that could not be saved was left in place")
	}
}

// Positions are per document in the restore reconcile too: a restore loses
// beta's document while the home document still holds the cursor's last
// record; beta's records are dropped and resent from its segment 1.
func TestReconcileDropsWhatASideDocumentLost(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.repo("alpha"), e.repo("beta")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	e.recordIn(alpha, "s1", "u3", "user", 2, "alpha three")
	expect(t, e.flush("s1", Options{}), Sent, 3, 2)
	snap := e.hs.backup()
	delete(snap.docs, "conversation:s1@beta")
	snap.ops = map[string]bool{}
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	e.clock = clockAt(13, 30)
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || !reflect.DeepEqual(rec.Resend, []string{"s1"}) {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	sent := len(e.hs.retains)
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.expectItems(sent, [4]string{"conversation:s1@beta", "", "1", "beta two"})
}

// A reconcile that stopped half way already dropped a record's session key
// (its .dropped mark): the next run still counts the record as lost.
func TestReconcileCountsWhatAnEarlierRunDropped(t *testing.T) {
	e := newEnv(t)
	alpha := e.repo("alpha")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	snap := e.hs.backup()
	e.recordIn(alpha, "s1", "u2", "user", 60, "alpha two")
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	e.clock = clockAt(13, 30)
	// the earlier run: marked u2 dropped and removed its key, then stopped
	if err := e.f.addDropped("s1", dropped{Records: map[string]bool{"u2": true}}); err != nil {
		t.Fatal(err)
	}
	_ = e.f.Index.Remove(sessionKey("claude-memory", "s1", "u2"))
	rec, err := e.f.Reconcile(context.Background())
	if err != nil || !reflect.DeepEqual(rec.Resend, []string{"s1"}) {
		t.Fatalf("reconcile: %+v %v", rec, err)
	}
	sent := len(e.hs.retains)
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	e.expectItems(sent, [4]string{"conversation:s1", "append", "2", "alpha two"})
}

// A fork's copied parent records are never marked as the fork's own, not
// even when its resync finds them before its document's position: a
// restore that loses the fork's document resends only the fork's turns.
func TestAForksCopiedRecordsStayTheParentsAfterARestore(t *testing.T) {
	e := newEnv(t)
	e.record("parent", "u1", "user", 0, "shared one")
	e.record("parent", "u2", "user", 1, "shared two")
	e.flush("parent", Options{})
	snap := e.hs.backup()
	raw, _ := os.ReadFile(filepath.Join(e.dir, "parent.jsonl"))
	_ = os.WriteFile(filepath.Join(e.dir, "fork.jsonl"), raw, 0o600)
	e.record("fork", "u9", "user", 30, "fork nine")
	expect(t, e.flush("fork", Options{}), Sent, 1, 1)
	_ = os.Remove(filepath.Join(e.f.Dirs.CursorsDir(), "fork.json"))
	e.record("fork", "u10", "user", 60, "fork ten")
	expect(t, e.flush("fork", Options{}), Sent, 1, 2) // resync: u1, u2 lie before conversation:fork's position
	if e.f.Index.Has(sessionKey("claude-memory", "fork", "u1")) {
		t.Error("the parent's record was marked as the fork's own")
	}
	e.hs.restore(snap, 1, clockAt(12, 30), clockAt(13, 30))
	e.clock = clockAt(13, 30)
	if _, err := e.f.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent := len(e.hs.retains)
	e.flush("fork", Options{})
	for _, it := range e.hs.retains[sent:] {
		if strings.Contains(it.Content, "shared") {
			t.Errorf("the parent's records went to the fork's document: %s", it.Content)
		}
	}
	if n := e.hs.countIn("conversation:fork", "fork nine"); n != 1 {
		t.Errorf("fork nine stored %d times", n)
	}
}

// countIn counts the stored lines of doc that contain text.
func (h *fakeHS) countIn(doc, text string) int {
	n := 0
	for _, l := range strings.Split(h.content[doc], "\n") {
		if strings.Contains(l, text) {
			n++
		}
	}
	return n
}

// A resync keeps the home of a per-project cursor: when the first
// directory's name changes (its linked worktree is gone, so its basename
// names it now), the home project keeps conversation:<sid>.
func TestAResyncKeepsTheCursorsHome(t *testing.T) {
	e := newEnv(t)
	alpha := e.repo("alpha")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", alpha, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(filepath.Dir(e.cwd), "alpha-fix")
	git("worktree", "add", "-q", wt)
	// the worktree turn was paused: never sent, but it names the home
	_ = os.MkdirAll(e.f.Dirs.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.SessionsDir(), "s1.json"),
		[]byte(`{"paused":false,"paused_spans":[{"from":"2026-10-24T08:59:00Z","to":"2026-10-24T09:01:00Z"}]}`), 0o600)
	e.recordIn(wt, "s1", "u1", "user", 0, "private worktree turn")
	e.recordIn(alpha, "s1", "u2", "user", 5, "alpha two")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.expectItems(0, [4]string{"conversation:s1", "", "1", "alpha two"})
	git("worktree", "remove", wt)
	home := filepath.Dir(filepath.Dir(e.cwd))
	e.f.Resolver = project.New(home, []string{".claude"}) // a later process: nothing cached
	p := filepath.Join(e.dir, "s1.jsonl")
	raw, _ := os.ReadFile(p)
	_ = os.WriteFile(p, append([]byte(`{"type":"mode","sessionId":"s1"}`+"\n"), raw...), 0o600) // rewritten: resync
	e.recordIn(alpha, "s1", "u3", "user", 60, "alpha three")
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	e.expectItems(1, [4]string{"conversation:s1", "append", "2", "alpha three"})
}

// A resync that finds every record this session sent in its document
// clears a stale <sid>.pending, even with nothing to send: a leftover
// resend mark would later resend a merely pending document as segment 1,
// replacing it.
func TestAResyncClearsAStalePendingMark(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "first")
	e.flush("s1", Options{})
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	_ = os.MkdirAll(e.f.Dirs.CursorsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.CursorsDir(), "s1.pending"), []byte(resendMark), 0o600)
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)
	if _, err := os.Stat(filepath.Join(e.f.Dirs.CursorsDir(), "s1.pending")); !os.IsNotExist(err) {
		t.Errorf("the stale mark outlived the resync: %v", err)
	}
}
