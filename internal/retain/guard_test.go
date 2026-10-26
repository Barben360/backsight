package retain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// guardHS fronts fakeHS with what the look for recreated purged documents
// reads: each document's creation time (the server's clock), the facts
// extracted from it, and the bank's queue.
type guardHS struct {
	hs      *fakeHS
	mu      sync.Mutex
	created map[string]time.Time // documents with no time are old
	units   map[string][]string  // document → the facts extracted from it
	ops     map[string][]hindsight.Operation
	total   map[string]int // a queue longer than its listing
	calls   []string       // every request, "METHOD path?query"
	cleared []string       // facts whose observations were cleared
	retired []string       // facts retired
}

func (g *guardHS) handler(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	q, path := r.URL.Query(), r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/v1/default/banks/claude-memory/documents":
		g.list(w, q)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/claude-memory/memories/list"):
		var items []map[string]any
		if q.Get("offset") == "0" || q.Get("offset") == "" {
			for _, id := range g.units[q.Get("document_id")] {
				items = append(items, map[string]any{"id": id, "tags": []string{"project:billing"}, "document_id": q.Get("document_id")})
			}
		}
		raw, _ := json.Marshal(map[string]any{"items": items, "total": len(items)})
		_, _ = w.Write(raw)
	case strings.HasSuffix(path, "/operations"):
		ops := g.ops[q.Get("status")]
		total := max(g.total[q.Get("status")], len(ops))
		off, _ := strconv.Atoi(q.Get("offset"))
		lim, _ := strconv.Atoi(q.Get("limit"))
		page := ops[min(off, len(ops)):min(off+lim, len(ops))]
		raw, _ := json.Marshal(map[string]any{"operations": page, "total": total})
		_, _ = w.Write(raw)
	case r.Method == http.MethodDelete && strings.HasSuffix(path, "/observations"):
		g.cleared = append(g.cleared, strings.TrimSuffix(path[strings.LastIndex(path, "/memories/")+len("/memories/"):], "/observations"))
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPatch && strings.Contains(path, "/claude-memory/memories/"):
		g.retired = append(g.retired, path[strings.LastIndex(path, "/")+1:])
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodDelete && strings.Contains(path, "/mental-models/"):
		w.WriteHeader(http.StatusNotFound)
	default:
		if r.Method == http.MethodDelete && strings.Contains(path, "/claude-memory/documents/") {
			id, _ := url.PathUnescape(r.URL.EscapedPath()[strings.Index(r.URL.EscapedPath(), "/documents/")+len("/documents/"):])
			delete(g.units, id)
			delete(g.created, id)
		}
		g.mu.Unlock()
		g.hs.handler(w, r)
		g.mu.Lock()
	}
}

// list answers the document listing like Hindsight 1.1.0: a time field
// filters (start_date) and orders newest first; limit and offset page.
func (g *guardHS) list(w http.ResponseWriter, q url.Values) {
	g.hs.mu.Lock()
	var ids []string
	for id := range g.hs.docs {
		if !strings.HasPrefix(id, "backsight:") {
			ids = append(ids, id)
		}
	}
	g.hs.mu.Unlock()
	sort.Strings(ids)
	if q.Get("time_field") == "created_at" {
		if s := q.Get("start_date"); s != "" {
			since, _ := time.Parse(time.RFC3339, s)
			ids = slices.DeleteFunc(ids, func(id string) bool { return g.created[id].Before(since) })
		}
		sort.SliceStable(ids, func(i, j int) bool { return g.created[ids[i]].After(g.created[ids[j]]) })
	}
	off, _ := strconv.Atoi(q.Get("offset"))
	lim, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		lim = 100
	}
	type item struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
	}
	items := []item{}
	for _, id := range ids[min(off, len(ids)):min(off+lim, len(ids))] {
		items = append(items, item{ID: id, CreatedAt: g.created[id].Format(time.RFC3339)})
	}
	raw, _ := json.Marshal(map[string]any{"items": items, "total": len(ids)})
	_, _ = w.Write(raw)
}

// count is how many requests matched every one of parts.
func (g *guardHS) count(parts ...string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(c, p)
		}
		if ok {
			n++
		}
	}
	return n
}

// recreate is an older client's append to a purged id: Hindsight 1.1.0
// makes the document again, created now (the server's clock), with the
// facts extracted from the new segment.
func (g *guardHS) recreate(doc string, at time.Time, units ...string) {
	g.hs.mu.Lock()
	g.hs.docs[doc] = map[string]string{"session_id": "s1"}
	g.hs.mu.Unlock()
	g.mu.Lock()
	g.created[doc], g.units[doc] = at, units
	g.mu.Unlock()
}

