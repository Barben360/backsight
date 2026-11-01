package index

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/testtool"
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
// no deadline, then checkpoints the WAL into the file (the jobs tick's
// TRUNCATE checkpoint, or the close, would flush it otherwise: up to
// 256 MiB on Windows); it returns the time that took, with the index left
// open.
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
	if err := ix.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ix, time.Since(start)
}

// catchUp15 appends 15 minutes of lines (60 telemetry, 10 journal) and
// times one catch-up within ViewBudget; the ledger is unchanged.
func catchUp15(t testing.TB, ix *Index, stateDir string, round int) time.Duration {
	t.Helper()
	indexgen.AppendRecent(t, stateDir, budgetSpec, 60, 10, round)
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
	state := testtool.BenchTempDir(t)
	indexgen.Generate(t, state, budgetSpec)
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
	ledgerSkipTimes(t, state, 20)
}

// countedCurrent is the feedback directory's CurrentReader, its reads
// counted; its Stamp is CurrentReader's.
type countedCurrent struct {
	feedback.CurrentReader
	alls atomic.Int64
}

func (c *countedCurrent) All() (feedback.Ledger, error) {
	c.alls.Add(1)
	return c.CurrentReader.All()
}

// stampless is a CurrentReader with its Stamp hidden, its reads counted.
type stampless struct {
	r    feedback.CurrentReader
	alls atomic.Int64
}

func (s *stampless) All() (feedback.Ledger, error) {
	s.alls.Add(1)
	return s.r.All()
}

// ledgerSkipTimes measures what the ledger's skip saves: the feedback
// files aged an hour before the clock, n catch-ups with nothing new on
// two handles of stateDir's index, one reading through the CurrentReader
// (it skips), one through the same reader without its Stamp (it reads
// every time), interleaved; it logs both medians and a Stamp's. Only the
// read counts are checked: the times are evidence, not a budget.
func ledgerSkipTimes(t *testing.T, stateDir string, n int) {
	t.Helper()
	ctx := context.Background()
	fdir := paths.Dirs{State: stateDir}.FeedbackDir()
	ents, err := os.ReadDir(fdir)
	if err != nil {
		t.Fatal(err)
	}
	aged := genNow.Add(-time.Hour)
	for _, e := range ents {
		if err := os.Chtimes(filepath.Join(fdir, e.Name()), aged, aged); err != nil {
			t.Fatal(err)
		}
	}
	open := func(r feedback.LedgerReader) *Index {
		o := benchOptions(t, stateDir)
		o.Ledger = r
		ix, err := Open(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ix.Close() })
		return ix
	}
	skipR := &countedCurrent{CurrentReader: feedback.CurrentReader{Dir: fdir}}
	fullR := &stampless{r: feedback.CurrentReader{Dir: fdir}}
	skipIx, fullIx := open(skipR), open(fullR)
	if !skipIx.localFeedback {
		t.Logf("ledger skip: the feedback directory is not local here (%v), not measured", detectFS(fdir, "", func(string) string { return "" }))
		return
	}
	run := func(ix *Index) time.Duration {
		start := time.Now()
		fr, err := ix.CatchUp(ctx, ViewBudget)
		d := time.Since(start)
		if err != nil || fr.Partial {
			t.Fatalf("catch-up: %v, partial %v", err, fr.Partial)
		}
		return d
	}
	run(skipIx) // records the stamp
	run(fullIx)
	var skips, fulls, stamps []time.Duration
	for range n {
		skips = append(skips, run(skipIx))
		fulls = append(fulls, run(fullIx))
		start := time.Now()
		if _, err := skipR.Stamp(); err != nil {
			t.Fatal(err)
		}
		stamps = append(stamps, time.Since(start))
	}
	if got := skipR.alls.Load(); got != 1 {
		t.Errorf("the stamped reader was read %d times, want once", got)
	}
	if got := fullR.alls.Load(); got != int64(n+1) {
		t.Errorf("the reader without a stamp was read %d times, want %d", got, n+1)
	}
	med := func(ds []time.Duration) time.Duration { slices.Sort(ds); return ds[len(ds)/2] }
	t.Logf("catch-up with nothing new, median of %d: ledger skipped %v, ledger read %v; a stamp %v", n, med(skips), med(fulls), med(stamps))
}

func BenchmarkFirstBuild(b *testing.B) {
	state := b.TempDir()
	indexgen.Generate(b, state, budgetSpec)
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
	indexgen.Generate(b, state, budgetSpec)
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
