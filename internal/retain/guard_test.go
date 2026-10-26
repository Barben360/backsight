package retain

import (
	"bytes"
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
	calls   []string       // every request, "METHOD path?query" (path decoded)
	cleared []string       // facts whose observations were cleared
	retired []string       // facts retired
	// docTotal: the document count the listing reports when it is not
	// the documents held (a large bank).
	docTotal int
	// listStatus, opsStatus: non-zero, the document listing or the queue
	// answers with it.
	listStatus, opsStatus int
	// extra: memories the fact listing adds to every answer, whatever
	// document was asked (a server that ignores the filter).
	extra []map[string]any
	// goneOnDelete: deletes of these answer 404 (another machine got there
	// first).
	goneOnDelete map[string]bool
}

func (g *guardHS) handler(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	q, path := r.URL.Query(), r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/v1/default/banks/claude-memory/documents":
		if g.listStatus != 0 {
			w.WriteHeader(g.listStatus)
			return
		}
		g.list(w, q)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/claude-memory/memories/list"):
		var items []map[string]any
		if q.Get("offset") == "0" || q.Get("offset") == "" {
			for _, id := range g.units[q.Get("document_id")] {
				items = append(items, map[string]any{"id": id, "tags": []string{"project:billing"}, "document_id": q.Get("document_id"), "fact_type": "world"})
			}
			items = append(items, g.extra...)
		}
		raw, _ := json.Marshal(map[string]any{"items": items, "total": len(items)})
		_, _ = w.Write(raw)
	case strings.HasSuffix(path, "/operations"):
		if g.opsStatus != 0 {
			w.WriteHeader(g.opsStatus)
			return
		}
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
			if g.goneOnDelete[id] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail":"not found"}`))
				return
			}
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
		// Hindsight 1.1.0 sends microseconds with a zone offset.
		items = append(items, item{ID: id, CreatedAt: g.created[id].Format("2006-01-02T15:04:05.000000-07:00")})
	}
	total := len(ids)
	if g.docTotal > 0 && q.Get("limit") == "1" { // the count the full look asks for
		total = g.docTotal
	}
	raw, _ := json.Marshal(map[string]any{"items": items, "total": total})
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

func (g *guardHS) reset() {
	g.mu.Lock()
	g.calls = nil
	g.mu.Unlock()
}

// writes are the requests that change something on the server.
func (g *guardHS) writes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
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

// const directGet: a tombstoned document read one by one (paths are
// decoded in calls).
const directGet = "GET /v1/default/banks/claude-memory/documents/conversation:"

// guardEnv: conversation:s1 was sent then purged on this machine; the
// bank also holds s3 (never purged) and s1's successor document.
func guardEnv(t *testing.T) (*env, *guardHS) {
	e := newEnv(t)
	g := &guardHS{hs: e.hs, created: map[string]time.Time{}, units: map[string][]string{}, ops: map[string][]hindsight.Operation{}, total: map[string]int{}, goneOnDelete: map[string]bool{}}
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

func stateBytes(t *testing.T, e *env) []byte {
	t.Helper()
	raw, _ := os.ReadFile(e.f.Dirs.StateFile())
	return raw
}

func addTombstone(t *testing.T, e *env, ts ...state.Tombstone) {
	t.Helper()
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error { s.AddTombstones(ts...); return nil }); err != nil {
		t.Fatal(err)
	}
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

// The tick's look lists the documents created since the newest one seen,
// less the overlap, in one request (the exact query), and never reads a
// tombstoned document one by one; the first one asks for the newest
// document once. A document created before the window is left to the
// full look of jobs maintain.
func TestGuardIncrementalLookCostsOneListing(t *testing.T) {
	e, g := guardEnv(t)
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil || got.Full {
		t.Fatalf("first look: %+v %v", got, err)
	}
	if n := g.count("time_field=created_at", "limit=1&"); n != 1 {
		t.Errorf("the first look asked for the newest document %d times: %v", n, g.calls)
	}
	g.reset()
	e.clock = clockAt(12, 45)
	g.recreate("conversation:s1", serverAt(12, 40), "f1")
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || got.Full || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Fatalf("%+v %v", got, err)
	}
	want := "GET /v1/default/banks/claude-memory/documents?" + url.Values{"limit": {"200"}, "offset": {"0"}, "start_date": {serverAt(12, 20).Add(-time.Hour).Format(time.RFC3339)}, "time_field": {"created_at"}}.Encode()
	if n := g.count("GET /v1/default/banks/claude-memory/documents?"); n != 1 || !slices.Contains(g.calls, want) {
		t.Errorf("listings (want only %s): %v", want, g.calls)
	}
	if n := g.count(directGet); n != 0 {
		t.Errorf("%d tombstoned documents read one by one", n)
	}
	// conversation:s3, created long before, is purged on another machine
	// and an older client wrote it again before this one knew: only the
	// full look finds it.
	addTombstone(t, e, state.Tombstone{Bank: "claude-memory", Document: "conversation:s3", Session: "s3", PurgedAt: clockAt(12, 46)})
	g.mu.Lock()
	g.created["conversation:s3"] = serverAt(6, 0)
	g.mu.Unlock()
	e.clock = clockAt(13, 0)
	if got, _ := e.f.GuardPurges(context.Background(), GuardOptions{}); !got.Purged.Empty() || !g.exists("conversation:s3") {
		t.Fatalf("the incremental look found an old document: %+v", got)
	}
	got, err = e.f.GuardPurges(context.Background(), GuardOptions{Full: true})
	if err != nil || !got.Full || !slices.Equal(got.Purged.Documents, []string{"conversation:s3"}) || g.exists("conversation:s3") || !g.exists("conversation:s1#2") {
		t.Fatalf("full look: %+v %v", got, err)
	}
	if pg := guardOf(t, e); !pg.FullAt.Equal(clockAt(13, 0)) {
		t.Errorf("state %+v", pg)
	}
}

