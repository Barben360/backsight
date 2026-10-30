package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/state"
)

// maintainClock is the day of the pruning tests: the cutoff is the start
// of 2024-10 (UTC), 24 months before 2026-10.
var maintainClock = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

func (e *ingestEnv) maintain() MaintainResult {
	e.t.Helper()
	o := opts(e.t, e.d, nil)
	o.Now = e.clock.Now
	res, err := Maintain(context.Background(), o)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *ingestEnv) cursorOf(rel string) bool {
	e.t.Helper()
	return len(e.rowsOf(`SELECT 1 FROM cursors WHERE source_file = ?`, rel)) == 1
}

func (e *ingestEnv) errorKeys() []string {
	e.t.Helper()
	var out []string
	for _, r := range e.rowsOf(`SELECT source_file, "offset" FROM ingest_errors ORDER BY source_file, "offset"`) {
		out = append(out, fmt.Sprintf("%v@%v", r["source_file"], r["offset"]))
	}
	return out
}

func (e *ingestEnv) addError(rel string, off int64, reason string, at time.Time) {
	e.t.Helper()
	if _, err := e.ix.sh.w.Exec(`INSERT INTO ingest_errors(source_file, "offset", reason, at) VALUES (?, ?, ?, ?)`,
		rel, off, reason, at.UnixMilli()); err != nil {
		e.t.Fatal(err)
	}
}

func settleLine(t *testing.T, at time.Time) string {
	t.Helper()
	return jline(t, feedback.Event{TS: at, Session: "s1", Kind: "settle",
		Settled: []feedback.Settled{{Key: fmt.Sprintf("s1|t1|%d|m1", at.UnixNano()), ID: "m1", At: at, Outcome: "credit"}}})
}

// TestMaintainPrunesByMonth: rows before the start of the UTC month 24
// months back go, the rest stays (a future stamp too); ingest errors go
// after 30 days; the cursor of a file gone with all its rows goes, that
// of a file gone with rows kept, or of a file still there, stays.
func TestMaintainPrunesByMonth(t *testing.T) {
	e := newEnv(t)
	e.clock.t = maintainClock
	before := time.Date(2024, 9, 30, 23, 59, 0, 0, time.UTC) // just before the cutoff
	cutoff := time.Date(2024, 10, 1, 0, 0, 0, 0, time.UTC)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339Nano) }

	e.write("2024-09", tline("prompt", stamp(before), map[string]any{"n": 1, "decision": "skip"})) // gone, every row pruned
	e.write("2024-10", tline("prompt", stamp(cutoff), map[string]any{"n": 2, "decision": "skip"})) // gone, its row kept
	e.write("2024-08", tline("prompt", stamp(before), map[string]any{"n": 3, "decision": "skip"})) // stays on disk, row pruned
	future := tline("prompt", "2027-01-01T00:00:00Z", map[string]any{"n": 4, "decision": "skip"})
	e.write("2026-10", future+"not json\n")
	writeFile(t, e.jpath("2024-09"), settleLine(t, before)) // settles: settle_ts is the time
	writeFile(t, e.jpath("2024-10"), settleLine(t, cutoff))
	e.ingest()
	if e.count("tel_prompt") != 4 || e.count("settles") != 2 {
		t.Fatalf("ingest: %d prompts, %d settles", e.count("tel_prompt"), e.count("settles"))
	}

	// ingest errors: the present file's line error is recent; add old ones
	present := "telemetry/events-2026-10.jsonl"
	e.addError(present, 1000, errUnparsable, maintainClock.Add(-31*24*time.Hour)) // pruned
	e.addError(present, 2000, errUnparsable, maintainClock.Add(-29*24*time.Hour)) // kept
	gone := "telemetry/events-2024-10.jsonl"
	e.addError(gone, 3000, errUnparsable, maintainClock.Add(-time.Hour)) // a gone file with a cursor: kept
	e.addError("telemetry/events-2025-01.jsonl", unreadableOffset, errUnreadable, maintainClock.Add(-time.Hour))
	e.addError(ledgerSource, 0, errLedgerUnreadable, maintainClock.Add(-time.Hour))
	e.addError(stateSource, 0, errLocalStatusUnreadable, maintainClock.Add(-time.Hour))

	for _, p := range []string{e.telPath("2024-09"), e.telPath("2024-10"), e.jpath("2024-09"), e.jpath("2024-10")} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	e.ingest() // marks them missing, keeps their rows

	res := e.maintain()
	if res.RowsPruned != 3 {
		t.Errorf("rows pruned %d, want 3 (two prompts and a settle)", res.RowsPruned)
	}
	var ns []int64
	for _, r := range e.rowsOf(`SELECT n FROM tel_prompt ORDER BY n`) {
		ns = append(ns, r["n"].(int64))
	}
	if !slices.Equal(ns, []int64{2, 4}) {
		t.Errorf("prompts kept %v, want [2 4]", ns)
	}
	if rs := e.rowsOf(`SELECT settle_ts FROM settles`); len(rs) != 1 || rs[0]["settle_ts"] != cutoff.UnixMilli() {
		t.Errorf("settles kept %v", rs)
	}
	wantErrs := []string{ // by source_file, then offset
		ledgerSource + "@0",
		stateSource + "@0",
		gone + "@3000",
		fmt.Sprintf("%s@%d", present, len(future)), // the "not json" line, recorded now
		present + "@2000",
	}
	if got := e.errorKeys(); !slices.Equal(got, wantErrs) {
		t.Errorf("ingest errors\n%v\nwant\n%v", got, wantErrs)
	}
	if res.ErrorsPruned != 2 {
		t.Errorf("errors pruned %d, want 2 (the 31-day one and the deleted unreadable file's)", res.ErrorsPruned)
	}
	for rel, want := range map[string]bool{
		"telemetry/events-2024-09.jsonl": false, // gone, rows all pruned
		"feedback/journal-2024-09.jsonl": false,
		"telemetry/events-2024-10.jsonl": true, // gone, rows kept
		"feedback/journal-2024-10.jsonl": true,
		"telemetry/events-2024-08.jsonl": true, // on disk: kept, so it is not read again
		present:                          true,
	} {
		if got := e.cursorOf(rel); got != want {
			t.Errorf("cursor of %s: %v, want %v", rel, got, want)
		}
	}
	if st := e.ingest(); st.Lines != 0 {
		t.Errorf("an ingest after the pruning read %d line(s) again", st.Lines)
	}

	// twice: nothing more
	snap := dump(t, e.ix)
	if again := e.maintain(); again.RowsPruned != 0 || again.ErrorsPruned != 0 || len(again.SideRemoved)+len(again.SideKept)+len(again.TempRemoved) != 0 {
		t.Errorf("second run %+v", again)
	}
	if dump(t, e.ix) != snap {
		t.Error("a second run changed the index")
	}
}

