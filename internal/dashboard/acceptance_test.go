package dashboard

import (
	"fmt"
	"html"
	"io/fs"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metrics"
)

// The acceptance sweeps read every page route on 13 months of history at
// low volume: sizes depend on positions and rows, not on volume.
var acceptanceSpec = indexgen.Spec{Months: 13, TelemetryPerYear: 20_000, JournalPerYear: 3_000, Memories: 500, Seed: 2, Collection: true}

const (
	sizeCap      = 307_200 // 300 KB
	longTurns    = 400
	hotShows     = 600
	year365Since = "2025-10-30"
	year365Until = "2026-10-30"
	// The longest custom window the picker takes on the generated clock (30 Oct 2026): from the first day
	// of the month 12 months back to tomorrow, 395 days.
	longSince = "2025-10-01"
	longUntil = "2026-10-31"
	// A comparison of the history's two versions (the long session's is the newer).
	compareQuery  = "compare=v0.1.17,v0.1.18"
	sweepWindow90 = "days=90"
)

var (
	charted   = map[string]bool{"overview": true, "quality": true, "cost": true, "bank": true, "pipeline": true, "system": true, "projects": true, "project": true, "day": true, "memory": true}
	devCharts = map[string]bool{"overview": true, "quality": true, "cost": true}
	// devPages are the pages whose body changes with the developer menu, and one list for the menu alone.
	devPages = map[string]bool{"overview": true, "quality": true, "cost": true, "bank": true, "pipeline": true, "system": true, "memory": true, "sessions": true}
	// defaultPages are the pages whose defaults differ from 90 days (their empty-state words and windows).
	defaultPages = map[string]bool{"overview": true, "system": true}
)

// accPage is one served page of the sweeps.
type accPage struct {
	Name string // what the failure message calls it
	Kind string // the route's Page ("overview", "quality", ..., "memory"), "text" for the text page, "scenario" for a drawn scenario
	Dev  bool
	Set  string // "90" (days=90), "365" (since/until), "long" (the longest custom window, shortcuts on), "compare" (two versions), "default" (no window), "all" (the memory with limit=500), "scenario"
	Body string
}

// accFail lists the pages that did not answer as expected while the set was built.
var accFail []string

var (
	accOnce  sync.Once
	accPages []accPage
)

// scenarioPage is the Overview drawn on scenario s.
func scenarioPage(t *testing.T, s scenario) (string, error) {
	t.Helper()
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		return "", err
	}
	spec := specs["overview"]
	d := fixtureData(t, spec, false)
	r := s.Result()
	d.V["overview"], d.Loc, d.L = r, r.Window.Loc, en
	d.P.Days, d.CLI = 7, "backsight stats overview"
	d.Frame.Nav, d.Frame.Prefs.Dev = navFor("/", false), false // the fixture frame has the developer menu on
	var sb strings.Builder
	err = tpls["overview"].ExecuteTemplate(&sb, spec.File, d)
	return sb.String(), err
}

