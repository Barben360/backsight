package dashboard

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

func texts(ns []*node) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.text())
	}
	return out
}

// TestOverviewScenarios runs the whole Overview template on each scenario
// and reads the page back: the verdict, the axes, the reasons, the six
// tiles, the attention list and the summary line are the ones the scenario
// wants, as a reader sees them.
func TestOverviewScenarios(t *testing.T) {
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			page := renderOverview(t, s.Result())
			v := page.first("section.verdict")
			if v == nil || !slices.Contains(strings.Fields(v.attr("class")), s.Want.Class) {
				t.Fatalf("verdict section %v, want class %s", v, s.Want.Class)
			}
			if h := v.first("h2").text(); !strings.HasPrefix(h, s.Want.Title) {
				t.Errorf("verdict title %q, want it to start %q", h, s.Want.Title)
			}
			var axes []string
			for _, a := range s.Want.Axes {
				axes = append(axes, strings.Join(a[:], " "))
			}
			if got := texts(page.find("li.axis")); !slices.Equal(got, axes) {
				t.Errorf("axes %q, want %q", got, axes)
			}
			if got := texts(page.find("li.reason p")); !slices.Equal(got, s.Want.Reasons) {
				t.Errorf("reasons %q, want %q", got, s.Want.Reasons)
			}
			tiles := page.find("article.tile")
			if len(tiles) != 6 {
				t.Fatalf("%d tiles, want 6", len(tiles))
			}
			var words []string
			for _, tile := range tiles {
				words = append(words, tile.first(".tile-st .st").text())
			}
			if !slices.Equal(words, s.Want.Tiles[:]) {
				t.Errorf("tile words %q, want %q", words, s.Want.Tiles)
			}
			if got := texts(page.find("li.att-item p")); !slices.Equal(got, s.Want.Attention) {
				t.Errorf("attention %q, want %q", got, s.Want.Attention)
			}
			if got := page.first("p.summary").text(); got != s.Want.Summary {
				t.Errorf("summary %q, want %q", got, s.Want.Summary)
			}
		})
	}
}

func glanceCard(t *testing.T, r *metrics.Result) *node {
	t.Helper()
	c := renderOverview(t, r).first("section.card[aria-labelledby=w-glance]")
	if c == nil {
		t.Fatal("no memory-size card")
	}
	return c
}

func num(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("%q is not a number", s)
	}
	return f
}

