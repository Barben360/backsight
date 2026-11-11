package dashboard

import (
	"html"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/testtool/speed"
	"github.com/Barben360/backsight/internal/views"
)

// The budgets are the spec's targets (300 ms a page, 4 s with the server
// down) times three, for a shared runner.
const (
	basePageLimit  = 900 * time.Millisecond
	firstByteLimit = 12 * time.Second
)

// pageLimitFor is the page budget on a machine: base times the larger of
// the runner's measured speed factor (speed.Runner: 1 on a runner as fast
// as GitHub's fast Ubuntu runners, at most 1.5 on a slower one) and
// runnerLimit's fixed doubling on GitHub's Windows runner. Its pages took
// 1.1 to 1.6 s there where the Windows test VM's slowest was 709 ms, so the
// runner, not Windows, is slow; the VM keeps the base limit. The two are
// not multiplied: both say how slow the runner is.
func pageLimitFor(base time.Duration, goos, githubActions string, factor float64) time.Duration {
	return speed.Scale(base, max(float64(runnerLimit(base, goos, githubActions))/float64(base), factor))
}

// runnerLimit is base, doubled on GitHub's Windows runner.
func runnerLimit(base time.Duration, goos, githubActions string) time.Duration {
	if goos == "windows" && githubActions == "true" {
		return 2 * base
	}
	return base
}

func TestRunnerLimitDoublesOnlyOnGitHubWindows(t *testing.T) {
	for _, c := range []struct {
		goos, ci string
		want     time.Duration
	}{{"windows", "true", 2 * time.Second}, {"windows", "", time.Second}, {"linux", "true", time.Second}} {
		if got := runnerLimit(time.Second, c.goos, c.ci); got != c.want {
			t.Errorf("%s ci=%q: %v, want %v", c.goos, c.ci, got, c.want)
		}
	}
	if renderLimit != runnerLimit(baseRenderLimit, runtime.GOOS, os.Getenv("GITHUB_ACTIONS")) {
		t.Error("renderLimit does not go through runnerLimit")
	}
}

func TestPageLimitTakesTheSlowerOfTheRunnerFactorAndWindows(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		goos, ci string
		factor   float64
		want     time.Duration
	}{
		{"linux", "true", 1, 900 * ms},      // a fast runner: the base limit
		{"linux", "true", 1.5, 1350 * ms},   // the slowest: half more
		{"linux", "", 1.3, 1170 * ms},       // any machine is measured
		{"windows", "true", 1, 1800 * ms},   // GitHub's Windows runner: doubled, as before
		{"windows", "true", 1.4, 1800 * ms}, // its doubling already says it is slow
		{"windows", "", 1.2, 1080 * ms},     // the Windows VM: measured like any machine
	} {
		if got := pageLimitFor(900*ms, c.goos, c.ci, c.factor); got != c.want {
			t.Errorf("%s ci=%q factor %.1f: %v, want %v", c.goos, c.ci, c.factor, got, c.want)
		}
	}
}

// runnerClasses are the three speed classes of GitHub's Ubuntu runners in
// 43 CI runs (2026-11-09 to 11-11, main and feat/codex alike): the slowest
// /?machines=all best of 5 seen in each, and the reference speed.Runner
// measured on a runner of that class.
var runnerClasses = []struct {
	name      string
	page, ref time.Duration
}{
	{"fast", 567 * time.Millisecond, speed.Baseline},
	{"middle", 691 * time.Millisecond, refMiddle},
	{"slow", 929 * time.Millisecond, refSlow},
}

// The reference on the middle and slow runners (PROVISIONAL: replaced by
// the CI measurements).
const (
	refMiddle = 75 * time.Millisecond
	refSlow   = 110 * time.Millisecond
)

// TestADoubledPageFailsOnEveryRunner: on every runner class, today's
// heaviest page passes its scaled limit and a change that doubles its cost
// does not: the speed factor loosens the budget for the runner, never
// enough to hide a regression.
func TestADoubledPageFailsOnEveryRunner(t *testing.T) {
	for _, c := range runnerClasses {
		f := speed.FactorOf(c.ref, speed.Baseline)
		limit := pageLimitFor(basePageLimit, "linux", "true", f)
		if c.page > limit {
			t.Errorf("%s runner: today's page %v over its limit %v (factor %.2f)", c.name, c.page, limit, f)
		}
		if 2*c.page <= limit {
			t.Errorf("%s runner: a page twice as slow (%v) passes its limit %v (factor %.2f)", c.name, 2*c.page, limit, f)
		}
	}
}

