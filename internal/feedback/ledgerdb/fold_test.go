package ledgerdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

const day = 24 * time.Hour

// testParams fold the tests' journals; their settle events record them.
var testParams = feedback.Params{ConfidentRank: 3, MissHalfLife: 30 * day, CreditHalfLife: 60 * day, TurnTimeout: 30 * time.Minute}

// nov is h hours into November 2026, the tests' current month.
func nov(h int) time.Time {
	return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour)
}

// oct is h hours into October 2026.
func oct(h int) time.Time {
	return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour)
}

func feedbackDir(o Options) string { return o.Dirs.FeedbackDir() }

// appendLines journals events through feedback.Journal.Append, each at
// its own explicit ts (so in that ts's month file).
func appendLines(t *testing.T, o Options, events ...feedback.Event) {
	t.Helper()
	j := feedback.NewJournal(feedbackDir(o), feedback.Common{Session: "s1", Binary: "v-test"})
	for _, e := range events {
		if e.TS.IsZero() {
			t.Fatal("a test line needs an explicit ts")
		}
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

func hideOf(ts time.Time, ids ...string) feedback.Event {
	return feedback.Event{Kind: feedback.KindHide, TS: ts, IDs: ids, Verdict: "wrong", Note: "doc-1"}
}

func unhideOf(ts time.Time, ids ...string) feedback.Event {
	return feedback.Event{Kind: feedback.KindUnhide, TS: ts, IDs: ids}
}

func retireOf(ts time.Time, ids ...string) feedback.Event {
	return feedback.Event{Kind: feedback.KindRetire, TS: ts, IDs: ids, Verdict: "wrong"}
}

func showsOf(ts time.Time, ids ...string) feedback.Event {
	e := feedback.Event{Kind: "shows", TS: ts, Path: "prompt"}
	for i, id := range ids {
		e.Items = append(e.Items, feedback.Item{ID: id, Rank: i + 1})
	}
	return e
}

func settleOf(ts time.Time, s ...feedback.Settled) feedback.Event {
	return feedback.Event{Kind: "settle", TS: ts, Settled: s, Params: testParams.Record()}
}

func show(key, id string, at time.Time, outcome string) feedback.Settled {
	return feedback.Settled{Key: key, ID: id, At: at, Outcome: outcome}
}

func mustFold(t *testing.T, w *Writer, p feedback.Params) FoldResult {
	t.Helper()
	res, err := w.Fold(context.Background(), p)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	return res
}

// oracle is the ledger a rebuild gives: feedback.Fold over ReadAll's
// lines (imports dropped when ledger.db's meta says so), its watermark
// the latest applied line's ts. Task 5's tests use it too.
func oracle(t *testing.T, dir string, p feedback.Params) feedback.Ledger {
	t.Helper()
	events, _, _, err := feedback.ReadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	kept := events
	if storedImports(t, dir) == importsDropped {
		kept = feedback.DropImports(events)
	}
	l := feedback.Fold(kept, p)
	l.Watermark = time.Time{}
	for _, e := range events {
		if feedback.Applied(e.Kind) && e.TS.After(l.Watermark) {
			l.Watermark = e.TS
		}
	}
	return l
}

// storedImports is meta.imports of dir's ledger.db ("" without one).
func storedImports(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, FileName)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return metaAt(t, path)["imports"]
}

// roQuery runs fn over a read-only connection to path, closed after.
func roQuery(t *testing.T, path string, fn func(db *sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", sqlitedb.ReaderDSN(path, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	fn(db)
}

func metaAt(t *testing.T, path string) map[string]string {
	t.Helper()
	m := map[string]string{}
	roQuery(t, path, func(db *sql.DB) {
		rows, err := db.Query(`SELECT key, value FROM meta`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			m[k] = v
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
	return m
}

func metaOf(t *testing.T, o Options) map[string]string { t.Helper(); return metaAt(t, mainPath(o)) }

func metaInt(t *testing.T, o Options, key string) int64 {
	t.Helper()
	s, ok := metaOf(t, o)[key]
	if !ok {
		t.Fatalf("meta has no %s", key)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func cursorsOf(t *testing.T, o Options) map[string]int64 {
	t.Helper()
	c := map[string]int64{}
	roQuery(t, mainPath(o), func(db *sql.DB) {
		rows, err := db.Query(`SELECT file, offset FROM ledger_cursors`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var f string
			var off int64
			if err := rows.Scan(&f, &off); err != nil {
				t.Fatal(err)
			}
			c[f] = off
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
	return c
}

// folded is o's ledger.db as the folds left it: its rows, watermark_ns
// and history_imported_ns, without the journal tail the reader applies.
func folded(t *testing.T, o Options) feedback.Ledger {
	t.Helper()
	l := feedback.Ledger{Schema: feedback.LedgerSchema, Memories: map[string]feedback.Entry{}}
	roQuery(t, mainPath(o), func(db *sql.DB) {
		rows, err := db.Query(`SELECT ` + memoryColumns + ` FROM memories`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var rw row
			if err := rows.Scan(rw.dest()...); err != nil {
				t.Fatal(err)
			}
			id, e := entryFromRow(rw)
			l.Memories[id] = e
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
	m := metaOf(t, o)
	ns := func(key string) time.Time {
		n, err := strconv.ParseInt(m[key], 10, 64)
		if err != nil {
			t.Fatalf("meta %s = %q: %v", key, m[key], err)
		}
		return fromNS(n)
	}
	l.Watermark, l.HistoryImported = ns("watermark_ns"), ns("history_imported_ns")
	return l
}

// readerAll is All() through a fresh reader (rows and the journal tail).
func readerAll(t *testing.T, o Options) feedback.Ledger {
	t.Helper()
	r, err := OpenReader(o, time.Second)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	l, err := r.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	return l
}

// sameLedger compares two ledgers field by field: the same memory ids,
// each entry as sameEntry does, the watermark and the history import
// time with Equal.
func sameLedger(t *testing.T, what string, got, want feedback.Ledger) {
	t.Helper()
	g, w := slices.Sorted(maps.Keys(got.Memories)), slices.Sorted(maps.Keys(want.Memories))
	if !slices.Equal(g, w) {
		t.Errorf("%s: memories %v, want %v", what, g, w)
	}
	for id, we := range want.Memories {
		if ge, ok := got.Memories[id]; ok {
			sameEntry(t, what+": "+id, ge, we)
		}
	}
	if !got.Watermark.Equal(want.Watermark) {
		t.Errorf("%s: watermark %v, want %v", what, got.Watermark, want.Watermark)
	}
	if !got.HistoryImported.Equal(want.HistoryImported) {
		t.Errorf("%s: history imported %v, want %v", what, got.HistoryImported, want.HistoryImported)
	}
}

// matchesOracle: ledger.db's rows equal a rebuild of the journal under p.
func matchesOracle(t *testing.T, what string, o Options, p feedback.Params) {
	t.Helper()
	sameLedger(t, what, folded(t, o), oracle(t, feedbackDir(o), p))
}

// journalEnds are ReadAll's ends over o's journal.
func journalEnds(t *testing.T, o Options) map[string]int64 {
	t.Helper()
	_, ends, _, err := feedback.ReadAll(feedbackDir(o))
	if err != nil {
		t.Fatal(err)
	}
	return ends
}

// setSeam sets *seam to v for the test.
func setSeam[T any](t *testing.T, seam *T, v T) {
	t.Helper()
	old := *seam
	*seam = v
	t.Cleanup(func() { *seam = old })
}

func TestFoldTwiceAppliesNothingTwice(t *testing.T) {
	o := tempOpts(t)
	tick := clock
	o.Now = func() time.Time { tick = tick.Add(time.Second); return tick }
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o,
		showsOf(nov(1), "m1"), showsOf(nov(2), "m2"), showsOf(nov(3), "m3"),
		settleOf(nov(4), show("k1", "m1", nov(1), feedback.OutcomeCredit), show("k2", "m2", nov(2), feedback.OutcomeMiss), show("k3", "m3", nov(3), feedback.OutcomeShown)))
	if res := mustFold(t, w, testParams); res.Lines != 4 || res.Refolded {
		t.Errorf("first fold: %+v, want 4 lines, no refold", res)
	}
	first, m1 := folded(t, o), metaOf(t, o)
	matchesOracle(t, "first fold", o, testParams)

	res := mustFold(t, w, testParams)
	if res.Lines != 0 || res.Refolded {
		t.Errorf("second fold: %+v, want 0 lines, no refold", res)
	}
	sameLedger(t, "second fold", folded(t, o), first)
	m2 := metaOf(t, o)
	for _, k := range []string{"generation", "watermark_ns"} {
		if m1[k] != m2[k] {
			t.Errorf("%s moved: %s then %s", k, m1[k], m2[k])
		}
	}
	if m1["folded_at_ns"] == m2["folded_at_ns"] {
		t.Errorf("folded_at_ns did not move: %s", m2["folded_at_ns"])
	}
	if e := first.Memories["m1"]; e.Shown != 1 || e.Credits != 1 {
		t.Errorf("m1: %+v, want shown 1, credits 1", e)
	}
}

func TestFoldIsOneTransaction(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)))
	mustFold(t, w, testParams)
	before, cursors, gen := folded(t, o), cursorsOf(t, o), metaInt(t, o, "generation")

	appendLines(t, o,
		settleOf(nov(4), show("k2", "m2", nov(3), feedback.OutcomeShown)),
		retireOf(nov(5), "m1"), hideOf(nov(6), "m1"))
	crash := errors.New("crash before the commit")
	setSeam(t, &beforeFoldCommit, func() error { return crash })
	if _, err := w.Fold(context.Background(), testParams); !errors.Is(err, crash) {
		t.Fatalf("Fold with a crash before its commit: %v", err)
	}
	sameLedger(t, "after the crash", folded(t, o), before)
	if got := cursorsOf(t, o); !maps.Equal(got, cursors) {
		t.Errorf("cursors %v after the crash, want %v", got, cursors)
	}
	if got := metaInt(t, o, "generation"); got != gen {
		t.Errorf("generation %d after the crash, want %d", got, gen)
	}

	beforeFoldCommit = nil
	if res := mustFold(t, w, testParams); res.Lines != 3 {
		t.Errorf("the fold after the crash read %d lines, want 3", res.Lines)
	}
	matchesOracle(t, "after the crash", o, testParams)
	if e := folded(t, o).Memories["m1"]; e.Wrong != 1 || e.Hidden == nil {
		t.Errorf("m1 %+v: want retired once and hidden", e)
	}
}

func TestConcurrentFoldsApplyOnce(t *testing.T) {
	o := tempOpts(t)
	writers := make([]*Writer, 8)
	for i := range writers {
		writers[i] = mustOpenWriter(t, o)
		defer mustCloseWriter(t, writers[i])
	}
	// 500 lines over October and November: 400 settles (show i of m0 when
	// i%8 < 4, of m1 when 4..6, of m2 when 7), 100 shows.
	var lines []feedback.Event
	for n := range 500 {
		ts := oct(0).Add(time.Duration(n) * 61 * day / 500)
		if n%5 == 4 {
			lines = append(lines, showsOf(ts, "m0", "m1"))
			continue
		}
		i := len(lines) - n/5 // the settle's number
		id := "m0"
		switch {
		case i%8 == 7:
			id = "m2"
		case i%8 >= 4:
			id = "m1"
		}
		outcome := feedback.OutcomeShown
		if i%3 == 0 {
			outcome = feedback.OutcomeCredit
		}
		lines = append(lines, settleOf(ts, show(fmt.Sprintf("k%d", i), id, ts.Add(-time.Hour), outcome)))
	}
	start, done := make(chan struct{}), make(chan struct{})
	errs := make(chan error, len(writers))
	var wg sync.WaitGroup
	for _, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-done:
					return
				default:
				}
				if _, err := w.Fold(context.Background(), testParams); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	appendLines(t, o, lines...)
	close(done)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent fold: %v", err)
	}
	mustFold(t, writers[0], testParams)

	matchesOracle(t, "after 8 concurrent folders", o, testParams)
	got := folded(t, o)
	for id, want := range map[string]int{"m0": 200, "m1": 150, "m2": 50} {
		if got.Memories[id].Shown != want {
			t.Errorf("%s shown %d, want %d", id, got.Memories[id].Shown, want)
		}
	}
	sizes := map[string]int64{}
	for _, name := range []string{feedback.MonthFile(oct(0)), feedback.MonthFile(nov(0))} {
		fi, err := os.Stat(filepath.Join(feedbackDir(o), name))
		if err != nil {
			t.Fatalf("the 500 lines span two month files: %v", err)
		}
		sizes[name] = fi.Size()
	}
	if c := cursorsOf(t, o); !maps.Equal(c, sizes) {
		t.Errorf("cursors %v, want the files' sizes %v", c, sizes)
	}
}

func TestFoldDuringAWrite(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeShown)))
	once := sync.Once{}
	// A retire beside the hide: applied twice, it would count 2.
	setSeam(t, &afterFoldRead, func() { once.Do(func() { appendLines(t, o, hideOf(nov(3), "m1"), retireOf(nov(3), "m1")) }) })
	mustFold(t, w, testParams)
	mustFold(t, w, testParams)
	matchesOracle(t, "after a write during a fold", o, testParams)
	if e := folded(t, o).Memories["m1"]; e.Hidden == nil || e.Wrong != 1 {
		t.Errorf("m1 %+v: want hidden and retired exactly once", e)
	}
	if c, ends := cursorsOf(t, o), journalEnds(t, o); !maps.Equal(c, ends) {
		t.Errorf("cursors %v, want %v", c, ends)
	}
}

func TestBackDatedLineFolds(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o,
		settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)),
		hideOf(nov(3), "m2"))
	mustFold(t, w, testParams)
	appendLines(t, o, hideOf(oct(100), "m1"))
	if _, err := os.Stat(filepath.Join(feedbackDir(o), "journal-2026-10.jsonl")); err != nil {
		t.Fatalf("the back-dated hide is not in October's file: %v", err)
	}
	res := mustFold(t, w, testParams)
	if !res.Refolded || res.Reason != "late_line" {
		t.Errorf("fold of a back-dated hide: %+v, want a late_line refold", res)
	}
	matchesOracle(t, "after a back-dated hide", o, testParams)
	if h := folded(t, o).Memories["m1"].Hidden; h == nil || !h.Since.Equal(oct(100)) {
		t.Errorf("m1 hidden %+v, want since %v", h, oct(100))
	}
}

func TestHideUnhideOneBatchBothOrders(t *testing.T) {
	t1, t2 := nov(1), nov(2)
	for _, tc := range []struct {
		name   string
		lines  []feedback.Event
		hidden bool // hidden since t2, else visible and unhidden at t2
	}{
		{"hide then unhide, in order", []feedback.Event{hideOf(t1, "m1"), unhideOf(t2, "m1")}, false},
		{"hide then unhide, reversed", []feedback.Event{unhideOf(t2, "m1"), hideOf(t1, "m1")}, false},
		{"unhide then hide, in order", []feedback.Event{unhideOf(t1, "m1"), hideOf(t2, "m1")}, true},
		{"unhide then hide, reversed", []feedback.Event{hideOf(t2, "m1"), unhideOf(t1, "m1")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tempOpts(t)
			w := mustOpenWriter(t, o)
			defer mustCloseWriter(t, w)
			appendLines(t, o, tc.lines...)
			if res := mustFold(t, w, testParams); res.Lines != 2 || res.Refolded {
				t.Errorf("one batch: %+v", res)
			}
			e := folded(t, o).Memories["m1"]
			switch {
			case tc.hidden && (e.Hidden == nil || !e.Hidden.Since.Equal(t2)):
				t.Errorf("m1 %+v: want hidden since %v", e, t2)
			case !tc.hidden && (e.Hidden != nil || !e.UnhiddenAt.Equal(t2)):
				t.Errorf("m1 %+v: want visible, unhidden at %v", e, t2)
			}
			matchesOracle(t, tc.name, o, testParams)
		})
	}
}

func TestLateLineRefoldsToTheRebuild(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, hideOf(nov(5), "m1"))
	mustFold(t, w, testParams)
	appendLines(t, o, unhideOf(nov(4), "m1")) // a clock stepped back
	res := mustFold(t, w, testParams)
	if !res.Refolded || res.Reason != "late_line" {
		t.Errorf("fold of an earlier unhide: %+v, want a late_line refold", res)
	}
	if e := folded(t, o).Memories["m1"]; e.Hidden == nil {
		t.Errorf("m1 %+v: the later hide must win", e)
	}
	matchesOracle(t, "after an earlier unhide", o, testParams)
}

