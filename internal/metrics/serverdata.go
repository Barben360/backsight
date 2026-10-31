package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// serverData is what one view needs from the server, fetched once, in
// parallel, before the read transaction. Err holds the reads that failed,
// by endpoint key (stats, series, tags, version, queue, failed, completed,
// llm:<op>); a key absent from it was read or not asked for.
type serverData struct {
	Stats               hindsight.BankStats
	Series              hindsight.Timeseries
	Tags                []hindsight.TagCount
	Version             hindsight.Version
	Pending, Processing int
	Failed              []hindsight.Operation // newest first (sorted by updated time on read)
	FailedTotal         int                   // the server's exact count of failed operations; Failed holds at most failedPages pages of them
	Completed           []hindsight.Operation
	LLM                 map[string][]hindsight.LLMBucket // key: operation ("" = all), over the window's period
	LLM30               []hindsight.LLMBucket            // all operations over 30d whatever the window (llm30d:)
	Memory              hindsight.MemoryDetail           // memory:<id>
	Doctor              []DoctorCheck
	DoctorErr           error
	Err                 map[string]error
	Have                map[string]bool // the keys read successfully; a key in neither Have nor Err was not asked for
}

// memoryKeyPrefix starts the server read of one memory: memory:<id>.
const memoryKeyPrefix = "memory:"

// llm30dKey is the server read of the LLM stats over 30 days, all
// operations, whatever the view's window (extraction.per_session needs 28
// days of them under a 7-day view).
const llm30dKey = "llm30d:"

// Paging of the operation reads, opsPage operations a page: the failed
// operations up to failedPages pages, the completed retains up to
// completedPages.
const (
	opsPage        = 100
	failedPages    = 5
	completedPages = 2
)

// errNoServer stands for every read of a view when no server is configured.
var errNoServer = errors.New("no server is configured")

// errNoDoctor is the doctor's error when the view has no doctor run.
var errNoDoctor = errors.New("no doctor run")

// errUnknownRead is a server read key no endpoint answers (a programming
// error, read as a failed call).
var errUnknownRead = errors.New("unknown server read")

// fetchParallel runs every call at once, each under its own context
// bounded by timeout, and returns the errors of those that failed, by key.
// A call still running when its context ends is abandoned with that
// context's error (a ServerReader that ignores its context cannot hold the
// view): its results must not be used after fetchParallel returns.
func fetchParallel(ctx context.Context, timeout time.Duration, calls map[string]func(context.Context) error) map[string]error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs = map[string]error{}
	)
	for key, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			done := make(chan error, 1) // an abandoned call still sends, to no one
			go func() { done <- call(cctx) }()
			var err error
			select {
			case err = <-done:
			case <-cctx.Done():
				select {
				case err = <-done: // it ended as its context did
				default:
					err = cctx.Err()
				}
			}
			if err != nil {
				mu.Lock()
				errs[key] = err
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errs
}

// seriesPeriod is the shortest memories-timeseries period covering span.
func seriesPeriod(span time.Duration) string {
	switch {
	case span <= day:
		return "1d"
	case span <= 7*day:
		return "7d"
	case span <= 30*day:
		return "30d"
	}
	return "90d"
}

// viewSpan is the span the server's periods must cover for w, read at now.
func viewSpan(now time.Time, w Window) time.Duration {
	return max(w.Length(), now.Sub(w.Since)-time.Minute)
}

// llmPeriod is the shortest LLM-stats period covering span (30d at most).
func llmPeriod(span time.Duration) string {
	switch {
	case span <= day:
		return "1d"
	case span <= 7*day:
		return "7d"
	}
	return "30d"
}

