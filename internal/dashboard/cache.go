package dashboard

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// The caches' lifetimes and bounds; tests lower resultMax.
var (
	resultTTL   = 30 * time.Second // a view's result, shared by its page and its API
	resultMax   = 256              // results kept at most, the oldest dropped
	serverTTL   = 60 * time.Second // a successful server read
	doctorTTL   = 5 * time.Minute  // a doctor run
	doctorBound = 30 * time.Second // how long a doctor run may take
)

// renderTimeout bounds each authenticated request and each shared
// computation of a view.
var renderTimeout = 10 * time.Second

// onJoin runs when a caller joins a computation already running for its
// key (tests count the joins).
var onJoin = func() {}

// fresh reports that something kept at at is still good at now for ttl:
// a clock stepped back before at counts as expired.
func fresh(now, at time.Time, ttl time.Duration) bool {
	age := now.Sub(at)
	return age >= 0 && age < ttl
}

// cacheKey is the result cache's key: the view, the id and every
// parameter but Text (never cached) and DevSet (Dev alone decides), each
// quoted so no two sets of values read alike. It holds no time: a window
// of days is keyed by its length, not by the instants it spans.
func cacheKey(view, id string, p views.Params) string {
	return fmt.Sprintf("%q %q days=%d/%t since=%q until=%q tz=%q project=%q path=%q dev=%t limit=%d offset=%d has=%q sort=%q state=%q",
		view, id, p.Days, p.DaysSet, p.Since, p.Until, p.TZ, p.Project, p.Path, p.Dev, p.Limit, p.Offset, p.Has, p.Sort, p.State)
}

// resultCache keeps each view's result resultTTL and shares a computation
// among the callers of one key. The computation runs detached under base,
// bounded by renderTimeout: a caller that leaves (a closed tab, its own
// timeout) does not cancel it for the others. Errors are never kept.
type resultCache struct {
	now   func() time.Time
	base  context.Context
	spawn func(func()) bool // runs a detached computation; false once the dashboard stops

	mu      sync.Mutex
	seq     uint64 // insertion order: the oldest entry has the lowest
	entries map[string]resultEntry
	calls   map[string]*resultCall
}

type resultEntry struct {
	res metrics.Result
	at  time.Time
	seq uint64
}

// resultCall is a computation in flight; res and err are set before done
// is closed.
type resultCall struct {
	done chan struct{}
	res  metrics.Result
	err  error
}

// get is key's result: kept, being computed (it waits for that), or
// computed now by compute. Each caller waits at most for its own ctx.
func (c *resultCache) get(ctx context.Context, key string, compute func(context.Context) (metrics.Result, error)) (metrics.Result, error) {
	c.mu.Lock()
	now := c.now()
	if e, ok := c.entries[key]; ok && fresh(now, e.at, resultTTL) {
		c.mu.Unlock()
		return e.res, nil
	}
	call, joined := c.calls[key]
	if !joined {
		call = &resultCall{done: make(chan struct{})}
		if !c.spawn(func() { c.run(key, call, now, compute) }) {
			c.mu.Unlock()
			return metrics.Result{}, context.Canceled
		}
		if c.calls == nil {
			c.calls = map[string]*resultCall{}
		}
		c.calls[key] = call
	}
	c.mu.Unlock()
	if joined {
		onJoin()
	}
	select {
	case <-call.done:
		return call.res, call.err
	case <-ctx.Done():
		return metrics.Result{}, ctx.Err()
	}
}

// run computes key's result, keeps it when it succeeded (as of at, the
// miss) and hands it to every caller waiting.
func (c *resultCache) run(key string, call *resultCall, at time.Time, compute func(context.Context) (metrics.Result, error)) {
	ctx, cancel := context.WithTimeout(c.base, renderTimeout)
	call.res, call.err = compute(ctx)
	cancel()
	c.mu.Lock()
	delete(c.calls, key)
	if call.err == nil {
		c.store(key, call.res, at)
	}
	c.mu.Unlock()
	close(call.done)
}

// store keeps res under key: the expired entries are swept first, then the
// oldest dropped while the cache is full. c.mu is held.
func (c *resultCache) store(key string, res metrics.Result, at time.Time) {
	if c.entries == nil {
		c.entries = map[string]resultEntry{}
	}
	now := c.now()
	for k, e := range c.entries {
		if !fresh(now, e.at, resultTTL) {
			delete(c.entries, k)
		}
	}
	delete(c.entries, key)
	for len(c.entries) >= resultMax && len(c.entries) > 0 {
		oldest, first := "", true
		for k, e := range c.entries {
			if first || e.seq < c.entries[oldest].seq {
				oldest, first = k, false
			}
		}
		delete(c.entries, oldest)
	}
	c.seq++
	c.entries[key] = resultEntry{res: res, at: at, seq: c.seq}
}

