package dashboard

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// hostile is a name that breaks any page that does not escape what it
// writes, in text, in an attribute and in a path.
const hostile = `<b>x</b>&"'`

// hostilePath is hostile as one path segment.
const hostilePath = `%3Cb%3Ex%3C%2Fb%3E&%22%27`

// pageHTML is pageNode's page, unparsed.
func pageHTML(t *testing.T, name string, edit func(d *PageData)) string {
	t.Helper()
	return render(t, name, func(d *PageData) {
		d.L, d.P = en, views.Params{Days: 30, DaysSet: true}
		d.CLI = "backsight stats " + name + " --days 30"
		d.Frame.Window = "last 30 days"
		d.Frame.Prefs.Dev = false
		if edit != nil {
			edit(d)
		}
	})
}

func figsOf(n *node) [][3]string {
	var out [][3]string
	for _, f := range n.find("div.fig") {
		sub := ""
		if s := f.first("span.s"); s != nil {
			sub = s.text()
		}
		out = append(out, [3]string{f.first("span.lab").text(), f.first("span.v").text(), sub})
	}
	return out
}

func mainOf(t *testing.T, n *node) *node {
	t.Helper()
	m := n.first("main")
	if m == nil {
		t.Fatal("no main")
	}
	return m
}

// richBank gives every bank metric a number: 1,284 memories (212 summaries,
// 431 facts about you, 641 facts from sessions) of which 186 were added
// over six days, three projects, four summaries waiting, three weeks of
// churn and a review queue.
func richBank(r *metrics.Result) {
	r.Window.Days = 30
	give(r, "bank.size", func(m *metrics.Metric) {
		m.Value = fp(1284)
		m.Extra = map[string]any{"world": int64(431), "experience": int64(641), "observation": int64(212), "documents": int64(286), "links": int64(9)}
	})
	give(r, "bank.growth", func(m *metrics.Metric) {
		m.Value = fp(31)
		m.Extra = map[string]any{"world": int64(64), "experience": int64(101), "observation": int64(21)}
		m.Series = nil
		for i := range 6 {
			m.Series = append(m.Series, metrics.Point{Day: fmt.Sprintf("2026-10-%02d", 5+i), Value: fp(1),
				Parts: map[string]int64{"world": int64(i + 1), "experience": int64(2 * i), "observation": 1}})
		}
	})
	give(r, "bank.by_project", func(m *metrics.Metric) { m.Value = fp(3) })
	r.Tables.ByProject = []metrics.ProjectCount{{Project: "backsight", Memories: 512}, {Project: "project-a", Memories: 301}, {Project: "project-b", Memories: 214}}
	give(r, "bank.consolidation", func(m *metrics.Metric) {
		m.Value = fp(4)
		m.Extra = map[string]any{"pending": int64(4), "failed": int64(0), "last_consolidated_at": r.GeneratedAt.Add(-35 * time.Minute)}
	})
	give(r, "notes.stored", func(m *metrics.Metric) {
		m.Value = fp(31)
		m.Extra = map[string]any{"by_result": map[string]int64{"saved": 25, "updated": 6, "refused": 1},
			"by_kind": map[string]int64{"decision": 14, "state": 6, "convention": 6, "pitfall": 5}}
	})
	give(r, "bank.churn", func(m *metrics.Metric) {
		m.Value = fp(9)
		m.Extra = map[string]any{"forget": int64(2), "retire": int64(3), "hide": int64(3), "unhide": int64(1)}
		m.Series = []metrics.Point{
			{Week: "2026-W41", Value: fp(2), Parts: map[string]int64{"forget": 2}},
			{Week: "2026-W42", Value: fp(4), Parts: map[string]int64{"retire": 3, "unhide": 1}},
			{Week: "2026-W43", Value: fp(3), Parts: map[string]int64{"hide": 3}}}
	})
	give(r, "stale.never_used", func(m *metrics.Metric) { m.Value, m.Tags = fp(7), []string{"not_filtered"} })
	give(r, "stale.hidden", func(m *metrics.Metric) { m.Value, m.Tags = fp(3), []string{"not_filtered"} })
	give(r, "stale.reported", func(m *metrics.Metric) { m.Value, m.Tags = fp(2), []string{"not_filtered"} })
	at := r.GeneratedAt.Add(-5 * time.Hour)
	r.Tables.Review = &metrics.Review{
		NeverUsed: []metrics.ReviewRow{{ID: "b81e44c0d93a4f7e8c2a61d05f9e3b72", Project: "backsight", Shown: 14, LastShown: &at, Command: "backsight forget b81e44c0d93a4f7e8c2a61d05f9e3b72"},
			{ID: "3d9f0a27e6c14b58a9d0e3f17c2b6a84", Project: "", Shown: 9, Command: "backsight forget 3d9f0a27e6c14b58a9d0e3f17c2b6a84"}},
		Hidden:   []metrics.ReviewRow{{ID: "c4a17e93b0d24f6a8e1c95d3072b4f18", Project: "project-a", Shown: 5, LastShown: &at, Command: "backsight forget --restore c4a17e93b0d24f6a8e1c95d3072b4f18"}},
		Reported: []metrics.ReviewRow{{ID: "aa11bb22cc33dd44", Project: "project-b", Shown: 2, LastShown: &at, Command: "backsight forget aa11bb22cc33dd44 --purge"}},
	}
}

func TestBankCards(t *testing.T) {
	n := pageNode(t, "bank", false, func(d *PageData) { richBank(d.V["bank"]) })
	want := []string{"w-bank-size", "w-bank-growth", "w-bank-by_project", "w-bank-consolidation", "w-notes-stored", "w-bank-churn", "w-review"}
	if got := cardIDs(n); !slices.Equal(got, want) {
		t.Fatalf("cards %v, want %v", got, want)
	}
	t.Run("the strip of figures comes first", func(t *testing.T) {
		m := mainOf(t, n)
		strips := m.find("section[aria-labelledby=w-bank-size] div.figs")
		if len(strips) != 1 || m.first("div.figs") != strips[0] {
			t.Fatalf("%d strips, the first of the page is %v", len(strips), m.first("div.figs"))
		}
		strip := strips[0]
		got := figsOf(strip)
		w := [][3]string{{"Memories", "1,284", "+186 in the last 30 days"}, {"summary", "212", "+21"}, {"fact about you and your work", "431", "+64"},
			{"fact from a session", "641", "+101"}, {"Documents", "286", "sessions and notes sent"}}
		if !slices.Equal(got, w) {
			t.Errorf("strip %q, want %q", got, w)
		}
		for _, f := range strip.find("div.fig") {
			if f.first("span.st") != nil {
				t.Error("a status word in a figure the canvas draws without one")
			}
		}
		if strip.find("div.fig")[0].first("span.i-server") == nil || strip.find("div.fig")[4].first("span.i-doc") == nil {
			t.Error("the icons of the memories and the documents are missing")
		}
		for i, class := range []string{"f-summary", "f-about", "f-session"} {
			if f := strip.find("div.fig")[i+1]; f.first("i.sw."+class) == nil {
				t.Errorf("figure %d lacks its swatch %s", i+1, class)
			}
		}
		// The question of the strip is its accessible name, before the cards.
		if h := m.first("h2#w-bank-size"); h == nil || h.text() != "How many memories are stored?" || !h.has("class") {
			t.Errorf("strip heading %+v", h)
		}
		if st := strip.Parent; st.Name != "section" || st.attr("aria-labelledby") != "w-bank-size" {
			t.Errorf("strip section %s %v", st.Name, st.Attr)
		}
	})
	t.Run("the layout is the canvas's", func(t *testing.T) {
		pair := func(id string) string { return cardOf(t, n, id).Parent.attr("class") }
		for _, id := range []string{"w-bank-growth", "w-bank-by_project"} {
			if pair(id) != "grid-2" {
				t.Errorf("%s sits in %q", id, pair(id))
			}
		}
		if cardOf(t, n, "w-bank-growth").Parent != cardOf(t, n, "w-bank-by_project").Parent {
			t.Error("growth and projects are not side by side")
		}
		for _, id := range []string{"w-bank-consolidation", "w-notes-stored", "w-bank-churn"} {
			if pair(id) != "grid-3" || cardOf(t, n, id).Parent != cardOf(t, n, "w-bank-churn").Parent {
				t.Errorf("%s sits in %q", id, pair(id))
			}
		}
		if cardOf(t, n, "w-review").Parent.Name != "main" {
			t.Error("the review queue is not a card of the page")
		}
	})
	t.Run("growth", func(t *testing.T) {
		c := cardOf(t, n, "w-bank-growth")
		if got := len(c.find("rect.hit")); got != 6 {
			t.Errorf("%d hit rects, want one per day (6)", got)
		}
		var legend []string
		for _, s := range c.find("figure.chart .legend span") {
			legend = append(legend, s.text())
		}
		if w := []string{"summary", "fact about you and your work", "fact from a session"}; !slices.Equal(legend, w) {
			t.Errorf("legend %v, want %v", legend, w)
		}
		for _, class := range []string{"f-summary", "f-about", "f-session"} {
			if len(c.find("path.seg."+class)) != 1 {
				t.Errorf("no area of %s", class)
			}
		}
		if got := c.first("rect.hit title").text(); got != "Oct 5 · summary: 1, fact about you and your work: 1" {
			t.Errorf("first day %q", got)
		}
		if !strings.Contains(c.text(), "added over the period: 64 facts about you and your work, 101 facts from sessions, 21 summaries") {
			t.Errorf("the per-kind line: %q", c.text())
		}
		if lead := c.first("div.row.lead"); lead == nil || !strings.HasPrefix(lead.text(), "31 ") {
			t.Errorf("lead %v", lead)
		}
	})
	t.Run("tabs", func(t *testing.T) {
		c := cardOf(t, n, "w-review")
		tabs := c.find("details.tabs")
		if len(tabs) != 3 {
			t.Fatalf("%d tabs", len(tabs))
		}
		for i, tab := range tabs {
			if tab.has("open") != (i == 0) {
				t.Errorf("tab %d open: %v", i, tab.has("open"))
			}
			if tab.attr("name") != "review" {
				t.Errorf("tab %d is not in the one group: %v", i, tab.Attr)
			}
		}
		if s := n.first("section#review"); s == nil || s != c {
			t.Error("the anchor is not the review card's section")
		}
	})
}

