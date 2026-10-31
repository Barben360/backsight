package dashboard

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// dataHarness is a dashboard with the fake index, server and doctor.
func dataHarness(t *testing.T, mods ...func(*fakeIndex)) *harness {
	t.Helper()
	return newDataHarness(t, withIndex(mods...), withServer(), withDoctor())
}

// overview is the overview's result for p, which must be ready.
func overview(t *testing.T, h *harness, p views.Params) {
	t.Helper()
	if _, st, err := h.s.data.result(context.Background(), "overview", p, ""); err != nil || st != stateReady {
		t.Fatalf("overview %+v: state %d, %v", p, st, err)
	}
}

// checkState is check's state, which must come with no error unless broken.
func checkState(t *testing.T, h *harness) indexState {
	t.Helper()
	st, err := h.s.data.check(context.Background())
	if st != stateBroken && err != nil {
		t.Fatalf("check: state %d with %v", st, err)
	}
	return st
}

func TestResultCacheShares30s(t *testing.T) {
	h := dataHarness(t)
	p := views.Params{Limit: 50}
	overview(t, h, p)
	cu, step, _ := h.ix.counts()
	if cu != 1 || step == 0 {
		t.Fatalf("first result: %d catch-ups, %d read transactions; want 1 and some", cu, step)
	}
	overview(t, h, p)
	h.clock.add(29 * time.Second)
	overview(t, h, p)
	if cu, rt, _ := h.ix.counts(); cu != 1 || rt != step {
		t.Errorf("within 30 s: %d catch-ups, %d read transactions; want 1 and %d", cu, rt, step)
	}
	h.clock.add(2 * time.Second)
	overview(t, h, p)
	if cu, rt, _ := h.ix.counts(); cu != 2 || rt != 2*step {
		t.Errorf("after 31 s: %d catch-ups, %d read transactions; want 2 and %d", cu, rt, 2*step)
	}
	overview(t, h, views.Params{Days: 30, DaysSet: true, Limit: 50})
	if cu, _, _ := h.ix.counts(); cu != 3 {
		t.Errorf("days 30: %d catch-ups, want its own computation (3)", cu)
	}
}

func TestCacheKeyIgnoresTheClock(t *testing.T) {
	h := dataHarness(t)
	p := views.Params{Days: 7, DaysSet: true, Limit: 50}
	overview(t, h, p)
	h.clock.add(10 * time.Second)
	overview(t, h, p)
	if cu, _, _ := h.ix.counts(); cu != 1 {
		t.Errorf("the same query 10 s later: %d computations, want 1", cu)
	}
	if cacheKey("quality", "", views.Params{Dev: true}) == cacheKey("quality", "", views.Params{}) {
		t.Error("the developer flag does not change the key")
	}

	// Every parameter but Text and DevSet tells two keys apart.
	zero := cacheKey("overview", "", views.Params{})
	if cacheKey("quality", "", views.Params{}) == zero || cacheKey("overview", "x", views.Params{}) == zero {
		t.Error("the view or the id does not change the key")
	}
	rt := reflect.TypeFor[views.Params]()
	for i := range rt.NumField() {
		var p views.Params
		f := reflect.ValueOf(&p).Elem().Field(i)
		switch f.Kind() {
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int:
			f.SetInt(1)
		case reflect.String:
			f.SetString("x")
		default:
			t.Fatalf("Params.%s: kind %s not covered", rt.Field(i).Name, f.Kind())
		}
		name := rt.Field(i).Name
		ignored := name == "Text" || name == "DevSet"
		if got := cacheKey("overview", "", p) != zero; got == ignored {
			t.Errorf("Params.%s changes the key: %v, want %v", name, got, !ignored)
		}
	}
}

// outcome is a result call's return.
type outcome struct {
	res metrics.Result
	st  indexState
	err error
}

// holdReadTx makes the fake index hold every ReadTx until the returned
// release is called; entered receives a value as each starts. joined
// receives one per caller joining a computation in flight.
func holdReadTx(t *testing.T, h *harness) (entered, joined chan struct{}, release func()) {
	t.Helper()
	block := make(chan struct{})
	entered, joined = make(chan struct{}, 64), make(chan struct{}, 64)
	h.ix.set(func(f *fakeIndex) { f.block, f.entered = block, entered })
	onJoin = func() { joined <- struct{}{} }
	t.Cleanup(func() { onJoin = func() {} })
	return entered, joined, func() { close(block) }
}