// The full look reads tombstoned documents one by one when that takes
// fewer requests than the listing (a big bank), and the listing otherwise;
// at ceil(documents/200) tombstones, one by one. The newest document is
// asked for once, and gives the count.
func TestGuardFullLookPicksTheCheaperWay(t *testing.T) {
	for _, c := range []struct {
		name               string
		tombstones, total  int
		gets, listingPages int
	}{
		{"many tombstones, small bank", 500, 0, 0, 1},
		{"at the boundary", 65, 65 * 200, 65, 0},
		{"past the boundary", 65, 64 * 200, 0, 1},
		{"few tombstones, big bank", 100, 50000, 100, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, g := guardEnv(t)
			var ts []state.Tombstone
			for i := range c.tombstones - 1 { // conversation:s1 is the last one
				ts = append(ts, state.Tombstone{Bank: "claude-memory", Document: "conversation:old" + strconv.Itoa(i), PurgedAt: clockAt(11, 0)})
			}
			addTombstone(t, e, ts...)
			g.docTotal = c.total
			g.recreate("conversation:old7", serverAt(9, 0), "f7")
			g.reset()
			got, err := e.f.GuardPurges(context.Background(), GuardOptions{Full: true})
			if err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:old7"}) || got.Checked != c.tombstones {
				t.Fatalf("%+v %v", got, err)
			}
			// Reads before the purge: the purge itself reads old7 once.
			if n := g.count(directGet); n != c.gets {
				t.Errorf("%d tombstoned documents read one by one, want %d", n, c.gets)
			}
			if n := g.count("GET /v1/default/banks/claude-memory/documents?limit=200"); n != c.listingPages {
				t.Errorf("%d listing pages, want %d", n, c.listingPages)
			}
			if n := g.count("time_field=created_at", "limit=1&"); n != 1 {
				t.Errorf("the newest document asked for %d times", n)
			}
		})
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
	addTombstone(t, e, state.Tombstone{Bank: "claude-memory", Document: "conversation:s1#2", Session: "s1", PurgedAt: clockAt(12, 40)})
	e.clock = clockAt(12, 50)
	if got, _ := e.f.GuardPurges(context.Background(), GuardOptions{Full: true}); !slices.Equal(got.Purged.Documents, []string{"conversation:s1#2"}) || g.exists("conversation:s1#2") {
		t.Errorf("a tombstoned successor: %+v", got)
	}
	if !g.exists("conversation:s3") {
		t.Error("a document no tombstone names was deleted")
	}
}