func TestBankSizeFromTheStripCarriesNoGrowthWithoutIt(t *testing.T) {
	n := pageNode(t, "bank", false, func(d *PageData) {
		richBank(d.V["bank"])
		give(d.V["bank"], "bank.growth", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonNoEvents, nil
		})
	})
	got := figsOf(mainOf(t, n).first("div.figs"))
	if got[0] != [3]string{"Memories", "1,284", ""} || got[1][2] != "" {
		t.Errorf("the strip claims growth the server did not report: %q", got)
	}
	// The server keeps only so many days: the figure says over how many.
	n = pageNode(t, "bank", false, func(d *PageData) {
		richBank(d.V["bank"])
		give(d.V["bank"], "bank.growth", func(m *metrics.Metric) {
			m.Value = fp(2)
			m.Extra = map[string]any{"world": int64(2), "experience": int64(3), "observation": int64(1), "days_covered": 14}
		})
	})
	if got := figsOf(mainOf(t, n).first("div.figs")); got[0][2] != "+6 in the last 14 days" {
		t.Errorf("covered days: %q", got[0])
	}
}

func TestBankUnderAProjectFilter(t *testing.T) {
	// The server counts memories of a project in one figure: no split by kind, no documents.
	n := pageNode(t, "bank", false, func(d *PageData) {
		richBank(d.V["bank"])
		give(d.V["bank"], "bank.size", func(m *metrics.Metric) {
			m.Value = fp(40)
			m.Extra = map[string]any{"world": nil, "experience": nil, "observation": nil, "documents": nil, "links": nil}
		})
	})
	if got := figsOf(mainOf(t, n).first("div.figs")); len(got) != 1 || got[0][1] != "40" {
		t.Errorf("filtered strip %q", got)
	}
}

func TestProjectBarWidths(t *testing.T) {
	bars := projectBars(en, []metrics.ProjectCount{{Project: "a", Memories: 40}, {Project: "b", Memories: 20}, {Project: "c", Memories: 10}})
	if len(bars) != 3 || bars[0].W != 100 || bars[1].W != 50 || bars[2].W != 25 {
		t.Fatalf("bars %+v", bars)
	}
	n := pageNode(t, "bank", false, func(d *PageData) {
		richBank(d.V["bank"])
		d.V["bank"].Tables.ByProject = []metrics.ProjectCount{{Project: "a", Memories: 40}, {Project: "b", Memories: 20}, {Project: "c", Memories: 10}}
	})
	var widths, labels []string
	for _, r := range cardOf(t, n, "w-bank-by_project").find("tbody tr") {
		svg := r.first("svg")
		widths = append(widths, svg.first("rect").attr("width"))
		labels = append(labels, svg.attr("aria-label"))
		if svg.attr("viewbox") != "0 0 100 10" || svg.attr("preserveaspectratio") != "none" || svg.attr("role") != "img" {
			t.Errorf("bar svg %v", svg.Attr)
		}
	}
	if w := []string{"100.0", "50.0", "25.0"}; !slices.Equal(widths, w) {
		t.Errorf("widths %v, want %v", widths, w)
	}
	if w := []string{"40 memories", "20 memories", "10 memories"}; !slices.Equal(labels, w) {
		t.Errorf("labels %v, want %v", labels, w)
	}
	one := pageNode(t, "bank", false, func(d *PageData) {
		richBank(d.V["bank"])
		d.V["bank"].Tables.ByProject = []metrics.ProjectCount{{Project: "a", Memories: 1}}
	})
	if label := cardOf(t, one, "w-bank-by_project").first("tbody svg").attr("aria-label"); label != "1 memory" {
		t.Errorf("one memory is labelled %q", label)
	}
	// An empty list and a project with no memory do not divide by zero.
	if got := projectBars(en, nil); len(got) != 0 {
		t.Errorf("bars of nothing: %+v", got)
	}
	if got := projectBars(en, []metrics.ProjectCount{{Project: "z"}}); len(got) != 1 || got[0].W != 0 {
		t.Errorf("bar of no memory: %+v", got)
	}
}

func TestProjectBarsEscapeNames(t *testing.T) {
	page := pageHTML(t, "bank", func(d *PageData) {
		richBank(d.V["bank"])
		d.V["bank"].Tables.ByProject = []metrics.ProjectCount{{Project: hostile, Memories: 3}}
	})
	if strings.Contains(page, "<b>x") {
		t.Fatalf("a raw tag from a project name reached the page")
	}
	c := cardOf(t, parseHTML(t, page), "w-bank-by_project")
	a := c.first("tbody a")
	if a == nil || a.text() != hostile || a.attr("href") != "/projects/"+hostilePath || a.attr("title") != hostile {
		t.Errorf("project link %+v", a)
	}
	if label := c.first("tbody svg").attr("aria-label"); label != "3 memories" {
		t.Errorf("bar label %q", label)
	}
	// A long name is clipped in the cell and whole in the title.
	long := strings.Repeat("p", 30)
	bars := projectBars(en, []metrics.ProjectCount{{Project: long, Memories: 1}})
	if bars[0].Name == long || !strings.HasSuffix(bars[0].Name, "…") || bars[0].Full != long {
		t.Errorf("clip %+v", bars[0])
	}
}

func TestProjectsCardIsCappedAndSaysWhyItIsEmpty(t *testing.T) {
	n := pageNode(t, "bank", false, func(d *PageData) {
		richBank(d.V["bank"])
		d.V["bank"].Tables.ByProject = nil
		for i := range 130 {
			d.V["bank"].Tables.ByProject = append(d.V["bank"].Tables.ByProject, metrics.ProjectCount{Project: fmt.Sprintf("p%03d", i), Memories: int64(200 - i)})
		}
	})
	c := cardOf(t, n, "w-bank-by_project")
	if got := len(c.find("tbody tr")); got != 120 {
		t.Errorf("%d rows, want the cap (120)", got)
	}
	if more := c.first("p.tbl-more"); more == nil || !strings.Contains(more.text(), "Showing 120 of 130 projects") || !hasCode(more, "backsight stats bank --days 30 --json") {
		t.Errorf("cap note %v", more)
	}
	// With the golden's value of 0 projects: a sentence, no table.
	g := pageNode(t, "bank", false, nil)
	c = cardOf(t, g, "w-bank-by_project")
	if c.first("table") != nil || !strings.Contains(c.first("div.empty").text(), "No memory carries a project yet") {
		t.Errorf("empty projects: %q", c.text())
	}
	// Without a number, the reason: never "nothing yet".
	down := pageNode(t, "bank", false, func(d *PageData) {
		give(d.V["bank"], "bank.by_project", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonServerDown, nil
		})
	})
	if e := cardOf(t, down, "w-bank-by_project").first("div.empty"); e == nil || !strings.HasPrefix(e.text(), "Memory server not answering") {
		t.Errorf("down: %v", e)
	}
}

func TestConsolidationCard(t *testing.T) {
	n := pageNode(t, "bank", false, func(d *PageData) { richBank(d.V["bank"]) })
	c := cardOf(t, n, "w-bank-consolidation")
	var dl []string
	for _, e := range c.find("dl.dl dt") {
		dl = append(dl, e.text())
	}
	var dd []string
	for _, e := range c.find("dl.dl dd") {
		dd = append(dd, e.text())
	}
	if w := []string{"Waiting", "Failed", "Last built"}; !slices.Equal(dl, w) {
		t.Errorf("rows %v, want %v", dl, w)
	}
	if w := []string{"4 memories", "0", "35 min ago"}; !slices.Equal(dd, w) {
		t.Errorf("values %v, want %v", dd, w)
	}
	if tm := c.first("dl.dl time"); tm == nil || tm.attr("datetime") == "" || tm.attr("title") == "" {
		t.Errorf("last built time %+v", tm)
	}
	if pill := c.first("span.pill"); pill == nil || pill.text() != "ok" || !strings.Contains(pill.attr("class"), "st-ok") {
		t.Errorf("pill %+v", pill)
	}
	if hasCode(c, "backsight doctor") {
		t.Error("advice with nothing failed")
	}
	// Failed summaries: the watch word and the command that helps; never built: no time.
	bad := pageNode(t, "bank", false, func(d *PageData) {
		richBank(d.V["bank"])
		give(d.V["bank"], "bank.consolidation", func(m *metrics.Metric) {
			m.Status, m.Value = metrics.StatusWatch, fp(1)
			m.Extra = map[string]any{"pending": int64(1), "failed": int64(2), "last_consolidated_at": nil}
		})
	})
	c = cardOf(t, bad, "w-bank-consolidation")
	var dd2 []string
	for _, e := range c.find("dl.dl dd") {
		dd2 = append(dd2, e.text())
	}
	if w := []string{"1 memory", "2", "not yet"}; !slices.Equal(dd2, w) {
		t.Errorf("values %v, want %v", dd2, w)
	}
	if pill := c.first("span.pill"); pill == nil || pill.text() != "watch" || !strings.Contains(pill.attr("class"), "st-watch") {
		t.Errorf("pill %+v", pill)
	}
	if !hasCode(c, "backsight doctor") {
		t.Error("no command for failed summaries")
	}
}

