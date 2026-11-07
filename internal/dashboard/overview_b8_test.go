package dashboard

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// b8Window is a window phrase of the Overview: the page's "last 7 days".
const b8Window = "last 7 days"

// allPCsPage is the Overview's parameters with all PCs selected.
var allPCsPage = views.Params{Days: 7, DaysSet: true, Machines: "all"}

// rangeMS is a bucket range as the view computes it.
func rangeMS(lo int64, hi *int64) *[2]*int64 { return &[2]*int64{&lo, hi} }

func i64p(n int64) *int64 { return &n }

// b8PCs is the scope of the PCs of the design's boards: this PC and three
// others, one of which went quiet 9 days ago and one document ignored; active
// of the four had a prompt in the window.
func b8PCs(active int) *metrics.PCs {
	pcs := fourPCs()
	pcs.Active = active
	return pcs
}

// allPCsOverview is the healthy Overview added up over the four PCs, with the
// figures of the design's B8Parts (98 of 325 prompts, 67 of 105 turns, 3 of 25
// verdicts, 2,300 characters per session over 311 sessions, the slowest 1 in 20
// between 1.0 and 2.0 s over 325 prompts, 360 sessions sent), each hand-set:
// the page reads them as the views write them on --machines all.
func allPCsOverview(t *testing.T) *metrics.Result {
	t.Helper()
	r := scenarioNamed(t, "healthy")
	r.GeneratedAt = scopeNow
	r.Scope.Machines, r.Scope.PCs = "all", b8PCs(2)
	for _, p := range []scPart{
		scRate("recall.hit_rate", metrics.StatusOK, 98, 325, scCI(0.25, 0.35), scDelta(0.01), scMDC(0.07), scDaily(0.01, 27, 30, 31, 26, 32, 30, 30)),
		scRate("used.turn_rate", metrics.StatusOK, 67, 105, scCI(0.54, 0.72), scDelta(0.03), scMDC(0.18), scTags("judged_28d"), scDaily(0.01, 61, 66, 60, 67, 63, 64, 64)),
		scRate("declare.coverage", metrics.StatusOK, 104, 105),
		scRate("verdict.wrong_rate", metrics.StatusOK, 3, 25, scCI(0.04, 0.30), scMDC(0.20), scDaily(0.01, 0, 8, 0, 13, 0, 12, 12)),
		scValue("context.per_session", metrics.StatusOK, 2300, 311, scTags("status_this_pc", "average"), scDelta(-200), scMDC(700),
			scExtra(map[string]any{"p50": 2100.0, "p95": 3600.0, "average": 2300.0, "tokens_p95": int64(900)}), scDaily(1, 2200, 2400, 2300, 2500, 2300, 2200, 2300)),
		scValue("recall.latency", metrics.StatusOK, 2000, 325, scTags("approximate"), scDelta(0), scMDC(300),
			scExtra(map[string]any{"p50": 840.0, "p95": 1600.0, "range_ms": rangeMS(1000, i64p(2000)), "p50_range_ms": []any{500.0, 1000.0}}),
			scDaily(1000, 1.5, 1.5, 1.5, 1.5, 1.5, 1.5, 1.5)),
		scFlow(metrics.StatusOK, 360, 360, 0, 0, 0, 0, scTags("status_this_pc"), scDelta(0), scDaily(0.01, 100, 100, 100, 100, 100, 100, 100)),
	} {
		m := p.m
		if p.id == "context.per_session" {
			m.Num = scN(715300) // the characters: 311 sessions of 2,300 on average
		}
		r.Metrics[p.id] = m
	}
	r.Summary = scSummary(98, 21, 31000, scF(43.62), true)
	return r
}

// renderAllPCs draws the Overview page template of r (the days calendar of
// days when given), with all PCs' own answer own.
func renderAllPCs(t *testing.T, r, own, days *metrics.Result, p views.Params) *node {
	t.Helper()
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	spec := specs["overview"]
	d := fixtureData(t, spec, false)
	d.V["overview"] = r
	if days != nil {
		d.V["days"] = days
	}
	d.Loc, d.L, d.P, d.Own = r.Window.Loc, en, p, own
	d.Frame.Window, d.Frame.Windows = b8Window, windowsFor("/", url.Values{})
	d.Frame.Scope, d.Frame.Generated = &r.Scope, r.GeneratedAt
	d.CLI = "backsight stats overview"
	var buf bytes.Buffer
	if err := tpls["overview"].ExecuteTemplate(&buf, spec.File, d); err != nil {
		t.Fatal(err)
	}
	return parseHTML(t, buf.String())
}

