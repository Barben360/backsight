package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The shared fixture of the metric tests: a state directory filled through
// the producers' formats (telemetry lines as the writer writes them,
// journal events through feedback.Journal.Append), the ledger and the
// local status handed to the index's own readers, caught up by the real
// index, and the server as an httptest fake behind the real read-only
// client. Everything is fixed: the clock (newFx's now), the zone
// (Europe/Paris), the ids the tests give, the server host and bank.

// fxParis is the fixtures' zone.
var fxParis = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		panic(err)
	}
	return loc
}()

// fxNow is the clock most metric tests use: 2026-10-30T12:00+01:00.
var fxNow = time.Date(2026, 10, 30, 12, 0, 0, 0, fxParis)

const (
	fxBank   = "backsight"
	fxHost   = "127.0.0.1:18888"
	fxBinary = "0.1.18"
)

// fxNeed is every server read fx.metrics makes by default (the LLM stats
// of all operations; a test adds "llm:<op>" for one operation).
var fxNeed = []string{"stats", "series", "tags", "version", "queue", "failed", "completed", "llm:", "doctor"}

// fx is one fixture: write events, then read metrics.
type fx struct {
	t      *testing.T
	now    time.Time
	home   string
	dirs   paths.Dirs
	binary string // binary_version of the lines and events written next

	ledgerSet *feedback.Ledger     // nil: the ledger the journal folds to
	localSet  *index.LocalStatus   // nil: an empty local status
	localFail bool                 // the local status is unreadable: the index holds no row
	server    *fakeServer          // nil: no server configured
	doctor    []DoctorCheck        // nil: no doctor run
	need      []string             // the server reads of fx.metrics
	ix        *index.Index         // opened by the first source()
	cfg       func(*config.Config) // edits the default configuration
}

// newFx is an empty fixture at now (read in Europe/Paris): its own state
// under BACKSIGHT_HOME, a fake server with nothing in it, no doctor run.
func newFx(t *testing.T, now time.Time) *fx {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BACKSIGHT_HOME", home)
	t.Setenv("BACKSIGHT_STATE_DIR", "")
	dirs, err := paths.Resolve(runtime.GOOS, home, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	return &fx{t: t, now: now.In(fxParis), home: home, dirs: dirs, binary: fxBinary,
		server: newFakeServer(t), need: slices.Clone(fxNeed)}
}

// window is the last days days before the fixture's now, in Paris.
func (x *fx) window(days int) Window {
	x.t.Helper()
	w, err := NewWindow(x.now, days, time.Time{}, time.Time{}, fxParis, "Europe/Paris")
	if err != nil {
		x.t.Fatal(err)
	}
	return w
}

// query is a query of the last days days, this PC.
func (x *fx) query(days int) Query { return Query{Window: x.window(days), Machines: "this"} }

// tel appends ev at ts to the telemetry month file, as telemetry.Writer
// writes it: the event's fields, then ts (UTC, milliseconds), v, event,
// session, project and binary_version (TestFixtureTelIsTheWritersLine
// holds the two together).
func (x *fx) tel(ts time.Time, ev telemetry.Event, session, project string) {
	x.t.Helper()
	fields := map[string]any{}
	if r, ok := ev.(telemetry.Raw); ok {
		for k, v := range r.Fields {
			fields[k] = v
		}
	} else {
		raw, err := json.Marshal(ev)
		if err != nil {
			x.t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			x.t.Fatal(err)
		}
	}
	fields["ts"] = ts.UTC().Format("2006-01-02T15:04:05.000Z")
	fields["v"] = instructions.Version
	fields["event"] = ev.EventName()
	fields["session"] = session
	fields["project"] = project
	fields["binary_version"] = x.binary
	line, err := json.Marshal(fields)
	if err != nil {
		x.t.Fatal(err)
	}
	dir := x.dirs.TelemetryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		x.t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "events-"+ts.UTC().Format("2006-01")+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		x.t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		x.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		x.t.Fatal(err)
	}
}

// journal appends e through the journal's own writer, in the month file
// of e.TS (which must be set: the fixture has no wall clock).
func (x *fx) journal(e feedback.Event) {
	x.t.Helper()
	if e.TS.IsZero() {
		x.t.Fatal("fx.journal: set the event's TS")
	}
	j := feedback.NewJournal(x.dirs.FeedbackDir(), feedback.Common{Binary: x.binary, V: instructions.Version})
	if err := j.Append(e); err != nil {
		x.t.Fatal(err)
	}
}