func TestNotesCard(t *testing.T) {
	render1 := func() *node {
		return pageNode(t, "bank", false, func(d *PageData) { richBank(d.V["bank"]) })
	}
	c := cardOf(t, render1(), "w-notes-stored")
	got := figsOf(c)
	if w := [][3]string{{"saved", "25", ""}, {"updated", "6", ""}, {"refused", "1", ""}}; !slices.Equal(got, w) {
		t.Errorf("figures %q, want %q", got, w)
	}
	// By kind, most first, a tie by name: stable whatever the map order.
	for range 20 {
		if txt := cardOf(t, render1(), "w-notes-stored").text(); !strings.Contains(txt, "by kind: decision 14, convention 6, state 6, pitfall 5") {
			t.Fatalf("kind order: %q", txt)
		}
	}
	if lead := c.first("div.row.lead"); lead == nil || !strings.HasPrefix(lead.text(), "31 ") {
		t.Errorf("lead %v", lead)
	}
	// No number: the reason.
	g := pageNode(t, "bank", false, nil)
	n := cardOf(t, g, "w-notes-stored")
	if e := n.first("div.empty"); e == nil || !strings.Contains(e.text(), "Nothing recorded in the last 30 days.") || n.first("div.fig") != nil {
		t.Errorf("no data: %q", n.text())
	}
}

func TestChurnCard(t *testing.T) {
	n := pageNode(t, "bank", false, func(d *PageData) { richBank(d.V["bank"]) })
	c := cardOf(t, n, "w-bank-churn")
	var legend []string
	for _, s := range c.find("figure.chart .legend span") {
		legend = append(legend, s.text())
	}
	if w := []string{"forgotten", "retired", "hidden", "restored"}; !slices.Equal(legend, w) {
		t.Errorf("legend %v, want %v", legend, w)
	}
	if got := len(c.find("rect.hit")); got != 3 {
		t.Errorf("%d weeks drawn, want 3", got)
	}
	if got := c.first("rect.hit title").text(); got != "2026-W41 · forgotten: 2" {
		t.Errorf("first week %q", got)
	}
	if th := c.find("details.tblfb th"); len(th) == 0 || th[0].text() != "Week" {
		t.Errorf("table head %v", th)
	}
	spoken := spokenText(pageHTML(t, "bank", func(d *PageData) { richBank(d.V["bank"]) }))
	for _, raw := range []string{"unhide", "retire ", "forget "} {
		if strings.Contains(spoken, raw) {
			t.Errorf("the stored name %q reached the page", raw)
		}
	}
	// A reason, never "nothing was retired": no events is a sentence naming the window.
	g := pageNode(t, "bank", false, nil)
	if e := cardOf(t, g, "w-bank-churn").first("div.empty"); e == nil || !strings.Contains(e.text(), "Nothing recorded in the last 30 days.") {
		t.Errorf("empty churn: %v", e)
	}
	off := pageNode(t, "bank", false, func(d *PageData) {
		give(d.V["bank"], "bank.churn", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonFeedbackOff, nil
		})
	})
	if e := cardOf(t, off, "w-bank-churn").first("div.empty"); e == nil || !strings.Contains(e.text(), "Memory feedback is off") {
		t.Errorf("feedback off: %v", e)
	}
}

func TestReviewTabs(t *testing.T) {
	n := pageNode(t, "bank", true, func(d *PageData) { richBank(d.V["bank"]) })
	c := cardOf(t, n, "w-review")
	tabs := c.find("details.tabs")
	var heads []string
	for _, tab := range tabs {
		heads = append(heads, tab.first("summary").text())
	}
	if w := []string{"Often shown, never used 7", "Hidden 3", "Reported wrong 2"}; !slices.Equal(heads, w) {
		t.Errorf("tab heads %q, want %q", heads, w)
	}
	// Each tab asks the catalog's own question.
	for i, q := range []string{"Which memories are shown again and again for nothing?", "How many memories are hidden from recall?", "How many memories were reported wrong?"} {
		if !strings.Contains(tabs[i].text(), q) {
			t.Errorf("tab %d lacks %q", i, q)
		}
	}
	rows := tabs[0].find("tbody tr")
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	cells := rows[0].find("td")
	if a := cells[0].first("a.idc"); a == nil || a.text() != "b81e44c0" || a.attr("href") != "/memories/b81e44c0d93a4f7e8c2a61d05f9e3b72" || a.attr("title") != "b81e44c0d93a4f7e8c2a61d05f9e3b72" {
		t.Errorf("id chip %+v", a)
	}
	if cells[1].text() != "backsight" || cells[2].text() != "14" || !strings.Contains(cells[2].attr("class"), "r") {
		t.Errorf("project %q shown %q (%v)", cells[1].text(), cells[2].text(), cells[2].Attr)
	}
	if tm := cells[3].first("time"); tm == nil || tm.text() != "5 h ago" || tm.attr("datetime") == "" {
		t.Errorf("last shown %+v", tm)
	}
	if !hasCode(cells[4], "backsight forget b81e44c0d93a4f7e8c2a61d05f9e3b72") || cells[4].first("div.cmd button.copy") == nil {
		t.Errorf("command cell %q", cells[4].text())
	}
	// A memory never shown, with no project: dashes, not an empty link.
	cells = rows[1].find("td")
	if cells[1].text() != "—" || cells[3].text() != "—" || cells[1].first("a") != nil {
		t.Errorf("empty cells %q %q", cells[1].text(), cells[3].text())
	}
	if got := hasCode(tabs[1], "backsight forget --restore c4a17e93b0d24f6a8e1c95d3072b4f18"); !got {
		t.Error("the hidden tab lacks the restore command")
	}
	if got := hasCode(tabs[2], "backsight forget aa11bb22cc33dd44 --purge"); !got {
		t.Error("the reported tab lacks the purge command")
	}
	// The ids of the three counts in developer mode only.
	for _, id := range []string{"stale.never_used", "stale.hidden", "stale.reported"} {
		if !hasCode(c, id) {
			t.Errorf("developer mode lacks %s", id)
		}
	}
	plain := cardOf(t, pageNode(t, "bank", false, func(d *PageData) { richBank(d.V["bank"]) }), "w-review")
	for _, id := range []string{"stale.never_used", "stale.hidden", "stale.reported"} {
		if hasCode(plain, id) {
			t.Errorf("%s shows without developer mode", id)
		}
	}
	if !strings.Contains(plain.text(), "Shown: recent-weighted counts.") {
		t.Errorf("no note on the counts: %q", plain.text())
	}
	if chip := plain.first("span.chip-nf"); chip == nil {
		t.Error("the queue is this PC's whatever the filter: no `not filtered` chip")
	}
}

func TestReviewTabsCapAndSayWhyTheyAreEmpty(t *testing.T) {
	n := pageNode(t, "bank", false, func(d *PageData) {
		richBank(d.V["bank"])
		give(d.V["bank"], "stale.never_used", func(m *metrics.Metric) { m.Value = fp(60) })
		give(d.V["bank"], "stale.hidden", func(m *metrics.Metric) { m.Value = fp(0) })
		give(d.V["bank"], "stale.reported", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonFeedbackOff, nil
		})
		d.V["bank"].Tables.Review.Hidden, d.V["bank"].Tables.Review.Reported = nil, nil
	})
	tabs := cardOf(t, n, "w-review").find("details.tabs")
	if !strings.Contains(tabs[0].text(), "Showing 2 of 60 memories") || !hasCode(tabs[0], "backsight memory stale") {
		t.Errorf("capped tab: %q", tabs[0].text())
	}
	if !strings.Contains(tabs[1].text(), "No memory is hidden") || tabs[1].first("table") != nil {
		t.Errorf("empty tab: %q", tabs[1].text())
	}
	if e := tabs[2].first("div.empty"); e == nil || !strings.Contains(e.text(), "Memory feedback is off") || tabs[2].first("table") != nil {
		t.Errorf("no-data tab: %q", tabs[2].text())
	}
	// No review lists at all (the view returned none): no rows, no crash.
	m := pageNode(t, "bank", false, func(d *PageData) { richBank(d.V["bank"]); d.V["bank"].Tables.Review = nil })
	if len(cardOf(t, m, "w-review").find("tbody tr")) != 0 {
		t.Error("rows without a review")
	}
}