func TestEqualTsFromAnotherProcessRefolds(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, hideOf(nov(5), "m1"))
	mustFold(t, w, testParams)
	if got := metaInt(t, o, "watermark_ns"); got != nov(5).UnixNano() {
		t.Fatalf("watermark_ns %d, want %d", got, nov(5).UnixNano())
	}
	appendLines(t, o, retireOf(nov(5), "m2")) // another process, its own seq, the same ts
	res := mustFold(t, w, testParams)
	if !res.Refolded || res.Reason != "late_line" {
		t.Errorf("fold of a line at the watermark: %+v, want a late_line refold", res)
	}
	matchesOracle(t, "after a line at the watermark", o, testParams)
}

func TestShowsNeverRefold(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, hideOf(nov(5), "m1"))
	mustFold(t, w, testParams)
	appendLines(t, o, showsOf(nov(1), "m1"), feedback.Event{Kind: "turn", TS: nov(2)})
	if res := mustFold(t, w, testParams); res.Refolded || res.Lines != 2 {
		t.Errorf("fold of back-dated shows and turn lines: %+v, want 2 lines and no refold", res)
	}
	matchesOracle(t, "after back-dated shows", o, testParams)
}

// encodeLine is e as a journal line, without its newline.
func encodeLine(t *testing.T, e feedback.Event) []byte {
	t.Helper()
	e.Schema = feedback.Schema
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPartialLineFoldsOnceComplete(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeShown)))
	file := filepath.Join(feedbackDir(o), feedback.MonthFile(nov(0)))
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	lineStart := fi.Size()
	ev := settleOf(nov(4), show("k2", "m2", nov(3), feedback.OutcomeCredit))
	ev.Seq = 99
	line := encodeLine(t, ev)
	appendBytes(t, file, line[:len(line)/2])

	if res := mustFold(t, w, testParams); res.Lines != 1 {
		t.Errorf("fold with a partial last line: %+v, want 1 line", res)
	}
	if c := cursorsOf(t, o)[filepath.Base(file)]; c != lineStart {
		t.Errorf("cursor %d, want %d, before the partial line", c, lineStart)
	}
	if _, ok := folded(t, o).Memories["m2"]; ok {
		t.Error("the partial line was applied")
	}
	matchesOracle(t, "with a partial last line", o, testParams)

	appendBytes(t, file, append(line[len(line)/2:], '\n'))
	if res := mustFold(t, w, testParams); res.Lines != 1 {
		t.Errorf("fold of the completed line: %+v, want 1 line", res)
	}
	mustFold(t, w, testParams)
	if e := folded(t, o).Memories["m2"]; e.Shown != 1 || e.Credits != 1 {
		t.Errorf("m2 %+v: want shown and credited once", e)
	}
	matchesOracle(t, "with the line completed", o, testParams)
}

