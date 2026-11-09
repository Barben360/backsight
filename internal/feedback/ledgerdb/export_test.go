package ledgerdb

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/sqlitedb"
	"github.com/Barben360/backsight/internal/state"
)

func mustExport(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Export(); err != nil {
		t.Fatalf("Export: %v", err)
	}
}

func loadExport(t *testing.T, o Options) feedback.Ledger {
	t.Helper()
	l, err := feedback.LoadLedger(feedbackDir(o))
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	if _, err := os.Stat(filepath.Join(feedbackDir(o), "ledger.json")); err != nil {
		t.Fatalf("no export: %v", err)
	}
	return l
}

func TestExportIsFaithful(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o,
		settleOf(oct(10), show("k1", "m1", oct(9), feedback.OutcomeCredit), show("k2", "m2", oct(9), feedback.OutcomeMiss)),
		importOf(nov(1), "m3", oct(0), oct(50)),
		feedback.Event{Kind: feedback.KindImport, TS: nov(1).Add(1), Outcome: feedback.ImportComplete})
	mustFold(t, w, testParams)
	// After the last settle: a watermark of settles only would let an
	// older reader retire m1 a second time.
	appendLines(t, o, settleOf(nov(2), show("k3", "m1", nov(1), feedback.OutcomeShown)), hideOf(nov(3), "m2"), retireOf(nov(4), "m1"))
	mustFold(t, w, testParams)
	mustExport(t, w)

	all := readerAll(t, o)
	got := loadExport(t, o)
	sameLedger(t, "ledger.json", got, all)
	if wm := metaInt(t, o, "watermark_ns"); !got.Watermark.Equal(fromNS(wm)) || wm != nov(4).UnixNano() {
		t.Errorf("ledger.json watermark %v, watermark_ns %d, want %v", got.Watermark, wm, nov(4))
	}
	latest := feedback.MonthFile(nov(0))
	if want := (feedback.JournalPos{File: latest, Offset: cursorsOf(t, o)[latest]}); got.JournalAt != want || want.Offset == 0 {
		t.Errorf("ledger.json journal_at %+v, want %+v", got.JournalAt, want)
	}
	cur, err := feedback.Current(feedbackDir(o))
	if err != nil {
		t.Fatal(err)
	}
	sameLedger(t, "Current over the export", cur, all)
	if m := metaOf(t, o); m["exported_generation"] != m["generation"] {
		t.Errorf("exported_generation %s, generation %s", m["exported_generation"], m["generation"])
	}
	if fi, err := os.Stat(filepath.Join(feedbackDir(o), "ledger.json")); err != nil {
		t.Error(err)
	} else if os.PathSeparator == '/' && fi.Mode().Perm() != 0o600 {
		t.Errorf("ledger.json mode %v, want 0600", fi.Mode())
	}
}

func TestExportIsOneSnapshot(t *testing.T) {
	forceFS(t, sqlitedb.FSLocal)
	fakeProbe(t, true) // WAL: a read transaction does not hold a writer back
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	other := mustOpenWriter(t, o)
	defer mustCloseWriter(t, other)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)), hideOf(nov(3), "m1"))
	mustFold(t, w, testParams)
	atG, g := folded(t, o), metaInt(t, o, "generation")
	appendLines(t, o, settleOf(nov(5), show("k2", "m2", nov(4), feedback.OutcomeShown)), retireOf(nov(6), "m1"))
	var once sync.Once
	setSeam(t, &betweenExportReads, func() {
		once.Do(func() {
			if _, err := other.Fold(context.Background(), testParams); err != nil {
				t.Errorf("the fold between the export's reads: %v", err)
			}
		})
	})
	mustExport(t, w)
	if gen := metaInt(t, o, "generation"); gen != g+1 {
		t.Fatalf("generation %d, want the other fold's %d", gen, g+1)
	}
	got := loadExport(t, o)
	sameLedger(t, "the export of generation g", got, atG) // memories and watermark alike
	if e := metaOf(t, o)["exported_generation"]; e != strconv.FormatInt(g, 10) {
		t.Errorf("exported_generation %s, want %d", e, g)
	}

	mustExport(t, w)
	sameLedger(t, "the next export", loadExport(t, o), folded(t, o))
	if e := metaOf(t, o)["exported_generation"]; e != strconv.FormatInt(g+1, 10) {
		t.Errorf("exported_generation %s, want %d", e, g+1)
	}
}

