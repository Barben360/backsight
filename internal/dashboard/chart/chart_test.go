package chart

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func p(v float64) *float64 { return &v }

func ys(vs ...*float64) []Y {
	out := make([]Y, len(vs))
	for i, v := range vs {
		out[i] = Y{V: v}
	}
	return out
}

func plain(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
func pct(v float64) string   { return strconv.FormatFloat(math.Round(v*100), 'f', -1, 64) + " %" }

// every output the tests produce, for the public-text and class checks.
var outputs []string

func keep(s string) string {
	outputs = append(outputs, s)
	return s
}

func spark(label string, vs ...float64) string {
	y := make([]*float64, len(vs))
	for i := range vs {
		y[i] = p(vs[i])
	}
	return keep(string(Spark{Label: label, Y: y, Fmt: plain}.HTML()))
}

func must(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestSparkMatchesTheDesign(t *testing.T) {
	out := spark("Found per day", 24, 31, 27, 22, 30, 29, 28)
	must(t, out,
		`points="4.0,22.7 34.0,4.0 64.0,14.7 94.0,28.0 124.0,6.7 154.0,9.3 184.0,12.0"`,
		`cx="184.0" cy="12.0"`,
		`aria-label="Found per day: 24, 31, 27, 22, 30, 29, 28"`,
		`<path class="sa" d="M 4.0 31.0 L 4.0 22.7`,
		`viewBox="0 0 188 32" preserveAspectRatio="none"`)

	one := spark("Found per day", 5)
	if strings.Count(one, `class="se"`) != 1 || strings.Contains(one, "<polyline") {
		t.Errorf("one value wants one end dot and no line: %s", one)
	}

	flat := spark("Found per day", 3, 3, 3)
	if got := regexp.MustCompile(`,(\d+\.\d)`).FindAllStringSubmatch(flat, -1); len(got) != 3 {
		t.Fatalf("flat points: %s", flat)
	} else {
		for _, m := range got {
			if m[1] != "16.0" {
				t.Errorf("flat series y = %s, want 16.0", m[1])
			}
		}
	}

	var forty []float64
	for i := 1; i <= 40; i++ {
		forty = append(forty, float64(i))
	}
	must(t, spark("Found per day", forty...), `aria-label="Found per day: from 1 to 40, lowest 1, highest 40"`)
}

func TestSparkGapsAndNoData(t *testing.T) {
	out := keep(string(Spark{Label: "L", Fmt: plain, Y: []*float64{p(1), nil, p(2), p(3), p(math.NaN())}}.HTML()))
	must(t, out, `aria-label="L: 1, —, 2, 3, —"`)
	if strings.Count(out, "<polyline") != 1 {
		t.Errorf("a gap splits the line, a lone point has none: %s", out)
	}
	none := keep(string(Spark{Label: "L", Fmt: plain, Y: []*float64{nil, nil}}.HTML()))
	if strings.Contains(none, "<polyline") || strings.Contains(none, `class="se"`) || strings.Contains(none, "NaN") {
		t.Errorf("no data draws no line or dot: %s", none)
	}
}

func usedLine(x []string, v ...*float64) Line {
	return Line{
		Label: "Used per day", Size: Half, XName: "Day", X: x, Fmt: pct, Rate: true, Empty: "Nothing yet.",
		Series: []Series{{Name: "Used", Class: "s-used", Y: ys(v...)}},
	}
}

func hit(out string) []string {
	return regexp.MustCompile(`<rect class="hit"[^>]*><title>([^<]*)</title></rect>`).FindAllString(out, -1)
}

func TestLineGeometry(t *testing.T) {
	out := keep(string(usedLine([]string{"Oct 24", "Oct 25"}, p(0.5), nil).HTML()))
	must(t, out, `cy="106.0"`, `<title>Oct 24 · Used: 50 %</title>`, `<title>Oct 25 · Used: —</title>`)
	if strings.Count(out, `class="pt s-used"`) != 1 || strings.Contains(out, "<polyline") {
		t.Errorf("one present value: one circle, no line: %s", out)
	}
	if len(hit(out)) != 2 {
		t.Errorf("hit rects = %d, want 2", len(hit(out)))
	}
	must(t, out, `class="bl"`, `y1="200.0"`, `class="ta-m"`, `y="218"`, `x="38"`)

	three := keep(string(usedLine([]string{"a", "b", "c"}, p(0), p(1), p(0.5)).HTML()))
	if strings.Count(three, "<polyline") != 1 {
		t.Errorf("one run: one polyline: %s", three)
	}
	must(t, three, `cy="12.0"`)

	gap := keep(string(usedLine([]string{"a", "b", "c", "d", "e"}, p(0), p(1), nil, p(0.5), p(0.2)).HTML()))
	if strings.Count(gap, "<polyline") != 2 {
		t.Errorf("a gap: two polylines: %s", gap)
	}

	c := usedLine([]string{"Oct 24", "Oct 25"}, p(0.5), p(0.25))
	c.N, c.NName = []int64{13, 0}, "turns"
	withN := keep(string(c.HTML()))
	if strings.Count(withN, `<rect class="nb"`) != 2 {
		t.Errorf("want two n bars: %s", withN)
	}
	must(t, withN, `<title>Oct 24 · Used: 50 % · turns: 13</title>`)

	lat := Line{
		Label: "Latency", Size: Full, XName: "Day", X: []string{"a", "b"}, Fmt: plain, Empty: "x",
		Series: []Series{{Name: "Slow", Class: "s-latency", Y: ys(p(1600), p(100))}},
		Limits: []Limit{{"2.5 s", 2500, "thr"}},
	}
	l := keep(string(lat.HTML()))
	must(t, l, `<line class="thr" x1="44.0" x2="992.0" y1="106.0" y2="106.0">`, `class="thr-t ta-e"`, `>2.5 s</text>`, `>5000</text>`)
}

func TestLineTableFallback(t *testing.T) {
	out := keep(string(func() Line {
		c := usedLine([]string{"Oct 24", "Oct 25"}, p(0.5), nil)
		c.N, c.NName = []int64{3, 4}, "turns"
		return c
	}().HTML()))
	must(t, out, `<th>Day</th>`, `<th class="r">Used</th>`, `<th class="r">turns</th>`,
		`<tr><td>Oct 24</td><td class="r">50 %</td><td class="r">3</td></tr>`,
		`<tr><td>Oct 25</td><td class="r">—</td><td class="r">4</td></tr>`,
		`<summary>Show as a table</summary>`, `<details class="tblfb">`)
	if strings.Contains(out, "tbl-more") {
		t.Error("a short table has no more-line")
	}

	x := make([]string, 300)
	v := make([]*float64, 300)
	for i := range x {
		x[i], v[i] = fmt.Sprintf("d%03d", i), p(0.5)
	}
	c := usedLine(x, v...)
	c.More = "backsight stats --all"
	big := keep(string(c.HTML()))
	if got := strings.Count(big, "<tr><td>"); got != 120 {
		t.Errorf("rows = %d, want 120", got)
	}
	if !strings.Contains(big, "<td>d299</td>") || strings.Contains(big, "<td>d179</td>") || !strings.Contains(big, "<td>d180</td>") {
		t.Error("the table keeps the latest 120 rows")
	}
	must(t, big, `<p class="tbl-more">Showing the latest 120 of 300 rows. Every row: <code>backsight stats --all</code></p>`)
	if strings.Index(big, "<td>d299") < strings.Index(big, "<td>d180") {
		t.Error("rows stay in order, latest last")
	}
}

const markup = `<b>x</b>&"'`
const escaped = `&lt;b&gt;x&lt;/b&gt;&amp;&#34;&#39;`

func TestChartEscapesText(t *testing.T) {
	c := Line{
		Label: markup, Size: Half, XName: markup, X: []string{markup, markup + "2"}, Fmt: func(float64) string { return markup },
		Series: []Series{{Name: markup, Class: "s-used", Y: ys(p(1), p(2))}},
		N:      []int64{1, 2}, NName: markup, Limits: []Limit{{markup, 1, "thr"}}, More: markup, Empty: markup,
	}
	big := make([]string, 130)
	bigY := make([]*float64, 130)
	for i := range big {
		big[i], bigY[i] = markup, p(1)
	}
	cc := c
	cc.X, cc.Series = big, []Series{{Name: markup, Class: "s-used", Y: ys(bigY...)}}
	cc.N = nil
	for name, out := range map[string]string{
		"line": keep(string(c.HTML())), "capped table": keep(string(cc.HTML())),
		"spark": keep(string(Spark{Label: markup, Y: []*float64{p(1)}, Fmt: func(float64) string { return markup }}.HTML())),
		"empty": keep(string(Line{Empty: markup}.HTML())),
	} {
		if !strings.Contains(out, escaped) {
			t.Errorf("%s: markup not escaped: %s", name, out)
		}
		if strings.Contains(out, "<b>") || strings.Contains(out, `x"'`) {
			t.Errorf("%s: raw markup reaches the output: %s", name, out)
		}
	}
	if !strings.Contains(string(cc.HTML()), `<code>`+escaped+`</code>`) {
		t.Error("the more-command is not escaped")
	}
	line := keep(string(c.HTML()))
	for _, want := range []string{`aria-label="` + escaped + `"`, `<th>` + escaped, `>` + escaped + `</text>`, `<title>` + escaped} {
		if !strings.Contains(line, want) {
			t.Errorf("line lacks %q", want)
		}
	}
}

// a copy of the patterns of internal/cli/publictext_test.go.
var publicPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:decisions?|plan) \d{4}\b|\b00\d{2}\b`),
	regexp.MustCompile(`(?i)\bblocks? \d+\b`),
	regexp.MustCompile(`\bSPEC\b|§`),
	regexp.MustCompile(`\b[CEM]\d{1,2}\b`),
	regexp.MustCompile(`(?i)\bROADMAP\b|\bblock review\b|\bsecurity review [A-Z]\d`),
	regexp.MustCompile(`(?i)\btranches?\b`),
	regexp.MustCompile(`(?i)\bauthor'?s'? (?:machine|pc|computer|laptop|install)`),
	regexp.MustCompile(`(?i)\bpre-?images?\b`),
	regexp.MustCompile(`(?i)\bissues? #?\d+\b`),
}

