package chart

import (
	"math"
	"regexp"
	"strings"
	"testing"
)

func f2(lo, hi float64) *[2]float64 { return &[2]float64{lo, hi} }

// releasesForest is the fixture of record: two strata with an interval, two
// too few, and the pooled difference.
func releasesForest() Forest {
	return Forest{Label: "Difference in turns where Claude used memory", B: "v0.1.17-m7", Rows: []ForestRow{
		{Name: "backsight · added to the prompt", Diff: p(6.7), CI: f2(-5.8, 19.1)},
		{Name: "project-a · added to the prompt", Diff: p(15.8), CI: f2(2.9, 28.7)},
		{Name: "backsight · Claude’s search", FewText: "too few: v0.1.17-m7 has 12 turns, 30 needed"},
		{Name: "project-b · added to the prompt", FewText: "too few: v0.1.16-m7 ran 4 days, 7 needed"},
		{Name: "Matched, pooled", Diff: p(11.46), CI: f2(2.5, 20.4), Pooled: true},
	}}
}

func TestForest(t *testing.T) {
	out := string(releasesForest().HTML())
	if n := strings.Count(out, `<path class="ci-pt"`); n != 2+1 { // two squares and the legend's
		t.Errorf("squares: %d", n)
	}
	if n := strings.Count(out, `<path class="ci-pool"`); n != 1+1 {
		t.Errorf("diamonds: %d", n)
	}
	lines := regexp.MustCompile(`<line class="ci-ln" x1="([\d.]+)" x2="([\d.]+)" y1="([\d.]+)" y2="[\d.]+"><title>([^<]*)</title></line>`).FindAllStringSubmatch(out, -1)
	if len(lines) != 3 {
		t.Fatalf("intervals: %s", out)
	}
	// 10 units per point from zero at 660; rows 44 apart from 30, the pooled one 8 lower.
	for i, want := range []struct{ lo, hi, y float64 }{{602, 851, 30}, {689, 947, 74}, {685, 864, 214}} {
		near(t, "lo", num(t, lines[i][1]), want.lo)
		near(t, "hi", num(t, lines[i][2]), want.hi)
		near(t, "y", num(t, lines[i][3]), want.y)
	}
	sq := regexp.MustCompile(`<path class="ci-pt" d="M ([\d.]+) ([\d.]+) H ([\d.]+) V ([\d.]+) H ([\d.]+) Z">`).FindAllStringSubmatch(out, -1)
	if len(sq) != 2 {
		t.Fatalf("square paths: %v", sq)
	}
	near(t, "square 1 left", num(t, sq[0][1]), 660+67-5)
	near(t, "square 1 right", num(t, sq[0][3]), 660+67+5)
	near(t, "square 2 top", num(t, sq[1][2]), 74-5)
	must(t, out,
		`<title>backsight · added to the prompt: +7 pts (−6 to +19), no clear difference</title>`,
		`<title>project-a · added to the prompt: +16 pts (+3 to +29), differs</title>`,
		`<title>Matched, pooled: +11 pts (+3 to +20), differs</title>`,
		`<line class="zero" x1="660.0" x2="660.0" y1="10" y2="232.0"></line>`,
		`<text class="few-t" x="372" y="122.0">too few: v0.1.17-m7 has 12 turns, 30 needed</text>`,
		`<text class="few-t" x="372" y="166.0">too few: v0.1.16-m7 ran 4 days, 7 needed</text>`,
		`<text class="lbl ta-e" x="652" y="266.0">←  lower in v0.1.17-m7</text>`,
		`<text class="lbl" x="668" y="266.0">higher in v0.1.17-m7 →</text>`,
		`<text class="ta-m" x="360.0" y="250.0">−30 pts</text>`, `<text class="ta-m" x="810.0" y="250.0">+15 pts</text>`, `<text class="ta-m" x="660.0" y="250.0">0 pts</text>`,
		`viewBox="0 0 1000 270"`,
		`<tr><td>backsight · Claude’s search</td><td class="r">—</td><td class="r">—</td><td class="r">too few: v0.1.17-m7 has 12 turns, 30 needed</td></tr>`,
		`<tr><td>project-a · added to the prompt</td><td class="r">+16 pts</td><td class="r">+3 to +29</td><td class="r">differs</td></tr>`)
	if strings.Count(out, `class="few-t"`) != 2 {
		t.Errorf("too-few rows must be text only: %s", out)
	}
	// a too-few row has no interval or mark: 3 intervals for 5 rows was checked above.
	d := regexp.MustCompile(`<path class="ci-pool" d="M ([\d.]+) ([\d.]+) L ([\d.]+) ([\d.]+) L ([\d.]+) ([\d.]+) L ([\d.]+) ([\d.]+) Z">`).FindStringSubmatch(out)
	if d == nil {
		t.Fatalf("diamond: %s", out)
	}
	cx, cy := 660+114.6, 214.0
	for i, want := range []float64{cx - 9, cy, cx, cy - 8, cx + 9, cy, cx, cy + 8} {
		near(t, "diamond point", num(t, d[i+1]), want)
	}
}

func TestForestClampsAtTheEdges(t *testing.T) {
	c := Forest{Label: "L", B: "B", Rows: []ForestRow{
		{Name: "wide", Diff: p(40), CI: f2(-50, 80)},
		{Name: "reversed", Diff: p(-2), CI: f2(3, -7)},
	}}
	out := string(c.HTML())
	must(t, out, `<line class="ci-ln" x1="360.0" x2="960.0" y1="30.0"`, `<path class="ci-pt" d="M 955.0 25.0 H 965.0`,
		`<line class="ci-ln" x1="590.0" x2="690.0" y1="74.0"`, `<title>wide: +40 pts (−50 to +80), no clear difference</title>`,
		`<title>reversed: −2 pts (−7 to +3), no clear difference</title>`)
}

func TestForestRowsWithoutAnInterval(t *testing.T) {
	c := Forest{Label: "L", B: "B", Rows: []ForestRow{{Name: "a", Diff: p(1)}, {Name: "b", Diff: p(1), CI: f2(0, 2), FewText: "too few"}, {Name: "c", Diff: p(math.NaN()), CI: f2(0, 1)}}}
	out := string(c.HTML())
	if strings.Contains(out, "ci-ln") || strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
		t.Errorf("rows without an interval are not drawn: %s", out)
	}
	must(t, out, `<text class="few-t" x="372" y="34.0">—</text>`, `<text class="few-t" x="372" y="78.0">too few</text>`)
	if got := string(Forest{}.HTML()); got != `<p class="empty">No comparison to show.</p>` {
		t.Errorf("empty: %s", got)
	}
	// an interval that touches 0 does not differ; one that excludes it does.
	d := string(Forest{Label: "L", B: "B", Rows: []ForestRow{{Name: "a", Diff: p(2), CI: f2(0, 4)}, {Name: "b", Diff: p(2), CI: f2(0.6, 4)}}}.HTML())
	must(t, d, `<title>a: +2 pts (0 to +4), no clear difference</title>`, `<title>b: +2 pts (+1 to +4), differs</title>`)
}

func markupForest() string {
	return string(Forest{Label: markup, B: markup, More: markup, Rows: []ForestRow{
		{Name: markup, Diff: p(1), CI: f2(0, 2)}, {Name: markup, FewText: markup}}}.HTML())
}