func TestShrunkFileRefolds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, o Options, file string, cursor int64)
	}{
		{"truncated to half, then a line", func(t *testing.T, o Options, file string, cursor int64) {
			if err := os.Truncate(file, cursor/2); err != nil {
				t.Fatal(err)
			}
			appendLines(t, o, retireOf(nov(10), "m2"))
		}},
		{"removed", func(t *testing.T, _ Options, file string, _ int64) {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}},
		{"a byte before the cursor replaced", func(t *testing.T, _ Options, file string, cursor int64) {
			f, err := os.OpenFile(file, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte(" "), cursor-1); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tempOpts(t)
			w := mustOpenWriter(t, o)
			defer mustCloseWriter(t, w)
			appendLines(t, o,
				settleOf(oct(20), show("k0", "m0", oct(19), feedback.OutcomeCredit)),
				settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit), show("k2", "m2", nov(1), feedback.OutcomeShown)),
				hideOf(nov(3), "m1"),
				retireOf(nov(4), "m2"))
			mustFold(t, w, testParams)
			name := feedback.MonthFile(nov(0))
			file := filepath.Join(feedbackDir(o), name)
			cursor := cursorsOf(t, o)[name]
			tc.change(t, o, file, cursor)
			if b, err := os.ReadFile(file); err == nil && int64(len(b)) >= cursor && b[cursor-1] == '\n' {
				t.Fatal("the change left the file looking unchanged at its cursor: the case tests nothing")
			}
			res := mustFold(t, w, testParams)
			if !res.Refolded || res.Reason != "file_changed" {
				t.Errorf("fold after the change: %+v, want a file_changed refold", res)
			}
			matchesOracle(t, tc.name, o, testParams)
			if c, ends := cursorsOf(t, o), journalEnds(t, o); !maps.Equal(c, ends) {
				t.Errorf("cursors %v, want ReadAll's ends %v", c, ends)
			}
		})
	}
}