// b8CardOf is the section that holds the heading id.
func b8CardOf(t *testing.T, doc *node, id string) *node {
	t.Helper()
	h := doc.first("#" + id)
	if h == nil {
		t.Fatalf("no heading %s", id)
	}
	for n := h.Parent; n != nil; n = n.Parent {
		if n.Name == "section" {
			return n
		}
	}
	t.Fatalf("heading %s is in no section", id)
	return nil
}

// A verdict that stays this PC's says so, quotes this PC's numbers and says
// why in its ⓘ; with this PC selected the banner is as shipped.
func TestBannerSaysThisPC(t *testing.T) {
	r, own := allPCsOverview(t), scenarioNamed(t, "healthy")
	doc := renderAllPCs(t, r, own, nil, allPCsPage)
	when := doc.first("section.verdict .v-when")
	if when == nil || when.text() != "(last 7 days, this PC)" {
		t.Errorf("the title's note: %v", when)
	}
	if info := doc.first("section.verdict details.info .pop"); info == nil || !strings.Contains(info.text(), bannerThisPC) {
		t.Errorf("the ⓘ lacks the paragraph: %v", info)
	}
	// The axes quote this PC's 28 %, not the merged 30 % the tiles read.
	if got := doc.first("ul.axes li.axis .axis-note").text(); got != "28 % of prompts got memory" {
		t.Errorf("Found's note %q: the banner must read this PC's numbers", got)
	}
	// Without this PC's own answer the banner falls back to the page's result.
	if got := renderAllPCs(t, r, nil, nil, allPCsPage).first("ul.axes li.axis .axis-note").text(); got != "30 % of prompts got memory" {
		t.Errorf("fallback note %q", got)
	}

	this := renderAllPCs(t, scenarioNamed(t, "healthy"), nil, nil, tilePage)
	// The title quotes this PC's slowest prompts (2.3 s), not the merged range's edge (2.0 s):
	// the verdict is this PC's, and so is every number it says.
	slow := allPCsOverview(t)
	slow.Verdict, slow.Attention = scenarioNamed(t, "latency").Verdict, scenarioNamed(t, "latency").Attention
	slowOwn := scenarioNamed(t, "latency")
	if got := renderAllPCs(t, slow, slowOwn, nil, allPCsPage).first("section.verdict .v-title").text(); !strings.HasPrefix(got, "Cost needs attention: slowest prompts take 2.3 s.") {
		t.Errorf("banner title %q", got)
	}
	if got := summaryText(slow, allPCsPage, parisZone(), "last 7 days", "test", slowOwn); !strings.Contains(got, "\nCost needs attention: slowest prompts take 2.3 s (this PC).\n") {
		t.Errorf("text title:\n%s", got)
	}
	// Nothing to judge yet is still this PC's.
	if b := bannerOf(&metrics.Result{}, en, time.UTC, allPCsPage); b.Title != "Nothing to judge yet." || !b.ThisPC {
		t.Errorf("no verdict: %+v", b)
	}
	if w := this.first("section.verdict .v-when"); w == nil || w.text() != "(last 7 days)" {
		t.Errorf("this PC's title note: %v", w)
	}
	if info := this.first("section.verdict details.info .pop"); info == nil || strings.Contains(info.text(), "verdict stays this PC") {
		t.Errorf("this PC's ⓘ carries the all PCs paragraph: %v", info)
	}
}