func TestChartOutputIsPublicText(t *testing.T) {
	// Path data a careless writer would produce, which the patterns read as a code.
	if !publicPatterns[3].MatchString(`d="M4 31"`) {
		t.Fatal("the patterns no longer catch M4: update this test")
	}
	// a sparkline with a value at x 4 and a long line chart.
	s := spark("L", 1, 2, 3)
	if strings.Contains(s, "M4") || !strings.Contains(s, "M 4.0 31.0 L 4.0") {
		t.Errorf("path data needs spaces after commands: %s", s)
	}
	x := make([]string, 40)
	v := make([]*float64, 40)
	for i := range x {
		x[i], v[i] = fmt.Sprintf("Oct %d", i+1), p(float64(i%7)/7)
	}
	keep(string(usedLine(x, v...).HTML()))
	for _, out := range outputs {
		for _, re := range publicPatterns {
			if m := re.FindString(out); m != "" {
				t.Errorf("output holds %q (%s)", m, re)
			}
		}
	}
}

func many(n int) Line {
	x := make([]string, n)
	a := make([]Y, n)
	b := make([]Y, n)
	cnt := make([]int64, n)
	for i := range x {
		x[i] = fmt.Sprintf("d%d", i)
		cnt[i] = 2
		if i%2 == 0 {
			a[i], b[i] = Y{V: p(0.5), Num: 1, Den: 2}, Y{V: p(0.25), Num: 1, Den: 4}
		} else {
			a[i], b[i] = Y{V: p(0.75), Num: 3, Den: 4}, Y{V: p(0.5), Num: 1, Den: 2}
		}
	}
	return Line{
		Label: "L", Size: Full, XName: "Day", X: x, Fmt: func(v float64) string { return fmt.Sprintf("%.4f", v) },
		Rate: true, Merge: MergeRate, N: cnt, NName: "turns", Empty: "e",
		Series: []Series{{Name: "A", Class: "s-used", Y: a}, {Name: "B", Class: "s-hit", Y: b}},
	}
}

