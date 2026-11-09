package dashboard

import (
	"encoding/json"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
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
		want = append(want, releaseIDPrefix+r.Version)
	}
	if len(want) == 0 || !slices.Equal(ids, want) {
		t.Fatalf("row ids %q, the prefixed versions (newest first) %q", ids, want)
	}
	if !strings.Contains(visibleText(b), "Observational: weeks, projects and memory differ.") {
		t.Errorf("the page does not say it is observational: %q", visibleText(b))
	}
	pages := append([]string{"/", "/quality", "/cost", "/bank", "/pipeline", "/system", "/releases", "/defs"}, browsePaths(t, h)...)
	for _, p := range pages {
		// The raw body: unescaping it first would turn a literal "<machine_id>" of a
		// popover into a tag.
		doc := parseHTML(t, body(t, h.do("GET", p, cookieHdr(ck)...)))
		// The footer names the running version, which may have no row in the window.
		foot := releaseLinks(doc, "footer.foot a")
		if len(foot) != 1 {
			t.Fatalf("%s: %d version links in the footer", p, len(foot))
		}
		if got := fragmentsOf(t, `<a href="`+foot[0].attr("href")+`">`, "/releases"); len(got) != 1 || got[0] != releaseIDPrefix+"test" || foot[0].text() != "test" {
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
	if got := idsOf(doc)[0]; got != "release-"+hostile {
		t.Errorf("row id %q, want the hostile version as plain text", got)
	}
	if n := len(doc.find("main tbody b")); n != 0 {
		t.Errorf("the version broke out of its attribute: %d injected nodes", n)
	}
	got := fragmentsOf(t, b, "/releases")
	if len(got) == 0 || got[0] != "release-"+hostile {
		t.Errorf("link fragments %q, want the version first, decoded", got)
	}
	if !strings.Contains(b, `#release-v%22%3E%3Cb%20id=%22z%22%3E1%20a%23b"`) {
		t.Errorf("the version is not escaped as a fragment: %q", fragmentsOf(t, b, "/releases"))
	}
}

// The rows follow the view's order (newest first); the page does not sort.
func TestReleaseRowsKeepTheViewsOrder(t *testing.T) {
	var golden []string
	b := pageHTML(t, "releases", func(d *PageData) {
		for _, r := range d.V["releases"].Tables.Releases {
			golden = append(golden, releaseIDPrefix+r.Version)
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
	if !strings.Contains(b, `href="/releases?days=30#release-v0.1.18"`) {
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
	// the settings table only: the time chart's table sits in its figure
	var tables int
	for _, w := range doc.find("main .tbl-wrap") {
		if !inside(w, "figure") {
			tables++
		}
	}
	if tables != 1 {
		t.Errorf("%d tables, want the settings one alone", tables)
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
		"Attributed per event to the binary that recorded it, not per day.",
		"Replay recorded recalls with another learned-ranking setting, without touching memory or config.toml:",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("the page lacks %q", w)
		}
	}
	if strings.Contains(text, "comes later") {
		t.Error("the page still says the comparison comes later")
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

// A version named like an element of the page cannot take that element's id
// (red while the row id was the bare version: "main" and "copy-status" would
// have been duplicated).
func TestReleaseIDsCannotCollideWithThePage(t *testing.T) {
	b := pageHTML(t, "releases", func(d *PageData) {
		rows := d.V["releases"].Tables.Releases
		rows[0].Version, rows[1].Version = "main", "copy-status"
	})
	seen := map[string]int{}
	for _, id := range idsOf(parseHTML(t, b)) {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("id %q used %d times", id, n)
		}
	}
	if got := strings.Count(b, `id="main"`) + strings.Count(b, `id="copy-status"`); got != 2 {
		t.Errorf("the page's own ids main and copy-status appear %d times, want 2", got)
	}
}

// inside says whether n is within an element called name.
func inside(n *node, name string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Name == name {
			return true
		}
	}
	return false
}

// versionA and versionB are the versions of the design's fixture of record.
const versionA, versionB = "v0.1.16-m7", "v0.1.17-m7"

// recordComparison is the engine's answer for the fixture of record (the
// golden internal/metrics writes: four groups, two compared, one short of
// turns, one short of days; the pooled difference 0.114625; raw figures that
// disagree with it), plus the setting that differs, which the engine reads
// from the config records and the golden does not hold.
func recordComparison() *metrics.Comparison {
	raw, err := os.ReadFile(filepath.Join("..", "metrics", "testdata", "compare-record.golden.json"))
	if err != nil {
		panic(err)
	}
	c := &metrics.Comparison{}
	if err := json.Unmarshal(raw, c); err != nil {
		panic(err)
	}
	changed := time.Date(2026, 10, 7, 18, 0, 0, 0, parisZone())
	c.SettingsDiffer = []metrics.SettingDiff{{Key: "recall.min_score", A: fp(0.5), B: fp(0.55), ChangedAt: &changed}}
	return c
}

// compareHTML is the Releases page with the comparison c in its view.
func compareHTML(t *testing.T, c *metrics.Comparison, edit ...func(d *PageData)) string {
	t.Helper()
	return pageHTML(t, "releases", func(d *PageData) {
		d.V["releases"].Tables.Compare = c
		if c != nil {
			d.P.Compare = c.A + "," + c.B
		}
		for _, e := range edit {
			e(d)
		}
	})
}

// compareCard is the card of the comparison on a page.
func compareCard(t *testing.T, page string) *node {
	t.Helper()
	for _, s := range parseHTML(t, page).find("main section.card") {
		if h := s.first("h2"); h != nil && h.text() == "Do two versions differ?" {
			return s
		}
	}
	t.Fatal("no card Do two versions differ?")
	return nil
}

// needFirst is the first element of n matching sel; the test stops without one.
func needFirst(t *testing.T, n *node, sel string) *node {
	t.Helper()
	f := n.first(sel)
	if f == nil {
		t.Fatalf("no %s", sel)
	}
	return f
}

// needAll are the elements of n matching sel, at least min of them.
func needAll(t *testing.T, n *node, sel string, min int) []*node {
	t.Helper()
	all := n.find(sel)
	if len(all) < min {
		t.Fatalf("%d of %s, want %d", len(all), sel, min)
	}
	return all
}

func lastOf(ns []*node) *node { return ns[len(ns)-1] }

// pillDesc describes a pill for a failure message; n may be nil.
func pillDesc(n *node) string {
	if n == nil {
		return "no pill"
	}
	return "pill " + strconv.Quote(n.text()) + " of class " + strconv.Quote(n.attr("class"))
}

// settingsLine is the "Settings differ" line of the comparison card.
func settingsLine(t *testing.T, card *node) *node {
	t.Helper()
	for _, s := range card.find("div.state") {
		if strings.HasPrefix(s.text(), "Settings differ:") {
			return s
		}
	}
	t.Fatal("no settings line")
	return nil
}

// notStatus fails when the pill is drawn in a status colour.
func notStatus(t *testing.T, what string, n *node) {
	t.Helper()
	for _, bad := range []string{"st-ok", "st-watch", "st-bad", "st-border", "st-learn"} {
		if slices.Contains(strings.Fields(n.attr("class")), bad) {
			t.Errorf("%s is drawn in a status colour: class %q", what, n.attr("class"))
		}
	}
}

func TestComparisonPage(t *testing.T) {
	c := compareCard(t, compareHTML(t, recordComparison()))

	// the readings first: Observational, the settings, the headline
	if got := needFirst(t, c, "div.state").text(); got != "Observational, not an experiment. The two versions ran on different weeks, on different work, with different memory. Matching by project and place removes some of that, not all." {
		t.Errorf("the observational box: %q", got)
	}
	var settings *node
	for _, s := range c.find("div.state") {
		if strings.HasPrefix(s.text(), "Settings differ:") {
			settings = s
		}
	}
	if settings == nil {
		t.Fatal("no settings line")
	}
	// A caveat, not a fault: drawn as the page's other caveats are, with the
	// info mark and no status colour.
	notStatus(t, "the settings line", settings)
	if settings.attr("class") != "state st-few" || settings.first("span.i-info") == nil {
		t.Errorf("settings line is %q with info mark %v, want the caveat style (state st-few, i-info)", settings.attr("class"), settings.first("span.i-info") != nil)
	}
	if got := settings.text(); got != "Settings differ: was 0.5 under v0.1.16-m7 and 0.55 under v0.1.17-m7 (changed Oct 7). A difference may come from the setting, not the release." {
		t.Errorf("settings line: %q", got)
	}
	if k := settings.first("code"); k == nil || k.Text != "recall.min_score" {
		t.Errorf("the setting is not named in <code>")
	}
	lead := c.first("div.lead")
	if lead == nil {
		t.Fatal("no headline")
	}
	if got := needFirst(t, lead, "span.big").text(); got != "+11 pts" {
		t.Errorf("headline %q", got)
	}
	if got := needFirst(t, lead, "span.sub").text(); got != "(+2 to +20) v0.1.17-m7 minus v0.1.16-m7, matched" {
		t.Errorf("headline detail %q", got)
	}
	pill := lead.first("span.pill")
	if pill == nil || pill.text() != "differs" || !strings.Contains(pill.attr("class"), "st-info") {
		t.Fatalf("reading: %s", pillDesc(pill))
	}
	notStatus(t, "the reading", pill)
	if got := needAll(t, lead, "div", 3)[2].text(); got != "Unmatched, the raw numbers say the opposite, −5 pts (57 % → 52 %): v0.1.17-m7 ran mostly on project-a (190 of its 295 turns), and v0.1.16-m7 mostly on backsight (236 of 326). Read the matched figure." {
		t.Errorf("raw line %q", got)
	}
	if strings.Contains(c.text(), "No clear difference: the interval includes 0") {
		t.Error("a difference that differs says there is no clear difference")
	}

	// the forest: two squares, the diamond, the two groups in words
	plot := c.first("div.chart-plot svg")
	if plot == nil {
		t.Fatal("no forest chart")
	}
	if n := len(plot.find("path.ci-pt")); n != 2 {
		t.Errorf("%d squares, want 2", n)
	}
	if n := len(plot.find("path.ci-pool")); n != 1 {
		t.Errorf("%d diamonds, want 1", n)
	}
	var titles []string
	for _, l := range plot.find("line.ci-ln") {
		titles = append(titles, needFirst(t, l, "title").text())
	}
	wantTitles := []string{
		"backsight · added to the prompt: +7 pts (−6 to +19), no clear difference",
		"project-a · added to the prompt: +16 pts (+3 to +29), differs",
		"Matched, pooled: +11 pts (+2 to +20), differs",
	}
	if !slices.Equal(titles, wantTitles) {
		t.Errorf("forest marks %q, want %q", titles, wantTitles)
	}
	var few []string
	for _, f := range plot.find("text.few-t") {
		few = append(few, f.text())
	}
	if want := []string{"too few: v0.1.17-m7 has 12 turns, 30 needed", "too few: v0.1.16-m7 ran 4 days, 7 needed"}; !slices.Equal(few, want) {
		t.Errorf("groups too few: %q, want %q", few, want)
	}
	if got := lastOf(needAll(t, plot, "text.ta-e", 1)).text(); !strings.Contains(got, "lower in v0.1.17-m7") {
		t.Errorf("axis note %q", got)
	}

	// the table: one row a group, then the pooled and the unmatched rows
	tbl := tableCaptioned(t, c, "Each project and place, and the matched total")
	if got := needFirst(t, tbl, "thead tr").text(); got != "Project Where memory was shown v0.1.16-m7 v0.1.17-m7 Difference (95 %) Reading" {
		t.Errorf("table head %q", got)
	}
	want := [][]string{
		{"backsight", "added to the prompt", "70 % (140 of 200 turns, 21 d)", "77 % (46 of 60 turns, 9 d)", "+7 pts (−6 to +19)", "no clear difference"},
		{"backsight", "Claude's search", "47 % (17 of 36 turns, 20 d)", "too few (6 of 12 turns)", "—", "needs 30 turns in each version"},
		{"project-a", "added to the prompt", "30 % (21 of 70 turns, 18 d)", "46 % (87 of 190 turns, 9 d)", "+16 pts (+3 to +29)", "differs: higher in v0.1.17-m7"},
		{"project-b", "added to the prompt", "too few (9 of 20 turns, 4 days)", "45 % (15 of 33 turns, 8 d)", "—", "needs 7 days in each version"},
		{"Matched, pooled (2 of 4 pairs)", "49 %", "60 %", "+11 pts (+2 to +20)", "differs: higher in v0.1.17-m7"},
		{"All turns, unmatched", "57 % (187 of 326)", "52 % (154 of 295)", "−5 pts", "not comparable: the mix of projects differs"},
	}
	got := rowsOf(tbl)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("table rows\n got %q\nwant %q", got, want)
	}
	if tr := needAll(t, tbl, "tbody tr", 6)[4]; tr.attr("class") != "total" {
		t.Errorf("the pooled row is not the total row: %q", tr.attr("class"))
	}
	if got := needFirst(t, c, "p.tbl-more").text(); got != "Pooled rates are each version's rate in the matched pairs, weighted by pair size (the two versions' turns combined as a·b / (a + b): 46 and 51), so the same weights apply to both versions." {
		t.Errorf("weights note %q", got)
	}
	// the cell of a group too few in its days is a neutral pill, not a colour
	for _, p := range tbl.find("span.st") {
		notStatus(t, "a too-few cell", p)
	}

	// without a disagreement there is no raw sentence, and the unmatched row says so
	agree := recordComparison()
	agree.RawDisagrees = false
	card := compareCard(t, compareHTML(t, agree))
	if strings.Contains(card.text(), "the raw numbers say the opposite") {
		t.Error("a raw sentence without a disagreement")
	}
	rows := rowsOf(tableCaptioned(t, card, "Each project and place, and the matched total"))
	if len(rows) != 6 {
		t.Fatalf("%d rows, want 6", len(rows))
	}
	if last := rows[len(rows)-1]; last[len(last)-1] != "raw figures, not matched" {
		t.Errorf("unmatched row without a disagreement: %q", last)
	}
	// the raw sentence names where each version mostly ran, in its measure's units
	prompts := recordComparison()
	prompts.Measure = "recall.hit_rate"
	if got := needAll(t, compareCard(t, compareHTML(t, prompts)), "div.lead div", 3)[2].text(); !strings.Contains(got, "(190 of its 295 prompts), and v0.1.16-m7 mostly on backsight (236 of 326). Read") {
		t.Errorf("raw line in prompts: %q", got)
	}
	// no project holds half of a version's units: the generic sentence, no project named
	spread := recordComparison()
	for i := range spread.Strata {
		spread.Strata[i].Project = "proj-" + strconv.Itoa(i)
	}
	spread.Strata[0].A.Den = 162 // 162 of 326 is just under half
	line := needAll(t, compareCard(t, compareHTML(t, spread)), "div.lead div", 3)[2].text()
	if want := "Unmatched, the raw numbers say the opposite, −5 pts (57 % → 52 %): the mix of projects differs between the two versions. Read the matched figure."; line != want {
		t.Errorf("raw line without a majority:\n got %q\nwant %q", line, want)
	}
	// the side that has a majority is not enough: both must
	oneSide := recordComparison() // B keeps project-a (190 of 295); A is spread over four projects
	oneSide.Strata[0].A.Den = 100
	for i, name := range []string{"w", "x", "project-a", "z"} {
		oneSide.Strata[i].Project = name
	}
	if got := needAll(t, compareCard(t, compareHTML(t, oneSide)), "div.lead div", 3)[2].text(); !strings.Contains(got, "the mix of projects differs") {
		t.Errorf("raw line with one side spread: %q", got)
	}
	// no setting differs: no settings line; a measure of verdicts counts verdicts
	plain := recordComparison()
	plain.SettingsDiffer = nil
	if strings.Contains(compareCard(t, compareHTML(t, plain)).text(), "Settings differ") {
		t.Error("a settings line without a setting that differs")
	}
	verdicts := recordComparison()
	verdicts.Measure = "verdict.wrong_rate"
	vrows := rowsOf(tableCaptioned(t, compareCard(t, compareHTML(t, verdicts)), "Each project and place, and the matched total"))
	if len(vrows) != 6 {
		t.Fatalf("%d verdict rows, want 6", len(vrows))
	}
	if vrows[0][2] != "70 % (140 of 200 verdicts, 21 d)" || vrows[5][0] != "All verdicts, unmatched" {
		t.Errorf("verdict units: %q", vrows)
	}
}

func TestComparisonShowsWhatItAsksAndHowToAskIt(t *testing.T) {
	c := compareCard(t, compareHTML(t, recordComparison(), func(d *PageData) { d.P.Measure = "recall.hit_rate" }))
	cmd := needFirst(t, c, "details.cli code").Text
	if cmd != "backsight stats releases --days 30 --compare "+posixOrPS("v0.1.16-m7,v0.1.17-m7", "'v0.1.16-m7,v0.1.17-m7'")+" --measure recall.hit_rate" { // PowerShell reads a bare comma as an array
		t.Errorf("command %q", cmd)
	}
	if got := needAll(t, c, "p.sub", 1)[0].text(); got != "Comparing v0.1.16-m7 with v0.1.17-m7 on turns where Claude used memory:" {
		t.Errorf("which comparison: %q", got)
	}
	if info := c.first("details.info div.pop"); info == nil || !strings.Contains(info.text(), "Never “better”") {
		t.Errorf("no explanation of the comparison")
	}
}

func TestComparisonPicker(t *testing.T) {
	h := dataHarness(t)
	hdr := cookieHdr(h.login())
	for _, c := range []struct{ query, want string }{
		{"a=v1&b=v2&measure=recall.hit_rate", "/releases?compare=v1,v2&measure=recall.hit_rate"},
		{"a=v1&b=v2", "/releases?compare=v1,v2"},
		// the window travels with the comparison, after it
		{"days=30&a=v1&b=v2&measure=verdict.wrong_rate&tz=UTC", "/releases?compare=v1,v2&measure=verdict.wrong_rate&days=30&tz=UTC"},
		// a version is escaped in the address
		{"a=" + url.QueryEscape(`v1"><b>`) + "&b=v2", "/releases?compare=" + url.QueryEscape(`v1"><b>`) + ",v2"},
	} {
		r := h.do("GET", "/releases?"+c.query, hdr...)
		if r.StatusCode != 303 || r.Header.Get("Location") != c.want {
			t.Errorf("%s: %d %q, want 303 %q", c.query, r.StatusCode, r.Header.Get("Location"), c.want)
		}
	}
	// the redirect is followed by a page that asks the view
	r := h.do("GET", "/releases?compare=v1,v2&measure=recall.hit_rate", hdr...)
	if r.StatusCode != 200 || !strings.Contains(body(t, r), "Comparing v1 with v2 on prompts that got memory:") {
		t.Errorf("the comparison page: %d", r.StatusCode)
	}
	// the same version twice, a missing one or an unknown measure: the view's sentence, 400
	for _, c := range []struct{ query, want string }{
		{"a=v1&b=v1", "--compare: A and B are the same version."},
		{"a=v1&b=", "--compare: two versions, as A,B."},
		{"a=v1&b=v2&measure=bogus", "--measure: one of used.turn_rate, recall.hit_rate, verdict.wrong_rate."},
	} {
		r := h.do("GET", "/releases?"+c.query, hdr...)
		if r.StatusCode != 400 || r.Header.Get("Location") != "" || !strings.Contains(visibleText(body(t, r)), c.want) {
			t.Errorf("%s: %d %q, want 400 with %q", c.query, r.StatusCode, r.Header.Get("Location"), c.want)
		}
	}
	// the filters travel through the redirect
	r = h.do("GET", "/releases?a=v1&b=v2&project=p1", hdr...)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/releases?compare=v1,v2&project=p1" {
		t.Errorf("project: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// a version that never ran in the window is too few, not an error
	r = h.do("GET", "/releases?a=v9&b=v2", hdr...)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/releases?compare=v9,v2" {
		t.Errorf("unknown version: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	r = h.do("GET", "/releases?compare=v9,v2", hdr...)
	card := compareCard(t, body(t, r))
	if r.StatusCode != 200 || !strings.Contains(card.text(), "Too few to compare. v9 has run 0 days with 0 turns that said what they used;") {
		t.Errorf("unknown version page: %d %q", r.StatusCode, card.text())
	}
	// the picker is only the Releases page's
	if r := h.do("GET", "/quality?a=v1&b=v2", hdr...); r.StatusCode != 400 {
		t.Errorf("/quality takes a: %d", r.StatusCode)
	}
}

func TestComparisonPickerLists(t *testing.T) {
	versions := func(n *node, name string) (values []string, selected string) {
		for _, o := range n.find("select[name=" + name + "] option") {
			values = append(values, o.attr("value"))
			if o.has("selected") {
				selected = o.attr("value")
			}
		}
		return values, selected
	}
	// without a comparison: the window's versions, the newest as B and the one before as A
	page := pageHTML(t, "releases", func(d *PageData) { d.Frame.Keep = [][2]string{{"days", "30"}, {"tz", "UTC"}} })
	c := compareCard(t, page)
	var window []string
	for _, r := range goldenResult(t, "releases").Tables.Releases {
		window = append(window, r.Version)
	}
	if len(window) < 2 {
		t.Fatalf("the golden has %d versions", len(window))
	}
	if form := c.first("form"); form == nil || form.attr("method") != "get" || form.attr("action") != "/releases" {
		t.Fatal("the picker is not a GET form to /releases")
	}
	if vs, sel := versions(c, "a"); !slices.Equal(vs, window) || sel != window[1] {
		t.Errorf("A lists %q selecting %q, want %q selecting %q", vs, sel, window, window[1])
	}
	if vs, sel := versions(c, "b"); !slices.Equal(vs, window) || sel != window[0] {
		t.Errorf("B lists %q selecting %q, want %q selecting %q", vs, sel, window, window[0])
	}
	if vs, sel := versions(c, "measure"); !slices.Equal(vs, metrics.CompareMeasures) || sel != "used.turn_rate" {
		t.Errorf("measures %q selecting %q", vs, sel)
	}
	var labels []string
	for _, o := range c.find("select[name=measure] option") {
		labels = append(labels, o.text())
	}
	if want := []string{"Turns where Claude used memory", "Prompts that got memory", "Memories reported wrong or out of date"}; !slices.Equal(labels, want) {
		t.Errorf("measure labels %q", labels)
	}
	var hidden []string
	for _, in := range c.find("form input[type=hidden]") {
		hidden = append(hidden, in.attr("name")+"="+in.attr("value"))
	}
	if want := []string{"days=30", "tz=UTC"}; !slices.Equal(hidden, want) {
		t.Errorf("the window is not carried by the picker: %q", hidden)
	}
	if !strings.Contains(c.text(), "Pick two versions to see whether they differ.") {
		t.Errorf("no hint: %q", c.text())
	}
	// with a comparison: its versions and measure are the ones selected, even outside the window
	cmp := recordComparison()
	cmp.Measure = "verdict.wrong_rate"
	c = compareCard(t, compareHTML(t, cmp))
	if vs, sel := versions(c, "a"); sel != versionA || !slices.Contains(vs, versionA) {
		t.Errorf("A selects %q among %q", sel, vs)
	}
	if vs, sel := versions(c, "b"); sel != versionB || !slices.Contains(vs, versionB) {
		t.Errorf("B selects %q among %q", sel, vs)
	}
	if _, sel := versions(c, "measure"); sel != "verdict.wrong_rate" {
		t.Errorf("measure selects %q", sel)
	}
	// one version only: nothing to compare, said so
	one := pageHTML(t, "releases", func(d *PageData) { d.V["releases"].Tables.Releases = d.V["releases"].Tables.Releases[:1] })
	if got := compareCard(t, one).text(); !strings.Contains(got, "Two versions need to have run in this window to compare them: pick a longer window.") {
		t.Errorf("one version: %q", got)
	}
}

func TestComparisonTooFewAndNoDifference(t *testing.T) {
	retry := time.Date(2026, 11, 6, 9, 0, 0, 0, parisZone())
	few := func(why string, v string, days, units int, retryAfter *time.Time) *metrics.Comparison {
		c := recordComparison()
		c.Diff, c.CI, c.PooledA, c.PooledB, c.Reading, c.RawDisagrees = nil, nil, nil, nil, "too_few", false
		c.TooFew = &metrics.TooFew{Version: v, Days: days, Units: units, Why: why, RetryAfter: retryAfter}
		return c
	}
	// too few by days, with a date to come back
	c := compareCard(t, compareHTML(t, few("days", "v0.1.18-m7", 3, 11, &retry)))
	sentence := "Too few to compare. v0.1.18-m7 has run 3 days with 11 turns that said what they used; each version needs 7 days and 30 such turns in at least one shared project and place. Try again around Nov 6."
	if got := needFirst(t, c, "div.state").text(); got != sentence {
		t.Errorf("too few:\n got %q\nwant %q", got, sentence)
	}
	pill := c.first("div.card-head span.pill")
	if pill == nil || pill.text() != "too few" {
		t.Fatalf("reading: %s", pillDesc(pill))
	}
	notStatus(t, "the too-few reading", pill)
	for _, none := range []string{"div.lead", "figure", "table", "span.big"} {
		if c.first(none) != nil {
			t.Errorf("a comparison that is too few draws %s", none)
		}
	}
	// by units, nothing to wait for: no date; one day and one turn are singular
	c = compareCard(t, compareHTML(t, few("units", "v0.1.18-m7", 1, 1, nil)))
	if got, want := needFirst(t, c, "div.state").text(), "Too few to compare. v0.1.18-m7 has run 1 day with 1 turn that said what it used; each version needs 7 days and 30 such turns in at least one shared project and place."; got != want {
		t.Errorf("too few by units:\n got %q\nwant %q", got, want)
	}
	// the other measures count their own units
	prompts := few("units", "v0.1.18-m7", 8, 12, nil)
	prompts.Measure = "recall.hit_rate"
	if got, want := needFirst(t, compareCard(t, compareHTML(t, prompts)), "div.state").text(), "Too few to compare. v0.1.18-m7 has run 8 days with 12 prompts; each version needs 7 days and 30 such prompts in at least one shared project and place."; got != want {
		t.Errorf("too few prompts:\n got %q\nwant %q", got, want)
	}
	// by sessions: the pooled comparison needs 10 sessions a version
	sess := few("sessions", versionA, 21, 200, nil)
	sess.SessionsA = 4
	if got := needFirst(t, compareCard(t, compareHTML(t, sess)), "div.state").text(); !strings.HasSuffix(got, " Together they need 10 sessions in each version; v0.1.16-m7 has 4.") {
		t.Errorf("too few sessions: %q", got)
	}

	// no clear difference: the interval includes 0
	nd := recordComparison()
	nd.Reading, nd.Diff, nd.CI, nd.RawDisagrees, nd.SettingsDiffer = "no_clear_difference", fp(0.012), &[2]float64{-0.041, 0.058}, false, nil
	c = compareCard(t, compareHTML(t, nd))
	lead := c.first("div.lead")
	if got := needFirst(t, lead, "span.big").text(); got != "+1 pt" {
		t.Errorf("headline %q", got)
	}
	if got := needFirst(t, lead, "span.sub").text(); got != "(−4 to +6) v0.1.17-m7 minus v0.1.16-m7, matched" {
		t.Errorf("headline detail %q", got)
	}
	pill = lead.first("span.pill")
	if pill == nil || pill.text() != "no clear difference" || !strings.Contains(pill.attr("class"), "st-info") {
		t.Fatalf("reading: %s", pillDesc(pill))
	}
	notStatus(t, "the no-clear-difference reading", pill)
	var said []string
	for _, p := range c.find("p.small") {
		said = append(said, p.text())
	}
	if !slices.Contains(said, "No clear difference: the interval includes 0. That is not proof that they are the same.") {
		t.Errorf("no sentence about the interval: %q", said)
	}
	rows := rowsOf(tableCaptioned(t, c, "Each project and place, and the matched total"))
	if len(rows) != 6 {
		t.Fatalf("%d rows, want 6", len(rows))
	}
	if got := rows[4][4]; got != "no clear difference" {
		t.Errorf("pooled reading %q", got)
	}
	// a clear difference downward says lower, never worse
	down := recordComparison()
	down.Diff, down.CI = fp(-0.1), &[2]float64{-0.15, -0.05}
	rows = rowsOf(tableCaptioned(t, compareCard(t, compareHTML(t, down)), "Each project and place, and the matched total"))
	if len(rows) != 6 {
		t.Fatalf("%d rows, want 6", len(rows))
	}
	if got := rows[4][4]; got != "differs: lower in v0.1.17-m7" {
		t.Errorf("pooled reading downward %q", got)
	}
	// the settings of a version are not known
	unk := recordComparison()
	unk.SettingsDiffer, unk.SettingsUnknown = nil, true
	if got := compareCard(t, compareHTML(t, unk)).text(); !strings.Contains(got, "Settings of a version are not known: nothing was recorded before its first turn.") {
		t.Errorf("unknown settings: %q", got)
	}
	// two settings, one new: each is named
	two := recordComparison()
	two.SettingsDiffer = append(two.SettingsDiffer, metrics.SettingDiff{Key: "recall.prompt_hits", A: nil, B: fp(3)})
	var codes []string
	for _, k := range settingsLine(t, compareCard(t, compareHTML(t, two))).find("code") {
		codes = append(codes, k.Text)
	}
	if !slices.Equal(codes, []string{"recall.min_score", "recall.prompt_hits"}) {
		t.Errorf("settings named %q", codes)
	}
}

func TestComparisonEscapesVersions(t *testing.T) {
	a, b, project := `v1"><b>`, `v2'<i>&amp;`, `p"><u>`
	c := recordComparison()
	c.A, c.B = a, b
	c.Strata[0].Project = project
	c.SettingsDiffer[0].Key = `k"><s>`
	page := compareHTML(t, c, func(d *PageData) {
		rel := d.V["releases"].Tables.Releases
		rel[0].Version = b
		d.V["releases"].Tables.Markers = []metrics.Marker{{At: d.V["releases"].Window.Since.Add(36 * time.Hour), Kind: "release", Version: b, Previous: a}}
	})
	for _, raw := range []string{a, project, `k"><s>`, `v2'<i>`} {
		if strings.Contains(page, raw) {
			t.Errorf("the page holds %q unescaped", raw)
		}
	}
	// no element came of a name: the page has as many <b>, <i>, <u> and <s> as with plain names
	plain := recordComparison()
	base := compareHTML(t, plain, func(d *PageData) {
		d.V["releases"].Tables.Markers = []metrics.Marker{{At: d.V["releases"].Window.Since.Add(36 * time.Hour), Kind: "release", Version: "v2", Previous: "v1"}}
	})
	for _, tag := range []string{"<b>", "<i>", "<u>", "<s>"} {
		if got, want := strings.Count(page, tag), strings.Count(base, tag); got != want {
			t.Errorf("%d %s in the page, %d with plain names", got, tag, want)
		}
	}
	card := compareCard(t, page)
	for _, name := range []string{"a", "b"} {
		var vals []string
		for _, o := range card.find("select[name=" + name + "] option") {
			vals = append(vals, o.attr("value"))
			if o.attr("value") != o.text() {
				t.Errorf("option %q reads %q", o.attr("value"), o.text())
			}
		}
		if !slices.Contains(vals, a) || !slices.Contains(vals, b) {
			t.Errorf("select %s lacks the versions: %q", name, vals)
		}
	}
	if got := needFirst(t, card, "div.lead span.sub").text(); got != "(+2 to +20) "+b+" minus "+a+", matched" {
		t.Errorf("headline detail %q", got)
	}
	if got := settingsLine(t, card).text(); !strings.Contains(got, "under "+a+" and 0.55 under "+b) {
		t.Errorf("settings line %q", got)
	}
	if got := needAll(t, card, "div.lead div", 3)[2].text(); !strings.Contains(got, b+" ran mostly on project-a (190 of its 295 turns), and "+a+" mostly on "+project+" (200 of 326).") {
		t.Errorf("raw line %q", got)
	}
	var svgText []string
	for _, n := range card.find("div.chart-plot svg text") {
		svgText = append(svgText, n.text())
	}
	if !slices.Contains(svgText, "← lower in "+b) || !slices.Contains(svgText, project+" · added to the prompt") {
		t.Errorf("chart texts %q", svgText)
	}
	for _, l := range card.find("div.chart-plot svg line.ci-ln") {
		if title := needFirst(t, l, "title").text(); strings.HasPrefix(title, project) && !strings.Contains(title, "differs") && !strings.Contains(title, "no clear") {
			t.Errorf("mark title %q", title)
		}
	}
	if svg := card.first("div.chart-plot svg"); !strings.HasPrefix(svg.attr("aria-label"), "Difference in turns where Claude used memory, "+b+" minus "+a) {
		t.Errorf("chart label %q", svg.attr("aria-label"))
	}
	rows := rowsOf(tableCaptioned(t, card, "Each project and place, and the matched total"))
	if len(rows) != 6 {
		t.Fatalf("%d rows, want 6", len(rows))
	}
	if rows[0][0] != project || rows[2][5] != "differs: higher in "+b {
		t.Errorf("table rows %q", rows)
	}
	// the time chart: the marker's label and sentence
	used := usedCardOf(t, page)
	var marks []string
	for _, m := range used.find("svg line.rel-mk title") {
		marks = append(marks, m.text())
	}
	if len(marks) != 1 || !strings.Contains(marks[0], "new version "+b+" (was "+a+")") {
		t.Errorf("release marker %q", marks)
	}
}

// usedCardOf is the time chart's card on a page.
func usedCardOf(t *testing.T, page string) *node {
	t.Helper()
	for _, s := range parseHTML(t, page).find("main section.card") {
		if h := s.first("h2"); h != nil && h.text() == "When did Claude use memory?" {
			return s
		}
	}
	t.Fatal("no card When did Claude use memory?")
	return nil
}

func TestReleasesMarkersChart(t *testing.T) {
	g := goldenResult(t, "releases")
	var days []string
	for _, p := range g.Metrics["used.turn_rate"].Series {
		days = append(days, p.Day)
	}
	oct7, oct8 := slices.Index(days, "2026-10-07"), slices.Index(days, "2026-10-08")
	if oct7 < 0 || oct8 != oct7+1 || len(g.Tables.Markers) != 1 || g.Tables.Markers[0].Version != "v0.1.18" {
		t.Fatalf("the golden changed: days %q, markers %+v", days, g.Tables.Markers)
	}
	two := func(d *PageData) {
		r := d.V["releases"]
		r.Tables.Markers = append([]metrics.Marker{{
			At: time.Date(2026, 10, 7, 18, 0, 0, 0, parisZone()), Kind: "config",
			Changes: []metrics.SettingChange{{Key: "recall.min_score", From: fp(0.5), To: fp(0.6)}},
		}}, r.Tables.Markers...)
		// one outside the days drawn is dropped, and with it nothing else
		r.Tables.Markers = append(r.Tables.Markers, metrics.Marker{At: time.Date(2026, 8, 1, 9, 0, 0, 0, parisZone()), Kind: "release", Version: "v0.0.1"})
	}
	card := usedCardOf(t, pageHTML(t, "releases", two))
	if card.first("div.card-head span.chip") == nil || lastOf(card.find("div.card-head span.chip")[:1]).text() != "this PC" {
		t.Errorf("the chart does not say it is this PC's")
	}
	var rel, cfg []string
	for _, m := range card.find("svg line.rel-mk title") {
		rel = append(rel, m.text())
	}
	for _, m := range card.find("svg line.cfg-mk title") {
		cfg = append(cfg, m.text())
	}
	if want := []string{"Oct 8: new version v0.1.18 (was v0.1.17)"}; !slices.Equal(rel, want) {
		t.Errorf("release markers %q, want %q", rel, want)
	}
	if want := []string{"Oct 7: settings changed, recorded at 18:00 (recall.min_score 0.5 → 0.6)"}; !slices.Equal(cfg, want) {
		t.Errorf("settings markers %q, want %q", cfg, want)
	}
	// each marker stands at its own day: the table's Marker column says which
	tbl := card.first("details.tblfb table")
	if tbl == nil {
		t.Fatal("no table behind the chart")
	}
	head := tbl.first("thead tr").find("th")
	if head[len(head)-1].text() != "Marker" {
		t.Fatalf("table head %q", needFirst(t, tbl, "thead tr").text())
	}
	rows := rowsOf(tbl)
	if len(rows) != len(days) {
		t.Fatalf("%d rows for %d days", len(rows), len(days))
	}
	for i, r := range rows {
		want := "—"
		switch i {
		case oct7:
			want = "settings changed"
		case oct8:
			want = "v0.1.18"
		}
		if got := r[len(r)-1]; got != want {
			t.Errorf("%s: marker %q, want %q", r[0], got, want)
		}
	}
	if !strings.Contains(needFirst(t, card, "div.legend").text(), "new version") || !strings.Contains(needFirst(t, card, "div.legend").text(), "settings changed") {
		t.Errorf("legend %q", needFirst(t, card, "div.legend").text())
	}
	// the line is the rate of used turns, with the faint bars of the turns
	var valued int
	for _, p := range g.Metrics["used.turn_rate"].Series {
		if p.Value != nil {
			valued++
		}
	}
	if valued == 0 {
		t.Fatal("the golden has no day with a rate")
	}
	if n := len(card.find("svg circle.pt.s-used")); n != valued {
		t.Errorf("%d points drawn for %d days with a rate", n, valued)
	}
	if n := len(card.find("svg rect.nb")); n != len(days) {
		t.Errorf("%d faint bars for %d days", n, len(days))
	}
	// without any marker, none is drawn and the column is gone
	bare := usedCardOf(t, pageHTML(t, "releases", func(d *PageData) { d.V["releases"].Tables.Markers = nil }))
	if len(bare.find("svg line.rel-mk"))+len(bare.find("svg line.cfg-mk")) != 0 || strings.Contains(bare.text(), "Marker") {
		t.Errorf("markers without any")
	}
	// no used turns in the view: a sentence, not an empty chart
	none := usedCardOf(t, pageHTML(t, "releases", func(d *PageData) { delete(d.V["releases"].Metrics, "used.turn_rate") }))
	if !strings.Contains(none.text(), "No days to draw in this window.") || none.first("svg polyline") != nil {
		t.Errorf("no series: %q", none.text())
	}
}
