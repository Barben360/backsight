package feedback

import (
	"bytes"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
)

// defaultCurveParams is what scripts/feedback-curve draws with: the boost
// part of config.Default's [feedback] table.
func defaultCurveParams() Params {
	f := config.Default(paths.Dirs{}, "").Feedback
	return Params{Weight: f.BoostWeight, PriorCredits: f.PriorCredits, PriorMisses: f.PriorMisses}
}

// The committed SVGs are what the script draws from today's defaults: a
// change to the formula or to a default fails here until they are redrawn.
func TestCurveSVGsMatchTheDefaults(t *testing.T) {
	for _, theme := range []string{"light", "dark"} {
		path := "../../docs/assets/feedback-curve-" + theme + ".svg"
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run go run ./scripts/feedback-curve)", path, err)
		}
		if got := CurveSVG(defaultCurveParams(), theme); !bytes.Equal(got, committed) {
			t.Errorf("%s differs from CurveSVG(defaults, %q): run go run ./scripts/feedback-curve", path, theme)
		}
	}
}

// One uncredited show in three counts as a miss, so a memory credited one
// show in ten has the prior's credit share: its boost is 0 at every count,
// and the chart draws that line on the zero line.
func TestCurveNeutralLineIsZero(t *testing.T) {
	p := defaultCurveParams()
	for n := 0; n <= curveMaxShown; n++ {
		if b := curveBoost(p, 0.1, n); math.Abs(b) > 1e-12 {
			t.Fatalf("boost at 10 %% after %d shows = %v, want 0", n, b)
		}
	}
	if b := curveBoost(p, 1, 50); b <= 0 {
		t.Errorf("always credited, 50 shows: boost %v, want > 0", b)
	}
	if b := curveBoost(p, 0, 50); b >= 0 {
		t.Errorf("never credited, 50 shows: boost %v, want < 0", b)
	}
	svg := string(CurveSVG(p, "light"))
	zero := regexp.MustCompile(`<line class="zero" x1="[^"]+" y1="([^"]+)"`).FindStringSubmatch(svg)
	if zero == nil {
		t.Fatal("no zero line")
	}
	lines := polylines(t, svg)
	for _, pt := range strings.Fields(lines[2]) {
		if y := strings.Split(pt, ",")[1]; y != zero[1] {
			t.Fatalf("neutral line point %s off the zero line (y %s)", pt, zero[1])
		}
	}
}

// polylines are the svg's data lines' points, in drawing order.
func polylines(t *testing.T, svg string) []string {
	t.Helper()
	m := regexp.MustCompile(`<polyline points="([^"]+)"`).FindAllStringSubmatch(svg, -1)
	if len(m) != 5 {
		t.Fatalf("%d data lines, want 5", len(m))
	}
	out := make([]string, len(m))
	for i, s := range m {
		out[i] = s[1]
	}
	return out
}

// luminance is a #rrggbb colour's WCAG 2 relative luminance.
func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	v, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	if err != nil || len(hex) != 7 {
		t.Fatalf("colour %q: not #rrggbb", hex)
	}
	lin := func(c uint64) float64 {
		s := float64(c) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(v>>16&0xff) + 0.7152*lin(v>>8&0xff) + 0.0722*lin(v&0xff)
}

func contrast(t *testing.T, a, b string) float64 {
	la, lb := luminance(t, a), luminance(t, b)
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
}

// Every colour the chart uses passes its WCAG ratio on its theme's
// background: text 4.5:1, lines and axes 3:1; and the background is
// GitHub's for that theme.
func TestCurveContrast(t *testing.T) {
	for theme, bg := range map[string]string{"light": "#ffffff", "dark": "#0d1117"} {
		svg := string(CurveSVG(defaultCurveParams(), theme))
		rect := regexp.MustCompile(`<rect [^>]*fill="(#[0-9a-f]{6})"`).FindStringSubmatch(svg)
		if rect == nil || rect[1] != bg {
			t.Fatalf("%s: background rect %v, want fill %s", theme, rect, bg)
		}
		texts := regexp.MustCompile(`<text [^>]*fill="(#[0-9a-f]{6})"`).FindAllStringSubmatch(svg, -1)
		if len(texts) < 5 {
			t.Fatalf("%s: %d text elements with a fill", theme, len(texts))
		}
		for _, m := range texts {
			if r := contrast(t, m[1], bg); r < 4.5 {
				t.Errorf("%s: text %s on %s = %.2f:1, want ≥ 4.5", theme, m[1], bg, r)
			}
		}
		strokes := regexp.MustCompile(`stroke="(#[0-9a-f]{6})"`).FindAllStringSubmatch(svg, -1)
		seen := map[string]bool{}
		for _, m := range strokes {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			r := contrast(t, m[1], bg)
			if r < 3 {
				t.Errorf("%s: stroke %s on %s = %.2f:1, want ≥ 3", theme, m[1], bg, r)
			}
			t.Logf("%s: stroke %s on %s = %.2f:1", theme, m[1], bg, r)
		}
		if len(seen) != 6 {
			t.Errorf("%s: %d stroke colours, want 6 (five lines, the axes)", theme, len(seen))
		}
		if fills := regexp.MustCompile(`fill="(#[0-9a-f]{6})"`).FindAllStringSubmatch(svg, -1); len(fills) != len(texts)+1 {
			t.Errorf("%s: %d fills, want the background and one per text (%d)", theme, len(fills), len(texts)+1)
		}
		t.Logf("%s: text %s on %s = %.2f:1", theme, texts[0][1], bg, contrast(t, texts[0][1], bg))
	}
}