func TestConcurrentMissesComputeOnce(t *testing.T) {
	h := dataHarness(t)
	entered, joined, release := holdReadTx(t, h)
	p := views.Params{Limit: 50}
	outs := make(chan outcome, 5)
	for range 5 {
		go func() {
			res, st, err := h.s.data.result(context.Background(), "overview", p, "")
			outs <- outcome{res, st, err}
		}()
	}
	// The one computation is held in its first ReadTx: another one
	// starting means the callers did not share it.
	enters, joins := 0, 0
	for enters < 1 || joins < 4 {
		select {
		case <-entered:
			if enters++; enters > 1 {
				t.Fatal("a second computation started: the misses did not share one")
			}
		case <-joined:
			joins++
		}
	}
	release()
	var first outcome
	for i := range 5 {
		o := <-outs
		if o.err != nil || o.st != stateReady {
			t.Fatalf("caller %d: state %d, %v", i, o.st, o.err)
		}
		if i == 0 {
			first = o
		} else if !reflect.DeepEqual(o.res, first.res) {
			t.Errorf("caller %d got another result", i)
		}
	}
	if cu, _, _ := h.ix.counts(); cu != 1 {
		t.Errorf("%d computations, want 1", cu)
	}
}

func TestSharedComputeSurvivesFirstCaller(t *testing.T) {
	h := dataHarness(t)
	entered, joined, release := holdReadTx(t, h)
	p := views.Params{Limit: 50}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	first := make(chan error, 1)
	go func() {
		_, _, err := h.s.data.result(ctx1, "overview", p, "")
		first <- err
	}()
	<-entered // the first caller's computation holds its ReadTx
	second := make(chan outcome, 1)
	go func() {
		res, st, err := h.s.data.result(context.Background(), "overview", p, "")
		second <- outcome{res, st, err}
	}()
	<-joined
	cancel1()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the first caller: %v, want context.Canceled", err)
	}
	release()
	o := <-second
	if o.err != nil || o.st != stateReady || o.res.View != "overview" {
		t.Fatalf("the second caller: state %d, view %q, %v", o.st, o.res.View, o.err)
	}
	if cu, _, _ := h.ix.counts(); cu != 1 || len(entered) != 0 {
		t.Errorf("%d computations (%d more started), want 1", cu, len(entered))
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	h := dataHarness(t)
	for i := range 2 {
		_, st, err := h.s.data.result(context.Background(), "sessions", views.Params{Limit: 50}, "zzzzzzzz")
		var nf metrics.NotFoundError
		if st != stateReady || !errors.As(err, &nf) {
			t.Fatalf("call %d: state %d, %v; want a NotFoundError", i, st, err)
		}
	}
	if cu, _, _ := h.ix.counts(); cu != 2 {
		t.Errorf("%d computations, want 2 (an error is not kept)", cu)
	}
}

func TestResultRefusesText(t *testing.T) {
	h := dataHarness(t)
	_, _, err := h.s.data.result(context.Background(), "memories", views.Params{Limit: 50, Text: true}, indexgen.MemID(0))
	if err == nil {
		t.Fatal("a text result was served")
	}
	if cu, rt, _ := h.ix.counts(); cu != 0 || rt != 0 {
		t.Errorf("%d catch-ups, %d read transactions: want no computation", cu, rt)
	}
	if n := len(h.s.data.results.entries); n != 0 {
		t.Errorf("%d cache entries, want none", n)
	}
}

func TestFirstBuildInBackground(t *testing.T) {
	t.Run("built", func(t *testing.T) {
		gate := make(chan struct{})
		h := newDataHarness(t, withIndex(func(f *fakeIndex) { f.openGate = gate }), withNeedsBuild())
		if st := checkState(t, h); st != stateBuilding {
			t.Fatalf("during the build: state %d, want building", st)
		}
		if _, st, _ := h.s.data.result(context.Background(), "overview", views.Params{Limit: 50}, ""); st != stateBuilding {
			t.Errorf("a result during the build: state %d, want building", st)
		}
		close(gate)
		<-h.s.data.built
		if st := checkState(t, h); st != stateReady {
			t.Fatalf("after the build: state %d, want ready", st)
		}
		h.ix.mu.Lock()
		budgets := h.ix.budgets
		h.ix.mu.Unlock()
		if len(budgets) != 1 || budgets[0] != buildBudget {
			t.Errorf("catch-up budgets %v, want [%v]", budgets, buildBudget)
		}
		if _, err := h.stop(); err != nil {
			t.Fatal(err)
		}
		if _, _, closes := h.ix.counts(); closes != 1 {
			t.Errorf("the stop closed the index %d times, want 1", closes)
		}
	})
	t.Run("the stop cancels it", func(t *testing.T) {
		h := newDataHarness(t, withIndex(func(f *fakeIndex) { f.openGate = make(chan struct{}) }), withNeedsBuild())
		if _, err := h.stop(); err != nil {
			t.Fatal(err)
		}
		h.ix.mu.Lock()
		ended := h.ix.openEnded
		h.ix.mu.Unlock()
		if !errors.Is(ended, context.Canceled) {
			t.Errorf("the build's open ended with %v, want context.Canceled", ended)
		}
	})
}