func TestGlance(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		r := scenarioNamed(t, "healthy")
		r.Info = []metrics.InfoItem{
			{Metric: "stale.never_used", Count: 7, Command: "backsight memory stale"},
			{Metric: "stale.hidden", Count: 3, Command: "backsight memory stale"},
			{Metric: "stale.reported", Count: 2, Command: "backsight memory stale"},
		}
		c := glanceCard(t, r)
		if got := c.first(".glance-total .v").text(); got != "1,284" {
			t.Errorf("total %q", got)
		}
		if got := c.first(".glance-total .sub").text(); got != "+46 in the last 7 days · 286 documents" {
			t.Errorf("growth %q", got)
		}
		rects := c.find("svg.kbar-svg rect")
		if len(rects) != 3 {
			t.Fatalf("%d rects, want 3", len(rects))
		}
		var sum, edge float64
		for i, class := range []string{"f-summary", "f-about", "f-session"} {
			if rects[i].attr("class") != class {
				t.Errorf("rect %d class %q, want %q", i, rects[i].attr("class"), class)
			}
			x, w := num(t, rects[i].attr("x")), num(t, rects[i].attr("width"))
			if w < 0 {
				t.Errorf("rect %d width %v", i, w)
			}
			if i > 0 && math.Abs(x-(edge+0.5)) > 0.011 {
				t.Errorf("rect %d starts at %v, want a 0.5 gap after %v", i, x, edge)
			}
			sum, edge = sum+w, x+w
		}
		if math.Abs(sum-99) > 0.03 {
			t.Errorf("widths sum to %v, want 100 minus two gaps of 0.5", sum)
		}
		if w := num(t, rects[0].attr("width")); math.Abs(w-99*212.0/1284) > 0.02 {
			t.Errorf("summaries are %v wide, want their share %v", w, 99*212.0/1284)
		}
		if got, want := c.first("svg.kbar-svg").attr("aria-label"), "Memories by kind: 212 summaries, 431 facts about you and your work, 641 facts from a session"; got != want {
			t.Errorf("aria-label %q, want %q", got, want)
		}
		want := []string{"summary 212", "fact about you and your work 431", "fact from a session 641"}
		if got := texts(c.find(".legend span")); !slices.Equal(got, want) {
			t.Errorf("legend %q, want %q", got, want)
		}
		badges := c.find("a.badge")
		if got, want := texts(badges), []string{"7 often shown, never used", "3 hidden", "2 reported wrong"}; !slices.Equal(got, want) {
			t.Errorf("badges %q, want %q", got, want)
		}
		for _, b := range badges {
			if b.attr("href") != "/bank#review" {
				t.Errorf("badge href %q", b.attr("href"))
			}
		}
		if strings.Contains(c.text(), "Nothing to review") {
			t.Error("a card with badges says there is nothing to review")
		}
		if c.first("div.state") != nil {
			t.Error("a server that answers shows the not-answering state")
		}
		pop := c.first("details.info .pop")
		if pop == nil || !strings.Contains(pop.text(), "The badges count memories worth a look; they never change the verdict.") {
			t.Fatalf("the info popover: %v", pop)
		}
		if got := pop.first("code.term").text(); got != "bank.size · bank.growth · stale.*" {
			t.Errorf("term %q", got)
		}
		if c.first(".chip-nf") != nil {
			t.Error("an unfiltered card carries the not-filtered chip")
		}
	})
	t.Run("serverdown", func(t *testing.T) {
		c := glanceCard(t, scenarioNamed(t, "serverdown"))
		st := c.first("div.state.st-watch")
		if st == nil || st.text() != "Memory server not answering since 09:12." {
			t.Fatalf("state %v", st)
		}
		if got := c.first(".glance-total .v").text(); got != "—" {
			t.Errorf("total %q, want a dash", got)
		}
		if c.first("svg.kbar-svg") != nil {
			t.Error("a bar is drawn without counts")
		}
		if strings.Contains(c.text(), "Nothing to review") {
			t.Error("an unreachable server says there is nothing to review")
		}
	})
	t.Run("new", func(t *testing.T) {
		c := glanceCard(t, scenarioNamed(t, "new"))
		if got := c.first(".glance-look .sub").text(); got != "Nothing to review." {
			t.Errorf("look %q", got)
		}
		if c.first("a.badge") != nil {
			t.Error("badges without any count")
		}
		rects := c.find("svg.kbar-svg rect")
		if len(rects) != 3 || num(t, rects[0].attr("width")) != 0 {
			t.Fatalf("rects %d, the empty summaries part must be 0 wide", len(rects))
		}
		if got := num(t, rects[1].attr("width")) + num(t, rects[2].attr("width")); math.Abs(got-99.5) > 0.02 {
			t.Errorf("two parts fill %v, want 99.5 (one gap)", got)
		}
		if got := c.first(".glance-total .sub").text(); got != "+38 in the last 7 days · 4 documents" {
			t.Errorf("growth %q", got)
		}
	})
	t.Run("one", func(t *testing.T) {
		r := scenarioNamed(t, "healthy")
		r.Metrics["bank.size"] = scBank(1, 2, 3, 0, [3]int64{})[0].m
		g := glanceOf(r, en, r.Window.Loc, "")
		if want := "Memories by kind: 1 summary, 2 facts about you and your work, 3 facts from a session"; g.Aria != want {
			t.Errorf("aria %q, want %q", g.Aria, want)
		}
		if strings.Contains(g.Growth, "documents") {
			t.Errorf("no documents, yet %q", g.Growth)
		}
	})
	t.Run("filtered", func(t *testing.T) {
		r := scenarioNamed(t, "healthy")
		g := glanceOf(r, en, r.Window.Loc, "my project")
		if !g.Head.NotFiltered || g.Head.CLI != "backsight stats bank --project 'my project'" {
			t.Errorf("filtered head: %v %q", g.Head.NotFiltered, g.Head.CLI)
		}
		if g := glanceOf(r, en, r.Window.Loc, ""); g.Head.NotFiltered || g.Head.CLI != "backsight stats bank" {
			t.Errorf("unfiltered head: %v %q", g.Head.NotFiltered, g.Head.CLI)
		}
		g = glancePage(r, en, r.Window.Loc, views.Params{Days: 30, DaysSet: true})
		if g.Head.CLI != "backsight stats bank --days 30" {
			t.Errorf("page flags: %q", g.Head.CLI)
		}
	})
}

