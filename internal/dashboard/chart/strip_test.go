package chart

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)

func baseStrip() Strip {
	return Strip{
		Label: "Every time it was shown", Size: StripSize, Rows: []string{"added to the prompt", "at session start"},
		From: t0, To: t0.AddDate(0, 0, 46), Ticks: []time.Time{t0, t0.AddDate(0, 0, 23), t0.AddDate(0, 0, 46)},
		Empty: "Never shown.", More: "backsight stats memory",
	}
}

var pathRe = regexp.MustCompile(`<path class="([^"]*)" d="([^"]*)"([^>]*)><title>([^<]*)</title></path>`)

func TestStripShapesAndRows(t *testing.T) {
	c := baseStrip()
	c.Marks = []Mark{
		{0, t0, "used", "Sep 14 · used"},
		{1, c.To, "shown", "Sep 30 · shown"},
		{0, t0.AddDate(0, 0, 40), "pending", "pending one"},
		{0, t0.AddDate(0, 0, 10), "ignored", "ignored one"},
		{1, t0.AddDate(0, 0, 12), "undeclared", "undeclared one"},
		{-1, t0.AddDate(0, 0, 20), "retired", "retired on Oct 4"},
	}
	out := string(c.HTML())
	got := map[string][]string{}
	for _, m := range pathRe.FindAllStringSubmatch(out, -1) {
		got[m[1]] = m
	}
	if len(got) != 5 {
		t.Fatalf("want 5 marks, got %v\n%s", got, out)
	}
	for class, d := range map[string]string{
		"mkp f-used":       `M 176.0 20.0 A 6 6 0 1 1 176.0 32.0 A 6 6 0 1 1 176.0 20.0 Z`,
		"mkp f-shown":      `M 992.0 60.5 A 3.5 3.5 0 1 1 992.0 67.5 A 3.5 3.5 0 1 1 992.0 60.5 Z`,
		"mkp mk-pend":      `A 5 5 0 1 1`,
		"mkp f-ignored":    `L `,
		"mkp f-undeclared": `H `,
	} {
		m, ok := got[class]
		if !ok || !strings.Contains(m[2], d) {
			t.Errorf("%s: %v", class, m)
		}
	}
	if !strings.HasPrefix(got["mkp f-undeclared"][2], "M ") || !strings.Contains(got["mkp f-undeclared"][2], " V ") ||
		strings.Contains(got["mkp f-ignored"][2], " A ") {
		t.Errorf("the undeclared mark is a square and the ignored one a diamond")
	}
	// y: rows at 26 and 64; the pending mark is on row 0, undeclared on row 1.
	if !strings.HasPrefix(got["mkp mk-pend"][2], "M ") || !strings.Contains(got["mkp mk-pend"][2], " 21.0 A 5 5") ||
		!strings.Contains(got["mkp f-undeclared"][2], " 59.0 H ") {
		t.Errorf("rows are not at 26 and 64: %v", got)
	}
	// at most 100 marks: each is a focus stop labelled with its text.
	for _, m := range got {
		if !strings.Contains(m[3], `tabindex="0" aria-label="`+m[4]+`"`) {
			t.Errorf("mark not focusable: %v", m)
		}
	}
	line := regexp.MustCompile(`<line class="thr-w" x1="([^"]*)" x2="([^"]*)" y1="8" y2="([^"]*)" tabindex="0" aria-label="retired on Oct 4"><title>retired on Oct 4</title></line>`).FindStringSubmatch(out)
	if line == nil || line[1] != line[2] || line[3] != "196.0" || line[1] != f(176+(992.0-176)*20/46) {
		t.Errorf("retirement line: %v", line)
	}
	if strings.Count(out, `tabindex="0"`) != 6 {
		t.Errorf("want a stop per mark (6), svg not a stop: %s", out)
	}
	must(t, out, `<text class="ta-e lbl" x="160.0" y="30.0">added to the prompt</text>`, `<text class="ta-e lbl" x="160.0" y="68.0">at session start</text>`,
		`<line class="gl" x1="170.0" x2="992.0" y1="26.0" y2="26.0">`,
		`<svg class="vb1000" viewBox="0 0 1000 236"`,
		`<svg width="12" height="12" viewBox="-6 -6 12 12" aria-hidden="true"><path class="f-used"`,
		`</svg>shown, then ignored</span>`, `</svg>pending</span>`, `<i class="sw sw-dash"></i>retired`,
		`<th>When</th><th>Where</th><th>What</th><th>Detail</th>`,
		`<td>all</td><td>retired</td><td>retired on Oct 4</td>`, `<td>at session start</td><td>shown only</td>`)
	// the table is in time order, the label row text aligned left.
	if strings.Contains(out, `<td class="r">`) || strings.Contains(out, `<th class="r">`) {
		t.Error("the strip table has no right-aligned column")
	}
}

func TestStripShapesAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, s := range shapeOrder {
		d := shapePath(s, 100, 50)
		if prev, dup := seen[d]; dup {
			t.Errorf("%s and %s draw the same shape", s, prev)
		}
		seen[d] = s
	}
	if len(seen) != 5 {
		t.Errorf("want five shapes, got %d", len(seen))
	}
}

func bigStrip() Strip {
	c := baseStrip()
	// bucket 0 (the first 4.9 h): 3 used and 1 ignored on row 0; a lone used show on row 1.
	c.Marks = []Mark{
		{0, t0.Add(time.Hour), "used", "u"}, {0, t0.Add(2 * time.Hour), "used", "u"}, {0, t0.Add(3 * time.Hour), "used", "u"},
		{0, t0.Add(4 * time.Hour), "ignored", "i"}, {1, t0, "used", "alone"},
	}
	for i := 0; i < 2500; i++ {
		at := t0.Add(24*time.Hour + time.Duration(i)*(44*24*time.Hour/2500))
		c.Marks = append(c.Marks, Mark{0, at, "shown", "s"}, Mark{1, at.Add(time.Minute), "shown", "s"})
	}
	for i := 0; i < 80; i++ {
		c.Marks = append(c.Marks, Mark{0, t0.Add(time.Duration(i) * time.Hour), "retired", fmt.Sprintf("retired %d", i)})
	}
	return c
}

func TestStripCapsMarks(t *testing.T) {
	out := string(bigStrip().HTML())
	if n := strings.Count(out, "<title>"); n > MaxMarks {
		t.Errorf("%d titles, cap is %d", n, MaxMarks)
	}
	if n := strings.Count(out, `<line class="thr-w"`); n != 50 {
		t.Errorf("want the latest 50 retirement lines, got %d", n)
	}
	if strings.Contains(out, `<title>retired 0</title>`) || !strings.Contains(out, `<title>retired 79</title>`) {
		t.Error("the retirements kept are not the latest")
	}
	d := pathRe.FindAllStringSubmatch(out, -1)
	// (500 - 50) / 2 = 225 buckets a row, at most.
	if len(d) > 450 || len(d) < 400 {
		t.Errorf("want about 2 x 225 bucket marks, got %d", len(d))
	}
	var four, one []string
	for _, m := range d {
		switch m[4] {
		case "Sep 14 · 4 shows: 3 used by Claude, 1 shown, then ignored":
			four = m
		case "Sep 14 · 1 show: 1 used by Claude":
			one = m
		}
	}
	if four == nil || four[1] != "mkp f-used" || strings.Contains(four[2], " L ") {
		t.Errorf("3 used + 1 ignored must be a used circle: %v", four)
	}
	if one == nil || one[1] != "mkp f-used" {
		t.Errorf("a bucket of one show: %v", one)
	}
	if strings.Count(out, "<tr>") != MaxTableRows+1 || !strings.Contains(out, `class="tbl-more"`) ||
		!strings.Contains(out, `<code>backsight stats memory</code>`) {
		t.Errorf("table: %d rows", strings.Count(out, "<tr>")-1)
	}
	// more than 100 marks: the svg alone is a stop.
	if strings.Count(out, "tabindex") != 1 || !strings.Contains(out, `aria-label="Every time it was shown" tabindex="0">`) {
		t.Errorf("past %d marks the svg alone is a stop", FocusMax)
	}
}

