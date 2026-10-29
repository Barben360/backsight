package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
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

// combined is this machine's shared-combined read at now.
func (m *shareMachine) combined(now time.Time) (feedback.Remote, time.Time, error) {
	c, err := feedback.LoadSharedCombined(m.d.FeedbackDir(), now)
	return c.Remote(), c.At(), err
}

// Every refresh combines the documents it cached, this machine's left
// out, into the view recall reads; a missing or unreadable view is
// combined again from the cache without a fetch.
func TestShareCombinesAtEveryRefresh(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(nil)
	p := feedbackParams(m.cfg)
	b := map[string]feedback.SharedEntry{shareM1: {Credits: 1.5, CreditsAt: shareT0.Add(-2 * time.Hour)}}
	m.put("pc-b", sharedMeta(t, "pc-b", shareT0.Add(-time.Hour), b))
	m.put("pc-a-old", sharedMeta(t, "pc-a", shareT0.Add(-time.Hour), map[string]feedback.SharedEntry{shareM2: {Credits: 9, CreditsAt: shareT0}}))
	m.put("pc-y", `{"v":2,"machine":"pc-y","at":1790000000,"entries":{"`+shareM2+`":[5,1790000000,0,0,0]}}`)
	at := shareT0.Add(500 * time.Millisecond) // combined to the second
	if _, _, _, err := m.share(at); err != nil {
		t.Fatal(err)
	}
	r, when, err := m.combined(at)
	if err != nil || !when.Equal(shareT0) {
		t.Fatalf("combined at %s: %v", when, err)
	}
	_, docs := m.cache()
	want := feedback.CombineRemote(docs, "pc-a", shareT0, p)
	if len(r) != 1 || math.Abs(r[shareM1].Credits-want[shareM1].Credits) > 1e-12 || !r[shareM1].CreditsAt.Equal(shareT0) {
		t.Errorf("combined %+v, want %+v (own and unknown-version documents left out)", r, want)
	}

	// Gone or unreadable before the next refresh: combined again from the
	// cache, which stays as it was.
	raw, _ := os.ReadFile(feedback.SharedCacheFile(m.d.FeedbackDir()))
	for name, spoil := range map[string]func(string) error{
		"missing":    os.Remove,
		"unreadable": func(f string) error { return os.WriteFile(f, []byte("{"), 0o600) },
	} {
		if err := spoil(feedback.SharedCombinedFile(m.d.FeedbackDir())); err != nil {
			t.Fatal(err)
		}
		later := shareT0.Add(time.Hour)
		if _, _, _, err := m.share(later); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r, when, err := m.combined(later); err != nil || !when.Equal(later) || r[shareM1].Credits == 0 {
			t.Errorf("%s: combined %+v at %s, %v", name, r, when, err)
		}
		if again, _ := os.ReadFile(feedback.SharedCacheFile(m.d.FeedbackDir())); string(again) != string(raw) {
			t.Errorf("%s: the cache was fetched again", name)
		}
	}

	// The next refresh combines anew, with what it fetched.
	m.put("pc-c", sharedMeta(t, "pc-c", shareT0, map[string]feedback.SharedEntry{shareM2: {Misses: 1, MissesAt: shareT0}}))
	refresh := shareT0.Add(6*time.Hour + time.Second)
	if _, _, _, err := m.share(refresh); err != nil {
		t.Fatal(err)
	}
	if r, when, err := m.combined(refresh); err != nil || !when.Equal(refresh) || r[shareM2].Misses < 0.99 || r[shareM2].Credits != 0 {
		t.Errorf("after the refresh: %+v at %s, %v", r, when, err)
	}
}