// A tombstone of another bank, or of no bank, names nothing here.
func TestFindRecreatedKeepsToItsBank(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s3", serverAt(12, 25))
	g.reset()
	sc, err := FindRecreated(context.Background(), e.f.Client, "claude-memory", []state.Tombstone{
		{Bank: "team-memory", Document: "conversation:s3"}, {Document: "conversation:s3"}}, Lookup{Documents: -1})
	if err != nil || len(sc.Found) != 0 || sc.Checked != 0 || len(g.calls) != 0 {
		t.Errorf("%+v %v", sc, err)
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
			g.reset()
			if _, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err == nil || !g.exists("conversation:s1") || len(g.retired) != 0 || len(g.calls) != 0 {
				t.Fatalf("looked or purged while blocked: %v, calls %v", err, g.calls)
			}
			done()
			if got, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
				t.Errorf("after: %+v %v", got, err)
			}
		})
	}
}

// A maintenance lock whose owner is gone (a crash mid-backup) is broken:
// the purge goes on; the lock is released after it.
func TestGuardBreaksAStaleMaintenanceLock(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	_ = os.MkdirAll(filepath.Dir(e.f.Dirs.MaintenanceLock()), 0o700)
	_ = os.WriteFile(e.f.Dirs.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(deadPID(t))+`,"command":"backup create"}`), 0o600)
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Fatalf("%+v %v", got, err)
	}
	if state.Inspect(e.f.Dirs.MaintenanceLock()) != nil {
		t.Error("the maintenance lock is still there")
	}
}

// An append queued or running for the recreated document waits: purging
// under it would let it recreate the document again, with facts the purge
// never saw. So does a retain naming no single document, a queue too long
// to read whole, and a queue that cannot be read. What waits is in state
// for doctor; once the queue moved, the next look purges it, even outside
// the window of new documents.
func TestGuardWaitsForAQueuedAppend(t *testing.T) {
	for name, queue := range map[string]func(g *guardHS){
		"append to it": func(g *guardHS) {
			g.ops["processing"] = []hindsight.Operation{{ID: "op9", Type: "batch_retain", Status: "processing", DocumentID: "conversation:s1"}}
		},
		"batch of several": func(g *guardHS) {
			g.ops["pending"] = append(g.ops["pending"], hindsight.Operation{ID: "op8", Type: "batch_retain", Status: "pending"})
		},
		"queue past the pages read": func(g *guardHS) {
			for i := range queuePages * 100 {
				g.ops["pending"] = append(g.ops["pending"], hindsight.Operation{ID: "c" + strconv.Itoa(i), Type: "consolidation", Status: "pending"})
			}
			g.total["pending"] = queuePages*100 + 1
		},
		"queue unreadable": func(g *guardHS) { g.opsStatus = http.StatusInternalServerError },
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
			if w := guardOf(t, e).Waiting; len(w) != 1 || w[0].Session != "s1" || w[0].Why == "" || w[0].NeedsUser {
				t.Errorf("waiting in state: %+v", w)
			}
			g.mu.Lock()
			g.ops, g.total, g.opsStatus = map[string][]hindsight.Operation{}, map[string]int{}, 0
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

// With Hold (the claude-code provider, a remote server), the look finds
// and records, as waiting for the user, and deletes nothing.
func TestGuardHoldFindsWithoutPurging(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	g.reset()
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{Hold: "run backsight retain purge-again --allow-subscription"})
	if err != nil || !got.Purged.Empty() || len(got.Waiting) != 1 || !strings.Contains(got.Waiting[0].Why, "--allow-subscription") || !g.exists("conversation:s1") || len(g.writes()) != 0 {
		t.Fatalf("%+v %v %v", got, err, g.writes())
	}
	if w := guardOf(t, e).Waiting; len(w) != 1 || w[0].Document != "conversation:s1" || !w[0].NeedsUser {
		t.Errorf("state %+v", w)
	}
}

// A plan finds, and writes nothing: neither state nor the server.
func TestGuardPlanWritesNothing(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	before := stateBytes(t, e)
	g.reset()
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{Full: true, Plan: true, Manual: true})
	if err != nil || len(got.Waiting) != 1 || got.Waiting[0].Document != "conversation:s1" || !got.Purged.Empty() {
		t.Fatalf("%+v %v", got, err)
	}
	if !bytes.Equal(before, stateBytes(t, e)) || len(g.writes()) != 0 || !g.exists("conversation:s1") {
		t.Errorf("a plan wrote: %v", g.writes())
	}
}