func TestUpdateInProgressRetried(t *testing.T) {
	// Start's open and the first request's both meet the update.
	h := dataHarness(t, func(f *fakeIndex) { f.openErrs = []error{index.ErrUpdateInProgress, index.ErrUpdateInProgress} })
	if st := checkState(t, h); st != stateUpdating {
		t.Fatalf("during the update: state %d, want updating", st)
	}
	if st := checkState(t, h); st != stateReady {
		t.Fatalf("the next request: state %d, want ready", st)
	}
	checkState(t, h)
	h.ix.mu.Lock()
	opens := h.ix.opens
	h.ix.mu.Unlock()
	if opens != 3 {
		t.Errorf("%d opens, want 3 (Start, then each request until one succeeds)", opens)
	}
}

func TestNewerSchemaIsSticky(t *testing.T) {
	t.Run("file version", func(t *testing.T) {
		h := dataHarness(t)
		if st := checkState(t, h); st != stateReady {
			t.Fatalf("state %d, want ready", st)
		}
		schema := h.ix.Schema()
		h.ix.set(func(f *fakeIndex) { f.fileVersion = schema + 1 })
		if st := checkState(t, h); st != stateUpdated {
			t.Fatalf("a newer file: state %d, want updated", st)
		}
		h.ix.set(func(f *fakeIndex) { f.fileVersion = schema })
		if st := checkState(t, h); st != stateUpdated {
			t.Errorf("set back: state %d, want updated until the restart", st)
		}
	})
	t.Run("schema changed", func(t *testing.T) {
		h := dataHarness(t, func(f *fakeIndex) { f.catchUpErr = index.ErrSchemaChanged })
		_, st, err := h.s.data.result(context.Background(), "overview", views.Params{Limit: 50}, "")
		if st != stateUpdated || err != nil {
			t.Fatalf("first miss: state %d, %v; want updated", st, err)
		}
		if st := checkState(t, h); st != stateUpdated {
			t.Errorf("after: state %d, want updated", st)
		}
	})
	t.Run("side file", func(t *testing.T) {
		h := dataHarness(t, func(f *fakeIndex) { f.side = true })
		if st := checkState(t, h); st != stateUpdated {
			t.Errorf("first request: state %d, want updated", st)
		}
	})
}

func TestBrokenIndexAtStart(t *testing.T) {
	p := freePort(t)
	cfg := Config{Dirs: testDirs(t), Port: p, OpenIndex: func(context.Context) (Index, error) { return nil, index.ErrUnreadable }}
	s, err := Start(context.Background(), cfg)
	if !errors.Is(err, index.ErrUnreadable) {
		if s != nil {
			_ = s.ln.Close()
		}
		t.Fatalf("Start: %v, want ErrUnreadable", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		t.Fatalf("a failed Start left port %d bound: %v", p, err)
	}
	_ = ln.Close()
	noRuntimeFile(t, cfg)
}

func TestNoDataLayer(t *testing.T) {
	h := newHarness(t)
	if st, err := h.s.data.check(context.Background()); st != stateBroken || err == nil {
		t.Errorf("check: state %d, %v; want broken with its sentence", st, err)
	}
	if _, st, _ := h.s.data.result(context.Background(), "overview", views.Params{Limit: 50}, ""); st != stateBroken {
		t.Errorf("result: state %d, want broken", st)
	}
	if src := h.s.data.base(); src.Index != nil {
		t.Errorf("base: an index %v", src.Index)
	}
}

// TestDoctorWarmedAtStart: the doctor's first run starts with the
// dashboard, before any request, and a request then reuses it.
func TestDoctorWarmedAtStart(t *testing.T) {
	started := make(chan struct{}, 8)
	h := newDataHarness(t, func(h *harness, c *Config) {
		c.Source.Doctor = func(context.Context) ([]metrics.DoctorCheck, error) {
			h.doctorRuns.Add(1)
			started <- struct{}{}
			return []metrics.DoctorCheck{{ID: "server", Status: "ok"}}, nil
		}
	})
	select {
	case <-started:
	case <-time.After(10 * time.Second): // a watchdog, not a synchronisation
		t.Fatal("no doctor run after Start")
	}
	if _, err := h.s.data.src.Doctor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := h.doctorRuns.Load(); n != 1 {
		t.Errorf("%d doctor runs, want the warm-up's only", n)
	}
}