// The summary line and the text say whose figures they add up, in B8Parts'
// words, with this PC's verdict and the PCs' notes.
func TestSummaryLineAllPCs(t *testing.T) {
	r, own := allPCsOverview(t), scenarioNamed(t, "healthy")
	doc := renderAllPCs(t, r, own, nil, allPCsPage)
	line := doc.first("p.summary")
	want := "In the last 7 days, on all PCs (4): 98 prompts got memory, Claude used it in 21 turns, about 31 k tokens added; extraction cost $43.62 (server, all PCs)."
	if line == nil || strings.Join(strings.Fields(line.text()), " ") != want {
		t.Errorf("summary line %v, want %q", line, want)
	}

	text := summaryText(r, allPCsPage, parisZone(), "last 7 days", "test", own)
	// The lines wrap at 72 characters; the design's lines are drawn whole.
	flat := strings.NewReplacer("\n"+strings.Repeat(" ", 21), " ", "\n"+strings.Repeat(" ", 7), " ").Replace(text)
	for _, w := range []string{
		"Activity on all PCs on this server (4) · memory on 127.0.0.1:18888\n1 PC sent nothing for 9 days · 1 document ignored\n",
		"Memory is working (this PC).\n",
		"  Found      ok      30 % of prompts got memory (25–35 %), +1 pt, all PCs\n",
		"  Speed      ok      slowest 1 in 20 between 1.0 and 2.0 s (approximate), all PCs\n",
		"  Context    ok      2,300 chars per session on average, all PCs; high value 3,600 on this PC\n",
	} {
		if !strings.Contains(flat, w) {
			t.Errorf("the text lacks %q:\n%s", w, text)
		}
	}
	if got := strings.Join(strings.Fields(text), " "); !strings.Contains(got, want) {
		t.Errorf("the text lacks the summary line %q:\n%s", want, text)
	}
	if strings.Contains(text, "Cost       ok") {
		t.Errorf("Cost's line stays beside Speed and Context:\n%s", text)
	}
	// What the copy button carries is the same text.
	if got := doc.first(".summary-row button.copy").attr("data-copy"); got != text {
		t.Errorf("the copied summary differs from the text:\n%q\n%q", got, text)
	}

	// This PC selected: the lines are as before.
	this := scenarioNamed(t, "healthy")
	plain := summaryText(this, tilePage, parisZone(), "last 7 days", fixtureVersion)
	for _, w := range []string{"Activity on this PC · memory on ", "Memory is working.\n", "  Cost       ok      slowest prompts 1.6 s; context high 3,600 chars\n", "In the last 7 days: 46 prompts"} {
		if !strings.Contains(plain, w) {
			t.Errorf("this PC's text lacks %q:\n%s", w, plain)
		}
	}
	for _, bad := range []string{"Activity on all", "on all PCs", "this PC)"} {
		if strings.Contains(plain, bad) {
			t.Errorf("this PC's text says %q:\n%s", bad, plain)
		}
	}
}

// A project filter that spares the other PCs is a line of the text too.
func TestSummaryTextSaysOtherPCsNotFiltered(t *testing.T) {
	r := allPCsOverview(t)
	r.Scope.OthersUnfiltered = true
	r.Scope.PCs.Stale, r.Scope.PCs.Ignored = 0, 0
	text := summaryText(r, allPCsPage, parisZone(), "last 7 days", fixtureVersion)
	if !strings.Contains(text, "(4) · memory on 127.0.0.1:18888\nother PCs not filtered\n") {
		t.Errorf("no note under the scope line:\n%s", text)
	}
	r.Scope.OthersUnfiltered = false
	if text = summaryText(r, allPCsPage, parisZone(), "last 7 days", fixtureVersion); strings.Contains(text, "other PCs") || strings.Contains(text, "sent nothing") {
		t.Errorf("a note with nothing to say:\n%s", text)
	}
}

// No PC had a prompt in the window: the page says how many PCs it counted,
// when the latest activity was and offers the longer windows.
func TestNoPCActivityState(t *testing.T) {
	r := allPCsOverview(t)
	r.Scope.PCs = b8PCs(0)
	days := &metrics.Result{Scope: r.Scope, Tables: metrics.Tables{Calendar: []metrics.CalendarDay{
		{Day: "2026-10-20", Prompts: 4, Recorded: true}, {Day: "2026-10-21", Prompts: 9, Recorded: true},
		{Day: "2026-10-22", Prompts: 0, Recorded: true}, {Day: "2026-10-30"},
	}}}
	doc := renderAllPCs(t, r, nil, days, allPCsPage)
	empty := noActivityCard(doc)
	if empty == nil {
		t.Fatal("no state")
	}
	if got, want := empty.first("span").text(), "No activity on any of the 4 PCs in the last 7 days. The latest was Oct 21."; got != want {
		t.Errorf("text %q, want %q", got, want)
	}
	var links []string
	for _, a := range empty.find(".seg a") {
		links = append(links, a.text()+" "+a.attr("href"))
	}
	if got := strings.Join(links, ","); got != "30 d /?days=30,90 d /?days=90" {
		t.Errorf("longer windows %q", got)
	}
	if !strings.Contains(empty.text(), "Pick a longer window") {
		t.Errorf("no invitation: %s", empty.text())
	}

	// The states that are not this one.
	state := func(r *metrics.Result, days *metrics.Result, window string) NoData {
		return noPCActivity(r, days, window, windowsFor("/", url.Values{}))
	}
	if n := state(r, days, "last 7 days"); !strings.Contains(n.Text, "The latest was Oct 21.") {
		t.Errorf("direct: %q", n.Text)
	}
	if n := state(r, nil, "last 7 days"); n.Text != "No activity on any of the 4 PCs in the last 7 days." {
		t.Errorf("without a calendar: %q", n.Text)
	}
	if n := state(r, days, "last 90 days"); len(n.Links) != 0 || n.Pick != "" {
		t.Errorf("a 90 d window has no longer one: %+v", n)
	}
	if n := state(r, days, "last 30 days"); len(n.Links) != 1 || n.Links[0].Label != "90 d" {
		t.Errorf("a 30 d window: %+v", n.Links)
	}
	if n := state(r, days, "Oct 1 to Oct 7"); n.Text != "No activity on any of the 4 PCs from Oct 1 to Oct 7. The latest was Oct 21." || len(n.Links) != 0 {
		t.Errorf("custom window: %+v", n)
	}
	active := allPCsOverview(t)
	if n := state(active, days, "last 7 days"); n.Text != "" {
		t.Errorf("PCs with activity: %q", n.Text)
	}
	alone := allPCsOverview(t)
	alone.Scope.PCs = &metrics.PCs{Sharing: true, Count: 1}
	if n := state(alone, days, "last 7 days"); n.Text != "" {
		t.Errorf("one PC: %q", n.Text)
	}
	this := scenarioNamed(t, "healthy")
	this.Scope.PCs = b8PCs(0)
	if n := state(this, days, "last 7 days"); n.Text != "" {
		t.Errorf("this PC selected: %q", n.Text)
	}
	if noActivityCard(renderAllPCs(t, active, nil, days, allPCsPage)) != nil {
		t.Error("the state is drawn for PCs with activity")
	}
}