// allAcceptancePages is every page route served on the 13 months of
// history, with the longest session and the hottest memory appended: at
// days=90 and at a 365-day since/until window (each with the developer menu
// off and on), at the longest custom window the picker takes (with the
// shortcuts on, the developer menu off and, on the pages it changes, on), at
// the defaults with it off, the hot memory with limit=500, a comparison of
// two versions, the text page, and the Overview on each scenario. Rendered once.
func allAcceptancePages(t *testing.T) []accPage {
	t.Helper()
	if raceEnabled {
		// Over 10 minutes on the CI runner under -race; make test runs the
		// sweep in its second line, without it.
		t.Skip("the page sweep runs without -race (make test)")
	}
	accOnce.Do(func() {
		oldSpec, oldTimeout := dataSpec, renderTimeout
		t.Cleanup(func() { dataSpec, renderTimeout = oldSpec, oldTimeout })
		// 13 months drawn over a year take seconds under -race: the sweep reads the pages, the budgets time them.
		dataSpec, renderTimeout = acceptanceSpec, time.Minute
		h := dataHarness(t)
		long := indexgen.AppendLongSession(t, h.dirs.State, longTurns)
		hot := indexgen.AppendHotMemory(t, h.dirs.State, hotShows)
		ck0 := h.login()
		ck := cookieHdr(ck0)
		get := func(p string, want int, hdr ...string) (string, bool) {
			if len(hdr) == 0 {
				hdr = ck
			}
			resp := h.do("GET", p, hdr...)
			b := body(t, resp)
			if resp.StatusCode != want {
				accFail = append(accFail, fmt.Sprintf("GET %s: %d, want %d", p, resp.StatusCode, want))
				return "", false
			}
			return b, true
		}
		// The largest item of each id form: the project and the day with the most sessions.
		proj, day := "", ""
		var most int64
		for _, p := range apiPath(t, h, "/api/v1/projects").Tables.Projects {
			if p.Sessions > most {
				proj, most = p.Project, p.Sessions
			}
		}
		most = 0
		for _, d := range apiPath(t, h, "/api/v1/days").Tables.Days {
			if d.Sessions > most {
				day, most = d.Day, d.Sessions
			}
		}
		if proj == "" || day == "" {
			t.Fatalf("no project (%q) or day (%q) in the history", proj, day)
		}
		for _, r := range routes {
			if r.Kind != "page" && r.Kind != "text" {
				continue
			}
			path := r.Sample
			switch r.Page {
			case "project":
				path = "/projects/" + url.PathEscape(proj)
			case "session":
				path = "/sessions/" + url.PathEscape(long)
			case "memory":
				path = "/memories/" + url.PathEscape(hot)
			case "day":
				path = "/days/" + day
			}
			if r.Kind == "text" {
				if b, ok := get("/memories/"+url.PathEscape(hot)+"/text", 200); ok {
					accPages = append(accPages, accPage{Name: path, Kind: "text", Set: "default", Body: b})
				}
				continue
			}
			for _, set := range []struct{ name, query string }{
				{"90", sweepWindow90 + "&"}, {"365", "since=" + year365Since + "&until=" + year365Until + "&"}, {"default", ""},
			} {
				// The 365-day window is for the pages that draw charts, the developer menu's extra charts
				// on the three that have them; the defaults only for the main pages, whose empty-state
				// words differ. Each page costs a second or more under -race.
				if set.name == "365" && !charted[r.Page] || set.name == "default" && !defaultPages[r.Page] {
					continue
				}
				for _, dev := range []bool{false, true} {
					if dev && (set.name == "default" || !devPages[r.Page] || set.name == "365" && !devCharts[r.Page]) {
						continue
					}
					q := set.query + "dev=" + map[bool]string{false: "0", true: "1"}[dev]
					if b, ok := get(path+"?"+q, 200); ok {
						accPages = append(accPages, accPage{Name: path + "?" + q, Kind: r.Page, Dev: dev, Set: set.name, Body: b})
					}
				}
			}
			// Every page at the longest window the picker allows, with the shortcuts on: the size cap's
			// worst case. The detail pages take the window like the lists.
			for _, dev := range []bool{false, true} {
				if dev && !devPages[r.Page] {
					continue
				}
				q := "since=" + longSince + "&until=" + longUntil + "&dev=" + map[bool]string{false: "0", true: "1"}[dev]
				if b, ok := get(path+"?"+q, 200, withPrefs(ck0, "v1.system.0.0.comfortable.1")...); ok {
					accPages = append(accPages, accPage{Name: path + "?" + q, Kind: r.Page, Dev: dev, Set: "long", Body: b})
				}
			}
			if r.Page == "day" {
				// The day the long session ran, on which the version changes: the markers card.
				d := "/days/" + indexgen.End().AddDate(0, 0, -1).Format("2006-01-02")
				q := sweepWindow90 + "&dev=0"
				if b, ok := get(d+"?"+q, 200); ok {
					accPages = append(accPages, accPage{Name: d + "?" + q, Kind: "day", Set: "markers", Body: b})
				}
			}
			if r.Page == "releases" {
				q := sweepWindow90 + "&" + compareQuery + "&dev=0"
				if b, ok := get(path+"?"+q, 200); ok {
					accPages = append(accPages, accPage{Name: path + "?" + q, Kind: "releases", Set: "compare", Body: b})
				}
			}
			if r.Page == "memory" {
				q := sweepWindow90 + "&limit=500&dev=0"
				if b, ok := get(path+"?"+q, 200); ok {
					accPages = append(accPages, accPage{Name: path + "?" + q, Kind: "memory", Set: "all", Body: b})
				}
			}
		}
		// Every page on all the PCs: a second history whose index holds 8 other PCs' figures (one gone
		// quiet, one document set aside), asked with machines=all at 90 days (the developer menu on
		// where it changes the page) and, for the four pages the longest window costs most, at the
		// longest custom window.
		accPCs(t, long, hot)
		// The framed failure pages, which carry a class of their kind.
		for _, c := range []struct {
			path string
			want int
		}{{"/sessions/zzzzzzzz", 404}, {"/quality?days=0", 400}} {
			if b, ok := get(c.path, c.want); ok {
				accPages = append(accPages, accPage{Name: "state " + c.path, Kind: "state", Set: "state", Body: b})
			}
		}
		// The preferences route asked for without a form, and the sign-in page.
		if b, ok := get("/prefs", 400); ok {
			accPages = append(accPages, accPage{Name: "/prefs", Kind: "prefs", Set: "state", Body: b})
		}
		if resp := newHarness(t).do("GET", "/"); resp.StatusCode != 401 {
			accFail = append(accFail, fmt.Sprintf("GET / without a session: %d, want 401", resp.StatusCode))
		} else {
			accPages = append(accPages, accPage{Name: "unauthorized", Kind: "unauthorized", Set: "state", Body: body(t, resp)})
		}
		for _, s := range scenarios {
			b, err := scenarioPage(t, s)
			if err != nil {
				t.Error(err)
				continue
			}
			accPages = append(accPages, accPage{Name: "overview scenario " + s.Name, Kind: "scenario", Set: "scenario", Body: b})
		}
	})
	// A page that did not answer as expected, or a set short of its pinned size, fails every sweep.
	if len(accFail) > 0 {
		t.Fatalf("pages did not answer as expected:\n%s", strings.Join(accFail, "\n"))
	}
	if want := wantPages(); len(accPages) != want {
		t.Fatalf("%d pages in the set, want %d", len(accPages), want)
	}
	return accPages
}

// pcsOthers is the number of other PCs of the "pcs" sets; the switch counts this PC too.
const pcsOthers = 8

// pcsLong are the pages asked with machines=all at the longest window.
var pcsLong = map[string]bool{"overview": true, "quality": true, "cost": true, "system": true}

