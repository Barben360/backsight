package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// parityDashboard is the dashboard on the fixture of the golden views
// (viewsFixture, the fixed doctor, the frozen clock, the fake server up;
// browseFixture too when browse) with a signed-in session: its base URL and
// cookie.
func parityDashboard(t *testing.T, browse bool) (string, *http.Cookie) {
	t.Helper()
	return parityDashboardWith(t, browse, nil)
}

// parityDashboardWith is parityDashboard with setup run on the fixture
// before the index is built (the other PCs' cache, for one).
func parityDashboardWith(t *testing.T, browse bool, setup func(*testing.T)) (string, *http.Cookie) {
	t.Helper()
	viewsFixture(t)
	fixedDoctor(t)
	if browse {
		browseFixture(t)
	}
	if setup != nil {
		setup(t)
	}
	buildIndex(t)
	base, _, first, _, _, _ := serveDashboard(t)
	return base, dashLogin(t, base, first)
}

// dashGet is a GET of path with the session: the status and the body.
func dashGet(t *testing.T, base string, c *http.Cookie, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// pair is one query parameter.
type pair [2]string

// parityTZ is the zone both sides read days in unless a query names one.
const parityTZ = "Europe/Paris"

// apiPath is the API path of a view, an id and a query.
func apiPath(view, id string, q []pair) string {
	p := "/api/v1/" + view
	if id != "" {
		p += "/" + id
	}
	v := url.Values{}
	if !slices.ContainsFunc(q, func(x pair) bool { return x[0] == "tz" }) {
		v.Set("tz", parityTZ)
	}
	for _, x := range q {
		v.Add(x[0], x[1])
	}
	return p + "?" + v.Encode()
}

// cliArgs is `stats <view> [<id>] <flags>` for the same query (runView adds
// --json): every parameter is a flag of the same name.
func cliArgs(view, id string, q []pair) []string {
	args := []string{"stats", view}
	if id != "" {
		args = append(args, id)
	}
	if !slices.ContainsFunc(q, func(x pair) bool { return x[0] == "tz" }) {
		args = append(args, "--tz", parityTZ)
	}
	for _, x := range q {
		args = append(args, "--"+x[0]+"="+x[1])
	}
	return args
}

// envelope is a decoded answer: the whole JSON, as a generic tree.
type envelope map[string]any

func decodeEnvelope(t *testing.T, what, raw string) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("%s: not an envelope: %v: %.300q", what, err, raw)
	}
	return e
}

// withoutIndex drops what differs by design between the two sides: the
// freshness of the index each one read (data.index, and its copy in the
// system view).
func (e envelope) withoutIndex() envelope {
	if d, ok := e["data"].(map[string]any); ok {
		delete(d, "index")
		if tb, ok := d["tables"].(map[string]any); ok {
			if sys, ok := tb["system"].(map[string]any); ok {
				delete(sys, "index")
			}
		}
	}
	return e
}

// firstDiff is the path of the first difference between two decoded
// values, "" when they are deep-equal.
func firstDiff(path string, a, b any) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return path + " (type)"
		}
		keys := map[string]bool{}
		for k := range x {
			keys[k] = true
		}
		for k := range y {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			xv, xok := x[k]
			yv, yok := y[k]
			if xok != yok {
				return path + "." + k + " (missing on one side)"
			}
			if d := firstDiff(path+"."+k, xv, yv); d != "" {
				return d
			}
		}
		return ""
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return path + " (length)"
		}
		for i := range x {
			if d := firstDiff(fmt.Sprintf("%s[%d]", path, i), x[i], y[i]); d != "" {
				return d
			}
		}
		return ""
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Sprintf("%s (%v vs %v)", path, a, b)
	}
	return ""
}

// errorOf is an envelope's error code and message.
func errorOf(e envelope) (code any, message any) {
	er, _ := e["error"].(map[string]any)
	return er["code"], er["message"]
}