// A failed purge leaves the document waiting with the error, the lock
// released, the other document purged; the next look retries it.
func TestGuardKeepsAFailedPurgeWaiting(t *testing.T) {
	e, g := guardEnv(t)
	addTombstone(t, e, state.Tombstone{Bank: "claude-memory", Document: "conversation:s3", Session: "s3", PurgedAt: clockAt(12, 11)})
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	g.recreate("conversation:s3", serverAt(12, 26), "f3")
	e.hs.mu.Lock()
	e.hs.failDelete = map[string]bool{"conversation:s1": true}
	e.hs.mu.Unlock()
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s3"}) || len(got.Waiting) != 1 || got.Waiting[0].Document != "conversation:s1" || !strings.Contains(got.Waiting[0].Why, "503") {
		t.Fatalf("%+v %v", got, err)
	}
	if state.Inspect(e.f.Dirs.MaintenanceLock()) != nil {
		t.Error("the maintenance lock is still held after a failed purge")
	}
	if w := guardOf(t, e).Waiting; len(w) != 1 || w[0].Document != "conversation:s1" {
		t.Errorf("state %+v", w)
	}
	e.hs.mu.Lock()
	e.hs.failDelete = nil
	e.hs.mu.Unlock()
	e.clock = clockAt(12, 45)
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) || len(guardOf(t, e).Waiting) != 0 {
		t.Errorf("retry: %+v %v", got, err)
	}
}

// A document already gone when its delete runs (another machine purged it
// first) counts as purged, not waiting.
func TestGuardTakesAGoneDocumentForPurged(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25))
	g.goneOnDelete["conversation:s1"] = true
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) || len(got.Waiting) != 0 {
		t.Errorf("%+v %v", got, err)
	}
}

// Only the raw facts of the document are cleared and retired: an
// observation listed with them is left to its sources, and a listing that
// names a fact of another document (a filter the server ignored) purges
// nothing at all.
func TestGuardTouchesOnlyTheDocumentsOwnRawFacts(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	g.extra = []map[string]any{{"id": "o1", "fact_type": "observation", "document_id": "conversation:s1"}}
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Fatalf("%+v %v", got, err)
	}
	if !slices.Equal(g.cleared, []string{"f1"}) || !slices.Equal(g.retired, []string{"f1"}) {
		t.Errorf("an observation was curated: cleared %v retired %v", g.cleared, g.retired)
	}

	e2, g2 := guardEnv(t)
	g2.recreate("conversation:s1", serverAt(12, 25), "f1")
	g2.extra = []map[string]any{{"id": "x9", "fact_type": "world", "document_id": "conversation:s3"}}
	got, err := e2.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || !got.Purged.Empty() || len(got.Waiting) != 1 || !strings.Contains(got.Waiting[0].Why, "other documents") {
		t.Fatalf("%+v %v", got, err)
	}
	if len(g2.cleared) != 0 || len(g2.retired) != 0 || !g2.exists("conversation:s1") || !g2.exists("conversation:s3") {
		t.Errorf("touched: cleared %v retired %v deleted %v", g2.cleared, g2.retired, e2.hs.deleted)
	}
}

// An older client that keeps writing to a purged id is purged again
// repeatMax times a day unattended; then the document waits for the user,
// with what to do. The user's own run still purges it.
func TestGuardStopsPurgingAnIdAnOlderClientKeepsWriting(t *testing.T) {
	e, g := guardEnv(t)
	for i := range repeatMax + 1 {
		e.clock = clockAt(12, 30+10*i)
		g.recreate("conversation:s1", serverAt(12, 25+10*i), "f"+strconv.Itoa(i))
		got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if i < repeatMax && !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
			t.Fatalf("look %d: %+v", i, got)
		}
		if i == repeatMax && (!got.Purged.Empty() || len(got.Waiting) != 1 || !got.Waiting[0].NeedsUser || !strings.Contains(got.Waiting[0].Why, "keeps writing")) {
			t.Fatalf("look %d purged again: %+v", i, got)
		}
	}
	if !g.exists("conversation:s1") {
		t.Fatal("deleted past the cap")
	}
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{Full: true, Manual: true}); err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Errorf("the user's run: %+v %v", got, err)
	}
}

