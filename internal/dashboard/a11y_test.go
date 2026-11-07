package dashboard

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/views"
)

// a11yPage is one rendered page the accessibility tests read.
type a11yPage struct {
	Name string
	Doc  *node
}

var (
	a11yOnce  sync.Once
	a11yPages []a11yPage
)

// a11yKeysPrefs are the preferences of the pages drawn a second time: shortcuts on (the dialog and its
// button) and the compact density.
const a11yKeysPrefs = "v1.system.0.0.compact.1"

// allA11yPages is every page route served with the developer menu off and
// on, and again with the shortcuts and the compact density on, every state
// page, the text page, and the Overview on each scenario. The history holds
// the local collection's files, so the cards that read them are drawn with
// data. The pages are rendered once and shared by the tests of this file.
func allA11yPages(t *testing.T) []a11yPage {
	t.Helper()
	a11yOnce.Do(func() {
		oldSpec := dataSpec
		dataSpec.Collection = true
		h := dataHarness(t)
		dataSpec = oldSpec
		ck0 := h.login()
		ck := cookieHdr(ck0)
		add := func(name, raw string) {
			doc, err := parseHTMLErr(raw)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			a11yPages = append(a11yPages, a11yPage{name, doc})
		}
		// The id pages need ids the fixture holds.
		sid := ""
		for _, s := range apiPath(t, h, "/api/v1/sessions").Tables.Sessions {
			if tab := apiPath(t, h, "/api/v1/sessions/"+url.PathEscape(s.ID)).Tables.Session; tab != nil && len(tab.Turns) > 0 {
				sid = s.ID
				break
			}
		}
		mem := apiPath(t, h, "/api/v1/memories").Tables.Memories[0].ID
		proj := apiPath(t, h, "/api/v1/projects").Tables.Projects[0].Project
		day := apiPath(t, h, "/api/v1/days").Tables.Days[0].Day
		for _, r := range routes {
			if r.Kind != "page" && r.Kind != "text" {
				continue
			}
			path := r.Sample
			switch r.Page {
			case "project":
				path = "/projects/" + url.PathEscape(proj)
			case "session":
				path = "/sessions/" + url.PathEscape(sid)
			case "memory":
				path = "/memories/" + url.PathEscape(mem)
			case "day":
				path = "/days/" + day
			}
			if r.Kind == "text" {
				path = "/memories/" + url.PathEscape(mem) + "/text"
			}
			for _, dev := range []string{"0", "1", "keys"} {
				p, hdr := path, ck
				if r.Kind == "page" {
					p += "?dev=" + dev
				} else if dev != "0" {
					continue
				}
				name := p
				if dev == "keys" {
					p, hdr = path+"?dev=0", withPrefs(ck0, a11yKeysPrefs)
					name = p + " (shortcuts, compact)"
				}
				resp := h.do("GET", p, hdr...)
				if resp.StatusCode != 200 {
					t.Errorf("GET %s: %d", p, resp.StatusCode)
					continue
				}
				add(name, body(t, resp))
			}
		}
		// Every page on all the PCs: a history with three other PCs, asked with machines=all (the switch, the
		// Which PCs list, the merged figures and their table of PCs are on these pages and no others).
		oldSpec = dataSpec
		dataSpec.Collection, dataSpec.OtherPCs = true, 3
		hp := dataHarness(t)
		dataSpec = oldSpec
		ckp := cookieHdr(hp.login())
		for _, r := range routes {
			if r.Kind != "page" {
				continue
			}
			path := r.Sample
			switch r.Page {
			case "project":
				path = "/projects/" + url.PathEscape(apiPath(t, hp, "/api/v1/projects").Tables.Projects[0].Project)
			case "session":
				path = "/sessions/" + url.PathEscape(sid)
			case "memory":
				path = "/memories/" + url.PathEscape(mem)
			case "day":
				path = "/days/" + day
			}
			p := path + "?dev=0&machines=all"
			resp := hp.do("GET", p, ckp...)
			if resp.StatusCode != 200 {
				t.Errorf("GET %s: %d", p, resp.StatusCode)
				continue
			}
			add(p, body(t, resp))
		}
		// A list in the middle of its pages has both pager links.
		for _, p := range []string{"/sessions?limit=1&offset=1", "/memories?limit=1&offset=1"} {
			resp := h.do("GET", p, ck...)
			if resp.StatusCode != 200 {
				t.Errorf("GET %s: %d", p, resp.StatusCode)
				continue
			}
			add(p, body(t, resp))
		}
		for _, p := range []string{"/sessions/zzzzzzzz", "/quality?days=0"} {
			add("state "+p, body(t, h.do("GET", p, ck...)))
		}
		for _, c := range []struct {
			name string
			h    *harness
		}{
			{"building", newDataHarness(t, withIndex(func(f *fakeIndex) { f.openGate = make(chan struct{}) }), withServer(), withNeedsBuild())},
			{"updating", dataHarness(t, func(f *fakeIndex) { f.openErrs = []error{index.ErrUpdateInProgress, index.ErrUpdateInProgress} })},
			{"updated", dataHarness(t, func(f *fakeIndex) { f.side = true })},
			{"timeout", timeoutHarness(t)},
			{"broken", dataHarness(t, func(f *fakeIndex) { f.catchUpErr = errors.New("disk exploded") })},
		} {
			add("state "+c.name, body(t, c.h.do("GET", "/", cookieHdr(c.h.login())...)))
		}
		add("unauthorized", body(t, newHarness(t).do("GET", "/")))
		for _, s := range scenarios {
			tpls, specs, err := loadPages(Templates())
			if err != nil {
				t.Error(err)
				continue
			}
			spec := specs["overview"]
			d := fixtureData(t, spec, false)
			r := s.Result()
			d.V["overview"], d.Loc, d.L = r, r.Window.Loc, en
			d.P.Days, d.CLI = 7, "backsight stats overview"
			var sb strings.Builder
			if err := tpls["overview"].ExecuteTemplate(&sb, spec.File, d); err != nil {
				t.Error(err)
				continue
			}
			add("overview scenario "+s.Name, sb.String())
		}
	})
	if len(a11yPages) == 0 {
		t.Fatal("no pages rendered")
	}
	return a11yPages
}

