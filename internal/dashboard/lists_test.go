package dashboard

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// colOf is the index of the column headed head in table.
func colOf(t *testing.T, table *node, head string) int {
	t.Helper()
	var heads []string
	for i, th := range table.find("thead th") {
		if th.text() == head {
			return i
		}
		heads = append(heads, th.text())
	}
	t.Fatalf("no column %q in %q", head, heads)
	return -1
}

// bodyRows are the rows of the table's body that are not the pooled one.
func bodyRows(table *node) (rows []*node, total *node) {
	for _, tr := range table.find("tbody tr") {
		if strings.Contains(" "+tr.attr("class")+" ", " total ") {
			total = tr
			continue
		}
		rows = append(rows, tr)
	}
	return rows, total
}

// listTable is the one design table of a page's main.
func listTable(t *testing.T, n *node) *node {
	t.Helper()
	m := mainOf(t, n)
	if got := len(m.find("div.tbl-wrap table.tbl")); got != 1 {
		t.Fatalf("%d design tables in main, want 1", got)
	}
	return m.first("div.tbl-wrap table.tbl")
}

func TestListsAreDesignTables(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	for _, l := range []struct {
		path string
		rows func(r metrics.Result) int
		pool bool
	}{
		{"/projects", func(r metrics.Result) int { return len(r.Tables.Projects) }, true},
		{"/sessions", func(r metrics.Result) int { return len(r.Tables.Sessions) }, false},
		{"/memories", func(r metrics.Result) int { return len(r.Tables.Memories) }, false},
		{"/days", func(r metrics.Result) int { return len(r.Tables.Days) }, false},
	} {
		want := l.rows(apiPath(t, h, "/api/v1"+l.path))
		_, b := pageGet(t, h, ck, l.path)
		rows, total := bodyRows(listTable(t, parseHTML(t, b)))
		if len(rows) != want || want == 0 {
			t.Errorf("%s: %d body rows, the API has %d", l.path, len(rows), want)
		}
		if (total != nil) != l.pool {
			t.Errorf("%s: pooled row %v, want %v", l.path, total != nil, l.pool)
		}
	}
	// A filter link is current when the page is filtered by it, and only that one.
	_, b := pageGet(t, h, ck, "/sessions?has=used")
	var cur []string
	for _, a := range parseHTML(t, b).find("main nav.seg a") {
		if a.attr("aria-current") == "true" {
			cur = append(cur, a.text())
		}
	}
	if len(cur) != 1 || cur[0] != views.HasWords["used"] {
		t.Errorf("/sessions?has=used: current links %q, want %q", cur, views.HasWords["used"])
	}
	_, b = pageGet(t, h, ck, "/sessions")
	cur = nil
	for _, a := range parseHTML(t, b).find("main nav.seg a") {
		if a.attr("aria-current") == "true" {
			cur = append(cur, a.text())
		}
	}
	if len(cur) != 1 || cur[0] != "All" {
		t.Errorf("/sessions: current links %q, want All", cur)
	}
}

func TestPooledRowShowsExtremes(t *testing.T) {
	n := pageNode(t, "projects", false, nil)
	table := listTable(t, n)
	rows, total := bodyRows(table)
	last := table.find("tbody tr")
	if total == nil || last[len(last)-1] != total || len(rows) != 2 {
		t.Fatalf("the pooled row is not the last of %d rows", len(last))
	}
	cells := total.find("td")
	if !strings.HasPrefix(total.text(), "all projects pooled") || cells[0].text() != "all projects pooled" {
		t.Errorf("pooled row starts %q", total.text())
	}
	pool := goldenResult(t, "projects").Tables.Pooled
	for _, c := range []struct{ head, key string }{
		{"Sessions", "sessions"}, {"Prompts", "prompts"}, {"Got memory", "prompts_with_memory"},
		{"Used", "used_turns"}, {"Reported wrong", "reported_wrong"},
	} {
		want := "lowest " + format.English.Int(int64(pool.Min[c.key])) + " · highest " + format.English.Int(int64(pool.Max[c.key]))
		if got := cells[colOf(t, table, c.head)].text(); !strings.Contains(got, want) {
			t.Errorf("%s: %q, want it to hold %q", c.head, got, want)
		}
	}
	// The columns differ in the golden: a swapped column would show.
	if pool.Max["sessions"] == pool.Max["prompts"] {
		t.Fatal("the golden cannot tell the Sessions and Prompts columns apart")
	}
	// Without the extremes the totals stand alone.
	n = pageNode(t, "projects", false, func(d *PageData) { d.V["projects"].Tables.Pooled.Min, d.V["projects"].Tables.Pooled.Max = nil, nil })
	_, total = bodyRows(listTable(t, n))
	if strings.Contains(total.text(), "lowest") || strings.Contains(total.text(), "highest") {
		t.Errorf("no extremes given, the row says %q", total.text())
	}
	// A row's own cells carry no extremes.
	if strings.Contains(rows[0].text(), "lowest") {
		t.Errorf("a project row says %q", rows[0].text())
	}
}