func TestStripBucketTiesAndRange(t *testing.T) {
	c := baseStrip()
	c.Marks = []Mark{
		{0, t0.Add(time.Hour), "shown", "a"}, {0, t0.Add(25 * time.Hour), "ignored", "b"}, {0, t0.Add(26 * time.Hour), "used", "c"},
	}
	g := c.bucket(c.Marks, 0)
	// one each: the tie goes to used, then ignored, then shown.
	if g.shape != "used" || g.text != "Sep 14–Sep 15 · 3 shows: 1 used by Claude, 1 shown, then ignored, 1 shown only" {
		t.Errorf("%+v", g)
	}
	if g := c.bucket([]Mark{{0, t0, "pending", "p"}, {0, t0, "undeclared", "u"}}, 0); g.shape != "undeclared" {
		t.Errorf("undeclared beats pending: %+v", g)
	}
}

func TestStripSurvivesDegenerateMarks(t *testing.T) {
	c := baseStrip()
	if out := string(c.HTML()); out != `<p class="empty">Never shown.</p>` {
		t.Errorf("no marks: %s", out)
	}
	c.Marks = []Mark{{7, t0, "used", "no such row"}, {0, t0, "weird", "no such shape"}}
	if out := string(c.HTML()); out != `<p class="empty">Never shown.</p>` {
		t.Errorf("unusable marks: %s", out)
	}
	z := c
	z.Rows = nil
	z.Marks = []Mark{{0, t0, "retired", "r"}}
	if out := string(z.HTML()); out != `<p class="empty">Never shown.</p>` {
		t.Errorf("no rows: %s", out)
	}
	c.Marks = []Mark{{0, t0, "used", "a"}, {1, t0.AddDate(0, 0, 9), "shown", "b"}}
	c.To = c.From
	out := string(c.HTML())
	for _, m := range pathRe.FindAllStringSubmatch(out, -1) {
		if !strings.HasPrefix(m[2], "M 992.0 ") {
			t.Errorf("From == To puts every mark at 992: %v", m)
		}
	}
	if strings.Count(out, "<path class=\"mkp") != 2 || strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
		t.Errorf("From == To: %s", out)
	}
	c = baseStrip()
	c.Marks = []Mark{{0, c.From.AddDate(0, 0, -10), "used", "before"}, {0, c.To.AddDate(0, 0, 10), "used", "after"}, {0, time.Time{}, "shown", "zero"}}
	got := map[string]string{}
	for _, m := range pathRe.FindAllStringSubmatch(string(c.HTML()), -1) {
		got[m[4]] = m[2]
	}
	if !strings.HasPrefix(got["before"], "M 176.0 ") || !strings.HasPrefix(got["after"], "M 992.0 ") || !strings.HasPrefix(got["zero"], "M 176.0 ") {
		t.Errorf("marks outside the range are clamped to its edges: %v", got)
	}
	// a To before From is no range.
	c.To = c.From.AddDate(0, 0, -5)
	if out := string(c.HTML()); strings.Contains(out, "NaN") || !strings.Contains(out, `d="M 992.0 `) {
		t.Errorf("reversed range: %s", out)
	}
}