// The cutoff is the start of the UTC month 24 months back, whatever the
// local zone and day of the month.
func TestMaintainCutoff(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip(err)
	}
	for _, c := range []struct {
		now  time.Time
		want string
	}{
		{time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC), "2024-10-01T00:00:00Z"},
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "2024-01-01T00:00:00Z"},
		{time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), "2024-12-01T00:00:00Z"},
		// 2026-11-01 00:30 in Paris is still October in UTC
		{time.Date(2026, 11, 1, 0, 30, 0, 0, paris), "2024-10-01T00:00:00Z"},
	} {
		if got := pruneCutoff(c.now).Format(time.RFC3339); got != c.want {
			t.Errorf("%s: cutoff %s, want %s", c.now, got, c.want)
		}
	}
}

// TestMaintainRemovesLowerSideFiles: a side file of a lower schema goes
// with its companions once nothing holds it; one held is kept and named,
// and goes at the next run; a newer one is never touched.
func TestMaintainRemovesLowerSideFiles(t *testing.T) {
	ctx := context.Background()
	d := tempDirs(t)
	head2 := withTest(t)
	build(t, opts(t, d, head2)) // metrics.db at 2
	v1 := filepath.Join(d.IndexDir(), SideFileName(1))
	v3 := filepath.Join(d.IndexDir(), SideFileName(3))
	build(t, opts(t, d, nil)) // an older binary's side file
	if _, err := os.Stat(v1); err != nil {
		t.Fatal(err)
	}
	writeFile(t, v1+"-wal", "")
	writeFile(t, v1+"-shm", "")
	writeFile(t, v3, "a newer binary's side file")
	writeFile(t, v3+"-wal", "its log")
	sum3, _ := fileSum(t, v3)

	raw, err := sql.Open(driverName, "file:"+v1)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	res, err := Maintain(ctx, opts(t, d, head2))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.SideKept, []string{SideFileName(1)}) || len(res.SideRemoved) != 0 {
		t.Errorf("held: %+v", res)
	}
	if _, err := os.Stat(v1); err != nil {
		t.Errorf("a held side file was removed: %v", err)
	}
	_ = tx.Rollback()
	_ = raw.Close()

	res, err = Maintain(ctx, opts(t, d, head2))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.SideRemoved, []string{SideFileName(1)}) || len(res.SideKept) != 0 {
		t.Errorf("released: %+v", res)
	}
	for _, sfx := range []string{"", "-wal", "-shm", "-journal"} {
		absent(t, v1+sfx)
	}
	if s, _ := fileSum(t, v3); s != sum3 {
		t.Error("the newer side file changed")
	}
	if got := readFile(t, v3+"-wal"); got != "its log" {
		t.Errorf("the newer side file's -wal: %q", got)
	}
	if _, err := os.Stat(mainFile(d)); err != nil {
		t.Errorf("metrics.db: %v", err)
	}
	if res, err := Maintain(ctx, opts(t, d, head2)); err != nil || len(res.SideRemoved)+len(res.SideKept) != 0 {
		t.Errorf("third run %+v %v", res, err)
	}
}