func TestReviewHostileValues(t *testing.T) {
	page := pageHTML(t, "bank", func(d *PageData) {
		richBank(d.V["bank"])
		d.V["bank"].Tables.Review.NeverUsed = []metrics.ReviewRow{{ID: "m/../x y" + hostile, Project: hostile, Shown: 1, Command: "backsight forget 'm'"}}
	})
	if strings.Contains(page, "<b>x") {
		t.Fatal("a raw tag reached the page")
	}
	row := cardOf(t, parseHTML(t, page), "w-review").first("tbody tr")
	cells := row.find("td")
	if a := cells[0].first("a"); a == nil || a.attr("href") != "/memories/m%2F..%2Fx%20y%3Cb%3Ex%3C%2Fb%3E&%22%27" {
		t.Errorf("memory link %+v", a)
	}
	if a := cells[1].first("a"); a == nil || a.attr("href") != "/projects/"+hostilePath || a.text() != hostile {
		t.Errorf("project link %+v", a)
	}
}

func TestBankSaysWhenTheServerIsDown(t *testing.T) {
	down := func(id string) func(m *metrics.Metric) {
		return func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonServerDown, nil
		}
	}
	n := pageNode(t, "bank", false, func(d *PageData) {
		richBank(d.V["bank"])
		for _, id := range []string{"bank.size", "bank.growth", "bank.by_project", "bank.consolidation"} {
			give(d.V["bank"], id, down(id))
		}
	})
	for _, id := range []string{"w-bank-growth", "w-bank-by_project", "w-bank-consolidation"} {
		c := cardOf(t, n, id)
		e := c.first("div.empty")
		if e == nil || !strings.HasPrefix(e.text(), "Memory server not answering") || c.first("figure.chart") != nil || c.first("dl.dl") != nil || c.first("table") != nil {
			t.Errorf("%s: %q", id, c.text())
		}
	}
	// The strip is one figure with the reason where the number would be.
	got := figsOf(mainOf(t, n).first("div.figs"))
	if len(got) != 1 || got[0][0] != "Memories" || got[0][1] != "—" || !strings.HasPrefix(got[0][2], "Memory server not answering") {
		t.Errorf("strip %q", got)
	}
	// The cards of this PC still show their numbers.
	if cardOf(t, n, "w-notes-stored").first("div.fig") == nil {
		t.Error("a PC's own card hides its numbers because the server is down")
	}
}

// richPipeline gives the pipeline a broken day: 188 of 192 sessions sent,
// 3 failed, 1 set aside, a backlog 26 h old, a failed server task and two
// crashed steps.
func richPipeline(r *metrics.Result) {
	give(r, "pipeline.flow", func(m *metrics.Metric) {
		m.Status = metrics.StatusBad
		m.Value, m.Num, m.Den, m.CI = fp(188.0/192), ip(188), ip(192), &[2]float64{0.95, 0.99}
		m.Extra = map[string]any{"ok": int64(188), "failed": int64(3), "set_aside": int64(1), "waiting": int64(0), "backlog_oldest_ms": int64(26 * 3600_000)}
		m.Series = nil
		for i := range 4 {
			m.Series = append(m.Series, metrics.Point{Day: fmt.Sprintf("2026-10-%02d", 5+i), Value: fp(0.9), Num: ip(45), Den: ip(50)})
		}
	})
	give(r, "retain.outcomes", func(m *metrics.Metric) {
		m.Value = fp(192)
		m.Extra = map[string]any{"by_status": map[string]int64{"ok": 188, "failed": 3, "quarantined": 1}, "p95_ms": 4200.0}
		m.Series = nil
		for i := range 4 {
			m.Series = append(m.Series, metrics.Point{Day: fmt.Sprintf("2026-10-%02d", 5+i), Value: fp(50),
				Parts: map[string]int64{"ok": 44, "failed": int64(i), "quarantined": 1, "queued": 1, "retry": 1}})
		}
	})
	give(r, "local.backlog", func(m *metrics.Metric) {
		m.Status = metrics.StatusBad
		m.Value = fp(6)
		m.Extra = map[string]any{"notes_queued": int64(2), "sessions_waiting": int64(3), "quarantined": int64(1), "oldest_ms": int64(26 * 3600_000)}
	})
	give(r, "server.queue", func(m *metrics.Metric) {
		m.Status = metrics.StatusWatch
		m.Value = fp(3)
		m.Extra = map[string]any{"pending": int64(2), "processing": int64(1), "failed": int64(1), "failed_total": int64(1), "failed_capped": false}
	})
	r.Tables.FailedOperations = []metrics.OperationRow{{Type: "retain", AgeMS: 5 * 3600_000, Retries: 3}}
	give(r, "server.extraction_delay", func(m *metrics.Metric) {
		m.Value, m.Den = fp(18*60_000), ip(200)
		m.Extra = map[string]any{"p50": 180_000.0, "p95": 18.0 * 60_000}
	})
	give(r, "hooks.failures", func(m *metrics.Metric) {
		m.Status = metrics.StatusWatch
		m.Value = fp(3)
		m.Extra = map[string]any{"by_kind": map[string]int64{"timeout": 2, "panic": 1}, "last_24h": map[string]int64{"panic": 1, "timeout": 1}}
		m.Series = nil
		for i := range 4 {
			p := metrics.Point{Day: fmt.Sprintf("2026-10-%02d", 5+i), Value: fp(0)}
			if i == 2 {
				p.Value, p.Parts = fp(3), map[string]int64{"panic": 1, "timeout": 2}
			}
			m.Series = append(m.Series, p)
		}
	})
	at := r.GeneratedAt.Add(-4 * time.Minute)
	give(r, "jobs.last_run", func(m *metrics.Metric) {
		m.Value = fp(4 * 60_000)
		m.Extra = map[string]any{"tick": map[string]any{"last_at": at, "status": "done", "ms": 2100, "age_ms": 4 * 60_000}}
	})
	r.Tables.Jobs = []metrics.JobRow{{Name: "tick", Status: "done", LastAt: at, MS: 2100, AgeMS: 4 * 60_000},
		{Name: "maintain", Status: "failed", LastAt: r.GeneratedAt.Add(-3 * time.Hour), MS: 48_000, AgeMS: 3 * 3600_000}}
}

func TestPipelineCards(t *testing.T) {
	n := pageNode(t, "pipeline", false, func(d *PageData) { richPipeline(d.V["pipeline"]) })
	want := []string{"w-pipeline-flow", "w-local-backlog", "w-server-queue", "w-server-extraction_delay", "w-hooks-failures", "w-jobs-last_run"}
	if got := cardIDs(n); !slices.Equal(got, want) {
		t.Fatalf("cards %v, want %v", got, want)
	}
	for i := 0; i < len(want); i += 2 {
		if cardOf(t, n, want[i]).Parent != cardOf(t, n, want[i+1]).Parent || cardOf(t, n, want[i]).Parent.attr("class") != "grid-2" {
			t.Errorf("%s and %s are not a pair", want[i], want[i+1])
		}
	}
	t.Run("sent", func(t *testing.T) {
		c := cardOf(t, n, "w-pipeline-flow")
		lead := c.first("div.row.lead")
		if lead == nil || !strings.HasPrefix(lead.text(), "188 of 192 · 98 % (95–99 %) sent") {
			t.Errorf("lead %q", lead.text())
		}
		if pill := c.first("span.pill"); pill == nil || pill.text() != "attention" || !strings.Contains(pill.attr("class"), "st-bad") {
			t.Errorf("pill %+v", pill)
		}
		if got := len(c.find("figure.chart")); got != 2 {
			t.Errorf("%d charts, want the share sent and the results", got)
		}
		if !hasCode(c, "backsight doctor") {
			t.Error("no advice for sessions that do not reach the server")
		}
		if !strings.Contains(c.text(), "slowest 1 in 20: 4.2 s") {
			t.Errorf("the slowest send: %q", c.text())
		}
	})
	t.Run("the server's queue", func(t *testing.T) {
		c := cardOf(t, n, "w-server-queue")
		rows := c.find("tbody tr")
		if len(rows) != 1 {
			t.Fatalf("%d rows", len(rows))
		}
		var cells []string
		for _, td := range rows[0].find("td") {
			cells = append(cells, td.text())
		}
		if w := []string{"saving notes", "5 h", "3"}; !slices.Equal(cells, w) {
			t.Errorf("failed task %q, want %q", cells, w)
		}
		if td := rows[0].find("td")[2]; !strings.Contains(td.attr("class"), "r") {
			t.Error("retries are not right-aligned")
		}
		if txt := c.text(); !strings.Contains(txt, "2 queued") || !strings.Contains(txt, "1 running") || !strings.Contains(txt, "1 failed") {
			t.Errorf("counts: %q", txt)
		}
		if !strings.Contains(c.text(), "No error text is shown here.") || !hasCode(c, "backsight doctor") {
			t.Errorf("the note on error text: %q", c.text())
		}
	})
	t.Run("the delay", func(t *testing.T) {
		c := cardOf(t, n, "w-server-extraction_delay")
		w := [][3]string{{"typical", "3 min", ""}, {"slowest 1 in 20", "18 min", "watch over 30 min"}, {"based on", "200", "last completed server tasks"}}
		if got := figsOf(c); !slices.Equal(got, w) {
			t.Errorf("figures %q, want %q", got, w)
		}
	})
	t.Run("steps that crashed", func(t *testing.T) {
		c := cardOf(t, n, "w-hooks-failures")
		var table *node
		for _, tb := range c.find("table.tbl") {
			if strings.HasPrefix(tb.text(), "How it stopped") {
				table = tb
			}
		}
		if table == nil {
			t.Fatalf("no failures table: %q", c.text())
		}
		var head []string
		for _, th := range table.find("thead th") {
			head = append(head, th.text())
		}
		if w := []string{"How it stopped", "Last 24 h", "Last 30 days"}; !slices.Equal(head, w) {
			t.Errorf("head %q, want %q", head, w)
		}
		var got []string
		for _, r := range table.find("tbody tr") {
			got = append(got, r.text())
		}
		// The window's count decides the order: ran too long (2), then crashed (1).
		if w := []string{"ran too long 1 2", "crashed 1 1"}; !slices.Equal(got, w) {
			t.Errorf("rows %q, want %q", got, w)
		}
		var legend []string
		for _, s := range c.find("figure.chart .legend span") {
			legend = append(legend, s.text())
		}
		if w := []string{"ran too long", "crashed"}; !slices.Equal(legend, w) {
			t.Errorf("legend %v, want %v", legend, w)
		}
		var swatches []string
		for _, i := range c.find("figure.chart .legend i") {
			swatches = append(swatches, i.attr("class"))
		}
		if w := []string{"sw f-seq1", "sw f-seq2"}; !slices.Equal(swatches, w) {
			t.Errorf("swatches %v, want %v", swatches, w)
		}
		if !hasCode(c, "backsight report") {
			t.Error("no way to report")
		}
		if txt := spokenText(pageHTML(t, "pipeline", func(d *PageData) { richPipeline(d.V["pipeline"]) })); strings.Contains(txt, "panic") || strings.Contains(txt, "timeout") {
			t.Errorf("a stored failure kind reached the page")
		}
	})
	t.Run("jobs", func(t *testing.T) {
		c := cardOf(t, n, "w-jobs-last_run")
		var rows []string
		for _, r := range c.find("tbody tr") {
			rows = append(rows, r.text())
		}
		if w := []string{"regular upkeep () 4 min ago done 2.1 s", "daily maintenance () 3 h ago failed 48.0 s"}; !slices.Equal(rows, w) {
			t.Errorf("rows %q, want %q", rows, w)
		}
		if tm := c.first("tbody time"); tm == nil || tm.attr("title") == "" || tm.attr("datetime") == "" {
			t.Errorf("last run time %+v", tm)
		}
		var classes []string
		for _, s := range c.find("tbody span.st") {
			classes = append(classes, s.attr("class"))
		}
		if w := []string{"st st-ok", "st st-bad"}; !slices.Equal(classes, w) {
			t.Errorf("result classes %q, want %q", classes, w)
		}
		if !hasCode(c.find("tbody tr")[0], "jobs tick") {
			t.Error("the job's command is not named")
		}
	})
}

