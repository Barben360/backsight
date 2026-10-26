package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

const (
	s1Path    = "/v1/default/banks/coding-agent%3A%3Amemoire/documents/conversation%3As1"
	s1Next    = "/v1/default/banks/coding-agent%3A%3Amemoire/documents/conversation%3As1%232"
	tombsPath = "/v1/default/banks/coding-agent%3A%3Amemoire-control/documents/backsight%3Atombstones"
)

func purgedS1(t *testing.T, f *fixture, waiting ...state.Recreated) {
	t.Helper()
	st := state.State{SchemaVersion: state.SchemaVersion, Tombstones: []state.Tombstone{
		{Bank: "coding-agent::memoire", Document: "conversation:s1", Session: "s1", PurgedAt: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}}}
	if len(waiting) > 0 {
		st.PurgeGuard = &state.PurgeGuard{Bank: "coding-agent::memoire", Waiting: waiting}
	}
	if err := state.Save(f.env.Dirs.StateFile(), st); err != nil {
		t.Fatal(err)
	}
}

// A purged document the server holds again is an error naming it, its
// session, why it is not purged yet, and the command to run. Its
// successor, which the server also holds, is not named.
func TestCheckPurgedNamesARecreatedDocument(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{
		s1Path: {200, `{"id":"conversation:s1","document_metadata":{}}`},
		s1Next: {200, `{"id":"conversation:s1#2","document_metadata":{}}`}})
	f := configured(t, srv.URL)
	purgedS1(t, f, state.Recreated{Document: "conversation:s1", Session: "s1", Why: "an append to it is pending (operation op9)"})
	r := f.run()
	c := check(t, r, "purged-documents")
	expect(t, c, Error, "1 purged document(s) of bank coding-agent::memoire are on the server again")
	if !strings.Contains(c.Summary, "conversation:s1 (session s1)") || strings.Contains(c.Summary, "#2") || !strings.Contains(c.Detail, "operation op9") ||
		!strings.HasPrefix(c.Fix, "backsight retain purge-again") || strings.Contains(c.Fix, "--allow-subscription") || r.Errors == 0 {
		t.Errorf("%+v", c)
	}
}

// With the claude-code provider the command to run carries the consent.
func TestCheckPurgedAsksForTheSubscriptionWithClaudeCode(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{s1Path: {200, `{"id":"conversation:s1","document_metadata":{}}`}})
	f := configured(t, srv.URL)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n[llm]\nprovider = \"claude-code\"\n", 0o600)
	purgedS1(t, f)
	c := check(t, f.run(), "purged-documents")
	expect(t, c, Error, "conversation:s1 (session s1)")
	if !strings.Contains(c.Fix, "backsight retain purge-again --allow-subscription") {
		t.Errorf("%+v", c)
	}
}

// Gone from the server: ok. No tombstone: ok. A read that fails: info,
// never an all-clear.
func TestCheckPurgedOKWhenGoneOrNone(t *testing.T) {
	f := configured(t, fakeServerWith(t, nil).URL)
	expect(t, check(t, f.run(), "purged-documents"), OK, "no purged document")
	purgedS1(t, f)
	expect(t, check(t, f.run(), "purged-documents"), OK, "none of the 1 purged document(s) is on the server again")

	g := configured(t, fakeServerWith(t, map[string]reply{s1Path: {500, `{"detail":"boom"}`}}).URL)
	purgedS1(t, g)
	expect(t, check(t, g.run(), "purged-documents"), Info, "cannot look for purged documents")
}

// Another machine's purge, known only from the control bank, is looked
// for too.
func TestCheckPurgedReadsTheControlBanksTombstones(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{
		tombsPath: {200, `{"id":"backsight:tombstones","document_metadata":{"tombstones":"[{\"document\":\"conversation:s1\",\"session\":\"s1\",\"purged_at\":\"2026-10-26T09:00:00Z\"}]"}}`},
		s1Path:    {200, `{"id":"conversation:s1","document_metadata":{}}`}})
	f := configured(t, srv.URL)
	expect(t, check(t, f.run(), "purged-documents"), Error, "conversation:s1 (session s1)")
}
