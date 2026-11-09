package ledgerdbtest

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/paths"
)

// failTB records the first Fatal and stops the call, as a real one does.
type failTB struct {
	testing.TB
	msg string
}

type stopped struct{}

func (f *failTB) Helper() {}
func (f *failTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	panic(stopped{})
}
func (f *failTB) Fatal(args ...any) { f.Fatalf("%s", fmt.Sprint(args...)) }

func seedFails(dir string, l feedback.Ledger) (msg string) {
	f := &failTB{}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(stopped); !ok {
				panic(r)
			}
			msg = f.msg
		}
	}()
	Seed(f, paths.Dirs{State: filepath.Join(dir, "state")}, l)
	return ""
}

func TestSeedRefusesWhatNoJournalMakes(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	cases := map[string]feedback.Entry{
		"fractional credits":        {Shown: 3, Credits: 1.5, CreditsAt: at, LastShown: at, LastCredited: at},
		"fractional misses":         {Shown: 3, Misses: 0.5, MissesAt: at, LastShown: at},
		"credits before last":       {Shown: 3, Credits: 1, CreditsAt: at.Add(-time.Hour), LastShown: at, LastCredited: at},
		"shown below the outcomes":  {Shown: 1, Credits: 2, CreditsAt: at, LastShown: at, LastCredited: at},
		"a last show no show makes": {Shown: 1, Credits: 1, CreditsAt: at, LastShown: at.Add(time.Hour), LastCredited: at},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			msg := seedFails(t.TempDir(), feedback.Ledger{Memories: map[string]feedback.Entry{"m1": e}})
			if msg == "" {
				t.Fatal("Seed accepted a ledger no journal produces")
			}
			if !strings.Contains(msg, "m1") {
				t.Errorf("the failure does not name the memory: %q", msg)
			}
		})
	}
}

func TestSeedBuildsALedgerTheJournalGivesBack(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	want := feedback.Ledger{
		Schema:          feedback.LedgerSchema,
		Watermark:       at.Add(2 * time.Hour),
		HistoryImported: at.Add(-time.Hour),
		Memories: map[string]feedback.Entry{
			"plain": {Shown: 5, Credits: 2, CreditsAt: at, LastCredited: at, Misses: 1, MissesAt: at.Add(-time.Minute), LastShown: at.Add(time.Minute)},
			"hid": {Shown: 1, Wrong: 2, LastShown: at,
				Hidden: &feedback.Hidden{Since: at.Add(time.Hour), Verdict: "wrong", Note: "doc-1"}},
			"restored": {UnhiddenAt: at.Add(time.Hour)},
			"imp": {Shown: 3, Credits: 1, CreditsAt: at, LastCredited: at, LastShown: at,
				Imported: feedback.Imported{Shown: 2, Credits: 1}},
		},
	}
	Seed(t, d, want)

	o := ledgerdb.Options{Dirs: d, Getenv: func(string) string { return "" }}
	r, err := ledgerdb.OpenReader(o, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	got, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Memories) != 4 || got.Memories["plain"].Shown != 5 || got.Memories["hid"].Hidden == nil || got.Memories["hid"].Hidden.Note != "doc-1" {
		t.Fatalf("ledger.db does not hold the ledger asked for: %+v", got.Memories)
	}
	// the journal alone gives the same ledger back
	events, _, _, err := feedback.ReadAll(d.FeedbackDir())
	if err != nil {
		t.Fatal(err)
	}
	if again := feedback.Fold(events, feedback.Params{}); again.Memories["plain"].Credits != 2 || again.Memories["imp"].Imported.Shown != 2 {
		t.Fatalf("the journal does not fold to the ledger: %+v", again.Memories)
	}
}
