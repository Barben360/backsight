package dashboard

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
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

// allA11yPages is every page route served with the developer menu off and
// on, every state page, the text page, and the Overview on each scenario.
// The pages are rendered once and shared by the tests of this file.
func allA11yPages(t *testing.T) []a11yPage {
	t.Helper()
	a11yOnce.Do(func() {
		h := dataHarness(t)
		ck := cookieHdr(h.login())
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
			for _, dev := range []string{"0", "1"} {
				p := path
				if r.Kind == "page" {
					p += "?dev=" + dev
				} else if dev == "1" {
					continue
				}
				resp := h.do("GET", p, ck...)
				if resp.StatusCode != 200 {
					t.Errorf("GET %s: %d", p, resp.StatusCode)
					continue
				}
				add(p, body(t, resp))
			}
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
	// 16 page routes dev off and on, the text page; 2 paged lists, 2 failures, 5 card states, the 401; the scenarios.
	if want := 16*2 + 1 + 2 + 2 + 5 + 1 + len(scenarios); len(pages) != want {
		t.Errorf("%d pages parsed, want %d", len(pages), want)
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

func TestLabels(t *testing.T) {
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
			case s.attr("role") == "img":
				if strings.TrimSpace(s.attr("aria-label")) == "" {
					t.Errorf("svg role=img at line %d has no aria-label", s.line)
				}
			case s.attr("aria-hidden") != "true" && !within(s.Parent, "svg", ""):
				t.Errorf("svg at line %d is neither role=img with a label nor aria-hidden", s.line)
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
// is skipped, and so are the Menu (hidden by CSS from 900 px) and the stale
// chip (hidden for 300 s).
func focusables(doc *node) []*node {
	var out []*node
	var rec func(*node)
	rec = func(n *node) {
		for _, k := range n.Kids {
			if k.has("hidden") || k.Name == "script" || within(k, "details", "navmenu") || within(k, "a", "chip-stale") {
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
// off, at 1440 px: R28's list.
func TestOverviewFocusOrder(t *testing.T) {
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	spec := specs["overview"]
	d := fixtureData(t, spec, false)
	r := scenarioNamed(t, "healthy")
	d.V["overview"], d.Loc, d.L = r, r.Window.Loc, en
	d.P.Days, d.CLI = 7, "backsight stats overview"
	d.Frame.Nav, d.Frame.Prefs.Dev = navFor("/", false), false
	var sb strings.Builder
	if err := tpls["overview"].ExecuteTemplate(&sb, spec.File, d); err != nil {
		t.Fatal(err)
	}
	doc := parseHTML(t, sb.String())
	var got []string
	for _, f := range focusables(doc) {
		got = append(got, accName(f))
	}
	want := []string{"Skip to content", "backsight", "Overview", "Quality", "Cost", "Bank", "Pipeline", "Browse", "System",
		"24 h", "7 d", "30 d", "90 d", "All projects", "Refresh", "Preferences", "How the verdict is judged", "CLI",
		"About Found", "167 prompts →", "CLI"}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Errorf("focus order\n got %q\nwant %q first", got, want)
	}
}