func (g *guardHS) exists(doc string) bool {
	g.hs.mu.Lock()
	defer g.hs.mu.Unlock()
	_, ok := g.hs.docs[doc]
	return ok
}

// serverAt: the server's clock, two hours behind this machine's (another
// host): the look never compares the two.
func serverAt(hour, minute int) time.Time { return clockAt(hour, minute).Add(-2 * time.Hour) }

// guardEnv: conversation:s1 was sent then purged on this machine; the
// bank also holds s3 (never purged) and s1's successor document.
func guardEnv(t *testing.T) (*env, *guardHS) {
	e := newEnv(t)
	g := &guardHS{hs: e.hs, created: map[string]time.Time{}, units: map[string][]string{}, ops: map[string][]hindsight.Operation{}, total: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(g.handler))
	t.Cleanup(srv.Close)
	e.f.Client, _ = hindsight.New(srv.URL, "k", 2*time.Second)
	e.clock = clockAt(12, 0)
	e.record("s3", "v1", "user", 10, "kept")
	expect(t, e.flush("s3", Options{}), Sent, 1, 1)
	e.record("s1", "u1", "user", 30, "the deploy token is hunter2-leaked")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	g.created["conversation:s3"], g.created["conversation:s1"] = serverAt(12, 0), serverAt(12, 0)
	e.clock = clockAt(12, 10)
	e.purge("conversation:s1")
	g.recreate("conversation:s1#2", serverAt(12, 20), "k1") // the successor: later turns, kept
	e.clock = clockAt(12, 30)
	return e, g
}

func guardOf(t *testing.T, e *env) state.PurgeGuard {
	t.Helper()
	st, err := state.Load(e.f.Dirs.StateFile())
	if err != nil || st.PurgeGuard == nil {
		t.Fatalf("no purge guard in state: %v", err)
	}
	return *st.PurgeGuard
}

// An older client appended to the purged id: the look finds it and purges
// it again as forget --purge does (the facts extracted from the new
// segment have their observations cleared and are retired, then the
// document goes), and leaves alone the successor and the documents no
// tombstone names. A second look does nothing.
func TestGuardPurgesARecreatedPurgedDocument(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25), "f1", "f2")
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) || got.Purged.Observations != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if !slices.Equal(g.cleared, []string{"f1", "f2"}) || !slices.Equal(g.retired, []string{"f1", "f2"}) {
		t.Errorf("facts: cleared %v retired %v", g.cleared, g.retired)
	}
	if g.exists("conversation:s1") || !g.exists("conversation:s1#2") || !g.exists("conversation:s3") || !slices.Equal(e.hs.deleted, []string{"conversation:s1"}) {
		t.Errorf("deleted %v", e.hs.deleted)
	}
	pg := guardOf(t, e)
	if !slices.Equal(pg.Purged, []string{"conversation:s1"}) || len(pg.Waiting) != 0 || !pg.Newest.Equal(serverAt(12, 25)) {
		t.Errorf("state %+v", pg)
	}
	e.clock = clockAt(12, 45)
	again, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || !again.Purged.Empty() || len(e.hs.deleted) != 1 {
		t.Errorf("second look: %+v %v, deleted %v", again, err, e.hs.deleted)
	}
}

// The look between full ones costs one listing of the documents created
// since the newest one seen (less the overlap), whatever the number of
// tombstones: no tombstoned document is read one by one. One recreated in
// that window is found.
func TestGuardLooksOnlyAtNewDocumentsBetweenFullLooks(t *testing.T) {
	e, g := guardEnv(t)
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil || !got.Full {
		t.Fatalf("first look: %+v %v", got, err)
	}
	g.mu.Lock()
	g.calls = nil
	g.mu.Unlock()
	e.clock = clockAt(12, 45)
	g.recreate("conversation:s1", serverAt(12, 40), "f1")
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || got.Full || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Fatalf("%+v %v", got, err)
	}
	want := url.Values{"time_field": {"created_at"}, "start_date": {serverAt(12, 20).Add(-time.Hour).Format(time.RFC3339)}}.Encode()
	if g.count("GET /v1/default/banks/claude-memory/documents?") != 1 || g.count(url.QueryEscape(serverAt(12, 20).Add(-time.Hour).Format(time.RFC3339))) != 1 {
		t.Errorf("listings (want one with %s): %v", want, g.calls)
	}
	if n := g.count("GET /v1/default/banks/claude-memory/documents/conversation"); n != 0 {
		t.Errorf("%d tombstoned documents read one by one between full looks", n)
	}
}

