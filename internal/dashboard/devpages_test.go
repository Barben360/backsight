package dashboard

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
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
	if len(ids) != len(metrics.Catalog) {
		t.Fatalf("/defs has %d rows with an id, the catalog %d", len(ids), len(metrics.Catalog))
	}
	for i, d := range metrics.Catalog {
		if ids[i] != d.ID {
			t.Errorf("row %d is %q, the catalog's is %q", i, ids[i], d.ID)
		}
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

func TestDefinitionRowsAreTheCatalog(t *testing.T) {
	h := dataHarness(t)
	res := apiPath(t, h, "/api/v1/defs")
	_, b := pageGet(t, h, h.login(), "/defs")
	doc := parseHTML(t, b)
	rows := doc.find("main tbody tr")
	if len(rows) != len(res.Tables.Defs) || len(rows) == 0 {
		t.Fatalf("%d rows, the API has %d", len(rows), len(res.Tables.Defs))
	}
	for i, tr := range rows {
		row := res.Tables.Defs[i]
		cells := tr.find("td")
		if len(cells) != 10 {
			t.Fatalf("%s: %d cells", row.ID, len(cells))
		}
		want := []string{row.ID, row.Label, row.Question, row.Definition, limitsText(row.Def, en), "", "", "", "", ""}
		if row.ID == "hooks.failures" {
			want[3] = hooksFailuresWhat
		}
		for _, c := range []int{0, 1, 2, 3, 4} {
			got := cells[c].text()
			if c == 0 {
				got = innerCode(cells[c])
			}
			if got != want[c] {
				t.Errorf("%s cell %d: %q, want %q", row.ID, c, got, want[c])
			}
		}
		// Rates carry their n and smallest change; the others dashes.
		if row.Unit == "ratio" {
			if cells[8].text() == dash {
				t.Errorf("%s: a rate without its n", row.ID)
			}
		} else if cells[8].text() != dash || cells[9].text() != dash {
			t.Errorf("%s: n %q, smallest %q, want dashes", row.ID, cells[8].text(), cells[9].text())
		}
		if row.MinN > 0 && cells[7].text() != en.Int(int64(row.MinN)) {
			t.Errorf("%s: fewest needed %q, want %d", row.ID, cells[7].text(), row.MinN)
		}
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

func TestDefinitionsPageIsPaged(t *testing.T) {
	h := dataHarness(t)
	_, b := pageGet(t, h, h.login(), "/defs?limit=5&offset=5")
	doc := parseHTML(t, b)
	ids := idsOf(doc)
	if len(ids) != 5 || ids[0] != metrics.Catalog[5].ID {
		t.Fatalf("page 2 of 5: %q", ids)
	}
	var next string
	if a := doc.first("main a[rel=next]"); a != nil {
		next = a.attr("href")
	}
	if !strings.Contains(next, "offset=10") || !strings.Contains(next, "limit=5") {
		t.Errorf("next link %q", next)
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
	seen := 0
	for _, p := range pages {
		_, body := pageGet(t, h, ck, p)
		for _, f := range fragmentsOf(t, body, "/releases") {
			seen++
			if !slices.Contains(ids, f) {
				t.Errorf("%s links /releases#%s, which has no row", p, f)
			}
		}
	}
	if seen < len(want) {
		t.Errorf("%d links to a version across the pages, want one per row at least", seen)
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
		t.Errorf("a version link lacks the window: %s", section(b, "Per version"))
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
		delete(row.Flagship, "verdict.wrong_rate")
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
	want := []string{"v0.1.18", "Oct 3 → Oct 21", "1,234", "7", "29 % (18–42 %)", "5 of 7", "—", "—", "—", "1.6 s", "alpha 38 %", "24.5", "1,284"}
	if !slices.Equal(got, want) {
		t.Errorf("cells\n got %q\nwant %q", got, want)
	}
	few := tr.find("td")[5].first("span")
	if few == nil || !strings.Contains(few.attr("class"), "st-few") {
		t.Errorf("a rate with too little behind it is not marked: %v", few)
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
