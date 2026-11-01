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
var acceptanceSpec = indexgen.Spec{Months: 13, TelemetryPerYear: 20_000, JournalPerYear: 3_000, Memories: 500, Seed: 2}

const (
	sizeCap       = 307_200 // 300 KB
	longTurns     = 400
	hotShows      = 600
	year365Since  = "2025-10-30"
	year365Until  = "2026-10-30"
	sweepWindow90 = "days=90"
)

var (
	charted   = map[string]bool{"overview": true, "quality": true, "cost": true, "bank": true, "pipeline": true, "projects": true, "project": true, "day": true, "memory": true}
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
	Set  string // "90" (days=90), "365" (since/until), "default" (no window), "all" (the memory with limit=500), "scenario"
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
// off and on), at the defaults with it off, the hot memory with limit=500,
// the text page, and the Overview on each scenario. Rendered once.
func allAcceptancePages(t *testing.T) []accPage {
	t.Helper()
	accOnce.Do(func() {
		oldSpec, oldTimeout := dataSpec, renderTimeout
		t.Cleanup(func() { dataSpec, renderTimeout = oldSpec, oldTimeout })
		// 13 months drawn over a year take seconds under -race: the sweep reads the pages, the budgets time them.
		dataSpec, renderTimeout = acceptanceSpec, time.Minute
		h := dataHarness(t)
		long := indexgen.AppendLongSession(t, h.dirs.State, longTurns)
		hot := indexgen.AppendHotMemory(t, h.dirs.State, hotShows)
		ck := cookieHdr(h.login())
		get := func(p string, want int) (string, bool) {
			resp := h.do("GET", p, ck...)
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
			if r.Page == "memory" {
				q := sweepWindow90 + "&limit=500&dev=0"
				if b, ok := get(path+"?"+q, 200); ok {
					accPages = append(accPages, accPage{Name: path + "?" + q, Kind: "memory", Set: "all", Body: b})
				}
			}
		}
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
// the charting pages at 365 days (dev on for three); Overview and System at the defaults; the memory
// with limit=500; the text page; two failure pages, /prefs and the sign-in page; the scenarios.
func wantPages() int {
	return 16 + len(devPages) + len(charted) + len(devCharts) + len(defaultPages) + 1 + 1 + 2 + 2 + len(scenarios)
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
	if n := strings.Count(pageOf(t, "quality", "365", false).Body, "<title>"); n < 365 {
		t.Errorf("the quality page over 365 days draws %d marks", n)
	}
}

// scriptEl is a <script> element with its body.
var scriptEl = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)

// mustShow are the ids (heading ids of the cards and tiles) a page shows.
var mustShow = map[string][]string{
	"overview": {"w-verdict", "w-attention", "w-lately", "w-glance", "w-recall-hit_rate", "w-used-turn_rate", "w-verdict-wrong_rate",
		"w-context-per_session", "w-recall-latency", "w-pipeline-flow"},
	"quality":  {"w-used-turn_rate", "w-outcomes", "w-reported", "w-recall-hit_rate", "w-recall-empty_reasons", "w-search-rate"},
	"cost":     {"w-context-per_session", "w-context-split", "w-recall-latency", "w-extraction-tokens"},
	"bank":     {"w-bank-size", "w-bank-growth", "w-bank-by_project", "w-bank-consolidation", "w-notes-stored", "w-bank-churn", "w-review"},
	"pipeline": {"w-pipeline-flow", "w-local-backlog", "w-server-queue", "w-server-extraction_delay", "w-hooks-failures", "w-jobs-last_run"},
	"system":   {"w-server-reach", "w-this-pc", "w-attention", "w-doctor-check"},
	"releases": {"w-releases"}, "defs": {"w-defs"},
	"projects": {"w-projects"}, "sessions": {"w-sessions"}, "memories": {"w-memories"}, "days": {"w-days"},
	"project": {"w-recall-hit_rate", "w-used-turn_rate", "w-verdict-wrong_rate", "w-context-per_session", "w-recall-latency", "w-pipeline-flow"},
	"day":     {"w-recall-hit_rate", "w-used-turn_rate", "w-verdict-wrong_rate", "w-context-per_session", "w-recall-latency", "w-pipeline-flow"},
	"session": {"w-session-turns"},
	"memory":  {"w-memory-text", "w-memory-strip", "w-memory-rank", "w-memory-do"},
}

// listTables are the pages whose widget is a table.
var listTables = []string{"projects", "sessions", "memories", "days"}

var onAttr = regexp.MustCompile(`(?i)<[^>]*[\s/"']on[a-z]+\s*=`)

var (
	detailsClasses = []string{"menu", "navmenu", "info", "cli", "tabs", "tblfb"}
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
		ids := mustShow[p.Kind]
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
		// A copy button is a button of class copy; the page holds no other way to show one.
		if strings.Count(p.Body, `class="copy"`) != len(doc.find("button.copy")) {
			t.Errorf("%d class=copy attributes, %d button.copy", strings.Count(p.Body, `class="copy"`), len(doc.find("button.copy")))
		}
	})
	for _, kind := range []string{"overview", "quality", "cost", "bank", "pipeline", "system", "memory"} {
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
	eachAcceptancePage(t, func(p accPage) bool { return p.Set == "90" || p.Set == "all" }, func(t *testing.T, p accPage) {
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
	for _, c := range []struct{ kind, set string }{{"memory", "all"}, {"session", "90"}, {"quality", "90"}} {
		pageOf(t, c.kind, c.set, false)
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
	eachAcceptancePage(t, func(p accPage) bool {
		return !p.Dev && p.Kind != "text" && p.Kind != "releases" && p.Kind != "defs" && p.Set != "365"
	}, func(t *testing.T, p accPage) {
		text := strings.ToLower(spoken(html.UnescapeString(p.Body)))
		for _, term := range internalTerms {
			if termAt(text, term) {
				i := regexp.MustCompile(`(?:^|[^a-z])` + regexp.QuoteMeta(term)).FindStringIndex(text)
				t.Errorf("shows %q: ...%s...", term, text[max(0, i[0]-40):min(len(text), i[1]+40)])
			}
		}
	})
}

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
	for _, d := range dev {
		if !shown[d.ID] {
			t.Errorf("no developer page shows %s", d.ID)
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
