package dashboard

import (
	"errors"
	"html"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// pageGet is the status and unescaped body of GET path under ck.
func pageGet(t *testing.T, h *harness, ck *http.Cookie, path string) (*http.Response, string) {
	t.Helper()
	r := h.do("GET", path, cookieHdr(ck)...)
	return r, html.UnescapeString(body(t, r))
}

func TestPageHeadersDeclareViews(t *testing.T) {
	files, err := fs.Glob(Templates(), "*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates: %v, %v", files, err)
	}
	for _, f := range files {
		src, err := fs.ReadFile(Templates(), f)
		if err != nil {
			t.Fatal(err)
		}
		names, ok := declared(src)
		if !ok {
			t.Errorf("%s: no views header", f)
		}
		for _, v := range names {
			if !slices.Contains(metrics.Views, v) {
				t.Errorf("%s: %q is not a view", f, v)
			}
		}
	}
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Kind != "page" {
			continue
		}
		if sp, ok := specs[r.Page]; !ok || len(sp.Views) == 0 || tpls[r.Page] == nil {
			t.Errorf("page route %s: spec %+v", r.Pattern, sp)
		}
	}
	for _, none := range []string{"frame", "state", "unauthorized"} {
		if _, ok := specs[none]; ok {
			t.Errorf("%s is not a page", none)
		}
	}
	if got := specs["overview"].Views; !slices.Equal(got, []string{"overview"}) {
		t.Errorf("overview reads %v", got)
	}

	frame := fstest.MapFS{"frame.html": {Data: []byte("{{/* views: none */}}\n")}}
	bad := fstest.MapFS{"frame.html": frame["frame.html"], "x.html": {Data: []byte("<p>no header</p>")}}
	if _, _, err := loadPages(bad); err == nil || !strings.Contains(err.Error(), "x.html") {
		t.Errorf("a file without the header: %v", err)
	}
	twice := fstest.MapFS{"frame.html": frame["frame.html"], "x.html": {Data: []byte("{{/* views: overview, overview */}}\n")}}
	if _, _, err := loadPages(twice); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a view declared twice: %v", err)
	}
	unknown := fstest.MapFS{"frame.html": frame["frame.html"], "x.html": {Data: []byte("{{/* views: nonsense */}}\n")}}
	if _, _, err := loadPages(unknown); err == nil {
		t.Error("an unknown view is accepted")
	}
	a, _, err1 := loadPages(Templates())
	b, _, err2 := loadPages(Templates())
	if err1 != nil || err2 != nil || a["overview"] == b["overview"] {
		t.Errorf("two loads share a template set: %v, %v", err1, err2)
	}
}

func TestPageParams(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	for _, c := range []struct {
		path   string
		status int
		want   string
	}{
		{"/quality?limit=99999", 400, "--limit: a whole number from 1 to 500"},
		{"/quality?days=1&days=2", 400, "Give days once."},
		{"/quality?has=used", 400, "Unknown parameter"},
		{"/quality?days=1;x", 400, "The query string is malformed."},
		{"/sessions?has=used", 200, ""},
	} {
		r, b := pageGet(t, h, ck, c.path)
		if r.StatusCode != c.status || !strings.Contains(b, c.want) {
			t.Errorf("GET %s: %d, want %d with %q in %q", c.path, r.StatusCode, c.status, c.want, b)
		}
	}
	_, plain := pageGet(t, h, ck, "/quality")
	_, dev := pageGet(t, h, ck, "/quality?dev=1")
	_, off := pageGet(t, h, ck, "/quality?dev=0")
	if strings.Contains(plain, "Developer") || !strings.Contains(dev, "Developer") || strings.Contains(off, "Developer") {
		t.Errorf("developer section: without %t, dev=1 %t, dev=0 %t; want false, true, false",
			strings.Contains(plain, "Developer"), strings.Contains(dev, "Developer"), strings.Contains(off, "Developer"))
	}
	if !strings.Contains(dev, "Releases") || !strings.Contains(dev, "Definitions") {
		t.Errorf("dev=1 lacks the developer pages: %q", dev)
	}
}