// The dashboard says what the command line says (acceptance): for every
// view, window, filter and id, the API's body and `stats <view> --json`
// parsed are deep-equal but for the freshness of the index each read; for
// every error class they carry the same code and message. Each of since,
// until, tz, project and path crosses both sides, and each is shown to
// change the answer, so a parameter that one side ignores cannot pass.
func TestAPIEqualsTheCLI(t *testing.T) {
	base, c := parityDashboard(t, true)
	bodies := map[string]string{} // by "view|id|query", the API's body

	type success struct {
		view, id string
		q        []pair
	}
	var ok []success
	windows := [][]pair{
		nil,
		{{"days", "1"}},
		{{"days", "30"}, {"project", "p1"}},
		{{"since", "2026-10-01"}, {"until", "2026-10-08"}},
	}
	for _, view := range metrics.Views {
		for _, q := range windows {
			ok = append(ok, success{view, "", q})
		}
	}
	ok = append(ok,
		success{"sessions", "", []pair{{"has", "used"}}},
		success{"sessions", "", []pair{{"limit", "2"}, {"offset", "1"}}},
		// the same window, one parameter apart: the dashboard's cache must
		// tell them apart
		success{"sessions", "", []pair{{"limit", "2"}, {"offset", "0"}}},
		success{"sessions", "", []pair{{"limit", "1"}, {"offset", "0"}}},
		success{"quality", "", []pair{{"path", "search"}}},
		success{"memories", "", []pair{{"sort", "rate"}, {"state", "never_used"}}},
		success{"quality", "", []pair{{"dev", "1"}}},
		success{"quality", "", []pair{{"path", "prompt"}}},
		success{"quality", "", []pair{{"path", "search"}, {"days", "30"}}},
		success{"overview", "", []pair{{"project", "p2"}, {"days", "30"}}},
		success{"overview", "", []pair{{"tz", "America/Los_Angeles"}}},
		success{"days", "", []pair{{"tz", "Pacific/Auckland"}, {"days", "30"}}},
		success{"days", "", []pair{{"days", "30"}}},
		success{"overview", "", []pair{{"since", "2026-10-07T12:00Z"}, {"tz", "Asia/Tokyo"}}},
		success{"projects", "p1", nil},
		success{"sessions", "sess-long-1", nil},
		success{"memories", "mem-long-1", nil},
		success{"days", "2026-10-08", nil},
		success{"defs", "recall.hit_rate", nil},
		// the block's parameters: a comparison of two versions, a measure,
		// the sessions the pipeline lost or a correction followed, and the
		// developer metrics of the pipeline and the bank
		success{"releases", "", []pair{{"compare", "v1,v2"}}},
		success{"releases", "", []pair{{"compare", "v1,v2"}, {"measure", "recall.hit_rate"}}},
		success{"sessions", "", []pair{{"has", "never_extracted"}}},
		success{"sessions", "", []pair{{"has", "contradicted"}}},
		success{"pipeline", "", []pair{{"dev", "1"}}},
		success{"bank", "", []pair{{"dev", "1"}, {"days", "30"}}},
	)
	for _, s := range ok {
		query := url.Values{}
		for _, x := range s.q {
			query.Add(x[0], x[1])
		}
		name := s.view + "/" + s.id + "?" + query.Encode()
		t.Run(name, func(t *testing.T) {
			status, raw := dashGet(t, base, c, apiPath(s.view, s.id, s.q))
			cli := runView(t, cliArgs(s.view, s.id, s.q)...)
			if status != http.StatusOK || cli.code != output.OK {
				t.Fatalf("API %d, CLI exit %d\nAPI: %.300s\nCLI: %.300s", status, cli.code, raw, cli.raw)
			}
			api := decodeEnvelope(t, "API", raw)
			cliEnv := decodeEnvelope(t, "CLI", cli.raw)
			if got := at(map[string]any(api), "data", "view"); got != s.view {
				t.Fatalf("the API answered the view %v, not %s", got, s.view)
			}
			if d := firstDiff("", api.withoutIndex(), cliEnv.withoutIndex()); d != "" {
				t.Errorf("the API and the CLI differ at %s", d)
			}
			bodies[name] = raw
		})
	}

	// Each parameter changes what is answered: were one ignored by a side,
	// the equality above would still hold only if the CLI ignored it too.
	differ := func(a, b string) {
		t.Helper()
		if bodies[a] == "" || bodies[b] == "" {
			t.Fatalf("no body for %q or %q", a, b)
		}
		x := decodeEnvelope(t, a, bodies[a]).withoutIndex()
		y := decodeEnvelope(t, b, bodies[b]).withoutIndex()
		delete(x["data"].(map[string]any), "window") // the windows differ anyway
		delete(y["data"].(map[string]any), "window")
		if firstDiff("", x, y) == "" {
			t.Errorf("%q and %q answer the same: the parameter changes nothing", a, b)
		}
	}
	differ("quality/?path=prompt", "quality/?days=30&path=search")
	// requested in this order in one run, at the same window: a cache key
	// that drops one of these serves the first body to the second request
	differ("sessions/?limit=2&offset=0", "sessions/?limit=2&offset=1")
	differ("sessions/?limit=1&offset=0", "sessions/?limit=2&offset=0")
	differ("quality/?path=prompt", "quality/?path=search")
	differ("overview/?days=30&project=p2", "overview/?days=30&project=p1")
	differ("overview/?tz=America%2FLos_Angeles", "overview/?")
	differ("days/?days=30&tz=Pacific%2FAuckland", "days/?days=30")
	differ("overview/?since=2026-10-01&until=2026-10-08", "overview/?")
	differ("releases/?compare=v1%2Cv2", "releases/?")
	differ("releases/?compare=v1%2Cv2", "releases/?compare=v1%2Cv2&measure=recall.hit_rate")

	// the errors: the same code and the same sentence, the HTTP status of
	// their class
	for _, e := range []struct {
		name, view, id string
		q              []pair
		status         int
	}{
		{"offset below zero", "sessions", "", []pair{{"offset", "-1"}}, http.StatusBadRequest},
		{"limit above the maximum", "sessions", "", []pair{{"limit", "501"}}, http.StatusBadRequest},
		{"days out of range", "overview", "", []pair{{"days", "0"}}, http.StatusBadRequest},
		{"days and since", "overview", "", []pair{{"days", "3"}, {"since", "2026-10-01"}}, http.StatusBadRequest},
		{"until alone", "overview", "", []pair{{"until", "2026-10-01"}}, http.StatusBadRequest},
		// windows the index could never fill: bounded before a bucket is built
		{"window of a century", "overview", "", []pair{{"since", "2000-01-01"}, {"until", "2099-12-31"}}, http.StatusBadRequest},
		{"until in year 9999", "overview", "", []pair{{"since", "2026-01-03"}, {"until", "9999-12-31"}}, http.StatusBadRequest},
		{"since in year 1", "overview", "", []pair{{"since", "0001-01-01"}}, http.StatusBadRequest},
		{"a week in year 9999", "days", "", []pair{{"since", "9999-12-01"}, {"until", "9999-12-08"}}, http.StatusBadRequest},
		{"window one day over the cap", "overview", "", []pair{{"since", "2025-09-01"}, {"until", "2026-10-07"}}, http.StatusBadRequest},
		{"unknown zone", "overview", "", []pair{{"tz", "Mars/Olympus"}}, http.StatusBadRequest},
		{"unknown filter value", "sessions", "", []pair{{"has", "nothing"}}, http.StatusBadRequest},
		{"unknown id", "sessions", "zzzzzzzzzz", nil, http.StatusNotFound},
		{"unknown project", "projects", "nope", nil, http.StatusNotFound},
		{"unknown metric", "defs", "nope.metric", nil, http.StatusNotFound},
		{"ambiguous id", "sessions", "sess-lon", nil, http.StatusNotFound},
		{"too-short id", "sessions", "sess", nil, http.StatusNotFound},
		{"unknown memory", "memories", "mem-nothing-here", nil, http.StatusNotFound},
		{"day not a date", "days", "2026-13-01", nil, http.StatusNotFound},
		{"compare with one version", "releases", "", []pair{{"compare", "v1"}}, http.StatusBadRequest},
		{"compare with the same version twice", "releases", "", []pair{{"compare", "v1,v1"}}, http.StatusBadRequest},
		{"compare with an unknown measure", "releases", "", []pair{{"compare", "v1,v2"}, {"measure", "bogus"}}, http.StatusBadRequest},
		{"measure without compare", "releases", "", []pair{{"measure", "recall.hit_rate"}}, http.StatusBadRequest},
	} {
		t.Run("error: "+e.name, func(t *testing.T) {
			status, raw := dashGet(t, base, c, apiPath(e.view, e.id, e.q))
			cli := runView(t, cliArgs(e.view, e.id, e.q)...)
			if cli.code == output.OK || status < 400 {
				t.Fatalf("API %d, CLI exit %d: not an error\nAPI: %.300s\nCLI: %.300s", status, cli.code, raw, cli.raw)
			}
			if status != e.status {
				t.Errorf("API status %d, want %d", status, e.status)
			}
			api := decodeEnvelope(t, "API", raw)
			cliEnv := decodeEnvelope(t, "CLI", cli.raw)
			ac, am := errorOf(api)
			cc, cm := errorOf(cliEnv)
			if am == nil || am == "" || ac != cc || am != cm {
				t.Errorf("API error %v %q, CLI error %v %q", ac, am, cc, cm)
			}
			if api["ok"] != false || cliEnv["ok"] != false {
				t.Errorf("ok: API %v, CLI %v", api["ok"], cliEnv["ok"])
			}
		})
	}

	// The longest window taken, 400 days, is a result on both sides.
	t.Run("window exactly at the cap", func(t *testing.T) {
		q := []pair{{"since", "2025-09-01"}, {"until", "2026-10-06"}}
		status, raw := dashGet(t, base, c, apiPath("overview", "", q))
		cli := runView(t, cliArgs("overview", "", q)...)
		if status != http.StatusOK || cli.code != output.OK {
			t.Fatalf("API %d, CLI exit %d\nAPI: %.300s\nCLI: %.300s", status, cli.code, raw, cli.raw)
		}
		w := decodeEnvelope(t, "API", raw)["data"].(map[string]any)["window"].(map[string]any)
		if w["days"] != float64(400) {
			t.Errorf("window days %v, want 400", w["days"])
		}
	})

	// dev=2: the API refuses it with the sentence the CLI's other flags
	// use; the CLI cannot be compared, its flag parser answers first.
	t.Run("error: dev not a boolean", func(t *testing.T) {
		status, raw := dashGet(t, base, c, apiPath("quality", "", []pair{{"dev", "2"}}))
		_, msg := errorOf(decodeEnvelope(t, "API", raw))
		if status != http.StatusBadRequest || msg != "--dev: 1, true, 0 or false" {
			t.Errorf("API %d %v", status, msg)
		}
		if code, _, _ := run(t, "stats", "quality", "--dev=2", "--json"); code != output.Usage {
			t.Errorf("CLI exit %d, want %d", code, output.Usage)
		}
	})
}