func TestProjectWithoutMemories(t *testing.T) {
	for _, none := range []bool{true, false} {
		n := pageNode(t, "project", false, func(d *PageData) {
			p := d.V["projects"].Tables.Project
			p.NoMemories, p.Command = none, "backsight retain backfill --project zz --plan"
			p.TopUsed, p.NeverUsed = nil, nil
		})
		var empty *node
		for _, e := range mainOf(t, n).find("div.empty") {
			if strings.Contains(e.text(), "This project has no memories yet. Import its past sessions:") {
				empty = e
			}
		}
		if (empty != nil) != none {
			t.Fatalf("no memories %v: sentence shown %v", none, empty != nil)
		}
		if !none {
			continue
		}
		if !hasCode(empty, "backsight retain backfill --project zz --plan") || empty.first("button.copy[hidden]") == nil {
			t.Errorf("the command is not in <code> with a hidden copy button: %q", empty.Text)
		}
		if len(mainOf(t, n).find("div.grid-2")) != 0 {
			t.Error("a project without memories lists memories")
		}
	}
}

func TestProjectAndDayTiles(t *testing.T) {
	names := "Found,Used,Reported wrong,Context,Speed,Flowing"
	check := func(label string, n *node, r *metrics.Result) {
		t.Helper()
		tiles := mainOf(t, n).find("article.tile")
		var got []string
		for _, a := range tiles {
			got = append(got, a.first("h3.tile-name").text())
		}
		if strings.Join(got, ",") != names {
			t.Fatalf("%s: tiles %q", label, got)
		}
		used := tiles[1]
		if used.first("p.tile-sub") != nil {
			t.Errorf("%s: the Used tile has a sub-line: %q", label, used.first("p.tile-sub").text())
		}
		den := *r.Metrics["used.turn_rate"].Den
		if a := used.first("div.tile-foot a"); a == nil || a.text() != format.Count(format.English, den, "turn", "turns")+" →" {
			t.Errorf("%s: the Used link %v counts used.turn_rate's turns (%d)", label, a, den)
		}
		for _, a := range tiles {
			if q := a.first("p.tile-q"); q == nil || q.text() == "" {
				t.Errorf("%s: %s has no question", label, a.first("h3.tile-name").text())
			}
		}
	}
	check("project", pageNode(t, "project", false, nil), goldenResult(t, "project-page"))
	check("day", pageNode(t, "day", false, nil), goldenResult(t, "day-page"))

	// The day page's heading is the question about that day.
	h := dataHarness(t)
	day := apiPath(t, h, "/api/v1/days").Tables.Days[0].Day
	_, b := pageGet(t, h, h.login(), "/days/"+day)
	res := apiPath(t, h, "/api/v1/days/"+day)
	if res.Tables.Day == nil || res.Tables.Day.Day != day {
		t.Fatalf("the API's day %+v", res.Tables.Day)
	}
	if h1 := mainOf(t, parseHTML(t, b)).first("h1"); h1 == nil || h1.text() != dayTitle(res.Tables.Day.Day) {
		t.Errorf("h1 %v, want %q", h1, dayTitle(res.Tables.Day.Day))
	}
	if got := len(mainOf(t, parseHTML(t, b)).find("article.tile")); got != 6 {
		t.Errorf("/days/%s: %d tiles", day, got)
	}
}

// The tiles of a page about one project or one day list that project's or
// that day's events and print the command that reproduces them.
func TestProjectAndDayTilesCarryTheirScope(t *testing.T) {
	foundOf := func(n *node) (href, cli string) {
		a := mainOf(t, n).first("article.tile")
		return a.first("div.tile-foot a").attr("href"), a.first("details.cli code").text()
	}
	n := pageNode(t, "project", false, func(d *PageData) { d.V["projects"].Tables.Project.Project = hostile })
	href, cli := foundOf(n)
	u, err := url.Parse(href)
	if err != nil || u.Path != "/sessions" || u.Query().Get("project") != hostile || u.Query().Get("has") != "memory" || u.Query().Get("days") != "30" {
		t.Errorf("project tile link %q", href)
	}
	if !strings.HasSuffix(cli, "--project "+quoteArg(hostile)) {
		t.Errorf("project tile command %q", cli)
	}
	n = pageNode(t, "day", false, func(d *PageData) { d.V["days"].Tables.Day.Day = "2026-10-05" })
	href, cli = foundOf(n)
	u, err = url.Parse(href)
	if err != nil || u.Query().Get("since") != "2026-10-05" || u.Query().Get("until") != "2026-10-06" || u.Query().Has("days") {
		t.Errorf("day tile link %q", href)
	}
	if !strings.Contains(cli, "--since 2026-10-05 --until 2026-10-06") || strings.Contains(cli, "--days") {
		t.Errorf("day tile command %q", cli)
	}
	// A day that is no date leaves the page's own window alone.
	n = pageNode(t, "day", false, func(d *PageData) { d.V["days"].Tables.Day.Day = "2026-10-05" + hostile })
	href, _ = foundOf(n)
	if u, _ = url.Parse(href); u.Query().Has("since") || u.Query().Get("days") != "30" {
		t.Errorf("tile link of an unreadable day %q", href)
	}
}

func TestTileSparksSayTheirSpan(t *testing.T) {
	label := func(n *node) string {
		return mainOf(t, n).first("article.tile svg.spark").attr("aria-label")
	}
	if got := label(pageNode(t, "day", false, nil)); !strings.HasPrefix(got, "Found per hour, that day: ") {
		t.Errorf("day spark %q", got)
	}
	if got := label(pageNode(t, "project", false, func(d *PageData) { d.V["projects"].Window.Days = 30 })); !strings.HasPrefix(got, "Found per day, last 30 days: ") {
		t.Errorf("30-day spark %q", got)
	}
	if got := label(pageNode(t, "project", false, nil)); !strings.HasPrefix(got, "Found per day, last 7 days: ") {
		t.Errorf("7-day spark %q", got)
	}
}

