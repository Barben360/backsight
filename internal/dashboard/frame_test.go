package dashboard

import (
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
	if len(hints) != 1 || hints[0].text() != "Adds ranking, learned ranking and release detail." {
		t.Errorf("preference hints %+v", hints)
	}
}
