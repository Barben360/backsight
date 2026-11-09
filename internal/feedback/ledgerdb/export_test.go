package ledgerdb

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/sqlitedb"
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