// ledger is the ledger the index reads from now on (instead of the one
// the journal folds to).
func (x *fx) ledger(l feedback.Ledger) { x.ledgerSet = &l }

// local is the local status the index reads from now on.
func (x *fx) local(ls index.LocalStatus) { x.localSet = &ls }

// fxLedger hands the index the fixture's ledger, else the producer's.
type fxLedger struct{ x *fx }

func (r fxLedger) All() (feedback.Ledger, error) {
	if r.x.ledgerSet != nil {
		return *r.x.ledgerSet, nil
	}
	return feedback.CurrentReader{Dir: r.x.dirs.FeedbackDir()}.All()
}

// source opens the index on the fixture's state (once), catches it up on
// what was written since, and returns the view's source: the default
// configuration with feedback on, the fake server, the doctor checks.
func (x *fx) source() Source {
	x.t.Helper()
	ctx := context.Background()
	if x.ix == nil {
		ix, err := index.Open(ctx, index.Options{
			Dirs: x.dirs, Home: x.home, Getenv: func(string) string { return "" },
			Now: func() time.Time { return x.now }, Ledger: fxLedger{x},
			LocalStatus: func() (index.LocalStatus, error) {
				if x.localFail {
					return index.LocalStatus{}, errors.New("fake: unreadable")
				}
				if x.localSet != nil {
					return *x.localSet, nil
				}
				return index.LocalStatus{}, nil
			},
		})
		if err != nil {
			x.t.Fatal(err)
		}
		x.t.Cleanup(func() { _ = ix.Close() })
		x.ix = ix
	}
	fr, err := x.ix.CatchUp(ctx, time.Minute)
	if err != nil {
		x.t.Fatal(err)
	}
	cfg := config.Default(x.dirs, x.home)
	cfg.Feedback.Enabled = true
	if x.cfg != nil {
		x.cfg(&cfg)
	}
	now := x.now
	src := Source{Index: x.ix, Fresh: fr, Now: func() time.Time { return now }, Config: cfg, Configured: true, ServerHost: fxHost}
	if x.server != nil {
		src.Server = x.server
	}
	if x.doctor != nil {
		checks := slices.Clone(x.doctor)
		src.Doctor = func(context.Context) ([]DoctorCheck, error) { return checks, nil }
	}
	return src
}

// metrics computes ids for q through withTx and computeMetrics, the path
// of every view, with the server reads of x.need.
func (x *fx) metrics(ids []string, q Query) map[string]Metric {
	x.t.Helper()
	var out map[string]Metric
	err := withTx(context.Background(), x.source(), q, x.need, func(e env) error {
		var err error
		out, err = computeMetrics(e, q.Window, ids)
		return err
	})
	if err != nil {
		x.t.Fatal(err)
	}
	return out
}

// run computes view for q through Run on the fixture's source, the path
// of `stats <view>` and of the dashboard.
func (x *fx) run(view string, q Query) Result {
	x.t.Helper()
	r, err := Run(context.Background(), view, x.source(), q)
	if err != nil {
		x.t.Fatalf("%s: %v", view, err)
	}
	return r
}

// pageViews are the views of this package's page functions.
var pageViews = []string{"overview", "quality", "cost", "bank", "pipeline", "system"}

// lowerTimeouts sets ServerTimeout and doctorTimeout for one test.
func lowerTimeouts(t *testing.T, server, doctor time.Duration) {
	t.Helper()
	oldS, oldD := ServerTimeout, doctorTimeout
	ServerTimeout, doctorTimeout = server, doctor
	t.Cleanup(func() { ServerTimeout, doctorTimeout = oldS, oldD })
}

// shortRead gives the server read of key a 10 ms timeout and the others a
// minute, for the test: only the read meant to time out does, however slow
// the runner.
func shortRead(t *testing.T, key string) {
	t.Helper()
	old := readTimeout
	readTimeout = shortFor(key)
	t.Cleanup(func() { readTimeout = old })
}