// A full look reads every tombstone: on the first look, once a day, and
// when a tombstone was learned since the last one (another machine's
// purge, whose document may have come back before this machine knew).
func TestGuardLooksAtEveryTombstoneWhenOneIsNew(t *testing.T) {
	e, g := guardEnv(t)
	if _, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil {
		t.Fatal(err)
	}
	// conversation:s3, created long ago, is purged on another machine; an
	// older client there appended to it before the tombstone arrived here.
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.AddTombstones(state.Tombstone{Bank: "claude-memory", Document: "conversation:s3", Session: "s3", PurgedAt: clockAt(12, 31)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.clock = clockAt(12, 45)
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || !got.Full || !slices.Equal(got.Purged.Documents, []string{"conversation:s3"}) || g.exists("conversation:s3") {
		t.Fatalf("%+v %v", got, err)
	}
	e.clock = clockAt(13, 0)
	if got, _ := e.f.GuardPurges(context.Background(), GuardOptions{}); got.Full {
		t.Errorf("full again with no new tombstone: %+v", got)
	}
	e.clock = clockAt(12, 45).Add(24 * time.Hour)
	if got, _ := e.f.GuardPurges(context.Background(), GuardOptions{}); !got.Full {
		t.Errorf("no full look after a day: %+v", got)
	}
}

// A long tombstone list is compared with the bank's listing, which takes
// fewer requests than reading each one (most are gone: a 404 each).
func TestGuardBatchesALongTombstoneList(t *testing.T) {
	e, g := guardEnv(t)
	var ts []state.Tombstone
	for i := range 500 {
		ts = append(ts, state.Tombstone{Bank: "claude-memory", Document: "conversation:old" + strconv.Itoa(i), PurgedAt: clockAt(11, 0)})
	}
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error { s.AddTombstones(ts...); return nil }); err != nil {
		t.Fatal(err)
	}
	g.recreate("conversation:old7", serverAt(9, 0), "f7")
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:old7"}) || got.Checked != 501 {
		t.Fatalf("%+v %v", got, err)
	}
	if n := g.count("GET /v1/default/banks/claude-memory/documents/conversation%3Aold"); n > 1 {
		t.Errorf("%d tombstoned documents read one by one", n)
	}
	if got.Requests > 5 {
		t.Errorf("%d requests for 501 tombstones and 4 documents", got.Requests)
	}
}