// eachA11yPage runs f as a subtest on every page.
func eachA11yPage(t *testing.T, f func(t *testing.T, doc *node)) {
	t.Helper()
	for _, p := range allA11yPages(t) {
		t.Run(p.Name, func(t *testing.T) { f(t, p.Doc) })
	}
}

// timeoutHarness is a dashboard whose pages take too long.
func timeoutHarness(t *testing.T) *harness {
	t.Helper()
	old := renderTimeout
	renderTimeout = 20 * time.Millisecond
	t.Cleanup(func() { renderTimeout = old })
	h := dataHarness(t)
	h.ix.set(func(f *fakeIndex) { f.block = make(chan struct{}) })
	return h
}

func TestPagesParse(t *testing.T) {
	pages := allA11yPages(t)
	// 16 page routes dev off and on and with the shortcuts and the compact density, the text page; the 16
	// routes on all PCs; 2 paged lists, 2 failures, 5 card states, the 401; the scenarios.
	if want := 16*3 + 1 + 16 + 2 + 2 + 5 + 1 + len(scenarios); len(pages) != want {
		t.Errorf("%d pages parsed, want %d", len(pages), want)
	}
}

// On the pages of all PCs every table is named (a caption, or the label of what holds it), and the tables of PCs
// (Machines on System, By PC on Cost) scope their column headers. The control is that both are read.
func TestPCsPagesNameEveryTable(t *testing.T) {
	read := map[string]int{}
	for _, p := range allA11yPages(t) {
		if !strings.HasSuffix(p.Name, "machines=all") {
			continue
		}
		t.Run(p.Name, func(t *testing.T) {
			for _, tb := range p.Doc.find("table") {
				named := false
				if c := tb.first("caption"); c != nil && strings.TrimSpace(c.text()) != "" {
					named = true
				}
				if tb.attr("aria-label") != "" || tb.attr("aria-labelledby") != "" {
					named = true
				}
				for n := tb.Parent; n != nil && !named; n = n.Parent {
					if n.Name == "section" && n.attr("aria-labelledby") != "" {
						named = true
					}
				}
				if !named {
					t.Errorf("table at line %d has no caption, label or labelled card", tb.line)
				}
				if !strings.Contains(" "+tb.attr("class")+" ", " mach ") {
					continue // the tables block 5 and 6 shipped are read by their own tests
				}
				read[p.Name]++
				for _, th := range tb.find("th") {
					if sc := th.attr("scope"); sc != "col" && sc != "row" {
						t.Errorf("th %q at line %d has no scope", th.text(), th.line)
					}
				}
			}
		})
	}
	for _, page := range []string{"/system?dev=0&machines=all", "/cost?dev=0&machines=all"} {
		if read[page] == 0 {
			t.Errorf("no table of PCs read on %s: the check sees nothing (read %v)", page, read)
		}
	}
}

