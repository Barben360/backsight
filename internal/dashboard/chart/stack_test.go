package chart

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// every output of this file, checked by the tests at its end (chart_test.go
// runs first, so its own lists cannot see these).
var stackOutputs []string

func kept(s string) string {
	stackOutputs = append(stackOutputs, s)
	return s
}

func counts(vs ...float64) []Y {
	out := make([]Y, len(vs))
	for i, v := range vs {
		if v >= 0 {
			out[i] = Y{V: p(v)}
		}
	}
	return out
}

func outcomes(x []string, used, ignored []float64) Stack {
	return Stack{
		Label: "Memories per day", Size: Half, XName: "Day", X: x, Fmt: plain, Empty: "Nothing yet.",
		Parts: []Series{{Name: "used", Class: "f-used", Y: counts(used...)}, {Name: "ignored", Class: "f-ignored", Y: counts(ignored...)}},
	}
}

var segRect = regexp.MustCompile(`<rect class="seg ([^"]*)" x="([^"]*)" y="([^"]*)" width="([^"]*)" height="([^"]*)"></rect>`)

func TestBarsStackBottomUp(t *testing.T) {
	c := outcomes([]string{"Oct 24", "Oct 25"}, []float64{3, 0}, []float64{1, 2})
	out := kept(string(c.Bars()))
	segs := segRect.FindAllStringSubmatch(out, -1)
	if len(segs) != 3 {
		t.Fatalf("want 3 segments (the zero draws none), got %d: %s", len(segs), out)
	}
	if segs[0][1] != "f-used" || segs[0][3] != "87.2" || segs[0][5] != "112.8" {
		t.Errorf("first used rect: %v", segs[0])
	}
	if segs[1][1] != "f-ignored" || segs[1][3] != "49.6" || segs[1][5] != "37.6" {
		t.Errorf("ignored stacked on used: %v", segs[1])
	}
	if segs[2][1] != "f-ignored" || segs[2][3] != "124.8" || segs[2][5] != "75.2" {
		t.Errorf("second column: %v", segs[2])
	}
	must(t, out, `<title>Oct 24 · used: 3, ignored: 1</title>`, `<title>Oct 25 · ignored: 2</title>`)
	if strings.Contains(out, "<title>used") || strings.Count(out, "<title>") != 2 {
		t.Errorf("titles sit on the hit rects only: %s", out)
	}
	if got := len(hit(out)); got != 2 {
		t.Errorf("hit rects = %d, want 2", got)
	}
	must(t, out, `<th scope="col">Day</th>`, `<th class="r" scope="col">used</th>`, `<tr><td>Oct 25</td><td class="r">0</td><td class="r">2</td></tr>`)

	none := kept(string(outcomes([]string{"a", "b"}, []float64{1, 0}, []float64{0, 0}).Bars()))
	must(t, none, `<title>b · none</title>`)
}

func TestHatchedPartHasNoFill(t *testing.T) {
	c := outcomes([]string{"a", "b"}, []float64{3, 3}, []float64{4, 0})
	c.Parts[1].Class = "f-pending"
	c.Hatch = "f-pending"
	out := kept(string(c.Bars()))
	var pend []string
	for _, s := range segRect.FindAllStringSubmatch(out, -1) {
		if s[1] == "f-pending" {
			pend = s
		}
	}
	if pend == nil {
		t.Fatalf("no pending rect: %s", out)
	}
	num := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	x0, y0, w, h := num(pend[2]), num(pend[3]), num(pend[4]), num(pend[5])
	lines := regexp.MustCompile(`<line class="hatch-ln" x1="([^"]*)" x2="([^"]*)" y1="([^"]*)" y2="([^"]*)"></line>`).FindAllStringSubmatch(out, -1)
	if len(lines) < 3 {
		t.Fatalf("a %vx%v pending bar wants several hatch lines, got %d", w, h, len(lines))
	}
	for _, l := range lines {
		a, b, c1, d := num(l[1]), num(l[2]), num(l[3]), num(l[4])
		for _, pt := range [][2]float64{{a, c1}, {b, d}} {
			if pt[0] < x0-1e-9 || pt[0] > x0+w+1e-9 || pt[1] < y0-1e-9 || pt[1] > y0+h+1e-9 {
				t.Errorf("hatch end %v outside the rect %v", pt, pend)
			}
		}
		if math.Abs((b-a)-(c1-d)) > 0.11 || b <= a {
			t.Errorf("hatch line %v is not at 45 degrees going up", l)
		}
	}
	if strings.Contains(out, "<pattern") || regexp.MustCompile(`\bid=`).MatchString(out) {
		t.Errorf("the hatch is drawn with lines, no pattern, no id: %s", out)
	}
	// the unhatched part draws no hatch.
	c.Hatch = ""
	if strings.Contains(string(c.Bars()), "hatch-ln") {
		t.Error("no Hatch, no hatch lines")
	}
}