// accPCs adds the "pcs" and "pcslong" pages to the set. The long session and the hot memory are appended to
// this history as to the first one, so their pages read the same ids.
func accPCs(t *testing.T, long, hot string) {
	t.Helper()
	spec := dataSpec
	dataSpec.OtherPCs = pcsOthers
	h := dataHarness(t)
	dataSpec = spec
	if got := indexgen.AppendLongSession(t, h.dirs.State, longTurns); got != long {
		t.Fatalf("the long session of the PCs history is %q, want %q", got, long)
	}
	if got := indexgen.AppendHotMemory(t, h.dirs.State, hotShows); got != hot {
		t.Fatalf("the hot memory of the PCs history is %q, want %q", got, hot)
	}
	ck0 := h.login()
	ck := cookieHdr(ck0)
	proj, day := "", ""
	var most int64
	for _, p := range apiPath(t, h, "/api/v1/projects").Tables.Projects {
		if p.Sessions > most {
			proj, most = p.Project, p.Sessions
		}
	}
	most = 0
	for _, d := range apiPath(t, h, "/api/v1/days").Tables.Days {
		if d.Sessions > most {
			day, most = d.Day, d.Sessions
		}
	}
	for _, r := range routes {
		if r.Kind != "page" {
			continue
		}
		path := r.Sample
		switch r.Page {
		case "project":
			path = "/projects/" + url.PathEscape(proj)
		case "session":
			path = "/sessions/" + url.PathEscape(long)
		case "memory":
			path = "/memories/" + url.PathEscape(hot)
		case "day":
			path = "/days/" + day
		}
		ask := func(q, set string, dev bool, hdr ...string) {
			if len(hdr) == 0 {
				hdr = ck
			}
			resp := h.do("GET", path+"?"+q, hdr...)
			b := body(t, resp)
			if resp.StatusCode != 200 {
				accFail = append(accFail, fmt.Sprintf("GET %s?%s: %d, want 200", path, q, resp.StatusCode))
				return
			}
			accPages = append(accPages, accPage{Name: path + "?" + q, Kind: r.Page, Dev: dev, Set: set, Body: b})
		}
		for _, dev := range []bool{false, true} {
			if dev && !devPages[r.Page] {
				continue
			}
			ask(sweepWindow90+"&machines=all&dev="+map[bool]string{false: "0", true: "1"}[dev], "pcs", dev)
		}
		if pcsLong[r.Page] {
			// The developer menu on as well: its extra cards are the worst case of the size cap.
			for _, dev := range []bool{false, true} {
				ask("since="+longSince+"&until="+longUntil+"&machines=all&dev="+map[bool]string{false: "0", true: "1"}[dev], "pcslong", dev,
					withPrefs(ck0, "v1.system.0.0.comfortable.1")...)
			}
		}
	}
}

// eachAcceptancePage runs f as a subtest on each page keep accepts.
func eachAcceptancePage(t *testing.T, keep func(accPage) bool, f func(t *testing.T, p accPage)) {
	t.Helper()
	n := 0
	for _, p := range allAcceptancePages(t) {
		if keep != nil && !keep(p) {
			continue
		}
		n++
		t.Run(p.Name, func(t *testing.T) { f(t, p) })
	}
	if n == 0 {
		t.Fatal("no page selected")
	}
}

// pageOf is the page of that kind, set and developer mode.
func pageOf(t *testing.T, kind, set string, dev bool) accPage {
	t.Helper()
	for _, p := range allAcceptancePages(t) {
		if p.Kind == kind && p.Set == set && p.Dev == dev {
			return p
		}
	}
	t.Fatalf("no %s page in set %s (dev %v)", kind, set, dev)
	return accPage{}
}

// wantPages is the size of the set: 16 routes at 90 days, dev on for the pages that change with it;
// the charting pages at 365 days (dev on for three); the 16 routes at the longest window, dev on for the
// pages that change with it; Overview and System at the defaults; the memory with limit=500; a
// comparison; the text page; two failure pages, /prefs and the sign-in page; the scenarios; then the 16 routes
// on all PCs at 90 days (dev on for the pages that change with it) and four of them at the longest window (dev off and on).
func wantPages() int {
	return 16 + len(devPages) + len(charted) + len(devCharts) + 16 + len(devPages) + len(defaultPages) + 1 + 1 + 1 + 1 + 2 + 2 + len(scenarios) +
		16 + len(devPages) + 2*len(pcsLong)
}

func TestAcceptancePageSet(t *testing.T) {
	allAcceptancePages(t) // fails when a page did not answer as pinned, or the set is short
	// The pages hold what the caps are for: a full page of turns, a full strip of shows, a year of days.
	if n := len(parseHTML(t, pageOf(t, "session", "90", false).Body).find("li.turn")); n != 50 {
		t.Errorf("the long session's page lists %d turns, want a page of 50", n)
	}
	if n := strings.Count(pageOf(t, "memory", "all", false).Body, "<title>"); n < 500 {
		t.Errorf("the hot memory's page with limit=500 draws %d marks, want 500 shows", n)
	}
	// A year of days is drawn: a bar for each day of the outcomes chart (the hover sentences stop at
	// chart.HoverMax positions, the bars do not).
	q365 := parseHTML(t, pageOf(t, "quality", "365", false).Body)
	if card := q365.first("section.card[aria-labelledby=w-outcomes]"); card == nil {
		t.Error("no outcomes card on the quality page over 365 days")
	} else if n := len(card.find("figure.chart rect.seg")); n < 365 {
		t.Errorf("the outcomes chart over 365 days draws %d bars", n)
	}
}

// thisOnlyKinds are the pages whose figures are always this PC's (the other PCs send no project, session or
// memory); the view each reads is what metrics.ThisOnlyView says it is.
var thisOnlyKinds = map[string]string{"projects": "projects", "project": "projects", "sessions": "sessions", "session": "sessions",
	"memories": "memories", "memory": "memories"}

// On every page of the "pcs" sets the switch counts this PC and the 8 others: on a page that merges them it
// reads all PCs and lists them, and on a this-PC-only page it stays on this PC, with the second half as plain
// text that says why. A page that dropped the parameter would fall back to this PC's, and fail here.
func TestAcceptancePCsPagesAreOnAllPCs(t *testing.T) {
	want := fmt.Sprintf("all PCs on this server (%d)", pcsOthers+1)
	var merged, alone int
	eachAcceptancePage(t, func(p accPage) bool { return p.Set == "pcs" || p.Set == "pcslong" }, func(t *testing.T, p accPage) {
		doc := parseHTML(t, p.Body)
		sw := doc.first(".scopebar .mscope")
		if sw == nil {
			t.Fatal("no switch in the scope line")
		}
		if view, only := thisOnlyKinds[p.Kind]; only {
			alone++
			if !metrics.ThisOnlyView(view) {
				t.Errorf("%s is not a this-PC-only view", view)
			}
			span := sw.first("nav.mseg span[aria-disabled]")
			if got := current(sw, "nav.mseg", "true"); !slices.Equal(got, []string{"this PC"}) || span == nil || span.text() != want ||
				span.attr("title") != thisOnlyHint || sw.first("details.pcs") != nil {
				t.Errorf("selected %q, disabled half %v, list %v: want this PC, and %q as text", got, span, sw.first("details.pcs") != nil, want)
			}
			return
		}
		merged++
		if got := current(sw, "nav.mseg", "true"); !slices.Equal(got, []string{want}) {
			t.Errorf("selected half %q, want %q", got, want)
		}
		if sw.first("details.pcs") == nil {
			t.Error("no Which PCs list")
		}
		if got := len(sw.find("details.pcs li")); got != pcsOthers+1 {
			t.Errorf("%d PCs listed, want %d", got, pcsOthers+1)
		}
	})
	if merged < 10 || alone < 5 {
		t.Errorf("%d merged pages and %d this-PC-only pages swept: the check sees too little", merged, alone)
	}
}

