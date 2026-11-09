package ledgerdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/sqlitedb"
	"github.com/Barben360/backsight/internal/state"
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
	for _, tc := range []struct{ name, ledgerJSON string }{
		{"garbage ledger.json", `{"schema":1,"memories":{"m9":{"shown":40}`},
		{"a ledger.json that differs", `{"schema":1,"memories":{"m9":{"shown":40},"m1":{"shown":7,"wrong":3}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tempOpts(t)
			appendLines(t, o,
				settleOf(oct(10), show("k1", "m1", oct(9), feedback.OutcomeCredit), show("k2", "m2", oct(9), feedback.OutcomeMiss)),
				importOf(nov(1), "m3", oct(0), oct(100)),
				feedback.Event{Kind: feedback.KindImport, TS: nov(1).Add(time.Second), Outcome: feedback.ImportComplete},
				hideOf(nov(2), "m2"),
				settleOf(nov(3), show("k3", "m1", nov(2), feedback.OutcomeShown)))
			writeFile(t, feedbackDir(o)+"/ledger.json", []byte(tc.ledgerJSON))

			w := mustOpenWriter(t, o)
			defer mustCloseWriter(t, w)
			matchesOracle(t, "first build", o, feedback.Params{})
			got := folded(t, o)
			if _, ok := got.Memories["m9"]; ok {
				t.Error("the first build read ledger.json")
			}
			if e := got.Memories["m1"]; e.Shown != 2 || e.Wrong != 0 {
				t.Errorf("m1 %+v: want the journal's two shows, nothing of ledger.json", e)
			}
			if e := got.Memories["m3"]; e.Imported.Shown != 2 || got.HistoryImported.IsZero() {
				t.Errorf("m3 %+v, history imported %v: the first build keeps the imports", e, got.HistoryImported)
			}
			m := metaOf(t, o)
			if _, cut := m["imports_dropped_before_ns"]; m["generation"] != "1" || cut || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(m["build_id"]) {
				t.Errorf("meta after the first build: %v", m)
			}
			if c, ends := cursorsOf(t, o), journalEnds(t, o); !maps.Equal(c, ends) {
				t.Errorf("cursors %v, want ReadAll's ends %v", c, ends)
			}
			if res := mustFold(t, w, feedback.Params{}); res.Lines != 0 || res.Refolded {
				t.Errorf("a fold after the first build: %+v, want nothing to do", res)
			}
		})
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
	if m := metaOf(t, o); m["generation"] != "1" || len(m["build_id"]) != 16 || m["imports_dropped_before_ns"] != "" {
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
	cut := strconv.FormatInt(clock.UnixNano(), 10) // the rebuild's now
	// later: whether the import journaled after the rebuild is folded.
	noImports := func(what string, later bool) {
		t.Helper()
		got := folded(t, o)
		if e, ok := got.Memories["m2"]; ok != later || later && e.Imported.Shown != 1 {
			t.Errorf("%s: m2 %+v (%v), want only an import journaled after the rebuild", what, e, ok)
		}
		if e := got.Memories["m1"]; e.Imported != (feedback.Imported{}) || e.Shown == 0 {
			t.Errorf("%s: m1 %+v, want its settles only", what, e)
		}
		if m := metaOf(t, o); m["imports_dropped_before_ns"] != cut {
			t.Errorf("%s: imports_dropped_before_ns %q, want %s", what, m["imports_dropped_before_ns"], cut)
		}
		matchesOracle(t, what, o, testParams)
	}
	noImports("rebuilt without imports", false)
	appendLines(t, o, hideOf(nov(0), "m1"))
	if res := mustFold(t, w, testParams); !res.Refolded {
		t.Fatalf("a back-dated hide: %+v, want a refold", res)
	}
	noImports("refolded", false)
	// An import journaled after the rebuild applies, as in v0.1.32.
	appendLines(t, o, settleOf(nov(5), show("k5", "m1", nov(4), feedback.OutcomeShown)), importOf(clock.Add(time.Hour), "m2", oct(2)))
	if res := mustFold(t, w, testParams); res.Refolded || res.Lines != 2 {
		t.Fatalf("a fold of later lines: %+v, want 2 lines and no refold", res)
	}
	noImports("folded", true)
	appendLines(t, o, hideOf(nov(0).Add(time.Minute), "m1"))
	if res := mustFold(t, w, testParams); !res.Refolded {
		t.Fatalf("a back-dated hide: %+v, want a refold", res)
	}
	noImports("refolded again", true)
	if l := readerAll(t, o); l.Memories["m1"].Imported != (feedback.Imported{}) {
		t.Errorf("the reader: m1 %+v, want no import", l.Memories["m1"])
	}

	if _, err := w.Rebuild(ctx, testParams, true); err != nil {
		t.Fatal(err)
	}
	got, m := folded(t, o), metaOf(t, o)
	if _, set := m["imports_dropped_before_ns"]; got.Memories["m2"].Imported.Shown != 2 || got.Memories["m1"].Imported.Shown != 1 || set {
		t.Errorf("rebuilt with imports: %+v, meta %v", got.Memories, m)
	}
	matchesOracle(t, "rebuilt with imports", o, testParams)
}

// TestTheTailLeavesDroppedImportsOut: an import a rebuild left out that
// is still past the cursors (journaled during the rebuild) stays out of
// what the reader shows.
func TestTheTailLeavesDroppedImportsOut(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)))
	setSeam(t, &beforeFoldCommit, func() error {
		beforeFoldCommit = nil
		appendLines(t, o, importOf(nov(3), "m2", oct(0))) // read past by the rebuild
		return nil
	})
	if _, err := w.Rebuild(context.Background(), testParams, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := readerAll(t, o).Memories["m2"]; ok {
		t.Error("the reader applies an import of the tail the rebuild left out")
	}
	appendLines(t, o, importOf(clock.Add(time.Hour), "m3", oct(0)))
	if e := readerAll(t, o).Memories["m3"]; e.Imported.Shown != 1 {
		t.Errorf("the reader: m3 %+v, want the import journaled after the rebuild", e)
	}
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
	var root int64
	if err := w.db.QueryRow(`SELECT rootpage FROM sqlite_schema WHERE name = ?`, table).Scan(&root); err != nil {
		t.Fatal(err)
	}
	corruptPageNo(t, w, root)
}

// corruptPageNo closes w, the last connection (the -wal is folded in and
// removed), and writes garbage over page n, past the header.
func corruptPageNo(t *testing.T, w *Writer, n int64) {
	t.Helper()
	var size int64
	if err := w.db.QueryRow(`PRAGMA page_size`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("page %d holds the header", n)
	}
	mustCloseWriter(t, w)
	path := w.path
	b := readBytes(t, path)
	copy(b[(n-1)*size:n*size], bytes.Repeat([]byte{0xff}, int(size)))
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
	// The import makes the fold after the set-aside refold (the fresh
	// build folded under no params): the lines must count once.
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)), hideOf(nov(3), "m1"), importOf(nov(4), "m2", oct(0)))
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
	if !res.Refolded || res.Reason != "set_aside" || res.Lines != 3 {
		t.Errorf("the fold that set the file aside: %+v, want a set_aside refold of the journal's 3 lines", res)
	}
	if !bytes.Contains(readBytes(t, main+".corrupt"), bytes.Repeat([]byte{0xff}, 4096)) {
		t.Error("ledger.db.corrupt does not hold the corrupt file")
	}
	matchesOracle(t, "rebuilt after the set-aside", o, testParams)
	if e := folded(t, o).Memories["m1"]; e.Shown != 1 || e.Hidden == nil {
		t.Errorf("m1 %+v after the rebuild (fold %+v)", e, res)
	}
}

// TestSetAsideLeavesAnotherProcessesWork: a file another process set
// aside between the day check and the lock stays as that process left it.
func TestSetAsideLeavesAnotherProcessesWork(t *testing.T) {
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
	// Another process sets the file aside between this one's day check and
	// its lock: its work stands, forced or not. A forced call yields only to
	// a stamp newer than the one it saw before the lock.
	for _, c := range []struct {
		name   string
		forced bool
		older  bool // a .corrupt from before the lock
	}{
		{"set aside meanwhile", false, false},
		{"set aside meanwhile over an older one", false, true},
		{"forced, set aside meanwhile", true, false},
		{"forced, set aside meanwhile over an older one", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := tempOpts(t)
			o.ForceSetAside = c.forced
			main := mainPath(o)
			garbage := bytes.Repeat([]byte("garbage "), 100)
			writeFile(t, main, garbage)
			if c.older {
				writeFile(t, main+".corrupt", []byte("from two days ago"))
				if err := os.Chtimes(main+".corrupt", clock.Add(-48*time.Hour), clock.Add(-48*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
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
}

// TestSetAsideKeepsTheImportsCut: the fresh file built after a set-aside
// keeps leaving out the imports a rebuild dropped, read from the corrupt
// file's meta.
func TestSetAsideKeepsTheImportsCut(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)), importOf(nov(3), "m2", oct(0)))
	if _, err := w.Rebuild(context.Background(), testParams, false); err != nil {
		t.Fatal(err)
	}
	corruptPage(t, w, "ledger_cursors")
	w = mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	if res := mustFold(t, w, testParams); res.Reason != "set_aside" {
		t.Fatalf("fold over a corrupt page: %+v, want a set-aside", res)
	}
	if _, err := os.Stat(mainPath(o) + ".corrupt"); err != nil {
		t.Fatalf("not set aside: %v", err)
	}
	if _, ok := folded(t, o).Memories["m2"]; ok {
		t.Error("the fresh build brought back an import the rebuild left out")
	}
	if got := metaOf(t, o)["imports_dropped_before_ns"]; got != strconv.FormatInt(clock.UnixNano(), 10) {
		t.Errorf("imports_dropped_before_ns %q after the set-aside, want the rebuild's", got)
	}
	matchesOracle(t, "after the set-aside", o, testParams)
}

// TestExportSetsAsideACorruptPage: a page no fold reads (rows outside its
// batch) is met by the export's full scan, which sets the file aside and
// exports the rebuilt one.
func TestExportSetsAsideACorruptPage(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	var lines []feedback.Event
	for i := range 600 {
		lines = append(lines, settleOf(nov(1).Add(time.Duration(i)*time.Second), show(fmt.Sprintf("k%d", i), fmt.Sprintf("m%04d", i), nov(0), feedback.OutcomeShown)))
	}
	appendLines(t, o, lines...)
	mustFold(t, w, testParams)
	var last int64 // the leaf holding the largest ids
	if err := w.db.QueryRow(`SELECT pageno FROM dbstat WHERE name = 'memories' AND pagetype = 'leaf' ORDER BY path DESC LIMIT 1`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	var leaves int
	if err := w.db.QueryRow(`SELECT count(*) FROM dbstat WHERE name = 'memories' AND pagetype = 'leaf'`).Scan(&leaves); err != nil || leaves < 3 {
		t.Fatalf("memories has %d leaves (%v): the case needs a page the fold does not read", leaves, err)
	}
	corruptPageNo(t, w, last)
	w = mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, hideOf(nov(5), "a-first"))
	if res := mustFold(t, w, testParams); res.Refolded || res.Lines != 1 {
		t.Fatalf("a fold of an id on another page: %+v, want 1 line and no refold", res)
	}
	mustExport(t, w)
	if _, err := os.Stat(mainPath(o) + ".corrupt"); err != nil {
		t.Fatalf("the export did not set the file aside: %v", err)
	}
	want := oracle(t, feedbackDir(o), testParams)
	sameLedger(t, "ledger.json after the set-aside", loadExport(t, o), want)
	matchesOracle(t, "ledger.db after the set-aside", o, testParams)
}

// TestABrokenWriterSaysSo: a set-aside whose fresh file cannot be opened
// leaves a writer that answers every call with why, never a closed
// connection's error.
func TestABrokenWriterSaysSo(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)))
	mustFold(t, w, testParams)
	corruptPage(t, w, "ledger_cursors")
	w = mustOpenWriter(t, o)
	// An update starts once the file is held: the fresh file waits for it.
	setSeam(t, &afterSeize, func() {
		if err := state.WriteUpdateMarker(o.Dirs, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
			t.Fatal(err)
		}
	})
	_, err := w.Fold(context.Background(), testParams)
	if !errors.Is(err, ErrUpdateInProgress) || !errors.Is(err, ErrUnreadable) {
		t.Fatalf("the fold whose fresh file cannot open: %v", err)
	}
	for name, call := range map[string]func() error{
		"Fold":    func() error { _, err := w.Fold(context.Background(), testParams); return err },
		"Rebuild": func() error { _, err := w.Rebuild(context.Background(), testParams, true); return err },
		"Export":  w.Export,
	} {
		if err := call(); !errors.Is(err, ErrUnreadable) || !errors.Is(err, ErrUpdateInProgress) {
			t.Errorf("%s on a broken writer: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close of a broken writer: %v", err)
	}
}

// TestABusyProbeNeverOpensReadWrite: a version probe that fails busy
// (not corrupt) beside a -wal is returned; the file is not opened
// read-write, which would set its journal mode.
func TestABusyProbeNeverOpensReadWrite(t *testing.T) {
	forceFS(t, sqlitedb.FSNetwork) // built in the rollback journal
	o := tempOpts(t)
	mustCloseWriter(t, mustOpenWriter(t, o))
	main := mainPath(o)
	writeFile(t, main+"-wal", nil)
	before := readBytes(t, main)
	forceFS(t, sqlitedb.FSLocal) // a writer would now set WAL
	fakeProbe(t, true)
	setSeam(t, &probeVersion, func(context.Context, string) (int, error) {
		return 0, fmt.Errorf("probe: %w", sqlitedb.ErrInUse)
	})
	if _, err := OpenWriter(context.Background(), o); sqlitedb.Code(err) != "busy" {
		t.Fatalf("OpenWriter after a busy probe: %v, want the busy error", err)
	}
	if !bytes.Equal(readBytes(t, main), before) {
		t.Error("ledger.db was opened read-write after a busy probe")
	}
}

// TestStoredCutLeavesNoCompanions: reading the imports cut of a file
// about to be set aside (and maybe refused) creates no -wal or -shm
// beside an idle WAL file.
func TestStoredCutLeavesNoCompanions(t *testing.T) {
	forceFS(t, sqlitedb.FSLocal)
	fakeProbe(t, true)
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	appendLines(t, o, importOf(nov(1), "m2", oct(0)))
	if _, err := w.Rebuild(context.Background(), testParams, false); err != nil {
		t.Fatal(err)
	}
	mustCloseWriter(t, w)
	main := mainPath(o)
	if !sqlitedb.HeaderIsWAL(main) {
		t.Fatal("ledger.db is not a WAL file: the case tests nothing")
	}
	absent(t, main+"-wal")
	absent(t, main+"-shm")
	if got := storedCut(context.Background(), main); got != clock.UnixNano() {
		t.Errorf("storedCut %d, want the rebuild's %d", got, clock.UnixNano())
	}
	absent(t, main+"-wal")
	absent(t, main+"-shm")
}