func TestStatePages(t *testing.T) {
	cases := []struct {
		name string
		h    func(t *testing.T) *harness
		path string
		want string
	}{
		{"first build", func(t *testing.T) *harness {
			return newDataHarness(t, withIndex(func(f *fakeIndex) { f.openGate = make(chan struct{}) }), withServer(), withNeedsBuild())
		}, "/", firstBuildSentence},
		{"updating", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.openErrs = []error{index.ErrUpdateInProgress, index.ErrUpdateInProgress} })
		}, "/", sentence(views.UpdatingSentence)},
		{"updated", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.side = true })
		}, "/", "backsight was updated: restart the dashboard (Ctrl+C, then backsight dashboard)."},
		{"broken", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.openErrs = []error{index.ErrUpdateInProgress, errors.New("boom /secret/path")} })
		}, "/", "The metrics index could not be read"},
		{"catch-up meets an update", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.catchUpErr = index.ErrUpdateInProgress })
		}, "/quality", sentence(views.UpdatingSentence)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := c.h(t)
			r, b := pageGet(t, h, h.login(), c.path)
			if r.StatusCode != 503 || !strings.Contains(b, c.want) || strings.Contains(b, "secret") {
				t.Fatalf("%d %q, want 503 with %q", r.StatusCode, b, c.want)
			}
			refresh := strings.Contains(b, `<meta http-equiv="refresh" content="2">`)
			if building := c.name == "first build"; refresh != building || (r.Header.Get("Retry-After") == "2") != building {
				t.Errorf("refresh %t, Retry-After %q; want both only for the first build", refresh, r.Header.Get("Retry-After"))
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		old := renderTimeout
		renderTimeout = 20 * time.Millisecond
		t.Cleanup(func() { renderTimeout = old })
		h := dataHarness(t)
		h.ix.set(func(f *fakeIndex) { f.block = make(chan struct{}) })
		r, b := pageGet(t, h, h.login(), "/")
		if r.StatusCode != 503 || !strings.Contains(b, "The page took too long: try again.") {
			t.Errorf("%d %q", r.StatusCode, b)
		}
	})
	t.Run("unclassified failure", func(t *testing.T) {
		h := dataHarness(t, func(f *fakeIndex) { f.catchUpErr = errors.New("disk exploded at /home/x/secret.db") })
		r, b := pageGet(t, h, h.login(), "/")
		if r.StatusCode != 500 || !strings.Contains(b, "The view failed: run backsight doctor.") || strings.Contains(b, "secret") {
			t.Errorf("%d %q", r.StatusCode, b)
		}
	})
	t.Run("not found", func(t *testing.T) {
		h := dataHarness(t)
		r, b := pageGet(t, h, h.login(), "/sessions/zzzzzzzz")
		if r.StatusCode != 404 || !strings.Contains(b, `No session "zzzzzzzz" in the metrics index.`) {
			t.Errorf("%d %q", r.StatusCode, b)
		}
	})
}

func TestPageCounter(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	h.do("GET", "/", cookieHdr(ck)...)
	h.do("GET", "/quality", cookieHdr(ck)...)
	h.do("HEAD", "/", cookieHdr(ck)...) // sends no page
	h.do("GET", "/api/v1/overview", cookieHdr(ck)...)
	h.do("GET", assetPath("dashboard.css"), cookieHdr(ck)...)
	h.do("GET", "/quality")                           // 401
	h.do("GET", "/quality?limit=0", cookieHdr(ck)...) // 400
	if got := h.s.Pages(); got != 2 {
		t.Errorf("Pages() = %d after two pages, a HEAD, an API call, an asset, a 401 and a 400; want 2", got)
	}
	h.ix.set(func(f *fakeIndex) { f.side = true })
	if r := h.do("GET", "/cost", cookieHdr(ck)...); r.StatusCode != 503 {
		t.Fatalf("state page: %d", r.StatusCode)
	}
	if got := h.s.Pages(); got != 2 {
		t.Errorf("Pages() = %d after a state page; want 2", got)
	}
}

func TestFooterAndScope(t *testing.T) {
	h := dataHarness(t)
	_, b := pageGet(t, h, h.login(), "/")
	for _, want := range []string{
		"Help: backsight guide dashboard",
		"https://github.com/Barben360/backsight/blob/main/docs/dashboard.md",
		"backsight test",
		"memory on hindsight.test:8888",
		"Activity on this PC",
		"<h1>Is my memory working?</h1>",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("/ lacks %q", want)
		}
	}
}

