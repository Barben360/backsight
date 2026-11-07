package chart

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// rbWant is the y of a time on the log axis of 50 ms (baseline 200) to 10 s (top 12).
func rbWant(ms float64) float64 { return 200 - math.Log(ms/50)/math.Log(200)*188 }

func rbSay(lo float64, hi *float64) (string, string) {
	if hi == nil {
		return "over 5 s", "over 5.0 s"
	}
	text := fmt.Sprintf("%.1f–%.1f s", lo/1000, *hi/1000)
	return "between " + strings.Replace(text, "–", " and ", 1), text
}

func rbSec(v float64) string {
	if v < 1000 {
		return strconv.FormatFloat(v, 'f', 0, 64) + " ms"
	}
	return strconv.FormatFloat(v/1000, 'f', 1, 64) + " s"
}

func rbDays() []RangeDay {
	return []RangeDay{
		{X: "Oct 28", N: 41, Lo: p(1000), Hi: p(2000), Own: p(1600)},
		{X: "Oct 29", N: 52, Lo: p(2000), Hi: p(2500), Own: p(1900)},
		{X: "Oct 30", N: 3, Lo: p(5000), Hi: nil},
	}
}

func rbChart(days ...RangeDay) RangeBars {
	return RangeBars{Label: "Slowest 1 in 20, all PCs", NName: "prompts", NOne: "prompt", Days: days, Say: rbSay, Fmt: rbSec,
		WatchMS: 2000, LimitMS: 2500, Empty: "No days.", More: "backsight stats cost --json", LegendBucket: "slowest 1 in 20, all PCs"}
}

func TestRangeBarsChart(t *testing.T) {
	out := string(rbChart(rbDays()...).HTML())
	bars := regexp.MustCompile(`<rect class="bkt" x="([\d.]+)" y="([\d.]+)" width="([\d.]+)" height="([\d.]+)"></rect>`).FindAllStringSubmatch(out, -1)
	rings := regexp.MustCompile(`<circle class="bkt-own" cx="([\d.]+)" cy="([\d.]+)" r="3.5"></circle>`).FindAllStringSubmatch(out, -1)
	if len(bars) != 3 || len(rings) != 2 {
		t.Fatalf("%d bars, %d rings: %s", len(bars), len(rings), out)
	}
	// Each bar runs from the lower to the upper edge of its bucket; the open one to the top of the axis.
	for i, e := range [][2]float64{{1000, 2000}, {2000, 2500}, {5000, 10000}} {
		y, h := num(t, bars[i][2]), num(t, bars[i][4])
		near(t, fmt.Sprintf("bar %d top", i), y, rbWant(e[1]))
		near(t, fmt.Sprintf("bar %d bottom", i), y+h, rbWant(e[0]))
	}
	// Rings are this PC's exact values, centred on their bar; the day this PC had none has no ring.
	for i, v := range []float64{1600, 1900} {
		near(t, fmt.Sprintf("ring %d y", i), num(t, rings[i][2]), rbWant(v))
		near(t, fmt.Sprintf("ring %d x", i), num(t, rings[i][1]), num(t, bars[i][1])+num(t, bars[i][3])/2)
	}
	// The watch line is at 2.0 s and the limit at 2.5 s, with block 3's dashes, and the limit says so.
	must(t, out,
		fmt.Sprintf(`<line class="thr-w" x1="44.0" x2="552.0" y1="%.1f" y2="%.1f"></line>`, rbWant(2000), rbWant(2000)),
		fmt.Sprintf(`<line class="thr" x1="44.0" x2="552.0" y1="%.1f" y2="%.1f"></line>`, rbWant(2500), rbWant(2500)),
		`>2.5 s</text>`)
	// Text is the stylesheet's 12 px at 1:1: the half-width viewBox, never a wider one scaled down.
	must(t, out, `viewBox="0 0 560 230"`, `class="vb560"`)
	if strings.Contains(out, "font-size") || strings.Contains(out, "style=") {
		t.Error("a size or style written on the markup")
	}
	// One hover rect per day, with the sentence; the day with no ring says so.
	hits := regexp.MustCompile(`<rect class="hit"[^>]*><title>([^<]*)</title></rect>`).FindAllStringSubmatch(out, -1)
	want := []string{
		"Oct 28: 41 prompts, all PCs; slowest 1 in 20 between 1.0 and 2.0 s; this PC alone 1.6 s",
		"Oct 29: 52 prompts, all PCs; slowest 1 in 20 between 2.0 and 2.5 s; this PC alone 1.9 s",
		"Oct 30: 3 prompts, all PCs; slowest 1 in 20 over 5 s; this PC had none",
	}
	if len(hits) != 3 {
		t.Fatalf("%d hover rects: %s", len(hits), out)
	}
	for i, w := range want {
		if hits[i][1] != w {
			t.Errorf("hover %d = %q, want %q", i, hits[i][1], w)
		}
	}
	// The table has every day, the open bucket in words, and the second column of text on the left.
	must(t, out, `<th scope="col">Day</th><th class="r" scope="col">Prompts, all PCs</th><th scope="col">Slowest 1 in 20, all PCs</th><th class="r" scope="col">This PC alone</th>`,
		`<td>Oct 30</td><td class="r">3</td><td>over 5.0 s</td><td class="r">—</td>`,
		`<td>Oct 28</td><td class="r">41</td><td>1.0–2.0 s</td><td class="r">1.6 s</td>`)
}