func TestStrayFileIgnored(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeShown)))
	stray := hideOf(nov(3), "m1")
	stray.Seq = 7
	appendBytes(t, filepath.Join(feedbackDir(o), "journal-old.jsonl"), append(encodeLine(t, stray), '\n'))
	visible := func(what string) {
		t.Helper()
		if e := folded(t, o).Memories["m1"]; e.Hidden != nil || e.Shown != 1 {
			t.Errorf("%s: m1 %+v, want shown once and not hidden", what, e)
		}
		if e := readerAll(t, o).Memories["m1"]; e.Hidden != nil {
			t.Errorf("%s: the reader shows m1 hidden", what)
		}
	}
	mustFold(t, w, testParams)
	visible("fold")
	appendLines(t, o, retireOf(nov(1), "m2"))
	if res := mustFold(t, w, testParams); !res.Refolded {
		t.Fatalf("a back-dated retire did not refold: %+v", res)
	}
	visible("refold")
	if _, err := w.Rebuild(context.Background(), testParams, true); err != nil {
		t.Fatal(err)
	}
	visible("rebuild")
	if _, ok := cursorsOf(t, o)["journal-old.jsonl"]; ok {
		t.Error("journal-old.jsonl has a cursor")
	}
}

func TestSettleKeyTwiceCountsOnce(t *testing.T) {
	credit := settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit))
	miss := settleOf(nov(3), show("k1", "m1", nov(1), feedback.OutcomeMiss))
	for _, tc := range []struct {
		name    string
		batches [][]feedback.Event
	}{
		{"one batch", [][]feedback.Event{{credit, miss}}},
		{"two batches", [][]feedback.Event{{credit}, {miss}}},
		{"two batches, the earlier last", [][]feedback.Event{{miss}, {credit}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tempOpts(t)
			w := mustOpenWriter(t, o)
			defer mustCloseWriter(t, w)
			for _, b := range tc.batches {
				appendLines(t, o, b...)
				mustFold(t, w, testParams)
			}
			if e := folded(t, o).Memories["m1"]; e.Shown != 1 || e.Credits != 1 || e.Misses != 0 {
				t.Errorf("m1 %+v: want the earlier settle's credit only", e)
			}
			matchesOracle(t, tc.name, o, testParams)
		})
	}
}