// At most perLook documents are purged per look; the rest wait for the
// next one.
func TestGuardPurgesABoundedBatchPerLook(t *testing.T) {
	e, g := guardEnv(t)
	var ts []state.Tombstone
	for i := range perLook + 2 {
		d := "conversation:b" + strconv.Itoa(i)
		ts = append(ts, state.Tombstone{Bank: "claude-memory", Document: d, PurgedAt: clockAt(12, 11)})
		g.recreate(d, serverAt(12, 25))
	}
	addTombstone(t, e, ts...)
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || len(got.Purged.Documents) != perLook || len(got.Waiting) != 2 {
		t.Fatalf("%d purged, %d waiting: %v", len(got.Purged.Documents), len(got.Waiting), err)
	}
	e.clock = clockAt(12, 45)
	if got, _ := e.f.GuardPurges(context.Background(), GuardOptions{}); len(got.Purged.Documents) != 2 {
		t.Errorf("next look: %+v", got)
	}
}

// A read that fails changes nothing: state keeps its window (no document
// is skipped), nothing is deleted.
func TestGuardChangesNothingWhenTheServerFails(t *testing.T) {
	e, g := guardEnv(t)
	if _, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil {
		t.Fatal(err)
	}
	before := stateBytes(t, e)
	g.recreate("conversation:s1", serverAt(12, 40), "f1")
	g.listStatus = http.StatusInternalServerError
	e.clock = clockAt(13, 30)
	for _, opt := range []GuardOptions{{}, {Full: true}} {
		if _, err := e.f.GuardPurges(context.Background(), opt); err == nil {
			t.Errorf("%+v: no error", opt)
		}
	}
	if !bytes.Equal(before, stateBytes(t, e)) || !g.exists("conversation:s1") {
		t.Fatal("state moved, or something was deleted, on a failed read")
	}
	g.listStatus = 0
	if got, _ := e.f.GuardPurges(context.Background(), GuardOptions{}); !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Errorf("after: %+v", got)
	}
}

// A corrupt state.json stops the look: nothing read, nothing deleted.
func TestGuardStopsOnACorruptState(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	_ = os.WriteFile(e.f.Dirs.StateFile(), []byte("{torn"), 0o600)
	if _, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err == nil || !g.exists("conversation:s1") {
		t.Errorf("%v", err)
	}
}

// A creation time far past this machine's clock never becomes the
// window's start (it would hide every later recreation), and a full look
// sets the newest again from the server, even backwards.
func TestGuardWindowIgnoresAFutureDate(t *testing.T) {
	e, g := guardEnv(t)
	g.recreate("conversation:wrong-clock", serverAt(12, 20).Add(30*24*time.Hour))
	if _, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := guardOf(t, e).Newest; n.After(clockAt(12, 30).Add(clockMargin)) {
		t.Fatalf("newest pinned in the future: %v", n)
	}
	e.clock = clockAt(12, 45)
	g.recreate("conversation:s1", serverAt(12, 40), "f1")
	if got, _ := e.f.GuardPurges(context.Background(), GuardOptions{}); !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Errorf("missed after a future date: %+v", got)
	}
	// A state pinned by an older look: the full look sets it again.
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.PurgeGuard.Newest = serverAt(12, 0).Add(10 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	delete(g.created, "conversation:wrong-clock")
	g.mu.Unlock()
	if _, err := e.f.GuardPurges(context.Background(), GuardOptions{Full: true}); err != nil {
		t.Fatal(err)
	}
	if n := guardOf(t, e).Newest; !n.Before(serverAt(12, 0).Add(10 * time.Hour)) {
		t.Errorf("the full look kept a later newest: %v", n)
	}
}

