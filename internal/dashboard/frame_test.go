package dashboard

import (
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// rawPage is path's body unmodified (no entity decoding), parsed.
func rawPage(t *testing.T, h *harness, hdr []string, path string) *node {
	t.Helper()
	r := h.do("GET", path, hdr...)
	if r.StatusCode != 200 {
		t.Fatalf("GET %s: %d", path, r.StatusCode)
	}
	return parseHTML(t, body(t, r))
}

// linksIn are the hrefs of the links in sel, in order.
func linksIn(n *node, sel string) []string {
	var out []string
	for _, a := range n.find(sel + " a") {
		out = append(out, a.attr("href"))
	}
	return out
}

// current is the text of the links in sel marked aria-current=want.
func current(n *node, sel, want string) []string {
	var out []string
	for _, a := range n.find(sel + " a") {
		if a.attr("aria-current") == want {
			out = append(out, a.text())
		}
	}
	return out
}

func TestFrameLinks(t *testing.T) {
	h := dataHarness(t)
	doc := rawPage(t, h, cookieHdr(h.login()), "/quality?days=30&project=p1")
	want := []string{"/quality?days=1&project=p1", "/quality?days=7&project=p1", "/quality?days=30&project=p1", "/quality?days=90&project=p1"}
	if got := linksIn(doc.first("nav.seg"), ""); !slices.Equal(got, want) {
		t.Errorf("window links %q, want %q", got, want)
	}
	if got := current(doc.first("nav.seg"), "", "true"); !slices.Equal(got, []string{"30 d"}) {
		t.Errorf("current window %q, want 30 d", got)
	}
	if got := current(doc.first("details.navmenu nav.seg"), "", "true"); !slices.Equal(got, []string{"30 d"}) {
		t.Errorf("current window in the Menu %q, want 30 d", got)
	}
	if a := doc.first("header a.refresh-t"); a == nil || a.attr("href") != "/quality?days=30&project=p1" || a.text() != "Refresh" {
		t.Errorf("refresh link %+v", a)
	}
	if got := linksIn(doc, "header details.proj"); !slices.Contains(got, "/quality?days=30") || !slices.Contains(got, "/projects") {
		t.Errorf("project menu links %q lack the unfiltered page and the list", got)
	}
	if !slices.Contains(linksIn(doc, "header"), "/prefs?theme=dark&back=%2Fquality%3Fdays%3D30%26project%3Dp1") {
		t.Errorf("no dark-theme link back to the page: %q", linksIn(doc, "header"))
	}
	f := doc.first("details.proj form")
	if f == nil || f.attr("method") != "get" || f.attr("action") != "/quality" {
		t.Fatalf("project form %+v", f)
	}
	if in := f.first("input[name=project]"); in == nil || in.attr("value") != "p1" || in.attr("type") != "text" {
		t.Errorf("project input %+v", in)
	}
	if in := f.first("input[name=days]"); in == nil || in.attr("value") != "30" || in.attr("type") != "hidden" {
		t.Errorf("days input %+v", in)
	}
	for _, s := range doc.find("summary") {
		if strings.HasPrefix(s.text(), "Developer") {
			t.Error("Developer menu without the preference")
		}
	}
}

func TestFrameCustomWindowIsKeptOnRefresh(t *testing.T) {
	h := dataHarness(t)
	doc := rawPage(t, h, cookieHdr(h.login()), "/quality?since=2026-10-01&until=2026-10-08")
	if a := doc.first("header a.refresh-t"); a == nil || a.attr("href") != "/quality?since=2026-10-01&until=2026-10-08" {
		t.Errorf("refresh link %+v", a)
	}
	if got := current(doc.first("nav.seg"), "", "true"); len(got) != 0 {
		t.Errorf("a custom window marks %q", got)
	}
}

func TestReflectedParamsAreEscaped(t *testing.T) {
	h := dataHarness(t)
	r := h.do("GET", "/quality?project=%3Cscript%3Ex%3C%2Fscript%3E", cookieHdr(h.login())...)
	b := body(t, r)
	if r.StatusCode != 200 || !strings.Contains(b, "&lt;script&gt;x") || strings.Contains(b, "<script>x") {
		t.Errorf("%d %s", r.StatusCode, b)
	}
}

func TestDefaultWindowIsMarked(t *testing.T) {
	h := dataHarness(t)
	// A repeated project is refused with a state page, which keeps the frame.
	doc := parseHTML(t, body(t, h.do("GET", "/quality?tz=UTC&project=a&project=b", cookieHdr(h.login())...)))
	if got := current(doc.first("nav.seg"), "", "true"); !slices.Equal(got, []string{"7 d"}) {
		t.Errorf("current window %q, want only 7 d", got)
	}
	if got := linksIn(doc.first("nav.seg"), "")[0]; got != "/quality?days=1&tz=UTC&project=a" {
		t.Errorf("24 h link %q", got)
	}
	if in := doc.first("details.proj input[name=project]"); in == nil || in.attr("value") != "a" {
		t.Errorf("project input %+v", in)
	}
}

func TestReflectedQuoteIsEscapedInAttributes(t *testing.T) {
	h := dataHarness(t)
	r := h.do("GET", "/quality?project=%22%3E%3Cb%3Ex", cookieHdr(h.login())...)
	b := body(t, r)
	if r.StatusCode != 200 || strings.Contains(b, `"><b>x`) || strings.Contains(b, "<b>x") || !strings.Contains(b, `value="&#34;&gt;&lt;b&gt;x"`) {
		t.Errorf("%d %s", r.StatusCode, b)
	}
}

func TestTopBarStructure(t *testing.T) {
	h := dataHarness(t)
	doc := rawPage(t, h, cookieHdr(h.login()), "/")
	// The frame's parts, in document order.
	last := 0
	for _, sel := range []string{"a.skip[href=#main]", "header.topbar", "nav[aria-label=Pages]", "nav[aria-label=Window]", "details.proj", "details.navmenu", "main#main", "footer.foot"} {
		n := doc.first(sel)
		if n == nil {
			t.Errorf("no %s", sel)
			continue
		}
		if n.line <= last {
			t.Errorf("%s at line %d, not after line %d", sel, n.line, last)
		}
		last = n.line
	}
	if s := doc.first("details.navmenu summary"); s == nil || s.text() != "Menu" {
		t.Errorf("Menu summary %+v", s)
	}
	for _, c := range doc.find("span.caret") {
		if c.attr("aria-hidden") != "true" {
			t.Errorf("caret without aria-hidden: %+v", c)
		}
	}
	if logos := doc.find("a.brand img"); len(logos) != 2 || logos[0].attr("alt") != "backsight" || logos[0].attr("width") != "24" {
		t.Errorf("brand logos %+v", logos)
	}
	for _, d := range doc.find("details summary") {
		if strings.Contains(d.text(), "Developer") {
			t.Error("Developer menu without the preference")
		}
	}
	if hs := doc.find("h1"); len(hs) != 1 || hs[0].Parent.attr("class") != "page-head" || hs[0].text() != "Is my memory working?" {
		t.Errorf("headings %+v", hs)
	}

	dev := rawPage(t, h, withPrefs(h.login(), "v1.system.1.0"), "/")
	if s := dev.find("nav.nav details summary"); len(s) != 2 || !strings.HasPrefix(s[1].text(), "Developer") {
		t.Errorf("developer mode menus %+v", s)
	}

	q := rawPage(t, h, cookieHdr(h.login()), "/quality?days=30")
	if got := current(q.first("nav.seg"), "", "true"); !slices.Equal(got, []string{"30 d"}) {
		t.Errorf("window marked %q", got)
	}
	head := q.first("div.page-head")
	if head == nil || head.first("h1").text() != "Is it right?" || head.first("span.muted").text() != "window: last 30 days" {
		t.Errorf("page head %+v", head)
	}
	if n := rawPage(t, h, cookieHdr(h.login()), "/quality?days=1").first("div.page-head span.muted"); n == nil || n.text() != "window: last 24 h" {
		t.Errorf("one-day window %+v", n)
	}
}

func TestStatePagesHaveNoWindowPhrase(t *testing.T) {
	h := dataHarness(t)
	r := h.do("GET", "/quality?days=0", cookieHdr(h.login())...)
	doc := parseHTML(t, body(t, r))
	if r.StatusCode != 400 || doc.first("div.page-head h1") == nil || doc.first("div.page-head span") != nil {
		t.Errorf("state page %d: head %+v", r.StatusCode, doc.first("div.page-head"))
	}
}

func TestMenuHoldsEveryPage(t *testing.T) {
	h := dataHarness(t)
	base := []string{"/", "/quality", "/cost", "/bank", "/pipeline", "/projects", "/sessions", "/memories", "/system",
		"/quality?days=1", "/quality?days=7", "/quality?days=30", "/quality?days=90"}
	got := linksIn(rawPage(t, h, cookieHdr(h.login()), "/quality"), "details.navmenu")
	for _, w := range base {
		if !slices.Contains(got, w) {
			t.Errorf("Menu lacks %s in %q", w, got)
		}
	}
	for _, w := range []string{"/releases", "/defs"} {
		if slices.Contains(got, w) {
			t.Errorf("Menu has %s without developer mode", w)
		}
	}
	got = linksIn(rawPage(t, h, withPrefs(h.login(), "v1.system.1.0"), "/quality"), "details.navmenu")
	for _, w := range []string{"/releases", "/defs"} {
		if !slices.Contains(got, w) {
			t.Errorf("Menu lacks %s in developer mode: %q", w, got)
		}
	}
}

func TestProjectMenuKeepsTheForm(t *testing.T) {
	h := dataHarness(t)
	doc := rawPage(t, h, cookieHdr(h.login()), "/quality")
	menu := doc.first("details.proj")
	if f := menu.first("form[method=get]"); f == nil || f.first("input[name=project]") == nil || f.first("button").text() != "Filter" {
		t.Errorf("project form %+v", f)
	}
	if !strings.Contains(menu.text(), "All projects") || menu.first("a[href=/projects]") == nil || menu.first("summary span.trunc").text() != "All projects" {
		t.Errorf("project menu %q", menu.text())
	}
	if got := current(menu, "", "true"); !slices.Equal(got, []string{"✓All projects"}) {
		t.Errorf("unfiltered current %q", got)
	}
	doc = rawPage(t, h, cookieHdr(h.login()), "/quality?days=30&project=p1")
	menu = doc.first("details.proj")
	if in := menu.first("input[name=project]"); in.attr("value") != "p1" {
		t.Errorf("input %+v", in)
	}
	if in := menu.first("input[name=days]"); in == nil || in.attr("type") != "hidden" || in.attr("value") != "30" {
		t.Errorf("hidden days %+v", in)
	}
	if s := menu.first("summary span.trunc").text(); s != "p1" {
		t.Errorf("summary %q", s)
	}
	long := rawPage(t, h, cookieHdr(h.login()), "/quality?project="+strings.Repeat("x", 30)).first("details.proj summary span.trunc")
	if long.text() != strings.Repeat("x", 23)+"…" || long.attr("title") != strings.Repeat("x", 30) {
		t.Errorf("long name %q title %q", long.text(), long.attr("title"))
	}
}

func TestPageTitles(t *testing.T) {
	for _, c := range []struct {
		spec pageSpec
		id   string
		want string
	}{
		{pageSpec{Name: "quality", Views: []string{"quality"}}, "", "Is it right?"},
		{pageSpec{Name: "day", Views: []string{"days"}}, "2026-10-29", "What happened on Thursday, Oct 29?"},
		{pageSpec{Name: "day", Views: []string{"days"}}, "x", "What happened that day?"},
		{pageSpec{Name: "session", Views: []string{"sessions"}}, "sess-a1b2c3d4e5", "What happened in session sess-a1b?"},
		{pageSpec{Name: "memory", Views: []string{"memories"}}, "0b4c0013-0000", "How has memory 0b4c0013 done?"},
	} {
		if got := pageTitle(c.spec, c.id); got != c.want {
			t.Errorf("pageTitle(%s, %q) = %q, want %q", c.spec.Name, c.id, got, c.want)
		}
	}
	if got := dayTitle("2026-10-29"); got != "What happened on Thursday, Oct 29?" {
		t.Errorf("dayTitle = %q", got)
	}
	paris := parisZone()
	w, err := metrics.NewWindow(time.Date(2026, 10, 30, 12, 0, 0, 0, paris), 0,
		time.Date(2026, 10, 1, 0, 0, 0, 0, paris), time.Date(2026, 10, 8, 0, 0, 0, 0, paris), paris, "")
	if err != nil {
		t.Fatal(err)
	}
	p := views.Params{Since: "2026-10-01", Until: "2026-10-08"}
	if got := windowPhrase(p, w); got != "Oct 1 to Oct 7" {
		t.Errorf("custom window %q", got)
	}
	if got := windowPhrase(p, metrics.Window{}); got != "Oct 1 to Oct 7" {
		t.Errorf("custom window without a resolved one %q", got)
	}
	// A custom window of one day is a date, whatever days says.
	one, err := metrics.NewWindow(time.Date(2026, 10, 30, 12, 0, 0, 0, paris), 0,
		time.Date(2026, 10, 1, 0, 0, 0, 0, paris), time.Date(2026, 10, 2, 0, 0, 0, 0, paris), paris, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := windowPhrase(views.Params{Days: 1, Since: "2026-10-01", Until: "2026-10-02"}, one); got != "Oct 1 to Oct 1" {
		t.Errorf("custom one-day window %q", got)
	}
	for days, want := range map[int]string{1: "last 24 h", 7: "last 7 days", 90: "last 90 days"} {
		if got := windowPhrase(views.Params{Days: days}, metrics.Window{Days: days}); got != want {
			t.Errorf("%d days: %q, want %q", days, got, want)
		}
	}
}

func TestMenuRefreshProjectAndPreferencesHint(t *testing.T) {
	h := dataHarness(t)
	ck := cookieHdr(h.login())
	menuLink := func(doc *node, text string) []string {
		var hrefs []string
		for _, a := range doc.find("details.navmenu a") {
			if strings.HasPrefix(a.text(), text) {
				hrefs = append(hrefs, a.attr("href"))
			}
		}
		return hrefs
	}
	doc := rawPage(t, h, ck, "/quality?days=30&project=p1")
	if got := menuLink(doc, "Refresh"); !slices.Equal(got, []string{"/quality?days=30&project=p1"}) {
		t.Errorf("Menu Refresh links %q; want the page itself", got)
	}
	if got := menuLink(doc, "p1"); !slices.Equal(got, []string{"/projects"}) {
		t.Errorf("Menu Project row %q; want the project list", got)
	}
	if got := menuLink(rawPage(t, h, ck, "/quality"), "All projects"); !slices.Equal(got, []string{"/projects"}) {
		t.Errorf("unfiltered Menu Project row %q", got)
	}
	hints := doc.find("span.hint")
	var got []string
	for _, h := range hints {
		got = append(got, h.text())
	}
	want := []string{"Adds ranking, learned ranking and release detail.",
		"Compact fits more on a screen; touch targets keep their size.",
		"g then a letter opens a page; press ? to list them all."}
	if !slices.Equal(got, want) {
		t.Errorf("preference hints %q", got)
	}
}

// scopeFx is one state of the scope line, drawn on the Overview from a
// fixture frame: the request's machines, the view's scope, the server's answer.
type scopeFx struct {
	path     string // the page's request path and query ("/")
	machines string // the URL's machines, "" this PC
	scope    metrics.Scope
	read     *metrics.ServerState
}

var scopeNow = time.Date(2026, 10, 30, 15, 42, 0, 0, time.UTC)

func ptrS(s string) *string { return &s }

// fourPCs is this PC and three others sharing, one of which has sent
// nothing for 9 days (it sent on Oct 21: its figures stop on Oct 20).
func fourPCs() *metrics.PCs {
	fetched := time.Date(2026, 10, 30, 13, 42, 0, 0, time.UTC)
	return &metrics.PCs{Sharing: true, Count: 4, Active: 3, Stale: 1, Ignored: 1, FetchedAt: &fetched, Until: ptrS("2026-10-20"), List: []metrics.PCRow{
		{Machine: "3f9c2a17", This: true, OS: "linux", FirstSeen: "2026-09-02", Until: ptrS("2026-10-29")},
		{Machine: "a07be4d1", OS: "darwin", FirstSeen: "2026-10-10", Until: ptrS("2026-10-29")},
		{Machine: "5d21c8e0", OS: "windows", FirstSeen: "2026-10-28", Until: ptrS("2026-10-29")},
		{Machine: "c84e1f39", OS: "linux", FirstSeen: "2026-07-18", Until: ptrS("2026-10-20"), Stale: true},
	}}
}

// scopeBar draws c and returns the scope line's node and its raw HTML.
func scopeBar(t *testing.T, c scopeFx) (*node, string) {
	t.Helper()
	set, err := newPageSet()
	if err != nil {
		t.Fatal(err)
	}
	spec := set.specs["overview"]
	d := fixtureData(t, spec, false)
	d.Loc, d.L = time.UTC, en
	path := cmpOr(c.path, "/")
	sc := c.scope
	d.Frame.Path, d.Frame.Scope, d.Frame.Generated, d.Frame.ServerRead = path, &sc, scopeNow, c.read
	d.Frame.Machines = c.machines
	d.Frame.RenderedAt = scopeNow
	d.Frame.Switch = switchOf(d)
	body, err := renderPage(set, "overview", d)
	if err != nil {
		t.Fatal(err)
	}
	body = keepMachines(body, c.machines)
	raw := string(body)
	i := strings.Index(raw, `<div class="scopebar">`)
	j := strings.Index(raw, `<main id="main"`)
	if i < 0 || j < i {
		t.Fatalf("no scope line in %.300s", raw)
	}
	return parseHTML(t, raw), raw[i:j]
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func TestScopeSwitchStates(t *testing.T) {
	const addr = "192.168.1.20:18888"
	base := func(m string, pcs *metrics.PCs) metrics.Scope {
		return metrics.Scope{Machines: m, Server: addr, ServerOK: true, PCs: pcs}
	}
	cases := []struct {
		name  string
		fx    scopeFx
		line  string   // the switch's two halves
		links []string // the switch's hrefs, in order
		cur   []string // the links marked aria-current
		chips []string // the notes after the line, by text
		which []string // the rows of "Which PCs"; nil: no list
	}{
		{name: "this PC with others", fx: scopeFx{scope: base("this", fourPCs())},
			line: "this PC all PCs on this server (4)", links: []string{"/", "/?machines=all"}, cur: []string{"this PC"}},
		{name: "all PCs", fx: scopeFx{machines: "all", scope: base("all", fourPCs())},
			line: "this PC all PCs on this server (4)", links: []string{"/", "/?machines=all", "/system?machines=all#machines"},
			cur:   []string{"all PCs on this server (4)"},
			chips: []string{"1 PC: nothing sent for 9 days", "1 document ignored"},
			which: []string{"3f9c2a17 Linux · since Sep 2 · this PC its own figures, live",
				"a07be4d1 macOS · since Oct 10 figures up to Oct 29",
				"5d21c8e0 Windows · since Oct 28 figures up to Oct 29",
				"c84e1f39 Linux · since Jul 18 nothing sent for 9 days: its figures stop on Oct 20"}},
		{name: "all PCs with a project filter", fx: scopeFx{path: "/?project=backsight", machines: "all", scope: func() metrics.Scope {
			s := base("all", fourPCs())
			s.Project, s.OthersUnfiltered = "backsight", true
			return s
		}()},
			line: "this PC all PCs on this server (4)", links: []string{"/?project=backsight", "/?project=backsight&machines=all", "/system?machines=all#machines"},
			cur:   []string{"all PCs on this server (4)"},
			chips: []string{"other PCs: not filtered", "1 PC: nothing sent for 9 days", "1 document ignored"},
			which: []string{"3f9c2a17 Linux · since Sep 2 · this PC its own figures, live",
				"a07be4d1 macOS · since Oct 10 figures up to Oct 29",
				"5d21c8e0 Windows · since Oct 28 figures up to Oct 29",
				"c84e1f39 Linux · since Jul 18 nothing sent for 9 days: its figures stop on Oct 20"}},
		{name: "sharing off", fx: scopeFx{scope: base("this", &metrics.PCs{List: []metrics.PCRow{}})},
			line: "this PC all PCs: sharing is off here", links: []string{"/", "/system#machines"}, cur: []string{"this PC"}},
		{name: "sharing off behind a kept link", fx: scopeFx{machines: "all", scope: base("this", &metrics.PCs{List: []metrics.PCRow{}})},
			line: "this PC all PCs: sharing is off here", links: []string{"/", "/system?machines=all#machines"}, cur: []string{"this PC"}},
		{name: "server not answering", fx: scopeFx{machines: "all", scope: base("all", func() *metrics.PCs {
			p := fourPCs()
			f := time.Date(2026, 10, 30, 7, 40, 0, 0, time.UTC)
			p.FetchedAt = &f
			return p
		}()), read: &metrics.ServerState{OK: false, LastOK: func() *time.Time { x := time.Date(2026, 10, 30, 9, 12, 0, 0, time.UTC); return &x }()}},
			line: "this PC all PCs on this server (4)", links: []string{"/", "/?machines=all", "/system?machines=all#machines"},
			cur:   []string{"all PCs on this server (4)"},
			chips: []string{"1 PC: nothing sent for 9 days", "1 document ignored", "other PCs as fetched at 07:40"},
			which: []string{"3f9c2a17 Linux · since Sep 2 · this PC its own figures, live",
				"a07be4d1 macOS · since Oct 10 figures up to Oct 29",
				"5d21c8e0 Windows · since Oct 28 figures up to Oct 29",
				"c84e1f39 Linux · since Jul 18 nothing sent for 9 days: its figures stop on Oct 20"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, raw := scopeBar(t, c.fx)
			bar := doc.first(".scopebar")
			sw := bar.first(".mscope")
			if sw == nil {
				t.Fatalf("no switch in %s", raw)
			}
			// The line sits in the scope line under the top bar, which stays at
			// 900 px: the top bar's tools fold into the Menu, the scope line does not.
			if len(doc.find("header .mscope")) != 0 || bar.first(".wrap.scope .mscope") == nil {
				t.Errorf("the switch is not in the scope line, outside the top bar: %s", raw)
			}
			if got := sw.first("nav.mseg").text(); got != c.line {
				t.Errorf("switch %q, want %q", got, c.line)
			}
			if got := linksIn(sw, ""); !slices.Equal(got, c.links) {
				t.Errorf("links %q, want %q", got, c.links)
			}
			if got := current(sw, "nav.mseg", "true"); !slices.Equal(got, c.cur) {
				t.Errorf("aria-current %q, want %q", got, c.cur)
			}
			var chips []string
			for _, n := range bar.find(".wrap.scope > .chip") {
				if txt := n.text(); !strings.HasPrefix(txt, "Updated over") && txt != "" {
					chips = append(chips, txt)
				}
			}
			for _, n := range bar.find(".wrap.scope > a") {
				if txt := n.text(); !strings.HasPrefix(txt, "Updated over") && !slices.Contains(chips, txt) {
					chips = append(chips, txt)
				}
			}
			slices.Sort(chips)
			want := slices.Clone(c.chips)
			slices.Sort(want)
			if !slices.Equal(chips, want) {
				t.Errorf("notes %q, want %q", chips, want)
			}
			var rows []string
			for _, li := range sw.find("details.pcs li") {
				rows = append(rows, li.text())
			}
			if !slices.Equal(rows, c.which) {
				t.Errorf("Which PCs %q, want %q", rows, c.which)
			}
			if (len(c.which) > 0) != (sw.first("details.pcs") != nil) {
				t.Errorf("the list is shown: %t, want %t", sw.first("details.pcs") != nil, len(c.which) > 0)
			}
			if off := sw.first("a.seg-off"); (off != nil) != strings.Contains(c.line, "sharing is off") {
				t.Errorf("seg-off %v for %q", off, c.line)
			} else if off != nil && off.attr("aria-current") != "" {
				t.Errorf("the sharing-off half is marked current")
			}
		})
	}

	t.Run("the stale PC is amber and says the word, the others do not", func(t *testing.T) {
		doc, _ := scopeBar(t, scopeFx{machines: "all", scope: base("all", fourPCs())})
		var stale []string
		for _, li := range doc.find("details.pcs li") {
			if st := li.first(".pcl-st"); strings.Contains(st.attr("class"), "st-watch") {
				stale = append(stale, li.first(".idc").text())
			}
		}
		if !slices.Equal(stale, []string{"c84e1f39"}) {
			t.Errorf("rows in watch %q, want only c84e1f39", stale)
		}
		if n := doc.find("details.pcs .pcl-note"); len(n) != 3 || n[0].text() != "1 document did not pass the checks and is left out. What it holds is not shown." ||
			n[2].text() != "Other PCs’ figures were fetched 2 h ago; they are fetched every 6 h." {
			t.Errorf("notes %v", n)
		}
	})
	t.Run("at 900 px the switch takes its own line and keeps its touch size", func(t *testing.T) {
		css := dashboardCSS(t)
		i := strings.Index(css, "@container page (max-width: 899px) {\n  .mscope")
		if i < 0 {
			t.Fatal("no narrow rule for the switch")
		}
		block := css[i : i+strings.Index(css[i:], "\n}")]
		for _, want := range []string{".mscope { flex-basis: 100%; }", ".scope .mseg a { min-height: var(--touch); }", ".scope details.pcs { flex-basis: 100%; }"} {
			if !strings.Contains(block, want) {
				t.Errorf("the narrow block lacks %q", want)
			}
		}
	})
}

// With no other PC sharing the line is the one block 5 shipped, byte for
// byte; a link that asks for all PCs still reads "(1)".
func TestNoSwitchWithoutOtherPCs(t *testing.T) {
	one := &metrics.PCs{Sharing: true, Count: 1, Active: 1, List: []metrics.PCRow{{Machine: "3f9c2a17", This: true, OS: "linux"}}}
	sc := metrics.Scope{Machines: "this", Server: "192.168.1.20:18888", ServerOK: true, PCs: one}
	_, raw := scopeBar(t, scopeFx{scope: sc})
	const shipped = `<div class="scopebar"><div class="wrap scope">
<span>Activity on <b>this PC</b></span>
<span class="dot">memory on <b>192.168.1.20:18888</b></span>
<span class="dot">updated <time datetime="2026-10-30T15:42:00Z" title="2026-10-30 15:42 UTC">just now</time></span>
<a class="chip chip-stale" href="/"><span class="i i-sm i-clock"></span>Updated over 5 min ago. Refresh.</a>
</div></div>
`
	if raw != shipped {
		t.Errorf("scope line\n%s\nwant\n%s", raw, shipped)
	}
	// Nothing on the page says "all PCs" either.
	if strings.Contains(raw, "machines") || strings.Contains(raw, "all PCs") {
		t.Errorf("the line mentions all PCs: %s", raw)
	}

	// A kept link: "(1)", the figures are this PC's and the list says so.
	sc.Machines = "all"
	doc, _ := scopeBar(t, scopeFx{machines: "all", scope: sc})
	sw := doc.first(".mscope")
	if sw == nil || sw.first("nav.mseg").text() != "this PC all PCs on this server (1)" {
		t.Fatalf("?machines=all: %v", sw)
	}
	if got := current(sw, "nav.mseg", "true"); !slices.Equal(got, []string{"all PCs on this server (1)"}) {
		t.Errorf("current %q", got)
	}
	// No server set up and sharing off: the line is the shipped one too, since
	// there is no server whose PCs could be counted.
	off := metrics.Scope{Machines: "this", PCs: &metrics.PCs{List: []metrics.PCRow{}}}
	if doc, _ := scopeBar(t, scopeFx{scope: off}); doc.first(".mscope") != nil {
		t.Error("a switch with no memory server and sharing off")
	}
}

// The scope line names the address as "<address>, this PC" when the server
// is on this machine, and only with the switch shown.
func TestHostScopeLine(t *testing.T) {
	for _, c := range []struct {
		name, server string
		pcs          *metrics.PCs
		want         string
	}{
		{"loopback with the switch", "127.0.0.1:8888", fourPCs(), "memory on 127.0.0.1:8888, this PC"},
		{"IPv6 loopback", "[::1]:8888", fourPCs(), "memory on [::1]:8888, this PC"},
		{"localhost", "localhost:8888", fourPCs(), "memory on localhost:8888, this PC"},
		{"a remote server", "192.168.1.20:8888", fourPCs(), "memory on 192.168.1.20:8888"},
		{"loopback without the switch", "127.0.0.1:8888", &metrics.PCs{Sharing: true, Count: 1, List: []metrics.PCRow{{This: true}}}, "memory on 127.0.0.1:8888"},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc, raw := scopeBar(t, scopeFx{scope: metrics.Scope{Machines: "this", Server: c.server, ServerOK: true, PCs: c.pcs}})
			var got string
			for _, d := range doc.find(".scope .dot") {
				if strings.HasPrefix(d.text(), "memory on") {
					got = d.text()
				}
			}
			if got != c.want {
				t.Errorf("%q, want %q in %s", got, c.want, raw)
			}
		})
	}
	// "holds the memory server" in the list, on the host only.
	doc, _ := scopeBar(t, scopeFx{machines: "all", scope: metrics.Scope{Machines: "all", Server: "127.0.0.1:8888", ServerOK: true, PCs: fourPCs()}})
	if li := doc.first("details.pcs li"); li == nil || li.text() != "3f9c2a17 Linux · since Sep 2 · this PC, holds the memory server its own figures, live" {
		t.Errorf("host row %v", li)
	}
}

// othersHarness is a dashboard whose index holds three other PCs' figures,
// one of them stale, and one ignored document (4 PCs counted, this one included).
func othersHarness(t *testing.T) *harness {
	t.Helper()
	old := dataSpec
	dataSpec.OtherPCs = 3
	t.Cleanup(func() { dataSpec = old })
	return dataHarness(t)
}

// cliOf maps each card or tile of a page to the command its CLI disclosure
// shows, by the id of its heading.
func cliOf(doc *node) map[string]string {
	out := map[string]string{}
	for _, c := range doc.find("details.cli") {
		id := ""
		for n := c.Parent; n != nil && id == ""; n = n.Parent {
			if h := n.first("h2"); h != nil && h.attr("id") != "" {
				id = h.attr("id")
			} else if h := n.first("h3"); h != nil && h.attr("id") != "" {
				id = h.attr("id")
			}
		}
		out[id] = c.first("code").text()
	}
	return out
}

// The command a widget shows carries --machines all only when the widget adds
// the PCs up: the six tiles' commands over the views that do, the Days
// calendar, the merged cards. "What should I do?", Lately, Helped most, every
// list and every server widget stay this PC's or the server's, and never do.
func TestCLIDisclosureFollowsTheSwitch(t *testing.T) {
	h := othersHarness(t)
	hdr := cookieHdr(h.login())
	follow := map[string][]string{
		"/":         {"w-recall-hit_rate", "w-context-per_session", "w-recall-latency", "w-pipeline-flow", "w-calendar"},
		"/quality":  {"w-used-turn_rate", "w-recall-hit_rate"},
		"/cost":     {"w-context-per_session", "w-recall-latency"},
		"/pipeline": {"w-pipeline-flow"},
		"/bank":     nil, "/system": nil, "/sessions": nil, "/projects": nil, "/memories": nil,
		"/days": {"w-calendar"}, "/releases": {"w-releases"},
	}
	for path, want := range follow {
		doc := rawPage(t, h, hdr, path+"?machines=all")
		var got []string
		for id, cmd := range cliOf(doc) {
			if strings.Contains(cmd, "--machines") {
				got = append(got, id)
				if !strings.HasSuffix(cmd, " --machines all") {
					t.Errorf("%s %s: %q does not end with --machines all", path, id, cmd)
				}
			}
		}
		slices.Sort(got)
		w := slices.Clone(want)
		slices.Sort(w)
		if !slices.Equal(got, w) {
			t.Errorf("%s with all: widgets with the flag %q, want %q", path, got, w)
		}
	}
	// The ones the design names, one by one, with the command they show.
	over := cliOf(rawPage(t, h, hdr, "/?machines=all"))
	for id, want := range map[string]string{
		"w-recall-hit_rate": "backsight stats quality --machines all", // Found
		"w-attention":       "backsight stats overview",               // What should I do?
		"w-lately":          "backsight stats sessions --limit 5",
		"w-helped":          "backsight stats overview",
		"w-glance":          "backsight stats bank",
		"w-verdict":         "backsight stats overview --days 7",
	} {
		if over[id] != want {
			t.Errorf("/ %s: %q, want %q", id, over[id], want)
		}
	}
	pipe := cliOf(rawPage(t, h, hdr, "/pipeline?machines=all"))
	for _, id := range []string{"w-server-queue", "w-server-extraction_delay"} {
		if c, ok := pipe[id]; !ok || strings.Contains(c, "--machines") {
			t.Errorf("/pipeline server widget %s: %q (present %t)", id, c, ok)
		}
	}
	// With this PC selected, whatever the way it is asked, none does.
	for path := range follow {
		for _, q := range []string{"", "?machines=this", "?days=30&project=backsight"} {
			for id, cmd := range cliOf(rawPage(t, h, hdr, path+q)) {
				if strings.Contains(cmd, "--machines") {
					t.Errorf("%s%s %s: %q carries the flag with this PC selected", path, q, id, cmd)
				}
			}
		}
	}
	// Sharing off: a kept link answers this PC's figures, and the commands say so.
	off := newDataHarness(t, withIndex(), withServer(), withDoctor(), func(_ *harness, c *Config) { c.Source.Config.Feedback.ShareLedger = false })
	doc := rawPage(t, off, cookieHdr(off.login()), "/?machines=all")
	for id, cmd := range cliOf(doc) {
		if strings.Contains(cmd, "--machines") {
			t.Errorf("sharing off, %s: %q carries the flag", id, cmd)
		}
	}
	if sw := doc.first(".scopebar .mscope a.seg-off"); sw == nil || sw.attr("href") != "/system?machines=all#machines" {
		t.Errorf("sharing off behind a kept link: second half %v", sw)
	}
}

// internalLinks are the hrefs of n that stay in the dashboard and that the
// parameter must follow: not the preferences (which carry the page to come
// back to), not the assets, not an anchor on the page.
func internalLinks(n *node) []*node {
	var out []*node
	for _, a := range n.find("a") {
		href := a.attr("href")
		if !strings.HasPrefix(href, "/") || strings.HasPrefix(href, "//") || strings.HasPrefix(href, "/prefs") || strings.HasPrefix(href, "/assets/") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// On every page asked with machines=all, every link inside the dashboard
// carries it but the preferences, the assets and the "this PC" half of the
// switch, which drops it; every form carries it as a hidden input. Asked
// without, nothing but the switch's own link mentions it.
func TestEveryLinkKeepsMachines(t *testing.T) {
	h := othersHarness(t)
	hdr := cookieHdr(h.login())
	var ids struct{ proj, sess, mem, day string }
	if p := apiPath(t, h, "/api/v1/projects").Tables.Projects; len(p) > 0 {
		ids.proj = p[0].Project
	}
	if s := apiPath(t, h, "/api/v1/sessions").Tables.Sessions; len(s) > 0 {
		ids.sess = s[0].ID
	}
	if m := apiPath(t, h, "/api/v1/memories").Tables.Memories; len(m) > 0 {
		ids.mem = m[0].ID
	}
	if d := apiPath(t, h, "/api/v1/days").Tables.Days; len(d) > 0 {
		ids.day = d[0].Day
	}
	if ids.proj == "" || ids.sess == "" || ids.mem == "" || ids.day == "" {
		t.Fatalf("history lacks an item: %+v", ids)
	}
	var paths []string
	for _, r := range routes {
		if r.Kind != "page" {
			continue
		}
		p := r.Sample
		switch r.Page {
		case "project":
			p = "/projects/" + url.PathEscape(ids.proj)
		case "session":
			p = "/sessions/" + url.PathEscape(ids.sess)
		case "memory":
			p = "/memories/" + url.PathEscape(ids.mem)
		case "day":
			p = "/days/" + ids.day
		}
		paths = append(paths, p)
	}
	paths = append(paths, "/releases?compare=v0.1.17,v0.1.18", "/sessions/zzzzzzzz", "/quality?days=0")
	sep := func(p string) string {
		if strings.Contains(p, "?") {
			return "&"
		}
		return "?"
	}
	seen := 0
	for _, p := range paths {
		for _, extra := range []string{"", "days=30&project=" + url.QueryEscape(ids.proj) + "&dev=1"} {
			all := p + sep(p) + "machines=all"
			if extra != "" {
				all += "&" + extra
			}
			r := h.do("GET", all, hdr...)
			if r.StatusCode != 200 && r.StatusCode != 404 && r.StatusCode != 400 {
				t.Errorf("GET %s: %d", all, r.StatusCode)
				continue
			}
			doc := parseHTML(t, body(t, r))
			links := internalLinks(doc)
			if len(links) == 0 {
				t.Errorf("%s: no internal link at all", all)
			}
			for _, a := range links {
				href := a.attr("href")
				seen++
				if a.attr("data-machines") == "this" {
					if strings.Contains(href, "machines") {
						t.Errorf("%s: the this-PC half %q keeps machines", all, href)
					}
					continue
				}
				if !strings.Contains(href, "machines=all") {
					t.Errorf("%s: link %q (%s) drops machines=all", all, href, a.text())
				}
			}
			if n := len(doc.find(".scopebar a[data-machines=this]")); n != 1 && r.StatusCode == 200 && !strings.HasPrefix(p, "/defs") {
				// the this-PC half is on every framed page that has a scope line
				t.Errorf("%s: %d this-PC halves", all, n)
			}
			for _, f := range doc.find("form") {
				in := f.first("input[name=machines]")
				if in == nil || in.attr("type") != "hidden" || in.attr("value") != "all" {
					t.Errorf("%s: form %q lacks a hidden machines=all (%v)", all, f.attr("action"), in)
				}
			}
			// Without the parameter: nothing mentions it but the switch's all half.
			plain := strings.Replace(all, "machines=all&", "", 1)
			plain = strings.TrimSuffix(strings.TrimSuffix(plain, "?machines=all"), "&machines=all")
			r = h.do("GET", plain, hdr...)
			if r.StatusCode == 200 {
				doc = parseHTML(t, body(t, r))
				for _, a := range doc.find("a") {
					if h := a.attr("href"); strings.Contains(h, "machines") && a.first(".mscope") == nil && a.Parent.attr("aria-label") != "Whose activity" {
						t.Errorf("%s: link %q mentions machines with this PC selected", plain, h)
					}
				}
			}
		}
	}
	if seen < 200 {
		t.Errorf("only %d links checked", seen)
	}
}
