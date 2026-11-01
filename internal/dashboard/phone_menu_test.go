package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// narrowDisplayNone returns the selectors that the stylesheet's narrow
// (under 900 px) block sets to display: none.
func narrowDisplayNone(t *testing.T) []string {
	t.Helper()
	b, err := assetFS.ReadFile("assets/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	start := strings.Index(css, "@container page (max-width: 899px) {")
	if start < 0 {
		t.Fatal("dashboard.css has no narrow block")
	}
	open := strings.Index(css[start:], "{") + start
	depth, end := 0, -1
	for i := open; i < len(css) && end < 0; i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		t.Fatal("narrow block is not closed")
	}
	var sels []string
	rule := regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	for _, m := range rule.FindAllStringSubmatch(css[open+1:end], -1) {
		if regexp.MustCompile(`display:\s*none\b`).MatchString(m[2]) {
			for _, s := range strings.Split(m[1], ",") {
				sels = append(sels, strings.TrimSpace(s))
			}
		}
	}
	return sels
}

// On a phone the Menu is the only way to change the window: nothing the
// narrow block hides may sit inside it.
func TestNarrowRulesDoNotHideTheMenuContent(t *testing.T) {
	page := renderOverview(t, scenarioNamed(t, "healthy"))
	menu := page.first("details.navmenu")
	if menu == nil {
		t.Fatal("no details.navmenu on the page")
	}
	var nav *node
	for _, c := range menu.find("nav.seg") {
		if c.attr("aria-label") == "Window (menu)" {
			nav = c
		}
	}
	if nav == nil {
		t.Fatal("the Menu has no window nav")
	}
	var links []string
	for _, a := range nav.find("a") {
		links = append(links, a.text())
	}
	if want := "24 h 7 d 30 d 90 d"; strings.Join(links, " ") != want {
		t.Errorf("Menu window links %q, want %q", links, want)
	}
	sels := narrowDisplayNone(t)
	if len(sels) < 3 {
		t.Fatalf("narrow display:none selectors %q: expected at least the nav and the top bar controls", sels)
	}
	hidesTopBar := false
	for _, sel := range sels {
		if len(page.find(sel)) > 0 && sel != ".nav" {
			hidesTopBar = true
		}
		for _, n := range page.find(sel) {
			for p := n; p != nil; p = p.Parent {
				if p.Name == "details" && strings.Contains(" "+p.attr("class")+" ", " navmenu ") {
					t.Errorf("%q hides <%s class=%q> inside the Menu", sel, n.Name, n.attr("class"))
					break
				}
			}
		}
	}
	if !hidesTopBar {
		t.Errorf("none of %q matches a top bar control: the check would be vacuous", sels)
	}
}

func TestFindChildCombinator(t *testing.T) {
	root := parseHTML(t, `<div class="a"><p class="x">1</p><div><p class="x">2</p></div></div>`)
	if n := len(root.find(".a .x")); n != 2 {
		t.Errorf(".a .x: %d, want 2", n)
	}
	if got := root.find(".a > .x"); len(got) != 1 || got[0].text() != "1" {
		t.Errorf(".a > .x: %v, want only the direct child", got)
	}
	if n := len(root.find("div > div > p")); n != 1 {
		t.Errorf("div > div > p: %d, want 1", n)
	}
}
