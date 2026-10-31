package dashboard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// Index is the metrics index as the dashboard reads it (*index.Index).
type Index interface {
	metrics.Reader
	CatchUp(ctx context.Context, budget time.Duration) (index.Freshness, error)
	Schema() int
	Side() bool
	FileVersion(ctx context.Context) (int, error)
	Close() error
}

var _ Index = (*index.Index)(nil)

// indexState is what the data routes can serve: the views, or the page
// that stands for them.
type indexState int

const (
	stateReady    indexState = iota // the index is open, of this binary's schema
	stateBuilding                   // its first build runs
	stateUpdating                   // an update is being applied: opened again at the next request
	stateUpdated                    // a newer backsight changed it: a restart is needed (sticky)
	stateBroken                     // it cannot be read
)

// buildBudget bounds the first build's catch-up, as the command line's.
var buildBudget = 5 * time.Minute

// errNoIndex: the dashboard was configured without an index.
var errNoIndex = errors.New("the dashboard has no metrics index")

// data is the dashboard's data layer: the index, its states and the
// caches the pages and the API share.
type data struct {
	cfg      Config
	life     context.Context // detached work: Start's context without its cancel, ended by stop
	endLife  context.CancelFunc
	endBuild context.CancelFunc // nil: no first build
	built    chan struct{}      // closed when the first build ended; nil: none
	results  *resultCache
	src      metrics.Source // the cached server and doctor; Index and Fresh per request

	mu       sync.Mutex
	ix       Index
	building bool
	updated  bool     // sticky until the restart
	opening  *opening // the open attempt in flight
	stopped  bool
	wg       sync.WaitGroup // the detached work stop waits for
}

// opening is an open attempt; err is set before done is closed.
type opening struct {
	done chan struct{}
	err  error
}

// newData is the data layer of cfg (cfg.Now set). Nothing is opened yet.
func newData(ctx context.Context, cfg Config) *data {
	life, end := context.WithCancel(context.WithoutCancel(ctx))
	d := &data{cfg: cfg, life: life, endLife: end, src: cfg.Source}
	d.results = &resultCache{now: cfg.Now, base: life, spawn: d.spawn}
	d.src.Index, d.src.Fresh, d.src.Now = nil, index.Freshness{}, cfg.Now
	if cfg.Source.Server != nil {
		d.src.Server = &cachedServer{next: cfg.Source.Server, now: cfg.Now}
	}
	if cfg.Source.Doctor != nil {
		d.src.Doctor = cachedDoctor(life, cfg.Source.Doctor, cfg.Now)
	}
	return d
}

// start opens the index, or starts its first build in the background
// under a context derived from ctx (stop cancels it). An update being
// applied is not fatal: the index is opened again at the next request.
func (d *data) start(ctx context.Context) error {
	if d.cfg.OpenIndex == nil {
		return nil
	}
	needs := false
	if d.cfg.NeedsBuild != nil {
		var err error
		if needs, err = d.cfg.NeedsBuild(ctx); err != nil {
			return fmt.Errorf("inspect the metrics index: %w", err)
		}
	}
	if needs {
		bctx, cancel := context.WithCancel(ctx)
		d.mu.Lock()
		d.endBuild, d.building, d.built = cancel, true, make(chan struct{})
		d.mu.Unlock()
		d.spawn(func() { d.build(bctx) })
		return nil
	}
	ix, err := d.cfg.OpenIndex(ctx)
	switch {
	case errors.Is(err, index.ErrUpdateInProgress):
	case err != nil:
		return fmt.Errorf("open the metrics index: %w", err)
	default:
		d.mu.Lock()
		d.ix = ix
		d.mu.Unlock()
	}
	return nil
}

// warm starts the doctor's first run, so the first page does not wait
// for it.
func (d *data) warm() {
	if d.src.Doctor != nil {
		go func() { _, _ = d.src.Doctor(d.life) }()
	}
}

// build is the first build: the index opened, then caught up within
// buildBudget. A catch-up cut short resumes at the next cache miss; an
// open that failed is tried again at the next request.
func (d *data) build(ctx context.Context) {
	defer close(d.built)
	ix, err := d.cfg.OpenIndex(ctx)
	if err == nil {
		_, err = ix.CatchUp(ctx, buildBudget)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.building = false
	if ix != nil {
		d.ix = ix
	}
	if errors.Is(err, index.ErrSchemaChanged) {
		d.updated = true
	}
}

// spawn runs fn detached, counted for stop; false once stopped.
func (d *data) spawn(fn func()) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.spawnLocked(fn)
}

func (d *data) spawnLocked(fn func()) bool {
	if d.stopped {
		return false
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		fn()
	}()
	return true
}

// stop cancels the first build and the detached work, waits for them and
// closes the index.
func (d *data) stop() error {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return nil
	}
	d.stopped = true
	endBuild := d.endBuild
	d.mu.Unlock()
	if endBuild != nil {
		endBuild()
	}
	d.endLife()
	d.wg.Wait()
	d.mu.Lock()
	ix := d.ix
	d.ix = nil
	d.mu.Unlock()
	if ix == nil {
		return nil
	}
	if err := ix.Close(); err != nil {
		return fmt.Errorf("close the metrics index: %w", err)
	}
	return nil
}