// A side file this process holds is kept (an open of a lower registry: a
// test's, or a dashboard of the same binary built with fewer migrations).
func TestMaintainKeepsASideFileThisProcessHolds(t *testing.T) {
	d := tempDirs(t)
	head2 := withTest(t)
	build(t, opts(t, d, head2))
	held := mustOpen(t, opts(t, d, nil))
	defer mustClose(t, held)
	res, err := Maintain(context.Background(), opts(t, d, head2))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.SideKept, []string{SideFileName(1)}) {
		t.Errorf("%+v", res)
	}
}

// A missing index: nothing to do, nothing created.
func TestMaintainWithoutAnIndexCreatesNothing(t *testing.T) {
	d := tempDirs(t)
	res, err := Maintain(context.Background(), opts(t, d, nil))
	if err != nil || res.RowsPruned != 0 || res.ErrorsPruned != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	absent(t, d.IndexDir())

	t.Run("an index directory without metrics.db", func(t *testing.T) {
		if err := os.MkdirAll(d.IndexDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Maintain(context.Background(), opts(t, d, nil)); err != nil {
			t.Fatal(err)
		}
		ents, _ := os.ReadDir(d.IndexDir())
		if len(ents) != 0 {
			t.Errorf("files created: %v", ents)
		}
	})

	t.Run("a newer metrics.db without this binary's side file", func(t *testing.T) {
		d := tempDirs(t)
		build(t, opts(t, d, withTest(t)))
		sum, _ := fileSum(t, mainFile(d))
		if _, err := Maintain(context.Background(), opts(t, d, nil)); err != nil {
			t.Fatal(err)
		}
		absent(t, filepath.Join(d.IndexDir(), SideFileName(1)))
		if s, _ := fileSum(t, mainFile(d)); s != sum {
			t.Error("the newer metrics.db changed")
		}
	})
}

// On a newer metrics.db, the pruning works on this binary's side file and
// leaves metrics.db alone.
func TestMaintainPrunesTheSideFileOfANewerIndex(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, withTest(t))) // a newer binary's metrics.db
	sum, _ := fileSum(t, mainFile(d))
	o := opts(t, d, nil)
	o.Now = func() time.Time { return maintainClock }
	writeFile(t, filepath.Join(d.TelemetryDir(), "events-2024-09.jsonl"), tline("prompt", "2024-09-30T23:59:00Z", map[string]any{"n": 1, "decision": "skip"}))
	ix := mustOpen(t, o)
	if !ix.Side() {
		t.Fatal("not on the side file")
	}
	if _, err := ix.CatchUp(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	mustClose(t, ix)
	res, err := Maintain(context.Background(), o)
	if err != nil || res.RowsPruned != 1 {
		t.Errorf("%+v %v", res, err)
	}
	if s, _ := fileSum(t, mainFile(d)); s != sum {
		t.Error("the newer metrics.db changed")
	}
}

// During an uncommitted update, an index that needs a migration is not
// opened (ErrUpdateInProgress), and nothing is written to it.
func TestMaintainWaitsForAnUncommittedUpdate(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, nil))
	sum, _ := fileSum(t, mainFile(d))
	if err := state.WriteUpdateMarker(d, state.UpdateMarker{From: "v1", To: "v2", Committed: false}); err != nil {
		t.Fatal(err)
	}
	_, err := Maintain(context.Background(), opts(t, d, withTest(t)))
	if !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("want ErrUpdateInProgress, got %v", err)
	}
	if s, _ := fileSum(t, mainFile(d)); s != sum {
		t.Error("metrics.db changed")
	}
}