func TestLineCapsMarks(t *testing.T) {
	out := keep(string(many(2000).HTML()))
	if got := strings.Count(out, "<title>"); got != 500 {
		t.Errorf("titles = %d, want 500", got)
	}
	// first bucket: positions 0..3, rate (1+3+1+3)/(2+4+2+4) = 8/12.
	must(t, out, `<title>d0–d3 · A: 0.6667 · B: 0.3333 · turns: 8</title>`, `>d1996–d1999</text>`)
	if got := strings.Count(out, "<title>"); got != 500 {
		t.Errorf("titles = %d", got)
	}
	if got := strings.Count(keep(string(many(2001).HTML())), "<title>"); got != 401 {
		t.Errorf("2001 positions: titles = %d, want 401 (k = 5)", got)
	}
	if got := strings.Count(string(many(500).HTML()), "<title>"); got != 500 {
		t.Errorf("500 positions are not bucketed: %d", got)
	}
	if got := strings.Count(string(many(501).HTML()), "<title>"); got != 251 {
		t.Errorf("501 positions: k = 2, titles = %d, want 251", got)
	}
}

func TestLineMergeModes(t *testing.T) {
	c := many(2)
	c.X = []string{"a", "b"}
	c.Series = []Series{{Name: "S", Class: "s-used", Y: ys(p(2), p(6))}}
	c.Rate = false
	for _, tc := range []struct {
		m    Merge
		want string
	}{{MergeMean, "S: 4.0000"}, {MergeMax, "S: 6.0000"}, {MergeSum, "S: 8.0000"}} {
		// 2 positions never bucket: use the merge function on the pair directly.
		if v := merge(tc.m, c.Series[0].Y); fmt.Sprintf("S: %.4f", v.v) != tc.want {
			t.Errorf("merge %d = %v, want %s", tc.m, v, tc.want)
		}
	}
	// without a positive denominator on every position a rate is the mean.
	if v := merge(MergeRate, []Y{{V: p(0.2), Num: 1, Den: 5}, {V: p(0.6)}}); math.Abs(v.v-0.4) > 1e-9 {
		t.Errorf("rate without denominators = %v, want the mean 0.4", v)
	}
}