func TestDayNavigation(t *testing.T) {
	navOf := func(day string, edit func(d *PageData)) (prev, next *node) {
		n := pageNode(t, "day", false, func(d *PageData) {
			d.V["days"].Tables.Day.Day = day
			if edit != nil {
				edit(d)
			}
		})
		as := mainOf(t, n).find("div.day-nav a")
		for _, a := range as {
			switch {
			case strings.HasPrefix(a.text(), "←"):
				prev = a
			case strings.HasSuffix(a.text(), "→") && a.attr("class") == "btn":
				next = a
			}
		}
		return prev, next
	}
	prev, next := navOf("2026-10-05", nil)
	if prev == nil || prev.attr("href") != "/days/2026-10-04" || prev.text() != "← Oct 4" {
		t.Errorf("previous %v", prev)
	}
	if next == nil || next.attr("href") != "/days/2026-10-06" || next.text() != "Oct 6 →" {
		t.Errorf("next %v", next)
	}
	// The zone is kept: the day boundaries depend on it.
	prev, next = navOf("2026-10-05", func(d *PageData) { d.P.TZ = "Europe/Paris" })
	if prev.attr("href") != "/days/2026-10-04?tz=Europe%2FParis" || next.attr("href") != "/days/2026-10-06?tz=Europe%2FParis" {
		t.Errorf("zone lost: %q %q", prev.attr("href"), next.attr("href"))
	}
	// The golden's generation day is 2026-10-08: no way on from today or after.
	for _, day := range []string{"2026-10-08", "2026-10-09"} {
		if _, next = navOf(day, nil); next != nil {
			t.Errorf("%s has a next day: %v", day, next.attr("href"))
		}
	}
	// A day that is no date has no neighbours, and nothing it holds reaches the page.
	bad := "2026-10-05\"><b>x</b>"
	page := pageHTML(t, "day", func(d *PageData) { d.V["days"].Tables.Day.Day = bad })
	if len(mainOf(t, parseHTML(t, page)).find("div.day-nav")) != 0 || strings.Contains(page, "<b>x") {
		t.Error("an unreadable day has neighbours or injected markup")
	}
}

func TestSessionRows(t *testing.T) {
	long := strings.Repeat("s", 30)
	page := pageHTML(t, "sessions", func(d *PageData) {
		rows := d.V["sessions"].Tables.Sessions
		rows[0].ID, rows[0].Project, rows[0].Wrong, rows[0].ContextChars, rows[0].Sent = long, hostile, 2, 3100, "ok"
		rows[1].ID, rows[1].Project, rows[1].Wrong, rows[1].Sent = "../"+hostile, "..", 0, "failed"
		rows[2].ID, rows[2].Project, rows[2].Sent = "..", "", "quarantined"
		rows[3].Sent = "queued"
		for i := range rows {
			rows[i].Start = d.V["sessions"].GeneratedAt.Add(-time.Duration(i+1) * time.Hour)
		}
	})
	if strings.Contains(page, "<b>x") {
		t.Fatal("a raw tag reached the page")
	}
	table := listTable(t, parseHTML(t, page))
	rows, _ := bodyRows(table)
	cell := func(i int, head string) *node { return rows[i].find("td")[colOf(t, table, head)] }
	// An id is a chip: 8 characters, the whole id on hover.
	if a := cell(0, "Session").first("a.idc"); a == nil || a.text() != long[:8] || a.attr("title") != long || a.attr("href") != "/sessions/"+long {
		t.Errorf("id chip %v", a)
	}
	// A project is cut at 24 characters with its name on hover, and links to its page with the window.
	if a := cell(0, "Project").first("a.trunc"); a == nil || a.text() != hostile || a.attr("title") != hostile || a.attr("href") != "/projects/"+hostilePath+"?days=30" {
		t.Errorf("project link %v", a)
	}
	// A name that reads as a relative path is not linked.
	if cell(1, "Project").first("a") != nil || cell(2, "Session").first("a") != nil {
		t.Errorf("a link to '..': %q %q", cell(1, "Project").Text, cell(2, "Session").Text)
	}
	if a := cell(1, "Session").first("a.idc"); a == nil || a.attr("href") != "/sessions/..%2F"+hostilePath {
		t.Errorf("hostile id link %v", a)
	}
	if cell(2, "Project").text() != "no project" {
		t.Errorf("no project: %q", cell(2, "Project").text())
	}
	// Times are <time>; numbers are right-aligned; the context is sized.
	if tm := cell(0, "When").first("time"); tm == nil || tm.attr("datetime") == "" || tm.text() != "1 h ago" {
		t.Errorf("time %v", tm)
	}
	for _, h := range []string{"Prompts", "Got memory", "Shown", "Used", "Wrong", "Context added"} {
		if c := cell(0, h); !strings.Contains(c.attr("class"), "r") {
			t.Errorf("%s is not right-aligned", h)
		}
	}
	if got := cell(0, "Context added").text(); got != "3,100 chars" {
		t.Errorf("context %q", got)
	}
	// A wrong report stands out; none does not.
	if s := cell(0, "Wrong").first("span.st-watch"); s == nil || s.text() != "2" {
		t.Errorf("wrong %v", s)
	}
	if cell(1, "Wrong").first("span") != nil {
		t.Error("a zero count is coloured")
	}
	for i, want := range []struct{ word, class string }{{"sent", "st-ok"}, {"failed", "st-bad"}, {"set aside after an error", "st-bad"}, {"waiting", "st-watch"}} {
		if s := cell(i, "Sent").first("span.st"); s == nil || s.text() != want.word || !strings.Contains(s.attr("class"), want.class) {
			t.Errorf("sent %d: %v, want %q %s", i, s, want.word, want.class)
		}
	}
}

