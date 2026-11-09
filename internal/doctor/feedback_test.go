package doctor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// feedbackDirs is a machine whose journal holds one shows event at last,
// and whose ledger last settled at watermark (zero: no ledger): a show
// of a memory forgotten since settles then, so the settle line, the
// ledger's watermark, is at watermark, and counts nothing.
func feedbackDirs(t *testing.T, last, watermark time.Time) paths.Dirs {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	j := feedback.NewJournal(d.FeedbackDir(), feedback.Common{Session: "s1"})
	if err := j.Append(feedback.Event{Kind: "shows", TS: last, Path: "prompt", Items: []feedback.Item{{ID: "m1", Rank: 1}}}); err != nil {
		t.Fatal(err)
	}
	if !watermark.IsZero() {
		for _, e := range []feedback.Event{
			{Kind: "shows", TS: watermark.Add(-3 * time.Hour), Path: "prompt", Items: []feedback.Item{{ID: "m0", Rank: 1}}},
			{Kind: "forget", TS: watermark.Add(-2 * time.Hour), IDs: []string{"m0"}},
		} {
			if err := j.Append(e); err != nil {
				t.Fatal(err)
			}
		}
		foldAt(t, d, watermark)
	}
	return d
}

// foldAt settles d's journal into its ledger database at clock time at
// (the fold's time too).
func foldAt(t *testing.T, d paths.Dirs, at time.Time) {
	t.Helper()
	clock := func() time.Time { return at }
	if _, err := ledgerdb.Settle(context.Background(), ledgerdb.Options{Dirs: d, Now: clock}, feedback.Params{ConfidentRank: 2, TurnTimeout: feedback.TurnTimeout}, clock); err != nil {
		t.Fatal(err)
	}
}

// ledgerCheck is the feedback-ledger check of d at usageNow, its facts
// read as doctor reads them.
func ledgerCheck(t *testing.T, d paths.Dirs) Check {
	t.Helper()
	f, err := ledgerdb.Facts(ledgerdb.Options{Dirs: d})
	return checkFeedbackLedger(d, true, f, err, usageNow)
}

// telemetryAt appends telemetry lines to d's month file, each at
// usageNow minus its age.
func telemetryAt(t *testing.T, d paths.Dirs, lines map[time.Duration][]string) {
	t.Helper()
	if err := os.MkdirAll(d.TelemetryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(d.TelemetryDir(), "events-2026-10.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for age, ls := range lines {
		for _, l := range ls {
			if _, err := fmt.Fprintf(f, `{"ts":%q,%s}`+"\n", usageNow.Add(-age).Format("2006-01-02T15:04:05.000Z"), l); err != nil {
				t.Fatal(err)
			}
		}
	}
}

const tickLine = `"event":"job","name":"tick","status":"done"`

// fallbacks are n feedback_error lines of reason.
func fallbacks(reason string, n int) []string {
	var out []string
	for range n {
		out = append(out, `"event":"feedback_error","reason":"`+reason+`"`)
	}
	return out
}

func journalFile(t *testing.T, d paths.Dirs) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(d.FeedbackDir(), "journal-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("journal files %v", files)
	}
	return files[0]
}