// walk calls f on every element under n, in document order.
func walk(n *node, f func(*node)) {
	for _, k := range n.Kids {
		f(k)
		walk(k, f)
	}
}

// within reports whether n or an ancestor matches the tag and class.
func within(n *node, tag, class string) bool {
	for ; n != nil; n = n.Parent {
		if n.Name == tag && (class == "" || slices.Contains(strings.Fields(n.Attr["class"]), class)) {
			return true
		}
	}
	return false
}

// accName is the accessible name of n: aria-label, else its text without
// aria-hidden parts, else the first img alt below it.
func accName(n *node) string {
	if v := strings.TrimSpace(n.Attr["aria-label"]); v != "" {
		return v
	}
	var b strings.Builder
	var rec func(*node)
	rec = func(c *node) {
		for _, it := range c.items {
			switch {
			case it.kid == nil:
				b.WriteString(it.text)
			case it.kid.Attr["aria-hidden"] == "true" || it.kid.has("hidden"):
			case it.kid.Name == "svg" || it.kid.Name == "script" || it.kid.Name == "style":
			default:
				b.WriteByte(' ')
				rec(it.kid)
				b.WriteByte(' ')
			}
		}
	}
	rec(n)
	if s := strings.Join(strings.Fields(b.String()), " "); s != "" {
		return s
	}
	if img := n.first("img"); img != nil {
		return strings.TrimSpace(img.Attr["alt"])
	}
	return ""
}

func TestLandmarksAndSkipLink(t *testing.T) {
	eachA11yPage(t, func(t *testing.T, doc *node) {
		if ms := doc.find("main"); len(ms) != 1 {
			t.Fatalf("%d main, want one", len(ms))
		}
		// A card page (build, update, 401) has one thing to read: no header,
		// footer or skip link.
		if within(doc.first("main"), "main", "solo") {
			if m := doc.first("main"); m.attr("id") != "main" {
				t.Errorf("solo main id %q, want main", m.attr("id"))
			}
		} else {
			if hs, fs := doc.find("header"), doc.find("footer"); len(hs) != 1 || len(fs) != 1 {
				t.Errorf("%d header, %d footer; want one each", len(hs), len(fs))
			}
			if m := doc.first("main"); m.attr("id") != "main" {
				t.Errorf("main id %q, want main", m.attr("id"))
			}
			if f := focusables(doc); len(f) == 0 || f[0].Name != "a" || f[0].attr("href") != "#main" || accName(f[0]) != "Skip to content" {
				t.Errorf("first focusable %v, want the Skip to content link to #main", f)
			}
		}
		seen := map[string]bool{}
		for _, nv := range doc.find("nav") {
			l := strings.TrimSpace(nv.attr("aria-label"))
			if l == "" || seen[l] {
				t.Errorf("nav %q: label empty or used twice", l)
			}
			seen[l] = true
		}
	})
}

// TestSoloPagesAreTheCards pins which pages are exempt from the header,
// footer and skip link: the card state pages and the 401, no other.
func TestSoloPagesAreTheCards(t *testing.T) {
	var solo []string
	for _, p := range allA11yPages(t) {
		if within(p.Doc.first("main"), "main", "solo") {
			solo = append(solo, p.Name)
		}
	}
	want := []string{"state building", "state updating", "state updated", "state timeout", "state broken", "unauthorized"}
	slices.Sort(solo)
	slices.Sort(want)
	if !slices.Equal(solo, want) {
		t.Errorf("solo pages %q, want %q", solo, want)
	}
}