// The same with the other PCs: for the views that merge them and the pages'
// ids, the API's body with machines=all and `stats <view> --machines all
// --json` are deep-equal, on a fixture with two other PCs (one gone quiet)
// and one document set aside. The scope and the merged figures differ from
// this PC's alone, so a side that drops the parameter cannot pass.
func TestAPIEqualsTheCLIAllPCs(t *testing.T) {
	base, c := parityDashboardWith(t, true, otherPCsFixture)
	all := pair{"machines", "all"}
	var bodies = map[string]string{}
	for _, s := range []struct {
		name, view, id string
		q              []pair
		this           bool // a view that always answers this PC's figures
	}{
		{"overview", "overview", "", nil, false},
		{"quality", "quality", "", nil, false},
		{"cost", "cost", "", nil, false},
		{"days", "days", "", nil, false},
		{"days/2026-10-07", "days", "2026-10-07", nil, false},
		{"releases", "releases", "", nil, false},
		{"system", "system", "", nil, false},
		{"memories/mem-long-1", "memories", "mem-long-1", nil, true},
		{"overview, a project", "overview", "", []pair{{"project", "p1"}}, false},
		{"cost, a month", "cost", "", []pair{{"days", "30"}}, false},
		{"overview, a day by the hour", "overview", "", []pair{{"days", "1"}}, false},
	} {
		t.Run(s.name, func(t *testing.T) {
			q := append(slices.Clone(s.q), all)
			status, raw := dashGet(t, base, c, apiPath(s.view, s.id, q))
			cli := runView(t, cliArgs(s.view, s.id, q)...)
			if status != http.StatusOK || cli.code != output.OK {
				t.Fatalf("API %d, CLI exit %d\nAPI: %.300s\nCLI: %.300s", status, cli.code, raw, cli.raw)
			}
			api, cliEnv := decodeEnvelope(t, "API", raw), decodeEnvelope(t, "CLI", cli.raw)
			if d := firstDiff("", api.withoutIndex(), cliEnv.withoutIndex()); d != "" {
				t.Errorf("the API and the CLI differ at %s", d)
			}
			bodies[s.name] = raw
			want := "all"
			if s.this {
				want = "this" // a browse view and an id page: this PC's figures, and the scope says so
			}
			if got := at(map[string]any(api), "data", "scope", "machines"); got != want {
				t.Errorf("the API answered scope.machines %v, not %s", got, want)
			}
			// the parameter changes the answer: this PC alone reads another scope
			if _, alone := dashGet(t, base, c, apiPath(s.view, s.id, s.q)); alone == raw && !s.this {
				t.Errorf("machines=all answers what this PC alone does")
			}
		})
	}
	if len(bodies) == 0 {
		t.Fatal("no case ran")
	}
	// the figures are the merged ones: the overview's rate counts the
	// other PCs' prompts (288 of the fixture's 8 days of 36, on this PC's 7)
	api := decodeEnvelope(t, "overview", bodies["overview"])
	if den := at(map[string]any(api), "data", "metrics", "recall.hit_rate", "den"); den != float64(295) {
		t.Errorf("recall.hit_rate den = %v, want 295", den)
	}
	if n := at(map[string]any(api), "data", "scope", "pcs", "count"); n != float64(3) {
		t.Errorf("scope.pcs.count = %v, want 3", n)
	}
}