// Another bank's record (the bank changed in the config) is not this
// one's: the look starts from this bank's newest document.
func TestGuardIgnoresAnotherBanksRecord(t *testing.T) {
	e, g := guardEnv(t)
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.PurgeGuard = &state.PurgeGuard{Bank: "team-memory", Newest: serverAt(23, 0), Waiting: []state.Recreated{{Document: "conversation:s3"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	g.recreate("conversation:s1", serverAt(12, 25), "f1")
	got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
	if err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) || g.count("time_field=created_at", "limit=1&") != 1 {
		t.Errorf("%+v %v", got, err)
	}
	if pg := guardOf(t, e); pg.Bank != "claude-memory" {
		t.Errorf("state %+v", pg)
	}
}

// A look that changed nothing within the hour leaves state.json alone.
func TestGuardQuietLookWritesNoState(t *testing.T) {
	e, _ := guardEnv(t)
	if _, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil {
		t.Fatal(err)
	}
	before := stateBytes(t, e)
	e.clock = clockAt(12, 45)
	if _, err := e.f.GuardPurges(context.Background(), GuardOptions{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, stateBytes(t, e)) {
		t.Error("state.json rewritten by a quiet look")
	}
	e.clock = clockAt(12, 30).Add(LookRecordEvery)
	_, _ = e.f.GuardPurges(context.Background(), GuardOptions{})
	if bytes.Equal(before, stateBytes(t, e)) {
		t.Error("the time of the look not recorded after LookRecordEvery")
	}
}

// A restore moves the server to another state: the reconcile resets the
// window, and the next maintain looks at every tombstone again.
func TestReconcileResetsTheLookAfterARestore(t *testing.T) {
	e := purgedEnv(t)
	if err := state.Update(e.f.Dirs, "test", func(s *state.State) error {
		s.PurgeGuard = &state.PurgeGuard{Bank: "claude-memory", Newest: serverAt(13, 0), FullAt: clockAt(13, 0),
			Waiting: []state.Recreated{{Document: "conversation:s1"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec, err := e.f.Reconcile(context.Background()); err != nil || rec.To != 1 {
		t.Fatalf("%+v %v", rec, err)
	}
	st, _ := state.Load(e.f.Dirs.StateFile())
	if g := st.PurgeGuard; g == nil || !g.Newest.IsZero() || !g.FullAt.IsZero() || len(g.Waiting) != 1 {
		t.Errorf("after the restore: %+v", g)
	}
}

// No tombstone: nothing to look for, no request.
func TestGuardWithoutTombstonesReadsNothing(t *testing.T) {
	e := newEnv(t)
	g := &guardHS{hs: e.hs, created: map[string]time.Time{}, units: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(g.handler))
	t.Cleanup(srv.Close)
	e.f.Client, _ = hindsight.New(srv.URL, "k", 2*time.Second)
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{Full: true}); err != nil || got.Requests != 0 || len(g.calls) != 0 {
		t.Errorf("%+v %v %v", got, err, g.calls)
	}
}

// A rollback: an older binary, unaware of tombstones, appended the
// session's later turns to the purged id after the purge (its cursor
// names that id). The look purges the recreated document again; the next
// flush sends those turns again, to the live document: nothing lost, and
// nothing the purge covered comes back.
func TestTurnsAnOlderBinarySentToAPurgedIDGoAgainToTheLiveDocument(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "the deploy token is hunter2-leaked")
	e.record("s1", "a1", "assistant", 1, "Noted the hunter2 token.")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	e.clock = clockAt(9, 5) // the purge, before the later turns
	e.purge("conversation:s1")
	e.clock = clockAt(12, 0)
	e.record("s1", "u2", "user", 10, "a later turn after the purge")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1) // to conversation:s1#2
	// The rollback: the older binary's flush of u3, a3.
	e.record("s1", "u3", "user", 20, "invoices now round half up")
	e.record("s1", "a3", "assistant", 21, "Noted: half up.")
	path := filepath.Join(e.dir, "s1.jsonl")
	fi, _ := os.Stat(path)
	cur, ok, err := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
	if err != nil || !ok {
		t.Fatal(err)
	}
	cur.SegmentDocs = nil // a field it does not know: its cursor names conversation:s1
	cur.Offset, cur.LastUUID = fi.Size(), "a3"
	cur.Fingerprint, _ = Fingerprint(path, cur.Offset)
	if err := SaveCursor(e.f.Dirs.CursorsDir(), "s1", cur); err != nil {
		t.Fatal(err)
	}
	_ = e.f.Index.Add("u3", sessionKey("claude-memory", "s1", "u3"), "a3", sessionKey("claude-memory", "s1", "a3"))
	e.hs.mu.Lock()
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "a3", "segment": "2", "project": "billing"}
	e.hs.content["conversation:s1"] = "user: invoices now round half up\nassistant: Noted: half up."
	e.hs.mu.Unlock()
	// The upgrade: the look purges the recreated document again.
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{Full: true}); err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Fatalf("%+v %v", got, err)
	}
	sent := len(e.hs.retains)
	r := e.flush("s1", Options{})
	if r.Status != Sent || r.Records != 2 {
		t.Fatalf("the older binary's turns were not sent again: %+v", r)
	}
	e.hs.mu.Lock()
	again := slices.Clone(e.hs.retains[sent:])
	e.hs.mu.Unlock()
	for _, it := range again {
		if it.DocumentID != "conversation:s1#2" {
			t.Errorf("sent to %s", it.DocumentID)
		}
	}
	if !e.sentText(sent, "round half up") || e.sentText(sent, "hunter2") || e.sentText(sent, "a later turn") {
		t.Errorf("resent: %+v", again)
	}
	if _, ok := e.hs.docs["conversation:s1"]; ok {
		t.Error("the purged id was written again")
	}
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)
}

// A turn this binary sent before the purge, whose async retain was still
// queued (the document's metadata did not name it yet), is past the
// tombstone's last record; the queued retain then recreates the document
// with it, and the look purges that again. It is never sent again to the
// live document: the user purged the session as it stood. A turn written
// after the purge still goes.
func TestATurnQueuedAtThePurgeIsNeverSentAgain(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u0", "user", 0, "a plain first turn")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.record("s1", "u1", "user", 10, "the deploy token is hunter2-leaked")
	e.hs.mu.Lock()
	e.hs.stale = true // the retain of u1 is queued: the metadata still names u0
	e.hs.mu.Unlock()
	expect(t, e.flush("s1", Options{}), Sent, 1, 2)
	e.hs.mu.Lock()
	e.hs.stale = false
	e.hs.mu.Unlock()
	e.clock = clockAt(12, 5)
	e.purge("conversation:s1") // last record u0: u1 is not in the metadata yet
	// The queued retain runs: the document is back, holding u1.
	e.hs.mu.Lock()
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "u1", "segment": "2", "project": "billing"}
	e.hs.content["conversation:s1"] = "user: the deploy token is hunter2-leaked"
	e.hs.mu.Unlock()
	if got, err := e.f.GuardPurges(context.Background(), GuardOptions{Full: true}); err != nil || !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Fatalf("%+v %v", got, err)
	}
	sent := len(e.hs.retains)
	expect(t, e.flush("s1", Options{}), Nothing, 0, 0)
	if e.sentText(sent, "hunter2") {
		t.Fatal("the purged turn was sent again")
	}
	// A turn written after the purge goes, to the live document, alone.
	e.record("s1", "u2", "user", 200, "a turn after the purge")
	e.clock = clockAt(13, 0)
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	if it := e.hs.retains[len(e.hs.retains)-1]; it.DocumentID != "conversation:s1#2" || e.sentText(sent, "hunter2") {
		t.Errorf("%+v", it)
	}
}

// The cap counts the purges of the last 24 hours, sliding: a day after
// the first of three, one more is allowed, and the next one waits again
// while the other two are within the day.
func TestGuardRepeatCapSlides(t *testing.T) {
	e, g := guardEnv(t)
	look := func(after time.Duration) Guarded {
		t.Helper()
		e.clock = clockAt(12, 30).Add(after)
		g.recreate("conversation:s1", serverAt(12, 25).Add(after), "f")
		got, err := e.f.GuardPurges(context.Background(), GuardOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, after := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		if got := look(after); !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
			t.Fatalf("+%v: %+v", after, got)
		}
	}
	if got := look(3 * time.Hour); !got.Purged.Empty() {
		t.Fatalf("a fourth within the day: %+v", got)
	}
	if got := look(24*time.Hour + 30*time.Minute); !slices.Equal(got.Purged.Documents, []string{"conversation:s1"}) {
		t.Fatalf("the first left the window: %+v", got)
	}
	if got := look(24*time.Hour + 40*time.Minute); !got.Purged.Empty() || len(got.Waiting) != 1 || !got.Waiting[0].NeedsUser {
		t.Errorf("three within the last day again: %+v", got)
	}
}