func TestHeadingOrder(t *testing.T) {
	eachA11yPage(t, func(t *testing.T, doc *node) {
		var levels []int
		walk(doc, func(n *node) {
			if len(n.Name) == 2 && n.Name[0] == 'h' && n.Name[1] >= '1' && n.Name[1] <= '6' {
				levels = append(levels, int(n.Name[1]-'0'))
			}
		})
		if len(levels) == 0 || levels[0] != 1 || slices.Index(levels[1:], 1) >= 0 {
			t.Fatalf("heading levels %v: want exactly one h1, first", levels)
		}
		for i := 1; i < len(levels); i++ {
			if levels[i] > levels[i-1]+1 {
				t.Errorf("heading levels %v: h%d follows h%d", levels, levels[i], levels[i-1])
			}
		}
	})
}

// newCardLevels are the cards added after block 5 by the page that draws them: each is a section named by
// an h2, as the cards around it are, so the outline stays h1, h2 and below.
var newCardLevels = []struct{ page, id string }{
	{"/?dev=0", "w-helped"}, {"/?dev=0", "w-calendar"}, {"/days?dev=0", "w-calendar"}, {"/pipeline?dev=0", "w-pipeline-time_to_recall"},
	{"/system?dev=0", "w-doctor-history"}, {"/system?dev=0", "w-secrets-suspects"}, {"/cost?dev=0", "w-memory-yield"},
	{"/releases?dev=0", "w-releases-compare"}, {"/bank?dev=1", "w-bank-active_share"}, {"/bank?dev=1", "w-shows-concentration"},
	{"/quality?dev=1", "w-injection-contradicted"}, {"/quality?dev=1", "w-boost-set_changed"},
}

func TestNewCardsAreH2Sections(t *testing.T) {
	for _, c := range newCardLevels {
		doc := pageNamed(t, c.page)
		h := doc.first("#" + c.id)
		if h == nil {
			t.Errorf("%s: no %s", c.page, c.id)
			continue
		}
		sec := h.Parent
		for sec != nil && sec.Name != "section" {
			sec = sec.Parent
		}
		if h.Name != "h2" || sec == nil || sec.attr("aria-labelledby") != c.id || strings.TrimSpace(h.text()) == "" {
			t.Errorf("%s: %s is a %s with text %q in a section labelled %v, want an h2 naming its section", c.page, c.id, h.Name, h.text(), sec != nil && sec.attr("aria-labelledby") == c.id)
		}
	}
	// The memory page's origin card is among its cards, under the page title.
	for _, p := range allA11yPages(t) {
		if strings.HasPrefix(p.Name, "/memories/") && strings.HasSuffix(p.Name, "?dev=0") {
			if h := p.Doc.first("#w-memory-origin"); h == nil || h.Name != "h2" {
				t.Errorf("%s: the origin card is not an h2", p.Name)
			}
		}
	}
}

// TestOverviewHeadingOrder pins the Overview's outline: the page question,
// the verdict, the Found group, its tile.
func TestOverviewHeadingOrder(t *testing.T) {
	doc := renderOverview(t, scenarioNamed(t, "healthy"))
	var levels []int
	walk(doc, func(n *node) {
		if len(n.Name) == 2 && n.Name[0] == 'h' && n.Name[1] >= '1' && n.Name[1] <= '6' {
			levels = append(levels, int(n.Name[1]-'0'))
		}
	})
	if len(levels) < 4 || !slices.Equal(levels[:4], []int{1, 2, 2, 3}) {
		t.Errorf("heading levels %v, want h1, h2 (verdict), h2 (Found group), h3 (Found tile) first", levels)
	}
	if h := doc.find("h2"); len(h) < 2 || !within(h[0], "section", "verdict") {
		t.Errorf("the first h2 is not the verdict's")
	}
}

// calendarDayName is the name of a calendar day: its weekday and date, then what the day holds or that nothing
// was recorded.
var calendarDayName = regexp.MustCompile(`^(Mon|Tue|Wed|Thu|Fri|Sat|Sun), [A-Z][a-z]{2} \d{1,2}: \S.*`)