func TestHatchIsBounded(t *testing.T) {
	x := make([]string, 40)
	v := make([]float64, 40)
	for i := range x {
		x[i], v[i] = fmt.Sprintf("d%d", i), 5
	}
	c := outcomes(x, v, nil)
	c.Size = Full
	c.Parts[0].Class, c.Hatch = "f-pending", "f-pending"
	out := string(c.Bars())
	// 40 bars of 14.7 x 94 take about 18 lines each: far over the budget without the cap.
	if n := strings.Count(out, `class="hatch-ln"`); n != maxHatchLines {
		t.Errorf("hatch lines = %d, want exactly the %d budget", n, maxHatchLines)
	}
	if strings.Count(out, `class="seg f-pending"`) != 40 {
		t.Error("a bar past the hatch budget keeps its rect")
	}
}

func hatchCount(c Stack) int { return strings.Count(string(c.Bars()), `class="hatch-ln"`) }

func TestNarrowBarsAreNotHatched(t *testing.T) {
	mk := func(n int) Stack {
		x := make([]string, n)
		v := make([]float64, n)
		for i := range x {
			x[i], v[i] = fmt.Sprintf("d%d", i), 5
		}
		c := outcomes(x, v, nil)
		c.Size = Full
		c.Parts[0].Class, c.Hatch = "f-pending", "f-pending"
		return c
	}
	// slot = 948/n, bar = 0.62 slot: 4.2 wide at 140, 3.9 wide at 150.
	if hatchCount(mk(140)) == 0 {
		t.Error("a 4.2 wide bar is hatched")
	}
	if n := hatchCount(mk(150)); n != 0 {
		t.Errorf("a 3.9 wide bar is not hatched, got %d lines", n)
	}
}

func TestValuedPartIsLabelled(t *testing.T) {
	c := Stack{
		Label: "Sessions", Size: Half, XName: "Day", X: []string{"a", "b"}, Fmt: plain, Empty: "x",
		Parts:  []Series{{Name: "sent", Class: "f-ok", Y: counts(30, 31)}, {Name: "failed", Class: "f-failed", Y: counts(2, 0)}},
		Valued: "f-failed",
	}
	out := kept(string(c.Bars()))
	if got := regexp.MustCompile(`<text class="val ta-m" [^>]*>([^<]*)</text>`).FindAllStringSubmatch(out, -1); len(got) != 1 || got[0][1] != "2 failed" {
		t.Errorf("want one label reading 2 failed: %s", out)
	}
	c.Valued = ""
	if strings.Contains(string(c.Bars()), `class="val`) {
		t.Error("no Valued, no label")
	}
}

func thirty() Stack {
	x := make([]string, 30)
	a, b, c := make([]float64, 30), make([]float64, 30), make([]float64, 30)
	for i := range x {
		x[i], a[i], b[i], c[i] = fmt.Sprintf("Oct %d", i+1), 1, 2, 3
	}
	return Stack{
		Label: "New memories", Size: Half, XName: "Day", X: x, Fmt: plain, Empty: "x",
		Parts: []Series{{Name: "summary", Class: "f-summary", Y: counts(a...)},
			{Name: "fact about you and your work", Class: "f-about", Y: counts(b...)},
			{Name: "fact from a session", Class: "f-session", Y: counts(c...)}},
	}
}