// loadServer reads what need names (endpoint keys, plus "doctor" for the
// live doctor run), every read in parallel under ServerTimeout and the
// doctor beside them under doctorTimeout. The server's periods count back
// from now, so they cover the window back to its start, less a minute for
// the clock moving on since the window was built (a 7-day window built a
// moment ago keeps the 7-day period), and at least the window's length. The state is nil when need names no server read; without a
// server every read fails as not configured. The state's OK is false when
// every read failed, and then its Error is the first failure's enum in key
// order; a partial failure shows only in its metrics' reasons. A read is
// kept only when it ends in time: a late one is dropped.
func loadServer(ctx context.Context, src Source, w Window, need []string) (*serverData, *ServerState) {
	d := &serverData{LLM: map[string][]hindsight.LLMBucket{}, Err: map[string]error{}, Have: map[string]bool{}}
	keys := slices.Clone(need)
	slices.Sort(keys)
	keys = slices.Compact(keys)

	var doctorDone chan struct{}
	if i := slices.Index(keys, "doctor"); i >= 0 {
		keys = slices.Delete(keys, i, i+1)
		doctorDone = make(chan struct{})
		go func() {
			defer close(doctorDone)
			d.Doctor, d.DoctorErr = runDoctor(ctx, src.Doctor)
		}()
	}
	if len(keys) == 0 {
		if doctorDone != nil {
			<-doctorDone
			d.Have["doctor"] = d.DoctorErr == nil
		}
		return d, nil
	}

	span := viewSpan(src.now(), w)
	var (
		kmu    sync.Mutex
		closed bool
	)
	// keep stores a read's result unless fetchParallel already gave up on it.
	keep := func(err error, set func()) error {
		if err != nil {
			return err
		}
		kmu.Lock()
		defer kmu.Unlock()
		if !closed {
			set()
		}
		return nil
	}
	llm := map[string]*[]hindsight.LLMBucket{}
	var llm30 []hindsight.LLMBucket
	// Over 7 days the stats of all operations are read for 30 days, the
	// very request of llm30d:: asked for both, the server is asked once.
	sameLLM := slices.Contains(keys, "llm:") && slices.Contains(keys, llm30dKey) && llmPeriod(span) == "30d"
	calls := map[string]func(context.Context) error{}
	s := src.Server
	for _, key := range keys {
		if s == nil {
			d.Err[key] = errNoServer
			continue
		}
		switch key {
		case "stats":
			calls[key] = func(ctx context.Context) error {
				v, err := s.BankStats(ctx)
				return keep(err, func() { d.Stats = v })
			}
		case "series":
			calls[key] = func(ctx context.Context) error {
				v, err := s.MemoriesTimeseries(ctx, seriesPeriod(span))
				return keep(err, func() { d.Series = v })
			}
		case "tags":
			calls[key] = func(ctx context.Context) error {
				v, err := s.ProjectTags(ctx)
				return keep(err, func() { d.Tags = v })
			}
		case "version":
			calls[key] = func(ctx context.Context) error {
				v, err := s.Version(ctx)
				return keep(err, func() { d.Version = v })
			}
		case "queue":
			calls[key] = func(ctx context.Context) error {
				p, err := s.OperationsPage(ctx, "pending", "", 1, 0)
				if err != nil {
					return err
				}
				q, err := s.OperationsPage(ctx, "processing", "", 1, 0)
				if err != nil {
					return err
				}
				return keep(nil, func() { d.Pending, d.Processing = p.Total, q.Total })
			}
		case "failed":
			calls[key] = func(ctx context.Context) error {
				v, total, err := operations(ctx, s, "failed", "", failedPages)
				slices.SortStableFunc(v, func(a, b hindsight.Operation) int { return opTime(b).Compare(opTime(a)) })
				return keep(err, func() { d.Failed, d.FailedTotal = v, total })
			}
		case "completed":
			calls[key] = func(ctx context.Context) error {
				v, _, err := operations(ctx, s, "completed", "retain", completedPages)
				return keep(err, func() { d.Completed = v })
			}
		case llm30dKey:
			calls[key] = func(ctx context.Context) error {
				v, err := s.LLMStats(ctx, "30d", "")
				return keep(err, func() { llm30 = v })
			}
		default:
			if id, ok := strings.CutPrefix(key, memoryKeyPrefix); ok {
				calls[key] = func(ctx context.Context) error {
					v, err := s.Memory(ctx, id)
					return keep(err, func() { d.Memory = v })
				}
				continue
			}
			op, ok := strings.CutPrefix(key, "llm:")
			if !ok {
				d.Err[key] = fmt.Errorf("%w %q", errUnknownRead, key)
				continue
			}
			if op == "" && sameLLM {
				continue // read as llm30d:, copied below
			}
			slot := new([]hindsight.LLMBucket)
			llm[op] = slot
			calls[key] = func(ctx context.Context) error {
				v, err := s.LLMStats(ctx, llmPeriod(span), op)
				return keep(err, func() { *slot = v })
			}
		}
	}
	for key, err := range fetchParallel(ctx, ServerTimeout, calls) {
		d.Err[key] = err
	}
	for key := range calls {
		d.Have[key] = d.Err[key] == nil
	}
	kmu.Lock()
	closed = true
	if _, asked := calls[llm30dKey]; asked && d.Err[llm30dKey] == nil {
		d.LLM30 = llm30
	}
	kmu.Unlock()
	for op, slot := range llm {
		if d.Err["llm:"+op] == nil {
			d.LLM[op] = *slot
		}
	}
	if sameLLM && s != nil {
		if err := d.Err[llm30dKey]; err != nil {
			d.Err["llm:"] = err
		} else {
			d.LLM[""] = d.LLM30
		}
		d.Have["llm:"] = d.Have[llm30dKey]
	}
	if doctorDone != nil {
		<-doctorDone
		d.Have["doctor"] = d.DoctorErr == nil
	}

	st := &ServerState{OK: len(d.Err) < len(keys)}
	for _, key := range keys { // sorted
		if err := d.Err[key]; err != nil && !st.OK {
			st.Error, _ = serverError(err)
			break
		}
	}
	return d, st
}

