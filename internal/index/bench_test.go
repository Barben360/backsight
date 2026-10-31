package index

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

// The plan's limits are 3x what the measures showed on the author's
// machine: a slower CI runner passes, a regression of an order of
// magnitude does not.
const (
	firstBuildLimit = 60 * time.Second
	catchUpLimit    = 600 * time.Millisecond
)

func benchOptions(t testing.TB, stateDir string) Options {
	t.Helper()
	return Options{
		Dirs: paths.Dirs{State: stateDir}, Home: t.TempDir(), Getenv: func(string) string { return "" },
		Now: func() time.Time { return genNow }, BinaryVersion: "v9.9.9-test",
	}
}

// firstBuild opens a new index over stateDir and ingests everything, with
// no deadline; it returns the time that took, with the index left open.
func firstBuild(t testing.TB, stateDir string) (*Index, time.Duration) {
	t.Helper()
	start := time.Now()
	ix, err := Open(context.Background(), benchOptions(t, stateDir))
	if err != nil {
		t.Fatal(err)
	}
	fr, err := ix.CatchUp(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Partial {
		t.Fatal("the first build is partial")
	}
	return ix, time.Since(start)
}

// catchUp15 appends 15 minutes of lines (60 telemetry, 10 journal) and
// times one catch-up within ViewBudget; the ledger is unchanged.
func catchUp15(t testing.TB, ix *Index, stateDir string, round int) time.Duration {
	t.Helper()
	appendRecent(t, stateDir, budgetSpec, 60, 10, round)
	start := time.Now()
	fr, err := ix.CatchUp(context.Background(), ViewBudget)
	d := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Partial || fr.Lines != 70 {
		t.Fatalf("catch-up: partial=%v lines=%d, want a complete one of 70", fr.Partial, fr.Lines)
	}
	return d
}

func TestIndexBudgets(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("time budgets are measured without -race and -short")
	}
	state := t.TempDir()
	genState(t, state, budgetSpec)
	ix, build := firstBuild(t, state)
	defer func() { _ = ix.Close() }()
	// best of 3 catch-ups (each of new lines): a slow shared runner's
	// one-off stall is not the index's cost
	catch := catchUp15(t, ix, state, 0)
	for round := 1; round < 3; round++ {
		catch = min(catch, catchUp15(t, ix, state, round))
	}
	// the measure is of real work: every kind landed and no line failed
	var errs, prompts, tools, shows, ledger int
	if err := ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
		for q, p := range map[string]*int{
			"ingest_errors": &errs, "tel_prompt": &prompts, "tel_tool": &tools, "shows": &shows, "ledger": &ledger,
		} {
			if err := tx.QueryRow("SELECT count(*) FROM " + q).Scan(p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if errs != 0 || prompts < 100_000 || tools < 80_000 || shows < 50_000 || ledger != budgetSpec.Memories {
		t.Errorf("generated state not fully ingested: %d errors, %d prompts, %d tools, %d shows, %d ledger rows", errs, prompts, tools, shows, ledger)
	}
	t.Logf("first build: %v (limit %v); 15-minute catch-up, best of 3: %v (limit %v)", build, firstBuildLimit, catch, catchUpLimit)
	if build > firstBuildLimit {
		t.Errorf("first build took %v, limit %v", build, firstBuildLimit)
	}
	if catch > catchUpLimit {
		t.Errorf("15-minute catch-up took %v, limit %v", catch, catchUpLimit)
	}
}

func BenchmarkFirstBuild(b *testing.B) {
	state := b.TempDir()
	genState(b, state, budgetSpec)
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		clean(b, state)
		b.StartTimer()
		ix, _ := firstBuild(b, state)
		b.StopTimer()
		_ = ix.Close()
	}
}

func BenchmarkCatchUp15Minutes(b *testing.B) {
	state := b.TempDir()
	genState(b, state, budgetSpec)
	ix, _ := firstBuild(b, state)
	defer func() { _ = ix.Close() }()
	var total time.Duration
	for i := 0; i < b.N; i++ {
		total += catchUp15(b, ix, state, i)
	}
	b.ReportMetric(float64(total.Nanoseconds())/float64(b.N), "ns/catchup")
}

// clean removes the index of stateDir, for the next first build.
func clean(b testing.TB, stateDir string) {
	b.Helper()
	if err := os.RemoveAll(paths.Dirs{State: stateDir}.IndexDir()); err != nil {
		b.Fatal(err)
	}
}