func TestSendStatusesArePlain(t *testing.T) {
	n := pageNode(t, "pipeline", false, func(d *PageData) {
		richPipeline(d.V["pipeline"])
		give(d.V["pipeline"], "retain.outcomes", func(m *metrics.Metric) {
			m.Series = []metrics.Point{
				{Day: "2026-10-05", Value: fp(1), Partial: true, Parts: map[string]int64{"ok": 1}},
				{Day: "2026-10-06", Value: fp(10), Parts: map[string]int64{"ok": 5, "quarantined": 1, "queued": 1, "retry": 1, "failed": 2}}}
		})
	})
	c := cardOf(t, n, "w-pipeline-flow")
	var titles []string
	for _, h := range c.find("figure.chart") {
		if h.first("rect.seg") != nil {
			for _, r := range h.find("rect.hit title") {
				titles = append(titles, r.text())
			}
		}
	}
	// The first day is cut short and left out of the bars; the second is the one asked about.
	if w := []string{"Oct 6 · sent: 5, queued: 2, set aside: 1, failed: 2"}; !slices.Equal(titles, w) {
		t.Fatalf("hit titles %q, want %q", titles, w)
	}
	bars := c.find("figure.chart")
	var results *node
	for _, f := range bars {
		if f.first("rect.seg") != nil {
			results = f
		}
	}
	var names []string
	for _, s := range results.find(".legend span") {
		names = append(names, s.text())
	}
	if w := []string{"sent", "queued", "set aside", "failed"}; !slices.Equal(names, w) {
		t.Errorf("legend %v, want %v", names, w)
	}
	for _, class := range []string{"f-ok", "f-queued", "f-aside", "f-failed"} {
		if len(results.find("rect.seg."+class)) != 1 {
			t.Errorf("no %s part", class)
		}
	}
	var val []string
	for _, v := range results.find("text.val") {
		val = append(val, v.text())
	}
	if w := []string{"2 failed"}; !slices.Equal(val, w) {
		t.Errorf("printed counts %v, want %v", val, w)
	}
	if txt := spokenText(pageHTML(t, "pipeline", func(d *PageData) { richPipeline(d.V["pipeline"]) })); strings.Contains(strings.ToLower(txt), "quarantin") {
		t.Errorf("a stored status reached the page: %q", txt)
	}
	// A document with no status logged is its own part, drawn only when there is one.
	u := pageNode(t, "pipeline", false, func(d *PageData) {
		richPipeline(d.V["pipeline"])
		give(d.V["pipeline"], "retain.outcomes", func(m *metrics.Metric) {
			m.Series = []metrics.Point{{Day: "2026-10-05", Value: fp(1), Parts: map[string]int64{"ok": 1}}, {Day: "2026-10-06", Value: fp(3), Parts: map[string]int64{"ok": 1, "unknown": 2}}}
		})
	})
	if !strings.Contains(cardOf(t, u, "w-pipeline-flow").text(), "unknown: 2") {
		t.Error("documents with no status are lost from the bars")
	}
}

func TestPipelineBanner(t *testing.T) {
	banner := func(edit func(r *metrics.Result)) *node {
		n := pageNode(t, "pipeline", false, func(d *PageData) {
			if edit != nil {
				edit(d.V["pipeline"])
			}
		})
		return mainOf(t, n).first("div.state")
	}
	b := banner(func(r *metrics.Result) { richPipeline(r) })
	if b == nil || !strings.Contains(b.attr("class"), "st-bad") {
		t.Fatalf("banner %+v", b)
	}
	if b.first("b").text() != "3 sessions could not be sent; 1 set aside after an error." {
		t.Errorf("lead sentence %q", b.first("b").text())
	}
	if txt := b.text(); !strings.Contains(txt, "The oldest item has waited on this PC for 26 h.") || !strings.Contains(txt, "2 steps crashed in the last 24 h.") {
		t.Errorf("banner %q", txt)
	}
	// One failure, a watch: singular, the other class.
	w := banner(func(r *metrics.Result) {
		richPipeline(r)
		give(r, "pipeline.flow", func(m *metrics.Metric) {
			m.Status = metrics.StatusWatch
			m.Extra = map[string]any{"ok": int64(9), "failed": int64(1), "set_aside": int64(0), "waiting": int64(0), "backlog_oldest_ms": int64(0)}
		})
		give(r, "local.backlog", func(m *metrics.Metric) { m.Status = metrics.StatusOK })
		give(r, "hooks.failures", func(m *metrics.Metric) {
			m.Status = metrics.StatusOK
			m.Extra = map[string]any{"by_kind": map[string]int64{}, "last_24h": map[string]int64{"panic": 1}}
		})
	})
	if w == nil || !strings.Contains(w.attr("class"), "st-watch") || w.first("b").text() != "1 session could not be sent." || !strings.Contains(w.text(), "1 step crashed in the last 24 h.") {
		t.Errorf("watch banner %+v", w)
	}
	if strings.Contains(w.text(), "waited") {
		t.Errorf("a wait that is not one: %q", w.text())
	}
	// Nothing wrong: no banner. The golden's pipeline is healthy.
	if g := banner(nil); g != nil {
		t.Errorf("a banner on a healthy pipeline: %q", g.text())
	}
}

func TestBacklogCard(t *testing.T) {
	n := pageNode(t, "pipeline", false, func(d *PageData) { richPipeline(d.V["pipeline"]) })
	c := cardOf(t, n, "w-local-backlog")
	var rows []string
	for _, r := range c.find("tbody tr") {
		td := r.find("td")
		cmd := td[2].text()
		if code := td[2].first("code"); code != nil {
			cmd = codeText(code)
		}
		rows = append(rows, strings.TrimSpace(td[0].text()+" | "+td[1].text()+" | "+cmd))
	}
	if w := []string{"sessions | 3 | backsight retain flush", "notes | 2 | sent with the next session", "set aside after an error | 1 | backsight retain retry-quarantine"}; !slices.Equal(rows, w) {
		t.Errorf("rows %q, want %q", rows, w)
	}
	if trs := c.find("tbody tr"); len(trs[1].find("div.cmd")) != 0 {
		t.Error("a command for notes, which go with the next session")
	}
	if old := c.first("p.oldest"); old == nil || old.text() != "Oldest: 26 h" || old.first("span.st") == nil || !strings.Contains(old.first("span.st").attr("class"), "st-bad") {
		t.Errorf("oldest %+v", old)
	}
	if lead := c.first("div.row.lead"); lead == nil || !strings.HasPrefix(lead.text(), "6 ") {
		t.Errorf("lead %v", lead)
	}
	if !strings.Contains(c.text(), "2 notes and 3 sessions wait on this PC, 1 set aside after an error; the oldest 26 h") {
		t.Errorf("the line under the table: %q", c.text())
	}
	if why := c.first("details.why"); why == nil || !strings.Contains(why.text(), "Nothing is lost while they wait") {
		t.Errorf("why: %v", why)
	}
	// Nothing waits: counts of 0 offer no command, and no age.
	g := pageNode(t, "pipeline", false, nil)
	e := cardOf(t, g, "w-local-backlog")
	if len(e.find("tbody div.cmd")) != 0 || e.first("p.oldest") != nil {
		t.Errorf("a command or an age for an empty backlog: %q", e.text())
	}
	if len(e.find("tbody tr")) != 3 {
		t.Errorf("rows of an empty backlog: %d", len(e.find("tbody tr")))
	}
	// No data: the reason.
	none := pageNode(t, "pipeline", false, func(d *PageData) {
		give(d.V["pipeline"], "local.backlog", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonServerOnlyHost, nil
		})
	})
	nc := cardOf(t, none, "w-local-backlog")
	if e := nc.first("div.empty"); e == nil || !strings.Contains(e.text(), "This PC runs the server only") || nc.first("table") != nil {
		t.Errorf("no data: %q", nc.text())
	}
}

