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

func keep(s string) string { return s }

// corpus builds the outputs the public-text and class checks scan: every
// feature of the package at least once, with no dependence on other tests.
func corpus() []string {
	x := make([]string, 40)
	v := make([]*float64, 40)
	for i := range x {
		x[i], v[i] = fmt.Sprintf("Oct %d", i+1), p(float64(i%7)/7)
	}
	full := usedLine(x, v...)
	full.N, full.NName = make([]int64, 40), "turns"
	full.Limits = []Limit{{"limit", 0.9, "thr"}, {"watch", 0.5, "thr-w"}}
	full.Series = append(full.Series, Series{Name: "Said", Class: "s-coverage", Dashed: true, Y: ys(v...)})
	full.Series[0].Band = true
	for i := range full.Series[0].Y {
		full.Series[0].Y[i].Num, full.Series[0].Y[i].Den = int64(i%7), 7
	}
	capped := many(2000)
	capped.More, capped.Fmt = "backsight stats", pct
	gap := usedLine([]string{"a", "b", "c", "d", "e"}, p(0), p(1), nil, p(0.5), p(0.2))
	out := []string{
		string(full.HTML()), string(capped.HTML()), string(gap.HTML()),
		string(usedLine([]string{"a"}, nil).HTML()),
		spark("Found per day", 24, 31, 27, 22, 30, 29, 28), spark("L", 1),
		string(Spark{Label: "L", Fmt: plain, Y: []*float64{nil, p(1), p(2)}}.HTML()),
	}
	return append(out, histStripOutputs()...)
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
	for _, out := range append(corpus(), keep(string(usedLine(x, v...).HTML()))) {
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
	known := map[string]bool{}
	for _, c := range Classes {
		known[c] = true
	}
	re := regexp.MustCompile(`class="([^"]*)"`)
	for _, out := range corpus() {
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

func TestBandIsTheWilsonInterval(t *testing.T) {
	mk := func(band bool, ys ...Y) Line {
		x := make([]string, len(ys))
		for i := range x {
			x[i] = fmt.Sprintf("d%d", i)
		}
		return Line{Label: "L", Size: Half, XName: "Day", X: x, Fmt: pct, Rate: true, Empty: "e", Merge: MergeRate,
			Series: []Series{{Name: "Found", Class: "s-used", Band: band, Y: ys}}}
	}
	half := Y{V: p(0.5), Num: 5, Den: 10}
	out := string(mk(true, half, half).HTML())
	lo, hi := wilson(5, 10)
	yy := func(v float64) string { return f(200 - v*188) }
	want := `<path class="band" d="M 52.0 ` + yy(hi) + ` L 544.0 ` + yy(hi) + ` L 544.0 ` + yy(lo) + ` L 52.0 ` + yy(lo) + ` Z"></path>`
	must(t, out, want, `<span><i class="sw band-sw"></i>interval (95 %)</span>`)
	if strings.Index(out, `class="band"`) > strings.Index(out, "<polyline") {
		t.Error("the band sits under the line")
	}
	if off := string(mk(false, half, half).HTML()); strings.Contains(off, "band") {
		t.Errorf("no Band, no band: %s", off)
	}
	// a gap breaks the band; a lone position has none.
	gap := string(mk(true, half, half, Y{}, half, half, Y{}, half).HTML())
	if got := strings.Count(gap, `class="band"`); got != 1 {
		t.Errorf("one band path, one subpath per run of two (the lone position has none): %s", gap)
	}
	if d := regexp.MustCompile(`class="band" d="([^"]*)"`).FindStringSubmatch(gap)[1]; strings.Count(d, "M ") != 2 {
		t.Errorf("band subpaths = %d, want 2: %s", strings.Count(d, "M "), d)
	}
	// bucketed: the interval of the sums.
	big := many(2000)
	big.Series[0].Band = true
	b := string(big.HTML())
	if strings.Count(b, `class="band"`) != 1 || !strings.Contains(b, yy(func() float64 { _, h := wilson(8, 12); return h }())) {
		t.Errorf("a bucket's band is the interval of ΣNum/ΣDen: %.300s", b)
	}
	// a series without denominators has no band.
	plainY := mk(true, Y{V: p(0.5)}, Y{V: p(0.6)})
	if strings.Contains(string(plainY.HTML()), `class="band"`) {
		t.Error("no denominators, no interval")
	}
}

func TestDashedSeriesHasNoDots(t *testing.T) {
	c := usedLine([]string{"a", "b"}, p(0.2), p(0.4))
	c.Series[0].Dashed = true
	out := string(c.HTML())
	must(t, out, `<polyline class="ln s-used ln-2"`, `<span><i class="sw s-used ln-2"></i>Used</span>`)
	if strings.Contains(out, `class="pt`) {
		t.Errorf("a dashed series draws no dots: %s", out)
	}
}

func TestAllNilPositionIsAGapEvenWithDen(t *testing.T) {
	c := usedLine([]string{"a", "b"})
	c.Merge = MergeRate
	c.Series[0].Y = []Y{{Num: 1, Den: 2}, {Num: 3, Den: 4}}
	out := string(c.HTML())
	must(t, out, `<p class="empty">Nothing yet.</p>`)
	c.Series[0].Y = []Y{{Num: 1, Den: 2}, {V: p(0.75), Num: 3, Den: 4}}
	must(t, string(c.HTML()), `<title>a · Used: —</title>`, `<title>b · Used: 75 %</title>`)
}

func TestXLabelsNeverOverlapOrClip(t *testing.T) {
	c := many(500)
	c.Size = Half
	for i := range c.X {
		c.X[i] = fmt.Sprintf("Wed Oct %d 2026 03:00", i)
	}
	type lab struct {
		x float64
		w float64
	}
	read := func(out string) []lab {
		var ls []lab
		for _, m := range regexp.MustCompile(`<text class="ta-m" x="([\d.]+)" y="218">([^<]*)</text>`).FindAllStringSubmatch(out, -1) {
			x, _ := strconv.ParseFloat(m[1], 64)
			ls = append(ls, lab{x, float64(len([]rune(m[2]))) * 7})
		}
		return ls
	}
	ls := read(string(c.HTML()))
	if len(ls) == 0 || len(ls) > 3 {
		t.Fatalf("long labels on a half-width chart: %d labels, want 1 to 3", len(ls))
	}
	for i, l := range ls {
		if l.x-l.w/2 < 0 || l.x+l.w/2 > 560 {
			t.Errorf("label %d spans %.1f to %.1f, outside the svg", i, l.x-l.w/2, l.x+l.w/2)
		}
		if i > 0 && l.x-ls[i-1].x < (l.w+ls[i-1].w)/2 {
			t.Errorf("labels %d and %d overlap", i-1, i)
		}
	}
	// short labels keep the full six, the first and last inside the svg.
	short := read(string(many(30).HTML()))
	if len(short) != 6 {
		t.Errorf("30 short labels: %d drawn, want 6", len(short))
	}
	for _, l := range short {
		if l.x-l.w/2 < 0 || l.x+l.w/2 > 1000 {
			t.Errorf("label at %.1f clips", l.x)
		}
	}
}

func TestLineBarAndHitGeometry(t *testing.T) {
	c := usedLine([]string{"a", "b"}, p(0.5), p(0.25))
	c.N, c.NName = []int64{10, 5}, "turns"
	out := string(c.HTML())
	// own max 10 -> 30 % of the 188-high plot is 56.4; the bar width is 0.6 of the 492 step.
	must(t, out,
		`<rect class="nb" x="44.0" y="143.6" width="155.6" height="56.4"></rect>`,
		`<rect class="nb" x="396.4" y="171.8" width="155.6" height="28.2"></rect>`,
		`<rect class="hit" x="44.0" y="12.0" width="254.0" height="188.0"`,
		`<rect class="hit" x="298.0" y="12.0" width="254.0" height="188.0"`)
}

func TestMergeIgnoresDenOutsideRate(t *testing.T) {
	ys := []Y{{V: p(2), Num: 1, Den: 2}, {V: p(6), Num: 3, Den: 4}}
	for m, want := range map[Merge]float64{MergeMax: 6, MergeMean: 4, MergeSum: 8} {
		if got := merge(m, ys); !got.ok || got.v != want {
			t.Errorf("merge %d = %v, want %v", m, got, want)
		}
	}
	if got := merge(MergeRate, ys); got.v != 0.5+0.0 && got.v != 4.0/6 {
		t.Errorf("rate = %v", got)
	}
	// a non-finite value inside a bucket is skipped, not poisoning the rest.
	if got := merge(MergeMean, []Y{{V: p(1)}, {V: p(math.NaN())}, {V: p(math.Inf(1))}}); !got.ok || got.v != 1 {
		t.Errorf("mean with non-finite = %v, want 1", got)
	}
	if got := merge(MergeSum, []Y{{V: p(math.NaN())}}); got.ok {
		t.Error("only non-finite values: missing")
	}
}

func TestNonFiniteLimitsAreSkipped(t *testing.T) {
	c := Line{Label: "L", Size: Half, XName: "D", X: []string{"a", "b"}, Fmt: plain, Empty: "e",
		Series: []Series{{Name: "S", Class: "s-used", Y: ys(p(1), p(2))}},
		Limits: []Limit{{"nan", math.NaN(), "thr"}, {"inf", math.Inf(1), "thr"}, {"ok", 1, "thr-w"}}}
	out := string(c.HTML())
	if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") || strings.Contains(out, ">nan<") || strings.Contains(out, ">inf<") {
		t.Errorf("non-finite limit drawn: %s", out)
	}
	if strings.Count(out, `class="thr-w"`) != 1 || strings.Contains(out, `class="thr"`) {
		t.Errorf("only the finite limit is drawn: %s", out)
	}
	must(t, out, `>2.5</text>`) // the axis ignores the infinite limit: max 2 -> 2.5
}

func TestGeometryNumbers(t *testing.T) {
	for in, want := range map[float64]string{math.NaN(): "0.0", math.Inf(1): "0.0", math.Inf(-1): "0.0", -0.04: "0.0", 1.25: "1.2", -3: "-3.0"} {
		if got := f(in); got != want {
			t.Errorf("f(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestSparkSummaryBoundary(t *testing.T) {
	list := func(n int) string {
		vs := make([]float64, n)
		for i := range vs {
			vs[i] = float64(i + 1)
		}
		return spark("L", vs...)
	}
	if out := list(31); !strings.Contains(out, "L: 1, 2, 3") || strings.Contains(out, "from") {
		t.Errorf("31 values are listed: %.200s", out)
	}
	if out := list(32); !strings.Contains(out, "L: from 1 to 32, lowest 1, highest 32") {
		t.Errorf("32 values are summarised: %.200s", out)
	}
}

func TestSizeClassIsWhitelisted(t *testing.T) {
	c := usedLine([]string{"a", "b"}, p(1), p(1))
	c.Size = Size{560, 230, `vb560" onload="x`}
	out := string(c.HTML())
	if strings.Contains(out, "onload") {
		t.Errorf("unknown size class passed through: %s", out)
	}
}