// Nothing on the pages of all PCs shows only when the pointer rests on it: each chart whose marks carry a hover
// sentence holds the same figures in its table, a bucket range's included. The control is that a chart of
// bucket ranges is among the charts read.
func TestAcceptancePCsPagesHaveNoHoverOnlyContent(t *testing.T) {
	charts, ranges := 0, 0
	eachAcceptancePage(t, func(p accPage) bool { return p.Set == "pcs" || p.Set == "pcslong" }, func(t *testing.T, p accPage) {
		doc := parseHTML(t, p.Body)
		for i, f := range doc.find("figure.chart") {
			if len(f.find("title")) == 0 {
				continue
			}
			charts++
			ranges += len(f.find("rect.bkt"))
			if tbl := f.first("details.tblfb table"); tbl == nil || len(tbl.find("tbody tr")) == 0 {
				t.Errorf("chart %d has hover sentences and no table with the same figures", i)
			}
		}
	})
	if charts == 0 || ranges == 0 {
		t.Errorf("%d charts and %d bucket ranges read: the check sees nothing", charts, ranges)
	}
}

// With 63 other PCs (64 counted, one document set aside) the pages that list them stay under the size cap at the
// longest window: Machines on System has 65 rows, By PC on Cost 64 and Which PCs in the scope line 64.
func TestPagesWithManyPCsStayUnderSizeCap(t *testing.T) {
	if raceEnabled {
		t.Skip("the page sweep runs without -race (make test)")
	}
	old, oldTimeout := dataSpec, renderTimeout
	t.Cleanup(func() { dataSpec, renderTimeout = old, oldTimeout })
	dataSpec, renderTimeout = acceptanceSpec, time.Minute
	dataSpec.OtherPCs = 63
	h := dataHarness(t)
	dataSpec = old
	ck := withPrefs(h.login(), "v1.system.0.0.comfortable.1")
	win := "since=" + longSince + "&until=" + longUntil + "&machines=all&dev=0"
	for _, c := range []struct {
		path string
		rows func(*node) []*node // the rows of the page's table of PCs; nil when it has none
		want int
	}{
		{"/system?" + win, func(d *node) []*node { return cardOf(t, d, "w-machines").find("table.mach tbody tr") }, 65},
		{"/cost?" + win, func(d *node) []*node { return d.find("details.tblfb table.mach tbody tr") }, 64},
		{"/?" + win, nil, 0},
	} {
		resp := h.do("GET", c.path, ck...)
		b := body(t, resp)
		if resp.StatusCode != 200 {
			t.Errorf("GET %s: %d", c.path, resp.StatusCode)
			continue
		}
		t.Logf("%s: %d bytes", c.path, len(b))
		if len(b) > sizeCap {
			t.Errorf("%s: %d bytes, cap %d", c.path, len(b), sizeCap)
		}
		doc := parseHTML(t, b)
		if got := len(doc.find(".scopebar details.pcs li")); got != 64 {
			t.Errorf("%s: %d PCs in Which PCs, want 64", c.path, got)
		}
		if c.rows != nil {
			if got := len(c.rows(doc)); got != c.want {
				t.Errorf("%s: %d rows in its table of PCs, want %d", c.path, got, c.want)
			}
		}
	}
}

// scriptEl is a <script> element with its body.
var scriptEl = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)

// mustShow are the ids (heading ids of the cards and tiles) a page shows.
var mustShow = map[string][]string{
	"overview": {"w-verdict", "w-attention", "w-lately", "w-glance", "w-recall-hit_rate", "w-used-turn_rate", "w-verdict-wrong_rate",
		"w-context-per_session", "w-recall-latency", "w-pipeline-flow", "w-helped", "w-calendar"},
	"quality":  {"w-used-turn_rate", "w-outcomes", "w-reported", "w-recall-hit_rate", "w-recall-empty_reasons", "w-search-rate"},
	"cost":     {"w-context-per_session", "w-context-split", "w-recall-latency", "w-extraction-tokens", "w-memory-yield"},
	"bank":     {"w-bank-size", "w-bank-growth", "w-bank-by_project", "w-bank-consolidation", "w-notes-stored", "w-bank-churn", "w-imports-sent", "w-review"},
	"pipeline": {"w-pipeline-flow", "w-local-backlog", "w-server-queue", "w-server-extraction_delay", "w-hooks-failures", "w-jobs-last_run", "w-pipeline-time_to_recall"},
	"system":   {"w-server-reach", "w-this-pc", "w-attention", "w-doctor-check", "w-doctor-history", "w-secrets-suspects"},
	"releases": {"w-releases", "w-releases-compare"}, "defs": {"w-defs"},
	"projects": {"w-projects"}, "sessions": {"w-sessions"}, "memories": {"w-memories"}, "days": {"w-days", "w-calendar"},
	"project": {"w-recall-hit_rate", "w-used-turn_rate", "w-verdict-wrong_rate", "w-context-per_session", "w-recall-latency", "w-pipeline-flow"},
	"day":     {"w-recall-hit_rate", "w-used-turn_rate", "w-verdict-wrong_rate", "w-context-per_session", "w-recall-latency", "w-pipeline-flow"},
	"session": {"w-session-turns"},
	"memory":  {"w-memory-text", "w-memory-strip", "w-memory-rank", "w-memory-do", "w-memory-origin"},
}

