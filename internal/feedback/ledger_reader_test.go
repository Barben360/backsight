package feedback

import (
	"path/filepath"
	"testing"
)

// The reader never waits for the ledger lock: with the lock held, All
// answers at once, with a hide journaled after the watermark applied.
func TestCurrentReaderReadsWithoutTheLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	if _, err := LegacySettle(dir, testParams, fixedClock(at(120))); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, dir, Event{Kind: KindHide, TS: at(130), IDs: []string{"o1"}, Verdict: "wrong", Note: "n1"})
	release, err := lockLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// The lock is held until the test returns: a reader that waits for it
	// either hangs until go test's -timeout or meets the hook.
	waited := false
	inLockWait = func() { waited = true }
	t.Cleanup(func() { inLockWait = nil })

	var r LedgerReader = CurrentReader{Dir: dir}
	l, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	if waited {
		t.Error("All waited for the ledger lock")
	}
	if h := l.Memories["o1"].Hidden; h == nil || !h.Since.Equal(at(130)) {
		t.Errorf("o1 not hidden since the journaled hide: %+v", l.Memories["o1"])
	}
	if l.Memories["m1"].Shown != 1 {
		t.Errorf("the settled ledger is missing: %+v", l.Memories)
	}
}

// TestCurrentReaderGetHidden: Get returns only the asked ids that have an
// entry, Hidden only the hidden entries, both with the lines journaled
// since the settle applied; GetAny is Get with Any's answer.
func TestCurrentReaderGetHidden(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	r := CurrentReader{Dir: dir}
	if m, has, err := r.GetAny([]string{"m1"}); err != nil || has || len(m) != 0 {
		t.Fatalf("GetAny without a ledger: %v, %v, %v", m, has, err)
	}
	writeJournal(t, dir, sampleSession()...)
	if _, err := LegacySettle(dir, testParams, fixedClock(at(120))); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, dir, Event{Kind: KindHide, TS: at(130), IDs: []string{"m2"}, Verdict: "wrong"})

	got, err := r.Get([]string{"m1", "m2", "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["m1"].Shown != 1 || got["m1"].Hidden != nil || got["m2"].Hidden == nil {
		t.Errorf("Get: %+v, want m1 shown once and m2 hidden by the journal, nothing for nope", got)
	}
	hidden, err := r.Hidden()
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 1 || hidden["m2"].Hidden == nil || !hidden["m2"].Hidden.Since.Equal(at(130)) {
		t.Errorf("Hidden: %+v, want m2 alone", hidden)
	}
	m, has, err := r.GetAny([]string{"nope"})
	if err != nil || !has || len(m) != 0 {
		t.Errorf("GetAny of an id without an entry: %v, %v, %v; want none, and an entry elsewhere", m, has, err)
	}
	if has, err := r.Any(); err != nil || !has {
		t.Errorf("Any: %v, %v", has, err)
	}
}
