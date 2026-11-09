package dashboard

import (
	"bytes"
	"html"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

const fixtureVersion = "v0.1.17-m7"

// withRecall gives r the time to recall: the typical whole way and the
// sessions it comes from.
func withRecall(r *metrics.Result, ms float64, sessions int64, st metrics.Status) {
	r.Metrics["pipeline.time_to_recall"] = metrics.Metric{Value: &ms, Num: &sessions, Den: &sessions, Status: st}
}

func healthyText(t *testing.T) (*metrics.Result, string) {
	t.Helper()
	r := scenarioNamed(t, "healthy")
	withRecall(r, 93_600_000, 118, metrics.StatusOK)
	return r, summaryText(r, views.Params{Days: 7}, parisZone(), "last 7 days", fixtureVersion)
}

// The text the design's Overview parts show for the healthy scenario.
const healthySummary = `backsight · Is my memory working? · last 7 days (Oct 24 to Oct 30)
Activity on this PC · memory on 127.0.0.1:18888

Memory is working.
  Found      ok      28 % of prompts got memory (21–35 %), +2 pts
  Right      ok      used in 62 % of turns (50–73 %), judged on 28 days
  Cost       ok      slowest prompts 1.6 s; context high 3,600 chars
  Flowing    ok      239 of 239 sessions sent; memory back in ~1 d 2 h

In the last 7 days: 46 prompts got memory, Claude used it in 10 turns,
about 17 k tokens added; extraction cost $43.62 (server, all PCs).

Reported wrong: too few to judge (1 of 14 verdicts).
Needs attention: nothing.
Memory: 1,284 memories, +46 in the last 7 days.

backsight v0.1.17-m7 · 2026-10-30 15:44`

func checkWidth(t *testing.T, text string) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if n := utf8.RuneCountInString(line); n > 72 {
			t.Errorf("%d columns, over 72: %q", n, line)
		}
	}
}

func TestSummaryTextHealthy(t *testing.T) {
	_, got := healthyText(t)
	if got != healthySummary {
		t.Errorf("summary text:\n%s\nwant:\n%s", got, healthySummary)
	}
	checkWidth(t, got)
}

func TestSummaryTextHasNoIDs(t *testing.T) {
	const canary = "CANARY-memory-text"
	r := scenarioNamed(t, "broken")
	r.Tables.Helped = []metrics.HelpedRow{{Memory: "7f3a9c219d2c4e1b0a7f63e85b1d0c4a", Project: "backsight", FactType: "world", Used: 6, Shown: 9}}
	r.Text = &metrics.MemoryText{ID: "7f3a9c219d2c4e1b0a7f63e85b1d0c4a", Text: canary}
	got := summaryText(r, views.Params{Days: 7}, parisZone(), "last 7 days", fixtureVersion)
	if !strings.Contains(got, "Needs attention:\n  1. 3 sessions could not be sent; 1 set aside after an error: backsight\n     retain retry-quarantine\n  2. A session has waited on this PC for 26 h: backsight doctor\n") {
		t.Errorf("the attention items are not in the text:\n%s", got)
	}
	if m := regexp.MustCompile(`\b[0-9a-f]{8}\b`).FindString(got); m != "" {
		t.Errorf("an id %q in the text:\n%s", m, got)
	}
	for _, bad := range []string{"sess-", "a41c09e2", "7f3a9c21", canary} {
		if strings.Contains(got, bad) {
			t.Errorf("%q in the text:\n%s", bad, got)
		}
	}
	checkWidth(t, got)
}

