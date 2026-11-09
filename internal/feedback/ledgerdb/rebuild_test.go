package ledgerdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"maps"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

// importOf is an import event of credited shows of id, recording the
// half-lives of testParams.
func importOf(ts time.Time, id string, at ...time.Time) feedback.Event {
	e := feedback.Event{Kind: feedback.KindImport, TS: ts, Source: "telemetry", Params: testParams.Record()}
	for i, a := range at {
		e.Settled = append(e.Settled, show("imp-"+id+"-"+a.Format(time.RFC3339)+"-"+string(rune('a'+i)), id, a, feedback.OutcomeCredit))
	}
	return e
}

func TestFirstBuildFoldsTheJournal(t *testing.T) {
	o := tempOpts(t)
	appendLines(t, o,
		settleOf(oct(10), show("k1", "m1", oct(9), feedback.OutcomeCredit), show("k2", "m2", oct(9), feedback.OutcomeMiss)),
		importOf(nov(1), "m3", oct(0), oct(100)),
		feedback.Event{Kind: feedback.KindImport, TS: nov(1).Add(time.Second), Outcome: feedback.ImportComplete},
		hideOf(nov(2), "m2"),
		settleOf(nov(3), show("k3", "m1", nov(2), feedback.OutcomeShown)))
	writeFile(t, feedbackDir(o)+"/ledger.json", []byte(`{"schema":1,"memories":{"m9":{"shown":40}}`))

	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	matchesOracle(t, "first build", o, feedback.Params{})
	got := folded(t, o)
	if _, ok := got.Memories["m9"]; ok {
		t.Error("the first build read ledger.json")
	}
	if e := got.Memories["m3"]; e.Imported.Shown != 2 || got.HistoryImported.IsZero() {
		t.Errorf("m3 %+v, history imported %v: the first build keeps the imports", e, got.HistoryImported)
	}
	m := metaOf(t, o)
	if m["generation"] != "1" || m["imports"] != "kept" || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(m["build_id"]) {
		t.Errorf("meta after the first build: %v", m)
	}
	if c, ends := cursorsOf(t, o), journalEnds(t, o); !maps.Equal(c, ends) {
		t.Errorf("cursors %v, want ReadAll's ends %v", c, ends)
	}
	if res := mustFold(t, w, feedback.Params{}); res.Lines != 0 || res.Refolded {
		t.Errorf("a fold after the first build: %+v, want nothing to do", res)
	}
}

// TestAFirstBuildThatDiesIsRedone: readers get ErrMissing until the first
// build commits; a fold builds a file whose build never committed.
func TestAFirstBuildThatDiesIsRedone(t *testing.T) {
	o := tempOpts(t)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)))
	crash := errors.New("crash before the commit")
	setSeam(t, &beforeFoldCommit, func() error { return crash })
	if _, err := OpenWriter(context.Background(), o); !errors.Is(err, crash) {
		t.Fatalf("OpenWriter with a crashing first build: %v", err)
	}
	_, err := OpenReader(o, 20*time.Millisecond)
	onlyErr(t, "a first build that died", err, ErrMissing)
	beforeFoldCommit = nil
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	if res := mustFold(t, w, testParams); res.Lines != 1 {
		t.Errorf("the fold that builds: %+v, want 1 line", res)
	}
	matchesOracle(t, "built by a fold", o, testParams)
	if m := metaOf(t, o); m["generation"] != "1" || len(m["build_id"]) != 16 || m["imports"] != "kept" {
		t.Errorf("meta after a fold's build: %v", m)
	}
}

func TestNoImportSurvivesARefold(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o,
		settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)),
		importOf(nov(3), "m2", oct(0)),
		importOf(nov(3), "m1", oct(1)))
	mustFold(t, w, testParams)
	ctx := context.Background()
	if _, err := w.Rebuild(ctx, testParams, false); err != nil {
		t.Fatal(err)
	}
	noImports := func(what string) {
		t.Helper()
		got := folded(t, o)
		if _, ok := got.Memories["m2"]; ok {
			t.Errorf("%s: m2, known by an import only, is back", what)
		}
		if e := got.Memories["m1"]; e.Imported != (feedback.Imported{}) || e.Shown == 0 {
			t.Errorf("%s: m1 %+v, want its settles only", what, e)
		}
		if m := metaOf(t, o); m["imports"] != "dropped" {
			t.Errorf("%s: imports %q, want dropped", what, m["imports"])
		}
		matchesOracle(t, what, o, testParams)
	}
	noImports("rebuilt without imports")
	appendLines(t, o, hideOf(nov(0), "m1"))
	if res := mustFold(t, w, testParams); !res.Refolded {
		t.Fatalf("a back-dated hide: %+v, want a refold", res)
	}
	noImports("refolded")
	// A later import folds as the rebuild would: left out.
	appendLines(t, o, settleOf(nov(5), show("k5", "m1", nov(4), feedback.OutcomeShown)), importOf(nov(6), "m2", oct(2)))
	if res := mustFold(t, w, testParams); res.Refolded || res.Lines != 2 {
		t.Fatalf("a fold of later lines: %+v, want 2 lines and no refold", res)
	}
	noImports("folded")

	if _, err := w.Rebuild(ctx, testParams, true); err != nil {
		t.Fatal(err)
	}
	got := folded(t, o)
	if got.Memories["m2"].Imported.Shown != 2 || got.Memories["m1"].Imported.Shown != 1 || metaOf(t, o)["imports"] != "kept" {
		t.Errorf("rebuilt with imports: %+v, imports %q", got.Memories, metaOf(t, o)["imports"])
	}
	matchesOracle(t, "rebuilt with imports", o, testParams)
}