// fakeData is what the fake server answers, in the client's types.
type fakeData struct {
	Version  hindsight.Version
	Stats    hindsight.BankStats
	StatsRaw string // when set, the /stats body as is (Stats ignored)
	Memories map[string]hindsight.MemoryDetail
	Tags     []hindsight.TagCount // every tag of the bank; ProjectTags keeps "project:"
	Series   hindsight.Timeseries
	LLM      map[string][]hindsight.LLMBucket // by operation, "" = all
	Ops      []hindsight.Operation            // most recent first; filtered and paged by the fake
}

// errBadBody makes a fake method answer 200 with a body that is not JSON.
var errBadBody = errors.New("fake: a body that does not decode")

// fakeServer is an httptest server speaking the server's JSON shapes,
// read through the real *hindsight.ReadOnly it embeds (so it is a
// ServerReader and every read decodes as in production). Each method,
// named as ReadOnly's, can fail (fail) or block until its call's context
// ends (block). set changes the answers.
type fakeServer struct {
	*hindsight.ReadOnly
	done chan struct{}

	mu     sync.Mutex
	data   fakeData
	errs   map[string]error
	blocks map[string]bool
	calls  []string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	s := &fakeServer{done: make(chan struct{}), errs: map[string]error{}, blocks: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(s.done) }) // runs first: blocked handlers end
	ro, err := hindsight.NewReadOnly(srv.URL, "fake-token", fxBank, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s.ReadOnly = ro
	return s
}

// set edits the fake's answers.
func (s *fakeServer) set(f func(*fakeData)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.data)
}

// fail makes method fail as err says: hindsight.ErrUnreachable closes the
// connection, hindsight.ErrTimeout blocks, an *hindsight.APIError answers
// its kind's status, errBadBody a body that does not decode; nil clears.
func (s *fakeServer) fail(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[method] = err
}

// failAll makes every method fail with err.
func (s *fakeServer) failAll(err error) {
	for _, m := range fakeMethods {
		s.fail(m, err)
	}
}

// block makes method wait for its call's context to end.
func (s *fakeServer) block(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks[method] = true
}

// called is every call so far, as "Method?query" (query sorted).
func (s *fakeServer) called() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

var fakeMethods = []string{"Version", "BankStats", "Memory", "ProjectTags", "MemoriesTimeseries", "LLMStats", "OperationsPage"}

var kindStatus = map[hindsight.Kind]int{
	hindsight.Unauthorized: http.StatusUnauthorized, hindsight.NotFound: http.StatusNotFound,
	hindsight.BadRequest: http.StatusBadRequest, hindsight.Unavailable: http.StatusServiceUnavailable,
	hindsight.ServerError: http.StatusInternalServerError, hindsight.RateLimited: http.StatusTooManyRequests,
	hindsight.Redirected: http.StatusFound,
}