// The cards that stay this PC's keep their chip and say so in the design's
// one line; the calendar and the tiles carry the all PCs chip; this PC's page
// has none of those lines.
func TestStayingCardsKeepTheirChip(t *testing.T) {
	r, own := allPCsOverview(t), scenarioNamed(t, "healthy")
	days := &metrics.Result{Scope: r.Scope, Tables: metrics.Tables{Calendar: []metrics.CalendarDay{{Day: "2026-10-29", Prompts: 3, PromptsWithMemory: 2, Recorded: true}, {Day: "2026-10-30"}}}}
	doc := renderAllPCs(t, r, own, days, allPCsPage)
	for _, c := range []struct{ id, chip, note string }{
		{"w-attention", "this PC", "Things to do are this PC’s, with all PCs too: each PC lists what it can fix itself."},
		{"w-lately", "this PC", "Sessions and projects never leave the PC they ran on: this list is this PC’s."},
		{"w-glance", "server", "The memory is the server’s: the same with this PC or all PCs."},
		{"w-helped", "this PC", ""},
		{"w-calendar", "all PCs (4)", "The calendar is drawn from the PCs’ daily figures added up. “Colour by sessions” is this PC’s only: other PCs send no sessions, so with all PCs that link shows this PC’s and says so."},
	} {
		card := b8CardOf(t, doc, c.id)
		if chip := card.first(".card-head span.chip"); chip == nil || chip.text() != c.chip {
			t.Errorf("%s: chip %v, want %q", c.id, chip, c.chip)
		}
		if c.note == "" {
			continue
		}
		var notes []string
		for _, p := range card.find("p") {
			notes = append(notes, p.text())
		}
		if !slicesHas(notes, c.note) {
			t.Errorf("%s: no note %q in %q", c.id, c.note, notes)
		}
	}
	if chip := b8CardOf(t, doc, "w-calendar").first("span.chip"); chip == nil || chip.attr("title") != "Added up from the 4 PCs that share their activity on this server" {
		t.Errorf("the calendar chip's title: %v", chip)
	}

	// This PC's page: chips as shipped, none of the lines.
	this := renderAllPCs(t, scenarioNamed(t, "healthy"), nil, nil, tilePage)
	for _, id := range []string{"w-attention", "w-lately", "w-glance", "w-calendar"} {
		card := b8CardOf(t, this, id)
		if txt := card.text(); strings.Contains(txt, "this PC’s") || strings.Contains(txt, "the same with this PC or all PCs") || strings.Contains(txt, "added up") {
			t.Errorf("%s on this PC says: %s", id, txt)
		}
		if chip := card.first(".card-head span.chip"); chip == nil || strings.Contains(chip.text(), "all PCs") {
			t.Errorf("%s on this PC: chip %v", id, chip)
		}
	}
}

func slicesHas(s []string, w string) bool {
	for _, x := range s {
		if x == w {
			return true
		}
	}
	return false
}

// noActivityCard is the state's empty block, nil when the page has none.
func noActivityCard(doc *node) *node {
	for _, c := range doc.find("section.card") {
		if c.attr("aria-label") == "Activity on all PCs" {
			return c.first(".empty")
		}
	}
	return nil
}