func TestDoctorFeedbackChecks(t *testing.T) {
	recent := usageNow.Add(-time.Hour)

	healthy := feedbackDirs(t, recent, usageNow.Add(-10*time.Minute))
	if c := checkFeedbackJournal(healthy, true); c.Status != OK {
		t.Errorf("healthy journal: %+v", c)
	}
	if c := ledgerCheck(t, healthy); c.Status != OK {
		t.Errorf("healthy ledger: %+v", c)
	}

	// Above 200 MB in total: a warning (a sparse file costs no disk).
	huge := feedbackDirs(t, recent, usageNow)
	big, _ := os.Create(filepath.Join(huge.FeedbackDir(), "journal-2020-01.jsonl"))
	_ = big.Truncate(150 << 20)
	_ = big.Close()
	if c := checkFeedbackJournal(huge, true); c.Status != OK {
		t.Errorf("150 MB + a line: %+v", c)
	}
	_ = os.Truncate(filepath.Join(huge.FeedbackDir(), "journal-2020-01.jsonl"), 201<<20)
	if c := checkFeedbackJournal(huge, true); c.Status != Warn || !strings.Contains(c.Summary, "MB") || c.Fix == "" {
		t.Errorf("201 MB: %+v", c)
	}
	// The month files are what the counts are rebuilt from: the advice
	// never has them moved away as if the counts stayed.
	if c := checkFeedbackJournal(huge, true); strings.Contains(c.Fix, "move the month files") || strings.Contains(c.Fix, "stay") ||
		!strings.Contains(c.Detail, "drops the counts it held") || !strings.Contains(c.Fix, "keep the month files") {
		t.Errorf("201 MB advice: %+v", c)
	}

	// The newest month file cannot be opened for append: an error.
	if os.Geteuid() != 0 {
		locked := feedbackDirs(t, recent, usageNow)
		f := journalFile(t, locked)
		_ = os.Chmod(f, 0o400)
		t.Cleanup(func() { _ = os.Chmod(f, 0o600) })
		if c := checkFeedbackJournal(locked, true); c.Status != Error || !strings.Contains(c.Summary, filepath.Base(f)) || c.Fix == "" {
			t.Errorf("unwritable: %+v", c)
		}
	}

	// The ledger last folded more than 24 h before the newest event.
	stale := feedbackDirs(t, recent, usageNow.Add(-26*time.Hour))
	if c := ledgerCheck(t, stale); c.Status != Warn || !strings.Contains(c.Fix, "backsight jobs tick") {
		t.Errorf("stale fold: %+v", c)
	}
	if c := ledgerCheck(t, feedbackDirs(t, recent, usageNow.Add(-23*time.Hour))); c.Status != OK {
		t.Errorf("23 h behind: %+v", c)
	}

	// [feedback] enabled = false: both inform, whatever the files say.
	for _, c := range []Check{checkFeedbackJournal(huge, false), checkFeedbackLedger(huge, false, ledgerdb.LedgerFacts{OpenError: "notadb"}, nil, usageNow)} {
		if c.Status != Info || !strings.Contains(c.Summary, "enabled = false") {
			t.Errorf("disabled: %+v", c)
		}
	}

	// No journal yet: nothing to judge.
	empty := paths.Dirs{State: t.TempDir()}
	if c := checkFeedbackJournal(empty, true); c.Status != OK {
		t.Errorf("no journal: %+v", c)
	}
	if c := ledgerCheck(t, empty); c.Status != OK {
		t.Errorf("no journal, no ledger: %+v", c)
	}
}