func TestStripTicksAreThinnedAndKept(t *testing.T) {
	c := baseStrip()
	c.Marks = []Mark{{0, t0, "used", "a"}}
	c.Ticks = nil
	for i := 0; i < 46; i++ {
		c.Ticks = append(c.Ticks, t0.AddDate(0, 0, i))
	}
	c.Ticks = append([]time.Time{t0.AddDate(0, 0, -3)}, c.Ticks...)
	c.Ticks = append(c.Ticks, t0.AddDate(0, 0, 90))
	out := string(c.HTML())
	n := strings.Count(out, `y="228.0">`)
	if n < 2 || n > 6 || !strings.Contains(out, `>Sep 15</text>`) && !strings.Contains(out, `>Sep 14</text>`) {
		t.Errorf("want 2 to 6 tick labels from the first tick, got %d", n)
	}
	for _, bad := range []string{"Sep 11", "Dec 13"} {
		if strings.Contains(out, ">"+bad+"</text>") {
			t.Errorf("tick %s is outside the range and is drawn", bad)
		}
	}
	c.TickFmt = func(t time.Time) string { return "D" + t.Format("2") }
	if !strings.Contains(string(c.HTML()), ">D14</text>") {
		t.Error("TickFmt is ignored")
	}
}

func TestStripRowsGrowTheViewBox(t *testing.T) {
	c := baseStrip()
	c.Rows = []string{"a", "b", "c", "d", "e", "f", "g"}
	c.Marks = []Mark{{6, t0, "used", "x"}}
	out := string(c.HTML())
	// the last row is at 26 + 6 x 38 = 254; the baseline 18 below, the labels 32 below that.
	must(t, out, `viewBox="0 0 1000 312"`, `d="M 176.0 248.0 A 6 6`, `y1="272.0" y2="272.0"`, `y="304.0">`)
}

func TestStripEscapesText(t *testing.T) {
	c := baseStrip()
	c.Label, c.Rows, c.More = markup, []string{markup, "<script>alert(1)</script>"}, markup
	c.Marks = []Mark{{0, t0, "used", markup}, {1, t0, "shown", "<script>alert(1)</script>"}, {0, t0, "retired", "<script>x</script>"}}
	c.TickFmt = func(time.Time) string { return markup }
	for _, out := range []string{string(c.HTML()), string(Strip{Empty: markup}.HTML())} {
		if !strings.Contains(out, escaped) || strings.Contains(out, "<script>") || strings.Contains(out, "<b>") {
			t.Errorf("markup is not escaped: %s", out)
		}
	}
	out := string(c.HTML())
	for _, want := range []string{`aria-label="` + escaped + `"`, `<title>` + escaped, `>` + escaped + `</text>`, `<td>` + escaped + `</td>`,
		`&lt;script&gt;alert(1)&lt;/script&gt;`} {
		if !strings.Contains(out, want) {
			t.Errorf("strip lacks %q", want)
		}
	}
}

// histStripOutputs feeds corpus(): one of every feature, so the class and
// public-text checks scan histograms and strips too.
func histStripOutputs() []string {
	h := rankHist()
	h.Markers = []Marker{{"typical", 2.5, "thr-w"}, {"high", 4.5, "thr"}, {"edge", 3, "mk"}}
	many := rankHist()
	many.Bins = nil
	for i := 0; i < 600; i++ {
		many.Bins = append(many.Bins, Bin{fmt.Sprintf("%d", i), int64(i%9 + 1)})
	}
	s := bigStrip()
	small := baseStrip()
	small.Marks = []Mark{{0, t0, "used", "u"}, {1, t0.AddDate(0, 0, 3), "shown", "s"}, {0, t0.AddDate(0, 0, 5), "pending", "p"},
		{0, t0.AddDate(0, 0, 6), "ignored", "i"}, {1, t0.AddDate(0, 0, 7), "undeclared", "n"}, {0, t0.AddDate(0, 0, 8), "retired", "r"}}
	odd := baseStrip()
	odd.Marks = []Mark{{0, t0, "used", markup}}
	return []string{string(h.HTML()), string(many.HTML()), string(Hist{Empty: "none"}.HTML()),
		string(s.HTML()), string(small.HTML()), string(odd.HTML()), string(Strip{Empty: "none"}.HTML())}
}

