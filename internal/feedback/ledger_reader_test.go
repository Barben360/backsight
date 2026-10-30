package feedback

import (
	"path/filepath"
	"testing"
	"time"
)

// The reader never waits for the ledger lock: with the lock held, All
// answers at once, with a hide journaled after the watermark applied.
func TestCurrentReaderReadsWithoutTheLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	if _, err := Settle(dir, testParams, fixedClock(at(120))); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, dir, Event{Kind: KindHide, TS: at(130), IDs: []string{"o1"}, Verdict: "wrong", Note: "n1"})
	release, err := lockLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var r LedgerReader = CurrentReader{Dir: dir}
	type result struct {
		l   Ledger
		err error
	}
	done := make(chan result, 1)
	go func() {
		l, err := r.All()
		done <- result{l, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal(res.err)
		}
		if h := res.l.Memories["o1"].Hidden; h == nil || !h.Since.Equal(at(130)) {
			t.Errorf("o1 not hidden since the journaled hide: %+v", res.l.Memories["o1"])
		}
		if res.l.Memories["m1"].Shown != 1 {
			t.Errorf("the settled ledger is missing: %+v", res.l.Memories)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("All waited for the ledger lock")
	}
}