// mustShowIn are the ids a kind's set of pages shows besides its kind's: the day the version changed on holds
// the markers.
var mustShowIn = map[string][]string{"day/markers": {"w-markers"}}

// listTables are the pages whose widget is a table.
var listTables = []string{"projects", "sessions", "memories", "days"}

var onAttr = regexp.MustCompile(`(?i)<[^>]*[\s/"']on[a-z]+\s*=`)

var (
	detailsClasses = []string{"menu", "navmenu", "info", "cli", "tabs", "tblfb", "wpick", "summary-text", "never"}
	popClasses     = []string{"pop", "menu-pop"}
)

func TestPagesWorkWithoutScripts(t *testing.T) {
	opened := map[string]int{} // the elements of each class seen across the set
	eachAcceptancePage(t, func(p accPage) bool {
		return p.Set != "365" && p.Set != "all" && p.Set != "default" || p.Kind == "text"
	}, func(t *testing.T, p accPage) {
		stripped := scriptEl.ReplaceAllString(p.Body, "")
		if strings.Contains(strings.ToLower(stripped), "<script") {
			t.Error("a <script> element the check does not strip")
		}
		// The only script is the copy script, loaded from a file: nothing a page shows is drawn by it.
		for _, s := range scriptEl.FindAllString(p.Body, -1) {
			if !regexp.MustCompile(`(?i)^<script[^>]*\ssrc="/assets/`).MatchString(s) || strings.TrimSpace(regexp.MustCompile(`(?is)^<script[^>]*>|</script>$`).ReplaceAllString(s, "")) != "" {
				t.Errorf("a script that is not the copy script file: %.80s", s)
			}
		}
		ids := append(slices.Clone(mustShow[p.Kind]), mustShowIn[p.Kind+"/"+p.Set]...)
		if p.Kind == "scenario" {
			ids = mustShow["overview"][:4]
		}
		for _, id := range ids {
			if !strings.Contains(stripped, ` id="`+id+`"`) {
				t.Errorf("no %s without scripts", id)
			}
		}
		if p.Kind == "cost" && !strings.Contains(stripped, `<div class="figs">`) {
			t.Error("no figure strip on the cost page without scripts")
		}
		if slices.Contains(listTables, p.Kind) && !strings.Contains(stripped, "<table") {
			t.Error("a list without its table")
		}
		if strings.Contains(strings.ToLower(p.Body), "<noscript") {
			t.Error("a <noscript> element")
		}
		if onAttr.MatchString(p.Body) {
			t.Errorf("an event handler attribute: %s", onAttr.FindString(p.Body))
		}
		if strings.Contains(strings.ToLower(html.UnescapeString(p.Body)), "javascript:") {
			t.Error("a javascript: URL")
		}
		doc, err := parseHTMLErr(p.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range doc.find("button.copy") {
			if !b.has("hidden") {
				t.Errorf("a copy button that shows without a script: %q", b.text())
			}
		}
		// The shortcuts' button and dialog are the script's too: the button hidden, the dialog closed.
		for _, b := range doc.find("button.keyhint-btn") {
			if !b.has("hidden") {
				t.Errorf("a shortcuts button that shows without a script: %q", b.text())
			}
		}
		for _, d := range doc.find("dialog") {
			if d.has("open") {
				t.Error("a dialog drawn open: only a script can close it")
			}
		}
		if p.Kind == "overview" || p.Kind == "scenario" {
			checkSummaryText(t, doc)
		}
		if p.Set == "compare" && !strings.Contains(stripped, "Comparing v0.1.17 with v0.1.18") {
			t.Error("no result line of the comparison without scripts")
		}
		// Everything that opens does it without a script: a <details>.
		for _, class := range detailsClasses {
			for _, n := range doc.find("." + class) {
				opened[class]++
				if n.Name != "details" {
					t.Errorf("the .%s is a %s, not a details", class, n.Name)
				}
			}
		}
		for _, class := range popClasses {
			for _, n := range doc.find("." + class) {
				opened[class]++
				if !within(n, "details", "") {
					t.Errorf("a .%s outside any details: it opens only with a script or a hover", class)
				}
			}
		}
	})
	// A class renamed away would make the loops above see nothing: each must meet its elements.
	for _, class := range append(slices.Clone(detailsClasses), popClasses...) {
		if opened[class] == 0 {
			t.Errorf("no .%s element on any page: the check sees nothing", class)
		}
	}
}

// checkSummaryText is the Overview's Copy summary: the button hidden, its text the one the page prints
// under "Summary as text", so a reader without a script has the same words.
func checkSummaryText(t *testing.T, doc *node) {
	t.Helper()
	var btns []*node
	for _, b := range doc.find("button.copy") {
		if b.has("data-copy") {
			btns = append(btns, b)
		}
	}
	if len(btns) != 1 || !btns[0].has("hidden") || !strings.HasPrefix(btns[0].attr("aria-label"), "Copy summary") {
		t.Errorf("%d summary buttons (hidden %v), want one hidden button named Copy summary", len(btns), len(btns) > 0 && btns[0].has("hidden"))
		return
	}
	pre := doc.find("details.summary-text pre code")
	if len(pre) != 1 {
		t.Errorf("%d summary texts, want one under Summary as text", len(pre))
		return
	}
	// The parsed text has its white space collapsed: compare the words.
	if got, want := strings.Join(strings.Fields(btns[0].attr("data-copy")), " "), strings.Join(strings.Fields(pre[0].text()), " "); got == "" || got != want {
		t.Errorf("Copy summary copies %q, the page prints %q", got, want)
	}
}

func TestEveryCopyButtonIsHidden(t *testing.T) {
	buttons := map[string]int{}
	eachAcceptancePage(t, func(p accPage) bool { return p.Kind != "scenario" && p.Set != "365" }, func(t *testing.T, p accPage) {
		doc, err := parseHTMLErr(p.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range doc.find("button.copy") {
			buttons[p.Kind]++
			if !b.has("hidden") {
				t.Errorf("a copy button without hidden: %q", b.text())
			}
		}
		for _, b := range doc.find("button.keyhint-btn") {
			buttons["keyhint"]++
			if !b.has("hidden") {
				t.Errorf("a shortcuts button without hidden: %q", b.text())
			}
		}
		// A copy button is a button of class copy; the page holds no other way to show one.
		if strings.Count(p.Body, `class="copy"`) != len(doc.find("button.copy")) {
			t.Errorf("%d class=copy attributes, %d button.copy", strings.Count(p.Body, `class="copy"`), len(doc.find("button.copy")))
		}
	})
	for _, kind := range []string{"overview", "quality", "cost", "bank", "pipeline", "system", "memory", "keyhint"} {
		if buttons[kind] == 0 {
			t.Errorf("the %s pages hold no copy button: the check sees nothing", kind)
		}
	}
}

func TestPagesUnderSizeCap(t *testing.T) {
	type sized struct {
		name string
		n    int
	}
	var all []sized
	eachAcceptancePage(t, func(p accPage) bool {
		return p.Set == "90" || p.Set == "365" || p.Set == "all" || p.Set == "long" || p.Set == "compare" || p.Set == "markers" || p.Set == "pcs" || p.Set == "pcslong"
	}, func(t *testing.T, p accPage) {
		all = append(all, sized{p.Name, len(p.Body)})
		if len(p.Body) > sizeCap {
			t.Errorf("%d bytes, cap %d", len(p.Body), sizeCap)
		}
	})
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })
	for _, s := range all[:min(5, len(all))] {
		t.Logf("%7d bytes  %s", s.n, s.name)
	}
	// The pages the cap is for are in the set.
	for _, c := range []struct{ kind, set string }{{"memory", "all"}, {"session", "90"}, {"quality", "90"}, {"overview", "long"}, {"system", "long"}, {"cost", "long"}, {"releases", "compare"}, {"day", "markers"},
		{"overview", "pcs"}, {"cost", "pcs"}, {"system", "pcs"}, {"memory", "pcs"}, {"cost", "pcslong"}, {"overview", "pcslong"}} {
		pageOf(t, c.kind, c.set, false)
	}
	// The Overview and Days pages hold the 13 months of the calendar whatever their window: a week a column,
	// seven days each, from the Monday on or before a year and a month back.
	for _, c := range []struct{ kind, set string }{{"overview", "90"}, {"overview", "long"}, {"days", "90"}, {"days", "long"}} {
		doc := parseHTML(t, pageOf(t, c.kind, c.set, false).Body)
		if n := len(doc.find("a.hm-c")); n < 13*28 || n > 14*31+7 {
			t.Errorf("%s (%s) draws %d calendar days, want the 13 months", c.kind, c.set, n)
		}
	}
}

