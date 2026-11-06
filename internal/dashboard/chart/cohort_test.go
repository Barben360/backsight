package chart

import (
	"math"
	"regexp"
	"strings"
	"testing"
)

// yieldCohort has a complete week with a share, one too few, one with no
// memory, and two not complete yet.
func yieldCohort() Cohort {
	return Cohort{Label: "Share of each week's new memories used within 28 days", Weeks: []CohortWeek{
		{Label: "Aug 4", Num: 5, Den: 12, Complete: true, Title: "Week of Aug 4: 12 new memories, too few for a share (5 used)"},
		{Label: "Aug 11", Num: 13, Den: 52, Share: p(0.25), Complete: true, Title: "Week of Aug 11: 25 % used within 28 days (13 of 52)"},
		{Label: "Aug 18", Complete: true, Title: "Week of Aug 18: no memory made"},
		{Label: "Aug 25", Num: 14, Den: 45, Share: p(0.31), Complete: true, Title: "Week of Aug 25: 31 % used within 28 days (14 of 45)"},
		{Label: "Sep 1", Den: 41, Title: "Week of Sep 1: not complete yet"},
		{Label: "Sep 8", Den: 38, Title: "Week of Sep 8: not complete yet"},
	}}
}

func TestCohort(t *testing.T) {
	out := string(yieldCohort().HTML())
	// five slots: (992-48)/6 = 157.3 each, bars 62 % of that.
	slot := (992.0 - 48) / 6
	bars := regexp.MustCompile(`<rect class="f-used" x="([\d.]+)" y="([\d.]+)" width="([\d.]+)" height="([\d.]+)" rx="2"></rect>`).FindAllStringSubmatch(out, -1)
	if len(bars) != 2 {
		t.Fatalf("bars: %s", out)
	}
	// the axis tops at 40 %: 25 % is 5/8 of the 176 high plot, 31 % is 31/40.
	near(t, "bar 1 height", num(t, bars[0][4]), 176*25/40.0)
	near(t, "bar 2 height", num(t, bars[1][4]), 176*31/40.0)
	near(t, "bar 2 top", num(t, bars[1][2]), 200-176*31/40.0)
	near(t, "bar 2 width", num(t, bars[1][3]), slot*0.62)
	near(t, "bar 2 x", num(t, bars[1][1]), 48+slot*3.5-slot*0.31)
	inc := regexp.MustCompile(`<rect class="inc" x="([\d.]+)" y="64.0" width="([\d.]+)" height="136.0" rx="2"></rect>`).FindAllStringSubmatch(out, -1)
	if len(inc) != 2 {
		t.Fatalf("dashed bands: %s", out)
	}
	near(t, "band 1 x", num(t, inc[0][1]), 48+slot*4.5-slot*0.31)
	must(t, out,
		`<text class="val ta-m" x="`+f(48+slot*3.5)+`" y="`+f(200-176*31/40.0-6)+`">31 %</text>`,
		`<text class="lbl ta-m" x="`+f(48+slot*2.5)+`" y="194.0">—</text>`,
		`<text class="few-t ta-m" x="`+f(48+slot*0.5)+`" y="194.0">too few</text>`,
		`<text class="lbl ta-m" x="`+f(48+slot*5)+`" y="236">not complete: their 28 days are not over</text>`,
		`<text class="ta-e" x="40" y="204.0">0 %</text>`, `<text class="ta-e" x="40" y="28.0">40 %</text>`,
		`<title>Week of Aug 25: 31 % used within 28 days (14 of 45)</title>`,
		`<title>Week of Aug 4: 12 new memories, too few for a share (5 used)</title>`,
		`<title>Week of Sep 8: not complete yet</title>`,
		`<tr><td>Aug 4</td><td class="r">12</td><td class="r">5</td><td class="r">too few</td></tr>`,
		`<tr><td>Aug 18</td><td class="r">0</td><td class="r">—</td><td class="r">—</td></tr>`,
		`<tr><td>Aug 25</td><td class="r">45</td><td class="r">14</td><td class="r">31 %</td></tr>`,
		`<tr><td>Sep 1</td><td class="r">41</td><td class="r">—</td><td class="r">not complete yet</td></tr>`,
		`<span><i class="sw f-used"></i>used within 28 days</span>`, `week not complete yet</span>`, `— : no memory made that week</span>`)
	if n := strings.Count(out, `class="hit"`); n != 6 {
		t.Errorf("hits: %d", n)
	}
}

func TestCohortEdges(t *testing.T) {
	if got := string(Cohort{}.HTML()); got != `<p class="empty">No weeks to show.</p>` {
		t.Errorf("empty: %s", got)
	}
	// no title given: the counts are composed; a share of 100 % tops the axis at 100.
	c := Cohort{Label: "L", Weeks: []CohortWeek{{Label: "Aug 4", Num: 30, Den: 30, Share: p(1), Complete: true}}}
	out := string(c.HTML())
	must(t, out, `<title>Aug 4: 30 of 30</title>`, `>100 %</text>`, `<rect class="f-used" x="`, `height="176.0" rx="2">`)
	// an incomplete week with no bar still has no value; the label is only drawn when one is incomplete.
	if strings.Contains(out, "not complete: their") || regexp.MustCompile(`<rect class="inc" [^>]*rx="2"`).MatchString(out) {
		t.Errorf("no incomplete week: %s", out)
	}
	// non-finite and out of range shares do not leak.
	bad := Cohort{Label: "L", Weeks: []CohortWeek{{Label: "a", Num: 1, Den: 30, Share: p(2), Complete: true}, {Label: "b", Den: 30, Share: p(math.NaN()), Complete: true}}}
	got := string(bad.HTML())
	if strings.Contains(got, "NaN") || strings.Contains(got, "Inf") || strings.Count(got, `<rect class="f-used"`) != 1 {
		t.Errorf("degenerate shares: %s", got)
	}
	must(t, got, `<text class="few-t ta-m"`, `>100 %</text>`)
	// more weeks than marks keeps the latest.
	many := make([]CohortWeek, MaxMarks+20)
	for i := range many {
		many[i] = CohortWeek{Label: "w", Num: 1, Den: 30, Share: p(0.5), Complete: true}
	}
	if n := strings.Count(string((Cohort{Label: "L", Weeks: many}).HTML()), `class="hit"`); n != MaxMarks {
		t.Errorf("hits = %d, want %d", n, MaxMarks)
	}
}

func markupCohort() string {
	return string(Cohort{Label: markup, More: markup, Weeks: []CohortWeek{
		{Label: markup, Num: 1, Den: 30, Share: p(0.5), Complete: true, Title: markup}}}.HTML())
}
