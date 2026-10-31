package metrics

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/telemetry"
)

func TestServerTimeoutIsThreeSeconds(t *testing.T) {
	if ServerTimeout != 3*time.Second {
		t.Fatalf("ServerTimeout = %s, want 3s", ServerTimeout)
	}
	if doctorTimeout != 10*time.Second {
		t.Fatalf("doctorTimeout = %s, want 10s", doctorTimeout)
	}
}

func TestFetchParallelOverlaps(t *testing.T) {
	// Three calls wait on a barrier that opens once all three have
	// started: a serial run would wait on the first until the guard
	// context ends, and fail it.
	guard, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var started atomic.Int32
	release := make(chan struct{})
	barrier := func(ctx context.Context) error {
		if started.Add(1) == 3 {
			close(release)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	errs := fetchParallel(guard, within(time.Hour), map[string]func(context.Context) error{"a": barrier, "b": barrier, "c": barrier})
	if guard.Err() != nil {
		t.Fatal("the guard context ended: the calls did not run at once")
	}
	if len(errs) != 0 {
		t.Fatalf("errors = %v, want none", errs)
	}

	// A call that blocks until its context ends fails alone at the
	// timeout; the others succeed.
	ok := func(context.Context) error { return nil }
	blocking := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	errs = fetchParallel(guard, shortFor("slow"), map[string]func(context.Context) error{
		"a": ok, "b": ok, "c": ok, "slow": blocking})
	if guard.Err() != nil {
		t.Fatal("the guard context ended: the blocking call was not cut at its timeout")
	}
	if len(errs) != 1 || !errors.Is(errs["slow"], context.DeadlineExceeded) {
		t.Fatalf("errors = %v, want only slow: deadline exceeded", errs)
	}

	// A call that ignores its context is abandoned at its timeout.
	hang := make(chan struct{})
	defer close(hang)
	deaf := func(context.Context) error { <-hang; return nil }
	errs = fetchParallel(guard, shortFor("deaf"), map[string]func(context.Context) error{"a": ok, "deaf": deaf})
	if guard.Err() != nil {
		t.Fatal("the guard context ended: a call ignoring its context held fetchParallel")
	}
	if len(errs) != 1 || !errors.Is(errs["deaf"], context.DeadlineExceeded) {
		t.Fatalf("errors = %v, want only deaf: deadline exceeded", errs)
	}
}

func TestServerErrorEnums(t *testing.T) {
	var syntax any
	decodeErr := fmt.Errorf("GET /x: invalid JSON: %w", json.Unmarshal([]byte(`{"a":`), &syntax))
	var typed struct{ Total int }
	typeErr := fmt.Errorf("GET /x: invalid JSON: %w", json.Unmarshal([]byte(`{"Total":"x"}`), &typed))
	api := func(k hindsight.Kind) error { return fmt.Errorf("read: %w", &hindsight.APIError{Kind: k, Status: 1}) }
	for _, c := range []struct {
		name         string
		err          error
		enum, reason string
	}{
		{"nil", nil, "", ""},
		{"no client", errNoServer, "not_configured", "not_configured"},
		{"unreachable", fmt.Errorf("%w: GET /x: %w", hindsight.ErrUnreachable, syscall.ECONNREFUSED), "unreachable", "server_down"},
		{"timeout", fmt.Errorf("%w: GET /x", hindsight.ErrTimeout), "timeout", "server_down"},
		{"context deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), "timeout", "server_down"},
		{"unauthorized", api(hindsight.Unauthorized), "unauthorized", "server_down"},
		{"not found", api(hindsight.NotFound), "not_found", "server_unsupported"},
		{"server error", api(hindsight.ServerError), "server_error", "server_down"},
		{"unavailable", api(hindsight.Unavailable), "server_error", "server_down"},
		{"rate limited", api(hindsight.RateLimited), "server_error", "server_down"},
		{"bad request", api(hindsight.BadRequest), "server_error", "server_down"},
		{"redirected", api(hindsight.Redirected), "server_error", "server_down"},
		{"syntax", decodeErr, "bad_response", "server_unsupported"},
		{"type", typeErr, "bad_response", "server_unsupported"},
		{"anything else", errors.New("GET /x: response larger than 32 bytes"), "server_error", "server_down"},
	} {
		enum, reason := serverError(c.err)
		if enum != c.enum || reason != c.reason {
			t.Errorf("%s: serverError = %q, %q; want %q, %q", c.name, enum, reason, c.enum, c.reason)
		}
	}
}

// TestServerFaultsThroughTheClient: each fault of the fake, read through
// the real client, gives the enum the view must show.
func TestServerFaultsThroughTheClient(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		enum string
	}{
		{"unreachable", hindsight.ErrUnreachable, "unreachable"},
		{"timeout", hindsight.ErrTimeout, "timeout"},
		{"401", &hindsight.APIError{Kind: hindsight.Unauthorized}, "unauthorized"},
		{"404", &hindsight.APIError{Kind: hindsight.NotFound}, "not_found"},
		{"500", &hindsight.APIError{Kind: hindsight.ServerError}, "server_error"},
		{"garbage", errBadBody, "bad_response"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "timeout" {
				lowerTimeouts(t, 10*time.Millisecond, time.Minute) // the fake blocks until the call's deadline
			}
			x := newFx(t, fxNow)
			x.server.fail("BankStats", c.err)
			d, st := loadServer(context.Background(), x.source(), x.window(7), []string{"stats"})
			if enum, _ := serverError(d.Err["stats"]); enum != c.enum {
				t.Fatalf("stats error %v → %q, want %q", d.Err["stats"], enum, c.enum)
			}
			if st == nil || st.OK || st.Error != c.enum {
				t.Fatalf("state %+v, want {OK: false, Error: %q}", st, c.enum)
			}
		})
	}
}

// stateOf runs withTx with need and returns the view's server state.
func stateOf(t *testing.T, x *fx, need []string) *ServerState {
	t.Helper()
	var st *ServerState
	if err := withTx(context.Background(), x.source(), x.query(7), need, func(e env) error {
		st = e.server
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestServerStateWhenAllFail(t *testing.T) {
	lastOK := time.Date(2026, 10, 29, 8, 30, 0, 0, time.UTC)

	x := newFx(t, fxNow)
	x.local(index.LocalStatus{ReachLastOK: lastOK})
	x.doctor = []DoctorCheck{{ID: "server", Status: "ok"}} // the doctor is not a server read
	x.server.failAll(hindsight.ErrUnreachable)
	st := stateOf(t, x, fxNeed)
	if st == nil || st.OK || st.Error != "unreachable" || st.LastOK == nil || !st.LastOK.Equal(lastOK) {
		t.Fatalf("every read unreachable: state %+v, want {OK: false, Error: unreachable, LastOK: %s}", st, lastOK)
	}
	if loc := st.LastOK.Location(); loc != fxParis {
		t.Errorf("LastOK in %s, want the window's zone", loc)
	}

	x.server.fail("Version", nil)
	if st := stateOf(t, x, fxNeed); st == nil || !st.OK || st.Error != "" {
		t.Fatalf("one read succeeding: state %+v, want OK true and no error (a partial failure shows in its metrics)", st)
	}

	// Error is the first failure in key order (completed < failed < ... <
	// version), whatever order the reads end in.
	x.server.failAll(hindsight.ErrUnreachable)
	x.server.fail("OperationsPage", &hindsight.APIError{Kind: hindsight.Unauthorized})
	if st := stateOf(t, x, fxNeed); st == nil || st.OK || st.Error != "unauthorized" {
		t.Fatalf("operations unauthorized, the rest unreachable: state %+v, want Error unauthorized (completed first)", st)
	}

	y := newFx(t, fxNow)
	y.server = nil
	st = stateOf(t, y, fxNeed)
	if st == nil || st.OK || st.Error != "not_configured" || st.LastOK != nil {
		t.Fatalf("no client: state %+v, want {OK: false, Error: not_configured, LastOK: nil}", st)
	}
	// Without a server, every read is not configured; the doctor still runs.
	var d *serverData
	if err := withTx(context.Background(), y.source(), y.query(7), fxNeed, func(e env) error { d = e.srv; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, key := range fxNeed {
		if key == "doctor" {
			continue
		}
		if !errors.Is(d.Err[key], errNoServer) {
			t.Errorf("no client: Err[%s] = %v, want not configured", key, d.Err[key])
		}
	}

	if st := stateOf(t, x, []string{"doctor"}); st != nil {
		t.Fatalf("a view reading no server endpoint: state %+v, want nil", st)
	}
}

func TestLoadServerReads(t *testing.T) {
	x := newFx(t, fxNow)
	at := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	op := func(i int, status, typ string) hindsight.Operation {
		return hindsight.Operation{ID: fmt.Sprintf("%s-%s-%03d", status, typ, i), Status: status, Type: typ, UpdatedAt: at, CreatedAt: at}
	}
	var ops []hindsight.Operation
	for i := range 3 {
		ops = append(ops, op(i, "pending", "retain"))
	}
	for i := range 2 {
		ops = append(ops, op(i, "processing", "consolidation"))
	}
	for i := range 650 {
		ops = append(ops, op(i, "failed", "retain"))
	}
	for i := range 250 {
		ops = append(ops, op(i, "completed", "retain"))
	}
	for i := range 40 {
		ops = append(ops, op(i, "completed", "consolidation"))
	}
	x.server.set(func(d *fakeData) {
		d.Ops = ops
		d.Version = hindsight.Version{APIVersion: "1.1.0"}
		d.Stats = hindsight.BankStats{BankID: fxBank, TotalNodes: 120, PendingConsolidation: 4, LastConsolidatedAt: at}
		d.Tags = []hindsight.TagCount{{Tag: "project:alpha", Count: 5}, {Tag: "session:s1", Count: 9}, {Tag: "project:beta", Count: 2}}
		d.Series = hindsight.Timeseries{Trunc: "day", Buckets: []hindsight.SeriesBucket{{Time: at, World: 3, Experience: 2, Observation: 1}}}
		d.LLM = map[string][]hindsight.LLMBucket{
			"":       {{Time: at, Statuses: map[string]int64{"success": 9}, Total: 9, Input: 1000, Output: 200, Cached: 300, Thoughts: 50}},
			"retain": {{Time: at, Statuses: map[string]int64{"success": 4}, Total: 4, Input: 400, Output: 80}},
		}
	})
	d, st := loadServer(context.Background(), x.source(), x.window(7), append(slices.Clone(fxNeed), "llm:retain", "stats"))
	if len(d.Err) != 0 || st == nil || !st.OK || st.Error != "" {
		t.Fatalf("errors %v, state %+v; want every read", d.Err, st)
	}
	if d.Pending != 3 || d.Processing != 2 {
		t.Errorf("queue = %d pending, %d processing; want 3, 2", d.Pending, d.Processing)
	}
	if len(d.Failed) != 500 || d.Failed[0].ID != "failed-retain-000" || d.Failed[499].ID != "failed-retain-499" {
		t.Errorf("failed: %d operations, want the first 5 pages (500)", len(d.Failed))
	}
	if len(d.Completed) != 200 || d.Completed[199].ID != "completed-retain-199" {
		t.Errorf("completed: %d operations, want the first 2 pages of retains (200)", len(d.Completed))
	}
	if d.Version.APIVersion != "1.1.0" || d.Stats.TotalNodes != 120 || !d.Stats.LastConsolidatedAt.Equal(at) {
		t.Errorf("version %+v, stats %+v", d.Version, d.Stats)
	}
	if want := []hindsight.TagCount{{Tag: "project:alpha", Count: 5}, {Tag: "project:beta", Count: 2}}; !reflect.DeepEqual(d.Tags, want) {
		t.Errorf("tags = %v, want %v", d.Tags, want)
	}
	if len(d.Series.Buckets) != 1 || d.Series.Buckets[0].World != 3 || !d.Series.Buckets[0].Time.Equal(at) {
		t.Errorf("series = %+v", d.Series)
	}
	if len(d.LLM) != 2 || d.LLM[""][0].Thoughts != 50 || d.LLM["retain"][0].Input != 400 {
		t.Errorf("llm = %+v, want the all-operations and retain buckets", d.LLM)
	}

	var reads []string
	for _, c := range x.server.called() {
		if !strings.HasPrefix(c, "ProjectTags") {
			reads = append(reads, c)
		}
	}
	sort.Strings(reads)
	want := []string{
		"BankStats?",
		"LLMStats?period=7d",
		"LLMStats?operation=retain&period=7d",
		"MemoriesTimeseries?period=7d&time_field=created_at",
		"OperationsPage?limit=1&offset=0&status=pending",
		"OperationsPage?limit=1&offset=0&status=processing",
		"OperationsPage?limit=100&offset=0&status=completed&type=retain",
		"OperationsPage?limit=100&offset=0&status=failed",
		"OperationsPage?limit=100&offset=100&status=completed&type=retain",
		"OperationsPage?limit=100&offset=100&status=failed",
		"OperationsPage?limit=100&offset=200&status=failed",
		"OperationsPage?limit=100&offset=300&status=failed",
		"OperationsPage?limit=100&offset=400&status=failed",
		"Version?",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(reads, want) {
		t.Errorf("reads =\n%s\nwant\n%s", strings.Join(reads, "\n"), strings.Join(want, "\n"))
	}

	// A key no endpoint answers is a failed read.
	if d, _ := loadServer(context.Background(), x.source(), x.window(7), []string{"nope"}); !errors.Is(d.Err["nope"], errUnknownRead) {
		t.Errorf("unknown key: %v", d.Err["nope"])
	}

	// The reads stop at a short page, and at a full page that reaches
	// the total.
	for _, c := range []struct{ failed, calls int }{{150, 2}, {100, 1}, {0, 1}} {
		y := newFx(t, fxNow)
		y.server.set(func(d *fakeData) { d.Ops = ops[:5+c.failed] })
		d, _ = loadServer(context.Background(), y.source(), y.window(7), []string{"failed"})
		if len(d.Failed) != c.failed || len(y.server.called()) != c.calls {
			t.Errorf("%d failed: %d read in %d calls, want %d in %d", c.failed, len(d.Failed), len(y.server.called()), c.failed, c.calls)
		}
	}
}

func TestPeriodsCoverTheWindow(t *testing.T) {
	for _, c := range []struct {
		span        time.Duration
		series, llm string
	}{
		{day, "1d", "1d"}, {day + time.Second, "7d", "7d"}, {7 * day, "7d", "7d"},
		{7*day + time.Second, "30d", "30d"}, {30 * day, "30d", "30d"}, {30*day + time.Second, "90d", "30d"}, {365 * day, "90d", "30d"},
	} {
		if s, l := seriesPeriod(c.span), llmPeriod(c.span); s != c.series || l != c.llm {
			t.Errorf("%s: series %s, llm %s; want %s, %s", c.span, s, l, c.series, c.llm)
		}
	}
	// The period follows the window's length, not the clock: the clock
	// moving on after the window was built keeps a 7-day window on 7d.
	x := newFx(t, fxNow)
	src := x.source()
	w := x.window(7)
	src.Now = func() time.Time { return fxNow.Add(time.Minute) }
	if _, st := loadServer(context.Background(), src, w, []string{"series", "llm:"}); st == nil || !st.OK {
		t.Fatalf("state %+v", st)
	}
	c := x.server.called()
	sort.Strings(c)
	if want := []string{"LLMStats?period=7d", "MemoriesTimeseries?period=7d&time_field=created_at"}; !reflect.DeepEqual(c, want) {
		t.Fatalf("calls %v, want %v", c, want)
	}

	// The periods count back from now: a 7-day window ending 20 days ago
	// needs the 27 days back to its start, the 30-day period.
	y := newFx(t, fxNow)
	past, err := NewWindow(fxNow, 0, fxNow.Add(-27*day), fxNow.Add(-20*day), fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	if _, st := loadServer(context.Background(), y.source(), past, []string{"series", "llm:"}); st == nil || !st.OK {
		t.Fatalf("state %+v", st)
	}
	c = y.server.called()
	sort.Strings(c)
	if want := []string{"LLMStats?period=30d", "MemoriesTimeseries?period=30d&time_field=created_at"}; !reflect.DeepEqual(c, want) {
		t.Fatalf("past window: calls %v, want %v", c, want)
	}
}

// deafServer's BankStats ignores its context until hang closes.
type deafServer struct {
	*fakeServer
	hang, returned chan struct{}
}

func (s deafServer) BankStats(context.Context) (hindsight.BankStats, error) {
	<-s.hang
	defer close(s.returned)
	return hindsight.BankStats{TotalNodes: 99}, nil
}

func (s deafServer) ProjectTags(context.Context) ([]hindsight.TagCount, error) {
	return []hindsight.TagCount{{Tag: "project:alpha", Count: 1}}, nil
}

// within is a read timeout of d for every key.
func within(d time.Duration) func(string) time.Duration {
	return func(string) time.Duration { return d }
}

// shortFor is a read timeout of 10 ms for key, a minute for the others:
// only the read meant to time out does, however slow the runner.
func shortFor(key string) func(string) time.Duration {
	return func(k string) time.Duration {
		if k == key {
			return 10 * time.Millisecond
		}
		return time.Minute
	}
}

// TestLoadServerAbandonsADeafRead: a ServerReader that ignores its
// context cannot hold the view: its read times out, the others are kept,
// and its late answer is dropped. Only the deaf read has a short timeout
// (it blocks until the test releases it, so it ends by its timeout); the
// tags read has a minute, however slow the runner.
func TestLoadServerAbandonsADeafRead(t *testing.T) {
	shortRead(t, "stats")
	x := newFx(t, fxNow)
	src := x.source()
	deaf := deafServer{fakeServer: x.server, hang: make(chan struct{}), returned: make(chan struct{})}
	src.Server = deaf
	d, st := loadServer(context.Background(), src, x.window(7), []string{"stats", "tags"})
	if enum, _ := serverError(d.Err["stats"]); enum != "timeout" || d.Err["tags"] != nil || len(d.Tags) != 1 {
		t.Fatalf("stats %v, tags %v %v; want stats timeout and the tags", d.Err["stats"], d.Err["tags"], d.Tags)
	}
	if st == nil || !st.OK || st.Error != "" {
		t.Fatalf("state %+v, want OK", st)
	}
	close(deaf.hang)
	<-deaf.returned
	if d.Stats.TotalNodes != 0 {
		t.Fatalf("the late answer was kept: %d nodes", d.Stats.TotalNodes)
	}
}

func TestDoctorRun(t *testing.T) {
	lowerTimeouts(t, time.Minute, time.Minute)
	x := newFx(t, fxNow)
	x.server.set(func(d *fakeData) { d.Stats.TotalNodes = 7 })

	src := x.source()
	d, _ := loadServer(context.Background(), src, x.window(7), []string{"doctor", "stats"})
	if !errors.Is(d.DoctorErr, errNoDoctor) || d.Doctor != nil || d.Stats.TotalNodes != 7 {
		t.Fatalf("no doctor: %v %v, stats %d", d.DoctorErr, d.Doctor, d.Stats.TotalNodes)
	}

	checks := []DoctorCheck{{ID: "bank-config", Status: "warn", Fix: "backsight setup"}}
	src.Doctor = func(context.Context) ([]DoctorCheck, error) { return checks, nil }
	d, _ = loadServer(context.Background(), src, x.window(7), []string{"doctor"})
	if d.DoctorErr != nil || !reflect.DeepEqual(d.Doctor, checks) {
		t.Fatalf("doctor: %v %v, want %v", d.DoctorErr, d.Doctor, checks)
	}

	// A run that ignores its context is abandoned at doctorTimeout; the
	// server reads are not held back.
	lowerTimeouts(t, time.Minute, 10*time.Millisecond)
	hang := make(chan struct{})
	defer close(hang)
	src.Doctor = func(context.Context) ([]DoctorCheck, error) { <-hang; return checks, nil }
	d, _ = loadServer(context.Background(), src, x.window(7), []string{"doctor", "stats"})
	if !errors.Is(d.DoctorErr, context.DeadlineExceeded) || d.Doctor != nil || d.Stats.TotalNodes != 7 {
		t.Fatalf("hung doctor: %v %v, stats %d; want a deadline error and the stats", d.DoctorErr, d.Doctor, d.Stats.TotalNodes)
	}
}

// TestFixtureTelIsTheWritersLine: fx.tel writes the line telemetry.Writer
// writes for the same event (but ts, which the writer takes from its
// clock), in the month file of ts.
func TestFixtureTelIsTheWritersLine(t *testing.T) {
	x := newFx(t, fxNow)
	ev := telemetry.Prompt{N: 3, Decision: "recall", Plen: 120, Injected: 2, IDs: []string{"m1", "m2"}, MS: 40, Chars: 900}
	ts := time.Date(2026, 9, 30, 23, 30, 0, 123e6, time.UTC)
	x.tel(ts, ev, "s1", "alpha")
	raw := telemetry.Raw{Name: "dashboard", Fields: map[string]any{"view": "overview", "session": "overwritten"}}
	x.tel(ts, raw, "s1", "alpha")

	stateDir := t.TempDir()
	w := telemetry.New(stateDir, telemetry.Common{V: instructions.Version, Session: "s1", Project: "alpha", BinaryVersion: fxBinary})
	w.Emit(ev)
	w.Emit(raw)

	ours := readLines(t, filepath.Join(x.dirs.TelemetryDir(), "events-2026-09.jsonl"))
	files, _ := filepath.Glob(filepath.Join(stateDir, "telemetry", "events-*.jsonl"))
	if len(files) != 1 || len(ours) != 2 {
		t.Fatalf("writer files %v, fixture lines %d", files, len(ours))
	}
	theirs := readLines(t, files[0])
	// Both ts read back exactly in one layout (UTC, milliseconds), and each
	// line is in the file of its ts's month.
	const layout = "2006-01-02T15:04:05.000Z"
	for i := range ours {
		for who, line := range map[string]map[string]any{"fixture": ours[i], "writer": theirs[i]} {
			s, _ := line["ts"].(string)
			at, err := time.Parse(layout, s)
			if err != nil || at.Format(layout) != s {
				t.Errorf("%s line %d ts %q is not in the layout %s", who, i, s, layout)
			}
			if who == "writer" && filepath.Base(files[0]) != "events-"+at.Format("2006-01")+".jsonl" {
				t.Errorf("writer line %d at %s is in %s", i, s, filepath.Base(files[0]))
			}
		}
		if ours[i]["ts"] != "2026-09-30T23:30:00.123Z" {
			t.Errorf("line %d ts = %v", i, ours[i]["ts"])
		}
		delete(ours[i], "ts")
		delete(theirs[i], "ts")
		if !reflect.DeepEqual(ours[i], theirs[i]) {
			t.Errorf("line %d:\n fixture %v\n writer  %v", i, ours[i], theirs[i])
		}
	}
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := map[string]any{}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// TestFixtureFeedsEverySource: what the fixture writes reaches the index
// through the real catch-up: a telemetry line, a back-dated journal event
// in its own month file, the ledger, the local status.
func TestFixtureFeedsEverySource(t *testing.T) {
	x := newFx(t, fxNow)
	x.tel(fxNow.Add(-time.Hour), telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, IDs: []string{"m1"}}, "s1", "alpha")
	sept := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	x.journal(feedback.Event{TS: sept, Kind: "shows", Session: "s1", Turn: "t1", Agent: "main", Path: "prompt",
		Items: []feedback.Item{{ID: "m1", Rank: 1, Project: "alpha"}}})
	x.ledger(feedback.Ledger{Schema: feedback.LedgerSchema, Memories: map[string]feedback.Entry{"m1": {Shown: 4, Wrong: 1}}})
	x.local(index.LocalStatus{NotesQueued: 2, ReachLastOK: fxNow.Add(-time.Minute)})

	if _, err := os.Stat(filepath.Join(x.dirs.FeedbackDir(), "journal-2026-09.jsonl")); err != nil {
		t.Fatalf("the journal event is not in its month file: %v", err)
	}
	src := x.source()
	var prompts, shows, shown, queued int
	var binary string
	if err := src.Index.ReadTx(context.Background(), func(tx *sql.Tx) error {
		for _, q := range []struct {
			sql string
			out any
		}{
			{`SELECT count(*) FROM tel_prompt WHERE session = 's1' AND project = 'alpha'`, &prompts},
			{`SELECT count(*) FROM shows WHERE memory_id = 'm1' AND ts = ` + fmt.Sprint(sept.UnixMilli()), &shows},
			{`SELECT binary_version FROM tel_prompt`, &binary},
			{`SELECT shown FROM ledger WHERE memory_id = 'm1'`, &shown},
			{`SELECT notes_queued FROM local_status WHERE id = 1`, &queued},
		} {
			if err := tx.QueryRow(q.sql).Scan(q.out); err != nil {
				return fmt.Errorf("%s: %w", q.sql, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 || shows != 1 || binary != fxBinary || shown != 4 || queued != 2 {
		t.Fatalf("prompts %d, shows %d, binary %q, ledger shown %d, notes queued %d; want 1, 1, %q, 4, 2",
			prompts, shows, binary, shown, queued, fxBinary)
	}
	if src.Fresh.IngestedAt.IsZero() || src.Fresh.Partial {
		t.Errorf("freshness %+v, want a complete catch-up", src.Fresh)
	}
	if !src.Config.Feedback.Enabled || src.Server == nil || src.ServerHost != fxHost || src.Doctor != nil {
		t.Errorf("source: feedback %v, server %v, host %q, doctor set %v", src.Config.Feedback.Enabled, src.Server, src.ServerHost, src.Doctor != nil)
	}
	if !src.Now().Equal(fxNow) || x.window(7).Loc != fxParis {
		t.Errorf("clock %s, zone %s", src.Now(), x.window(7).Loc)
	}
}