func TestNoChartOverMaxMarks(t *testing.T) {
	charts := 0
	eachAcceptancePage(t, func(p accPage) bool { return p.Kind != "text" }, func(t *testing.T, p accPage) {
		doc, err := parseHTMLErr(p.Body)
		if err != nil {
			t.Fatal(err)
		}
		for i, f := range doc.find("figure.chart") {
			charts++
			if n := len(f.find("title")); n > chart.MaxMarks {
				t.Errorf("chart %d holds %d marks, cap %d", i, n, chart.MaxMarks)
			}
		}
	})
	if charts == 0 {
		t.Error("no chart on any page: the check sees nothing")
	}
	// The health-check strip has a row for every check and a position for every day: at a year, and at the
	// longest custom window, it merges days into buckets rather than going over the cap or dropping rows.
	for _, set := range []string{"365", "long"} {
		doc := parseHTML(t, pageOf(t, "system", set, false).Body)
		h := doc.first("#w-doctor-history")
		for h != nil && h.Name != "section" {
			h = h.Parent
		}
		if h == nil {
			t.Fatalf("no health-check card on the system page (%s)", set)
		}
		figs := h.find("figure.chart")
		if len(figs) != 1 {
			t.Fatalf("%d charts in the health-check card (%s), want its strip", len(figs), set)
		}
		marks := len(figs[0].find("title"))
		rows := len(figs[0].find("text.lbl"))
		if marks == 0 || marks > chart.MaxMarks || rows < 20 || marks < rows*10 {
			t.Errorf("the health-check strip (%s) draws %d marks on %d rows, want 10 or more a row and at most %d", set, marks, rows, chart.MaxMarks)
		}
	}
}