func TestLabels(t *testing.T) {
	seen := map[string]int{} // the controls of the new cards the sweep meets
	defer func() {
		for _, k := range []string{"date input", "dialog", "calendar day", "shortcuts button", "summary button"} {
			if seen[k] == 0 {
				t.Errorf("no %s on any page: the check sees nothing", k)
			}
		}
	}()
	eachA11yPage(t, func(t *testing.T, doc *node) {
		ids := map[string]bool{}
		walk(doc, func(n *node) {
			if id, ok := n.Attr["id"]; ok {
				if ids[id] {
					t.Errorf("id %q used twice", id)
				}
				ids[id] = true
			}
		})
		for _, img := range doc.find("img") {
			if strings.TrimSpace(img.attr("alt")) == "" && img.attr("aria-hidden") != "true" {
				t.Errorf("img %s has no alt", img.attr("src"))
			}
		}
		for _, s := range doc.find("svg") {
			switch {
			case s.attr("role") == "img" || s.attr("role") == "group":
				if strings.TrimSpace(s.attr("aria-label")) == "" {
					t.Errorf("svg role=%s at line %d has no aria-label", s.attr("role"), s.line)
				}
				// The children of role=img are presentational: nothing
				// inside may be a focus stop.
				if s.attr("role") == "img" {
					walk(s, func(k *node) {
						if k != s && k.attr("tabindex") == "0" {
							t.Errorf("%s at line %d is focusable inside svg role=img", k.Name, k.line)
						}
					})
				}
			case s.attr("aria-hidden") != "true" && !within(s.Parent, "svg", ""):
				t.Errorf("svg at line %d is neither role=img or group with a label nor aria-hidden", s.line)
			}
		}
		// A copy button is named after the command it copies, so a page
		// with several does not read "Copy, Copy, Copy".
		for _, b := range doc.find("button.copy") {
			if b.has("data-copy") {
				if strings.HasPrefix(b.attr("aria-label"), "Copy summary") {
					seen["summary button"]++
				}
				continue // the memory page's id button and the summary's have their own names
			}
			name, code := b.attr("aria-label"), b.Parent.first("code")
			if code == nil || name != "Copy: "+strings.TrimSpace(code.text()) {
				t.Errorf("copy button at line %d is named %q, want %q", b.line, name, "Copy: <its command>")
			}
		}
		// A tile is named by its heading, not by a second copy of the text.
		for _, tile := range doc.find("article.tile") {
			h := tile.first("h3")
			if tile.has("aria-label") || h == nil || tile.attr("aria-labelledby") != h.attr("id") || h.attr("id") == "" {
				t.Errorf("tile at line %d is not named by aria-labelledby on its h3", tile.line)
			}
		}
		for _, g := range doc.find("div.tgroups") {
			if g.attr("role") != "group" {
				t.Errorf("div.tgroups at line %d has an aria-label but no role", g.line)
			}
		}
		// Every table fallback is named and scopes its column headers.
		for _, tb := range doc.find("details.tblfb table") {
			if c := tb.first("caption"); c == nil || strings.TrimSpace(c.text()) == "" {
				t.Errorf("table fallback at line %d has no caption", tb.line)
			}
			for _, th := range tb.find("th") {
				if th.attr("scope") != "col" && th.attr("scope") != "row" {
					t.Errorf("th %q at line %d has no scope", th.text(), th.line)
				}
			}
		}
		for _, in := range doc.find("input") {
			if in.attr("type") == "hidden" || in.has("hidden") {
				continue
			}
			if in.attr("aria-label") == "" && !within(in, "label", "") {
				t.Errorf("input at line %d has no label", in.line)
			}
		}
		// The custom window's two date fields are named by their visible label, and bounded.
		for _, in := range doc.find("input[type=date]") {
			seen["date input"]++
			l := in
			for l != nil && l.Name != "label" {
				l = l.Parent
			}
			if l == nil || (!strings.HasPrefix(l.text(), "From") && !strings.HasPrefix(l.text(), "To")) {
				t.Errorf("date input %q at line %d is not inside a label From or To", in.attr("name"), in.line)
			}
			if in.attr("min") == "" || in.attr("max") == "" || in.attr("min") > in.attr("max") {
				t.Errorf("date input %q at line %d: min %q, max %q", in.attr("name"), in.line, in.attr("min"), in.attr("max"))
			}
		}
		// The shortcuts' dialog is named by its title, and its close button by what it does.
		for _, dg := range doc.find("dialog") {
			seen["dialog"]++
			h := ""
			for _, id := range strings.Fields(dg.attr("aria-labelledby")) {
				if n := doc.first("#" + id); n != nil {
					h += n.text()
				}
			}
			if strings.TrimSpace(h) == "" {
				t.Errorf("dialog at line %d is not labelled by a heading with text", dg.line)
			}
			if cl := dg.find("button"); len(cl) != 1 || accName(cl[0]) != "Close keyboard shortcuts" {
				t.Errorf("dialog at line %d: close buttons %d", dg.line, len(cl))
			}
		}
		for _, b := range doc.find("button.keyhint-btn") {
			seen["shortcuts button"]++
			if !b.has("hidden") || b.attr("aria-keyshortcuts") != "?" || accName(b) != "?Keyboard shortcuts" && accName(b) != "? Keyboard shortcuts" {
				t.Errorf("shortcuts button at line %d: hidden %v, aria-keyshortcuts %q, name %q", b.line, b.has("hidden"), b.attr("aria-keyshortcuts"), accName(b))
			}
		}
		// Every day of the calendar says its date and what it holds, in text (not by colour); one is a Tab stop.
		for _, cal := range doc.find("div.hm") {
			stops, days := 0, cal.find("a.hm-c")
			for _, a := range days {
				seen["calendar day"]++
				name := a.attr("aria-label")
				if !calendarDayName.MatchString(name) || a.attr("title") != name || accName(a) != name {
					t.Errorf("calendar day at line %d is named %q (title %q)", a.line, name, a.attr("title"))
				}
				if a.attr("tabindex") == "0" {
					stops++
					if !strings.HasSuffix(name, "(today)") {
						t.Errorf("the calendar's Tab stop %q is not today", name)
					}
				} else if a.attr("tabindex") != "-1" {
					t.Errorf("calendar day %q has tabindex %q, want -1", name, a.attr("tabindex"))
				}
			}
			if len(days) == 0 || stops != 1 {
				t.Errorf("calendar at line %d: %d days, %d Tab stops, want one", cal.line, len(days), stops)
			}
		}
		for _, sel := range []string{"button", "summary"} {
			for _, b := range doc.find(sel) {
				if accName(b) == "" {
					t.Errorf("%s at line %d has no text or aria-label", sel, b.line)
				}
			}
		}
		for _, a := range doc.find("a[href]") {
			if accName(a) == "" {
				t.Errorf("link %s at line %d has no name", a.attr("href"), a.line)
			}
		}
		walk(doc, func(n *node) {
			for _, k := range []string{"aria-describedby", "aria-labelledby"} {
				for _, id := range strings.Fields(n.attr(k)) {
					if !ids[id] {
						t.Errorf("%s=%q at line %d names no id on the page", k, id, n.line)
					}
				}
			}
		})
	})
}