func TestQueueCardNeverShowsServerText(t *testing.T) {
	n := pageNode(t, "pipeline", false, func(d *PageData) {
		richPipeline(d.V["pipeline"])
		d.V["pipeline"].Tables.FailedOperations = []metrics.OperationRow{
			{Type: "<script>alert(1)</script>", AgeMS: 60_000, Retries: 1}, {Type: "batch_retain", AgeMS: 3600_000, Retries: 2}}
	})
	c := cardOf(t, n, "w-server-queue")
	var tasks []string
	for _, r := range c.find("tbody tr") {
		tasks = append(tasks, r.find("td")[0].text())
	}
	// A type the dashboard has no word for reads "other": the server's own text is never printed.
	if w := []string{"other", "saving notes in batches"}; !slices.Equal(tasks, w) {
		t.Errorf("tasks %q, want %q", tasks, w)
	}
	if strings.Contains(c.text(), "script") {
		t.Errorf("server text on the page: %q", c.text())
	}
	// None failed, a number: a sentence that is true.
	ok := pageNode(t, "pipeline", false, func(d *PageData) {
		richPipeline(d.V["pipeline"])
		d.V["pipeline"].Tables.FailedOperations = nil
		give(d.V["pipeline"], "server.queue", func(m *metrics.Metric) {
			m.Value = fp(0)
			m.Extra = map[string]any{"pending": int64(0), "processing": int64(0), "failed": int64(0), "failed_total": int64(0), "failed_capped": false}
		})
	})
	if txt := cardOf(t, ok, "w-server-queue").text(); !strings.Contains(txt, "No server task failed in the last 7 days.") {
		t.Errorf("no failure: %q", txt)
	}
	// A lower bound is said so, and a number-less queue says why instead.
	capped := pageNode(t, "pipeline", false, func(d *PageData) {
		richPipeline(d.V["pipeline"])
		give(d.V["pipeline"], "server.queue", func(m *metrics.Metric) {
			m.Extra = map[string]any{"pending": int64(2), "processing": int64(1), "failed": int64(40), "failed_total": int64(90), "failed_capped": true}
		})
	})
	if txt := cardOf(t, capped, "w-server-queue").text(); !strings.Contains(txt, "40+ failed") {
		t.Errorf("a lower bound shown as exact: %q", txt)
	}
	down := pageNode(t, "pipeline", false, func(d *PageData) {
		give(d.V["pipeline"], "server.queue", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonServerDown, nil
		})
		give(d.V["pipeline"], "server.extraction_delay", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonServerDown, nil
		})
	})
	for _, id := range []string{"w-server-queue", "w-server-extraction_delay"} {
		c := cardOf(t, down, id)
		if e := c.first("div.empty"); e == nil || !strings.HasPrefix(e.text(), "Memory server not answering") || c.first("table") != nil || c.first("div.fig") != nil {
			t.Errorf("%s down: %q", id, c.text())
		}
	}
}

func TestHooksCardOrderIsStable(t *testing.T) {
	counts := map[string]int64{"timeout": 2, "panic": 2, "fatal": 2, "exit": 1, "spawn": 1}
	var first []string
	for i := range 20 {
		n := pageNode(t, "pipeline", false, func(d *PageData) {
			richPipeline(d.V["pipeline"])
			give(d.V["pipeline"], "hooks.failures", func(m *metrics.Metric) {
				m.Extra = map[string]any{"by_kind": counts, "last_24h": map[string]int64{}}
			})
		})
		var got []string
		for _, tb := range cardOf(t, n, "w-hooks-failures").find("table.tbl") {
			if strings.HasPrefix(tb.text(), "How it stopped") {
				for _, r := range tb.find("tbody tr") {
					got = append(got, r.find("td")[0].text())
				}
			}
		}
		if i == 0 {
			first = got
			// By count, a tie by the name as the page writes it? No: by stored name, so it never moves.
			if w := []string{"stopped on an error", "crashed", "ran too long", "exited with an error", "could not start"}; !slices.Equal(got, w) {
				t.Fatalf("order %q, want %q", got, w)
			}
		} else if !slices.Equal(got, first) {
			t.Fatalf("order moved: %q then %q", first, got)
		}
	}
	// A kind the dashboard has no word for reads as an unrecorded reason; nothing the index wrote is printed.
	n := pageNode(t, "pipeline", false, func(d *PageData) {
		richPipeline(d.V["pipeline"])
		give(d.V["pipeline"], "hooks.failures", func(m *metrics.Metric) {
			m.Extra = map[string]any{"by_kind": map[string]int64{"<i>zz</i>": 1}, "last_24h": map[string]int64{}}
			m.Series = []metrics.Point{{Day: "2026-10-05"}, {Day: "2026-10-06", Parts: map[string]int64{"<i>zz</i>": 1}}}
		})
	})
	c := cardOf(t, n, "w-hooks-failures")
	if txt := c.text(); strings.Contains(txt, "zz") || !strings.Contains(txt, "reason not recorded") || c.first("figure.chart rect.seg") == nil {
		t.Errorf("an unknown kind (drawn %v): %q", c.first("figure.chart rect.seg") != nil, txt)
	}
	// Nothing failed: a sentence naming the window (a number of 0 is true) ; no number: the reason.
	zero := pageNode(t, "pipeline", false, nil)
	if e := cardOf(t, zero, "w-hooks-failures").first("div.empty"); e == nil || !strings.Contains(e.text(), "No step failed in the last 30 days.") {
		t.Errorf("zero failures: %v", e)
	}
	off := pageNode(t, "pipeline", false, func(d *PageData) {
		give(d.V["pipeline"], "hooks.failures", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonServerOnlyHost, nil
		})
	})
	if e := cardOf(t, off, "w-hooks-failures").first("div.empty"); e == nil || !strings.Contains(e.text(), "This PC runs the server only") {
		t.Errorf("no data: %v", e)
	}
}

func TestJobsCardWordsAndNoData(t *testing.T) {
	n := pageNode(t, "pipeline", false, func(d *PageData) {
		richPipeline(d.V["pipeline"])
		d.V["pipeline"].Tables.Jobs = []metrics.JobRow{{Name: "tick", Status: "<b>x</b>", LastAt: d.V["pipeline"].GeneratedAt, MS: 1, AgeMS: 0}}
	})
	c := cardOf(t, n, "w-jobs-last_run")
	// The age is the metric's own wording whatever the calendar says: "3 days ago", not "3 d ago".
	old := pageNode(t, "pipeline", false, func(d *PageData) {
		richPipeline(d.V["pipeline"])
		d.V["pipeline"].Tables.Jobs = []metrics.JobRow{{Name: "maintain", Status: "done", LastAt: d.V["pipeline"].GeneratedAt.Add(-72 * time.Hour), MS: 1000, AgeMS: 72 * 3600_000}}
	})
	if tm := cardOf(t, old, "w-jobs-last_run").first("tbody time"); tm == nil || tm.text() != "3 days ago" {
		t.Errorf("job age %+v", tm)
	}
	if txt := c.text(); strings.Contains(txt, "<b>") || strings.Contains(txt, "x") && strings.Contains(txt, "</b>") || !strings.Contains(txt, "not recorded") {
		t.Errorf("a status the dashboard has no word for: %q", txt)
	}
	none := pageNode(t, "pipeline", false, nil)
	nc := cardOf(t, none, "w-jobs-last_run")
	if e := nc.first("div.empty"); e == nil || nc.first("table") != nil {
		t.Errorf("no jobs: %q", nc.text())
	}
}

func TestPipelineFlowSaysWhyItIsEmpty(t *testing.T) {
	n := pageNode(t, "pipeline", false, func(d *PageData) {
		for _, id := range []string{"pipeline.flow", "retain.outcomes"} {
			give(d.V["pipeline"], id, func(m *metrics.Metric) {
				m.Status, m.Reason, m.Value, m.Num, m.Den = metrics.StatusNoData, metrics.ReasonServerOnlyHost, nil, nil, nil
			})
		}
	})
	c := cardOf(t, n, "w-pipeline-flow")
	if e := c.first("div.empty"); e == nil || !strings.Contains(e.text(), "This PC runs the server only") || c.first("figure.chart") != nil || c.first("div.row.lead") != nil {
		t.Errorf("empty flow: %q", c.text())
	}
}