func TestEmptyListsSayWhy(t *testing.T) {
	for _, c := range []struct {
		page string
		edit func(d *PageData)
		want string
	}{
		{"sessions", func(d *PageData) { d.V["sessions"].Tables.Sessions = nil }, "No session in this window."},
		{"sessions", func(d *PageData) { d.V["sessions"].Tables.Sessions = nil; d.P.Has = "used" }, "No session in this window matches this filter."},
		{"memories", func(d *PageData) { d.V["memories"].Tables.Memories = nil }, "No memory has been shown in this window."},
		{"memories", func(d *PageData) { d.V["memories"].Tables.Memories = nil; d.P.State = "hidden" }, "No memory in this window matches this filter."},
		{"days", func(d *PageData) { d.V["days"].Tables.Days = nil }, "No day in this window has any activity."},
		{"projects", func(d *PageData) { d.V["projects"].Tables.Projects, d.V["projects"].Tables.Pooled = nil, nil }, "No project has any activity in this window."},
		{"day", func(d *PageData) { d.V["days"].Tables.Day.Sessions = nil }, "No sessions that day."},
		{"project", func(d *PageData) { d.V["projects"].Tables.Project.Sessions = nil }, "No session of this project in this window."},
	} {
		n := pageNode(t, c.page, false, c.edit)
		m := mainOf(t, n)
		var said bool
		for _, e := range m.find("div.empty") {
			said = said || strings.Contains(e.text(), c.want)
		}
		if !said || len(m.find("div.tbl-wrap")) != 0 && c.page != "project" {
			t.Errorf("%s: no %q (or a table anyway): %q", c.page, c.want, m.text())
		}
	}
}

func TestPagerKeepsTheFiltersAndWindow(t *testing.T) {
	enc := "%22%3E%3Cb%3Ex%3C%2Fb%3E"
	for _, c := range []struct{ page, path string }{
		{"sessions", "/sessions?project=" + enc + "&days=30&has=used&offset=1"},
		{"memories", "/memories?project=" + enc + "&days=30&state=hidden&sort=rate&offset=1"},
		{"days", "/days?project=" + enc + "&days=30&offset=1"},
		{"projects", "/projects?project=" + enc + "&days=30&offset=1"},
	} {
		page := pageHTML(t, c.page, func(d *PageData) {
			d.Frame.Path = c.path
			d.V[c.page].Tables.Page = &metrics.Page{Limit: 1, Offset: 1, Total: 4}
		})
		if strings.Contains(page, "<b>x") {
			t.Fatalf("%s: a raw tag reached the page", c.page)
		}
		base := strings.TrimSuffix(c.path, "&offset=1")
		var got []string
		for _, a := range mainOf(t, parseHTML(t, page)).find("div.pager a") {
			got = append(got, a.attr("rel")+" "+a.attr("href"))
		}
		want := []string{"prev " + base + "&offset=0", "next " + base + "&offset=2"}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: pager links %q, want %q", c.page, got, want)
		}
		if sp := mainOf(t, parseHTML(t, page)).first("div.pager span"); sp == nil || sp.text() != "2–2 of 4" {
			t.Errorf("%s: pager text %v", c.page, sp)
		}
	}
	// Filter links keep the window and the project; the paging starts over.
	page := pageHTML(t, "memories", func(d *PageData) { d.Frame.Path = "/memories?project=" + enc + "&days=30&state=hidden&offset=50" })
	for _, a := range mainOf(t, parseHTML(t, page)).find("nav.seg a") {
		if h := a.attr("href"); !strings.Contains(h, "project="+enc) || !strings.Contains(h, "days=30") || strings.Contains(h, "offset") {
			t.Errorf("filter link %q", h)
		}
	}
	// No page, no pager; one page, no links.
	n := pageNode(t, "sessions", false, func(d *PageData) { d.V["sessions"].Tables.Page = &metrics.Page{Limit: 50, Total: 4} })
	if m := mainOf(t, n); len(m.find("div.pager a")) != 0 || m.first("div.pager") == nil {
		t.Error("a single page has links or no count")
	}
	n = pageNode(t, "sessions", false, func(d *PageData) { d.V["sessions"].Tables.Page = nil })
	if mainOf(t, n).first("div.pager") != nil {
		t.Error("a list without a page has a pager")
	}
}

