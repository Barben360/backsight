package doctor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

const (
	s1Path       = "/v1/default/banks/coding-agent%3A%3Amemoire/documents/conversation%3As1"
	docsListPath = "/v1/default/banks/coding-agent%3A%3Amemoire/documents"
	tombsPath    = "/v1/default/banks/coding-agent%3A%3Amemoire-control/documents/backsight%3Atombstones"
	// listS1: the documents created since the last look hold
	// conversation:s1 again, and its successor.
	listS1 = `{"items":[{"id":"conversation:s1","created_at":"2026-10-26T09:30:00+00:00"},{"id":"conversation:s1#2","created_at":"2026-10-26T09:10:00+00:00"}],"total":2}`
)

var doctorAt = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// purgedS1 records conversation:s1's tombstone and, unless lastLook is
// zero, the tick's last look at lastLook with what waits.
func purgedS1(t *testing.T, f *fixture, lastLook time.Time, waiting ...state.Recreated) {
	t.Helper()
	f.env.Now = func() time.Time { return doctorAt }
	st := state.State{SchemaVersion: state.SchemaVersion, Tombstones: []state.Tombstone{
		{Bank: "coding-agent::memoire", Document: "conversation:s1", Session: "s1", PurgedAt: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}}}
	if !lastLook.IsZero() {
		st.PurgeGuard = &state.PurgeGuard{Bank: "coding-agent::memoire", Newest: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC), CheckedAt: lastLook, Waiting: waiting}
	}
	if err := state.Save(f.env.Dirs.StateFile(), st); err != nil {
		t.Fatal(err)
	}
}

// A recreated document recorded as waiting for the user (consent, an
// older client that keeps writing) is an error naming it, its session,
// why, and the command; its successor is not named.
func TestCheckPurgedErrorWhenItWaitsForTheUser(t *testing.T) {
	f := configured(t, fakeServerWith(t, map[string]reply{docsListPath: {200, listS1}}).URL)
	purgedS1(t, f, doctorAt.Add(-10*time.Minute), state.Recreated{Document: "conversation:s1", Session: "s1", NeedsUser: true,
		Why: "an older client keeps writing to it (purged again 3 times since 2026-10-26 08:00 UTC)"})
	r := f.run()
	c := check(t, r, "purged-documents")
	expect(t, c, Error, "1 purged document(s) of bank coding-agent::memoire are on the server again")
	if !strings.Contains(c.Summary, "conversation:s1 (session s1)") || strings.Contains(c.Summary, "#2") || !strings.Contains(c.Detail, "keeps writing") ||
		!strings.HasPrefix(c.Fix, "backsight retain purge-again") || strings.Contains(c.Fix, "--allow-subscription") || r.Errors == 0 {
		t.Errorf("%+v", c)
	}
}

// Waiting for the queue, or not yet seen by a tick that looked within the
// hour: the next tick does it, a warning. Not seen by a tick for an hour
// or more: an error.
func TestCheckPurgedWarnsWhileTheTickHandlesIt(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{docsListPath: {200, listS1}})
	f := configured(t, srv.URL)
	purgedS1(t, f, doctorAt.Add(-10*time.Minute), state.Recreated{Document: "conversation:s1", Session: "s1", Why: "an append to it is pending (operation op9)"})
	c := check(t, f.run(), "purged-documents")
	expect(t, c, Warn, "conversation:s1 (session s1)")
	if !strings.Contains(c.Detail, "operation op9") || !strings.Contains(c.Fix, "jobs tick") {
		t.Errorf("%+v", c)
	}
	purgedS1(t, f, doctorAt.Add(-10*time.Minute))
	expect(t, check(t, f.run(), "purged-documents"), Warn, "conversation:s1 (session s1)")
	purgedS1(t, f, doctorAt.Add(-2*time.Hour))
	expect(t, check(t, f.run(), "purged-documents"), Error, "conversation:s1 (session s1)")
}