func TestBarShares(t *testing.T) {
	xs, ws := barOf([]int64{1, 0, 3})
	if ws[1] != 0 || ws[0] != 24.88 || ws[2] != 74.63 || xs[2] != 25.38 || xs[1] != 25.38 {
		t.Errorf("xs %v ws %v: want 1 and 3 sharing 99.5 around one gap", xs, ws)
	}
	if xs, ws = barOf([]int64{0, 0, 0}); ws[0]+ws[1]+ws[2] != 0 || xs[2] != 0 {
		t.Errorf("an empty bar draws %v %v", xs, ws)
	}
	if _, ws = barOf([]int64{1, 1, 1}); math.Abs(ws[0]+ws[1]+ws[2]-99) > 0.02 {
		t.Errorf("three parts fill %v", ws)
	}
}

func TestLatelyLinks(t *testing.T) {
	r := scenarioNamed(t, "healthy")
	r.Tables.Lately[0].ID = "ab/c d#e12345678"
	r.Tables.Lately[1].Project = "<img src=x onerror=a>"
	page := renderOverview(t, r)
	card := page.first("section.card[aria-labelledby=w-lately]")
	if card == nil {
		t.Fatal("no Lately card")
	}
	ids := card.find("a.idc")
	if len(ids) != 5 {
		t.Fatalf("%d session links, want 5", len(ids))
	}
	for i, a := range ids {
		row := r.Tables.Lately[i]
		if got, want := a.attr("href"), "/sessions/"+strings.NewReplacer("/", "%2F", " ", "%20", "#", "%23").Replace(row.ID); got != want {
			t.Errorf("row %d href %q, want %q", i, got, want)
		}
		if a.attr("title") != row.ID || a.text() != row.ID[:8] {
			t.Errorf("row %d: text %q title %q, want %q and %q", i, a.text(), a.attr("title"), row.ID[:8], row.ID)
		}
	}
	long := card.find("a.trunc")[4]
	if long.text() != "project-d-shared-fronte…" || long.attr("title") != "project-d-shared-frontend-tools" ||
		long.attr("href") != "/projects/project-d-shared-frontend-tools" {
		t.Errorf("long project: %q %q %q", long.text(), long.attr("title"), long.attr("href"))
	}
	if n := len([]rune(long.text())); n != 24 {
		t.Errorf("clipped to %d characters", n)
	}
	short := card.find("a.trunc")[0]
	if short.text() != "backsight" || short.attr("title") != "backsight" {
		t.Errorf("short project %q %q", short.text(), short.attr("title"))
	}
	hostile := card.find("a.trunc")[1]
	if hostile.text() != "<img src=x onerror=a>" || len(card.find("img")) != 0 {
		t.Errorf("a project name is not escaped: %q", hostile.text())
	}
	if hostile.attr("href") != "/projects/%3Cimg%20src=x%20onerror=a%3E" {
		t.Errorf("hostile href %q", hostile.attr("href"))
	}
	// When: told against the page's time, with the exact instant in the title.
	gen := r.GeneratedAt
	for i, tm := range card.find("td time") {
		want := format.Rel(gen, r.Tables.Lately[i].Last, r.Window.Loc)
		if tm.text() != want.Text || tm.attr("datetime") != want.Datetime || tm.attr("title") != want.Title {
			t.Errorf("row %d time %q %q %q, want %+v", i, tm.text(), tm.attr("datetime"), tm.attr("title"), want)
		}
	}
	if got := card.first("td time").text(); got != "25 min ago" {
		t.Errorf("first row time %q", got)
	}
	// Wrong shows as a watch only above zero.
	if marks := card.find("td .st-watch"); len(marks) != 1 || marks[0].text() != "1" {
		t.Errorf("wrong marks %v, want one reading 1", texts(marks))
	}
	var shown []string
	for _, row := range card.find("tbody tr") {
		cells := row.find("td.r")
		shown = append(shown, strings.Join(texts(cells), "/"))
	}
	if want := []string{"10/2/1", "4/1/0", "9/3/0", "3/0/0", "5/1/0"}; !slices.Equal(shown, want) {
		t.Errorf("shown/used/wrong %q, want %q", shown, want)
	}
	if f := card.first(".card-foot a"); f == nil || f.attr("href") != "/sessions" || f.text() != "All sessions →" {
		t.Errorf("foot link %v", f)
	}
	if got := texts(card.find("thead th")); !slices.Equal(got, []string{"Session", "Project", "When", "Shown", "Used", "Wrong"}) {
		t.Errorf("headings %q", got)
	}
	if c := card.first("details.cli code"); c == nil || c.text() != "backsight stats sessions --limit 5" {
		t.Errorf("CLI %v", c)
	}

	// No row: no table, and a sentence.
	r.Tables.Lately = nil
	card = renderOverview(t, r).first("section.card[aria-labelledby=w-lately]")
	if card.first("table") != nil || !strings.Contains(card.text(), "No sessions in this window.") {
		t.Errorf("empty card: %q", card.text())
	}
}