// setUpdated records that a newer backsight changed the index.
func (d *data) setUpdated() {
	d.mu.Lock()
	d.updated = true
	d.mu.Unlock()
}

// check is the index's state for a data request: the index opened when
// none is and no build runs, then its version checked. A broken state
// comes with the reported error (a fixed sentence).
func (d *data) check(ctx context.Context) (indexState, error) {
	if d.cfg.OpenIndex == nil {
		return stateBroken, views.Error(errNoIndex)
	}
	ix, st, err := d.current(ctx)
	if ix == nil {
		return st, err
	}
	if ix.Side() {
		d.setUpdated()
		return stateUpdated, nil
	}
	v, err := ix.FileVersion(ctx)
	if err != nil {
		return stateBroken, views.Error(err)
	}
	if v > ix.Schema() {
		d.setUpdated()
		return stateUpdated, nil
	}
	return stateReady, nil
}

// current is the open index, or nil and the state that stands for it. With
// no index and no build running, the index is opened: one attempt at a
// time, detached (a caller that leaves does not cancel it), each caller
// waiting for it or for its own ctx.
func (d *data) current(ctx context.Context) (Index, indexState, error) {
	d.mu.Lock()
	switch {
	case d.building:
		d.mu.Unlock()
		return nil, stateBuilding, nil
	case d.updated:
		d.mu.Unlock()
		return nil, stateUpdated, nil
	case d.ix != nil:
		ix := d.ix
		d.mu.Unlock()
		return ix, stateReady, nil
	}
	op := d.opening
	if op == nil {
		op = &opening{done: make(chan struct{})}
		if !d.spawnLocked(func() { d.open(op) }) {
			d.mu.Unlock()
			return nil, stateBroken, views.Error(context.Canceled)
		}
		d.opening = op
	}
	d.mu.Unlock()
	select {
	case <-op.done:
	case <-ctx.Done():
		return nil, stateBroken, views.Error(ctx.Err())
	}
	switch {
	case errors.Is(op.err, index.ErrUpdateInProgress):
		return nil, stateUpdating, nil
	case op.err != nil:
		return nil, stateBroken, views.Error(op.err)
	}
	return d.current(ctx)
}

// open is one open attempt, under the dashboard's own context.
func (d *data) open(op *opening) {
	ix, err := d.cfg.OpenIndex(d.life)
	d.mu.Lock()
	if err == nil {
		d.ix = ix
	}
	d.opening = nil
	op.err = err
	d.mu.Unlock()
	close(op.done)
}

// source is what a cache miss reads: the index caught up within
// index.ViewBudget. A newer schema met there is sticky.
func (d *data) source(ctx context.Context) (metrics.Source, error) {
	d.mu.Lock()
	ix := d.ix
	d.mu.Unlock()
	if ix == nil {
		return metrics.Source{}, errNoIndex
	}
	fr, err := ix.CatchUp(ctx, index.ViewBudget)
	if err != nil {
		if errors.Is(err, index.ErrSchemaChanged) {
			d.setUpdated()
		}
		return metrics.Source{}, fmt.Errorf("catch up the metrics index: %w", err)
	}
	src := d.src
	src.Index, src.Fresh = ix, fr
	return src, nil
}

// base is the source without a catch-up: the index as it is, its
// freshness unknown (the text page's).
func (d *data) base() metrics.Source {
	d.mu.Lock()
	ix := d.ix
	d.mu.Unlock()
	src := d.src
	if ix != nil {
		src.Index = ix
	}
	return src
}

// result is view's result for p and id, from the result cache: on a miss
// the index is caught up and the view computed, once for every caller of
// the key. A state other than ready comes without a result. Text is
// refused: the cache key ignores it, so a memory's text must never reach
// the cache.
func (d *data) result(ctx context.Context, view string, p views.Params, id string) (metrics.Result, indexState, error) {
	if p.Text {
		return metrics.Result{}, stateReady, views.Usage("a memory's text is read on its own page only")
	}
	// The parameters first, as the command line does: a request that can
	// never succeed is not told to retry.
	if _, err := views.Query(view, p, id, d.cfg.Now()); err != nil {
		return metrics.Result{}, stateReady, err
	}
	if st, err := d.check(ctx); st != stateReady {
		return metrics.Result{}, st, err
	}
	res, err := d.results.get(ctx, cacheKey(view, id, p), func(ctx context.Context) (metrics.Result, error) {
		src, err := d.source(ctx)
		if err != nil {
			return metrics.Result{}, err
		}
		q, err := views.Query(view, p, id, d.cfg.Now())
		if err != nil {
			return metrics.Result{}, err
		}
		return metrics.Run(ctx, view, src, q)
	})
	if errors.Is(err, index.ErrSchemaChanged) {
		return metrics.Result{}, stateUpdated, nil
	}
	return res, stateReady, err
}