func TestRebuildIsANewBuild(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)), retireOf(nov(3), "m1"))
	mustFold(t, w, testParams)
	before := metaOf(t, o)
	l, err := w.Rebuild(context.Background(), testParams, true)
	if err != nil {
		t.Fatal(err)
	}
	after := metaOf(t, o)
	if after["build_id"] == before["build_id"] || after["generation"] == before["generation"] {
		t.Errorf("meta before %v, after %v: a rebuild is a new build and a new generation", before, after)
	}
	sameLedger(t, "Rebuild's ledger", l, oracle(t, feedbackDir(o), testParams))
	if e := l.Memories["m1"]; e.Wrong != 1 {
		t.Errorf("m1 %+v after a rebuild: retired once", e)
	}
}

// corruptPage writes garbage over the page holding table's root, past
// the header: the file opens, the first read of table fails.
func corruptPage(t *testing.T, w *Writer, table string) {
	t.Helper()
	var root, size int64
	if err := w.db.QueryRow(`SELECT rootpage FROM sqlite_schema WHERE name = ?`, table).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := w.db.QueryRow(`PRAGMA page_size`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if root < 2 {
		t.Fatalf("%s's root is page %d", table, root)
	}
	mustCloseWriter(t, w) // the last connection: the -wal is folded in and removed
	path := w.path
	b := readBytes(t, path)
	copy(b[(root-1)*size:root*size], bytes.Repeat([]byte{0xff}, int(size)))
	writeFile(t, path, b)
	if v := int(binary.BigEndian.Uint32(b[60:64])); v != 1 {
		t.Fatalf("header user_version %d after the change", v)
	}
}

func TestPageCorruptionHeldOpenIsSetAsideAfterSeize(t *testing.T) {
	forceFS(t, sqlitedb.FSLocal)
	fakeProbe(t, true) // WAL: an open connection keeps Seize out
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)), hideOf(nov(3), "m1"))
	mustFold(t, w, testParams)
	corruptPage(t, w, "ledger_cursors")
	main := mainPath(o)
	garbage := readBytes(t, main)

	holder, err := sql.Open("sqlite", sqlitedb.ReaderDSN(main, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	var v int
	if err := holder.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}

	w = mustOpenWriter(t, o) // the header reads fine
	defer mustCloseWriter(t, w)
	_, err = w.Fold(context.Background(), testParams)
	var ce *corruptErr
	if !errors.Is(err, ErrBusy) || !errors.As(err, &ce) {
		t.Fatalf("a fold over a corrupt page, the file held: %v, want the corruption and ErrBusy", err)
	}
	absent(t, main+".corrupt")
	if !bytes.Equal(readBytes(t, main), garbage) {
		t.Error("the held file was changed")
	}

	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := w.Fold(context.Background(), testParams)
	if err != nil {
		t.Fatalf("the fold after the holder closed: %v", err)
	}
	if !bytes.Contains(readBytes(t, main+".corrupt"), bytes.Repeat([]byte{0xff}, 4096)) {
		t.Error("ledger.db.corrupt does not hold the corrupt file")
	}
	matchesOracle(t, "rebuilt after the set-aside", o, testParams)
	if e := folded(t, o).Memories["m1"]; e.Shown != 1 || e.Hidden == nil {
		t.Errorf("m1 %+v after the rebuild (fold %+v)", e, res)
	}
}

// TestSetAsideLeavesAnotherProcesssWork: a file another process set
// aside between the day check and the lock stays as that process left it.
func TestSetAsideLeavesAnotherProcesssWork(t *testing.T) {
	t.Run("the file is gone", func(t *testing.T) {
		o := tempOpts(t)
		main := mainPath(o)
		old := clock.Add(-48 * time.Hour)
		writeFile(t, main+".corrupt", []byte("an older set-aside"))
		if err := os.Chtimes(main+".corrupt", old, old); err != nil {
			t.Fatal(err)
		}
		if err := setAside(context.Background(), o, &corruptErr{path: main, err: errors.New("corrupt")}); err != nil {
			t.Fatalf("setAside of a file gone: %v", err)
		}
		if fi, err := os.Stat(main + ".corrupt"); err != nil || !fi.ModTime().Equal(old) {
			t.Errorf("the older ledger.db.corrupt was stamped again (%v, %v)", fi.ModTime(), err)
		}
	})
	t.Run("set aside meanwhile", func(t *testing.T) {
		o := tempOpts(t)
		main := mainPath(o)
		garbage := bytes.Repeat([]byte("garbage "), 100)
		writeFile(t, main, garbage)
		setSeam(t, &afterSeize, func() {
			writeFile(t, main+".corrupt", []byte("the other process's"))
			if err := os.Chtimes(main+".corrupt", clock, clock); err != nil {
				t.Fatal(err)
			}
		})
		if err := setAside(context.Background(), o, &corruptErr{path: main, err: errors.New("corrupt")}); err != nil {
			t.Fatalf("setAside after another process did it: %v", err)
		}
		if !bytes.Equal(readBytes(t, main), garbage) || string(readBytes(t, main+".corrupt")) != "the other process's" {
			t.Error("the file was set aside a second time")
		}
	})
}
