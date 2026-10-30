package index

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/state"
)

// indexDir lists the index directory with each file's sha256.
func (r *recEnv) indexDir() map[string]string {
	r.t.Helper()
	ents, err := os.ReadDir(r.d.IndexDir())
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range ents {
		s, _ := fileSum(r.t, filepath.Join(r.d.IndexDir(), e.Name()))
		out[e.Name()] = s
	}
	return out
}

func (r *recEnv) rebuild(ro RebuildOptions) (RebuildResult, error) {
	return Rebuild(context.Background(), r.opts(), ro)
}

// unchanged fails when the index directory differs from before.
func (r *recEnv) unchanged(before map[string]string) {
	r.t.Helper()
	after := r.indexDir()
	for name, s := range before {
		if after[name] != s {
			r.t.Errorf("%s changed", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			r.t.Errorf("%s appeared", name)
		}
	}
}

// rowsOfSource counts the rows of every row table from one source file.
func rowsOfSource(t *testing.T, ix *Index, rel string) int64 {
	t.Helper()
	var total int64
	for _, tbl := range rowTables {
		var n int64
		if err := ix.sh.r.QueryRow(`SELECT count(*) FROM `+tbl+` WHERE source_file = ?`, rel).Scan(&n); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	return total
}

func TestRebuildListsLostMonths(t *testing.T) {
	r := newRecEnv(t)
	r.writeTel("2025-10", 20, 23)
	r.writeTel("2025-11", 30, 31)
	writeFile(t, filepath.Join(r.d.TelemetryDir(), "events-2025-12.jsonl"), "not json, no row\n")
	ix := r.openUp()
	rows := rowsOfSource(t, ix, "telemetry/events-2025-10.jsonl")
	if rows == 0 {
		t.Fatal("no rows")
	}
	// 2025-11 is caught up as gone (missing = 1), 2025-10 is only absent.
	if err := os.Remove(filepath.Join(r.d.TelemetryDir(), "events-2025-11.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.CatchUp(context.Background(), ViewBudget); err != nil {
		t.Fatal(err)
	}
	mustClose(t, ix)
	for _, m := range []string{"2025-10", "2025-12"} { // 2025-12 had no row: nothing lost
		if err := os.Remove(filepath.Join(r.d.TelemetryDir(), "events-"+m+".jsonl")); err != nil {
			t.Fatal(err)
		}
	}
	before := r.indexDir()

	res, err := r.rebuild(RebuildOptions{Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []LostSource{
		{SourceFile: "telemetry/events-2025-10.jsonl", Month: "2025-10", Rows: rows},
		{SourceFile: "telemetry/events-2025-11.jsonl", Month: "2025-11", Rows: 2},
	}
	if !res.Plan || !slices.Equal(res.Lost, want) || !res.NeedsYes || res.LostUnknown || res.File != r.main() {
		t.Errorf("plan %+v", res)
	}
	r.unchanged(before)

	res, err = r.rebuild(RebuildOptions{})
	if !errors.Is(err, ErrNeedsYes) || err.Error() != "some months would be lost" {
		t.Fatalf("want ErrNeedsYes, got %v", err)
	}
	if !slices.Equal(res.Lost, want) {
		t.Errorf("lost %v", res.Lost)
	}
	r.unchanged(before)

	res, err = r.rebuild(RebuildOptions{Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan || res.Lines != 6 {
		t.Errorf("result %+v", res)
	}
	absent(t, r.main()+".tmp")
	ix = mustOpen(t, r.opts())
	defer mustClose(t, ix)
	if n := rowsOfSource(t, ix, "telemetry/events-2025-10.jsonl"); n != 0 {
		t.Errorf("%d rows of the lost month", n)
	}
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Errorf("rebuilt index differs from a fresh build:\n%s\nwant\n%s", got, want)
	}
}

func TestRebuildOfAnUnreadableIndexNeedsYes(t *testing.T) {
	r := newRecEnv(t)
	writeFile(t, r.main(), "not a database")
	before := r.indexDir()
	res, err := r.rebuild(RebuildOptions{Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.LostUnknown || !res.NeedsYes || len(res.Lost) != 0 {
		t.Errorf("plan %+v", res)
	}
	if _, err := r.rebuild(RebuildOptions{}); !errors.Is(err, ErrNeedsYes) {
		t.Errorf("want ErrNeedsYes, got %v", err)
	}
	r.unchanged(before)
	writeFile(t, r.main()+"-wal", "a stale write-ahead log")
	writeFile(t, r.main()+"-shm", "a stale shared-memory index")
	left := companionsAtRename(t, r.main())
	if _, err := r.rebuild(RebuildOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if len(*left) != 0 {
		t.Errorf("left beside the new file: %v", *left)
	}
	ix := mustOpen(t, r.opts())
	defer mustClose(t, ix)
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Error("rebuilt index differs from a fresh build")
	}
}

// The old file left WAL mode for the rename (its header says so): its
// rows are what they were.
func TestRebuildRenameFailureKeepsTheOldIndex(t *testing.T) {
	r := newRecEnv(t)
	ix := r.openUp()
	want := dump(t, ix)
	mustClose(t, ix)
	r.writeTel("2026-10", 4, 8) // a rebuild would read more
	before := r.indexDir()
	prev := renameFile
	renameFile = func(string, string) error { return errors.New("injected rename failure") }
	t.Cleanup(func() { renameFile = prev })
	_, err := r.rebuild(RebuildOptions{Yes: true})
	if !errors.Is(err, ErrInUse) || err.Error() != "the metrics index is in use: close the dashboard and run backsight stats index rebuild again; the old index is kept" {
		t.Fatalf("want ErrInUse, got %v", err)
	}
	after := r.indexDir()
	if len(after) != len(before) {
		t.Errorf("files %v, were %v", after, before)
	}
	absent(t, r.main()+".tmp")
	ix = mustOpen(t, r.opts())
	defer mustClose(t, ix)
	if got := dump(t, ix); got != want {
		t.Errorf("the old index changed:\n%s\nwas\n%s", got, want)
	}
}

func TestRebuildRefusesWhileAnotherConnectionIsOpen(t *testing.T) {
	r := newRecEnv(t)
	r.build()
	r.writeTel("2026-10", 4, 8) // the rebuild has more to read than the old index
	sum, _ := fileSum(t, r.main())

	raw, err := sql.Open(driverName, "file:"+r.main())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM cursors`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := r.rebuild(RebuildOptions{Yes: true}); !errors.Is(err, ErrInUse) {
		t.Fatalf("want ErrInUse, got %v", err)
	}
	if s, _ := fileSum(t, r.main()); s != sum {
		t.Error("metrics.db changed")
	}
	absent(t, r.main()+".tmp")
	_ = tx.Rollback()
	_ = raw.Close()

	t.Run("a handle of this process", func(t *testing.T) {
		held := mustOpen(t, r.opts())
		defer mustClose(t, held)
		if _, err := r.rebuild(RebuildOptions{Yes: true}); !errors.Is(err, ErrInUse) {
			t.Fatalf("want ErrInUse, got %v", err)
		}
		absent(t, r.main()+".tmp")
	})

	if _, err := r.rebuild(RebuildOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	ix := mustOpen(t, r.opts())
	defer mustClose(t, ix)
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Error("rebuilt index differs from a fresh build")
	}
}

func TestRebuildWaitsForTheUpdate(t *testing.T) {
	r := newRecEnv(t)
	r.build()
	if err := state.WriteUpdateMarker(r.d, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
		t.Fatal(err)
	}
	before := r.indexDir()
	for _, ro := range []RebuildOptions{{Yes: true}, {Plan: true}} {
		if _, err := r.rebuild(ro); !errors.Is(err, ErrUpdateInProgress) {
			t.Errorf("%+v: want ErrUpdateInProgress, got %v", ro, err)
		}
	}
	r.unchanged(before)
}

func TestRebuildOnANewerIndexRebuildsTheSideFile(t *testing.T) {
	r := newRecEnv(t)
	o := r.opts()
	o.Migrations = withTest(t)
	ix := mustOpen(t, o)
	if _, err := ix.CatchUp(context.Background(), ViewBudget); err != nil {
		t.Fatal(err)
	}
	mustClose(t, ix)
	sum, _ := fileSum(t, r.main())
	res, err := r.rebuild(RebuildOptions{Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	side := filepath.Join(r.d.IndexDir(), SideFileName(1))
	if res.File != side {
		t.Errorf("file %s", res.File)
	}
	if s, _ := fileSum(t, r.main()); s != sum {
		t.Error("metrics.db changed")
	}
	if v := headerVersion(t, side); v != 1 {
		t.Errorf("side file version %d", v)
	}
	ix = mustOpen(t, r.opts())
	defer mustClose(t, ix)
	if !ix.Side() {
		t.Error("not the side file")
	}
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Error("rebuilt side file differs from a fresh build")
	}
}

// A user's rebuild lifts the stop of a second automatic failure.
func TestRebuildClearsTheStop(t *testing.T) {
	r := newRecEnv(t)
	r.build()
	r.setStatus(func(s *recoveryState) { s.RebuiltAt, s.StoppedAt = clock, clock })
	if _, err := r.rebuild(RebuildOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if st := r.status(); !st.StoppedAt.IsZero() || !st.RebuiltAt.Equal(clock) {
		t.Errorf("status %+v", st)
	}
}

// In DELETE mode an idle handle holds no lock: the process's own registry
// refuses the rename under it.
func TestRebuildRefusesUnderAnIdleHandleOnANetworkFolder(t *testing.T) {
	onNetworkFS(t)
	r := newRecEnv(t)
	r.build()
	r.writeTel("2026-10", 4, 8)
	held := r.openUp()
	want := dump(t, held)
	if _, err := r.rebuild(RebuildOptions{Yes: true}); !errors.Is(err, ErrInUse) {
		t.Fatalf("want ErrInUse, got %v", err)
	}
	absent(t, r.main()+".tmp")
	if got := dump(t, held); got != want {
		t.Error("the held index changed")
	}
}