func TestRangeBarsOneDayOfOne(t *testing.T) {
	d := rbDays()[0]
	d.N = 1
	out := string(rbChart(d).HTML())
	must(t, out, `Oct 28: 1 prompt, all PCs;`)
}

func TestRangeBarsEscapesAndEmpty(t *testing.T) {
	d := RangeDay{X: `<b>"x"</b>`, N: 2, Lo: p(500), Hi: p(1000)}
	out := string(rbChart(d).HTML())
	if strings.Contains(out, "<b>") || !strings.Contains(out, "&lt;b&gt;") {
		t.Errorf("label not escaped: %s", out)
	}
	c := rbChart(d)
	c.Label = `a "label" <i>`
	c.After = ""
	if o := string(c.HTML()); strings.Contains(o, "<i>") {
		t.Errorf("chart label not escaped: %s", o)
	}
	// No day with a bucket: the sentence, no chart.
	none := string(rbChart(RangeDay{X: "Oct 1", N: 3}).HTML())
	if !strings.Contains(none, `<p class="empty">No days.</p>`) || strings.Contains(none, "<svg") {
		t.Errorf("empty: %s", none)
	}
	// A day of the first bucket is still a visible bar, and a bucket never draws outside the plot.
	low := string(rbChart(RangeDay{X: "Oct 1", N: 3, Lo: p(0), Hi: p(50), Own: p(40)}).HTML())
	m := regexp.MustCompile(`<rect class="bkt" x="[\d.]+" y="([\d.]+)" width="[\d.]+" height="([\d.]+)"></rect>`).FindStringSubmatch(low)
	if m == nil || num(t, m[2]) < 2 || num(t, m[1])+num(t, m[2]) > 200.05 {
		t.Errorf("first bucket: %v in %s", m, low)
	}
}

func TestRangeBarsLongWindow(t *testing.T) {
	var days []RangeDay
	for i := 0; i < 395; i++ {
		days = append(days, RangeDay{X: fmt.Sprintf("d%d", i), N: 5, Lo: p(1000), Hi: p(2000), Own: p(1500)})
	}
	out := string(rbChart(days...).HTML())
	// Over the hover limit no day has a hover rect or a title, the table keeps its capped rows with the command.
	if strings.Contains(out, `class="hit"`) || strings.Contains(out, "<title>") {
		t.Error("hover rects on a long window")
	}
	if got := strings.Count(out, `class="bkt"`); got != 395 {
		t.Errorf("%d bars, want one per day", got)
	}
	if rows := strings.Count(out, "<tbody><tr>") + strings.Count(out, "</tr><tr>"); rows != MaxTableRows {
		t.Errorf("%d table rows, want %d", rows, MaxTableRows)
	}
	must(t, out, "Showing the latest 120 of 395 rows.", "<code>backsight stats cost --json</code>")
	if n := strings.Count(out, "<title>"); n > MaxMarks {
		t.Errorf("%d marks", n)
	}
	// Under the hover limit but over the focus limit: hover rects, none focusable.
	mid := string(rbChart(days[:150]...).HTML())
	if strings.Count(mid, `class="hit"`) != 150 || strings.Contains(mid, `tabindex="0" aria-label`) {
		t.Error("150 days: want 150 hover rects and none focusable")
	}
	few := string(rbChart(days[:100]...).HTML())
	if strings.Count(few, `<rect class="hit" `) != 100 || strings.Count(few, `tabindex="0" aria-label`) != 100 {
		t.Error("100 days: want every hover rect a focus stop")
	}
}

func TestRangeBarsAfterClosesTheFigure(t *testing.T) {
	c := rbChart(rbDays()...)
	c.After = `<details class="tblfb"><summary>By PC</summary></details>`
	out := string(c.HTML())
	if !strings.HasSuffix(out, `</details>`+c.After+`</figure>`) {
		t.Errorf("the disclosure is not inside the figure after the table: ...%s", out[len(out)-200:])
	}
}