// TestPageBudgets times the pages on 13 months of generated history with
// every cache off, then the first byte of a page with the server down.
func TestPageBudgets(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("time budgets are measured without -race and -short")
	}
	m, err := speed.Runner()
	if err != nil {
		t.Fatal(err)
	}
	pageLimit := pageLimitFor(basePageLimit, runtime.GOOS, os.Getenv("GITHUB_ACTIONS"), m.Factor)
	t.Logf("runner speed: %v; page limit %v (base %v)", m, pageLimit, basePageLimit)
	// Restored after the harness stops (cleanups run last in, first out).
	oldSpec, oldResult, oldServer := dataSpec, resultTTL, serverTTL
	t.Cleanup(func() { dataSpec, resultTTL, serverTTL = oldSpec, oldResult, oldServer })
	dataSpec = indexgen.Budget
	resultTTL, serverTTL = 0, 0 // every render computes, every server read goes out

	h := dataHarness(t)
	ck := h.login()
	get := func(path string) (*http.Response, time.Duration) {
		t.Helper()
		start := time.Now()
		r := h.do("GET", path, cookieHdr(ck)...)
		d := time.Since(start)
		_ = body(t, r)
		return r, d
	}

	for _, path := range []string{"/", "/quality"} {
		const n = 20
		var ds []time.Duration
		for range n {
			r, d := get(path)
			if r.StatusCode != 200 {
				t.Fatalf("GET %s: %d", path, r.StatusCode)
			}
			ds = append(ds, d)
		}
		slices.Sort(ds)
		p95 := ds[18] // the 19th of 20
		t.Logf("%s: p95 %v, median %v, max %v (limit %v)", path, p95, ds[n/2], ds[n-1], pageLimit)
		if p95 > pageLimit {
			t.Errorf("%s: p95 %v, limit %v", path, p95, pageLimit)
		}
	}

	// Every other page route, from the route table so a new page is measured;
	// a detail page is asked for a project, session and memory of the history.
	sample := map[string]string{
		"project": "/projects/" + url.PathEscape(apiPath(t, h, "/api/v1/projects").Tables.Projects[0].Project),
		"session": "/sessions/" + url.PathEscape(apiPath(t, h, "/api/v1/sessions").Tables.Sessions[0].ID),
		"memory":  "/memories/" + url.PathEscape(apiPath(t, h, "/api/v1/memories").Tables.Memories[0].ID),
	}
	others := 0
	for _, rt := range routes {
		if rt.Kind != "page" || rt.Sample == "/" || rt.Sample == "/quality" {
			continue
		}
		path := rt.Sample
		if s, ok := sample[rt.Page]; ok {
			path = s
		}
		r, cold := get(path) // the first render is logged, not held to the limit
		if r.StatusCode != 200 {
			t.Errorf("GET %s: %d", path, r.StatusCode)
		}
		// Best of 3, like TestViewBudgets: a shared runner's one-off stall is
		// not the page's cost. Every render computes (the caches are off).
		best := time.Duration(1<<63 - 1)
		for range 3 {
			_, d := get(path)
			best = min(best, d)
		}
		t.Logf("%s: first %v, best of 3 %v (limit %v)", path, cold, best, pageLimit)
		if best > pageLimit {
			t.Errorf("%s took %v, limit %v", path, best, pageLimit)
		}
		others++
	}
	if others == 0 {
		t.Fatal("no other page route measured")
	}

	// The same pages with the other PCs: the index holds the figures of 8 other PCs, and each page is asked
	// with machines=all. A page that also reads this PC's own answer (the Overview, Cost and the day page
	// do, for their "This PC's N" links) pays that read here. Best of 5, every render computes.
	t.Run("all PCs", func(t *testing.T) {
		budget := indexgen.Budget
		budget.OtherPCs = 8
		dataSpec = budget
		ha := dataHarness(t)
		dataSpec = indexgen.Budget
		cka := cookieHdr(ha.login())
		day := apiPath(t, ha, "/api/v1/days").Tables.Days[0].Day
		for _, path := range []string{"/", "/quality", "/cost", "/days", "/releases", "/system", "/pipeline", "/days/" + day} {
			path += "?machines=all"
			ask := func() (int, time.Duration, string) {
				start := time.Now()
				r := ha.do("GET", path, cka...)
				d := time.Since(start)
				return r.StatusCode, d, body(t, r)
			}
			status, cold, b := ask()
			if status != 200 || !strings.Contains(b, "all PCs on this server (9)") {
				t.Errorf("GET %s: %d, switch on all PCs: %v", path, status, strings.Contains(b, "all PCs on this server (9)"))
			}
			best := time.Duration(1<<63 - 1)
			for range 5 {
				_, d, _ := ask()
				best = min(best, d)
			}
			t.Logf("%s: first %v, best of 5 %v, %d bytes (limit %v)", path, cold, best, len(b), pageLimit)
			if best > pageLimit {
				t.Errorf("%s took %v, limit %v", path, best, pageLimit)
			}
		}
	})

	// The server down: every call blocks until its context ends, at the
	// real 3 s server timeout; the doctor cache was warmed at start.
	h.srv.set(func(f *fakeServer) { f.down = true })
	var first time.Duration
	start := time.Now()
	req, err := http.NewRequest("GET", "http://"+h.host()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", ck.Name+"="+ck.Value)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotFirstResponseByte: func() { first = time.Since(start) },
	}))
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b := html.UnescapeString(body(t, resp))
	total := time.Since(start)
	t.Logf("/ with the server down: first byte %v, whole page %v (limit %v)", first, total, firstByteLimit)
	if resp.StatusCode != 200 {
		t.Fatalf("/ with the server down: %d", resp.StatusCode)
	}
	if first == 0 || first > firstByteLimit {
		t.Errorf("first byte %v, limit %v", first, firstByteLimit)
	}
	sec := parseHTML(t, b).first("section.card[aria-labelledby=w-glance]")
	if sec == nil || !strings.Contains(sec.text(), "Memory server not answering") {
		t.Errorf("the memory-size card does not say the server is not answering: %v", sec)
	}
	hit, _ := metrics.DefOf("recall.hit_rate")
	res := apiResult(t, h, "overview")
	want := views.FormatValue(hit, res.Metrics["recall.hit_rate"])
	if want == "—" || !strings.Contains(visibleText(b), want) {
		t.Errorf("recall.hit_rate %q not shown with the server down", want)
	}
}