func TestStripRowNamesAreRightAlignedAndTruncated(t *testing.T) {
	c := baseStrip()
	long := "a row name that is far too long"
	c.Rows = []string{long, "short"}
	c.Marks = []Mark{{0, t0, "used", "x"}, {1, t0, "used", "y"}}
	out := string(c.HTML())
	must(t, out, `<text class="ta-e lbl" x="160.0" y="30.0">a row name that is f…<title>`+long+`</title></text>`,
		`<text class="ta-e lbl" x="160.0" y="68.0">short</text>`, `<td>`+long+`</td>`)
	if strings.Contains(out, ">"+long+"</text>") {
		t.Error("the long name is drawn whole")
	}
	c.Rows[0] = strings.Repeat("é", maxName+1)
	if out := string(c.HTML()); !strings.Contains(out, strings.Repeat("é", maxName-1)+"…<title>") {
		t.Errorf("a name of %d runes is cut to %d and an ellipsis", maxName+1, maxName)
	}
	// exactly maxName runes pass whole.
	c.Rows[0] = strings.Repeat("é", maxName)
	if out := string(c.HTML()); strings.Contains(out, "…") {
		t.Errorf("a name of %d runes is cut", maxName)
	}
}

func TestStripLegendIsAbovePlotAndCreatedAndRetiredAreLabelled(t *testing.T) {
	c := baseStrip()
	c.Created = t0.AddDate(0, 0, 23)
	c.Marks = []Mark{{0, t0.AddDate(0, 0, 30), "used", "u"}, {0, t0.AddDate(0, 0, 10), "retired", "first"}, {0, t0.AddDate(0, 0, 20), "retired", "last"}}
	out := string(c.HTML())
	if strings.Index(out, `class="legend"`) > strings.Index(out, `<svg class="vb1000"`) {
		t.Error("the legend is after the plot")
	}
	x := f(176 + (992.0-176)*23/46)
	must(t, out, `<line class="mk" x1="`+x+`" x2="`+x+`" y1="8" y2="196.0"></line><text class="lbl" x="`+f(176+(992.0-176)*23/46+4)+`" y="208.0">created</text>`,
		`<text class="ta-e lbl" x="`+f(176+(992.0-176)*20/46-4)+`" y="208.0">retired</text>`)
	c.Created = time.Time{}
	if strings.Contains(string(c.HTML()), `class="mk"`) || strings.Contains(string(c.HTML()), ">created<") {
		t.Error("no creation time, no line")
	}
	c.Created = c.To.AddDate(0, 0, 1)
	if strings.Contains(string(c.HTML()), `class="mk"`) {
		t.Error("a creation outside the range is drawn")
	}
	c.Created = c.To
	if o := string(c.HTML()); !strings.Contains(o, `<text class="ta-e lbl" x="988.0" y="208.0">created</text>`) {
		t.Errorf("a late creation is labelled to the left: %s", o)
	}
}

func TestStripBoundsRowsByTheMarkCap(t *testing.T) {
	c := baseStrip()
	c.Rows = nil
	for i := 0; i < 700; i++ {
		c.Rows = append(c.Rows, fmt.Sprintf("row %d is quite a long row name", i))
		c.Marks = append(c.Marks, Mark{i, t0.Add(time.Duration(i) * time.Hour), "used", fmt.Sprintf("mark %d", i)})
	}
	for i := 0; i < 80; i++ {
		c.Marks = append(c.Marks, Mark{0, t0.Add(time.Duration(i) * time.Minute), "retired", "r"})
	}
	out := string(c.HTML())
	if n := strings.Count(out, "<title>"); n > MaxMarks {
		t.Errorf("%d titles with 700 rows", n)
	}
	drawn := strings.Count(out, `<line class="gl"`)
	if drawn < 100 || drawn >= 700 {
		t.Errorf("rows drawn: %d", drawn)
	}
	must(t, out, `<span>`+strconv.Itoa(700-drawn)+` more rows in the table</span>`)
	if strings.Contains(out, "<title>mark 699</title>") || !strings.Contains(out, "<title>mark 0</title>") {
		t.Error("marks of the rows not drawn are drawn, or the first row is not")
	}
	// the marks of rows that are not drawn stay in the table.
	if !strings.Contains(out, "Showing the latest 120 of 780 rows") {
		t.Error("the table does not hold every mark")
	}
	c.Rows, c.Marks = c.Rows[:1], c.Marks[:1]
	if strings.Contains(string(c.HTML()), "more row") {
		t.Error("nothing is hidden, nothing is said")
	}
	two := baseStrip()
	two.Rows = make([]string, 451)
	two.Marks = []Mark{{450, t0, "used", "last"}}
	if !strings.Contains(string(two.HTML()), "338 more rows in the table") && !strings.Contains(string(two.HTML()), "more rows in the table") {
		t.Error("rows past the cap are not announced")
	}
}