func TestParamsChangeRefoldsWithImports(t *testing.T) {
	p30, p60 := testParams, testParams
	p30.CreditHalfLife, p60.CreditHalfLife = 30*day, 60*day
	t.Run("with an import", func(t *testing.T) {
		o := tempOpts(t)
		w := mustOpenWriter(t, o)
		defer mustCloseWriter(t, w)
		// An import recording no half-lives: it decays under the folding ones.
		appendLines(t, o, feedback.Event{Kind: feedback.KindImport, TS: nov(1), Source: "telemetry", Settled: []feedback.Settled{
			show("i1", "m1", oct(0), feedback.OutcomeCredit), show("i2", "m1", oct(240), feedback.OutcomeCredit),
		}})
		mustFold(t, w, p30)
		matchesOracle(t, "folded at 30 days", o, p30)
		if a, b := oracle(t, feedbackDir(o), p30).Memories["m1"].Credits, oracle(t, feedbackDir(o), p60).Memories["m1"].Credits; a == b {
			t.Fatalf("credits %v at both half-lives: the case tests nothing", a)
		}
		res := mustFold(t, w, p60)
		if !res.Refolded || res.Reason != "params" {
			t.Errorf("fold at 60 days: %+v, want a params refold", res)
		}
		matchesOracle(t, "folded at 60 days", o, p60)
		if res := mustFold(t, w, p60); res.Refolded {
			t.Errorf("a second fold at 60 days: %+v, want no refold", res)
		}
	})
	t.Run("without one", func(t *testing.T) {
		o := tempOpts(t)
		w := mustOpenWriter(t, o)
		defer mustCloseWriter(t, w)
		appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)))
		for _, p := range []feedback.Params{p30, p60} {
			if res := mustFold(t, w, p); res.Refolded {
				t.Errorf("fold at %v: %+v, want no refold", p.CreditHalfLife, res)
			}
		}
		matchesOracle(t, "no import", o, p60)
	})
}