var altAttr = regexp.MustCompile(`(?is)\salt\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// spoken is spokenText plus the alt text, which a screen reader says too.
func spoken(page string) string {
	out := []string{spokenText(page)}
	for _, m := range altAttr.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1]+m[2])
	}
	return strings.Join(out, " ")
}

// internalTerms are the words of the issue that no person reads on a page.
var internalTerms = []string{"credited", "confident miss", "consolidation", "quarantine", "backfill", "boost", "retain", "settle",
	"p50", "p95", "mrr", "hit@", "gini", "hook failure"}

// termAt finds term at the start of a word in lower-cased text.
func termAt(text, term string) bool {
	return regexp.MustCompile(`(?:^|[^a-z])` + regexp.QuoteMeta(term)).MatchString(text)
}

func TestNoInternalTermsOnPages(t *testing.T) {
	read := map[string]int{} // the new cards the sweep reads
	eachAcceptancePage(t, func(p accPage) bool {
		return !p.Dev && p.Kind != "text" && p.Kind != "releases" && p.Kind != "defs" && p.Set != "365"
	}, func(t *testing.T, p accPage) {
		for _, id := range newCardIDs {
			if strings.Contains(p.Body, ` id="`+id+`"`) {
				read[id]++
			}
		}
		text := strings.ToLower(spoken(html.UnescapeString(p.Body)))
		for _, term := range internalTerms {
			if termAt(text, term) {
				i := regexp.MustCompile(`(?:^|[^a-z])` + regexp.QuoteMeta(term)).FindStringIndex(text)
				t.Errorf("shows %q: ...%s...", term, text[max(0, i[0]-40):min(len(text), i[1]+40)])
			}
		}
	})
	for _, id := range newCardIDs {
		if read[id] == 0 {
			t.Errorf("no page without developer mode draws %s: the check sees nothing", id)
		}
	}
}

// newCardIDs are the cards added after block 5 that a person reads (not the developer ones, nor the
// comparison, which sits on the Releases page where the terms are allowed).
var newCardIDs = []string{"w-helped", "w-calendar", "w-markers", "w-pipeline-time_to_recall", "w-doctor-history", "w-secrets-suspects", "w-memory-yield", "w-memory-origin"}

func TestTermMatchingStartsAtAWord(t *testing.T) {
	for _, c := range []struct {
		text, term string
		want       bool
	}{
		{"set aside, quarantined by the server", "quarantine", true},
		{"settled and boosted", "settle", true},
		{"settled and boosted", "boost", true},
		{"the p95 latency", "p95", true},
		{"hit@1", "hit@", true},
		{"a rebooted machine", "boost", false},
		{"unsettled", "settle", false},
		{"sub-p95", "p95", true},
		{"xp95", "p95", false},
	} {
		if got := termAt(c.text, c.term); got != c.want {
			t.Errorf("termAt(%q, %q) = %v, want %v", c.text, c.term, got, c.want)
		}
	}
}

// devidClass is a class attribute holding the devid token.
var devidClass = regexp.MustCompile(`\sclass=(?:"[^"]*\bdevid\b[^"]*"|'[^']*\bdevid\b[^']*')`)

func TestDeveloperMetricsOnlyInDeveloperMode(t *testing.T) {
	var dev []metrics.Def
	for _, d := range metrics.Catalog {
		if d.Dev {
			dev = append(dev, d)
		}
	}
	if len(dev) == 0 {
		t.Fatal("the catalog holds no developer metric")
	}
	mention := func(text string, d metrics.Def) []string {
		var out []string
		if regexp.MustCompile(`(?:^|[^a-z_.])` + regexp.QuoteMeta(d.ID) + `(?:[^a-z_]|$)`).MatchString(text) {
			out = append(out, "id "+d.ID)
		}
		if strings.Contains(text, d.Question) {
			out = append(out, "question "+d.Question)
		}
		return out
	}
	eachAcceptancePage(t, func(p accPage) bool { return !p.Dev && p.Kind != "releases" && p.Kind != "defs" }, func(t *testing.T, p accPage) {
		text := html.UnescapeString(p.Body)
		for _, d := range dev {
			if m := mention(text, d); len(m) > 0 {
				t.Errorf("developer metric shown without developer mode: %v", m)
			}
		}
		if devidClass.MatchString(p.Body) {
			t.Error("a devid line without developer mode")
		}
		if strings.Contains(p.Body, `id="w-ranking"`) {
			t.Error("the ranking card without developer mode")
		}
	})
	// With the menu on, the pages do show them: the check above sees something to refuse.
	shown := map[string]bool{}
	for _, p := range allAcceptancePages(t) {
		if p.Dev && p.Set == "90" && p.Kind != "releases" && p.Kind != "defs" {
			text := html.UnescapeString(p.Body)
			for _, d := range dev {
				if len(mention(text, d)) > 0 {
					shown[d.ID] = true
				}
			}
		}
	}
	// Every developer metric of the catalog is drawn by a developer page.
	for _, d := range dev {
		if !shown[d.ID] {
			t.Errorf("no developer page shows %s", d.ID)
		}
	}
	// The developer cards: on the Bank and Quality pages with the menu on, on no page without it, whatever
	// the window (the longest custom one included).
	devCards := map[string]string{"w-bank-active_share": "bank", "w-shows-concentration": "bank", "w-injection-contradicted": "quality", "w-boost-set_changed": "quality"}
	for id, kind := range devCards {
		if d := pageOf(t, kind, "90", true); !strings.Contains(d.Body, ` id="`+id+`"`) {
			t.Errorf("the %s page in developer mode lacks %s", kind, id)
		}
		for _, set := range []string{"90", "long"} {
			if p := pageOf(t, kind, set, false); strings.Contains(p.Body, id) {
				t.Errorf("the %s page (%s) without developer mode mentions %s", kind, set, id)
			}
		}
	}
	q := pageOf(t, "quality", "90", true)
	if !strings.Contains(q.Body, `id="w-ranking"`) {
		t.Error("the quality page in developer mode lacks the ranking card")
	}
	if o := pageOf(t, "overview", "90", true); !devidClass.MatchString(o.Body) {
		t.Error("the Overview in developer mode lacks the devid lines")
	}
}

var (
	classAttr = regexp.MustCompile(`\sclass=(?:"([^"]*)"|'([^']*)')`)
	notSel    = regexp.MustCompile(`:not\([^)]*\)|\[[^\]]*\]`)
	tplCtl    = regexp.MustCompile(`(?s)\{\{-?\s*(?:if|else|end|range|with|/\*)\b.*?\}\}`)
	selClass  = regexp.MustCompile(`\.(-?[A-Za-z_][\w-]*)`)
	tplExpr   = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
)

// styledClasses are the classes the stylesheet has a selector for.
func styledClasses(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, r := range cssRules(dashboardCSS(t)) {
		for _, sel := range r.Sel {
			// A class only inside :not(...) or [...] is excluded, not styled.
			for _, m := range selClass.FindAllStringSubmatch(notSel.ReplaceAllString(sel, ""), -1) {
				out[m[1]] = true
			}
		}
	}
	return out
}