func TestPageAndAPIShareOneResult(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	if r, _ := pageGet(t, h, ck, "/quality?days=30"); r.StatusCode != 200 {
		t.Fatalf("page: %d", r.StatusCode)
	}
	if r := h.do("GET", "/api/v1/quality?days=30", cookieHdr(ck)...); r.StatusCode != 200 {
		t.Fatalf("api: %d", r.StatusCode)
	}
	if cu, _, _ := h.ix.counts(); cu != 1 {
		t.Errorf("%d catch-ups for one page and one API call of the same view, want 1", cu)
	}
}

func TestNavigationMarksTheCurrentPage(t *testing.T) {
	h := dataHarness(t)
	_, b := pageGet(t, h, h.login(), "/cost")
	if !strings.Contains(b, `<a href="/cost" aria-current="page">Cost</a>`) || strings.Contains(b, `<a href="/" aria-current`) {
		t.Errorf("navigation: %q", b)
	}
}

func TestStateAndErrorPagesKeepTheFrame(t *testing.T) {
	h := dataHarness(t)
	r, b := pageGet(t, h, h.login(), "/quality?days=0")
	if r.StatusCode != 400 || !strings.Contains(b, "Help: backsight guide dashboard") || !strings.Contains(b, "--days: a whole number from 1 to 365") {
		t.Errorf("%d %q", r.StatusCode, b)
	}
}

func TestMetricFunc(t *testing.T) {
	r := &metrics.Result{Metrics: map[string]metrics.Metric{"recall.hit_rate": {Status: metrics.StatusOK}}}
	v, err := metricOf(r, "recall.hit_rate")
	if err != nil || !v.Found || v.Def.ID != "recall.hit_rate" {
		t.Fatalf("known metric: %+v, %v", v, err)
	}
	if v, err := metricOf(r, "bank.size"); err != nil || v.Found {
		t.Errorf("a metric the result lacks: %+v, %v", v, err)
	}
	if _, err := metricOf(r, "no.such"); err == nil {
		t.Error("an id outside the catalog is accepted")
	}
	if got := funcs["value"].(func(MetricView) string)(MetricView{}); got != "—" {
		t.Errorf("value of a missing metric %q", got)
	}
}

func TestWhen(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("no zoneinfo")
	}
	at := time.Date(2026, 10, 30, 22, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		loc  *time.Location
		in   any
		want string
	}{
		{paris, at, "2026-10-30 23:30 CET"},
		{nil, at, "2026-10-30 22:30 UTC"},
		{paris, &at, "2026-10-30 23:30 CET"},
		{paris, (*time.Time)(nil), "—"},
		{paris, time.Time{}, "—"},
		{paris, "2026", "—"},
	} {
		if got := when(c.loc, c.in); got != c.want {
			t.Errorf("when(%v, %v) = %q, want %q", c.loc, c.in, got, c.want)
		}
	}
}

func TestEchoedIDsAreEscaped(t *testing.T) {
	h := dataHarness(t)
	r := h.do("GET", "/sessions/%3Cscript%3Ex%26%22y", cookieHdr(h.login())...)
	raw := body(t, r)
	if r.StatusCode != 404 || strings.Contains(raw, "<script>") || !strings.Contains(raw, "&lt;script&gt;x&amp;\\&#34;y") {
		t.Errorf("%d, raw body %q: want the id escaped", r.StatusCode, raw)
	}
}

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{
		"no session": "No session.", "Done.": "Done.", "why?": "Why?", "--limit: a number": "--limit: a number.", "": "",
	} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAssetFunc(t *testing.T) {
	if p, err := assetOf("logo-light.svg"); err != nil || p != assetPath("logo-light.svg") {
		t.Errorf("known asset: %q, %v", p, err)
	}
	if _, err := assetOf("nope.css"); err == nil {
		t.Error("an unknown asset is accepted")
	}
	tpl := template.Must(template.New("x").Funcs(funcs).Parse(`{{asset "logo-lite.svg"}}`))
	if err := tpl.Execute(io.Discard, nil); err == nil {
		t.Error("a typo in an asset name does not fail the page")
	}
}