// Maintain sweeps the temp files a crashed rebuild left (untouched for an
// hour) under rebuild.lock, keeps a fresh one, and leaves them all while
// a live rebuild holds the lock.
func TestMaintainSweepsStaleRebuildTemps(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, nil))
	main := mainFile(d)
	stale, fresh := main+".tmp-88888-dcba", main+".tmp-99999-abcd"
	writeFile(t, stale, "a crashed rebuild's file")
	writeFile(t, stale+"-wal", "its log")
	writeFile(t, fresh, "a live rebuild's file")
	old := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{stale, stale + "-wal"} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	lock, err := rebuildLock(d)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Maintain(context.Background(), opts(t, d, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !res.TempSkipped || len(res.TempRemoved) != 0 {
		t.Errorf("under a live rebuild's lock: %+v", res)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("swept under a live rebuild's lock: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	res, err = Maintain(context.Background(), opts(t, d, nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.TempSkipped || !slices.Equal(res.TempRemoved, []string{filepath.Base(stale)}) {
		t.Errorf("%+v", res)
	}
	absent(t, stale)
	absent(t, stale+"-wal")
	if got := readFile(t, fresh); got != "a live rebuild's file" {
		t.Errorf("fresh temp %q", got)
	}
	absent(t, filepath.Join(d.IndexDir(), rebuildLockName))
}

// setPruneBatch makes the pruning's batches n rows for the test.
func setPruneBatch(t *testing.T, n int) {
	t.Helper()
	prev := pruneBatch
	pruneBatch = n
	t.Cleanup(func() { pruneBatch = prev })
}

// A pruning larger than one batch removes every row, a short transaction
// per batch, so that the write lock is never held for a whole table; the
// old ingest errors go the same way.
func TestMaintainPrunesInBatches(t *testing.T) {
	e := newEnv(t)
	e.clock.t = maintainClock
	var b strings.Builder
	for i := 0; i < 7; i++ {
		b.WriteString(tline("prompt", "2024-09-01T00:00:00Z", map[string]any{"n": i, "decision": "skip"}))
	}
	e.write("2024-09", b.String())
	e.write("2026-10", tline("prompt", "2026-10-01T00:00:00Z", map[string]any{"n": 100, "decision": "skip"}))
	e.ingest()
	for i := 0; i < 5; i++ {
		e.addError("telemetry/events-2026-10.jsonl", int64(1000+i), errUnparsable, maintainClock.Add(-40*24*time.Hour))
	}
	setPruneBatch(t, 3)
	commits := countBatches(t)
	res := e.maintain()
	if res.RowsPruned != 7 || res.ErrorsPruned != 5 {
		t.Errorf("%+v", res)
	}
	if n := e.count("tel_prompt"); n != 1 {
		t.Errorf("%d prompt row(s) left, want 1", n)
	}
	if n := e.count("ingest_errors"); n != 0 {
		t.Errorf("%d ingest error(s) left", n)
	}
	if *commits != 5 { // rows 3+3+1, errors 3+2
		t.Errorf("%d commits, want 5 (one per batch)", *commits)
	}
}

// A pruning stops between batches when its context ends.
func TestMaintainPruneStopsOnItsContext(t *testing.T) {
	e := newEnv(t)
	e.clock.t = maintainClock
	var b strings.Builder
	for i := 0; i < 7; i++ {
		b.WriteString(tline("prompt", "2024-09-01T00:00:00Z", map[string]any{"n": i, "decision": "skip"}))
	}
	e.write("2024-09", b.String())
	e.ingest()
	setPruneBatch(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	setAfterBatch(t, cancel) // the first batch's commit ends the context
	o := opts(t, e.d, nil)
	o.Now = e.clock.Now
	res, err := Maintain(ctx, o)
	if !errors.Is(err, context.Canceled) || res.RowsPruned != 3 {
		t.Errorf("%+v %v", res, err)
	}
	if n := e.count("tel_prompt"); n != 4 {
		t.Errorf("%d row(s) left, want 4 (one batch committed)", n)
	}
}

// With an index directory but no index and no temp file, Maintain writes
// nothing there, not even rebuild.lock: a directory it cannot write is
// no error.
func TestMaintainWritesNothingWithoutAnIndex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a read-only directory mode does not stop writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	d := tempDirs(t)
	if err := os.MkdirAll(d.IndexDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d.IndexDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(d.IndexDir(), 0o700) })
	if res, err := Maintain(context.Background(), opts(t, d, nil)); err != nil || res.TempSkipped {
		t.Errorf("%+v %v", res, err)
	}
	if ents, _ := os.ReadDir(d.IndexDir()); len(ents) != 0 {
		t.Errorf("files created: %v", ents)
	}
}
