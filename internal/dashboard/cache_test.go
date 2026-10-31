package dashboard

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metrics"
)

func TestResultCacheIsBounded(t *testing.T) {
	old := resultMax
	resultMax = 3
	t.Cleanup(func() { resultMax = old })
	clk := &clock{t: indexgen.Now}
	c := &resultCache{now: clk.now, base: context.Background(), spawn: func(f func()) bool { go f(); return true }}
	computes := map[string]int{}
	var mu sync.Mutex
	get := func(key string) {
		t.Helper()
		_, err := c.get(context.Background(), key, func(context.Context) (metrics.Result, error) {
			mu.Lock()
			computes[key]++
			mu.Unlock()
			return metrics.Result{View: key}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	count := func(key string) int {
		mu.Lock()
		defer mu.Unlock()
		return computes[key]
	}
	for _, k := range []string{"a", "b", "c", "d"} {
		get(k)
	}
	get("d")
	if n := count("d"); n != 1 {
		t.Errorf("the newest key computed %d times, want 1", n)
	}
	get("a")
	if n := count("a"); n != 2 {
		t.Errorf("the oldest key computed %d times, want 2 (dropped for the fourth)", n)
	}
	get("c")
	if n := count("c"); n != 1 {
		t.Errorf("a key still kept computed %d times, want 1", n)
	}
	clk.add(31 * time.Second)
	get("e")
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n != 1 {
		t.Errorf("%d entries after the expired ones were swept, want 1", n)
	}
}

func TestServerReadsCached60s(t *testing.T) {
	clk := &clock{t: indexgen.Now}
	fs := &fakeServer{memText: "a memory's text"}
	cs := &cachedServer{next: fs, now: clk.now}
	ctx := context.Background()
	for range 2 {
		if _, err := cs.BankStats(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := fs.count("BankStats"); n != 1 {
		t.Errorf("BankStats twice: %d calls, want 1", n)
	}
	clk.add(59 * time.Second)
	_, _ = cs.BankStats(ctx)
	if n := fs.count("BankStats"); n != 1 {
		t.Errorf("BankStats 59 s later: %d calls, want 1", n)
	}
	clk.add(2 * time.Second)
	_, _ = cs.BankStats(ctx)
	if n := fs.count("BankStats"); n != 2 {
		t.Errorf("BankStats 61 s later: %d calls, want 2", n)
	}

	fs.set(func(f *fakeServer) { f.fail = errors.New("the server failed") })
	if _, err := cs.ProjectTags(ctx); err == nil {
		t.Fatal("ProjectTags: want the server's error")
	}
	fs.set(func(f *fakeServer) { f.fail = nil })
	for range 2 {
		if _, err := cs.ProjectTags(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := fs.count("ProjectTags"); n != 2 {
		t.Errorf("a failing call then two: %d calls, want 2 (the error is not kept)", n)
	}

	for _, period := range []string{"7d", "30d", "7d"} {
		if _, err := cs.LLMStats(ctx, period, ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := fs.count("LLMStats"); n != 2 {
		t.Errorf("LLMStats 7d, 30d, 7d: %d calls, want 2", n)
	}

	for range 2 {
		m, err := cs.Memory(ctx, "m")
		if err != nil || m.Text != "a memory's text" {
			t.Fatalf("Memory: %+v, %v", m, err)
		}
	}
	if n := fs.count("Memory"); n != 2 {
		t.Errorf("Memory twice: %d calls, want 2 (never cached)", n)
	}
}

func TestDoctorCached5min(t *testing.T) {
	clk := &clock{t: indexgen.Now}
	var (
		mu      sync.Mutex
		runs    int
		gate    chan struct{}
		fail    error
		entered = make(chan struct{}, 8)
	)
	checks := []metrics.DoctorCheck{{ID: "server", Status: "ok"}}
	run := func(ctx context.Context) ([]metrics.DoctorCheck, error) {
		mu.Lock()
		runs++
		g, err := gate, fail
		mu.Unlock()
		entered <- struct{}{}
		if g != nil {
			select {
			case <-g:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return checks, err
	}
	nruns := func() int {
		mu.Lock()
		defer mu.Unlock()
		return runs
	}
	doc := cachedDoctor(context.Background(), run, clk.now)
	ctx := context.Background()
	for range 2 {
		got, err := doc(ctx)
		if err != nil || !reflect.DeepEqual(got, checks) {
			t.Fatalf("doctor: %v, %v", got, err)
		}
	}
	if n := nruns(); n != 1 {
		t.Errorf("two calls: %d runs, want 1", n)
	}
	clk.add(5*time.Minute + time.Second)
	_, _ = doc(ctx)
	if n := nruns(); n != 2 {
		t.Errorf("5 min 1 s later: %d runs, want 2", n)
	}

	// A failed run is not kept.
	clk.add(5*time.Minute + time.Second)
	mu.Lock()
	fail = errors.New("the doctor failed")
	mu.Unlock()
	if _, err := doc(ctx); err == nil {
		t.Fatal("want the run's error")
	}
	mu.Lock()
	fail = nil
	mu.Unlock()
	if _, err := doc(ctx); err != nil {
		t.Fatal(err)
	}
	if n := nruns(); n != 4 {
		t.Errorf("after a failed run: %d runs, want 4", n)
	}

	// A run whose first caller leaves still ends, for the next caller.
	clk.add(5*time.Minute + time.Second)
	for len(entered) > 0 {
		<-entered
	}
	release := make(chan struct{})
	mu.Lock()
	gate = release
	mu.Unlock()
	ctx1, cancel1 := context.WithCancel(ctx)
	left := make(chan error, 1)
	go func() {
		_, err := doc(ctx1)
		left <- err
	}()
	<-entered
	cancel1()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that left: %v, want context.Canceled", err)
	}
	close(release)
	got, err := doc(ctx)
	if err != nil || !reflect.DeepEqual(got, checks) {
		t.Fatalf("the next caller: %v, %v", got, err)
	}
	if n := nruns(); n != 5 {
		t.Errorf("%d runs, want 5 (the held run's result reused)", n)
	}
}