// Two machines on one server, through the commands: A's credits rank a
// memory up in B's search once B's tick fetched them; A hides an
// observation (a verdict on one whose sources cannot be listed), which
// leaves B's search at B's next refresh, not before; B's restore brings
// it back on B, while A still hides it.
func TestTwoMachinesShareOneLedger(t *testing.T) {
	port := fakeHindsight(t)
	newMachine := func() string {
		home := devInstall(t, true)
		pointAt(t, home, port)
		return home
	}
	a, b := newMachine(), newMachine()
	use := func(home string) {
		t.Setenv("BACKSIGHT_HOME", filepath.Join(home, "backsight"))
		t.Setenv("HOME", home)
	}
	t.Setenv("BACKSIGHT_PROJECT", "billing")
	oldNow := now
	t.Cleanup(func() { now = oldNow })
	// A first by the server's score; A has no sources to retire.
	memories[uA] = `{"id":"` + uA + `","text":"memory A about billing","type":"observation","tags":["project:billing"],"source_memories":[]}`
	memories[uB] = `{"id":"` + uB + `","text":"memory B about billing","type":"observation","tags":["project:billing"]}`
	recallAnswer = `[{"id":"` + uA + `","text":"memory A about billing","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.82}},` +
		`{"id":"` + uB + `","text":"memory B about billing","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.81}}]`
	search := func(name, home string) []string {
		t.Helper()
		use(home)
		code, out, errOut := run(t, "search", "billing", "--json")
		var env struct {
			Data recall.Result `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
			t.Fatalf("%s search: exit %d\n%s%s", name, code, out, errOut)
		}
		var ids []string
		for _, h := range env.Data.Hits {
			ids = append(ids, h.ID)
		}
		_, text, _ := run(t, "search", "billing")
		t.Logf("%s$ backsight search billing\n%s", name, text)
		return ids
	}
	tick := func(name, home string) tickResult {
		t.Helper()
		use(home)
		r := tickOf(t)
		t.Logf("%s$ backsight jobs tick\n%s", name, tickText(r))
		return r
	}
	order := func(ids []string) string { return strings.Join(ids, " ") }

	if got := search("B", b); order(got) != uA+" "+uB {
		t.Fatalf("B before any sharing: %s", got)
	}

	// A: three turns shown A and B, B credited each time, settled by the
	// tick (shows 3 h old), which sends A's counts.
	use(a)
	j := feedback.NewJournal(pathsOf(a).FeedbackDir(), feedback.Common{Session: "s1", Project: "billing"})
	start := time.Now().Add(-3 * time.Hour)
	for i := range 3 {
		turn, at := "p-"+strconv.Itoa(i+1), start.Add(time.Duration(i)*time.Minute)
		if err := j.Append(feedback.Event{Kind: "shows", TS: at, Turn: turn, Path: "search", Items: []feedback.Item{{ID: uA, Rank: 1, Project: "billing"}, {ID: uB, Rank: 2, Project: "billing"}}}); err != nil {
			t.Fatal(err)
		}
		if err := j.Append(feedback.Event{Kind: "credit", TS: at.Add(10 * time.Second), Turn: turn, IDs: []string{uB}, Verdict: "used"}); err != nil {
			t.Fatal(err)
		}
	}
	if r := tick("A", a); !r.SharedUp || r.Feedback.Credits != 3 {
		t.Fatalf("A's first tick: %+v", r)
	}

	// B: its first tick fetches A's counts; B now comes first.
	if r := tick("B", b); r.SharedMachines != 1 {
		t.Fatalf("B's first tick: %+v", r)
	}
	idA, idB := retain.ReadMachineID(pathsOf(a).State), retain.ReadMachineID(pathsOf(b).State)
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "k", 5*time.Second)
	docs, _ := retain.ControlDocuments(context.Background(), cl, config.LegacyBank, "backsight:feedback:")
	var held []string
	for _, d := range docs {
		held = append(held, d.ID)
	}
	if idA == "" || idA == idB || len(held) != 2 || !slices.Contains(held, "backsight:feedback:"+idA) || !slices.Contains(held, "backsight:feedback:"+idB) {
		t.Fatalf("machines %q and %q, the server holds %q", idA, idB, held)
	}
	if got := search("B", b); order(got) != uB+" "+uA {
		t.Fatalf("B after fetching A's credits: %s", got)
	}

	// A: a verdict on A, whose sources cannot be listed, hides it there;
	// the hiding is sent at once.
	use(a)
	mcpParentPID = 4848
	t.Cleanup(func() { mcpParentPID = 0 })
	cs := mcpConnect(t, a)
	_ = session.WriteLive(pathsOf(a).SessionsDir(), 4848, session.Live{SessionID: "S", Cwd: filepath.Join(a, "billing"), Project: "billing", At: time.Now()})
	showItems(t, a, "S", "search", "billing", feedback.Item{ID: uA, Rank: 1, Project: "billing"})
	out, isErr := call(t, cs, "memory_credit", verdict("outdated", uA))
	t.Logf("A: memory_credit outdated %s\n%s", uA, out)
	if isErr || !strings.HasPrefix(out, "Hidden.") {
		t.Fatalf("A's verdict: %v %s", isErr, out)
	}
	if got := search("A", a); order(got) != uB {
		t.Fatalf("A after its verdict: %s", got)
	}
	if r := tick("A", a); !r.SharedUp {
		t.Fatalf("A's tick after the hiding: %+v", r)
	}

	// B: its cache is fresh, so A stays; 6 h later B's tick fetches again
	// and A is gone from B's search.
	if r := tick("B", b); r.SharedMachines != 1 {
		t.Fatalf("B's tick: %+v", r)
	}
	if got := search("B", b); order(got) != uB+" "+uA {
		t.Fatalf("B before its next fetch: %s", got)
	}
	now = func() time.Time { return time.Now().Add(6*time.Hour + time.Minute) }
	tick("B, 6 h later", b)
	if got := search("B, 6 h later", b); order(got) != uB {
		t.Fatalf("B after fetching A's hiding: %s", got)
	}

	// B restores A: back on B, still hidden on A, whose next fetch reads
	// B's document.
	use(b)
	code, out, errOut := run(t, "forget", "--restore", uA)
	t.Logf("B, 6 h later$ backsight forget --restore %s\n%s%s", uA, out, errOut)
	if code != output.OK || !strings.Contains(out, "visible again on this machine") {
		t.Fatalf("B's restore: exit %d\n%s%s", code, out, errOut)
	}
	if got := search("B, 6 h later", b); order(got) != uB+" "+uA {
		t.Fatalf("B after its restore: %s", got)
	}
	tick("B, 6 h later", b)
	if r := tick("A, 6 h later", a); r.SharedMachines != 1 {
		t.Fatalf("A's tick: %+v", r)
	}
	if got := search("A, 6 h later", a); order(got) != uB {
		t.Fatalf("A after B's restore: %s", got)
	}
}

// A document is trusted only under its own name: one whose machine is not
// its id's (another machine's, or this one's) is skipped and counted, so
// it can neither replace that machine's own document nor pass for this
// one's.
func TestShareTrustsADocumentOnlyUnderItsOwnName(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(nil)
	c := sharedMeta(t, "pc-c", shareT0.Add(-time.Hour), map[string]feedback.SharedEntry{shareM1: {Credits: 1, CreditsAt: shareT0.Add(-time.Hour)}})
	m.put("pc-c", c)
	m.put("pc-b", sharedMeta(t, "pc-c", shareT0, map[string]feedback.SharedEntry{shareM1: {HiddenSince: shareT0}}))
	m.put("pc-d", sharedMeta(t, "pc-a", shareT0, map[string]feedback.SharedEntry{shareM2: {Credits: 9, CreditsAt: shareT0}}))
	_, machines, skipped, err := m.share(shareT0)
	if err != nil || machines != 1 || skipped != 2 {
		t.Fatalf("%d machine(s), %d skipped, %v", machines, skipped, err)
	}
	if cache, _ := m.cache(); len(cache.Docs) != 1 || string(cache.Docs[0]) != c {
		t.Errorf("cache %s", cache.Docs)
	}
	if r, _, err := m.combined(shareT0); err != nil || !r[shareM1].HiddenSince.IsZero() || r[shareM1].Credits == 0 || r[shareM2].Credits != 0 {
		t.Errorf("combined %+v, %v", r, err)
	}
}

// A refresh caches no document a month old (a machine no longer in use,
// never deleted from the server) and at most the 64 newest.
func TestShareCachesOnlyRecentDocuments(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(nil)
	month := 30 * 24 * time.Hour
	m.put("pc-month", sharedMeta(t, "pc-month", shareT0.Add(-month), nil))
	m.put("pc-gone", sharedMeta(t, "pc-gone", shareT0.Add(-month-time.Second), nil))
	_, machines, skipped, err := m.share(shareT0)
	if err != nil || machines != 1 || skipped != 0 {
		t.Fatalf("%d machine(s), %d skipped, %v", machines, skipped, err)
	}
	if _, docs := m.cache(); len(docs) != 1 || docs[0].Machine != "pc-month" {
		t.Errorf("cache %+v", docs)
	}

	m = newShareMachine(t)
	m.ledger(nil)
	for i := range 70 {
		name := fmt.Sprintf("pc-%02d", i)
		m.put(name, sharedMeta(t, name, shareT0.Add(-time.Duration(i)*time.Minute), nil))
	}
	if _, machines, _, err := m.share(shareT0); err != nil || machines != 64 {
		t.Fatalf("%d machine(s), %v", machines, err)
	}
	_, docs := m.cache()
	var names []string
	for _, d := range docs {
		names = append(names, d.Machine)
	}
	if len(names) != 64 || !slices.Contains(names, "pc-00") || !slices.Contains(names, "pc-63") || slices.Contains(names, "pc-64") || slices.Contains(names, "pc-69") {
		t.Errorf("cached %v", names)
	}
}

// The upload record names the server and bank it went to: another one
// (another joined server id, or another address while none is known, or
// another bank) gets this machine's document at once, unchanged.
func TestShareResendsAfterAServerChange(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: shareT0, LastShown: shareT0}})
	at := shareT0
	step := func(name string, change func(), want bool) {
		t.Helper()
		change()
		at = at.Add(15 * time.Minute)
		if up, _, _, err := m.share(at); err != nil || up != want {
			t.Errorf("%s: uploaded %v, want %v (%v)", name, up, want, err)
		}
	}
	joined := func(id, url string) func() {
		return func() {
			m.cfg.Server.Mode, m.cfg.Server.URL = "remote", url
			if err := state.Update(m.d, "test", func(s *state.State) error {
				s.RemoteServer = &state.RemoteServer{ID: id, URL: url}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	step("first", func() {}, true)
	step("same server", func() {}, false)
	step("another address", func() { m.cfg.Server.URL = "http://10.0.0.9:8888" }, true)
	step("another bank", func() { m.cfg.Server.Bank = "work" }, true)
	step("joined server", joined("srv-1", "http://10.0.0.9:8888"), true)
	step("same server, new address", joined("srv-1", "http://10.0.0.7:8888"), false)
	step("another joined server", joined("srv-2", "http://10.0.0.7:8888"), true)
	if rec, ok := feedback.LoadSharedUpload(m.d.FeedbackDir()); !ok || !strings.Contains(rec.Server, "srv-2") {
		t.Errorf("record %+v", rec)
	}
}

// viewLine is a combined view's line of id, as the tick wrote it.
func viewLine(t *testing.T, dir, id string) string {
	t.Helper()
	raw, _ := os.ReadFile(feedback.SharedCombinedFile(dir))
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, id+"\t") {
			return line
		}
	}
	return ""
}

// A half-life changed in config: the tick's local step combines the view
// again from shared.json under the new half-lives, at once; the same
// half-lives leave it as it is, and a view an older binary wrote (no
// half-lives recorded) is combined again once.
func TestViewRebuiltWhenHalfLivesChange(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(nil)
	dir := m.d.FeedbackDir()
	m.put("pc-b", sharedMeta(t, "pc-b", shareT0.Add(-time.Hour), map[string]feedback.SharedEntry{
		shareM1: {Credits: 2, CreditsAt: shareT0.Add(-100 * 24 * time.Hour), Misses: 3, MissesAt: shareT0.Add(-50 * 24 * time.Hour)},
	}))
	if _, _, _, err := m.share(shareT0); err != nil {
		t.Fatal(err)
	}
	cache, _ := os.ReadFile(feedback.SharedCacheFile(dir))
	_, docs := m.cache()
	later := shareT0.Add(time.Hour)

	if err := recombineShared(m.d, m.cfg, "pc-a", later); err != nil {
		t.Fatal(err)
	}
	if _, when, err := m.combined(later); err != nil || !when.Equal(shareT0) {
		t.Errorf("same half-lives: combined again at %s, %v", when, err)
	}

	old, _, _ := m.combined(later)
	m.cfg.Feedback.CreditHalfLifeDays, m.cfg.Feedback.MissHalfLifeDays = 30, 7
	if err := recombineShared(m.d, m.cfg, "pc-a", later); err != nil {
		t.Fatal(err)
	}
	r, when, err := m.combined(later)
	want := feedback.CombineRemote(docs, "pc-a", later, feedbackParams(m.cfg))
	if err != nil || !when.Equal(later) || len(r) != 1 ||
		math.Abs(r[shareM1].Credits-want[shareM1].Credits) > 1e-12 || math.Abs(r[shareM1].Misses-want[shareM1].Misses) > 1e-12 {
		t.Errorf("new half-lives: %+v at %s (%v), want %+v", r, when, err, want)
	}
	if math.Abs(old[shareM1].Credits-want[shareM1].Credits) < 0.1 || math.Abs(old[shareM1].Misses-want[shareM1].Misses) < 0.1 {
		t.Fatalf("the test's half-lives change nothing: %+v vs %+v", old, want)
	}
	if again, _ := os.ReadFile(feedback.SharedCacheFile(dir)); string(again) != string(cache) {
		t.Error("shared.json changed")
	}

	// A view as the previous binary wrote it: combined again once.
	line := viewLine(t, dir, shareM1)
	if line == "" {
		t.Fatal("no line for the memory")
	}
	legacy := fmt.Sprintf("v1 %d\n%s\n", later.Unix(), line)
	if err := os.WriteFile(feedback.SharedCombinedFile(dir), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := feedback.LoadSharedCombined(dir, later); err != nil || c.Empty() {
		t.Fatalf("recall no longer reads the previous binary's view: %v", err)
	}
	next := later.Add(15 * time.Minute)
	if err := recombineShared(m.d, m.cfg, "pc-a", next); err != nil {
		t.Fatal(err)
	}
	if _, when, err := m.combined(next); err != nil || !when.Equal(next) {
		t.Errorf("previous binary's view: combined at %s, %v", when, err)
	}
	if err := recombineShared(m.d, m.cfg, "pc-a", next.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, when, _ := m.combined(next); !when.Equal(next) {
		t.Errorf("combined again at %s with nothing changed", when)
	}
}

// A machine whose document turns 30 days old leaves the view at the next
// local step, not at the next 6 h refresh; not a second before.
func TestViewDropsADocumentPast30Days(t *testing.T) {
	m := newShareMachine(t)
	m.ledger(nil)
	turns := shareT0.Add(time.Hour) // pc-b's document is 30 days old then
	m.put("pc-b", sharedMeta(t, "pc-b", turns.Add(-30*24*time.Hour), map[string]feedback.SharedEntry{shareM1: {Credits: 1, CreditsAt: shareT0.Add(-31 * 24 * time.Hour)}}))
	m.put("pc-c", sharedMeta(t, "pc-c", shareT0, map[string]feedback.SharedEntry{shareM2: {Credits: 1, CreditsAt: shareT0}}))
	if _, _, _, err := m.share(shareT0); err != nil {
		t.Fatal(err)
	}
	if r, _, err := m.combined(shareT0); err != nil || r[shareM1].Credits == 0 || r[shareM2].Credits == 0 {
		t.Fatalf("both machines first: %+v, %v", r, err)
	}
	if err := recombineShared(m.d, m.cfg, "pc-a", turns); err != nil {
		t.Fatal(err)
	}
	if r, when, _ := m.combined(turns); !when.Equal(shareT0) || r[shareM1].Credits == 0 {
		t.Errorf("at 30 days: %+v combined at %s", r, when)
	}
	past := turns.Add(time.Second)
	if err := recombineShared(m.d, m.cfg, "pc-a", past); err != nil {
		t.Fatal(err)
	}
	if r, when, err := m.combined(past); err != nil || !when.Equal(past) || len(r) != 1 || r[shareM2].Credits == 0 {
		t.Errorf("past 30 days: %+v combined at %s, %v", r, when, err)
	}
}

// The tick combines the view again while the server is down: from
// shared.json, never fetched; without shared.json it leaves the view as
// it is.
func TestViewRebuildNeedsNoServer(t *testing.T) {
	home := devInstall(t, true) // its server does not answer
	d := pathsOf(home)
	dir := d.FeedbackDir()
	writeShareLedger(t, d, nil)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	b := sharedMeta(t, "pc-b", at, map[string]feedback.SharedEntry{shareM1: {Credits: 2, CreditsAt: at.Add(-100 * 24 * time.Hour)}})
	gone := sharedMeta(t, "pc-c", at.Add(-31*24*time.Hour), map[string]feedback.SharedEntry{shareM2: {Credits: 5, CreditsAt: at.Add(-31 * 24 * time.Hour)}})
	if err := feedback.SaveSharedCache(dir, feedback.SharedCache{FetchedAt: at, Docs: []json.RawMessage{json.RawMessage(b), json.RawMessage(gone)}}); err != nil {
		t.Fatal(err)
	}
	// The previous binary's view, still counting pc-c.
	legacy := fmt.Sprintf("v1 %d\n%s\t2\t%d\t0\t0\t0\n%s\t5\t%d\t0\t0\t0\n", at.Unix(), shareM1, at.Unix(), shareM2, at.Unix())
	if err := os.WriteFile(feedback.SharedCombinedFile(dir), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, _ := os.ReadFile(feedback.SharedCacheFile(dir))
	run(t, "jobs", "tick")
	c, err := feedback.LoadSharedCombined(dir, time.Now())
	if err != nil || !c.At().After(at) {
		t.Fatalf("not combined again: at %s, %v", c.At(), err)
	}
	cfg, _ := config.Load(d.ConfigFile(), d, home)
	docs, _ := (feedback.SharedCache{Docs: []json.RawMessage{json.RawMessage(b)}}).Parsed()
	want := feedback.CombineRemote(docs, "", c.At(), feedbackParams(cfg))
	if r := c.Remote(); len(r) != 1 || math.Abs(r[shareM1].Credits-want[shareM1].Credits) > 1e-12 {
		t.Errorf("combined %+v, want %+v", r, want)
	}
	if again, _ := os.ReadFile(feedback.SharedCacheFile(dir)); string(again) != string(cache) {
		t.Error("shared.json changed")
	}

	// No shared.json: the view stays as it is.
	if err := os.Remove(feedback.SharedCacheFile(dir)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(feedback.SharedCombinedFile(dir), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "jobs", "tick")
	if again, _ := os.ReadFile(feedback.SharedCombinedFile(dir)); string(again) != legacy {
		t.Errorf("without shared.json the view changed:\n%s", again)
	}
}
