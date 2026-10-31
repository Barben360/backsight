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
// llm:<op>); a key absent from it was read or not asked for. State is the
// server section of the view (nil when the view reads nothing from the
// server); withTx sets its LastOK.
type serverData struct {
	Stats               hindsight.BankStats
	Series              hindsight.Timeseries
	Tags                []hindsight.TagCount
	Version             hindsight.Version
	Pending, Processing int
	Failed              []hindsight.Operation
	Completed           []hindsight.Operation
	LLM                 map[string][]hindsight.LLMBucket // key: operation ("" = all)
	Doctor              []DoctorCheck
	DoctorErr           error
	Err                 map[string]error
	State               *ServerState
}

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
// It waits for every call: a call must honour its context.
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
			if err := call(cctx); err != nil {
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
// doctor beside them under doctorTimeout. The periods cover the window
// from its start to now. The state is nil when need names no server read;
// without a server every read fails as not configured. The state's OK is
// false when every read failed; its Error is the first failure's enum in
// key order.
func loadServer(ctx context.Context, src Source, w Window, need []string) (*serverData, *ServerState) {
	d := &serverData{LLM: map[string][]hindsight.LLMBucket{}, Err: map[string]error{}}
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
		}
		return d, nil
	}

	span := max(src.now().Sub(w.Since), 0)
	llm := map[string]*[]hindsight.LLMBucket{}
	calls := map[string]func(context.Context) error{}
	s := src.Server
	for _, key := range keys {
		if s == nil {
			d.Err[key] = errNoServer
			continue
		}
		switch key {
		case "stats":
			calls[key] = func(ctx context.Context) (err error) { d.Stats, err = s.BankStats(ctx); return err }
		case "series":
			calls[key] = func(ctx context.Context) (err error) {
				d.Series, err = s.MemoriesTimeseries(ctx, seriesPeriod(span))
				return err
			}
		case "tags":
			calls[key] = func(ctx context.Context) (err error) { d.Tags, err = s.ProjectTags(ctx); return err }
		case "version":
			calls[key] = func(ctx context.Context) (err error) { d.Version, err = s.Version(ctx); return err }
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
				d.Pending, d.Processing = p.Total, q.Total
				return nil
			}
		case "failed":
			calls[key] = func(ctx context.Context) (err error) {
				d.Failed, err = operations(ctx, s, "failed", "", failedPages)
				return err
			}
		case "completed":
			calls[key] = func(ctx context.Context) (err error) {
				d.Completed, err = operations(ctx, s, "completed", "retain", completedPages)
				return err
			}
		default:
			op, ok := strings.CutPrefix(key, "llm:")
			if !ok {
				d.Err[key] = fmt.Errorf("%w %q", errUnknownRead, key)
				continue
			}
			slot := new([]hindsight.LLMBucket)
			llm[op] = slot
			calls[key] = func(ctx context.Context) (err error) {
				*slot, err = s.LLMStats(ctx, llmPeriod(span), op)
				return err
			}
		}
	}
	for key, err := range fetchParallel(ctx, ServerTimeout, calls) {
		d.Err[key] = err
	}
	for op, slot := range llm {
		if d.Err["llm:"+op] == nil {
			d.LLM[op] = *slot
		}
	}
	if doctorDone != nil {
		<-doctorDone
	}

	st := &ServerState{OK: len(d.Err) < len(keys)}
	for _, key := range keys { // sorted
		if err := d.Err[key]; err != nil {
			st.Error, _ = serverError(err)
			break
		}
	}
	d.State = st
	return d, st
}

// operations reads up to pages pages of the operations of status and typ,
// most recent first, stopping at the last one.
func operations(ctx context.Context, s ServerReader, status, typ string, pages int) ([]hindsight.Operation, error) {
	var out []hindsight.Operation
	for k := range pages {
		p, err := s.OperationsPage(ctx, status, typ, opsPage, k*opsPage)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Operations...)
		if len(p.Operations) < opsPage || (k+1)*opsPage >= p.Total {
			break
		}
	}
	return out, nil
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
		return "not_configured", ReasonServerDown
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