func TestAreaHasOneHitPerPosition(t *testing.T) {
	out := kept(string(thirty().Area()))
	if n := strings.Count(out, `<path class="seg`); n != 3 {
		t.Errorf("paths = %d, want 3", n)
	}
	hits := hit(out)
	if len(hits) != 30 {
		t.Fatalf("hit rects = %d, want 30", len(hits))
	}
	must(t, hits[0], `<title>Oct 1 · summary: 1, fact about you and your work: 2, fact from a session: 3</title>`)
	for _, m := range regexp.MustCompile(`<path class="seg [^"]*" d="([^"]*)"`).FindAllStringSubmatch(out, -1) {
		if !strings.HasPrefix(m[1], "M ") || strings.HasSuffix(m[1], "Z") == false || strings.Contains(m[1], "NaN") {
			t.Errorf("path data: %q", m[1])
		}
	}
	if strings.Contains(out, "<title>summary") || strings.Count(out, "<title>") != 30 {
		t.Error("titles sit on the hit rects only")
	}
	// the bands meet: the top of part 1 is the bottom of part 2 (cumulative 1 -> y 200 - 1/10*188).
	must(t, out, ` 181.2 `)
}

func TestStacksCapMarksAndKeepTotals(t *testing.T) {
	const n = 2000
	x := make([]string, n)
	parts := make([]Series, 5)
	for p := range parts {
		parts[p] = Series{Name: fmt.Sprintf("p%d", p), Class: "f-seq1", Y: make([]Y, n)}
	}
	var total int
	for i := range x {
		x[i] = fmt.Sprintf("d%04d", i)
		for p := range parts {
			v := (i*7 + p*3) % 5
			total += v
			parts[p].Y[i] = Y{V: p2(float64(v))}
		}
	}
	c := Stack{Label: "L", Size: Full, XName: "Day", X: x, Parts: parts, Fmt: plain, Empty: "x", More: "backsight stats --all"}
	for name, out := range map[string]string{"bars": kept(string(c.Bars())), "area": kept(string(c.Area()))} {
		titles := regexp.MustCompile(`<title>([^<]*)</title>`).FindAllStringSubmatch(out, -1)
		if len(titles) != MaxMarks {
			t.Errorf("%s: titles = %d, want %d", name, len(titles), MaxMarks)
		}
		var sum int
		for _, m := range titles {
			for _, kv := range regexp.MustCompile(`p\d: (\d+)`).FindAllStringSubmatch(m[1], -1) {
				v, _ := strconv.Atoi(kv[1])
				sum += v
			}
		}
		if sum != total {
			t.Errorf("%s: titles sum to %d, input %d", name, sum, total)
		}
		if got := strings.Count(out, "<tr><td>"); got != MaxTableRows {
			t.Errorf("%s: table rows = %d, want %d", name, got, MaxTableRows)
		}
		must(t, out, `<p class="tbl-more">Showing the latest 120 of 500 rows.`)
	}
}

func p2(v float64) *float64 { return &v }