// An index that cannot be opened because an update is being applied is
// the same refusal on both sides.
func TestAPIEqualsTheCLIWhileAnUpdateRuns(t *testing.T) {
	viewsFixture(t)
	buildIndex(t)
	d, _, err := machine()
	if err != nil {
		t.Fatal(err)
	}
	needsMigration(t, d)
	base, _, first, _, _, _ := serveDashboard(t)
	c := dashLogin(t, base, first)

	status, raw := dashGet(t, base, c, "/api/v1/overview")
	cli := runView(t, "stats", "overview")
	if status != http.StatusServiceUnavailable || cli.code != output.Blocked {
		t.Fatalf("API %d, CLI exit %d\nAPI: %.300s\nCLI: %.300s", status, cli.code, raw, cli.raw)
	}
	ac, am := errorOf(decodeEnvelope(t, "API", raw))
	if ac != float64(cli.env.Error.Code) || am != cli.env.Error.Message || am == "" {
		t.Errorf("API error %v %q, CLI error %v %q", ac, am, cli.env.Error.Code, cli.env.Error.Message)
	}
}

// pagesDeclared are the views each template declares in its header, by
// file name; templates that read nothing are left out.
func pagesDeclared(t *testing.T) map[string][]string {
	t.Helper()
	header := regexp.MustCompile(`\A\{\{/\* views: ([a-z]+(?:, [a-z]+)*|none) \*/\}\}\n`)
	files, err := fs.Glob(dashboard.Templates(), "*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	out := map[string][]string{}
	for _, f := range files {
		src, err := fs.ReadFile(dashboard.Templates(), f)
		if err != nil {
			t.Fatal(err)
		}
		m := header.FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s has no views header", f)
		}
		if string(m[1]) != "none" {
			out[f] = strings.Split(string(m[1]), ", ")
		}
	}
	return out
}

