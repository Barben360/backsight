package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
)

const (
	shareM1 = "11111111-1111-4111-8111-111111111111"
	shareM2 = "22222222-2222-4222-8222-222222222222"
)

var shareT0 = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

// shareMachine is a machine with feedback on whose server is the fake.
type shareMachine struct {
	t   *testing.T
	cl  *hindsight.Client
	d   paths.Dirs
	cfg config.Config
}

func newShareMachine(t *testing.T) *shareMachine {
	t.Helper()
	port := fakeHindsight(t)
	root := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	cl, err := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "k", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return &shareMachine{t: t, cl: cl, d: d, cfg: config.Default(d, root)}
}

// ledger writes this machine's ledger.json.
func (m *shareMachine) ledger(memories map[string]feedback.Entry) {
	m.t.Helper()
	writeShareLedger(m.t, m.d, memories)
}

func writeShareLedger(t *testing.T, d paths.Dirs, memories map[string]feedback.Entry) {
	t.Helper()
	raw, _ := json.Marshal(feedback.Ledger{Schema: feedback.LedgerSchema, Memories: memories})
	if err := os.MkdirAll(d.FeedbackDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.FeedbackDir(), "ledger.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (m *shareMachine) share(at time.Time) (bool, int, int, error) {
	return shareLedger(context.Background(), m.cl, m.d, m.cfg, "pc-a", at)
}

// put stores machine's shared document on the server, as its tick would.
func (m *shareMachine) put(machine, meta string) {
	m.t.Helper()
	if err := retain.WriteControlDocument(context.Background(), m.cl, m.cfg.Server.Bank, "backsight:feedback:"+machine, "x", map[string]string{"feedback": meta}); err != nil {
		m.t.Fatal(err)
	}
}

// uploaded is this machine's (pc-a) document on the server, ok false when
// there is none.
func (m *shareMachine) uploaded() (feedback.SharedDoc, map[string]string, bool) {
	m.t.Helper()
	docs, err := retain.ControlDocuments(context.Background(), m.cl, m.cfg.Server.Bank, "backsight:feedback:")
	if err != nil {
		m.t.Fatal(err)
	}
	for _, it := range docs {
		if it.ID == "backsight:feedback:pc-a" {
			doc, err := feedback.ParseSharedDoc(it.Metadata["feedback"])
			if err != nil {
				m.t.Fatalf("%s: %v", it.ID, err)
			}
			return doc, it.Metadata, true
		}
	}
	return feedback.SharedDoc{}, nil, false
}

func (m *shareMachine) cache() (feedback.SharedCache, []feedback.SharedDoc) {
	m.t.Helper()
	c, err := feedback.LoadSharedCache(m.d.FeedbackDir())
	if err != nil {
		m.t.Fatal(err)
	}
	docs, skipped := c.Parsed()
	if skipped != 0 {
		m.t.Errorf("the cache holds %d unreadable document(s)", skipped)
	}
	return c, docs
}

func sharedMeta(t *testing.T, machine string, at time.Time, entries map[string]feedback.SharedEntry) string {
	t.Helper()
	s, err := feedback.SharedDoc{V: feedback.SharedVersion, Machine: machine, At: at, Entries: entries}.MarshalMeta()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The first share uploads this machine's document, ids and numbers only;
// the same export later uploads nothing; a changed one waits until the
// last upload is an hour old; a changed set of hidden memories goes at
// once; an unreadable upload record counts as never uploaded.
func TestShareUploadsWhenChanged(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(map[string]feedback.Entry{shareM1: {Shown: 3, Credits: 2, CreditsAt: shareT0.Add(-time.Hour), LastShown: shareT0.Add(-time.Hour)}})
	up, _, _, err := m.share(shareT0)
	if err != nil || !up {
		t.Fatalf("first share: up %v, %v", up, err)
	}
	doc, meta, ok := m.uploaded()
	if !ok || doc.Machine != "pc-a" || !doc.At.Equal(shareT0) || doc.Entries[shareM1].Credits != 2 || len(doc.Entries) != 1 {
		t.Fatalf("uploaded %+v (found %v)", doc, ok)
	}
	if len(meta) != 1 {
		t.Errorf("metadata holds more than the counts: %v", meta)
	}
	if sharedSends.Load() != 1 {
		t.Errorf("first share: %d send(s)", sharedSends.Load())
	}
	if up, _, _, err := m.share(shareT0.Add(15 * time.Minute)); up || err != nil || sharedSends.Load() != 1 {
		t.Errorf("same export: up %v, %d send(s), %v", up, sharedSends.Load(), err)
	}

	m.ledger(map[string]feedback.Entry{shareM1: {Shown: 4, Credits: 3, CreditsAt: shareT0.Add(20 * time.Minute), LastShown: shareT0.Add(20 * time.Minute)}})
	if up, _, _, _ := m.share(shareT0.Add(30 * time.Minute)); up || sharedSends.Load() != 1 {
		t.Errorf("changed 30 min after the last upload: up %v", up)
	}
	if up, _, _, _ := m.share(shareT0.Add(61 * time.Minute)); !up {
		t.Error("changed, 61 min after the last upload: not uploaded")
	}
	if doc, _, _ := m.uploaded(); doc.Entries[shareM1].Credits != 3 || !doc.At.Equal(shareT0.Add(61*time.Minute)) {
		t.Errorf("after 61 min: %+v", doc)
	}

	// A memory hidden 9 minutes after the last upload: at once.
	m.ledger(map[string]feedback.Entry{
		shareM1: {Shown: 4, Credits: 3, CreditsAt: shareT0.Add(20 * time.Minute), LastShown: shareT0.Add(20 * time.Minute)},
		shareM2: {Shown: 1, Wrong: 1, Hidden: &feedback.Hidden{Since: shareT0.Add(65 * time.Minute), Verdict: "wrong", Note: "n1"}},
	})
	if up, _, _, _ := m.share(shareT0.Add(70 * time.Minute)); !up {
		t.Error("a new hiding: not uploaded at once")
	}
	if doc, meta, _ := m.uploaded(); !doc.Entries[shareM2].HiddenSince.Equal(shareT0.Add(65*time.Minute)) || strings.Contains(meta["feedback"], "n1") || strings.Contains(meta["feedback"], "wrong") {
		t.Errorf("hidden: %+v %v", doc.Entries[shareM2], meta)
	}

	// An unreadable record of the last upload: uploaded again.
	if err := os.WriteFile(filepath.Join(m.d.FeedbackDir(), "shared-upload.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if up, _, _, _ := m.share(shareT0.Add(75 * time.Minute)); !up {
		t.Error("unreadable upload record: not uploaded")
	}
}

// An unchanged export is sent again once the last send is 7 days old, so
// that the other machines never take a quiet machine for one no longer
// in use; before that it is not. A record of the last send dated ahead of
// the clock (stepped back since) makes a changed export due at once.
func TestShareResendsAQuietLedgerWeekly(t *testing.T) {
	m := newShareMachine(t)
	quiet := map[string]feedback.Entry{shareM1: {Shown: 3, Credits: 2, CreditsAt: shareT0.Add(-time.Hour), LastShown: shareT0.Add(-time.Hour)}}
	m.ledger(quiet)
	if up, _, _, err := m.share(shareT0); !up || err != nil {
		t.Fatalf("first share: up %v, %v", up, err)
	}
	if up, _, _, _ := m.share(shareT0.Add(2 * time.Hour)); up || sharedSends.Load() != 1 {
		t.Errorf("unchanged, 2 h later: up %v, %d send(s)", up, sharedSends.Load())
	}
	if up, _, _, _ := m.share(shareT0.Add(7*24*time.Hour - time.Minute)); up {
		t.Error("unchanged, 1 minute short of 7 days: sent")
	}
	week := shareT0.Add(7 * 24 * time.Hour)
	if up, _, _, _ := m.share(week); !up || sharedSends.Load() != 2 {
		t.Errorf("unchanged, 7 days later: up %v, %d send(s)", up, sharedSends.Load())
	}
	if doc, _, _ := m.uploaded(); !doc.At.Equal(week) || doc.Entries[shareM1].Credits != 2 {
		t.Errorf("re-sent %+v", doc)
	}

	// The clock stepped back an hour: the record is ahead of it.
	m.ledger(map[string]feedback.Entry{shareM1: {Shown: 4, Credits: 3, CreditsAt: week.Add(-2 * time.Hour), LastShown: week.Add(-2 * time.Hour)}})
	if up, _, _, _ := m.share(week.Add(-time.Hour)); !up {
		t.Error("changed, record ahead of the clock: not sent")
	}
}

// The other machines' documents are fetched every 6 hours, as they are;
// one that does not parse or of another version is skipped and counted.
func TestShareRefreshesTheCacheEvery6h(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(nil)
	b := sharedMeta(t, "pc-b", shareT0.Add(-time.Hour), map[string]feedback.SharedEntry{shareM1: {Credits: 1.5, CreditsAt: shareT0.Add(-2 * time.Hour)}})
	m.put("pc-b", b)
	m.put("pc-x", "{garbage")
	m.put("pc-y", `{"v":2,"machine":"pc-y","at":1790000000,"entries":{}}`)
	_, machines, skipped, err := m.share(shareT0)
	if err != nil || machines != 1 || skipped != 2 {
		t.Fatalf("first fetch: %d machine(s), %d skipped, %v", machines, skipped, err)
	}
	c, docs := m.cache()
	if !c.FetchedAt.Equal(shareT0) || len(c.Docs) != 1 || string(c.Docs[0]) != b || docs[0].Entries[shareM1].Credits != 1.5 {
		t.Fatalf("cache %s %s", c.FetchedAt, c.Docs)
	}
	raw, _ := os.ReadFile(feedback.SharedCacheFile(m.d.FeedbackDir()))

	m.put("pc-c", sharedMeta(t, "pc-c", shareT0, map[string]feedback.SharedEntry{shareM2: {Misses: 1, MissesAt: shareT0}}))
	if _, machines, skipped, _ := m.share(shareT0.Add(5*time.Hour + 59*time.Minute)); machines != 1 || skipped != 0 {
		t.Errorf("before 6 h: %d machine(s), %d skipped", machines, skipped)
	}
	if again, _ := os.ReadFile(feedback.SharedCacheFile(m.d.FeedbackDir())); string(again) != string(raw) {
		t.Errorf("cache rewritten before 6 h:\n%s", again)
	}
	if _, machines, _, _ := m.share(shareT0.Add(6 * time.Hour)); machines != 2 {
		t.Errorf("after 6 h: %d machine(s)", machines)
	}
	if c, _ := m.cache(); !c.FetchedAt.Equal(shareT0.Add(6*time.Hour)) || len(c.Docs) != 2 {
		t.Errorf("after 6 h: %s %d docs", c.FetchedAt, len(c.Docs))
	}
	if left, _ := filepath.Glob(filepath.Join(m.d.FeedbackDir(), ".shared.json.tmp-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}

	// An unreadable cache is fetched again at once.
	if err := os.WriteFile(feedback.SharedCacheFile(m.d.FeedbackDir()), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, machines, _, _ := m.share(shareT0.Add(6*time.Hour + time.Minute)); machines != 2 {
		t.Errorf("unreadable cache: %d machine(s)", machines)
	}
	if c, _ := m.cache(); len(c.Docs) != 2 {
		t.Errorf("unreadable cache not replaced: %d docs", len(c.Docs))
	}
}

// This machine's own document, under its id or another, never comes back
// into the cache nor counts as another machine.
func TestOwnDocumentIsNotCountedTwice(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: shareT0, LastShown: shareT0}})
	m.put("pc-a-old", sharedMeta(t, "pc-a", shareT0.Add(-time.Hour), map[string]feedback.SharedEntry{shareM1: {Credits: 9, CreditsAt: shareT0}}))
	m.put("pc-b", sharedMeta(t, "pc-b", shareT0, map[string]feedback.SharedEntry{shareM2: {Credits: 1, CreditsAt: shareT0}}))
	up, machines, _, err := m.share(shareT0)
	if err != nil || !up || machines != 1 {
		t.Fatalf("up %v, %d machine(s), %v", up, machines, err)
	}
	if _, _, ok := m.uploaded(); !ok {
		t.Fatal("own document not uploaded before the fetch")
	}
	_, docs := m.cache()
	if len(docs) != 1 || docs[0].Machine != "pc-b" {
		t.Errorf("cache %+v", docs)
	}
}

// Sharing off, or feedback off: nothing is sent, read or written.
func TestShareOffDoesNothing(t *testing.T) {
	for name, set := range map[string]func(*config.Config){
		"share_ledger = false": func(c *config.Config) { c.Feedback.ShareLedger = false },
		"enabled = false":      func(c *config.Config) { c.Feedback.Enabled = false },
	} {
		m := newShareMachine(t)
		set(&m.cfg)
		m.ledger(map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: shareT0, LastShown: shareT0}})
		m.put("pc-b", sharedMeta(t, "pc-b", shareT0, map[string]feedback.SharedEntry{shareM2: {Credits: 1, CreditsAt: shareT0}}))
		calls := len(bankCalls)
		up, machines, _, err := m.share(shareT0)
		if up || machines != 0 || err != nil || len(bankCalls) != calls {
			t.Errorf("%s: up %v, %d machine(s), %d call(s), %v", name, up, machines, len(bankCalls)-calls, err)
		}
		for _, f := range []string{"shared.json", "shared-upload.json"} {
			if _, err := os.Stat(filepath.Join(m.d.FeedbackDir(), f)); err == nil {
				t.Errorf("%s: %s written", name, f)
			}
		}
	}
}

// A server that fails sends nothing and keeps the cache as it was; the
// tick skips sharing while the server is down or another server answers
// at the joined address, and goes on.
func TestShareSurvivesAnUnreachableServer(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: shareT0, LastShown: shareT0}})
	m.put("pc-b", sharedMeta(t, "pc-b", shareT0, map[string]feedback.SharedEntry{shareM2: {Credits: 1, CreditsAt: shareT0}}))
	if _, _, _, err := m.share(shareT0); err != nil {
		t.Fatal(err)
	}
	cache, _ := os.ReadFile(feedback.SharedCacheFile(m.d.FeedbackDir()))
	upload, _ := os.ReadFile(filepath.Join(m.d.FeedbackDir(), "shared-upload.json"))
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	m.cl, _ = hindsight.New(broken.URL, "k", 5*time.Second)
	m.ledger(map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: shareT0, LastShown: shareT0}, shareM2: {Hidden: &feedback.Hidden{Since: shareT0}}})
	up, machines, _, err := m.share(shareT0.Add(7 * time.Hour))
	if up || err == nil || machines != 1 {
		t.Errorf("500: up %v, %d machine(s) (the cache's), err %v", up, machines, err)
	}
	if again, _ := os.ReadFile(feedback.SharedCacheFile(m.d.FeedbackDir())); string(again) != string(cache) {
		t.Errorf("cache changed:\n%s\nwas\n%s", again, cache)
	}
	if again, _ := os.ReadFile(filepath.Join(m.d.FeedbackDir(), "shared-upload.json")); string(again) != string(upload) {
		t.Errorf("upload record changed: %s", again)
	}

	// Through the tick: shared while the joined server answers...
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	machine, _ := retain.MachineID(d.State)
	writeShareLedger(t, d, map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: time.Now(), LastShown: time.Now()}})
	if r := tickOf(t); !r.SharedUp || r.Status != "done" {
		t.Fatalf("healthy tick: %+v", r)
	}
	if _, out, _ := run(t, "jobs", "tick"); strings.Contains(out, "counts sent") {
		t.Errorf("unchanged, sent again: %s", out)
	}
	cl, _ := hindsight.New(serverURL(t, home), "k", 5*time.Second)
	if docs, _ := retain.ControlDocuments(context.Background(), cl, config.LegacyBank, "backsight:feedback:"); len(docs) != 1 || docs[0].ID != "backsight:feedback:"+machine {
		t.Fatalf("server holds %+v", docs)
	}

	// A send the server refuses: the tick is done, says so, and the cache
	// stays as it was.
	cacheBefore, _ := os.ReadFile(feedback.SharedCacheFile(d.FeedbackDir()))
	if err := os.Remove(feedback.SharedUploadFile(d.FeedbackDir())); err != nil { // a send due
		t.Fatal(err)
	}
	retainStatus.Store(http.StatusInternalServerError)
	_, out, _ := run(t, "jobs", "tick")
	r := tickOf(t)
	retainStatus.Store(0)
	if r.Status != "done" || r.SharedUp || r.SharedError == "" || !strings.Contains(out, "counts not shared") {
		t.Errorf("refused send: %+v\n%s", r, out)
	}
	if again, _ := os.ReadFile(feedback.SharedCacheFile(d.FeedbackDir())); len(cacheBefore) == 0 || string(again) != string(cacheBefore) {
		t.Errorf("cache changed:\n%s\nwas\n%s", again, cacheBefore)
	}

	// ...not while another server answers there...
	other := devInstall(t, true)
	pointAt(t, other, fakeHindsight(t))
	recordMismatch(t, other)
	writeShareLedger(t, pathsOf(other), map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: time.Now(), LastShown: time.Now()}})
	if r := tickOf(t); r.SharedUp || r.SharedMachines != 0 {
		t.Errorf("another server: %+v", r)
	}
	cl, _ = hindsight.New(serverURL(t, other), "k", 5*time.Second)
	if docs, _ := retain.ControlDocuments(context.Background(), cl, config.LegacyBank, "backsight:feedback:"); len(docs) != 0 {
		t.Errorf("another server got %+v", docs)
	}
	assertNoShareFiles(t, pathsOf(other))

	// ...nor when the tick's health call fails, though the rest answers...
	sick := devInstall(t, true)
	fake, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(fakeHindsight(t)))
	proxy := httputil.NewSingleHostReverseProxy(fake)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	frontPort, _ := strconv.Atoi(front.URL[strings.LastIndex(front.URL, ":")+1:])
	pointAt(t, sick, frontPort)
	writeShareLedger(t, pathsOf(sick), map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: time.Now(), LastShown: time.Now()}})
	if r := tickOf(t); r.SharedUp || sharedSends.Load() != 0 {
		t.Errorf("health failing: %+v, %d send(s)", r, sharedSends.Load())
	}
	assertNoShareFiles(t, pathsOf(sick))

	// ...nor while the server is down.
	down := devInstall(t, true)
	writeShareLedger(t, pathsOf(down), map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: time.Now(), LastShown: time.Now()}})
	if _, out, _ := run(t, "jobs", "tick"); strings.Contains(out, "counts sent") {
		t.Errorf("server down:\n%s", out)
	}
	assertNoShareFiles(t, pathsOf(down))
}

func assertNoShareFiles(t *testing.T, d paths.Dirs) {
	t.Helper()
	for _, f := range []string{"shared.json", "shared-upload.json"} {
		if _, err := os.Stat(filepath.Join(d.FeedbackDir(), f)); err == nil {
			t.Errorf("%s written", f)
		}
	}
}

// serverURL is the server home's config.toml points at.
func serverURL(t *testing.T, home string) string {
	t.Helper()
	cfg, err := config.Load(pathsOf(home).ConfigFile(), pathsOf(home), home)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Server.URL
}