// The ratios match the palette's ruling (R16), token by token.
func TestCurvePalette(t *testing.T) {
	want := map[string][]struct {
		hex   string
		ratio float64
	}{
		"light": {{"#9f1f15", 7.83}, {"#e0602b", 3.57}, {"#6b6b66", 5.36}, {"#3b86de", 3.72}, {"#163f7a", 10.36}, {"#1f2328", 15.80}, {"#59636e", 6.11}},
		"dark":  {{"#ffa198", 9.74}, {"#d9533f", 4.74}, {"#a3a39c", 7.46}, {"#5598e7", 6.34}, {"#b6d6fb", 12.62}, {"#e6edf3", 16.02}, {"#9198a1", 6.50}},
	}
	for theme, tokens := range want {
		c := curveThemes[theme]
		got := append(slices.Clone(c.lines[:]), c.text, c.axis)
		for i, tok := range tokens {
			if got[i] != tok.hex {
				t.Errorf("%s token %d = %s, want %s", theme, i, got[i], tok.hex)
			}
			if r := math.Round(contrast(t, tok.hex, c.bg)*100) / 100; r != tok.ratio {
				t.Errorf("%s %s on %s = %.2f:1, ruling says %.2f", theme, tok.hex, c.bg, r, tok.ratio)
			}
		}
	}
}

// Five lines, each with its own dash pattern and a label at its right end,
// in the text colour, the labels at least a line height apart.
func TestCurveLabelsEveryLine(t *testing.T) {
	for _, theme := range []string{"light", "dark"} {
		svg := string(CurveSVG(defaultCurveParams(), theme))
		dashes := regexp.MustCompile(`<polyline [^>]*stroke-dasharray="([^"]+)"`).FindAllStringSubmatch(svg, -1)
		distinct := map[string]bool{}
		for _, d := range dashes {
			distinct[d[1]] = true
		}
		if len(dashes) != 5 || len(distinct) != 5 {
			t.Errorf("%s: dash patterns %v, want five distinct", theme, dashes)
		}
		if !distinct["none"] {
			t.Errorf("%s: no solid line", theme)
		}
		labels := regexp.MustCompile(`<text class="label" x="[^"]+" y="([^"]+)" fill="(#[0-9a-f]{6})">([^<]+)</text>`).FindAllStringSubmatch(svg, -1)
		var names []string
		var ys []float64
		for _, l := range labels {
			if l[2] != curveThemes[theme].text {
				t.Errorf("%s: label %q in %s, want the text colour %s", theme, l[3], l[2], curveThemes[theme].text)
			}
			y, _ := strconv.ParseFloat(l[1], 64)
			names, ys = append(names, l[3]), append(ys, y)
		}
		if want := []string{"never", "5 %", "10 % (neutral)", "25 %", "always"}; !slices.Equal(names, want) {
			t.Fatalf("%s: labels %q, want %q", theme, names, want)
		}
		slices.Sort(ys)
		for i := 1; i < len(ys); i++ {
			if ys[i]-ys[i-1] < curveLineHeight-0.01 {
				t.Errorf("%s: labels at y %v and %v overlap (line height %v)", theme, ys[i-1], ys[i], curveLineHeight)
			}
		}
		for _, s := range []string{"<title>How a memory's rank moves</title>", "<desc>", ">times shown<", ">boost<"} {
			if !strings.Contains(svg, s) {
				t.Errorf("%s: no %q", theme, s)
			}
		}
	}
}

// Two runs draw the same bytes, and no number carries more than 2
// decimals.
func TestCurveIsStable(t *testing.T) {
	p := defaultCurveParams()
	for _, theme := range []string{"light", "dark"} {
		a, b := CurveSVG(p, theme), CurveSVG(p, theme)
		if !bytes.Equal(a, b) {
			t.Errorf("%s: two runs differ", theme)
		}
		if long := regexp.MustCompile(`\d\.\d{3,}`).FindString(string(a)); long != "" {
			t.Errorf("%s: number %s has more than 2 decimals", theme, long)
		}
	}
}

