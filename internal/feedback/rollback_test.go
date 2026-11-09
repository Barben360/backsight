package feedback_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

const day = 24 * time.Hour

var rollbackParams = feedback.Params{ConfidentRank: 3, MissHalfLife: 30 * day, CreditHalfLife: 60 * day, TurnTimeout: 30 * time.Minute}

// rollbackStart is when the first session begins, in November 2026.
var rollbackStart = time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)

func ledgerOpts(t *testing.T) ledgerdb.Options {
	t.Helper()
	return ledgerdb.Options{
		Dirs:          paths.Dirs{State: filepath.Join(t.TempDir(), "state")},
		Getenv:        func(string) string { return "" },
		BinaryVersion: "v-test",
		Now:           func() time.Time { return rollbackStart },
	}
}

func fixed(ts time.Time) func() time.Time { return func() time.Time { return ts } }

// journalSession journals into dir one session id, starting at start: a
// declared turn showing id1 (rank 1, credited), id2 (rank 2, a miss) and
// a store reply's id3, a used credit of a memory never shown (late), and
// a second session's undeclared turn showing id4 and id5, id5 then
// forgotten (counted nowhere).
func journalSession(t *testing.T, dir, sid string, start time.Time, id1, id2, id3, id4, id5 string) {
	t.Helper()
	j := feedback.NewJournal(dir, feedback.Common{Session: sid, Binary: "v-test"})
	other := sid + "-b"
	for _, e := range []feedback.Event{
		{Kind: "turn", TS: start, Turn: "p1", N: 1},
		{Kind: "shows", TS: start.Add(time.Minute), Turn: "p1", Path: "prompt", Items: []feedback.Item{{ID: id1, Rank: 1}, {ID: id2, Rank: 2}}},
		{Kind: "shows", TS: start.Add(90 * time.Second), Turn: "p1", Path: feedback.PathStore, Items: []feedback.Item{{ID: id3, Rank: 1}}},
		{Kind: "credit", TS: start.Add(2 * time.Minute), Turn: "p1", Verdict: "used", IDs: []string{id1}},
		{Kind: "credit", TS: start.Add(150 * time.Second), Turn: "p1", Verdict: "used", IDs: []string{"never-shown-" + sid}},
		{Kind: "stop", TS: start.Add(3 * time.Minute), Turn: "p1", Outcome: "declared"},
		{Kind: "turn", TS: start.Add(5 * time.Minute), Session: other, Turn: "q1", N: 1},
		{Kind: "shows", TS: start.Add(6 * time.Minute), Session: other, Turn: "q1", Path: "search", Items: []feedback.Item{{ID: id4, Rank: 1}, {ID: id5, Rank: 2}}},
		{Kind: "forget", TS: start.Add(10 * time.Minute), IDs: []string{id5}},
	} {
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

// appendFolded journals a line the ledger folds as v0.1.32 does: under the
// ledger lock, its ts raised above ledger.json's watermark.
func appendFolded(t *testing.T, dir string, e feedback.Event) {
	t.Helper()
	if err := feedback.NewJournal(dir, feedback.Common{Binary: "v-test"}).AppendFolded(e); err != nil {
		t.Fatal(err)
	}
}

func ledgerSettle(t *testing.T, o ledgerdb.Options, now time.Time) feedback.SettleResult {
	t.Helper()
	res, err := ledgerdb.Settle(context.Background(), o, rollbackParams, fixed(now))
	if err != nil {
		t.Fatalf("ledgerdb.Settle at %v: %v", now, err)
	}
	return res
}

func legacySettle(t *testing.T, dir string, now time.Time) feedback.SettleResult {
	t.Helper()
	res, err := feedback.LegacySettle(dir, rollbackParams, fixed(now))
	if err != nil {
		t.Fatalf("LegacySettle at %v: %v", now, err)
	}
	return res
}

// memoriesJSON is memories as ledger.json writes them: equal strings
// are equal entries, times to the nanosecond and floats bit for bit.
func memoriesJSON(t *testing.T, memories map[string]feedback.Entry) string {
	t.Helper()
	b, err := json.Marshal(memories)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sameMemories(t *testing.T, what string, got, want map[string]feedback.Entry) {
	t.Helper()
	if g, w := memoriesJSON(t, got), memoriesJSON(t, want); g != w {
		t.Errorf("%s:\n got %s\nwant %s", what, g, w)
	}
}

func loadJSON(t *testing.T, dir string) feedback.Ledger {
	t.Helper()
	l, err := feedback.LoadLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.json")); err != nil {
		t.Fatalf("no ledger.json: %v", err)
	}
	return l
}

// dbMemories is ledger.db's ledger, through a reader.
func dbMemories(t *testing.T, o ledgerdb.Options) map[string]feedback.Entry {
	t.Helper()
	r, err := ledgerdb.OpenReader(o, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	l, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	return l.Memories
}

// oracleMemories is a rebuild of dir's journal: every line folded in
// time order, each settle key once.
func oracleMemories(t *testing.T, dir string) map[string]feedback.Entry {
	t.Helper()
	events, _, _, err := feedback.ReadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	return feedback.Fold(events, rollbackParams).Memories
}

func TestSettleCountsAsLegacySettle(t *testing.T) {
	o := ledgerOpts(t)
	dbDir, legacyDir := o.Dirs.FeedbackDir(), filepath.Join(t.TempDir(), "feedback")
	journal := func(sid string, start time.Time, ids ...string) {
		for _, dir := range []string{dbDir, legacyDir} {
			journalSession(t, dir, sid, start, ids[0], ids[1], ids[2], ids[3], ids[4])
		}
	}
	journal("s1", rollbackStart, "m1", "m2", "m3", "m4", "m5")
	for _, dir := range []string{dbDir, legacyDir} {
		appendFolded(t, dir, feedback.Event{Kind: feedback.KindRetire, TS: rollbackStart.Add(20 * time.Minute), IDs: []string{"m2"}, Verdict: "wrong"})
		appendFolded(t, dir, feedback.Event{Kind: feedback.KindHide, TS: rollbackStart.Add(25 * time.Minute), IDs: []string{"m4"}, Verdict: "outdated", Note: "doc-1"})
	}
	first := rollbackStart.Add(3 * time.Hour)
	got, want := ledgerSettle(t, o, first), legacySettle(t, legacyDir, first)
	if want != (feedback.SettleResult{Shows: 4, Credits: 1, Misses: 1, Unknown: 1, Late: 1}) {
		t.Fatalf("the legacy settle gave %+v: the fixture no longer covers every outcome", want)
	}
	if got != want {
		t.Errorf("first settle: %+v, the legacy settle %+v", got, want)
	}
	sameMemories(t, "after the first settle", dbMemories(t, o), loadJSON(t, legacyDir).Memories)

	// A second session (m1 shown again), an unhide, and a later settle.
	journal("s2", rollbackStart.Add(5*time.Hour), "m1", "m6", "m7", "m8", "m9")
	for _, dir := range []string{dbDir, legacyDir} {
		appendFolded(t, dir, feedback.Event{Kind: feedback.KindUnhide, TS: rollbackStart.Add(5*time.Hour + 20*time.Minute), IDs: []string{"m4"}, Outcome: "restored"})
	}
	second := rollbackStart.Add(9 * time.Hour)
	if got, want := ledgerSettle(t, o, second), legacySettle(t, legacyDir, second); got != want || want.Shows == 0 {
		t.Errorf("second settle: %+v, the legacy settle %+v", got, want)
	}
	legacy := loadJSON(t, legacyDir).Memories
	sameMemories(t, "after the second settle", dbMemories(t, o), legacy)
	if e := legacy["m1"]; e.Shown != 2 || e.Credits == 0 {
		t.Errorf("m1 %+v: want shown twice and credited", e)
	}
}

// Rolled back after the new settle ran, v0.1.32's settle meets the
// ledger.json the database exported: it counts nothing twice, and the
// re-upgrade gives the rebuild again.
func TestRollbackSettleCountsNothingTwice(t *testing.T) {
	o := ledgerOpts(t)
	dir := o.Dirs.FeedbackDir()
	for i, ids := range [][]string{{"m1", "m2", "m3", "m4", "m5"}, {"m1", "m6", "m3", "m7", "m8"}, {"m2", "m6", "m9", "m4", "m10"}} {
		start := rollbackStart.Add(time.Duration(i) * 4 * time.Hour)
		journalSession(t, dir, "s"+string(rune('1'+i)), start, ids[0], ids[1], ids[2], ids[3], ids[4])
		ledgerSettle(t, o, start.Add(3*time.Hour))
	}
	if n := len(settleLines(t, dir)); n != 3 {
		t.Fatalf("%d settle lines, want 3", n)
	}

	// The rollback: new shows and a hide, then v0.1.32's settle, twice.
	rolled := rollbackStart.Add(13 * time.Hour)
	journalSession(t, dir, "r1", rolled, "m1", "m11", "m3", "m12", "m13")
	appendFolded(t, dir, feedback.Event{Kind: feedback.KindHide, TS: rolled.Add(20 * time.Minute), IDs: []string{"m6"}, Verdict: "wrong", Note: "doc-2"})
	res := legacySettle(t, dir, rolled.Add(3*time.Hour))
	if res.Shows == 0 {
		t.Fatalf("the legacy settle settled nothing: %+v", res)
	}
	afterLegacy := loadJSON(t, dir).Memories
	sameMemories(t, "after the legacy settle", afterLegacy, oracleMemories(t, dir))
	if e := afterLegacy["m1"]; e.Shown != 3 {
		t.Errorf("m1 shown %d, want 3: once per session", e.Shown)
	}
	if afterLegacy["m6"].Hidden == nil {
		t.Error("the hide journaled before the legacy settle is not applied")
	}
	if again := legacySettle(t, dir, rolled.Add(4*time.Hour)); again != (feedback.SettleResult{}) {
		t.Errorf("the second legacy settle decided %+v", again)
	}
	sameMemories(t, "after the second legacy settle", loadJSON(t, dir).Memories, afterLegacy)

	// The re-upgrade.
	ledgerSettle(t, o, rolled.Add(5*time.Hour))
	sameMemories(t, "after the re-upgrade", dbMemories(t, o), oracleMemories(t, dir))
	sameMemories(t, "ledger.json after the re-upgrade", loadJSON(t, dir).Memories, dbMemories(t, o))
	if n := len(settleLines(t, dir)); n != 4 {
		t.Errorf("%d settle lines, want 4: the re-upgrade decided again what the legacy settle did", n)
	}
}

func settleLines(t *testing.T, dir string) []feedback.Event {
	t.Helper()
	events, _, _, err := feedback.ReadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(events, func(e feedback.Event) bool { return e.Kind != "settle" })
}

// journaled reports whether dir's journal holds a line of kind for id.
func journaled(t *testing.T, dir, kind, id string) (feedback.Event, bool) {
	t.Helper()
	events, _, _, err := feedback.ReadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == kind && slices.Contains(e.IDs, id) {
			return e, true
		}
	}
	return feedback.Event{}, false
}

// AppendFolded keeps v0.1.32's lock: an older binary's settle, run
// during a manual downgrade, reads the line or sees it stamped after it.
func TestAppendFoldedStillTakesTheLock(t *testing.T) {
	o := ledgerOpts(t)
	dir := o.Dirs.FeedbackDir()
	journalSession(t, dir, "s1", rollbackStart, "m1", "m2", "m3", "m4", "m5")
	ledgerSettle(t, o, rollbackStart.Add(3*time.Hour))
	watermark := loadJSON(t, dir).Watermark
	if watermark.IsZero() {
		t.Fatal("the export holds no watermark")
	}
	early := watermark.Add(-time.Hour) // a clock behind the ledger's

	lock, err := state.TryAcquire(filepath.Join(o.Dirs.LocksDir(), "feedback-ledger.lock"), "test")
	if err != nil {
		t.Fatal(err)
	}
	// done is signalled once AppendFolded returns. Its error is the lock's
	// only when it waited the whole of its wait for the lock (lockLedgerWait
	// returns it at the deadline, never before), so the error says exactly
	// that it waited, and the hide is journaled by then.
	done := make(chan error, 1)
	go func() {
		done <- feedback.NewJournal(dir, feedback.Common{}).AppendFolded(feedback.Event{Kind: feedback.KindHide, TS: early, IDs: []string{"m1"}, Verdict: "wrong"})
	}()
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("AppendFolded never returned")
	}
	var held *state.HeldError
	if !errors.As(err, &held) {
		t.Errorf("AppendFolded with the lock held: %v, want the lock's error (it waited its whole wait)", err)
	}
	if e, ok := journaled(t, dir, feedback.KindHide, "m1"); !ok || !e.TS.After(watermark) {
		t.Errorf("hide %+v (journaled %v): want it journaled after the watermark %v", e, ok, watermark)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	// Released while it waits: it takes the lock (no error) instead of
	// giving up.
	if lock, err = state.TryAcquire(filepath.Join(o.Dirs.LocksDir(), "feedback-ledger.lock"), "test"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	go func() {
		close(started)
		done <- feedback.NewJournal(dir, feedback.Common{}).AppendFolded(feedback.Event{Kind: feedback.KindHide, TS: early, IDs: []string{"m3"}, Verdict: "wrong"})
	}()
	<-started
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("AppendFolded never returned")
	}
	if err != nil {
		t.Errorf("AppendFolded with the lock released during its wait: %v, want nil", err)
	}

	if err := feedback.NewJournal(dir, feedback.Common{}).AppendFolded(feedback.Event{Kind: feedback.KindUnhide, TS: early, IDs: []string{"m2"}}); err != nil {
		t.Errorf("AppendFolded with the lock free: %v", err)
	}
	if e, ok := journaled(t, dir, feedback.KindUnhide, "m2"); !ok || !e.TS.After(watermark) {
		t.Errorf("unhide %+v (journaled %v): want it journaled after the watermark %v", e, ok, watermark)
	}
}