func TestStacksSurviveDegenerateParts(t *testing.T) {
	nan := math.NaN()
	for name, run := range map[string]func(Stack) string{
		"bars": func(c Stack) string { return string(c.Bars()) },
		"area": func(c Stack) string { return string(c.Area()) },
	} {
		empty := Stack{Label: "L", Size: Half, XName: "Day", X: []string{"a", "b"}, Fmt: plain, Empty: "Nothing yet.",
			Parts: []Series{{Name: "u", Class: "f-used", Y: make([]Y, 2)}, {Name: "i", Class: "f-ignored"}}}
		out := kept(run(empty))
		if !strings.Contains(out, `<p class="empty">Nothing yet.</p>`) || strings.Contains(out, "<svg") {
			t.Errorf("%s: every part nil wants the empty sentence only: %s", name, out)
		}
		if out := kept(run(Stack{Empty: "none"})); !strings.Contains(out, "none") || strings.Contains(out, "<svg") {
			t.Errorf("%s: no positions: %s", name, out)
		}

		one := outcomes([]string{"only"}, []float64{4}, []float64{2})
		out = kept(run(one))
		if !strings.Contains(out, "<svg") || strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
			t.Errorf("%s: one position: %s", name, out)
		}
		if name == "bars" && strings.Count(out, `<rect class="seg`) != 2 {
			t.Errorf("one position, two parts: two rects: %s", out)
		}
		if len(hit(out)) != 1 {
			t.Errorf("%s: one hit rect, got %d", name, len(hit(out)))
		}

		bad := outcomes([]string{"a", "b", "c"}, []float64{2, 1, 1}, []float64{1, 1, 1})
		bad.Parts[0].Y[0] = Y{V: &nan}
		bad.Parts[1].Y[1] = Y{V: p2(math.Inf(1))}
		bad.Parts[1].Y[2] = Y{V: p2(-3)}
		out = kept(run(bad))
		for _, s := range []string{"NaN", "Inf"} {
			if strings.Contains(out, s) {
				t.Errorf("%s: %s in output: %s", name, s, out)
			}
		}
		must(t, out, `<title>a · ignored: 1</title>`, `<title>b · used: 1</title>`)
		if name == "bars" && strings.Count(out, `<rect class="seg`) != 3 {
			t.Errorf("bars: NaN, Inf and negative parts draw no rect, 3 remain: %s", out)
		}
	}
}