func TestEveryClassIsStyled(t *testing.T) {
	styled := styledClasses(t)
	if len(styled) < 100 {
		t.Fatalf("%d classes read from the stylesheet", len(styled))
	}
	used := map[string]string{} // class -> where it is first used
	note := func(class, where string) {
		if _, ok := used[class]; !ok {
			used[class] = where
		}
	}
	for _, c := range chart.Classes {
		note(c, "chart.Classes")
	}
	for _, p := range allAcceptancePages(t) {
		for _, m := range classAttr.FindAllStringSubmatch(p.Body, -1) {
			for _, c := range strings.Fields(m[1] + m[2]) {
				note(c, p.Name)
			}
		}
	}
	// A class written in a template, drawn or not by the pages above.
	files, err := fs.Glob(Templates(), "*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates: %v, %v", files, err)
	}
	for _, f := range files {
		src, err := fs.ReadFile(Templates(), f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range classAttr.FindAllStringSubmatch(string(src), -1) {
			// Control actions (`{{if}}`, `{{end}}`) split tokens; an output action (`{{.X}}`) makes a token
			// computed: the pages above hold the composed classes, a literal prefix must still be styled.
			attr := tplExpr.ReplaceAllString(tplCtl.ReplaceAllString(m[1]+m[2], " "), "\x00")
			for _, c := range strings.Fields(attr) {
				if i := strings.IndexByte(c, 0); i >= 0 {
					if prefix := c[:i]; prefix != "" && !slices.ContainsFunc(slices.Collect(maps.Keys(styled)), func(s string) bool { return strings.HasPrefix(s, prefix) }) {
						t.Errorf("no class of dashboard.css starts with %q (template %s)", prefix, f)
					}
					continue
				}
				note(c, "template "+f)
			}
		}
	}
	// The new cards draw these: the sweep above must have met them, or it checks nothing of the cards.
	for _, c := range []string{"helped", "hm-c", "hm-legend", "funnel", "markers", "keyhint-btn", "wpick", "summary-text", "never", "keys"} {
		if _, ok := used[c]; !ok {
			t.Errorf("no page or template uses the class %q: the check does not see the new cards", c)
		}
	}
	var missing []string
	for c, where := range used {
		if !styled[c] {
			missing = append(missing, fmt.Sprintf("%s (%s)", c, where))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("classes with no selector in dashboard.css:\n%s", strings.Join(missing, "\n"))
	}
}

var (
	styleAttr    = regexp.MustCompile(`(?i)\sstyle\s*=`)
	styleEl      = regexp.MustCompile(`(?i)<style\b`)
	inlineScript = regexp.MustCompile(`(?is)<script\b((?:[^>]*\ssrc\s*=)?)[^>]*>(.*?)</script>`)
	scriptOpen   = regexp.MustCompile(`(?is)<script\b[^>]*>`)
)

// inlineScripts counts the scripts of src that have no src attribute or
// hold a body.
func inlineScripts(src string) int {
	n := 0
	for _, m := range inlineScript.FindAllStringSubmatch(src, -1) {
		if m[1] == "" || strings.TrimSpace(m[2]) != "" {
			n++
		}
	}
	return n + len(scriptOpen.FindAllString(src, -1)) - len(inlineScript.FindAllString(src, -1))
}

var copyButton = regexp.MustCompile(`(?is)<button\b[^>]*\bclass="copy"[^>]*>`)

func TestNoInlineStyleOrScript(t *testing.T) {
	for _, p := range allAcceptancePages(t) {
		if styleAttr.MatchString(p.Body) {
			t.Errorf("%s: a style attribute: %.60s", p.Name, styleAttr.FindString(p.Body))
		}
		if styleEl.MatchString(p.Body) {
			t.Errorf("%s: a <style> element", p.Name)
		}
		if inlineScripts(p.Body) > 0 {
			t.Errorf("%s: an inline script", p.Name)
		}
	}
	files, err := fs.Glob(Templates(), "*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates: %v, %v", files, err)
	}
	for _, f := range files {
		src, err := fs.ReadFile(Templates(), f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		if styleAttr.MatchString(s) || styleEl.MatchString(s) {
			t.Errorf("template %s: a style attribute or element", f)
		}
		if inlineScripts(s) > 0 {
			t.Errorf("template %s: an inline script", f)
		}
		if strings.Contains(strings.ToLower(s), "<noscript") || strings.Contains(strings.ToLower(s), "javascript:") || onAttr.MatchString(s) {
			t.Errorf("template %s: a noscript, a javascript: URL or an event handler", f)
		}
		for _, b := range copyButton.FindAllString(s, -1) {
			if !strings.Contains(b, "hidden") {
				t.Errorf("template %s: a copy button that shows without a script: %s", f, b)
			}
		}
	}
}

func TestInlineCheckersSeeViolations(t *testing.T) {
	for _, src := range []string{`<a href="/" onclick="x()">`, `<a/onclick=x>`, `<a href='/' onfocus = "x">`} {
		if !onAttr.MatchString(src) {
			t.Errorf("onAttr misses %s", src)
		}
	}
	if onAttr.MatchString(`<a href="/online" class="one">`) {
		t.Error("onAttr reads a value as a handler")
	}
	if !devidClass.MatchString(`<p class="x devid">`) || !devidClass.MatchString(`<p class='devid'>`) || devidClass.MatchString(`<p class="devidx">`) {
		t.Error("devidClass does not read a class list")
	}
	if !styleAttr.MatchString(`<p style="x:y">`) || !styleEl.MatchString(`<style>`) || !styleAttr.MatchString("<p\nstyle = 'x'>") {
		t.Error("the style checks miss a style")
	}
	for _, c := range []struct {
		src  string
		want int
	}{
		{`<script src="/assets/copy.js"></script>`, 0},
		{`<script>alert(1)</script>`, 1},
		{`<script src="/a.js">alert(1)</script>`, 1},
		{`<script type="module">x</script><script src="/a.js"></script>`, 1},
		{`<script>`, 1},
	} {
		if got := inlineScripts(c.src); got != c.want {
			t.Errorf("inlineScripts(%q) = %d, want %d", c.src, got, c.want)
		}
	}
}