func TestProjectRows(t *testing.T) {
	long := strings.Repeat("p", 30)
	page := pageHTML(t, "projects", func(d *PageData) {
		r := d.V["projects"]
		r.Tables.Projects[0].Project = long
		r.Tables.Projects[0].Prompts, r.Tables.Projects[0].PromptsWithMemory = 312, 97
		r.Tables.Projects[0].MemoriesOnServer = ip(1284)
		r.Tables.Projects[1].Project, r.Tables.Projects[1].MemoriesOnServer = hostile, nil
		r.Tables.Projects[1].Prompts, r.Tables.Projects[1].PromptsWithMemory = 7, 5
		r.Tables.Projects = append(r.Tables.Projects, metrics.ProjectRow{Project: "..", Prompts: 0})
		r.Tables.Projects = append(r.Tables.Projects, metrics.ProjectRow{Project: "", Prompts: 20, PromptsWithMemory: 20})
	})
	if strings.Contains(page, "<b>x") {
		t.Fatal("a raw tag reached the page")
	}
	table := listTable(t, parseHTML(t, page))
	rows, _ := bodyRows(table)
	cell := func(i int, head string) *node { return rows[i].find("td")[colOf(t, table, head)] }
	if a := cell(0, "Project").first("a.trunc"); a == nil || a.text() != views.Clip(long) || a.attr("title") != long || a.attr("href") != "/projects/"+long+"?days=30" {
		t.Errorf("long project %v", a)
	}
	if a := cell(1, "Project").first("a.trunc"); a == nil || a.attr("href") != "/projects/"+hostilePath+"?days=30" || a.attr("title") != hostile {
		t.Errorf("hostile project %v", a)
	}
	if cell(2, "Project").first("a") != nil {
		t.Error("'..' is linked")
	}
	if cell(3, "Project").text() != "no project" || cell(3, "Project").first("a") != nil {
		t.Errorf("no project: %q", cell(3, "Project").text())
	}
	// Enough prompts: a percent with its count. Too few: the counts and why.
	if got := cell(0, "Got memory").text(); got != "31 % (n 312)" {
		t.Errorf("share %q", got)
	}
	if s := cell(1, "Got memory").first("span.st-few"); s == nil || s.text() != "5 of 7: too few" {
		t.Errorf("few %v", s)
	}
	if got := cell(0, "Memories").text(); got != "1,284" {
		t.Errorf("memories %q", got)
	}
	// Each row has its spark, named for the reader; none for the pooled row.
	for i, r := range rows {
		sp := r.first("svg.spark[role=img]")
		if sp == nil || !strings.HasPrefix(sp.attr("aria-label"), "Prompts per day, ") {
			t.Errorf("row %d spark %v", i, sp)
		}
	}
	if sp := rows[0].first("svg.spark"); sp != nil && strings.Contains(sp.attr("aria-label"), long) {
		t.Error("the spark names the whole project")
	}
	_, total := bodyRows(table)
	if total.first("svg") != nil || total.first("a") != nil {
		t.Error("the pooled row has a link or a spark")
	}
	// No sort links: the view takes no sort.
	if len(table.find("thead a")) != 0 {
		t.Error("the Projects table is sortable")
	}
	if tm := cell(0, "Last activity").first("time"); tm == nil || tm.attr("datetime") == "" {
		t.Errorf("last activity %v", tm)
	}
}

func TestMemoryRows(t *testing.T) {
	now := goldenResult(t, "memories").GeneratedAt
	ago := func(h int) *time.Time { t := now.Add(-time.Duration(h) * time.Hour); return &t }
	long := strings.Repeat("m", 30)
	edit := func(d *PageData) {
		d.V["memories"].Tables.Memories = []metrics.MemoryRow{
			{ID: long, Project: "p1", Shown: 21, Used: 12.64, Ignored: 4.1, Rate: fp(0.75), Boost: 0.5, LastShown: ago(1), LastUsed: ago(2), State: ""},
			{ID: "m2", Project: "", Shown: 3, Used: 0, Ignored: 8.7, Rate: nil, Boost: -0.3, LastShown: ago(3), LastUsed: nil, State: "wrong"},
			{ID: "m3/../x", Project: hostile, Shown: 1, Boost: 0.001, LastShown: nil, LastUsed: nil, State: "hidden"},
			{ID: "m4", Shown: 1, State: "never_used"},
		}
	}
	for _, dev := range []bool{false, true} {
		n := pageNode(t, "memories", dev, edit)
		table := listTable(t, n)
		rows, _ := bodyRows(table)
		if len(rows) != 4 {
			t.Fatalf("%d rows", len(rows))
		}
		cell := func(i int, head string) *node { return rows[i].find("td")[colOf(t, table, head)] }
		if a := cell(0, "Memory").first("a.idc"); a == nil || a.text() != long[:8] || a.attr("title") != long || a.attr("href") != "/memories/"+long {
			t.Errorf("id chip %v", a)
		}
		for i, want := range []string{"21", "12.6", "4.1", "75 %"} {
			if got := cell(0, []string{"Shown", "Used", "Ignored", "Used when judged"}[i]).text(); got != want {
				t.Errorf("column %d: %q, want %q", i, got, want)
			}
		}
		for i, want := range []string{"raised", "lowered", "unchanged", "unchanged"} {
			got := cell(i, "Learned ranking").text()
			if !strings.HasPrefix(got, want) {
				t.Errorf("row %d ranking %q, want %q", i, got, want)
			}
			// The number is for the developer.
			if has := strings.Contains(got, "+") || strings.Contains(got, "-0"); has != dev {
				t.Errorf("row %d, dev %v: ranking %q", i, dev, got)
			}
		}
		if cell(1, "Project").text() != "no project" || cell(2, "Project").first("a").attr("href") != "/projects/"+hostilePath+"?days=30" {
			t.Errorf("projects %q %q", cell(1, "Project").text(), cell(2, "Project").Text)
		}
		if a := cell(2, "Memory").first("a.idc"); a == nil || a.attr("href") != "/memories/m3%2F..%2Fx" {
			t.Errorf("odd id %v", a)
		}
		// Times, and the word for a moment that never came.
		if cell(0, "Last shown").first("time") == nil || cell(0, "Last used").first("time") == nil {
			t.Error("no <time>")
		}
		for _, h := range []string{"Last shown", "Last used"} {
			if c := cell(2, h); c.first("time") != nil || c.first("span.muted") == nil || c.text() != "never" {
				t.Errorf("%s of a memory never shown: %q", h, c.text())
			}
		}
		if cell(1, "Last used").text() != "never" || cell(1, "Last shown").text() == "never" {
			t.Errorf("last used %q last shown %q", cell(1, "Last used").text(), cell(1, "Last shown").text())
		}
		if got := cell(1, "Used when judged").text(); got != dash {
			t.Errorf("no rate: %q", got)
		}
		// States are words; a wrong report stands out.
		for i, want := range []string{"active", "reported wrong", "hidden", "often shown, never used"} {
			if got := cell(i, "State").text(); got != want {
				t.Errorf("state %d: %q, want %q", i, got, want)
			}
		}
		if s := cell(1, "State").first("span.st-watch"); s == nil {
			t.Error("a wrong report is not coloured")
		}
		if cell(2, "State").first("span") != nil {
			t.Error("hidden is coloured")
		}
	}
	// The note under the table.
	n := pageNode(t, "memories", false, edit)
	if p := mainOf(t, n).find("p.small.muted"); len(p) == 0 || p[len(p)-1].text() != "Counts are recent-weighted: older uses count less." {
		t.Errorf("note %v", p)
	}
}