// cachedServer keeps each successful server read serverTTL, per method and
// arguments; a failed read is never kept. Memory is never cached: it
// carries the memory's text.
type cachedServer struct {
	next metrics.ServerReader
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]serverEntry
}

var _ metrics.ServerReader = (*cachedServer)(nil)

type serverEntry struct {
	v  any
	at time.Time
}

// cachedRead is key's kept answer, or read's, kept when it succeeds.
func cachedRead[T any](c *cachedServer, ctx context.Context, key string, read func(context.Context) (T, error)) (T, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && fresh(now, e.at, serverTTL) {
		c.mu.Unlock()
		return e.v.(T), nil
	}
	c.mu.Unlock()
	v, err := read(ctx)
	if err != nil {
		return v, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]serverEntry{}
	}
	for k, e := range c.entries {
		if !fresh(now, e.at, serverTTL) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = serverEntry{v: v, at: now}
	return v, nil
}

func (c *cachedServer) Version(ctx context.Context) (hindsight.Version, error) {
	return cachedRead(c, ctx, "version", c.next.Version)
}

func (c *cachedServer) BankStats(ctx context.Context) (hindsight.BankStats, error) {
	return cachedRead(c, ctx, "bank_stats", c.next.BankStats)
}

// Memory is never cached: it carries the memory's text.
func (c *cachedServer) Memory(ctx context.Context, id string) (hindsight.MemoryDetail, error) {
	return c.next.Memory(ctx, id)
}

func (c *cachedServer) ProjectTags(ctx context.Context) ([]hindsight.TagCount, error) {
	return cachedRead(c, ctx, "project_tags", c.next.ProjectTags)
}

func (c *cachedServer) MemoriesTimeseries(ctx context.Context, period string) (hindsight.Timeseries, error) {
	return cachedRead(c, ctx, "memories_timeseries "+strconv.Quote(period), func(ctx context.Context) (hindsight.Timeseries, error) {
		return c.next.MemoriesTimeseries(ctx, period)
	})
}

func (c *cachedServer) LLMStats(ctx context.Context, period, operation string) ([]hindsight.LLMBucket, error) {
	return cachedRead(c, ctx, fmt.Sprintf("llm_stats %q %q", period, operation), func(ctx context.Context) ([]hindsight.LLMBucket, error) {
		return c.next.LLMStats(ctx, period, operation)
	})
}

func (c *cachedServer) OperationsPage(ctx context.Context, status, typ string, limit, offset int) (hindsight.OperationPage, error) {
	return cachedRead(c, ctx, fmt.Sprintf("operations_page %q %q %d %d", status, typ, limit, offset), func(ctx context.Context) (hindsight.OperationPage, error) {
		return c.next.OperationsPage(ctx, status, typ, limit, offset)
	})
}

// cachedDoctor wraps the doctor run: one run at a time, under base bounded
// by doctorBound (a closed tab does not stop it), its checks kept
// doctorTTL; a failed run is never kept. A caller waits for the run or for
// its own ctx. A run that overstays its bound (a probe ignoring its
// context) is abandoned as failed.
func cachedDoctor(base context.Context, run func(context.Context) ([]metrics.DoctorCheck, error), now func() time.Time) func(context.Context) ([]metrics.DoctorCheck, error) {
	type doctorCall struct {
		done   chan struct{}
		checks []metrics.DoctorCheck
		err    error
	}
	var (
		mu      sync.Mutex
		kept    []metrics.DoctorCheck
		keptAt  time.Time
		have    bool
		running *doctorCall
	)
	start := func(at time.Time) *doctorCall {
		call := &doctorCall{done: make(chan struct{})}
		go func() {
			ctx, cancel := context.WithTimeout(base, doctorBound)
			defer cancel()
			type outcome struct {
				checks []metrics.DoctorCheck
				err    error
			}
			ran := make(chan outcome, 1) // an abandoned run still sends, to no one
			go func() {
				checks, err := run(ctx)
				ran <- outcome{checks, err}
			}()
			var o outcome
			select {
			case o = <-ran:
			case <-ctx.Done():
				select {
				case o = <-ran: // it ended as its context did
				default:
					o.err = ctx.Err()
				}
			}
			checks, err := o.checks, o.err
			mu.Lock()
			running = nil
			if err == nil {
				kept, keptAt, have = checks, at, true
			}
			mu.Unlock()
			call.checks, call.err = checks, err
			close(call.done)
		}()
		return call
	}
	return func(ctx context.Context) ([]metrics.DoctorCheck, error) {
		mu.Lock()
		at := now()
		if have && fresh(at, keptAt, doctorTTL) {
			out := slices.Clone(kept)
			mu.Unlock()
			return out, nil
		}
		if running == nil {
			running = start(at)
		}
		call := running
		mu.Unlock()
		select {
		case <-call.done:
			return slices.Clone(call.checks), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