// richSystem gives the system page a server, a PC with an update, a backup
// and a doctor with one of each status.
func richSystem(r *metrics.Result) {
	at := r.GeneratedAt
	r.Tables.System = &metrics.SystemInfo{ServerVersion: "1.1.0", Runtime: "native", Mode: "local",
		Reach:  metrics.ReachInfo{LastOK: at.Add(-time.Minute)},
		Backup: metrics.BackupInfo{CreatedAt: at.Add(-3 * 24 * time.Hour), VerifiedAt: at.Add(-3 * 24 * time.Hour), OK: true},
		Update: metrics.UpdateInfo{CheckedAt: at.Add(-time.Hour), Newer: 1},
		Index:  metrics.IndexInfo{Schema: 1, IngestedAt: at.Add(-3 * time.Minute), LagSeconds: 3}}
	r.Tables.Doctor = []metrics.DoctorRow{
		{ID: "server", Status: "ok"}, {ID: "backup", Status: "warn", Fix: "backsight backup create"},
		{ID: "update", Status: "info", Fix: "/backsight:update"}, {ID: "bank", Status: "error", Fix: "backsight doctor"}, {ID: "config", Status: "ok"}}
	give(r, "server.reach", func(m *metrics.Metric) { m.Value = fp(0) })
	give(r, "doctor.check", func(m *metrics.Metric) { m.Status, m.Value = metrics.StatusBad, fp(1) })
	give(r, "local.backlog", func(m *metrics.Metric) {
		m.Value = fp(0)
		m.Extra = map[string]any{"notes_queued": int64(0), "sessions_waiting": int64(0), "quarantined": int64(0), "oldest_ms": int64(0)}
	})
	give(r, "jobs.last_run", func(m *metrics.Metric) { m.Value = fp(4 * 60_000) })
	r.Tables.Jobs = []metrics.JobRow{{Name: "tick", Status: "done", LastAt: at.Add(-4 * time.Minute), MS: 1, AgeMS: 4 * 60_000},
		{Name: "maintain", Status: "partial", LastAt: at.Add(-3 * time.Hour), MS: 1, AgeMS: 3 * 3600_000}}
}

// dlOf are the label and text of each row of a card's description list.
func dlOf(c *node) [][2]string {
	var out [][2]string
	dts, dds := c.find("dl.dl dt"), c.find("dl.dl dd")
	for i := range dts {
		out = append(out, [2]string{dts[i].text(), dds[i].text()})
	}
	return out
}

func TestSystemCards(t *testing.T) {
	n := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		d.V["system"].Scope.Server = "127.0.0.1:18888"
		d.Frame.Version = "v0.1.17-m7"
		d.Frame.ServerRead = &metrics.ServerState{OK: true}
	})
	var titles []string
	for _, h := range mainOf(t, n).find("h2") {
		titles = append(titles, h.text())
	}
	if w := []string{"Memory server", "This PC", "What needs a look here?", "Do backsight's own health checks pass?"}; !slices.Equal(titles, w) {
		t.Fatalf("titles %q, want %q", titles, w)
	}
	grid := cardOf(t, n, "w-server-reach").Parent
	if grid.attr("class") != "grid-3" || cardOf(t, n, "w-this-pc").Parent != grid || cardOf(t, n, "w-attention").Parent != grid {
		t.Error("the three cards are not side by side")
	}
	t.Run("memory server", func(t *testing.T) {
		c := cardOf(t, n, "w-server-reach")
		if chip := c.first("span.chip"); chip == nil || chip.text() != "server" {
			t.Errorf("chip %+v", chip)
		}
		w := [][2]string{{"Address", "127.0.0.1:18888"}, {"Version", "1.1.0"}, {"Runs as", "a native install, on this PC"},
			{"Last answer", "1 min ago"}, {"Pausing after errors", "no"}, {"Prompts skipped", "0 in the last 30 days"}}
		if got := dlOf(c); !slices.Equal(got, w) {
			t.Errorf("rows %q, want %q", got, w)
		}
		if pill := c.first("span.pill"); pill == nil || pill.text() != "answering" || !strings.Contains(pill.attr("class"), "st-ok") {
			t.Errorf("pill %+v", pill)
		}
		// The question of the metric is in the popover.
		if !strings.Contains(c.first("details.info").text(), "Does the memory server answer this PC?") {
			t.Errorf("popover %q", c.first("details.info").text())
		}
	})
	t.Run("this PC", func(t *testing.T) {
		c := cardOf(t, n, "w-this-pc")
		w := [][2]string{{"backsight", "v0.1.17-m7"}, {"Update", "1 newer release"}, {"Background jobs", "regular upkeep: done, 4 min ago · daily maintenance: partly done, 3 h ago"},
			{"Waiting to be sent", "0 waiting"}, {"Last backup", "3 d ago · verified"}, {"History index", "updated 3 min ago, 3 s behind"}}
		got := dlOf(c)
		if len(got) != len(w) {
			t.Fatalf("rows %q", got)
		}
		for i := range w {
			if got[i][0] != w[i][0] || !strings.HasPrefix(got[i][1], w[i][1]) {
				t.Errorf("row %d: %q, want %q", i, got[i], w[i])
			}
		}
		if !hasCode(c, "/backsight:update") {
			t.Error("no update command")
		}
		if up := c.find("dl.dl dd")[1].first("span.st"); up == nil || !strings.Contains(up.attr("class"), "st-info") {
			t.Errorf("update class %+v", up)
		}
		for _, q := range []string{"Are the background jobs running?", "Is anything stuck on this PC?"} {
			if !strings.Contains(c.first("details.info").text(), q) {
				t.Errorf("popover lacks %q", q)
			}
		}
	})
	t.Run("what needs a look", func(t *testing.T) {
		c := cardOf(t, n, "w-attention")
		if c.first("span.chip") != nil {
			t.Error("a scope chip on a card that mixes the server and this PC")
		}
		var items []string
		for _, li := range c.find("li.att-item") {
			items = append(items, li.first("span.pill").text()+" | "+li.first("p").text())
		}
		// Worst first: a problem, then a warning.
		if w := []string{"attention | memory: problem", "watch | backup: warning"}; !slices.Equal(items, w) {
			t.Errorf("items %q, want %q", items, w)
		}
		if li := c.find("li.att-item")[1]; !hasCode(li, "backsight backup create") {
			t.Error("the fix of a warning is not offered")
		}
		if !strings.Contains(c.text(), "1 problem, 1 warning") {
			t.Errorf("count: %q", c.text())
		}
	})
	t.Run("health checks", func(t *testing.T) {
		c := cardOf(t, n, "w-doctor-check")
		var rows, classes []string
		for _, r := range c.find("tbody tr") {
			td := r.find("td")
			fix := td[2].text()
			if code := td[2].first("code"); code != nil {
				fix = codeText(code)
			}
			rows = append(rows, td[0].text()+" | "+td[1].text()+" | "+fix)
			classes = append(classes, td[0].first("span.st").attr("class"))
		}
		// Worst first, then the doctor's own order.
		w := []string{"problem | memory | backsight doctor", "warning | backup | backsight backup create", "for information | update | /backsight:update", "ok | memory server | —", "ok | settings | —"}
		if !slices.Equal(rows, w) {
			t.Errorf("rows %q, want %q", rows, w)
		}
		if wc := []string{"st st-bad", "st st-watch", "st st-info", "st st-ok", "st st-ok"}; !slices.Equal(classes, wc) {
			t.Errorf("classes %q, want %q", classes, wc)
		}
		for _, r := range c.find("tbody tr") {
			td := r.find("td")
			if fix := td[2]; fix.text() != "—" && fix.first("div.cmd") == nil {
				t.Errorf("fix %q is not a command", fix.text())
			}
		}
	})
}

func TestSystemWhenNothingNeedsALook(t *testing.T) {
	n := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		d.V["system"].Tables.Doctor = []metrics.DoctorRow{{ID: "server", Status: "ok"}, {ID: "update", Status: "info", Fix: "/backsight:update"}}
		give(d.V["system"], "doctor.check", func(m *metrics.Metric) { m.Status, m.Value = metrics.StatusOK, fp(0) })
	})
	c := cardOf(t, n, "w-attention")
	if e := c.first("div.empty"); e == nil || !strings.Contains(e.text(), "Nothing needs a look.") || len(c.find("li.att-item")) != 0 {
		t.Errorf("attention: %q", c.text())
	}
	// Without the doctor's answer nobody can say nothing needs a look.
	d := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		d.V["system"].Tables.Doctor = nil
		give(d.V["system"], "doctor.check", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonNoEvents, nil
		})
	})
	if strings.Contains(cardOf(t, d, "w-attention").text(), "Nothing needs a look.") {
		t.Error("`Nothing needs a look` without any check")
	}
	if e := cardOf(t, d, "w-doctor-check").first("div.empty"); e == nil || cardOf(t, d, "w-doctor-check").first("table") != nil {
		t.Error("the checks table has no sentence for no data")
	}
	// The items the view itself returns come first, with their severity.
	items := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		d.V["system"].Attention = []metrics.Item{{Text: "Something is off <b>x</b>", Severity: metrics.SeverityBad, Why: "Because", Command: "backsight doctor"}}
	})
	li := cardOf(t, items, "w-attention").find("li.att-item")
	if len(li) != 3 || li[0].first("p").text() != "Something is off <b>x</b>" || li[0].first("span.pill").text() != "attention" {
		t.Errorf("view items: %d", len(li))
	}
}