func TestSummaryTextWithProject(t *testing.T) {
	r := scenarioNamed(t, "healthy")
	got := summaryText(r, views.Params{Days: 7, Project: "backsight"}, parisZone(), "last 7 days", fixtureVersion)
	want := "backsight · Is my memory working? · last 7 days (Oct 24 to Oct 30)\n  project backsight\nActivity on this PC"
	if !strings.HasPrefix(got, want) {
		t.Errorf("first lines:\n%s\nwant them to start:\n%s", got, want)
	}
	// A window of dates leaves room for the project on the first line.
	got = summaryText(r, views.Params{Since: "2026-10-01", Until: "2026-10-08", Project: "backsight"}, parisZone(), "Oct 1 to Oct 7", fixtureVersion)
	if first, _, _ := strings.Cut(got, "\n"); first != "backsight · Is my memory working? · Oct 1 to Oct 7 · project backsight" {
		t.Errorf("first line %q", first)
	}
	checkWidth(t, got)
}

func TestSummaryTextHidesRemoteHosts(t *testing.T) {
	for _, c := range []struct{ server, line string }{
		{"127.0.0.1:18888", "memory on 127.0.0.1:18888"},
		{"http://127.0.0.1:18888", "memory on 127.0.0.1:18888"},
		{"localhost:18888", "memory on localhost:18888"},
		{"[::1]:18888", "memory on [::1]:18888"},
		{"http://127.0.0.2:18888", "memory on 127.0.0.2:18888"},
		{"http://10.0.0.5:18888", "memory on a remote server"},
		{"10.0.0.5:18888", "memory on a remote server"},
		{"http://nas.lan:18888", "memory on a remote server"},
		{"nas.lan:18888", "memory on a remote server"},
		{"localhost.example.com:18888", "memory on a remote server"},
		{"", "no memory server set up"}, // none set up: not a remote one
	} {
		r := scenarioNamed(t, "healthy")
		r.Scope.Server = c.server
		got := summaryText(r, views.Params{Days: 7}, parisZone(), "last 7 days", fixtureVersion)
		if want := "Activity on this PC · " + c.line + "\n"; !strings.Contains(got, want) {
			t.Errorf("server %q: want the line %q in:\n%s", c.server, want, got)
		}
		if c.line == "memory on a remote server" && (strings.Contains(got, "10.0.0.5") || strings.Contains(got, "nas.lan")) {
			t.Errorf("server %q: its host is in the text:\n%s", c.server, got)
		}
	}
}

func helpedRows() []metrics.HelpedRow {
	return []metrics.HelpedRow{
		{Memory: "7f3a9c219d2c4e1b0a7f63e85b1d0c4a", Project: "backsight", FactType: "world", Used: 6, Shown: 9},
		{Memory: "b20e5d479d2c4e1b0a7f63e85b1d0c4a", Project: "project-d-shared-frontend-tools", FactType: "experience", Used: 4, Shown: 11},
		{Memory: "4c81f0a39d2c4e1b0a7f63e85b1d0c4a", Project: "project-a", FactType: "observation", Used: 3, Shown: 3},
	}
}

func helpedCard(t *testing.T, r *metrics.Result) *node {
	t.Helper()
	c := renderOverview(t, r).first("section.card[aria-labelledby=w-helped]")
	if c == nil {
		t.Fatal("no Helped most card")
	}
	return c
}

