package dashboard

import (
	"net/url"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
)

// baseRenderLimit is the render target (30 ms a page) times three, for a
// shared runner.
const baseRenderLimit = 90 * time.Millisecond

// renderLimit is baseRenderLimit, doubled on GitHub's Windows runner like
// pageLimit.
var renderLimit = runnerLimit(baseRenderLimit, runtime.GOOS, os.Getenv("GITHUB_ACTIONS"))

// TestRenderBudgets times the pages with the result cache on and warmed by
// one request per route: what the dashboard itself costs once the views are
// computed. TestPageBudgets is the cold measure.
func TestRenderBudgets(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("time budgets are measured without -race and -short")
	}
	oldSpec := dataSpec
	t.Cleanup(func() { dataSpec = oldSpec })
	dataSpec = indexgen.Budget

	h := dataHarness(t)
	ck := h.login()
	sample := map[string]string{
		"project": "/projects/" + url.PathEscape(apiPath(t, h, "/api/v1/projects").Tables.Projects[0].Project),
		"session": "/sessions/" + url.PathEscape(apiPath(t, h, "/api/v1/sessions").Tables.Sessions[0].ID),
		"memory":  "/memories/" + url.PathEscape(apiPath(t, h, "/api/v1/memories").Tables.Memories[0].ID),
	}
	var paths []string
	for _, rt := range routes {
		if rt.Kind != "page" {
			continue
		}
		path := rt.Sample
		if s, ok := sample[rt.Page]; ok {
			path = s
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		t.Fatal("no page route measured")
	}

	for _, path := range paths {
		const n = 20
		if r := h.do("GET", path, cookieHdr(ck)...); r.StatusCode != 200 { // the warming request
			t.Fatalf("GET %s: %d", path, r.StatusCode)
		} else {
			_ = body(t, r)
		}
		var ds []time.Duration
		for range n {
			start := time.Now()
			r := h.do("GET", path, cookieHdr(ck)...)
			_ = body(t, r)
			d := time.Since(start)
			if r.StatusCode != 200 {
				t.Fatalf("GET %s: %d", path, r.StatusCode)
			}
			ds = append(ds, d)
		}
		slices.Sort(ds)
		p95 := ds[18] // the 19th of 20
		t.Logf("%s: warm p95 %v, median %v, max %v (limit %v)", path, p95, ds[n/2], ds[n-1], renderLimit)
		if p95 > renderLimit {
			t.Errorf("%s: p95 %v, limit %v", path, p95, renderLimit)
		}
	}
}