func (s *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	bp := "/v1/default/banks/" + fxBank
	p := r.URL.Path
	var method string
	switch {
	case p == "/version":
		method = "Version"
	case p == bp+"/stats":
		method = "BankStats"
	case p == bp+"/stats/memories-timeseries":
		method = "MemoriesTimeseries"
	case p == bp+"/llm-requests/stats":
		method = "LLMStats"
	case p == bp+"/operations":
		method = "OperationsPage"
	case p == bp+"/tags":
		method = "ProjectTags"
	case strings.HasPrefix(p, bp+"/memories/"):
		method = "Memory"
	default:
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	s.mu.Lock()
	s.calls = append(s.calls, method+"?"+q.Encode())
	err, blocked, d := s.errs[method], s.blocks[method], s.data
	if err == nil && method == "LLMStats" { // fail("LLMStats:<operation>") fails one operation
		err = s.errs["LLMStats:"+q.Get("operation")]
	}
	s.mu.Unlock()

	var api *hindsight.APIError
	switch {
	case blocked || errors.Is(err, hindsight.ErrTimeout):
		select {
		case <-r.Context().Done():
		case <-s.done:
		}
		return
	case errors.Is(err, hindsight.ErrUnreachable):
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, herr := hj.Hijack(); herr == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	case errors.As(err, &api):
		w.WriteHeader(kindStatus[api.Kind])
		_, _ = w.Write([]byte(`{"detail":"fake"}`))
		return
	case errors.Is(err, errBadBody):
		_, _ = w.Write([]byte(`{"total": "not json`))
		return
	case err != nil:
		panic(fmt.Sprintf("fake server: no fault for %v", err))
	}

	var body any
	switch method {
	case "Version":
		body = d.Version
	case "BankStats":
		if d.StatsRaw != "" {
			_, _ = w.Write([]byte(d.StatsRaw))
			return
		}
		body = d.Stats
	case "Memory":
		m, ok := d.Memories[strings.TrimPrefix(p, bp+"/memories/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body = m
	case "ProjectTags":
		prefix := strings.TrimSuffix(q.Get("q"), "*")
		var items []hindsight.TagCount
		for _, tc := range d.Tags {
			if strings.HasPrefix(tc.Tag, prefix) {
				items = append(items, tc)
			}
		}
		page := pageOf(items, q)
		body = map[string]any{"items": nonNil(page), "total": len(items)}
	case "MemoriesTimeseries":
		period := d.Series.Period
		if period == "" {
			period = q.Get("period")
		}
		buckets := []map[string]any{}
		for _, b := range d.Series.Buckets {
			buckets = append(buckets, map[string]any{"time": b.Time.UTC().Format(time.RFC3339),
				"world": b.World, "experience": b.Experience, "observation": b.Observation})
		}
		body = map[string]any{"bank_id": fxBank, "period": period, "trunc": d.Series.Trunc, "time_field": "created_at", "buckets": buckets}
	case "LLMStats":
		buckets := []map[string]any{}
		trunc := "" // one trunc per response: the first bucket's
		if bs := d.LLM[q.Get("operation")]; len(bs) > 0 {
			trunc = bs[0].Trunc
		}
		for _, b := range d.LLM[q.Get("operation")] {
			buckets = append(buckets, map[string]any{"time": b.Time.UTC().Format(time.RFC3339),
				"statuses": b.Statuses, "total": b.Total, "tokens": map[string]any{"input": b.Input,
					"output": b.Output, "cached": b.Cached, "thoughts": b.Thoughts,
					"total": b.Input + b.Output + b.Cached + b.Thoughts}})
		}
		body = map[string]any{"bank_id": fxBank, "period": q.Get("period"), "trunc": trunc, "buckets": buckets}
	case "OperationsPage":
		var ops []hindsight.Operation
		for _, o := range d.Ops {
			if (q.Get("status") == "" || o.Status == q.Get("status")) && (q.Get("type") == "" || o.Type == q.Get("type")) {
				ops = append(ops, o)
			}
		}
		body = map[string]any{"bank_id": fxBank, "total": len(ops), "operations": nonNil(pageOf(ops, q))}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// pageOf is the limit/offset page of xs.
func pageOf[T any](xs []T, q map[string][]string) []T {
	get := func(k string, def int) int {
		if v := q[k]; len(v) > 0 {
			if n, err := strconv.Atoi(v[0]); err == nil {
				return n
			}
		}
		return def
	}
	off := min(max(get("offset", 0), 0), len(xs))
	return xs[off:min(off+max(get("limit", len(xs)), 0), len(xs))]
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

// toolUse writes a tool_use line (a main-agent or subagent tool call of
// turn n) at ts.
func (x *fx) toolUse(ts time.Time, session, project string, n int, name string, subagent bool) {
	x.t.Helper()
	x.tel(ts, telemetry.Raw{Name: "tool_use", Fields: map[string]any{"n": n, "name": name, "ids": 0, "subagent": subagent}}, session, project)
}

// prompts writes count prompts of project, one per hour going back from
// start (the first one an hour before it), each in its own session
// prefix-<i> as turn 1; edit changes the i-th prompt before it is
// written. It returns the sessions' names.
func (x *fx) prompts(project, prefix string, start time.Time, count int, edit func(i int, p *telemetry.Prompt)) []string {
	x.t.Helper()
	var sessions []string
	for i := range count {
		p := telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}
		if edit != nil {
			edit(i, &p)
		}
		s := fmt.Sprintf("%s-%d", prefix, i)
		x.tel(start.Add(-time.Duration(i+1)*time.Hour), p, s, project)
		sessions = append(sessions, s)
	}
	return sessions
}