// Every view a page reads is one the API serves and a `stats` subcommand
// prints: the pages show nothing the command line cannot.
func TestEveryPageReadsServedViews(t *testing.T) {
	// "served", not "fast": the 10 s request deadline can expire on a busy
	// runner (windows-latest answered 503 "the page took too long"); go
	// test's -timeout ends a hang.
	t.Cleanup(dashboard.SetRenderTimeout(time.Hour))
	base, c := parityDashboard(t, true)
	declared := pagesDeclared(t)
	if len(declared) < 16 {
		t.Fatalf("%d pages declare views: want the 16 pages of the dashboard", len(declared))
	}
	root := newRoot(&env{stdout: io.Discard, stderr: io.Discard})
	stats, _, err := root.Find([]string{"stats"})
	if err != nil || stats.Name() != "stats" {
		t.Fatalf("no stats command: %v", err)
	}
	subs := map[string]bool{}
	for _, sub := range stats.Commands() {
		subs[sub.Name()] = true
	}
	seen := map[string]bool{}
	for page, vs := range declared {
		for _, v := range vs {
			if !subs[v] {
				t.Errorf("%s reads %q, which is not a stats subcommand", page, v)
			}
			if seen[v] {
				continue
			}
			seen[v] = true
			if status, body := dashGet(t, base, c, "/api/v1/"+v); status != http.StatusOK {
				t.Errorf("%s reads %q: GET /api/v1/%s answers %d: %.200s", page, v, v, status, body)
			}
		}
	}
	for _, v := range metrics.Views {
		if !seen[v] {
			t.Errorf("no page reads the view %q", v)
		}
	}
}