// Run reports both checks, and [feedback] enabled = false in config.toml
// turns them to information.
func TestRunIncludesTheFeedbackChecks(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	expect(t, check(t, r, "feedback-journal"), OK, "no journal yet")
	expect(t, check(t, r, "feedback-ledger"), OK, "")
	expect(t, check(t, r, "feedback-shared"), OK, "not fetched yet")
	srv := fakeServer(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n[feedback]\nenabled = false\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	r = f.run()
	expect(t, check(t, r, "feedback-journal"), Info, "enabled = false")
	expect(t, check(t, r, "feedback-ledger"), Info, "enabled = false")
	expect(t, check(t, r, "feedback-shared"), Info, "enabled = false")
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n[feedback]\nshare_ledger = false\n", 0o600)
	expect(t, check(t, f.run(), "feedback-shared"), Info, "share_ledger = false")
}

// The shared cache: how many other machines and how old. It warns only
// when sharing is on, the server answered within the last day and the
// last fetch is more than 24 h old.
func TestDoctorWarnsOnAStaleSharedCache(t *testing.T) {
	const m1 = "11111111-1111-4111-8111-111111111111"
	dirs := func(answered, fetched time.Time) paths.Dirs {
		t.Helper()
		d := paths.Dirs{State: t.TempDir()}
		if !answered.IsZero() {
			state.RecordReach(d, true, answered)
		}
		if !fetched.IsZero() {
			meta, err := feedback.SharedDoc{V: feedback.SharedVersion, Machine: "pc-b", At: fetched, Entries: map[string]feedback.SharedEntry{m1: {Credits: 1, CreditsAt: fetched}}}.MarshalMeta()
			if err != nil {
				t.Fatal(err)
			}
			if err := feedback.SaveSharedCache(d.FeedbackDir(), feedback.SharedCache{FetchedAt: fetched, Docs: []json.RawMessage{json.RawMessage(meta)}}); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	answered := usageNow.Add(-time.Hour)

	fresh := checkFeedbackShared(dirs(answered, usageNow.Add(-23*time.Hour)), true, true, usageNow)
	if fresh.Status != OK || !strings.Contains(fresh.Summary, "1 other machine") || !strings.Contains(fresh.Summary, "23h") {
		t.Errorf("23 h old: %+v", fresh)
	}
	stale := checkFeedbackShared(dirs(answered, usageNow.Add(-25*time.Hour)), true, true, usageNow)
	if stale.Status != Warn || !strings.Contains(stale.Summary, "25h") || !strings.Contains(stale.Fix, "backsight jobs tick") {
		t.Errorf("25 h old, server answering: %+v", stale)
	}
	// The server silent for two days: the age is not the tick's fault.
	if c := checkFeedbackShared(dirs(usageNow.Add(-48*time.Hour), usageNow.Add(-25*time.Hour)), true, true, usageNow); c.Status != OK {
		t.Errorf("25 h old, server silent: %+v", c)
	}
	if c := checkFeedbackShared(dirs(time.Time{}, usageNow.Add(-25*time.Hour)), true, true, usageNow); c.Status != OK {
		t.Errorf("25 h old, server never reached: %+v", c)
	}
	// Not fetched yet: fine while this machine has not sent its counts, or
	// sent them within a day; a warning once that send is older, the
	// server answering (the fetch that follows every send never came).
	if c := checkFeedbackShared(dirs(answered, time.Time{}), true, true, usageNow); c.Status != OK || !strings.Contains(c.Summary, "not fetched yet") {
		t.Errorf("no cache, never sent: %+v", c)
	}
	sentAt := func(d paths.Dirs, at time.Time) paths.Dirs {
		t.Helper()
		if err := feedback.SaveSharedUpload(d.FeedbackDir(), feedback.SharedUpload{At: at, Hash: "h", HiddenHash: "hh"}); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if c := checkFeedbackShared(sentAt(dirs(answered, time.Time{}), usageNow.Add(-23*time.Hour)), true, true, usageNow); c.Status != OK {
		t.Errorf("no cache, sent 23 h ago: %+v", c)
	}
	if c := checkFeedbackShared(sentAt(dirs(answered, time.Time{}), usageNow.Add(-25*time.Hour)), true, true, usageNow); c.Status != Warn || !strings.Contains(c.Fix, "backsight jobs tick") {
		t.Errorf("no cache, sent 25 h ago, server answering: %+v", c)
	}
	if c := checkFeedbackShared(sentAt(dirs(usageNow.Add(-48*time.Hour), time.Time{}), usageNow.Add(-25*time.Hour)), true, true, usageNow); c.Status != OK {
		t.Errorf("no cache, sent 25 h ago, server silent: %+v", c)
	}
	// An unreadable cache, the server answering: a warning.
	broken := dirs(answered, time.Time{})
	_ = os.MkdirAll(broken.FeedbackDir(), 0o700)
	_ = os.WriteFile(feedback.SharedCacheFile(broken.FeedbackDir()), []byte("{"), 0o600)
	if c := checkFeedbackShared(broken, true, true, usageNow); c.Status != Warn || !strings.Contains(c.Fix, "backsight jobs tick") {
		t.Errorf("unreadable: %+v", c)
	}
	silent := dirs(usageNow.Add(-48*time.Hour), time.Time{})
	_ = os.MkdirAll(silent.FeedbackDir(), 0o700)
	_ = os.WriteFile(feedback.SharedCacheFile(silent.FeedbackDir()), []byte("{"), 0o600)
	if c := checkFeedbackShared(silent, true, true, usageNow); c.Status != Info || !strings.Contains(c.Summary, "unreadable") {
		t.Errorf("unreadable, server silent: %+v", c)
	}
	// Sharing off, or feedback off: information, whatever the cache says.
	if c := checkFeedbackShared(dirs(answered, usageNow.Add(-25*time.Hour)), true, false, usageNow); c.Status != Info || !strings.Contains(c.Summary, "share_ledger = false") {
		t.Errorf("sharing off: %+v", c)
	}
	if c := checkFeedbackShared(dirs(answered, usageNow.Add(-25*time.Hour)), false, true, usageNow); c.Status != Info || !strings.Contains(c.Summary, "enabled = false") {
		t.Errorf("feedback off: %+v", c)
	}
}

// A restore of a memory this machine never showed leaves an entry
// holding only its time: the ledger check does not count it.
func TestDoctorDoesNotCountARestoreAsAMemory(t *testing.T) {
	d := feedbackDirs(t, usageNow.Add(-time.Hour), usageNow.Add(-20*time.Minute))
	j := feedback.NewJournal(d.FeedbackDir(), feedback.Common{})
	if err := j.Append(feedback.Event{Kind: feedback.KindUnhide, TS: usageNow.Add(-15 * time.Minute), IDs: []string{"never-shown"}, Outcome: "restored"}); err != nil {
		t.Fatal(err)
	}
	foldAt(t, d, usageNow.Add(-10*time.Minute))
	if l, _ := feedback.LoadLedger(d.FeedbackDir()); len(l.Memories) != 1 {
		t.Fatalf("ledger %+v", l.Memories)
	}
	if c := ledgerCheck(t, d); c.Status != OK || !strings.HasPrefix(c.Summary, "0 memories,") {
		t.Errorf("%+v", c)
	}
}

// The texts of the ledger.json check this one replaces: the tick no
// longer keeps a file aside at each run, and a database that cannot be
// read stops no settle.
var oldLedgerDetails = []string{"the next tick keeps it aside and rebuilds it from the journal", "the tick settles nothing while it cannot be read"}

func noOldDetail(t *testing.T, name string, c Check) {
	t.Helper()
	for _, old := range oldLedgerDetails {
		if strings.Contains(c.Summary+c.Detail+c.Fix, old) {
			t.Errorf("%s: %q in %+v", name, old, c)
		}
	}
}

// TestDoctorLedgerFacts: the feedback-ledger check reads the ledger
// database's facts: built and folded lately is fine; missing is "not
// built yet" until a recent tick could not build it, which names the
// folder and why; a side file beside a newer database is named; a fold
// more than a day behind the journal is the lag warning, an empty fold
// is no lag.
func TestDoctorLedgerFacts(t *testing.T) {
	recent := usageNow.Add(-30 * time.Minute)
	cases := []struct {
		name  string
		check func(t *testing.T) Check
		want  Status
		has   []string
	}{
		{"built and current", func(t *testing.T) Check {
			return ledgerCheck(t, feedbackDirs(t, recent, usageNow.Add(-10*time.Minute)))
		}, OK, []string{"memories, settled up to"}},
		{"missing, a journal, no tick", func(t *testing.T) Check {
			return ledgerCheck(t, feedbackDirs(t, recent, time.Time{}))
		}, OK, []string{"not built yet: it builds at the next jobs tick"}},
		{"missing, a journal, no tick, the folder read-only", func(t *testing.T) Check {
			d := feedbackDirs(t, recent, time.Time{})
			return checkFeedbackLedger(d, true, ledgerdb.LedgerFacts{File: ledgerdb.FileName, Head: 1, OpenError: "readonly"}, nil, usageNow)
		}, OK, []string{"not built yet"}},
		{"missing, a tick in the last hour, the folder read-only", func(t *testing.T) Check {
			d := feedbackDirs(t, recent, time.Time{})
			telemetryAt(t, d, map[time.Duration][]string{10 * time.Minute: {tickLine}})
			return checkFeedbackLedger(d, true, ledgerdb.LedgerFacts{File: ledgerdb.FileName, Head: 1, OpenError: "readonly"}, nil, usageNow)
		}, Warn, []string{"readonly", "feedback"}},
		{"a side file in use", func(t *testing.T) Check {
			d := feedbackDirs(t, recent, usageNow.Add(-10*time.Minute))
			db, err := sql.Open("sqlite", "file:"+filepath.Join(d.FeedbackDir(), ledgerdb.FileName))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			foldAt(t, d, usageNow.Add(-5*time.Minute))
			return ledgerCheck(t, d)
		}, Warn, []string{ledgerdb.SideFileName(1)}},
		{"a fold 25 h old, a tick lately", func(t *testing.T) Check {
			d := feedbackDirs(t, recent, usageNow.Add(-25*time.Hour))
			telemetryAt(t, d, map[time.Duration][]string{10 * time.Minute: {tickLine}})
			return ledgerCheck(t, d)
		}, Warn, []string{"folded up to", "backsight jobs tick"}},
		{"an empty fold 1 h ago, no show for a day", func(t *testing.T) Check {
			d := feedbackDirs(t, usageNow.Add(-30*time.Hour), usageNow.Add(-29*time.Hour))
			foldAt(t, d, usageNow.Add(-26*time.Hour)) // the last show settled: the watermark
			foldAt(t, d, usageNow.Add(-time.Hour))    // nothing new
			telemetryAt(t, d, map[time.Duration][]string{10 * time.Minute: {tickLine}})
			return ledgerCheck(t, d)
		}, OK, []string{"memories, settled up to"}},
		{"a show 10 min ago, after an empty fold 1 h ago", func(t *testing.T) Check {
			// The watermark is a day old; the fold is not behind.
			d := feedbackDirs(t, usageNow.Add(-30*time.Hour), usageNow.Add(-29*time.Hour))
			foldAt(t, d, usageNow.Add(-26*time.Hour))
			foldAt(t, d, usageNow.Add(-time.Hour))
			j := feedback.NewJournal(d.FeedbackDir(), feedback.Common{Session: "s2"})
			if err := j.Append(feedback.Event{Kind: "shows", TS: usageNow.Add(-10 * time.Minute), Path: "prompt", Items: []feedback.Item{{ID: "m2", Rank: 1}}}); err != nil {
				t.Fatal(err)
			}
			return ledgerCheck(t, d)
		}, OK, []string{"memories, settled up to"}},
		{"a corrupt database", func(t *testing.T) Check {
			d := feedbackDirs(t, recent, usageNow.Add(-10*time.Minute))
			if err := os.WriteFile(filepath.Join(d.FeedbackDir(), ledgerdb.FileName), []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
				t.Fatal(err)
			}
			return ledgerCheck(t, d)
		}, Warn, []string{"notadb", "backsight feedback rebuild"}},
		{"an export behind at two ticks", func(t *testing.T) Check {
			d := feedbackDirs(t, recent, usageNow.Add(-10*time.Minute))
			f, err := ledgerdb.Facts(ledgerdb.Options{Dirs: d})
			if err != nil {
				t.Fatal(err)
			}
			f.ExportBehind = true
			telemetryAt(t, d, map[time.Duration][]string{20 * time.Minute: fallbacks("ledger_export", 1), 5 * time.Minute: fallbacks("ledger_export", 1)})
			return checkFeedbackLedger(d, true, f, nil, usageNow)
		}, Warn, []string{"ledger.json"}},
		{"an export behind, no failure logged", func(t *testing.T) Check {
			d := feedbackDirs(t, recent, usageNow.Add(-10*time.Minute))
			f, err := ledgerdb.Facts(ledgerdb.Options{Dirs: d})
			if err != nil {
				t.Fatal(err)
			}
			f.ExportBehind = true
			return checkFeedbackLedger(d, true, f, nil, usageNow)
		}, OK, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.check(t)
			if got.ID != "feedback-ledger" || got.Status != c.want {
				t.Errorf("%+v, want %s", got, c.want)
			}
			for _, h := range c.has {
				if !strings.Contains(got.Summary+" "+got.Detail+" "+got.Fix, h) {
					t.Errorf("%+v: no %q", got, h)
				}
			}
			noOldDetail(t, c.name, got)
		})
	}
}

// TestDoctorReportsRepeatedLedgerFallbacks: three reads or more in a day
// that fell back past the database (or failed to fold or export it) are
// a warning naming the most frequent reason and the rebuild; fewer, older
// ones and ledger_missing (the missing line's) are not.
func TestDoctorReportsRepeatedLedgerFallbacks(t *testing.T) {
	recent := usageNow.Add(-30 * time.Minute)
	for _, c := range []struct {
		name  string
		lines map[time.Duration][]string
		want  Status
		has   []string
	}{
		{"2 ledger_busy", map[time.Duration][]string{time.Hour: fallbacks("ledger_busy", 2)}, OK, nil},
		{"3 ledger_busy", map[time.Duration][]string{time.Hour: fallbacks("ledger_busy", 3)}, Warn, []string{"ledger_busy", "backsight feedback rebuild"}},
		{"3 older than 24 h", map[time.Duration][]string{25 * time.Hour: fallbacks("ledger_busy", 3)}, OK, nil},
		{"10 ledger_missing", map[time.Duration][]string{time.Hour: fallbacks("ledger_missing", 10)}, OK, nil},
		{"the most frequent named", map[time.Duration][]string{time.Hour: append(fallbacks("ledger_busy", 3), fallbacks("ledger_json", 4)...)}, Warn, []string{"mostly ledger_json"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := feedbackDirs(t, recent, usageNow.Add(-10*time.Minute))
			telemetryAt(t, d, c.lines)
			got := ledgerCheck(t, d)
			if got.Status != c.want {
				t.Errorf("%+v, want %s", got, c.want)
			}
			for _, h := range c.has {
				if !strings.Contains(got.Summary+" "+got.Fix, h) {
					t.Errorf("%+v: no %q", got, h)
				}
			}
		})
	}
}
