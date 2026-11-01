package dashboard

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// cssRule is one style rule, with the at-rule prelude it sits in ("" at
// the top level, "@container page (max-width: 899px)" in that block).
type cssRule struct {
	At   string
	Sel  []string // the comma-separated selectors, trimmed
	Body string
}

var cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// cssRules reads css into its rules, descending into @media and @container.
func cssRules(css string) []cssRule {
	return rulesIn(cssComment.ReplaceAllString(css, ""), "")
}

func rulesIn(s, at string) []cssRule {
	var out []cssRule
	for {
		open := strings.IndexByte(s, '{')
		if open < 0 {
			return out
		}
		depth, end := 1, open+1
		for ; end < len(s) && depth > 0; end++ {
			switch s[end] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		head, body := strings.TrimSpace(s[:open]), s[open+1:end-1]
		if strings.HasPrefix(head, "@media") || strings.HasPrefix(head, "@container") {
			out = append(out, rulesIn(body, head)...)
		} else if !strings.HasPrefix(head, "@") {
			var sels []string
			for _, p := range strings.Split(head, ",") {
				if p = strings.TrimSpace(p); p != "" {
					sels = append(sels, p)
				}
			}
			out = append(out, cssRule{At: at, Sel: sels, Body: body})
		}
		s = s[end:]
	}
}

// declStarts reports whether the rule declares prop with a value that
// begins with prefix (spaces collapsed).
func (r cssRule) declStarts(prop, prefix string) bool {
	re := regexp.MustCompile(`(?i)(?:^|[;\s])` + regexp.QuoteMeta(prop) + `\s*:\s*([^;]*)`)
	for _, m := range re.FindAllStringSubmatch(r.Body, -1) {
		if strings.HasPrefix(strings.Join(strings.Fields(m[1]), " "), prefix) {
			return true
		}
	}
	return false
}

func dashboardCSS(t testing.TB) string {
	t.Helper()
	b, err := assetFS.ReadFile("assets/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const narrow = "@container page (max-width: 899px)"

// selMatch reports whether n is matched by the selector sel: compounds
// joined by a space (descendant) or > (child), as dashboard.css writes them.
func selMatch(n *node, sel string) bool {
	sel = strings.ReplaceAll(sel, ">", " > ")
	parts := strings.Fields(sel)
	var rec func(n *node, i int) bool
	rec = func(n *node, i int) bool {
		if !parseSimple(parts[i]).matches(n) {
			return false
		}
		if i == 0 {
			return true
		}
		child := parts[i-1] == ">"
		j := i - 1
		if child {
			j = i - 2
		}
		if child {
			return n.Parent != nil && rec(n.Parent, j)
		}
		for p := n.Parent; p != nil; p = p.Parent {
			if rec(p, j) {
				return true
			}
		}
		return false
	}
	return len(parts) > 0 && !strings.ContainsAny(sel, ":*") && rec(n, len(parts)-1)
}

// touchSelectors are the selectors that get the 40 px minimum under 900 px:
// those of the container block, and with always the ones that have it at all
// sizes.
func touchSelectors(t *testing.T, always bool) []string {
	t.Helper()
	var out []string
	for _, r := range cssRules(dashboardCSS(t)) {
		if (r.At == narrow || always && r.At == "") && r.declStarts("min-height", "var(--touch)") {
			out = append(out, r.Sel...)
		}
	}
	return out
}

func TestTouchTargetsUnder900(t *testing.T) {
	got := touchSelectors(t, false)
	for _, want := range []string{".btn", ".copy", "details.cli > summary", ".seg a", "details.info > summary", ".badge", ".mchip", ".tabs a",
		"details.why > summary", ".idc", ".menu > summary", ".nav a"} {
		if !slices.Contains(got, want) {
			t.Errorf("under 900 px, %q has no min-height: var(--touch) (have %q)", want, got)
		}
	}
	// Every control on a page is one of those: no link, button or summary
	// a finger must hit escapes the rule, bar the ones in running text.
	all := touchSelectors(t, true)
	uncovered := map[string][]string{}
	for _, p := range allA11yPages(t) {
		walk(p.Doc, func(n *node) {
			if n.Name != "a" && n.Name != "button" && n.Name != "summary" {
				return
			}
			if n.Name == "a" && !n.has("href") {
				return
			}
			if n.Name == "a" && n.attr("class") == "" && inRunningText(n) {
				return
			}
			for _, s := range all {
				if selMatch(n, s) {
					return
				}
			}
			key := n.Name + "." + n.attr("class")
			if n.attr("class") == "" {
				key = n.Name + " (no class)"
			}
			uncovered[key] = append(uncovered[key], p.Name)
		})
	}
	for key, pages := range uncovered {
		if !touchExempt[key] {
			t.Errorf("%s on %d pages (e.g. %s) has no touch-target rule under 900 px", key, len(pages), pages[0])
		}
	}
	for key := range touchExempt {
		if _, used := uncovered[key]; !used {
			t.Errorf("exemption %q matches no control any more: drop it", key)
		}
	}
}

// inRunningText reports whether the link a is a word or phrase of a
// paragraph, list item or table cell that has other text around it, in the
// page body: the one place a small target is accepted (WCAG 2.5.8 inline
// links), or a link in a table cell. A link that stands alone in a footer, a breadcrumb, a popover or a
// card's foot is a control.
func inRunningText(a *node) bool {
	p := a.Parent
	if p == nil || !slices.Contains([]string{"p", "li", "td", "dd"}, p.Name) || !within(p, "main", "") {
		return false
	}
	for _, c := range [][2]string{{"nav", ""}, {"footer", ""}, {"div", "pop"}, {"div", "tile-foot"}, {"div", "card-foot"}} {
		if within(p, c[0], c[1]) {
			return false
		}
	}
	if p.Name == "td" {
		return true // a table cell's text: the row is the target
	}
	for _, it := range p.items {
		if it.kid == nil && strings.TrimSpace(it.text) != "" {
			return true
		}
	}
	return false
}

// touchExempt are the controls that are not a target of their own and need
// no rule: none today.
var touchExempt = map[string]bool{}

func TestFocusIsVisible(t *testing.T) {
	css := dashboardCSS(t)
	rules := cssRules(css)
	found := false
	for _, r := range rules {
		if r.At == "" && slices.Contains(r.Sel, ":focus-visible") && r.declStarts("outline", "2px solid var(--focus)") {
			found = true
		}
	}
	if !found {
		t.Error("no `:focus-visible { outline: 2px solid var(--focus)` rule")
	}
	for _, v := range outlineViolations(rules) {
		t.Error(v)
	}
}

// removesOutline reports whether r takes an outline away: none, 0 (a
// length of zero, not 0.5px), transparent, or an outline-style of none.
func removesOutline(r cssRule) bool {
	re := regexp.MustCompile(`(?i)(?:^|[;\s])outline(-style|-color)?\s*:\s*([^;]*)`)
	for _, m := range re.FindAllStringSubmatch(r.Body, -1) {
		v := strings.Join(strings.Fields(m[2]), " ")
		first, _, _ := strings.Cut(v, " ")
		switch m[1] {
		case "-style":
			if strings.EqualFold(first, "none") || strings.EqualFold(first, "hidden") {
				return true
			}
		case "-color":
			if strings.EqualFold(first, "transparent") {
				return true
			}
		default:
			if strings.EqualFold(first, "none") || regexp.MustCompile(`^0(px|em|rem)?$`).MatchString(first) || strings.Contains(strings.ToLower(v), "transparent") {
				return true
			}
		}
	}
	return false
}

// outlineViolations are the rules that remove an outline without giving
// the same selector a :focus-visible outline.
func outlineViolations(rules []cssRule) []string {
	var out []string
	for _, r := range rules {
		if !removesOutline(r) {
			continue
		}
		for _, s := range r.Sel {
			ok := false
			for _, o := range rules {
				if slices.Contains(o.Sel, s+":focus-visible") && o.declStarts("outline", "") && !removesOutline(o) {
					ok = true
				}
			}
			if !ok {
				out = append(out, "outline removed from "+s+" with no :focus-visible outline on the same selector")
			}
		}
	}
	return out
}

func TestOutlineCheckerSeesViolations(t *testing.T) {
	bad := cssRules(`a { outline: none; } b { outline: 0 } c { outline: none } c:focus-visible { outline: 2px solid red }
d { outline-style: none } e { outline: transparent } f { outline: 0.5px solid red } g { outline: 2px solid transparent }`)
	got := outlineViolations(bad)
	want := []string{"a", "b", "d", "e", "g"} // not c (it has the :focus-visible rule), not f (0.5px)
	if len(got) != len(want) {
		t.Fatalf("violations %q, want %q", got, want)
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], "outline removed from "+w+" ") {
			t.Errorf("violation %d %q, want %s", i, got[i], w)
		}
	}
}

func TestForcedColors(t *testing.T) {
	rules := cssRules(dashboardCSS(t))
	has := func(sel, prop, value string) bool {
		for _, r := range rules {
			if r.At == "@media (forced-colors: active)" && slices.Contains(r.Sel, sel) && r.declStarts(prop, value) {
				return true
			}
		}
		return false
	}
	for _, c := range [][3]string{
		{".chart svg", "forced-color-adjust", "none"},
		{".st::before", "background", "CanvasText"},
		{".i", "background", "CanvasText"},
		{".v-ico", "background", "CanvasText"},
		{".chart svg rect.seg", "stroke", "CanvasText"},
		{`.chart svg path.seg[class*="f-"]`, "stroke", "CanvasText"},
	} {
		if !has(c[0], c[1], c[2]) {
			t.Errorf("forced-colors: %q lacks %s: %s", c[0], c[1], c[2])
		}
	}
	// A chart is the only thing whose colours must not be forced.
	for _, r := range rules {
		if r.At == "@media (forced-colors: active)" && r.declStarts("forced-color-adjust", "none") && !r.declStarts("background", "CanvasText") {
			for _, s := range r.Sel {
				if !strings.HasPrefix(s, ".chart svg") && s != ".kbar-svg" && s != ".pbar" && s != ".sw" && s != ".spark" {
					t.Errorf("forced-colors: %q switches forced colours off", s)
				}
			}
		}
	}
}

func TestChartsKeep12px(t *testing.T) {
	rules := cssRules(dashboardCSS(t))
	for _, c := range [][3]string{{".vb560", "min-width", "560px"}, {".vb1000", "min-width", "1000px"}, {".chart-plot", "overflow-x", "auto"}, {".tbl-wrap", "overflow-x", "auto"}} {
		ok := false
		for _, r := range rules {
			ok = ok || r.At == "" && slices.Contains(r.Sel, c[0]) && r.declStarts(c[1], c[2])
		}
		if !ok {
			t.Errorf("%s { %s: %s } is missing", c[0], c[1], c[2])
		}
	}
	svgs := 0
	for _, p := range allA11yPages(t) {
		for _, f := range p.Doc.find("figure.chart") {
			walk(f, func(s *node) {
				if s.Name != "svg" || within(s, "div", "legend") || within(s, "span", "legend") || within(s, "ul", "legend") {
					return
				}
				svgs++
				w, err := viewBoxWidth(s.attr("viewbox"))
				var want int
				switch {
				case slices.Contains(strings.Fields(s.attr("class")), "vb560"):
					want = 560
				case slices.Contains(strings.Fields(s.attr("class")), "vb1000"):
					want = 1000
				}
				if err != nil || want == 0 || w != want {
					t.Errorf("%s: chart svg at line %d: class %q, viewBox %q; want vb560 or vb1000 with a viewBox as wide", p.Name, s.line, s.attr("class"), s.attr("viewbox"))
				}
			})
		}
	}
	if svgs == 0 {
		t.Error("no chart svg on any page")
	}
}

func viewBoxWidth(vb string) (int, error) {
	f := strings.Fields(vb)
	if len(f) != 4 {
		return 0, strconv.ErrSyntax
	}
	w, err := strconv.ParseFloat(f[2], 64)
	return int(w), err
}

// Every table scrolls sideways inside its own .tbl-wrap, so a wide table
// never makes the page scroll.
func TestEveryTableScrollsInItsWrap(t *testing.T) {
	tables := 0
	for _, p := range allA11yPages(t) {
		for _, tb := range p.Doc.find("table") {
			tables++
			if !within(tb, "div", "tbl-wrap") {
				t.Errorf("%s: table at line %d is not inside div.tbl-wrap", p.Name, tb.line)
			}
		}
	}
	if tables == 0 {
		t.Error("no table on any page")
	}
}

// A row of links or a command box must not be wider than the card at 390 px:
// the axis tabs wrap, and a command in an empty state stays inside it.
func TestNarrowRowsStayInsideTheirCard(t *testing.T) {
	rules := cssRules(dashboardCSS(t))
	for _, c := range [][3]string{{".seg", "flex-wrap", "wrap"}, {".seg", "max-width", "100%"}, {".empty .cmd", "max-width", "100%"}} {
		ok := false
		for _, r := range rules {
			ok = ok || r.At == "" && slices.Contains(r.Sel, c[0]) && r.declStarts(c[1], c[2])
		}
		if !ok {
			t.Errorf("%s { %s: %s } is missing", c[0], c[1], c[2])
		}
	}
}

// A chart is never drawn much larger than its viewBox, whatever its card,
// so the 12 user units of its text stay about 12 px and every chart's text
// is the same size: the widest a chart svg gets is within 1.13 of its
// viewBox width, for both widths (the full-width strip is vb1000).
func TestChartsAreNotScaledUp(t *testing.T) {
	rules := cssRules(dashboardCSS(t))
	val := func(sel, prop string) float64 {
		for _, r := range rules {
			if r.At == "" && slices.Contains(r.Sel, sel) {
				if m := regexp.MustCompile(`(?i)(?:^|[;\s])` + prop + `\s*:\s*(\d+)px`).FindStringSubmatch(r.Body); m != nil {
					v, _ := strconv.ParseFloat(m[1], 64)
					return v
				}
			}
		}
		return 0
	}
	for _, c := range []struct {
		sel string
		vb  float64
	}{{".vb560", 560}, {".vb1000", 1000}} {
		if min := val(c.sel, "min-width"); min != c.vb {
			t.Errorf("%s min-width = %v, want %v", c.sel, min, c.vb)
		}
		max := val(c.sel, "max-width")
		if max < c.vb || max > c.vb*1.13 {
			t.Errorf("%s max-width = %v, want %v to %v: a chart scaled up shows larger text", c.sel, max, c.vb, c.vb*1.13)
		}
	}
	// The strip draws with a vb class like the others, on the memory page.
	strips := 0
	for _, p := range allA11yPages(t) {
		for _, f := range p.Doc.find("figure.chart") {
			walk(f, func(s *node) {
				if s.Name == "svg" && strings.Contains(s.attr("aria-label"), "was shown, by where") {
					strips++
					if !slices.Contains(strings.Fields(s.attr("class")), "vb1000") {
						t.Errorf("%s: the strip svg has class %q, want vb1000", p.Name, s.attr("class"))
					}
				}
			})
		}
	}
	if strips == 0 {
		t.Error("no strip svg on any page")
	}
}

// A grid item stretches to its row: the attention pill would grow into a
// tall oval beside wrapped text. Red without the align-self rule.
func TestAttentionPillDoesNotStretch(t *testing.T) {
	for _, r := range cssRules(dashboardCSS(t)) {
		if slices.Contains(r.Sel, ".att-item > .pill") && r.declStarts("align-self", "start") {
			return
		}
	}
	t.Error(".att-item > .pill has no align-self: start")
}
