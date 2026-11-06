package chart

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
)

func ms(min float64) *float64 { v := min * 60000; return &v }

func funnelRanges() Ranges {
	return Ranges{
		Label: "Time for each step", WatchMS: 3600000, Empty: "Not enough sessions yet.",
		Hops: []Hop{
			{Name: "last turn → sent", P50: ms(6), P95: ms(22), N: 188},
			{Name: "sent → turned into memories", P50: ms(3), P95: ms(18), N: 181},
			{Name: "first memory recalled", P50: ms(1500), P95: ms(12960), N: 109},
		},
	}
}

// logX is the design's scale: 1 minute at 210, 14 days at 992.
func logX(min float64) float64 { return 210 + math.Log10(min)/math.Log10(20160)*782 }

func TestRangesLogScale(t *testing.T) {
	out := string(funnelRanges().HTML())
	circles := regexp.MustCompile(`<circle class="rng-p50" cx="([\d.]+)" cy="([\d.]+)" r="6"><title>([^<]*)</title></circle>`).FindAllStringSubmatch(out, -1)
	lines := regexp.MustCompile(`<line class="rng" x1="([\d.]+)" x2="([\d.]+)" y1="([\d.]+)" y2="([\d.]+)"><title>([^<]*)</title></line>`).FindAllStringSubmatch(out, -1)
	if len(circles) != 3 || len(lines) != 3 {
		t.Fatalf("%d dots, %d ranges: %s", len(circles), len(lines), out)
	}
	for i, m := range [][2]float64{{6, 22}, {3, 18}, {1500, 12960}} {
		near(t, fmt.Sprintf("hop %d p50", i), num(t, circles[i][1]), logX(m[0]))
		near(t, fmt.Sprintf("hop %d range start", i), num(t, lines[i][1]), logX(m[0]))
		near(t, fmt.Sprintf("hop %d range end", i), num(t, lines[i][2]), logX(m[1]))
		near(t, fmt.Sprintf("hop %d row", i), num(t, circles[i][2]), 30+36*float64(i))
		if lines[i][3] != circles[i][2] || lines[i][4] != circles[i][2] {
			t.Errorf("hop %d: the range is not on its row", i)
		}
	}
	if circles[0][3] != "last turn → sent: typical 6 min, slowest 1 in 20 22 min" || circles[2][3] != "first memory recalled: typical 1 d 1 h, slowest 1 in 20 9 d" {
		t.Errorf("titles: %q, %q", circles[0][3], circles[2][3])
	}
	// the ticks and the watch line sit on the same scale; the watch line is at 1 h.
	for _, tick := range []struct {
		min   float64
		label string
	}{{1, "1 min"}, {10, "10 min"}, {60, "1 h"}, {360, "6 h"}, {1440, "1 d"}, {10080, "7 d"}} {
		re := regexp.MustCompile(`<text class="ta-m" x="([\d.]+)" y="140.0">` + tick.label + `</text>`)
		m := re.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no tick %q: %s", tick.label, out)
		}
		near(t, "tick "+tick.label, num(t, m[1]), logX(tick.min))
	}
	w := regexp.MustCompile(`<line class="thr-w" x1="([\d.]+)" x2="([\d.]+)" y1="8" y2="122.0"></line><text class="lbl" x="([\d.]+)" y="118.0">watch 1 h</text>`).FindStringSubmatch(out)
	if w == nil {
		t.Fatalf("watch line: %s", out)
	}
	near(t, "watch x", num(t, w[1]), logX(60))
	near(t, "watch label x", num(t, w[3]), logX(60)+4)
	must(t, out, `viewBox="0 0 1000 150"`, `Time scale is logarithmic: each step is about ten times longer.`,
		`<tr><td>first memory recalled</td><td class="r">1 d 1 h</td><td class="r">9 d</td><td class="r">109</td></tr>`)
	if !strings.Contains(out, `<line class="bl" x1="210.0" x2="992.0" y1="122.0" y2="122.0">`) {
		t.Errorf("baseline: %s", out)
	}
}

func TestRangesPartialHops(t *testing.T) {
	c := funnelRanges()
	c.Hops[1].P95 = nil // a typical time only: the dot, no range
	c.Hops[2].P50, c.Hops[2].P95 = nil, nil
	out := string(c.HTML())
	if strings.Count(out, `class="rng-p50" cx`) != 2 || len(regexp.MustCompile(`<line class="rng" [^>]*><title>`).FindAllString(out, -1)) != 1 {
		t.Errorf("dots and ranges: %s", out)
	}
	must(t, out, `<title>sent → turned into memories: typical 3 min</title>`,
		`<tr><td>sent → turned into memories</td><td class="r">3 min</td><td class="r">—</td><td class="r">181</td></tr>`,
		`<tr><td>first memory recalled</td><td class="r">—</td><td class="r">—</td><td class="r">109</td></tr>`,
		`>first memory recalled</text>`)
	// beyond 14 days stays at the edge; below a minute at the start; a non-finite value is not drawn.
	c = Ranges{Hops: []Hop{{Name: "a", P50: ms(0.2), P95: ms(40000)}, {Name: "b", P50: p(math.NaN()), P95: p(math.Inf(1))}}}
	out = string(c.HTML())
	must(t, out, `<line class="rng" x1="210.0" x2="992.0"`)
	if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") || strings.Count(out, "rng-p50\" cx") != 1 {
		t.Errorf("degenerate: %s", out)
	}
	if strings.Contains(out, "thr-w") {
		t.Errorf("a watch line without a limit: %s", out)
	}
	// no typical time at all: the empty sentence.
	if got := string((Ranges{Hops: []Hop{{Name: "a"}}, Empty: "Not enough."}).HTML()); got != `<p class="empty">Not enough.</p>` {
		t.Errorf("empty: %s", got)
	}
	if got := string((Ranges{Empty: "Not enough."}).HTML()); got != `<p class="empty">Not enough.</p>` {
		t.Errorf("no hop: %s", got)
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[float64]string{6: "6 min", 59.6: "1 h", 60: "1 h", 160: "2 h 40 min", 1440: "1 d", 1500: "1 d 1 h", 12960: "9 d", 20160: "14 d"} {
		if got := duration(in); got != want {
			t.Errorf("duration(%v) = %q, want %q", in, got, want)
		}
	}
}

func markupRanges() string {
	return string(Ranges{Label: markup, WatchMS: 3600000, More: markup, Hops: []Hop{{Name: markup, P50: ms(5), P95: ms(9)}}}.HTML())
}

func rangeOutputs() []string {
	return []string{string(funnelRanges().HTML()), markupRanges(), string(Ranges{Empty: "e"}.HTML()),
		string(releasesForest().HTML()), markupForest(), string(Forest{}.HTML()),
		string(yieldCohort().HTML()), markupCohort(), string(Cohort{}.HTML())}
}