func TestMemoryBadges(t *testing.T) {
	n := pageNode(t, "memories", false, func(d *PageData) {
		for id, v := range map[string]float64{"stale.never_used": 7, "stale.hidden": 3, "stale.reported": 2} {
			give(d.V["memories"], id, func(m *metrics.Metric) { m.Value = fp(v) })
		}
		d.Frame.Path = "/memories?days=30"
	})
	var got []string
	for _, a := range mainOf(t, n).find("div.badges a.badge") {
		got = append(got, a.first("b").text()+" "+strings.TrimSpace(strings.TrimPrefix(a.text(), a.first("b").text()))+" "+a.attr("href"))
	}
	want := []string{
		"7 often shown, never used /memories?days=30&state=never_used",
		"3 hidden /memories?days=30&state=hidden",
		"2 reported wrong /memories?days=30&state=wrong",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("badges %q, want %q", got, want)
	}
	// A count the view could not give is not shown as zero.
	n = pageNode(t, "memories", false, func(d *PageData) {
		give(d.V["memories"], "stale.hidden", func(m *metrics.Metric) {
			m.Value = nil
			m.Status = metrics.StatusNoData
			m.Reason = metrics.ReasonServerDown
		})
		give(d.V["memories"], "stale.never_used", func(m *metrics.Metric) { m.Value = fp(1) })
		give(d.V["memories"], "stale.reported", func(m *metrics.Metric) { m.Value = fp(0) })
	})
	got = nil
	for _, a := range mainOf(t, n).find("div.badges a.badge") {
		got = append(got, a.text())
	}
	if len(got) != 2 || !strings.Contains(got[0], "never used") || !strings.Contains(got[1], "reported wrong") {
		t.Errorf("badges without a count for hidden: %q", got)
	}
}

func TestDayRows(t *testing.T) {
	n := pageNode(t, "days", false, func(d *PageData) {
		d.P.TZ = "Europe/Paris"
		d.V["days"].Tables.Days = []metrics.DayRow{
			{Day: "2026-10-08", Sessions: 4, Prompts: 29, Recalls: 31, PromptsWithMemory: 9, UsedTurns: 3, Wrong: 1, ContextChars: 3400, SentOK: 6, SentFailed: 2},
			{Day: "..", Sessions: 1},
			{Day: "2026-10-07" + hostile},
		}
	})
	table := listTable(t, n)
	rows, _ := bodyRows(table)
	cell := func(i int, head string) *node { return rows[i].find("td")[colOf(t, table, head)] }
	if a := cell(0, "Day").first("a"); a == nil || a.attr("href") != "/days/2026-10-08?tz=Europe%2FParis" || a.text() != "2026-10-08" {
		t.Errorf("day link %v", a)
	}
	for _, c := range []struct{ head, want string }{{"Sessions", "4"}, {"Prompts", "29"}, {"Memory lookups", "31"}, {"Got memory", "9"}, {"Used", "3"}, {"Wrong", "1"}, {"Context added", "3,400 chars"}, {"Sent", "6"}, {"Send failed", "2"}} {
		if got := cell(0, c.head).text(); got != c.want {
			t.Errorf("%s: %q, want %q", c.head, got, c.want)
		}
	}
	if s := cell(0, "Send failed").first("span.st-bad"); s == nil || cell(1, "Send failed").first("span") != nil {
		t.Errorf("failed sends: coloured %v / %v", s, cell(1, "Send failed").first("span"))
	}
	if cell(1, "Day").first("a") != nil {
		t.Error("'..' is linked")
	}
	if a := cell(2, "Day").first("a"); a == nil || !strings.HasPrefix(a.attr("href"), "/days/2026-10-07%3Cb%3E") {
		t.Errorf("hostile day %v", a)
	}
}