func TestNoPositiveTabindex(t *testing.T) {
	eachA11yPage(t, func(t *testing.T, doc *node) {
		walk(doc, func(n *node) {
			if v, ok := n.Attr["tabindex"]; ok && v != "0" && v != "-1" {
				t.Errorf("%s at line %d has tabindex=%q", n.Name, n.line, v)
			}
		})
	})
}

// focusables are the elements a Tab walk reaches at 1440 px within five
// minutes of rendering, in document order: the content of a closed details
// is skipped, and so are the Menu (hidden by CSS from 900 px), the stale
// chip (hidden for 300 s) and what has tabindex -1 (focused by a script's
// arrow keys, never by Tab: the calendar's days but one).
func focusables(doc *node) []*node {
	var out []*node
	var rec func(*node)
	rec = func(n *node) {
		for _, k := range n.Kids {
			if k.has("hidden") || k.Name == "script" || within(k, "details", "navmenu") || within(k, "a", "chip-stale") {
				continue
			}
			if k.Attr["tabindex"] == "-1" {
				rec(k)
				continue
			}
			switch {
			case k.Name == "a" && k.has("href"), k.Name == "button", k.Name == "summary", k.Attr["tabindex"] == "0",
				k.Name == "input" && k.attr("type") != "hidden":
				out = append(out, k)
			}
			if k.Name == "details" && !k.has("open") {
				for _, kk := range k.Kids {
					if kk.Name == "summary" && !kk.has("hidden") {
						out = append(out, kk)
					}
				}
				continue
			}
			rec(k)
		}
	}
	rec(doc)
	return out
}