// Only a tombstone's exact id is purged again: the successor of a purged
// document is its own, unless a later purge tombstoned it too.
func TestGuardTouchesASuccessorOnlyWhenItIsTombstoned(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	if got, _ := e.f.GuardPurges(context.Background(), GuardOptions{}); !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) || !g.exists("conversation:s1#2") {
		t.Fatalf("%+v, deleted %v", got, e.hs.deleted)
	}
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.AddTombstones(state.Tombstone{Bank: "claude-memory", Document: "conversation:s1#2", Session: "s1", PurgedAt: clockAt(12, 40)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.clock = clockAt(12, 50)
	if got, _ := e.f.GuardPurges(context.Background(), GuardOptions{}); !slices.Equal(got.Purged.Documents, []string{"conversation:s1#2"}) || g.exists("conversation:s1#2") {
		t.Errorf("a tombstoned successor: %+v", got)
	}
	if !g.exists("conversation:s3") {
		t.Error("a document no tombstone names was deleted")
	}
}

// Never under a maintenance lock, a restore or an update in progress:
// nothing is read or deleted, and the next look does it.
func TestGuardWaitsForMaintenanceAndMarkers(t *testing.T) {
	for name, block := range map[string]func(e *env) func(){
		"maintenance lock": func(e *env) func() {
			_ = os.MkdirAll(filepath.Dir(e.f.Dirs.MaintenanceLock()), 0o700)
			_ = os.WriteFile(e.f.Dirs.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"command":"backup create"}`), 0o600)
			return func() { _ = os.Remove(e.f.Dirs.MaintenanceLock()) }
		},
		"restore marker": func(e *env) func() {
			_ = state.WriteRestoreMarker(e.f.Dirs, state.RestoreMarker{Command: "restore", Archive: "/b/a.tar.gz.age", StartedAt: clockAt(12, 20)})
			return func() { _ = state.ClearRestoreMarker(e.f.Dirs) }
		},
		"update marker": func(e *env) func() {
			_ = state.WriteUpdateMarker(e.f.Dirs, state.UpdateMarker{From: "v1", To: "v2", StartedAt: clockAt(12, 20)})
			return func() { _ = os.Remove(e.f.Dirs.UpdateMarker()) }
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, g := guardEnv(t)
			g.recreate("conversation:s1", serverAt(12, 25), "f1")
			done := block(e)
			if _, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err == nil || g.exists("conversation:s1") == false || len(g.retired) != 0 {
				t.Fatalf("looked or purged while blocked: %v, deleted %v", err, e.hs.deleted)
			}
			done()
			if got, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
				t.Errorf("after: %+v %v", got, err)
			}
		})
	}
}

// An append queued or running for the recreated document waits: purging
// under it would let it recreate the document again, with facts the purge
// never saw. So does a retain naming no single document, and a queue too
// long to read whole. What waits is in state for doctor; once the queue
// moved, the next look purges it, even outside the window of new
// documents.
func TestGuardWaitsForAQueuedAppend(t *testing.T) {
	for name, queue := range map[string]func(g *guardHS){
		"append to it": func(g *guardHS) {
			g.ops["processing"] = []hindsight.Operation{{ID: "op9", Type: "batch_retain", Status: "processing", DocumentID: "conversation:s1"}}
		},
		"batch of several": func(g *guardHS) {
			g.ops["pending"] = []hindsight.Operation{{ID: "op8", Type: "batch_retain", Status: "pending"}}
		},
		"queue too long": func(g *guardHS) {
			g.ops["pending"] = []hindsight.Operation{{ID: "op7", Type: "consolidation", Status: "pending"}}
			g.total["pending"] = 5000
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, g := guardEnv(t)
			g.recreate("conversation:s1", serverAt(12, 25), "f1")
			g.ops["pending"] = []hindsight.Operation{{ID: "op1", Type: "consolidation", Status: "pending"}, {ID: "op2", Type: "batch_retain", Status: "pending", DocumentID: "conversation:s3"}}
			queue(g)
			got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
			if err != nil || !got.Purged.Empty() || len(got.Waiting) != 1 || got.Waiting[0].Document != "conversation:s1" || !g.exists("conversation:s1") || len(g.retired) != 0 {
				t.Fatalf("%+v %v, deleted %v", got, err, e.hs.deleted)
			}
			if w := guardOf(t, e).Waiting; len(w) != 1 || w[0].Session != "s1" || w[0].Why == "" {
				t.Errorf("waiting in state: %+v", w)
			}
			g.mu.Lock()
			g.ops, g.total = map[string][]hindsight.Operation{}, map[string]int{}
			g.created["conversation:s1"] = serverAt(1, 0) // out of the window: read again as waiting
			g.mu.Unlock()
			e.clock = clockAt(13, 0)
			got, err = e.f.GuardPurges(context.Background(), GuardOptions{})
			if err != nil || got.Full || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) || len(guardOf(t, e).Waiting) != 0 {
				t.Errorf("after the queue: %+v %v", got, err)
			}
		})
	}
}

// With Hold (the claude-code provider without the user's consent: the
// purge makes the server consolidate again), the look finds and records,
// and deletes nothing.
func TestGuardHoldFindsWithoutPurging(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{Hold: "the claude-code provider: run backsight retain purge-again --allow-subscription"})
	if err != nil || !got.Purged.Empty() || len(got.Waiting) != 1 || !strings.Contains(got.Waiting[0].Why, "--allow-subscription") || !g.exists("conversation:s1") || len(g.cleared) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if w := guardOf(t, e).Waiting; len(w) != 1 || w[0].Document != "conversation:s1" {
		t.Errorf("state %+v", w)
	}
}

// A failed purge leaves the document waiting with the error, and the next
// look retries it.
func TestGuardKeepsAFailedPurgeWaiting(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	e.hs.mu.Lock()
	e.hs.deleteStatus = http.StatusInternalServerError
	e.hs.mu.Unlock()
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || len(got.Waiting) != 1 || !strings.Contains(got.Waiting[0].Why, "500") {
		t.Fatalf("%+v %v", got, err)
	}
	e.hs.mu.Lock()
	e.hs.deleteStatus = 0
	e.hs.mu.Unlock()
	e.clock = clockAt(12, 45)
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) || len(guardOf(t, e).Waiting) != 0 {
		t.Errorf("retry: %+v %v", got, err)
	}
}

// No tombstone: nothing to look for, no request.
func TestGuardWithoutTombstonesReadsNothing(t *testing.T) {
	e := newEnv(t)
	g := &guardHS{hs: e.hs, created: map[string]time.Time{}, units: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(g.handler))
	t.Cleanup(srv.Close)
	e.f.Client, _ = hindsight.New(srv.URL, "k", 2*time.Second)
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil || got.Requests != 0 || len(g.calls) != 0 {
		t.Errorf("%+v %v %v", got, err, g.calls)
	}
}