func TestProjectPageCards(t *testing.T) {
	now := goldenResult(t, "project-page").GeneratedAt
	n := pageNode(t, "project", false, func(d *PageData) {
		p := d.V["projects"].Tables.Project
		p.NoMemories, p.AttributionDisagreements = false, 2
		p.TopUsed = []metrics.ReviewRow{{ID: "top/../1" + hostile, Project: "p1", Shown: 9, LastShown: &now}, {ID: "top2", Shown: 4}}
		p.NeverUsed = []metrics.ReviewRow{{ID: "never-0000001", Shown: 12, LastShown: &now, Command: "backsight forget never-0000001"}}
	})
	m := mainOf(t, n)
	cards := map[string]*node{}
	for _, c := range m.find("section.card") {
		cards[c.attr("aria-labelledby")] = c
	}
	top, never := cards["w-project-top-used"], cards["w-project-never-used"]
	if top == nil || never == nil || cards["w-project-sessions"] == nil {
		t.Fatalf("cards %v", cards)
	}
	rows, _ := bodyRows(top.first("table"))
	if len(rows) != 2 || rows[0].find("td")[1].text() != "9" {
		t.Fatalf("top used rows %d", len(rows))
	}
	if a := rows[0].first("a.idc"); a == nil || a.attr("href") != "/memories/top%2F..%2F1"+hostilePath || a.text() != "top/../1" {
		t.Errorf("hostile id %v", a)
	}
	if rows[1].find("td")[2].text() != dash {
		t.Errorf("a memory never shown: %q", rows[1].find("td")[2].text())
	}
	rows, _ = bodyRows(never.first("table"))
	if len(rows) != 1 || !hasCode(rows[0], "backsight forget never-0000001") || rows[0].first("button.copy[hidden]") == nil {
		t.Errorf("never used rows %d", len(rows))
	}
	if !strings.Contains(m.text(), "2 memories are attributed to another project by the server.") {
		t.Errorf("attribution: %q", m.text())
	}
	// Empty lists say why, one memory is singular, none is silent.
	n = pageNode(t, "project", false, func(d *PageData) {
		p := d.V["projects"].Tables.Project
		p.NoMemories, p.AttributionDisagreements, p.TopUsed, p.NeverUsed = false, 1, nil, nil
	})
	m = mainOf(t, n)
	for _, w := range []string{"No memory of this project has been used in this window.", "No memory of this project is shown often without being used.", "1 memory is attributed to another project by the server."} {
		if !strings.Contains(m.text(), w) {
			t.Errorf("project page lacks %q", w)
		}
	}
	n = pageNode(t, "project", false, func(d *PageData) { d.V["projects"].Tables.Project.AttributionDisagreements = 0 })
	if strings.Contains(mainOf(t, n).text(), "attributed") {
		t.Error("attribution said with none")
	}
	// The project's name is cut and its whole name on hover.
	long := strings.Repeat("q", 30)
	n = pageNode(t, "project", false, func(d *PageData) { d.V["projects"].Tables.Project.Project = long })
	if b := mainOf(t, n).first("p.small b"); b == nil || b.text() != views.Clip(long) || b.attr("title") != long {
		t.Errorf("project name %v", b)
	}
}

// Metric ids are the developer's: the list pages name none in plain mode,
// and the tiles of the project and day pages name theirs only in developer mode.
func TestListPagesNameNoMetricInPlainMode(t *testing.T) {
	ids := []string{"stale.never_used", "stale.hidden", "stale.reported", "recall.hit_rate", "used.turn_rate", "verdict.wrong_rate", "context.per_session", "recall.latency", "pipeline.flow"}
	for _, page := range []string{"projects", "project", "sessions", "memories", "days", "day"} {
		for _, dev := range []bool{false, true} {
			n := pageNode(t, page, dev, nil)
			m := mainOf(t, n)
			var named []string
			for _, c := range m.find("code") {
				for _, id := range ids {
					if strings.TrimSpace(c.Text) == id && c.attr("class") != "term" {
						named = append(named, id)
					}
				}
			}
			for _, d := range m.find("p.devid") {
				named = append(named, d.text())
			}
			for _, d := range m.find("span.devid") {
				named = append(named, d.text())
			}
			if (len(named) > 0) != (dev && (page == "project" || page == "day")) {
				t.Errorf("%s, dev %v: ids named %q", page, dev, named)
			}
		}
	}
}

func TestDayPageStatesItsDay(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	days := apiPath(t, h, "/api/v1/days").Tables.Days
	past := days[len(days)-1].Day
	_, b := pageGet(t, h, ck, "/days/"+past)
	n := parseHTML(t, b)
	head := n.first("main div.page-head")
	if head == nil || !strings.Contains(head.text(), "window: "+past) || strings.Contains(head.text(), "last 24 h") {
		t.Errorf("day page head %v", head)
	}
	if len(n.find("nav.seg[aria-label=Window]")) != 0 {
		t.Error("a day page offers a window")
	}
	// Other pages keep theirs.
	for _, p := range []string{"/", "/sessions", "/days", "/projects"} {
		_, b = pageGet(t, h, ck, p)
		n = parseHTML(t, b)
		if len(n.find("nav.seg[aria-label=Window]")) == 0 || n.first("main div.page-head span.muted") == nil ||
			!strings.Contains(n.first("main div.page-head").text(), "window: last 7 days") {
			t.Errorf("%s lost its window", p)
		}
	}
}