// TestOverviewFocusOrder is the keyboard walk of the healthy Overview, dev
// off, at 1440 px, with Helped most's three memories and the custom window
// picker: R28's list. The Copy summary button is hidden until the script
// shows it, so it is no stop; the calendar is one stop (today), the arrow
// keys move inside it; the summary as text is the last card's fold.
func TestOverviewFocusOrder(t *testing.T) {
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	spec := specs["overview"]
	d := fixtureData(t, spec, false)
	r := scenarioNamed(t, "healthy")
	r.Tables.Helped = helpedRows()
	d.V["overview"], d.Loc, d.L = r, r.Window.Loc, en
	d.P.Days, d.CLI = 7, "backsight stats overview"
	d.Frame.Nav, d.Frame.Prefs.Dev = navFor("/", false), false
	c := newCustom("/", url.Values{}, views.Params{}, r.Window, r.GeneratedAt)
	d.Frame.Custom, d.Frame.CustomMenu = &c, c.inMenu()
	var sb strings.Builder
	if err := tpls["overview"].ExecuteTemplate(&sb, spec.File, d); err != nil {
		t.Fatal(err)
	}
	doc := parseHTML(t, sb.String())
	var got []string
	for _, f := range focusables(doc) {
		got = append(got, accName(f))
	}
	// The calendar's one stop is today's: its label says what the day holds, then (today).
	var today []string
	for i, g := range got {
		if strings.HasSuffix(g, " (today)") {
			today = append(today, g)
			got[i] = "<today> (today)"
		}
	}
	if len(today) != 1 {
		t.Errorf("%d calendar stops named (today): %q, want one", len(today), today)
	}
	want := []string{"Skip to content", "backsight", "Overview", "Quality", "Cost", "Bank", "Pipeline", "Browse", "System",
		"24 h", "7 d", "30 d", "90 d", "Custom window: pick the days", "All projects", "Refresh", "Preferences", "How the verdict is judged", "CLI",
		"About Found", "167 prompts →", "CLI", "About Used", "61 turns →", "CLI", "About Reported wrong", "14 verdicts →", "CLI",
		"About Context", "34 sessions →", "CLI", "About Speed", "167 prompts →", "CLI", "About Flowing", "239 sessions →", "CLI", "CLI", "CLI",
		"a41c09e2", "backsight", "5be2d7f0", "project-a", "93c1a4e8", "backsight", "d0f63b15", "project-c", "2e8a91c7", "project-d-shared-fronte…",
		"All sessions →", "About memory at a glance", "CLI",
		"About helped most", "CLI", "7f3a9c21", "backsight", "Show text of memory 7f3a9c21", "b20e5d47", "project-d-shared-fronte…", "Show text of memory b20e5d47",
		"4c81f0a3", "project-a", "Show text of memory 4c81f0a3", "All memories, by use →",
		"About the days calendar", "CLI", "Prompts that got memory", "Turns where Claude used memory", "Sessions", "<today> (today)",
		"Show as a table", "Summary as text", "test", "Help: backsight guide dashboard", "Definitions"}
	if !slices.Equal(got, want) {
		t.Errorf("focus order\n got %q\nwant %q", got, want)
	}
	// The picker is where the served page puts it too: right after the window links.
	served := pageNamed(t, "/?dev=0")
	var names []string
	for _, f := range focusables(served) {
		names = append(names, accName(f))
	}
	if i := slices.Index(names, "90 d"); i < 0 || i+1 >= len(names) || names[i+1] != "Custom window: pick the days" {
		t.Errorf("the served Overview's walk after 90 d: %q, want the custom window picker next", names[min(max(i, 0), len(names)):min(max(i, 0)+3, len(names))])
	}
	if n := slices.Index(names, "Copy summary"); n >= 0 {
		t.Error("the Copy summary button is a Tab stop before any script shows it")
	}
}

// pageNamed is the rendered a11y page of that name.
func pageNamed(t *testing.T, name string) *node {
	t.Helper()
	for _, p := range allA11yPages(t) {
		if p.Name == name {
			return p.Doc
		}
	}
	t.Fatalf("no page %s", name)
	return nil
}