func TestFocusableMarks(t *testing.T) {
	mk := func(n int) string {
		x := make([]string, n)
		v := make([]*float64, n)
		for i := range x {
			x[i], v[i] = fmt.Sprintf("d%d", i), p(0.5)
		}
		return keep(string(usedLine(x, v...).HTML()))
	}
	small := mk(30)
	rects := hit(small)
	if len(rects) != 30 {
		t.Fatalf("hit rects = %d", len(rects))
	}
	for _, r := range rects {
		title := regexp.MustCompile(`<title>([^<]*)</title>`).FindStringSubmatch(r)[1]
		if !strings.Contains(r, ` tabindex="0"`) || !strings.Contains(r, `aria-label="`+title+`"`) {
			t.Errorf("hit rect not a focus stop with its title: %s", r)
		}
	}
	if regexp.MustCompile(`<svg[^>]*tabindex`).MatchString(small) {
		t.Error("with few positions the svg is not a focus stop")
	}
	big := mk(200)
	if len(hit(big)) != 200 {
		t.Fatalf("hit rects = %d", len(hit(big)))
	}
	if strings.Count(big, "tabindex") != 1 || !regexp.MustCompile(`<svg[^>]*tabindex="0"`).MatchString(big) {
		t.Error("with many positions the svg alone is the focus stop")
	}
	if strings.Count(mk(100), "tabindex") != 100 || strings.Count(mk(101), "tabindex") != 1 {
		t.Error("the focus limit is 100 positions")
	}
}

func TestPlotAloneScrolls(t *testing.T) {
	out := keep(string(usedLine([]string{"a", "b"}, p(1), p(1)).HTML()))
	must(t, out, `<svg class="vb560" viewBox="0 0 560 230"`)
	_, plot, found := strings.Cut(out, `<div class="chart-plot">`)
	if !found {
		t.Fatalf("no plot div: %s", out)
	}
	end := strings.Index(plot, "</svg></div>")
	if end < 0 {
		t.Fatalf("svg not closed inside the plot div: %s", out)
	}
	inside := plot[:end]
	if !strings.Contains(inside, "<svg") || strings.Contains(inside, `class="legend"`) || strings.Contains(inside, "<details") {
		t.Error("only the svg sits in the scrolling plot")
	}
	rest := plot[end:]
	if !strings.Contains(rest, `<div class="legend">`) || !strings.Contains(rest, `<details class="tblfb">`) {
		t.Error("legend and table sit outside the plot")
	}
	full := Full
	c := usedLine([]string{"a"}, p(1))
	c.Size = full
	must(t, string(c.HTML()), `class="vb1000" viewBox="0 0 1000 230"`)
	// legend entries.
	c.N, c.NName = []int64{1}, "turns"
	must(t, string(c.HTML()), `<span><i class="sw s-used"></i>Used</span>`, `<span><i class="sw nb-sw"></i>faint bars: turns</span>`)
}