func TestSparkSpanIsTheDayPagesAlone(t *testing.T) {
	r := goldenResult(t, "day-page") // hourly series, as any window of two days or less
	v, _ := metricOf(r, "recall.hit_rate")
	if got := sparkSpan(r, v); got != "per hour, that day" {
		t.Errorf("day page: %q", got)
	}
	r.Tables.Day = nil
	for _, days := range []int{1, 2} {
		r.Window.Days = days
		want := "per day, last " + strconv.Itoa(days) + " days"
		if got := sparkSpan(r, v); got != want {
			t.Errorf("a %d-day window: %q, want %q", days, got, want)
		}
	}
}

func TestSessionCounts(t *testing.T) {
	for _, c := range []struct{ page, card string }{{"sessions", "w-sessions"}, {"day", "w-day-sessions"}} {
		n := pageNode(t, c.page, false, func(d *PageData) {
			view := map[string]string{"sessions": "sessions", "day": "days"}[c.page]
			d.V[view].Tables.Page = &metrics.Page{Limit: 2, Total: 34}
		})
		if a := cardOf(t, n, c.card).first("div.card-head span.small"); a == nil || a.text() != "34 sessions" {
			t.Errorf("%s: count %v", c.page, a)
		}
	}
	n := pageNode(t, "day", false, func(d *PageData) { d.V["days"].Tables.Page = &metrics.Page{Total: 1} })
	if a := cardOf(t, n, "w-day-sessions").first("div.card-head span.small"); a == nil || a.text() != "1 session" {
		t.Errorf("one session: %v", a)
	}
}

func TestDaySessionsStartAtAClockTimeAndLinkProjectsToTheDay(t *testing.T) {
	n := pageNode(t, "day", false, func(d *PageData) {
		d.P.TZ = "Europe/Paris"
		d.V["days"].Tables.Day.Day = "2026-10-08"
		d.V["days"].Tables.Day.Sessions[0].Start = time.Date(2026, 10, 8, 3, 0, 0, 0, parisZone())
		d.V["days"].Tables.Day.Sessions[0].Project = "p1"
	})
	table := listTable(t, n)
	rows, _ := bodyRows(table)
	when := rows[0].find("td")[colOf(t, table, "Started")]
	if when.text() != "03:00" || when.first("time") == nil {
		t.Errorf("started %q", when.text())
	}
	a := rows[0].find("td")[colOf(t, table, "Project")].first("a")
	if a == nil || a.attr("href") != "/projects/p1?since=2026-10-08&tz=Europe%2FParis&until=2026-10-09" {
		t.Errorf("project link %v", a)
	}
	// A page of sessions elsewhere keeps its own window.
	n = pageNode(t, "sessions", false, nil)
	table = listTable(t, n)
	rows, _ = bodyRows(table)
	if a := rows[0].find("td")[colOf(t, table, "Project")].first("a"); a == nil || a.attr("href") != "/projects/p1?days=30" {
		t.Errorf("sessions project link %v", a)
	}
}

func TestProjectSparkLeavesOutTheCutFirstDay(t *testing.T) {
	n := pageNode(t, "projects", false, func(d *PageData) {
		d.V["projects"].Tables.Projects[0].Series = []metrics.Point{
			{Day: "2026-10-01", Value: fp(99), Partial: true}, {Day: "2026-10-02", Value: fp(1)}, {Day: "2026-10-03", Value: fp(2)},
		}
	})
	table := listTable(t, n)
	rows, _ := bodyRows(table)
	if got := rows[0].first("svg.spark").attr("aria-label"); got != "Prompts per day, p1: 1, 2" {
		t.Errorf("spark label %q", got)
	}
}

func TestFilterLabelsAreCapitalised(t *testing.T) {
	for _, key := range []string{"has", "sort", "state"} {
		for _, l := range filterLinks("/memories", key) {
			if l.Label[:1] != strings.ToUpper(l.Label[:1]) {
				t.Errorf("%s label %q", key, l.Label)
			}
		}
	}
}

// A tile whose series holds no value draws no sparkline (red when it drew a
// 24-dash label), and a tile that has values still draws one.
func TestTileWithoutValuesDrawsNoSpark(t *testing.T) {
	has := goldenResult(t, "day-page")
	var withSeries int
	for _, m := range has.Metrics {
		for _, p := range m.Series {
			if p.Value != nil {
				withSeries++
				break
			}
		}
	}
	n := pageNode(t, "day", false, func(d *PageData) {
		for id, m := range d.V["days"].Metrics {
			for i := range m.Series {
				m.Series[i].Value = nil
			}
			d.V["days"].Metrics[id] = m
		}
	})
	tiles := mainOf(t, n).find("article.tile")
	if len(tiles) != 6 {
		t.Fatalf("%d tiles, want the six", len(tiles))
	}
	for _, a := range tiles {
		if sp := a.first("svg.spark"); sp != nil {
			t.Errorf("a tile without values draws %q", sp.attr("aria-label"))
		}
	}
	if withSeries == 0 {
		t.Skip("the golden holds no series")
	}
	full := mainOf(t, pageNode(t, "day", false, nil)).find("article.tile svg.spark")
	if len(full) == 0 {
		t.Error("a tile with values lost its sparkline")
	}
}