func TestHelpedStates(t *testing.T) {
	const foot = "Show text opens the memory's page with its text, read live from the server and never kept."
	t.Run("normal", func(t *testing.T) {
		r := scenarioNamed(t, "healthy")
		r.Tables.Helped = helpedRows()
		c := helpedCard(t, r)
		if got := c.first("h2").text(); got != "Which memories helped most?" {
			t.Errorf("title %q", got)
		}
		if got := c.first("p.small.sub").text(); got != "Used most in the last 7 days." {
			t.Errorf("intro %q", got)
		}
		lis := c.find("ol.helped li")
		if len(lis) != 3 {
			t.Fatalf("%d rows, want 3", len(lis))
		}
		want := []string{
			"1 7f3a9c21 backsight · fact about you and your work used 6 of 9 shown Show text",
			"2 b20e5d47 project-d-shared-fronte… · fact from a session used 4 of 11 shown Show text",
			"3 4c81f0a3 project-a · summary used 3 of 3 shown Show text",
		}
		if got := texts(lis); !slices.Equal(got, want) {
			t.Errorf("rows %q, want %q", got, want)
		}
		id := lis[0].first("a.idc")
		if id.attr("href") != "/memories/7f3a9c219d2c4e1b0a7f63e85b1d0c4a" || id.attr("title") != "7f3a9c219d2c4e1b0a7f63e85b1d0c4a" {
			t.Errorf("id chip %v", id.Attr)
		}
		if p := lis[1].first("a.trunc"); p.attr("href") != "/projects/project-d-shared-frontend-tools" || p.attr("title") != "project-d-shared-frontend-tools" {
			t.Errorf("project link %v", p.Attr)
		}
		for i, class := range []string{"f-about", "f-session", "f-summary"} {
			if lis[i].first("i.sw") == nil || lis[i].first("i.sw").attr("class") != "sw "+class {
				t.Errorf("row %d swatch %v, want %s", i, lis[i].first("i.sw"), class)
			}
		}
		btn := lis[0].first("a.btn")
		if btn.attr("href") != "/memories/7f3a9c219d2c4e1b0a7f63e85b1d0c4a/text" || btn.attr("aria-label") != "Show text of memory 7f3a9c21" {
			t.Errorf("Show text %v", btn.Attr)
		}
		if got := c.find("p.xs.muted"); len(got) != 1 || got[0].text() != foot {
			t.Errorf("foot sentences %q", texts(got))
		}
		if len(c.find("p.small.muted")) != 0 || len(c.find("div.empty")) != 0 || len(c.find("div.state")) != 0 {
			t.Error("a few/none/down sentence on a normal card")
		}
		if foot := c.first("div.card-foot a"); foot == nil || foot.attr("href") != "/memories?sort=used" || foot.text() != "All memories, by use →" {
			t.Errorf("card foot %v", foot)
		}
		if cli := c.first("details.cli code"); cli == nil || cli.Text != "backsight stats overview" {
			t.Errorf("CLI %v", cli)
		}
	})
	t.Run("few", func(t *testing.T) {
		r := scenarioNamed(t, "healthy")
		r.Tables.Helped = helpedRows()[:2]
		c := helpedCard(t, r)
		if n := len(c.find("ol.helped li")); n != 2 {
			t.Errorf("%d rows, want 2", n)
		}
		if got := texts(c.find("p.small.muted")); !slices.Equal(got, []string{"Only 2 memories were used in the last 7 days."}) {
			t.Errorf("sentence %q", got)
		}
		r.Tables.Helped = helpedRows()[:1]
		if got := texts(helpedCard(t, r).find("p.small.muted")); !slices.Equal(got, []string{"Only 1 memory was used in the last 7 days."}) {
			t.Errorf("one: %q", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		r := scenarioNamed(t, "healthy")
		r.Tables.Helped = nil
		c := helpedCard(t, r)
		if len(c.find("ol.helped")) != 0 {
			t.Error("a list with no memory")
		}
		want := "No memory was used in the last 7 days yet. When Claude says it used a memory, the ones it used most show up here. A longer window may already have some."
		if got := c.first("div.empty").text(); got != want {
			t.Errorf("empty %q, want %q", got, want)
		}
	})
	t.Run("server down", func(t *testing.T) {
		r := scenarioNamed(t, "serverdown")
		r.Tables.Helped = helpedRows()
		c := helpedCard(t, r)
		want := "Memory server not answering since 09:12: the counts are from this PC, the text can't be read until it answers."
		if got := c.first("div.state").text(); got != want {
			t.Errorf("state %q, want %q", got, want)
		}
		if n := len(c.find("a.btn")); n != 0 {
			t.Errorf("%d Show text links while the server is down", n)
		}
		if got := texts(c.find("span.act")); !slices.Equal(got, []string{"text unavailable", "text unavailable", "text unavailable"}) {
			t.Errorf("actions %q", got)
		}
		if n := len(c.find("ol.helped li a.idc")); n != 3 {
			t.Errorf("%d id links, want 3: the counts and pages stay", n)
		}
	})
	t.Run("a window of dates", func(t *testing.T) {
		if got := windowIn("Oct 1 to Oct 7"); got != "from Oct 1 to Oct 7" {
			t.Errorf("windowIn %q", got)
		}
	})
}

func flowingTileNode(t *testing.T, r *metrics.Result) *node {
	t.Helper()
	tiles := renderOverview(t, r).find("article.tile")
	return tiles[len(tiles)-1]
}

func TestFlowingSubline(t *testing.T) {
	const sub = "Session to recalled memory: typical 1 d 2 h · 239 of 239 sessions sent"
	r := scenarioNamed(t, "healthy")
	withRecall(r, 93_600_000, 118, metrics.StatusOK)
	tile := flowingTileNode(t, r)
	if got := tile.first("p.tile-sub").text(); got != sub {
		t.Errorf("subline %q, want %q", got, sub)
	}
	if got := tile.first("details.info").text(); !strings.Contains(got, "time to recall from 118 sessions recalled") {
		t.Errorf("Based on %q lacks the sessions the time comes from", got)
	}

	// Its watch shows on its own card only: the tile says the value and nothing else.
	r = scenarioNamed(t, "healthy")
	withRecall(r, 93_600_000, 118, metrics.StatusWatch)
	tile = flowingTileNode(t, r)
	if got := tile.first("p.tile-sub").text(); got != sub {
		t.Errorf("watch: subline %q, want %q", got, sub)
	}
	if got := tile.first(".tile-st .st").text(); got != "ok" {
		t.Errorf("watch: tile word %q, want ok", got)
	}
	if strings.Contains(tile.first("p.tile-sub").text(), "watch") || len(tile.find(".st-watch")) != 0 || strings.Contains(tile.attr("class"), "is-watch") {
		t.Error("the tile shows the watch of time to recall")
	}

	// Too few sessions: no value, the subline is the existing one.
	r = scenarioNamed(t, "healthy")
	r.Metrics["pipeline.time_to_recall"] = metrics.Metric{Status: metrics.StatusTooFew}
	if got := flowingTileNode(t, r).first("p.tile-sub").text(); got != "239 of 239 sessions sent" {
		t.Errorf("too few: subline %q", got)
	}
	// Not computed at all (an older result): the same.
	r = scenarioNamed(t, "healthy")
	if got := flowingTileNode(t, r).first("p.tile-sub").text(); got != "239 of 239 sessions sent" {
		t.Errorf("absent: subline %q", got)
	}
}

func TestLongDuration(t *testing.T) {
	for _, c := range []struct {
		ms   float64
		want string
	}{
		{22 * 60_000, "22 min"},
		{5.2 * 3_600_000, "5.2 h"},
		{23.6 * 3_600_000, "1 d"},
		{24 * 3_600_000, "1 d"},
		{26 * 3_600_000, "1 d 2 h"},
		{(24*9 + 0.2) * 3_600_000, "9 d"},
		{(24*2 + 23.6) * 3_600_000, "3 d"},
	} {
		if got := longDuration(en, c.ms); got != c.want {
			t.Errorf("%v ms: %q, want %q", c.ms, got, c.want)
		}
	}
}

func TestCopySummaryOnTheOverview(t *testing.T) {
	r, want := healthyText(t)
	d := renderOverviewChanged(t, r, func(d *PageData) { d.Frame.Version = fixtureVersion })
	row := d.first("div.summary-row")
	if row == nil || row.first("p.summary") == nil {
		t.Fatalf("no summary row: %v", row)
	}
	btn := row.first("button.copy")
	if btn == nil || !btn.has("hidden") || btn.attr("data-copy") != want || btn.text() != "Copy summary" {
		t.Fatalf("Copy summary button %v", btn)
	}
	if !strings.HasPrefix(btn.attr("aria-label"), "Copy summary") {
		t.Errorf("aria-label %q", btn.attr("aria-label"))
	}
	box := d.first("details.summary-text")
	if box == nil || box.has("open") || box.first("summary").text() != "Summary as text" {
		t.Fatalf("Summary as text disclosure %v", box)
	}
	if got := html.UnescapeString(box.first("pre.sample code").Text); got != want {
		t.Errorf("the disclosure holds:\n%s\nwant the button's text:\n%s", got, want)
	}
}

func overviewGet(t *testing.T, h *harness, path string) (int, *node) {
	t.Helper()
	r := h.do("GET", path, cookieHdr(h.login())...)
	b := body(t, r)
	if r.StatusCode != http.StatusOK {
		return r.StatusCode, nil
	}
	return r.StatusCode, parseHTML(t, b)
}

func TestCalendarOnOverview(t *testing.T) {
	h := dataHarness(t)
	cal := apiPath(t, h, "/api/v1/days").Tables.Calendar
	if len(cal) < 390 {
		t.Fatalf("the days view holds %d days: the check sees too little", len(cal))
	}
	status, page := overviewGet(t, h, "/")
	if status != 200 {
		t.Fatalf("GET /: %d", status)
	}
	card := page.first("section.card[aria-labelledby=w-calendar]")
	if card == nil {
		t.Fatal("no calendar card on /")
	}
	if got := card.first("h2").text(); got != "Which days did memory help?" {
		t.Errorf("title %q", got)
	}
	links := card.find("div.hm a.hm-c")
	if len(links) != len(cal) {
		t.Errorf("%d day links, want the %d days of the days view", len(links), len(cal))
	}
	for i, a := range links {
		want := "/days/" + cal[i].Day
		if a.attr("href") != want {
			t.Errorf("day %d links to %q, want %q (every day is a link, recorded or not)", i, a.attr("href"), want)
			break
		}
	}
	if n := len(card.find("a.hm-c.none")); n == 0 {
		t.Error("no unrecorded day: the check sees nothing of them")
	}
	if got := card.first(".hm-legend .ramp span").text(); got != "Prompts that got memory:" {
		t.Errorf("default legend %q", got)
	}
	if n := len(card.find("a.hm-c[tabindex=\"0\"]")); n != 1 {
		t.Errorf("%d day links in the tab order, want 1", n)
	}
	if tip := card.first("p.hm-tip"); tip == nil || tip.attr("aria-live") != "polite" {
		t.Errorf("tip %v", tip)
	}
	hint := "One Tab stop: arrow keys move from day to day, Enter opens it. On a small screen, the last 13 weeks; older days are in the table."
	if got := texts(card.find("p.xs.muted")); !slices.Equal(got, []string{hint}) {
		t.Errorf("hint %q", got)
	}
	if cli := card.first("details.cli code"); cli == nil || cli.Text != "backsight stats days" {
		t.Errorf("CLI %v", cli)
	}
	seg := card.find("nav.seg a")
	if got := texts(seg); !slices.Equal(got, []string{"Prompts that got memory", "Turns where Claude used memory", "Sessions"}) {
		t.Fatalf("colour choices %q", got)
	}
	for i, hm := range []string{"got", "used", "sessions"} {
		if got := seg[i].attr("href"); got != "/?hm="+hm+"#w-calendar" {
			t.Errorf("choice %d links to %q", i, got)
		}
	}
	if seg[0].attr("aria-current") != "true" || seg[1].has("aria-current") || seg[2].has("aria-current") {
		t.Error("the default choice is not the current one")
	}

	// ?hm=used colours by turns where Claude used memory.
	status, page = overviewGet(t, h, "/?hm=used")
	if status != 200 {
		t.Fatalf("GET /?hm=used: %d", status)
	}
	card = page.first("section.card[aria-labelledby=w-calendar]")
	if got := card.first(".hm-legend .ramp span").text(); got != "Turns where Claude used memory:" {
		t.Errorf("legend %q", got)
	}
	if got := texts(card.find(".hm-legend .ramp span")); !slices.Contains(got, "5+") || slices.Contains(got, "10+") {
		t.Errorf("legend steps %q: want 1, 2, 3–4, 5+", got)
	}
	if cur := card.find("nav.seg a[aria-current]"); len(cur) != 1 || cur[0].text() != "Turns where Claude used memory" {
		t.Errorf("current choice %v", cur)
	}
	// Only the page takes hm: the API and the other pages refuse it.
	for _, path := range []string{"/api/v1/overview?hm=used", "/quality?hm=used"} {
		if r := h.do("GET", path, cookieHdr(h.login())...); r.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: %d, want 400", path, r.StatusCode)
		}
	}
	// A word it does not know is the default.
	status, page = overviewGet(t, h, "/?hm=zzz")
	if status != 200 {
		t.Fatalf("GET /?hm=zzz: %d", status)
	}
	if got := page.first("section.card[aria-labelledby=w-calendar] .hm-legend .ramp span").text(); got != "Prompts that got memory:" {
		t.Errorf("hm=zzz: legend %q, want the default", got)
	}
	// The window and project stay on the choice links.
	status, page = overviewGet(t, h, "/?days=30&hm=sessions")
	if status != 200 {
		t.Fatalf("GET /?days=30&hm=sessions: %d", status)
	}
	if got := page.first("section.card[aria-labelledby=w-calendar] nav.seg a").attr("href"); got != "/?days=30&hm=got#w-calendar" {
		t.Errorf("choice link %q", got)
	}
}

func TestCalendarMarksTodayAndTheWindow(t *testing.T) {
	r := goldenResult(t, "days")
	n := len(r.Tables.Calendar)
	card := calendarOf(r, r.Window, views.Params{Days: 7}, "got", "/", "last 7 days")
	page := parseHTML(t, string(card.Chart))
	if got := len(page.find("a.hm-c.today")); got != 1 {
		t.Errorf("%d days marked today", got)
	}
	if page.first("a.hm-c.today").attr("href") != "/days/"+r.Tables.Calendar[n-1].Day {
		t.Errorf("today links to %q", page.first("a.hm-c.today").attr("href"))
	}
	// The golden's window is [Oct 1 14:00, Oct 8 14:00): the days Oct 1 to Oct 8.
	if got := len(page.find("a.hm-c.inwin")); got != 8 {
		t.Errorf("%d days in the window, want 8 (Oct 1 to Oct 8)", got)
	}
	if got := page.first("a.hm-c.inwin").attr("aria-label"); !strings.HasPrefix(got, "Thu, Oct 1:") {
		t.Errorf("first day of the window %q", got)
	}
	if got := card.Head.CLI; got != "backsight stats days" {
		t.Errorf("CLI %q", got)
	}
	if got := calendarOf(nil, metrics.Window{}, views.Params{}, "got", "/", "").Chart; !strings.Contains(string(got), "No days to show.") {
		t.Errorf("no days: %q", got)
	}
}

func TestOverviewReadsTwoViews(t *testing.T) {
	_, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	if got := specs["overview"].Views; !slices.Equal(got, []string{"overview", "days"}) {
		t.Errorf("the Overview reads %v", got)
	}
}

// renderOverviewChanged is renderOverview with a change to the page data.
func renderOverviewChanged(t *testing.T, r *metrics.Result, change func(*PageData)) *node {
	t.Helper()
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	spec := specs["overview"]
	d := fixtureData(t, spec, false)
	d.V["overview"] = r
	d.Loc, d.L = r.Window.Loc, en
	d.P = views.Params{Days: 7}
	d.Frame.Window = "last 7 days"
	d.CLI = "backsight stats overview"
	change(&d)
	var buf bytes.Buffer
	if err := tpls["overview"].ExecuteTemplate(&buf, spec.File, d); err != nil {
		t.Fatal(err)
	}
	return parseHTML(t, buf.String())
}