func TestCrashBetweenFoldAndExport(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)))
	mustFold(t, w, testParams)
	mustExport(t, w)
	// ledger.json cannot be replaced: a folder in its place.
	blocked := filepath.Join(feedbackDir(o), "ledger.json")
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(blocked, "x"), []byte("x"))
	appendLines(t, o, retireOf(nov(3), "m1"))
	res := mustFold(t, w, testParams)
	if err := w.Export(); err == nil {
		t.Fatal("Export over a folder succeeded")
	}
	if e := folded(t, o).Memories["m1"]; e.Wrong != 1 {
		t.Errorf("m1 %+v: the fold must commit without its export", e)
	}
	exported, gen := metaInt(t, o, "exported_generation"), metaInt(t, o, "generation")
	if exported >= gen || gen != res.Generation {
		t.Errorf("exported_generation %d, generation %d (fold %d): the export must be behind", exported, gen, res.Generation)
	}

	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}
	if res := mustFold(t, w, testParams); res.Lines != 0 || res.Generation != gen {
		t.Errorf("a fold of nothing: %+v", res)
	}
	mustExport(t, w)
	if e := loadExport(t, o).Memories["m1"]; e.Wrong != 1 {
		t.Errorf("the retried export: m1 %+v, want retired once", e)
	}
	if exported := metaInt(t, o, "exported_generation"); exported != gen {
		t.Errorf("exported_generation %d, want %d", exported, gen)
	}
}

// TestConcurrentExportsAgreeWithTheirGeneration: an export waits for
// another's lock, so ledger.json always holds the rows of
// exported_generation.
func TestConcurrentExportsAgreeWithTheirGeneration(t *testing.T) {
	forceFS(t, sqlitedb.FSLocal)
	fakeProbe(t, true) // WAL: a read transaction does not hold a writer back
	o := tempOpts(t)
	a := mustOpenWriter(t, o)
	defer mustCloseWriter(t, a)
	b := mustOpenWriter(t, o)
	defer mustCloseWriter(t, b)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)))
	mustFold(t, a, testParams)
	appendLines(t, o, retireOf(nov(3), "m1"))
	// A's export, inside its snapshot of generation g, has another fold
	// commit g+1, then starts B's export and waits: B must wait for A's
	// lock. Without it, B exports g+1 within the wait and A then writes g
	// over it. Only the first export to reach the seam acts on it: B's
	// passes straight through.
	setSeam(t, &exportLockWait, 10*time.Second)
	bDone := make(chan error, 1)
	var claimed atomic.Bool
	setSeam(t, &betweenExportReads, func() {
		if !claimed.CompareAndSwap(false, true) {
			return
		}
		if _, err := b.Fold(context.Background(), testParams); err != nil {
			t.Errorf("the fold between the exports: %v", err)
		}
		go func() { bDone <- b.Export() }()
		select {
		case err := <-bDone:
			t.Errorf("the second export finished (%v) while the first held its snapshot", err)
			bDone <- err
		case <-time.After(time.Second):
		}
	})
	mustExport(t, a)
	if err := <-bDone; err != nil {
		t.Fatalf("the second export: %v", err)
	}
	gen, exported := metaInt(t, o, "generation"), metaInt(t, o, "exported_generation")
	if exported != gen {
		t.Errorf("exported_generation %d, generation %d: the later export must be the last written", exported, gen)
	}
	sameLedger(t, "ledger.json at exported_generation", loadExport(t, o), folded(t, o))
}

// TestAnExportThatCannotLockSkips: an export that waits out the lock
// writes nothing and leaves exported_generation as it was.
func TestAnExportThatCannotLockSkips(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)))
	mustFold(t, w, testParams)
	setSeam(t, &exportLockWait, 50*time.Millisecond)
	l, _, err := state.Acquire(filepath.Join(o.Dirs.LocksDir(), "feedback-export.lock"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Export(); err != nil {
		t.Fatalf("an export behind a held lock: %v, want a skip", err)
	}
	absent(t, filepath.Join(feedbackDir(o), "ledger.json"))
	if _, ok := metaOf(t, o)["exported_generation"]; ok {
		t.Error("exported_generation was written without an export")
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	mustExport(t, w)
	sameLedger(t, "the export once the lock is free", loadExport(t, o), folded(t, o))
}

// A journal that ends in a later month than now's (a line dated in the
// future) is exported without journal_at, as v0.1.32 wrote it: a hide
// journaled now, in now's month, is then seen by Current.
func TestExportOmitsALaterMonthsJournalAt(t *testing.T) {
	o := tempOpts(t) // now: 2026-11-08
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	december := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)), showsOf(december, "m1"))
	mustFold(t, w, testParams)
	mustExport(t, w)
	if got := loadExport(t, o).JournalAt; got != (feedback.JournalPos{}) {
		t.Errorf("journal_at %+v, want none: the journal ends in %s", got, feedback.MonthFile(december))
	}
	appendLines(t, o, hideOf(clock, "m1"))
	cur, err := feedback.Current(feedbackDir(o))
	if err != nil {
		t.Fatal(err)
	}
	if cur.Memories["m1"].Hidden == nil {
		t.Error("Current misses a hide journaled now, in now's month")
	}
}