func TestStripOneHiddenRowIsSingular(t *testing.T) {
	c := baseStrip()
	c.Rows = make([]string, 113) // 500/4 = 125 rows fit with no retirement; with 50 retirements 112 do
	c.Marks = []Mark{{0, t0, "used", "a"}}
	for i := 0; i < 50; i++ {
		c.Marks = append(c.Marks, Mark{0, t0, "retired", "r"})
	}
	must(t, string(c.HTML()), "<span>1 more row in the table</span>")
}

func TestStripOtherShapesHaveTheirGeometry(t *testing.T) {
	c := baseStrip()
	c.Marks = []Mark{{0, t0, "ignored", "d"}, {1, c.To, "undeclared", "q"}}
	out := string(c.HTML())
	must(t, out, `d="M 176.0 19.0 L 183.0 26.0 L 176.0 33.0 L 169.0 26.0 Z"`, `d="M 987.0 59.0 H 997.0 V 69.0 H 987.0 Z"`)
}

func TestStripBucketIsAtTheMeanOfItsShows(t *testing.T) {
	c := baseStrip()
	c.To = t0.AddDate(0, 0, 40)
	c.Rows = c.Rows[:1]
	// 602 shows on one row make 500 buckets of 1.9 h over 40 days; two shows 12 and
	// 84 min in share the first and sit at their mean, 48 min.
	for i := 0; i < 600; i++ {
		c.Marks = append(c.Marks, Mark{0, t0.Add(40*24*time.Hour - time.Duration(600-i)*time.Minute), "shown", "s"})
	}
	c.Marks = append(c.Marks, Mark{0, t0.Add(12 * time.Minute), "used", "a"}, Mark{0, t0.Add(84 * time.Minute), "used", "b"})
	out := string(c.HTML())
	want := f(176 + (992.0-176)*float64(48*time.Minute)/float64(40*24*time.Hour))
	if !strings.Contains(out, ` d="M `+want+` 20.0 A 6 6`) {
		t.Errorf("bucket not at x %s:\n%s", want, out)
	}
}

func TestStripTableIsInTimeOrderWhateverTheInput(t *testing.T) {
	c := baseStrip()
	c.Marks = []Mark{{0, t0.AddDate(0, 0, 9), "used", "late"}, {0, t0.AddDate(0, 0, 2), "used", "early"}}
	out := string(c.HTML())
	if strings.Index(out, "<td>early</td>") > strings.Index(out, "<td>late</td>") {
		t.Error("the table is not in time order")
	}
}

func TestStripFocusCountsRetirements(t *testing.T) {
	c := baseStrip()
	for i := 0; i < 99; i++ {
		c.Marks = append(c.Marks, Mark{0, t0.Add(time.Duration(i) * time.Hour), "used", "u"})
	}
	if strings.Count(string(c.HTML()), "tabindex") != 99 {
		t.Error("99 marks: each a stop")
	}
	for i := 0; i < 50; i++ {
		c.Marks = append(c.Marks, Mark{0, t0.Add(time.Duration(i) * time.Hour), "retired", "r"})
	}
	if n := strings.Count(string(c.HTML()), "tabindex"); n != 1 {
		t.Errorf("99 marks and 50 retirements are 149 stops: the svg alone is one, got %d", n)
	}
}