// The README shows the chart as the logo is shown, one image per GitHub
// theme, under the heading how-it-works.md links to (embedded for the
// guide, it holds no image).
func TestReadmeUsesThemeFragments(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"docs/assets/feedback-curve-light.svg#gh-light-mode-only", "docs/assets/feedback-curve-dark.svg#gh-dark-mode-only", "\n## How memory learns what helps\n"} {
		if !bytes.Contains(readme, []byte(s)) {
			t.Errorf("README.md: no %q", s)
		}
	}
	doc, err := os.ReadFile("../../docs/how-it-works.md")
	if err != nil {
		t.Fatal(err)
	}
	if url := "https://github.com/Barben360/backsight#how-memory-learns-what-helps"; !bytes.Contains(doc, []byte(url)) {
		t.Errorf("docs/how-it-works.md: no link %s", url)
	}
}

// The description says, line by line, what the data draws: each label
// with the way its line goes and where it ends.
func TestCurveDescribesItsData(t *testing.T) {
	p := defaultCurveParams()
	svg := string(CurveSVG(p, "light"))
	m := regexp.MustCompile(`<desc>([^<]+)</desc>`).FindStringSubmatch(svg)
	if m == nil {
		t.Fatal("no desc")
	}
	desc := m[1]
	for _, s := range []string{"from 0 to 50 shows", "one uncredited show in 3",
		"never: it sinks to −0.01", "5 %: it sinks to −0.01", "10 % (neutral): it stays at 0",
		"25 %: it rises to +0.02", "always: it rises to +0.06"} {
		if !strings.Contains(desc, s) {
			t.Errorf("desc %q: no %q", desc, s)
		}
	}
}

// The README's paragraph under the chart says the numbers the defaults
// and the chart give: the boost's bounds, both half-lives, the share of
// uncredited shows the chart counts as misses and the neutral credit
// rate. Changing a default or the chart without the prose fails here.
func TestReadmeCurveNumbersMatchDefaults(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(readme), "\n## How memory learns what helps\n")
	if !ok {
		t.Fatal("README.md: no section How memory learns what helps")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	f := config.Default(paths.Dirs{}, "").Feedback
	k, a, b := f.BoostWeight, f.PriorCredits, f.PriorMisses
	prior := a / (a + b) // the credit share of a memory without counts
	// With one uncredited show in m a miss, credit rate r gives the credit
	// share r / (r + (1-r)/m): the prior's at r = prior / (m(1-prior) + prior).
	neutral := prior / (curveMissEvery*(1-prior) + prior)
	words := map[string]int{"two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "twenty": 20}
	number := func(s string) float64 {
		if n, ok := words[s]; ok {
			return float64(n)
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(s, "−", "-"), 64)
		if err != nil {
			t.Fatalf("README.md: %q is not a number", s)
		}
		return v
	}
	days := func(s string) float64 {
		if s == "a year" {
			return 365
		}
		return number(strings.TrimSuffix(s, " days"))
	}
	for _, c := range []struct {
		what string
		re   string
		read func(string) float64
		want float64
	}{
		{"lowest boost", `boost stays between (−[0-9.]+) and`, number, -k * prior},
		{"highest boost", `boost stays between −[0-9.]+ and \+([0-9.]+)`, number, k * (1 - prior)},
		{"miss half-life", `Misses lose half their weight in ([0-9]+ days|a year)`, days, float64(f.MissHalfLifeDays)},
		{"credit half-life", `Misses lose half their weight in (?:[0-9]+ days|a year) and credits in ([0-9]+ days|a year)`, days, float64(f.CreditHalfLifeDays)},
		{"uncredited shows counted as a miss", `one uncredited show in ([a-z0-9]+) counted as a miss`, number, curveMissEvery},
		{"neutral credit rate", `a memory credited one time in ([a-z0-9]+) stays where`, func(s string) float64 { return 1 / number(s) }, neutral},
	} {
		m := regexp.MustCompile(c.re).FindStringSubmatch(section)
		if m == nil {
			t.Errorf("README.md: the %s is not where the test reads it (%s)", c.what, c.re)
			continue
		}
		if got := c.read(m[1]); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("README.md: %s %q, the defaults give %v", c.what, m[1], c.want)
		}
	}
	// The neutral rate is the chart's neutral line.
	drawn := false
	for _, l := range curveLines {
		drawn = drawn || strings.Contains(l.label, "neutral") && math.Abs(l.rate-neutral) < 1e-9
	}
	if !drawn {
		t.Errorf("the chart's neutral line is not at the neutral rate %v", neutral)
	}
}
