package dashboard

import (
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// idsOf are the ids of the rows of the page's table body, in order.
func idsOf(n *node) []string {
	var out []string
	for _, tr := range n.find("main tbody tr") {
		if tr.has("id") {
			out = append(out, tr.attr("id"))
		}
	}
	return out
}

var fragLink = regexp.MustCompile(`href="(/(?:defs|releases)(?:\?[^"#]*)?)#([^"]*)"`)

// fragmentsOf are the fragments of the links to path (/defs or /releases)
// in a page, decoded as a browser matches them to an id.
func fragmentsOf(t *testing.T, page, path string) []string {
	t.Helper()
	var out []string
	for _, m := range fragLink.FindAllStringSubmatch(page, -1) {
		base, _, _ := strings.Cut(m[1], "?")
		if base != path {
			continue
		}
		frag, err := url.PathUnescape(strings.ReplaceAll(m[2], "&amp;", "&"))
		if err != nil {
			t.Fatalf("fragment %q: %v", m[2], err)
		}
		out = append(out, frag)
	}
	return out
}

func TestDefinitionAnchors(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	_, defs := pageGet(t, h, ck, "/defs")
	ids := idsOf(parseHTML(t, defs))
	var want []string
	for _, d := range metrics.Catalog {
		want = append(want, d.ID)
	}
	if len(ids) != len(want) || !slices.Equal(slices.Sorted(slices.Values(ids)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("/defs has the rows %q, the catalog %q", ids, want)
	}
	if !slices.Contains(ids, "recall.hit_rate") {
		t.Error("/defs has no row recall.hit_rate")
	}
	_, home := pageGet(t, h, ck, "/")
	links := fragmentsOf(t, home, "/defs")
	if len(links) == 0 {
		t.Fatal("/ links no definition")
	}
	for _, f := range links {
		if !slices.Contains(ids, f) {
			t.Errorf("/ links /defs#%s, which /defs has no row for", f)
		}
	}
	// The id is also shown in <code>, once per row.
	for _, d := range metrics.Catalog {
		if got := strings.Count(defs, "<code>"+d.ID+"</code>"); got != 1 {
			t.Errorf("/defs shows %s in <code> %d times", d.ID, got)
		}
	}
}

func rowByID(doc *node, id string) *node {
	for _, tr := range doc.find("main tbody tr") {
		if tr.attr("id") == id {
			return tr
		}
	}
	return nil
}

func TestDefinitionRowsAreTheCatalog(t *testing.T) {
	h := dataHarness(t)
	res := apiPath(t, h, "/api/v1/defs")
	_, b := pageGet(t, h, h.login(), "/defs")
	doc := parseHTML(t, b)
	if got := len(doc.find("main tbody tr")); got != len(res.Tables.Defs) || got == 0 {
		t.Fatalf("%d rows, the API has %d", got, len(res.Tables.Defs))
	}
	var server, noMin, rates int
	for _, row := range res.Tables.Defs {
		tr := rowByID(doc, row.ID)
		if tr == nil {
			t.Fatalf("no row %s", row.ID)
		}
		cells := tr.find("td")
		if len(cells) != 10 {
			t.Fatalf("%s: %d cells", row.ID, len(cells))
		}
		definition := row.Definition
		if row.ID == "hooks.failures" {
			definition = hooksFailuresWhat
		}
		unit := map[string]string{"ratio": "rate", "ms": "time"}[row.Unit]
		if unit == "" {
			unit = row.Unit
		}
		where := "this PC"
		if row.Scope == "server" {
			where, server = "server", server+1
		}
		minN := dash
		if row.MinN > 0 {
			minN = en.Int(int64(row.MinN))
		} else {
			noMin++
		}
		n, smallest := dash, dash
		if row.Unit == "ratio" {
			rates++
			n, smallest = en.Int(row.N), "not yet measurable"
			if row.MDC != nil {
				smallest = "about " + en.Int(int64(math.Round(*row.MDC*100))) + " pts"
			}
		}
		want := []string{row.ID, row.Label, row.Question, definition, limitsText(row.Def, en), unit, where, minN, n, smallest}
		for c, w := range want {
			got := cells[c].text()
			if c == 0 {
				got = innerCode(cells[c])
			}
			if got != w {
				t.Errorf("%s cell %d: %q, want %q", row.ID, c, got, w)
			}
		}
	}
	if server == 0 || noMin == 0 || rates == 0 {
		t.Errorf("the catalog lacks a server metric (%d), a metric without a minimum (%d) or a rate (%d): the checks above are vacuous", server, noMin, rates)
	}
}

func TestDefinitionSmallestChange(t *testing.T) {
	b := pageHTML(t, "defs", func(d *PageData) {
		rows := d.V["defs"].Tables.Defs
		for i := range rows {
			if rows[i].ID == "recall.hit_rate" {
				rows[i].N, rows[i].MDC = 120, ptrF(0.142)
			}
		}
	})
	cells := rowByID(parseHTML(t, b), "recall.hit_rate").find("td")
	if cells[8].text() != "120" || cells[9].text() != "about 14 pts" || cells[7].text() != "20" || cells[5].text() != "rate" {
		t.Errorf("recall.hit_rate: fewest %q, n %q, smallest %q, unit %q", cells[7].text(), cells[8].text(), cells[9].text(), cells[5].text())
	}
	if h := parseHTML(t, b).find("main thead th"); len(h) == 0 || !strings.Contains(h[8].text(), "n now (7 days)") {
		t.Error("the n heading does not name its window")
	}
}

// The axis tabs link the sections, each holding the metrics of one axis,
// and the catalog is on one page whatever the paging parameters say.
func TestDefinitionAxisTabs(t *testing.T) {
	h := dataHarness(t)
	for _, path := range []string{"/defs", "/defs?limit=5&offset=5"} {
		_, b := pageGet(t, h, h.login(), path)
		doc := parseHTML(t, b)
		if got := len(idsOf(doc)); got != len(metrics.Catalog) {
			t.Fatalf("%s: %d rows, want the %d of the catalog", path, got, len(metrics.Catalog))
		}
		if doc.first("main .pager") != nil || doc.first("main a[rel=next]") != nil {
			t.Errorf("%s pages the catalog", path)
		}
		var tabs []string
		total := 0
		inSection := map[string]string{} // metric id -> its section's id
		for _, a := range doc.find("main nav.seg a") {
			tabs = append(tabs, strings.Fields(a.text())[0])
			sec := doc.first("main section" + a.attr("href"))
			if sec == nil {
				t.Fatalf("%s: tab %q links %q, which no section has", path, a.text(), a.attr("href"))
			}
			rows := sec.find("tbody tr")
			if want := a.first("span").text(); want != en.Int(int64(len(rows))) {
				t.Errorf("tab %q says %s, its section holds %d", a.text(), want, len(rows))
			}
			total += len(rows)
			for _, tr := range rows {
				inSection[tr.attr("id")] = sec.attr("id")
			}
			if head := sec.first("h3"); head == nil || !strings.HasPrefix(a.text(), head.text()) {
				t.Errorf("section %s has the heading %v", a.attr("href"), head)
			}
		}
		if got := strings.Join(tabs, ","); !strings.HasPrefix(got, "Found,Right,Cost,Bank,Flowing") {
			t.Errorf("%s: tabs %q, want the canvas's order", path, got)
		}
		if total != len(metrics.Catalog) {
			t.Errorf("%s: the sections hold %d rows, the catalog %d", path, total, len(metrics.Catalog))
		}
		for _, d := range metrics.Catalog {
			if got, want := inSection[d.ID], "axis-"+views.Heading(d); got != want {
				t.Errorf("%s is in section %q, want %q", d.ID, got, want)
			}
		}
	}
	if len(metrics.Catalog) > metrics.MaxLimit {
		t.Errorf("the catalog (%d) no longer fits one page (%d)", len(metrics.Catalog), metrics.MaxLimit)
	}
}

// innerCode is the text inside the first <code> of n.
func innerCode(n *node) string {
	for _, it := range n.first("code").items {
		if it.kid == nil {
			return it.text
		}
	}
	return ""
}

// TestDefinitionsArePlainForHooks: the catalog's own words for
// hooks.failures use a term the pages do not, so its row says the override
// and the others keep the catalog's text.
func TestDefinitionsArePlainForHooks(t *testing.T) {
	b := pageHTML(t, "defs", nil)
	doc := parseHTML(t, b)
	d, _ := metrics.DefOf("hooks.failures")
	if d.Definition == hooksFailuresWhat {
		t.Fatal("the catalog already says the override: the test is vacuous")
	}
	var row *node
	for _, tr := range doc.find("main tbody tr") {
		if tr.attr("id") == "hooks.failures" {
			row = tr
		}
	}
	if row == nil {
		t.Fatal("no hooks.failures row")
	}
	got := row.find("td")[3].text()
	if got != hooksFailuresWhat {
		t.Errorf("definition %q, want %q", got, hooksFailuresWhat)
	}
	if strings.Contains(b, d.Definition) {
		t.Errorf("the page still carries the catalog's text %q", d.Definition)
	}
	other, _ := metrics.DefOf("recall.hit_rate")
	for _, tr := range doc.find("main tbody tr") {
		if tr.attr("id") == "recall.hit_rate" && tr.find("td")[3].text() != other.Definition {
			t.Errorf("recall.hit_rate: %q, want the catalog's %q", tr.find("td")[3].text(), other.Definition)
		}
	}
}

func TestDefinitionIDIsEscaped(t *testing.T) {
	hostile := `x" onmouseover="y"><b id="z">`
	b := pageHTML(t, "defs", func(d *PageData) { d.V["defs"].Tables.Defs[0].ID = hostile })
	doc := parseHTML(t, b)
	if got := idsOf(doc)[0]; got != hostile {
		t.Errorf("row id %q, want the hostile value as plain text", got)
	}
	if n := len(doc.find("main b")) + len(doc.find("main tr[onmouseover]")); n != 0 {
		t.Errorf("the id broke out of its attribute: %d injected nodes", n)
	}
}

func TestReleaseAnchors(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	res := apiPath(t, h, "/api/v1/releases")
	_, b := pageGet(t, h, ck, "/releases")
	ids := idsOf(parseHTML(t, b))
	var want []string
	for _, r := range res.Tables.Releases {
		want = append(want, r.Version)
	}
	if len(want) == 0 || !slices.Equal(ids, want) {
		t.Fatalf("row ids %q, the versions (newest first) %q", ids, want)
	}
	if !strings.Contains(visibleText(b), "Observational: weeks, projects and memory differ.") {
		t.Errorf("the page does not say it is observational: %q", visibleText(b))
	}
	pages := append([]string{"/", "/quality", "/cost", "/bank", "/pipeline", "/system", "/releases", "/defs"}, browsePaths(t, h)...)
	for _, p := range pages {
		_, body := pageGet(t, h, ck, p)
		doc := parseHTML(t, body)
		// The footer names the running version, which may have no row in the window.
		foot := releaseLinks(doc, "footer.foot a")
		if len(foot) != 1 {
			t.Fatalf("%s: %d version links in the footer", p, len(foot))
		}
		if got := fragmentsOf(t, `<a href="`+foot[0].attr("href")+`">`, "/releases"); len(got) != 1 || got[0] != "test" || foot[0].text() != "test" {
			t.Errorf("%s: the footer links %q for the version %q, want test", p, got, foot[0].text())
		}
		for _, a := range releaseLinks(doc, "main a") {
			f := fragmentsOf(t, `<a href="`+a.attr("href")+`">`, "/releases")
			if len(f) != 1 || !slices.Contains(ids, f[0]) {
				t.Errorf("%s links %s, which has no row", p, a.attr("href"))
			}
		}
	}
	if links := releaseLinks(parseHTML(t, b), "main tbody a"); len(links) != len(want) {
		t.Errorf("%d version links in the table, want one per row (%d)", len(links), len(want))
	}
}

func TestReleaseVersionIsEscaped(t *testing.T) {
	hostile := `v"><b id="z">1 a#b`
	b := pageHTML(t, "releases", func(d *PageData) { d.V["releases"].Tables.Releases[0].Version = hostile })
	doc := parseHTML(t, b)
	if got := idsOf(doc)[0]; got != hostile {
		t.Errorf("row id %q, want the hostile version as plain text", got)
	}
	if n := len(doc.find("main tbody b")); n != 0 {
		t.Errorf("the version broke out of its attribute: %d injected nodes", n)
	}
	got := fragmentsOf(t, b, "/releases")
	if len(got) == 0 || got[0] != hostile {
		t.Errorf("link fragments %q, want the version first, decoded", got)
	}
	if !strings.Contains(b, `#v%22%3E%3Cb%20id=%22z%22%3E1%20a%23b"`) {
		t.Errorf("the version is not escaped as a fragment: %q", fragmentsOf(t, b, "/releases"))
	}
}

// The rows follow the view's order (newest first); the page does not sort.
func TestReleaseRowsKeepTheViewsOrder(t *testing.T) {
	var golden []string
	b := pageHTML(t, "releases", func(d *PageData) {
		for _, r := range d.V["releases"].Tables.Releases {
			golden = append(golden, r.Version)
		}
	})
	if got := idsOf(parseHTML(t, b)); len(golden) < 2 || !slices.Equal(got, golden) {
		t.Errorf("rows %q, the view's %q", got, golden)
	}
	b = pageHTML(t, "releases", func(d *PageData) {
		rows := d.V["releases"].Tables.Releases
		rows[0], rows[1] = rows[1], rows[0]
	})
	if got := idsOf(parseHTML(t, b)); !slices.Equal(got, []string{golden[1], golden[0]}) {
		t.Errorf("swapped rows %q", got)
	}
}

func TestReleaseLinkKeepsTheWindow(t *testing.T) {
	b := pageHTML(t, "releases", nil)
	if !strings.Contains(b, `href="/releases?days=30#v0.1.18"`) {
		t.Errorf("a version link lacks the window: %q", linksIn(parseHTML(t, b), "main tbody"))
	}
}

func TestReleaseTableCells(t *testing.T) {
	b := pageHTML(t, "releases", func(d *PageData) {
		r := d.V["releases"]
		r.GeneratedAt = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
		row := &r.Tables.Releases[0]
		row.First = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
		row.Last = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
		row.N, row.PromptsPerDay, row.BankSize = 1234, 24.46, ptrI(1284)
		row.ProjectShares = map[string]float64{"zeta": 0.25, "alpha": 0.375, "beta": 0.375}
		hit := row.Flagship["recall.hit_rate"]
		hit.Value, hit.CI = ptrF(0.29), &[2]float64{0.18, 0.42}
		row.Flagship["recall.hit_rate"] = hit
		used := row.Flagship["used.turn_rate"]
		used.Value, used.Num, used.Den = nil, ptrI(5), ptrI(7)
		row.Flagship["used.turn_rate"] = used
		wrong := row.Flagship["verdict.wrong_rate"]
		wrong.Value, wrong.Num, wrong.Den = nil, ptrI(0), ptrI(6)
		row.Flagship["verdict.wrong_rate"] = wrong
		lat := row.Flagship["recall.latency"]
		lat.Value = ptrF(1600)
		row.Flagship["recall.latency"] = lat
	})
	doc := parseHTML(t, b)
	tr := doc.find("main tbody tr")[0]
	var got []string
	for _, td := range tr.find("td") {
		got = append(got, td.text())
	}
	want := []string{"v0.1.18", "Oct 3 → Oct 21", "1,234", "7", "29 % (18–42 %)", "5 of 7", "—", "0 of 6", "—", "1.6 s", "alpha 38 %", "24.5", "1,284"}
	if !slices.Equal(got, want) {
		t.Errorf("cells\n got %q\nwant %q", got, want)
	}
	few := tr.find("td")[5].first("span")
	if few == nil || !strings.Contains(few.attr("class"), "st-few") {
		t.Errorf("a rate with too little behind it is not marked: %v", few)
	}
	if plain := tr.find("td")[7]; plain.first("span") != nil {
		t.Errorf("0 of 6 is marked like a count that is too small: %q", plain.text())
	}
	// The hover names every project, the largest first, ties by name.
	if title := tr.find("td")[10].attr("title"); title != "alpha 38 %; beta 38 %; zeta 25 %" {
		t.Errorf("shares %q", title)
	}
	if h := doc.find("main thead th"); len(h) == 0 || !strings.Contains(b, "<th class=\"r xs\">recall.hit_rate</th>") {
		t.Error("the flagship ids are not named under their headings")
	}
}

func ptrI(n int64) *int64     { return &n }
func ptrF(v float64) *float64 { return &v }

func TestReleaseTopProjectIsTheNoProjectWords(t *testing.T) {
	b := pageHTML(t, "releases", func(d *PageData) {
		d.V["releases"].Tables.Releases[0].ProjectShares = map[string]float64{metrics.NoProject: 1}
	})
	td := parseHTML(t, b).find("main tbody tr")[0].find("td")[10]
	if got := td.text(); got != "no project 100 %" {
		t.Errorf("top project %q", got)
	}
	if strings.Contains(b, metrics.NoProject) {
		t.Error("the stored placeholder reaches the page")
	}
}

func TestReleaseSettingsAreSortedAndWritten(t *testing.T) {
	b := pageHTML(t, "releases", func(d *PageData) {
		d.V["releases"].Tables.Settings = map[string]float64{
			"recall.session_budget_chars": 6000, "feedback.boost_weight": 0.1, "recall.min_score": 0.55,
		}
	})
	var got [][2]string
	for _, tr := range parseHTML(t, b).find("main .grid-2 tbody tr") {
		td := tr.find("td")
		got = append(got, [2]string{td[0].text(), td[1].text()})
	}
	want := [][2]string{{"feedback.boost_weight", "0.1"}, {"recall.min_score", "0.55"}, {"recall.session_budget_chars", "6,000"}}
	if !slices.Equal(got, want) {
		t.Errorf("settings %q, want %q", got, want)
	}
	if !strings.Contains(b, "<code>backsight feedback replay --set ") {
		t.Error("no replay command in <code>")
	}
}

func TestReleasesWithoutVersionsSaySo(t *testing.T) {
	b := pageHTML(t, "releases", func(d *PageData) {
		d.V["releases"].Tables.Releases, d.V["releases"].Tables.Page = nil, nil
	})
	doc := parseHTML(t, b)
	if n := len(doc.find("main .tbl-wrap")); n != 1 { // the settings table only
		t.Errorf("%d tables, want the settings one alone", n)
	}
	if !strings.Contains(visibleText(b), "No version recorded a prompt in this window.") {
		t.Errorf("no empty sentence: %q", visibleText(b))
	}
	if !strings.Contains(visibleText(b), "Observational: weeks, projects and memory differ.") {
		t.Error("the observational sentence went with the rows")
	}
}

// Among equal shares the first name by order is the top project, whatever
// order the map is walked in.
func TestTopShareBreaksTiesByName(t *testing.T) {
	shares := map[string]float64{}
	for _, n := range []string{"h", "d", "a", "f", "c", "g", "b", "e"} {
		shares[n] = 0.125
	}
	for range 20 {
		if name, _, ok := topShare(shares); !ok || name != "a" {
			t.Fatalf("top project %q, want a", name)
		}
	}
	if _, _, ok := topShare(nil); ok {
		t.Error("a version with no project has a top project")
	}
}

func TestReleaseSpanNamesYearsAndTheRunningVersion(t *testing.T) {
	b := pageHTML(t, "releases", func(d *PageData) {
		r := d.V["releases"]
		r.GeneratedAt = time.Date(2027, 1, 5, 12, 0, 0, 0, time.UTC)
		d.Loc = time.UTC
		d.Frame.Version = r.Tables.Releases[1].Version
		r.Tables.Releases[0].First = time.Date(2026, 12, 20, 9, 0, 0, 0, time.UTC)
		r.Tables.Releases[0].Last = time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC)
		r.Tables.Releases[1].First = time.Date(2027, 1, 3, 9, 0, 0, 0, time.UTC)
		r.Tables.Releases[1].Last = time.Date(2027, 1, 4, 9, 0, 0, 0, time.UTC)
	})
	rows := parseHTML(t, b).find("main tbody tr")
	if got := rows[0].find("td")[1].text(); got != "Dec 20, 2026 → Jan 2" {
		t.Errorf("a span over a year: %q", got)
	}
	if got := rows[1].find("td")[1].text(); got != "Jan 3 → now" {
		t.Errorf("the running version: %q", got)
	}
	if strings.Contains(rows[0].find("td")[1].text(), "now") {
		t.Error("a version that is not the running one reads as in use")
	}
}

func TestReleasePageSentences(t *testing.T) {
	b := pageHTML(t, "releases", nil)
	text := visibleText(b)
	for _, w := range []string{
		"Attributed per event to the binary that recorded it, not per day. Comparing two versions with matched projects comes later.",
		"Replay recorded recalls with another learned-ranking setting, without touching memory or config.toml:",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("the page lacks %q", w)
		}
	}
	var cmds []string
	for _, c := range parseHTML(t, b).find("main div.cmd code") {
		cmds = append(cmds, innerText(c))
	}
	if !slices.Contains(cmds, "backsight feedback replay --set boost_weight=0.15") {
		t.Errorf("commands %q lack the replay command", cmds)
	}
}