// opTime is when an operation last changed: its update time, else its
// creation time.
func opTime(o hindsight.Operation) time.Time {
	if o.UpdatedAt.IsZero() {
		return o.CreatedAt
	}
	return o.UpdatedAt
}

// operations reads up to pages pages of the operations of status and typ,
// stopping at the last one, and returns them with the server's exact total.
// The server lists by created time, newest first (confirmed live), not by
// updated time: the pages kept are the latest created operations, so an
// older one updated recently can fall outside a capped read. Readers sort
// by updated time, and a count of the last 7 days from a capped read is a
// lower bound (failed_capped). Past the cap the total is larger than what
// was read.
func operations(ctx context.Context, s ServerReader, status, typ string, pages int) ([]hindsight.Operation, int, error) {
	var out []hindsight.Operation
	total := 0
	for k := range pages {
		p, err := s.OperationsPage(ctx, status, typ, opsPage, k*opsPage)
		if err != nil {
			return nil, 0, err
		}
		total = p.Total
		out = append(out, p.Operations...)
		if len(p.Operations) < opsPage || (k+1)*opsPage >= p.Total {
			break
		}
	}
	return out, total, nil
}

// runDoctor runs the live doctor under doctorTimeout and abandons it past
// the limit: its probes may not honour the context, so the result is read
// from a channel beside the timer. A run abandoned or absent is an error.
func runDoctor(ctx context.Context, doctor func(context.Context) ([]DoctorCheck, error)) ([]DoctorCheck, error) {
	if doctor == nil {
		return nil, errNoDoctor
	}
	dctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	type result struct {
		checks []DoctorCheck
		err    error
	}
	done := make(chan result, 1) // an abandoned run still sends, to no one
	go func() {
		checks, err := doctor(dctx)
		done <- result{checks, err}
	}()
	select {
	case r := <-done:
		return r.checks, r.err
	case <-dctx.Done():
		return nil, dctx.Err()
	}
}

// serverError classifies a failed server read: the enum of the view's
// server section and the no_data reason of its metrics. nil is "".
func serverError(err error) (enum string, reason string) {
	var (
		api    *hindsight.APIError
		syntax *json.SyntaxError
		typ    *json.UnmarshalTypeError
	)
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, errNoServer):
		return "not_configured", ReasonNotConfigured
	case errors.Is(err, hindsight.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout", ReasonServerDown
	case errors.Is(err, hindsight.ErrUnreachable):
		return "unreachable", ReasonServerDown
	case errors.As(err, &api):
		switch api.Kind {
		case hindsight.Unauthorized:
			return "unauthorized", ReasonServerDown
		case hindsight.NotFound:
			return "not_found", ReasonServerUnsupported
		}
		return "server_error", ReasonServerDown
	case errors.As(err, &syntax), errors.As(err, &typ):
		return "bad_response", ReasonServerUnsupported
	}
	return "server_error", ReasonServerDown
}

// serverReason is the no_data reason of a read of key: the failure's, or
// server_unsupported when the view did not ask for it (its data would be
// an invented zero); "" when it succeeded. Without a server, every read
// failed.
func serverReason(e env, key string) string {
	if e.srv == nil {
		_, reason := serverError(errNoServer)
		return reason
	}
	if err := e.srv.Err[key]; err != nil {
		_, reason := serverError(err)
		return reason
	}
	if !e.srv.Have[key] {
		return ReasonServerUnsupported
	}
	return ""
}