// The overview page shows the numbers of the fixture, hand-checked against
// testdata/stats-view-overview.golden (the same fixture, the same zone):
// 4 prompts of 5 got memory, the Found and Right axes are still learning
// (day 2 of about 7, 6 prompts so far) and Flowing needs attention. The page
// asks the question where the command prints the label.
func TestPagesShowTheFixturesNumbers(t *testing.T) {
	base, c := parityDashboard(t, false)
	status, page := dashGet(t, base, c, "/?tz="+parityTZ)
	if status != http.StatusOK {
		t.Fatalf("/: %d", status)
	}
	text := visibleText(page)
	for _, want := range []string{
		"Does memory show up when you ask something? 4 of 5 prompts got memory too few",
		"4 of 5: too few to judge.",
		"Found learning day 2 of about 7",
		"Flowing needs attention (2 reasons).",
		"Flowing attention 1 of 1 item sent",
		"watch Health check «memory» failed.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the overview does not show %q:\n%s", want, text)
		}
	}
	// the same 4 of 5 in the API, which the golden pins as the CLI's
	status, raw := dashGet(t, base, c, "/api/v1/overview?tz="+parityTZ)
	m := decodeEnvelope(t, "API", raw)
	hit := at(map[string]any(m), "data", "metrics", "recall.hit_rate")
	if status != http.StatusOK || at(hit, "num") != float64(4) || at(hit, "den") != float64(5) {
		t.Errorf("recall.hit_rate: %d %v, want 4 of 5", status, hit)
	}
}

// An id that names nothing, names several or is too short is told in the
// words of the command line, as a not-found page, never an error page.
func TestPageIDMessages(t *testing.T) {
	base, c := parityDashboard(t, true)
	// A page tells a message as a sentence: its first letter upper-cased
	// and a full stop at the end. An id that names nothing, or is written
	// wrongly in the path, is a 404.
	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{"/sessions/sess-lon", `Ambiguous session id "sess-lon": sess-long-1, sess-long-2.`, http.StatusNotFound},
		{"/sessions/sess", "Session id: give at least 8 characters.", http.StatusNotFound},
		{"/days/2026-13-01", "A day is written YYYY-MM-DD.", http.StatusNotFound},
		{"/projects/nope", `No project "nope" in the window.`, http.StatusNotFound},
	} {
		t.Run(tc.path, func(t *testing.T) {
			status, page := dashGet(t, base, c, tc.path)
			if status != tc.status {
				t.Errorf("status %d, want %d", status, tc.status)
			}
			if !strings.Contains(visibleText(page), tc.want) {
				t.Errorf("the page does not say %q:\n%s", tc.want, visibleText(page))
			}
		})
	}
}

// needsMigration puts the built index back to a schema older than this
// binary's and marks an update as uncommitted: opening the index must now
// wait for the update.
func needsMigration(t *testing.T, d paths.Dirs) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(d.IndexDir(), index.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteUpdateMarker(d, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
		t.Fatal(err)
	}
}