// End to end: on a dashboard whose index holds three other PCs, ?machines=all
// draws the merged tiles with this PC's counts in their links (read from this
// PC's own answer), the banner this PC's, and the page asking for this PC is
// free of every mark of all PCs.
func TestOverviewPageOnAllPCs(t *testing.T) {
	h := othersHarness(t)
	hdr := cookieHdr(h.login())
	this := rawPage(t, h, hdr, "/")
	all := rawPage(t, h, hdr, "/?machines=all")

	thisLinks := map[string]string{}
	for _, art := range this.find("article.tile") {
		thisLinks[art.first("h3").text()] = art.first("div.tile-foot a").text()
		if c := art.first("span.chip"); c == nil || c.text() != "this PC" {
			t.Errorf("this PC's %s: chip %v", art.first("h3").text(), c)
		}
	}
	tiles := all.find("article.tile")
	if len(tiles) != 6 {
		t.Fatalf("%d tiles", len(tiles))
	}
	for _, art := range tiles {
		name := art.first("h3").text()
		if c := art.first("span.chip"); c == nil || c.text() != "all PCs (4)" {
			t.Errorf("%s: chip %v", name, c)
		}
		// The link counts this PC's events: the very number this PC's own tile shows.
		want := "This PC’s " + thisLinks[name]
		if name == "Flowing" && strings.Contains(thisLinks[name], "Waiting") {
			want = thisLinks[name]
		}
		if got := art.first("div.tile-foot a").text(); got != want {
			t.Errorf("%s: link %q, want %q", name, got, want)
		}
	}
	if w := all.first("section.verdict .v-when"); w == nil || w.text() != "(last 7 days, this PC)" {
		t.Errorf("banner note %v", w)
	}
	if s := all.first("p.summary"); s == nil || !strings.Contains(s.text(), ", on all PCs (4): ") {
		t.Errorf("summary line %v", s)
	}
	if w := this.first("section.verdict .v-when"); w == nil || w.text() != "(last 7 days)" {
		t.Errorf("this PC's banner note %v", w)
	}
	if s := this.first("p.summary"); s == nil || strings.Contains(s.text(), "all PCs (") {
		t.Errorf("this PC's summary line %v", s)
	}
	for _, mark := range []string{"all PCs (4)", "i-pcs", "this PC’s only", "added up"} {
		if strings.Contains(this.text(), mark) {
			t.Errorf("this PC's page holds %q", mark)
		}
	}

	if this.first("span.approx") != nil || this.first("span.chip-nf") != nil {
		t.Error("this PC's page holds a range or an other-PCs chip")
	}

	// Sharing off: a kept all PCs link answers this PC's, with none of the marks.
	off := newDataHarness(t, withIndex(), withServer(), withDoctor(), func(_ *harness, c *Config) { c.Source.Config.Feedback.ShareLedger = false })
	doc := rawPage(t, off, cookieHdr(off.login()), "/?machines=all")
	if w := doc.first("section.verdict .v-when"); w == nil || w.text() != "(last 7 days)" {
		t.Errorf("sharing off: banner note %v", w)
	}
	for _, art := range doc.find("article.tile") {
		if c := art.first("span.chip"); c == nil || c.text() != "this PC" {
			t.Errorf("sharing off, %s: chip %v", art.first("h3").text(), c)
		}
	}
}

// The calendar coloured by sessions says that figure is this PC's: other PCs
// send none.
func TestCalendarBySessionsSaysThisPC(t *testing.T) {
	r := allPCsOverview(t)
	days := &metrics.Result{Scope: r.Scope, Tables: metrics.Tables{Calendar: []metrics.CalendarDay{{Day: "2026-10-29", Prompts: 3, Sessions: 2, Recorded: true}, {Day: "2026-10-30"}}}}
	win := metrics.Window{Loc: time.UTC}
	by := func(res *metrics.Result, hm string) string {
		return string(calendarOf(res, win, allPCsPage, hm, "/", b8Window).Chart)
	}
	if got := by(days, "sessions"); !strings.Contains(got, "sessions on this PC") || !strings.Contains(got, "Sessions, this PC") {
		t.Errorf("by sessions on all PCs: %.400s", got)
	}
	if got := by(days, "got"); strings.Contains(got, "this PC") {
		t.Errorf("by prompts says this PC: %.400s", got)
	}
	this := &metrics.Result{Scope: metrics.Scope{Machines: "this"}, Tables: days.Tables}
	if got := by(this, "sessions"); strings.Contains(got, "this PC") {
		t.Errorf("this PC's calendar: %.400s", got)
	}
}
