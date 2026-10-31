package dashboard

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/paths"
)

// dataSpec is the generated state of the data tests: a month, small.
var dataSpec = indexgen.Spec{Months: 1, TelemetryPerYear: 6000, JournalPerYear: 1200, Memories: 50, Seed: 4}

// fakeIndex wraps a real index, generated and caught up, counting its
// calls. fileVersion, side and catchUpErr are what it reports; block, when
// set, holds each ReadTx until it is closed or the call's context ends;
// entered, when set, receives a value as each ReadTx starts. open is the
// Config's OpenIndex: it fails with openErrs in turn, and waits for
// openGate when set.
type fakeIndex struct {
	real *index.Index

	mu          sync.Mutex
	fileVersion int
	side        bool
	catchUpErr  error
	block       chan struct{}
	entered     chan struct{}
	catchUps    int
	budgets     []time.Duration
	readTxs     int
	closes      int
	openErrs    []error
	openGate    chan struct{}
	opens       int
	openEnded   error // the context error an open waiting for openGate returned
}

var _ Index = (*fakeIndex)(nil)

// newFakeIndex generates dataSpec under dirs.State, opens it on the clock
// now and catches it up.
func newFakeIndex(t *testing.T, dirs paths.Dirs, home string, now func() time.Time) *fakeIndex {
	t.Helper()
	indexgen.Generate(t, dirs.State, dataSpec)
	ctx := context.Background()
	ix, err := index.Open(ctx, index.Options{Dirs: dirs, Home: home, Getenv: func(string) string { return "" }, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	if fr, err := ix.CatchUp(ctx, time.Hour); err != nil || fr.Partial {
		t.Fatalf("catch-up: %v, partial %v", err, fr.Partial)
	}
	return &fakeIndex{real: ix, fileVersion: ix.Schema()}
}

// set changes the fake under its lock.
func (f *fakeIndex) set(fn func(*fakeIndex)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// counts are the CatchUp, ReadTx and Close calls so far.
func (f *fakeIndex) counts() (catchUps, readTxs, closes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catchUps, f.readTxs, f.closes
}

func (f *fakeIndex) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	f.mu.Lock()
	f.readTxs++
	block, entered := f.block, f.entered
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default: // a full channel: the test reads none past what it buffered
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.real.ReadTx(ctx, fn)
}

func (f *fakeIndex) CatchUp(ctx context.Context, budget time.Duration) (index.Freshness, error) {
	f.mu.Lock()
	f.catchUps++
	f.budgets = append(f.budgets, budget)
	err := f.catchUpErr
	f.mu.Unlock()
	if err != nil {
		return index.Freshness{}, err
	}
	return f.real.CatchUp(ctx, budget)
}

func (f *fakeIndex) Schema() int { return f.real.Schema() }

func (f *fakeIndex) Side() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.side
}

func (f *fakeIndex) FileVersion(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fileVersion, nil
}

// Close is counted; the real index closes at the test's end.
func (f *fakeIndex) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

// open is the Config's OpenIndex.
func (f *fakeIndex) open(ctx context.Context) (Index, error) {
	f.mu.Lock()
	f.opens++
	gate := f.openGate
	var err error
	if len(f.openErrs) > 0 {
		err, f.openErrs = f.openErrs[0], f.openErrs[1:]
	}
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			f.set(func(f *fakeIndex) { f.openEnded = ctx.Err() })
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// fakeServer is a server answering at once with empty data, counting the
// calls per method. down holds each call until its context ends; gate,
// when set, holds each call until it is closed or its context ends; fail
// is every call's error; memText is the text Memory returns.
type fakeServer struct {
	mu      sync.Mutex
	calls   map[string]int
	down    bool
	gate    chan struct{}
	fail    error
	memText string
}

var _ metrics.ServerReader = (*fakeServer)(nil)

// set changes the fake under its lock.
func (f *fakeServer) set(fn func(*fakeServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// count is how many times method was called.
func (f *fakeServer) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// enter counts a call of method and holds it as down or gate say.
func (f *fakeServer) enter(ctx context.Context, method string) error {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[method]++
	down, gate, fail := f.down, f.gate, f.fail
	f.mu.Unlock()
	if down {
		<-ctx.Done()
		return ctx.Err()
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fail
}

func (f *fakeServer) Version(ctx context.Context) (hindsight.Version, error) {
	return hindsight.Version{APIVersion: "test"}, f.enter(ctx, "Version")
}

func (f *fakeServer) BankStats(ctx context.Context) (hindsight.BankStats, error) {
	return hindsight.BankStats{}, f.enter(ctx, "BankStats")
}

func (f *fakeServer) Memory(ctx context.Context, id string) (hindsight.MemoryDetail, error) {
	if err := f.enter(ctx, "Memory"); err != nil {
		return hindsight.MemoryDetail{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return hindsight.MemoryDetail{ID: id, Text: f.memText}, nil
}

func (f *fakeServer) ProjectTags(ctx context.Context) ([]hindsight.TagCount, error) {
	return nil, f.enter(ctx, "ProjectTags")
}

func (f *fakeServer) MemoriesTimeseries(ctx context.Context, period string) (hindsight.Timeseries, error) {
	return hindsight.Timeseries{Period: period}, f.enter(ctx, "MemoriesTimeseries")
}

func (f *fakeServer) LLMStats(ctx context.Context, _, _ string) ([]hindsight.LLMBucket, error) {
	return nil, f.enter(ctx, "LLMStats")
}

func (f *fakeServer) OperationsPage(ctx context.Context, _, _ string, _, _ int) (hindsight.OperationPage, error) {
	return hindsight.OperationPage{}, f.enter(ctx, "OperationsPage")
}

// withIndex installs a fakeIndex (h.ix) as the index, mods applied before
// Start, and the default configuration.
func withIndex(mods ...func(*fakeIndex)) harnessOpt {
	return func(h *harness, c *Config) {
		h.ix = newFakeIndex(h.t, h.dirs, h.home, h.clock.now)
		for _, m := range mods {
			m(h.ix)
		}
		c.OpenIndex = h.ix.open
		c.Source.Config = config.Default(h.dirs, h.home)
		c.Source.Configured = true
	}
}

// withServer installs a fakeServer (h.srv) as the server.
func withServer() harnessOpt {
	return func(h *harness, c *Config) {
		h.srv = &fakeServer{}
		c.Source.Server = h.srv
		c.Source.ServerHost = "hindsight.test:8888"
	}
}

// withDoctor installs an instant doctor counting its runs (h.doctorRuns).
func withDoctor() harnessOpt {
	return func(h *harness, c *Config) {
		c.Source.Doctor = func(context.Context) ([]metrics.DoctorCheck, error) {
			h.doctorRuns.Add(1)
			return []metrics.DoctorCheck{{ID: "server", Status: "ok"}}, nil
		}
	}
}

// withNeedsBuild makes Start build the index in the background.
func withNeedsBuild() harnessOpt {
	return func(_ *harness, c *Config) {
		c.NeedsBuild = func(context.Context) (bool, error) { return true, nil }
	}
}