func TestStackClassesAreKnown(t *testing.T) {
	known := map[string]bool{}
	for _, c := range Classes {
		known[c] = true
	}
	re := regexp.MustCompile(`class="([^"]*)"`)
	for _, out := range stackOutputs {
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
	c := outcomes([]string{"a"}, []float64{1}, []float64{1})
	c.Parts[0].Class = `f-used evil" onload="x`
	if out := string(c.Bars()) + string(c.Area()); strings.Contains(out, "evil") || strings.Contains(out, "onload") {
		t.Errorf("unknown class passed through: %s", out)
	}
}

func TestStackEscapesText(t *testing.T) {
	c := Stack{
		Label: markup, Size: Half, XName: markup, X: []string{markup, markup + "2"}, Fmt: func(float64) string { return markup },
		Parts:  []Series{{Name: markup, Class: "f-used", Y: counts(1, 2)}, {Name: markup, Class: "f-failed", Y: counts(1, 2)}},
		Valued: "f-failed", More: markup, Empty: markup,
	}
	for name, out := range map[string]string{"bars": kept(string(c.Bars())), "area": kept(string(c.Area())),
		"empty": kept(string(Stack{Empty: markup}.Bars()))} {
		if !strings.Contains(out, escaped) {
			t.Errorf("%s: markup not escaped: %s", name, out)
		}
		if strings.Contains(out, "<b>") || strings.Contains(out, `x"'`) {
			t.Errorf("%s: raw markup reaches the output: %s", name, out)
		}
	}
	bars := string(c.Bars())
	for _, want := range []string{`aria-label="` + escaped + `"`, `<th scope="col">` + escaped, `<span><i class="sw f-used"></i>` + escaped, `>` + escaped + `</text>`, `<title>` + escaped} {
		if !strings.Contains(bars, want) {
			t.Errorf("bars lack %q", want)
		}
	}
}

func TestStackOutputIsPublicText(t *testing.T) {
	// path data keeps a space after each command, so no coordinate reads as a code.
	for _, out := range stackOutputs {
		for _, re := range publicPatterns {
			if m := re.FindString(out); m != "" {
				t.Errorf("output holds %q (%s)", m, re)
			}
		}
	}
	if len(stackOutputs) == 0 {
		t.Error("no output was checked")
	}
}

func TestThinPartsStayVisible(t *testing.T) {
	// one failed among a column of 120: 0.94 units tall.
	c := Stack{Label: "L", Size: Half, XName: "Day", X: []string{"a", "b"}, Fmt: plain, Empty: "x",
		Parts: []Series{{Name: "sent", Class: "f-ok", Y: counts(119, 119)}, {Name: "failed", Class: "f-failed", Y: counts(1, 0)}}}
	out := kept(string(c.Bars()))
	segs := segRect.FindAllStringSubmatch(out, -1)
	if len(segs) != 3 {
		t.Fatalf("segments: %s", out)
	}
	h, _ := strconv.ParseFloat(segs[1][5], 64)
	if h < 1 || !strings.HasPrefix(segs[1][1], "seg-fine f-failed") {
		t.Errorf("the thin part is 1 tall and has no stroke: %v", segs[1])
	}
	if strings.Contains(segs[0][1], "seg-fine") {
		t.Errorf("a tall wide part keeps its stroke: %v", segs[0])
	}

	// 500 bars at Full width: every non-zero part has a width and a height of at least 1.
	const n = 500
	x := make([]string, n)
	big, small := make([]float64, n), make([]float64, n)
	for i := range x {
		x[i], big[i], small[i] = fmt.Sprintf("d%d", i), 120, 1
	}
	for _, sz := range []Size{Half, Full} {
		wide := outcomes(x, big, small)
		wide.Size = sz
		out = kept(string(wide.Bars()))
		segs = segRect.FindAllStringSubmatch(out, -1)
		if len(segs) != 2*n {
			t.Fatalf("segments = %d, want %d", len(segs), 2*n)
		}
		for _, s := range segs {
			w, _ := strconv.ParseFloat(s[4], 64)
			h, _ := strconv.ParseFloat(s[5], 64)
			if w < 1 || h < 1 || !strings.Contains(s[1], "seg-fine") {
				t.Fatalf("a bar at 500 positions (%d wide) must be visible and unstroked: %v", sz.W, s)
			}
		}
	}
}

func TestStackFocusRule(t *testing.T) {
	mk := func(n int) Stack {
		x := make([]string, n)
		v := make([]float64, n)
		for i := range x {
			x[i], v[i] = fmt.Sprintf("d%d", i), 1
		}
		return outcomes(x, v, v)
	}
	for name, run := range map[string]func(Stack) string{"bars": func(c Stack) string { return string(c.Bars()) }, "area": func(c Stack) string { return string(c.Area()) }} {
		in := run(mk(100))
		if got := strings.Count(in, `<rect class="hit"`); got != 100 || strings.Count(in, `tabindex="0" aria-label=`) != 100 || strings.Contains(in, `role="img" aria-label="Memories per day" tabindex`) {
			t.Errorf("%s at 100: each hit is a stop, the svg is not: %s", name, in)
		}
		over := run(mk(101))
		if strings.Contains(over, `<rect class="hit" x="`) && strings.Contains(over, `tabindex="0" aria-label="d`) {
			t.Errorf("%s at 101: hit rects are not stops", name)
		}
		if !strings.Contains(over, `role="img" aria-label="Memories per day" tabindex="0"`) {
			t.Errorf("%s at 101: the svg is the one stop: %s", name, over)
		}
	}
}

func pathPoints(d string) [][2]float64 {
	var pts [][2]float64
	f := strings.Fields(d)
	for i := 0; i+2 < len(f); i += 3 {
		a, _ := strconv.ParseFloat(f[i+1], 64)
		b, _ := strconv.ParseFloat(f[i+2], 64)
		pts = append(pts, [2]float64{a, b})
	}
	return pts
}

func TestAreaStacksCumulatively(t *testing.T) {
	out := string(thirty().Area())
	paths := regexp.MustCompile(`<path class="seg [^"]*" d="([^"]*)"`).FindAllStringSubmatch(out, -1)
	if len(paths) != 3 {
		t.Fatalf("paths = %d", len(paths))
	}
	tops := []string{"181.2", "143.6", "87.2"} // cumulative 1, 3, 6 of a nice max of 10
	var prev [][2]float64
	for k, m := range paths {
		pts := pathPoints(m[1])
		if len(pts) != 60 {
			t.Fatalf("band %d has %d points, want 60", k, len(pts))
		}
		for i := 0; i < 30; i++ {
			if f(pts[i][1]) != tops[k] {
				t.Fatalf("band %d top at %d = %v, want %s", k, i, pts[i][1], tops[k])
			}
			base := pts[59-i][1]
			if k == 0 && base != 200 {
				t.Fatalf("band 0 sits on the baseline, got %v", base)
			}
			if k > 0 && base != prev[i][1] {
				t.Fatalf("band %d bottom %v != band %d top %v", k, base, k-1, prev[i][1])
			}
		}
		prev = pts[:30]
	}
}

func TestAreaSkipsZeroPartsAndDrawsOnePositionAsBand(t *testing.T) {
	c := outcomes([]string{"a", "b"}, []float64{2, 3}, []float64{0, 0})
	out := string(c.Area())
	if strings.Count(out, `<path class="seg`) != 1 || strings.Count(out, `<i class="sw f-ignored">`) != 1 {
		t.Errorf("an all-zero part draws no path but stays in the legend: %s", out)
	}
	must(t, out, `<th class="r" scope="col">ignored</th>`)

	one := string(outcomes([]string{"only"}, []float64{4}, []float64{2}).Area())
	paths := regexp.MustCompile(`<path class="seg [^"]*" d="([^"]*)"`).FindAllStringSubmatch(one, -1)
	if len(paths) != 2 {
		t.Fatalf("paths = %d: %s", len(paths), one)
	}
	for _, m := range paths {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, pt := range pathPoints(m[1]) {
			lo, hi = math.Min(lo, pt[0]), math.Max(hi, pt[0])
		}
		if hi-lo != 16 {
			t.Errorf("one position is a 16 wide band, got %v..%v", lo, hi)
		}
	}
}

func TestBucketedLabelsShowTheRange(t *testing.T) {
	x := make([]string, 1000)
	v := make([]float64, 1000)
	for i := range x {
		x[i], v[i] = fmt.Sprintf("d%04d", i), 1
	}
	out := string(outcomes(x, v, v).Bars())
	must(t, out, `<title>d0000–d0001 · used: 2, ignored: 2</title>`)
}

func TestValueLabelsDoNotOverlap(t *testing.T) {
	x := make([]string, 30)
	u, fl := make([]float64, 30), make([]float64, 30)
	for i := range x {
		x[i], u[i], fl[i] = fmt.Sprintf("Oct %d", i+1), 10, 3
	}
	c := Stack{Label: "L", Size: Half, XName: "Day", X: x, Fmt: plain, Empty: "x",
		Parts:  []Series{{Name: "Sent", Class: "f-ok", Y: counts(u...)}, {Name: "Failed", Class: "f-failed", Y: counts(fl...)}},
		Valued: "f-failed"}
	out := kept(string(c.Bars()))
	labels := regexp.MustCompile(`<text class="val ta-m" x="([^"]*)" y="[^"]*">([^<]*)</text>`).FindAllStringSubmatch(out, -1)
	if len(labels) < 2 || len(labels) >= 30 {
		t.Fatalf("labels = %d: thinned but present", len(labels))
	}
	prev := -1000.0
	for _, l := range labels {
		x, _ := strconv.ParseFloat(l[1], 64)
		if l[2] != "3 failed" || x < 20 || x > 540 || x-prev < 56 {
			t.Errorf("label %v at %v overlaps or clips (previous %v)", l, x, prev)
		}
		prev = x
	}
}
