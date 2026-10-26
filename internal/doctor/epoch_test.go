package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

const epochPath = "/v1/default/banks/coding-agent%3A%3Amemoire-control/documents/backsight%3Aepoch"

// configured is a fixture with backsight's own config on srv's URL.
func configured(t *testing.T, url string) *fixture {
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+url+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	return f
}

// The epoch read fails on the server's side (5xx): doctor cannot judge, an
// info. The server answers a document whose metadata does not parse: every
// send holds on it, a warning with the way out.
func TestCheckRestoreEpochUnreadable(t *testing.T) {
	f := configured(t, fakeServerWith(t, map[string]reply{epochPath: {500, `{"detail":"boom"}`}}).URL)
	expect(t, check(t, f.run(), "restore-epoch"), Info, "cannot read the bank's restore epoch")

	g := configured(t, fakeServerWith(t, map[string]reply{epochPath: {200, `{"id":"backsight:epoch","document_metadata":{"epoch":"two","restored_at":"2026-10-25T10:00:00Z","backup_created_at":"2026-10-25T03:00:00Z"}}`}}).URL)
	c := check(t, g.run(), "restore-epoch")
	expect(t, c, Warn, "restore epoch does not parse")
	if !strings.Contains(c.Detail, "two") || !strings.Contains(c.Fix, "backsight restore") || !strings.Contains(c.Fix, "coding-agent::memoire-control") {
		t.Errorf("%+v", c)
	}
}

// A client ahead of the bank (state 5, bank 2: another server behind the
// same bank id) warns; a state.json that does not parse warns too.
func TestCheckRestoreEpochClientAheadOrStateUnreadable(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{epochPath: {200, `{"id":"backsight:epoch","document_metadata":{"epoch":"2","restored_at":"2026-10-25T10:00:00Z","backup_created_at":"2026-10-25T03:00:00Z"}}`}})
	f := configured(t, srv.URL)
	if err := state.Save(f.env.Dirs.StateFile(), state.State{SchemaVersion: state.SchemaVersion,
		RestoreEpoch: &state.RestoreEpoch{Bank: "coding-agent::memoire", Epoch: 5}}); err != nil {
		t.Fatal(err)
	}
	c := check(t, f.run(), "restore-epoch")
	expect(t, c, Warn, "client on restore epoch 5, the bank on 2")
	if c.Fix == "" {
		t.Errorf("no fix: %+v", c)
	}
	put(t, f.env.Dirs.StateFile(), "{not json", 0o600)
	expect(t, check(t, f.run(), "restore-epoch"), Warn, "cannot read the client's restore epoch")
}

// A restore that did not finish is an error naming the safety archive and
// the two ways out; without a marker the check is ok.
func TestCheckRestoreIncomplete(t *testing.T) {
	f := newFixture(t)
	expect(t, check(t, f.run(), "restore"), OK, "no restore in progress")
	m := state.RestoreMarker{Command: "restore", Safety: "/b/backsight-20261025T140000Z.tar.gz.age", Archive: "/b/backsight-20261024T040000Z.tar.gz.age",
		StartedAt: time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)}
	if err := state.WriteRestoreMarker(f.env.Dirs, m); err != nil {
		t.Fatal(err)
	}
	r := f.run()
	c := check(t, r, "restore")
	expect(t, c, Error, "did not finish")
	if !strings.Contains(c.Detail, m.Safety) || !strings.Contains(c.Fix, "backsight restore "+m.Safety+" --no-safety") || !strings.Contains(c.Fix, "backsight restore "+m.Archive) || r.Errors == 0 {
		t.Errorf("%+v", c)
	}
	put(t, f.env.Dirs.RestoreMarker(), "{torn", 0o600)
	c = check(t, f.run(), "restore")
	expect(t, c, Error, "did not finish")
	if !strings.Contains(c.Fix, f.env.Dirs.RestoreMarker()) {
		t.Errorf("a torn marker: %+v", c)
	}
}

// A reconcile that could not purge again what the server still holds left
// a hold: doctor names the bank and how many documents, whatever the
// epochs say (issue 285 review LOW-4).
func TestCheckRestoreEpochShowsAPurgeHold(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{epochPath: {200, `{"id":"backsight:epoch","document_metadata":{"epoch":"2","restored_at":"2026-10-25T10:00:00Z","backup_created_at":"2026-10-25T03:00:00Z"}}`}})
	f := configured(t, srv.URL)
	if err := state.Save(f.env.Dirs.StateFile(), state.State{SchemaVersion: state.SchemaVersion,
		RestoreEpoch: &state.RestoreEpoch{Bank: "coding-agent::memoire", Epoch: 1},
		PurgeHold:    &state.PurgeHold{Bank: "coding-agent::memoire", Documents: 3, Error: "DELETE: 500"}}); err != nil {
		t.Fatal(err)
	}
	c := check(t, f.run(), "restore-epoch")
	expect(t, c, Error, "3 purged document(s) of bank coding-agent::memoire")
	if !strings.Contains(c.Detail, "DELETE: 500") || c.Fix == "" {
		t.Errorf("%+v", c)
	}
}