func TestSystemServerStates(t *testing.T) {
	// Down: the pill says so, the last answer is the one this PC recorded.
	n := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		d.V["system"].Tables.System.ServerVersion = ""
		d.V["system"].Tables.System.Reach = metrics.ReachInfo{LastOK: d.V["system"].GeneratedAt.Add(-time.Hour), FailingSince: d.V["system"].GeneratedAt.Add(-30 * time.Minute),
			BreakerOpenUntil: d.V["system"].GeneratedAt.Add(10 * time.Minute), BreakerFailures: 3}
		d.Frame.ServerRead = &metrics.ServerState{}
	})
	c := cardOf(t, n, "w-server-reach")
	if pill := c.first("span.pill"); pill == nil || pill.text() != "not answering" || !strings.Contains(pill.attr("class"), "st-watch") {
		t.Errorf("pill %+v", pill)
	}
	rows := dlOf(c)
	got := map[string]string{}
	for _, r := range rows {
		got[r[0]] = r[1]
	}
	if got["Version"] != "—" || got["Last answer"] != "1 h ago" || !strings.HasPrefix(got["Pausing after errors"], "yes, until ") || !strings.Contains(got["Pausing after errors"], "after 3 failures") ||
		!strings.HasPrefix(got["Not answering since"], "30 min ago") {
		t.Errorf("rows %q", rows)
	}
	// A breaker that opened and closed is not a pause.
	past := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		d.V["system"].Tables.System.Reach = metrics.ReachInfo{BreakerOpenUntil: d.V["system"].GeneratedAt.Add(-time.Minute), BreakerFailures: 3}
	})
	pg := map[string]string{}
	for _, r := range dlOf(cardOf(t, past, "w-server-reach")) {
		pg[r[0]] = r[1]
	}
	if pg["Pausing after errors"] != "no" || pg["Last answer"] != "no answer recorded yet" {
		t.Errorf("past breaker: %q", pg)
	}
	// No server set up: a sentence, no rows.
	none := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		d.V["system"].Scope.Server = ""
		give(d.V["system"], "server.reach", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonNotConfigured, nil
		})
		d.Frame.ServerRead = nil
	})
	nc := cardOf(t, none, "w-server-reach")
	if e := nc.first("div.empty"); e == nil || !strings.Contains(e.text(), "No memory server is set up on this PC.") || len(nc.find("dl.dl dt")) != 0 {
		t.Errorf("no server: %q", nc.text())
	}
}

func TestSystemPCRows(t *testing.T) {
	n := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		s := d.V["system"].Tables.System
		s.Backup = metrics.BackupInfo{CreatedAt: d.V["system"].GeneratedAt.Add(-2 * time.Hour)}
		s.Update = metrics.UpdateInfo{CheckFailed: true}
		s.Index = metrics.IndexInfo{IngestedAt: d.V["system"].GeneratedAt.Add(-time.Minute), LagSeconds: 7200, Partial: true}
		give(d.V["system"], "local.backlog", func(m *metrics.Metric) {
			m.Value = fp(6)
			m.Extra = map[string]any{"notes_queued": int64(2), "sessions_waiting": int64(3), "quarantined": int64(1), "oldest_ms": int64(3600_000)}
		})
	})
	got := map[string]string{}
	for _, r := range dlOf(cardOf(t, n, "w-this-pc")) {
		got[r[0]] = r[1]
	}
	if got["Update"] != "the last check failed" || got["Last backup"] != "2 h ago · not verified" ||
		got["History index"] != "updated 1 min ago, 2 h behind, still catching up" ||
		got["Waiting to be sent"] != "6 waiting: 2 notes, 3 sessions, 1 set aside" {
		t.Errorf("rows %q", got)
	}
	if hasCode(cardOf(t, n, "w-this-pc"), "/backsight:update") {
		t.Error("an update command without an update")
	}
	// No backup and no release check yet.
	e := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		d.V["system"].Tables.System.Backup = metrics.BackupInfo{}
		d.V["system"].Tables.System.Update = metrics.UpdateInfo{}
		d.V["system"].Tables.Jobs = nil
		give(d.V["system"], "jobs.last_run", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonNoEvents, nil
		})
	})
	eg := map[string]string{}
	for _, r := range dlOf(cardOf(t, e, "w-this-pc")) {
		eg[r[0]] = r[1]
	}
	if eg["Last backup"] != "none yet" || eg["Update"] != "none newer" || eg["Background jobs"] != "none recorded yet" {
		t.Errorf("empty rows %q", eg)
	}
}

func TestSystemHasNoDeveloperIDsInPlainMode(t *testing.T) {
	ids := []string{"stale.never_used", "stale.hidden", "stale.reported", "server.reach", "local.backlog", "jobs.last_run", "doctor.check"}
	plain := pageNode(t, "system", false, func(d *PageData) { richSystem(d.V["system"]) })
	dev := pageNode(t, "system", true, func(d *PageData) { richSystem(d.V["system"]) })
	for _, id := range ids[3:] {
		var found bool
		for _, c := range dev.find("code") {
			found = found || strings.TrimSpace(c.Text) == id
		}
		if !found {
			t.Errorf("developer mode does not name %s", id)
		}
		for _, c := range plain.find("p.devid code") {
			if strings.TrimSpace(c.Text) == id {
				t.Errorf("plain mode names %s in a developer line", id)
			}
		}
	}
}

func TestPagesDoNotNameStoredValues(t *testing.T) {
	for page, rich := range map[string]func(r *metrics.Result){"bank": richBank, "pipeline": richPipeline, "system": richSystem} {
		txt := strings.ToLower(spokenText(pageHTML(t, page, func(d *PageData) { rich(d.V[page]) })))
		for _, bad := range []string{"quarantin", "consolidation", "retain ", "p95", "p50", "hook failure", "backfill"} {
			if strings.Contains(txt, bad) {
				t.Errorf("%s says %q", page, bad)
			}
		}
	}
}

func TestSystemHostileValues(t *testing.T) {
	page := pageHTML(t, "system", func(d *PageData) {
		richSystem(d.V["system"])
		d.V["system"].Scope.Server = hostile
		d.V["system"].Tables.Doctor = []metrics.DoctorRow{{ID: "backup", Status: "warn", Fix: "backsight " + hostile}, {ID: "odd", Status: "<i>zz</i>"}}
		d.V["system"].Tables.System.ServerVersion = hostile
	})
	if strings.Contains(page, "<b>x") || strings.Contains(page, "<i>zz") {
		t.Fatal("a raw tag reached the page")
	}
	n := parseHTML(t, page)
	got := map[string]string{}
	for _, r := range dlOf(cardOf(t, n, "w-server-reach")) {
		got[r[0]] = r[1]
	}
	if got["Address"] != hostile || got["Version"] != hostile {
		t.Errorf("rows %q", got)
	}
	rows := cardOf(t, n, "w-doctor-check").find("tbody tr")
	if len(rows) != 2 || !hasCode(rows[0], "backsight "+hostile) {
		t.Fatalf("%d rows", len(rows))
	}
	// A status the dashboard has no word for is "unknown", sorted last, and needs no look.
	cells := rows[1].find("td")
	if s := cells[0].first("span.st"); s == nil || s.text() != "unknown" || !strings.Contains(s.attr("class"), "st-few") {
		t.Errorf("unknown status %+v", s)
	}
	if got := len(cardOf(t, n, "w-attention").find("li.att-item")); got != 1 {
		t.Errorf("%d items to look at, want the one warning", got)
	}
}

func TestChartsAreHalfWidth(t *testing.T) {
	for page, rich := range map[string]func(r *metrics.Result){"bank": richBank, "pipeline": richPipeline} {
		n := pageNode(t, page, false, func(d *PageData) { rich(d.V[page]) })
		charts := mainOf(t, n).find("figure.chart")
		if len(charts) < 2 {
			t.Fatalf("%s: %d charts", page, len(charts))
		}
		for _, c := range charts {
			if svg := c.first("svg"); svg.attr("class") != "vb560" || svg.attr("viewbox") != "0 0 560 230" {
				t.Errorf("%s: a chart of %v", page, svg.Attr)
			}
		}
	}
}

func TestFlowCardDrawsWhenOnlyTheResultsHaveANumber(t *testing.T) {
	n := pageNode(t, "pipeline", false, func(d *PageData) {
		richPipeline(d.V["pipeline"])
		give(d.V["pipeline"], "pipeline.flow", func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value, m.Num, m.Den = metrics.StatusNoData, metrics.ReasonNoEvents, nil, nil, nil
		})
	})
	c := cardOf(t, n, "w-pipeline-flow")
	if c.first("div.empty") != nil || c.first("rect.seg.f-ok") == nil || c.first("div.row.lead") != nil {
		t.Errorf("flow without a share: %q", c.text())
	}
	// A healthy pipeline offers no advice.
	g := pageNode(t, "pipeline", false, nil)
	if hasCode(cardOf(t, g, "w-pipeline-flow"), "backsight doctor") {
		t.Error("advice for a pipeline that is fine")
	}
}
