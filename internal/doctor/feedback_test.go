package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// feedbackDirs is a machine whose journal holds one shows event at last,
// and whose ledger last settled at watermark (zero: no ledger).
func feedbackDirs(t *testing.T, last, watermark time.Time) paths.Dirs {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	j := feedback.NewJournal(d.FeedbackDir(), feedback.Common{Session: "s1"})
	if err := j.Append(feedback.Event{Kind: "shows", TS: last, Path: "prompt", Items: []feedback.Item{{ID: "m1", Rank: 1}}}); err != nil {
		t.Fatal(err)
	}
	if !watermark.IsZero() {
		if _, err := feedback.Settle(d.FeedbackDir(), feedback.Params{ConfidentRank: 2, TurnTimeout: feedback.TurnTimeout}, watermark); err != nil {
			t.Fatal(err)
		}
	}
	return d
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
	if c := checkFeedbackLedger(healthy, true); c.Status != OK {
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

	// The ledger last settled more than 24 h before the newest event.
	stale := feedbackDirs(t, recent, usageNow.Add(-26*time.Hour))
	if c := checkFeedbackLedger(stale, true); c.Status != Warn || !strings.Contains(c.Fix, "backsight jobs tick") {
		t.Errorf("stale watermark: %+v", c)
	}
	if c := checkFeedbackLedger(feedbackDirs(t, recent, usageNow.Add(-23*time.Hour)), true); c.Status != OK {
		t.Errorf("23 h behind: %+v", c)
	}
	// Never settled, and the journal spans more than a day.
	never := feedbackDirs(t, recent, time.Time{})
	j := feedback.NewJournal(never.FeedbackDir(), feedback.Common{Session: "s0"})
	_ = j.Append(feedback.Event{Kind: "turn", Turn: "p0", TS: recent.Add(-30 * time.Hour)})
	if c := checkFeedbackLedger(never, true); c.Status != Warn {
		t.Errorf("never settled: %+v", c)
	}
	if c := checkFeedbackLedger(feedbackDirs(t, recent, time.Time{}), true); c.Status != OK {
		t.Errorf("never settled, one fresh event: %+v", c)
	}

	// A ledger of another schema: an error naming the rebuild.
	unknown := feedbackDirs(t, recent, usageNow)
	_ = os.WriteFile(filepath.Join(unknown.FeedbackDir(), "ledger.json"), []byte(`{"schema":9,"memories":{}}`), 0o600)
	if c := checkFeedbackLedger(unknown, true); c.Status != Error || !strings.Contains(c.Fix, "backsight feedback rebuild") {
		t.Errorf("unknown schema: %+v", c)
	}

	// [feedback] enabled = false: both inform, whatever the files say.
	for _, c := range []Check{checkFeedbackJournal(huge, false), checkFeedbackLedger(unknown, false)} {
		if c.Status != Info || !strings.Contains(c.Summary, "enabled = false") {
			t.Errorf("disabled: %+v", c)
		}
	}

	// No journal yet: nothing to judge.
	empty := paths.Dirs{State: t.TempDir()}
	if c := checkFeedbackJournal(empty, true); c.Status != OK {
		t.Errorf("no journal: %+v", c)
	}
	if c := checkFeedbackLedger(empty, true); c.Status != OK {
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