func innerText(n *node) string {
	for _, it := range n.items {
		if it.kid == nil {
			return it.text
		}
	}
	return ""
}

// releaseLinks are the links in sel that go to the Releases page.
func releaseLinks(n *node, sel string) []*node {
	var out []*node
	for _, a := range n.find(sel) {
		if strings.HasPrefix(a.attr("href"), "/releases") {
			out = append(out, a)
		}
	}
	return out
}

// A metric with no family is listed, under Other, instead of breaking the page.
func TestDefinitionWithoutFamilyIsUnderOther(t *testing.T) {
	b := pageHTML(t, "defs", func(d *PageData) {
		d.V["defs"].Tables.Defs[0].Family = ""
	})
	doc := parseHTML(t, b)
	id := doc.find("main tbody tr")
	var other *node
	for _, sec := range doc.find("main section") {
		if sec.attr("id") == "axis-other" {
			other = sec
		}
	}
	if other == nil || other.first("h3").text() != "Other" {
		t.Fatalf("no Other section among %d rows", len(id))
	}
	if rows := other.find("tbody tr"); len(rows) != 1 || rows[0].attr("id") != metrics.Catalog[0].ID {
		t.Errorf("Other holds %d rows", len(rows))
	}
	var tab string
	for _, a := range doc.find("main nav.seg a") {
		if a.attr("href") == "#axis-other" {
			tab = a.text()
		}
	}
	if tab != "Other 1" {
		t.Errorf("tab %q, want Other 1", tab)
	}
}