func TestChartsSurviveDegenerateSeries(t *testing.T) {
	empty := keep(string(usedLine([]string{"a", "b"}, nil, nil).HTML()))
	must(t, empty, `<p class="empty">Nothing yet.</p>`)
	if strings.Contains(empty, "<svg") {
		t.Error("nothing drawable draws no svg")
	}
	if !strings.Contains(string(Line{Empty: "x"}.HTML()), `<p class="empty">x</p>`) {
		t.Error("no positions is empty")
	}

	bad := keep(string(usedLine([]string{"a", "b", "c", "d"}, p(math.NaN()), p(math.Inf(1)), p(math.Inf(-1)), p(0.5)).HTML()))
	if strings.Contains(bad, "NaN") || strings.Contains(bad, "Inf") {
		t.Errorf("non-finite values leak: %s", bad)
	}
	if strings.Count(bad, `class="pt `) != 1 {
		t.Errorf("non-finite values are missing points: %s", bad)
	}
	must(t, bad, `<title>a · Used: —</title>`)

	neg := Line{Label: "L", Size: Half, XName: "Day", X: []string{"a", "b"}, Fmt: plain, Empty: "e",
		Series: []Series{{Name: "S", Class: "s-used", Y: ys(p(-5), p(3))}}}
	out := keep(string(neg.HTML()))
	m := regexp.MustCompile(`<circle class="pt s-used" cx="[\d.]+" cy="([\d.-]+)"`).FindAllStringSubmatch(out, -1)
	if len(m) != 2 {
		t.Fatalf("circles: %s", out)
	}
	if y, _ := strconv.ParseFloat(m[0][1], 64); y > 200 || y < 12 {
		t.Errorf("negative value drawn at y = %v", y)
	}
	must(t, out, `<title>a · S: -5</title>`)

	zero := Line{Label: "L", Size: Half, XName: "Day", X: []string{"a", "b"}, Fmt: plain, Empty: "e", Merge: MergeRate,
		Series: []Series{{Name: "S", Class: "s-used", Y: []Y{{V: p(0.4), Num: 1, Den: 0}, {V: p(0.6), Num: 0, Den: 0}}}}}
	keep(string(zero.HTML()))
	must(t, string(zero.HTML()), `<title>a · S: 0.4</title>`)

	allEqual := keep(string(Line{Label: "L", Size: Half, XName: "D", X: []string{"a", "b"}, Fmt: plain, Empty: "e",
		Series: []Series{{Name: "S", Class: "s-used", Y: ys(p(0), p(0))}}}.HTML()))
	if strings.Contains(allEqual, "NaN") || strings.Contains(allEqual, "Inf") {
		t.Errorf("all zero: %s", allEqual)
	}
	huge := keep(string(Line{Label: "L", Size: Half, XName: "D", X: []string{"a", "b"}, Fmt: plain, Empty: "e",
		Series: []Series{{Name: "S", Class: "s-used", Y: ys(p(math.MaxFloat64), p(1))}}}.HTML()))
	if strings.Contains(huge, "NaN") || strings.Contains(huge, "Inf") {
		t.Errorf("huge: %s", huge)
	}
	// a short series and a short X: missing positions are gaps, never a panic.
	short := Line{Label: "L", Size: Half, XName: "D", X: []string{"a", "b", "c"}, Fmt: plain, Empty: "e", N: []int64{1},
		Series: []Series{{Name: "S", Class: "s-used", Y: ys(p(1))}}}
	must(t, keep(string(short.HTML())), `<title>c · S: — · : 0</title>`)
}

func TestChartClassesAreKnown(t *testing.T) {
	if len(outputs) == 0 {
		t.Skip("run with the other tests")
	}
	known := map[string]bool{}
	for _, c := range Classes {
		known[c] = true
	}
	re := regexp.MustCompile(`class="([^"]*)"`)
	for _, out := range outputs {
		for _, m := range re.FindAllStringSubmatch(out, -1) {
			for _, tok := range strings.Fields(m[1]) {
				if !known[tok] {
					t.Errorf("class %q is not in Classes", tok)
				}
			}
		}
		if strings.Contains(out, "style=") || strings.Contains(out, "<pattern") || regexp.MustCompile(`\bid=`).MatchString(out) {
			t.Errorf("style, pattern or id in the output: %s", out)
		}
	}
	// a caller's class outside the set never reaches the page.
	c := usedLine([]string{"a", "b"}, p(1), p(1))
	c.Series[0].Class = `s-used evil" onload="x`
	out := string(c.HTML())
	if strings.Contains(out, "evil") || strings.Contains(out, "onload") {
		t.Errorf("unknown class passed through: %s", out)
	}
}

func TestNiceMax(t *testing.T) {
	for in, want := range map[float64]float64{0: 1, -3: 1, 0.7: 1, 1: 2, 1600: 2000, 2500: 5000, 3: 5, 5: 10, 0.03: 0.05, math.Inf(1): 1} {
		if got := niceMax(in); math.Abs(got-want) > 1e-12 {
			t.Errorf("niceMax(%v) = %v, want %v", in, got, want)
		}
	}
}