// With the claude-code provider, or a server this machine does not run,
// the command to run carries the consent.
func TestCheckPurgedAsksForConsent(t *testing.T) {
	for name, extra := range map[string]string{"claude-code": "[llm]\nprovider = \"claude-code\"\n", "remote": "mode = \"remote\"\n"} {
		t.Run(name, func(t *testing.T) {
			srv := fakeServerWith(t, map[string]reply{docsListPath: {200, listS1}})
			f := configured(t, srv.URL)
			cfg := "[server]\nurl = \"" + srv.URL + "\"\nbank = \"coding-agent::memoire\"\n"
			cfg += extra
			put(t, f.env.Dirs.ConfigFile(), cfg, 0o600)
			purgedS1(t, f, doctorAt.Add(-10*time.Minute), state.Recreated{Document: "conversation:s1", Session: "s1", NeedsUser: true, Why: "consent"})
			c := check(t, f.run(), "purged-documents")
			expect(t, c, Error, "conversation:s1 (session s1)")
			if !strings.Contains(c.Fix, "backsight retain purge-again --allow-subscription") {
				t.Errorf("%+v", c)
			}
		})
	}
}

// Gone from the server: ok. No tombstone: ok. A read that fails: info,
// never an all-clear.
func TestCheckPurgedOKWhenGoneOrNone(t *testing.T) {
	f := configured(t, fakeServerWith(t, map[string]reply{docsListPath: {200, `{"items":[],"total":0}`}}).URL)
	f.env.Now = func() time.Time { return doctorAt }
	expect(t, check(t, f.run(), "purged-documents"), OK, "no purged document")
	purgedS1(t, f, doctorAt.Add(-10*time.Minute))
	expect(t, check(t, f.run(), "purged-documents"), OK, "none of the 1 purged document(s) is on the server again")

	g := configured(t, fakeServerWith(t, map[string]reply{docsListPath: {500, `{"detail":"boom"}`}}).URL)
	purgedS1(t, g, doctorAt.Add(-10*time.Minute))
	expect(t, check(t, g.run(), "purged-documents"), Info, "cannot look for purged documents")
}

// doctor never looks at every tombstone itself: before the tick recorded
// a look, it reads nothing and says so.
func TestCheckPurgedNeverLooksAtEveryTombstone(t *testing.T) {
	var mu sync.Mutex
	var reads []string
	inner := fakeHandler(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reads = append(reads, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f := configured(t, srv.URL)
	purgedS1(t, f, time.Time{})
	expect(t, check(t, f.run(), "purged-documents"), Info, "not looked for on the server yet")
	mu.Lock()
	defer mu.Unlock()
	for _, p := range reads {
		if strings.HasPrefix(p, s1Path+"?") || (strings.HasPrefix(p, docsListPath+"?") && strings.Contains(p, "time_field=created_at") && !strings.Contains(p, "tags=")) {
			t.Errorf("doctor looked: %s", p)
		}
	}
}

// Another machine's purge, known only from the control bank, is looked
// for too.
func TestCheckPurgedReadsTheControlBanksTombstones(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{
		tombsPath:    {200, `{"id":"backsight:tombstones","document_metadata":{"tombstones":"[{\"document\":\"conversation:s1\",\"session\":\"s1\",\"purged_at\":\"2026-10-26T09:00:00Z\"}]"}}`},
		docsListPath: {200, listS1}})
	f := configured(t, srv.URL)
	f.env.Now = func() time.Time { return doctorAt }
	if err := state.Save(f.env.Dirs.StateFile(), state.State{SchemaVersion: state.SchemaVersion,
		PurgeGuard: &state.PurgeGuard{Bank: "coding-agent::memoire", Newest: doctorAt.Add(-3 * time.Hour), CheckedAt: doctorAt.Add(-5 * time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	expect(t, check(t, f.run(), "purged-documents"), Warn, "conversation:s1 (session s1)")
}

// A state.json that does not parse: a warning, nothing looked for.
func TestCheckPurgedUnreadableState(t *testing.T) {
	f := configured(t, fakeServerWith(t, nil).URL)
	put(t, f.env.Dirs.StateFile(), "{torn", 0o600)
	expect(t, check(t, f.run(), "purged-documents"), Warn, "cannot read the purge tombstones")
}
