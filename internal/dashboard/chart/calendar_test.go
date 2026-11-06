package chart

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// calFixture is the design's calendar: 57 weeks from Monday 2025-09-29 to
// Thursday 2026-10-30 (today), 397 days. Day i of the list holds the value
// vals[i%len(vals)] and every seventh day is not recorded.
func calFixture(vals ...int64) Calendar {
	start := time.Date(2025, 9, 29, 0, 0, 0, 0, time.UTC)
	today := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	var days []CalDay
	for d, i := start, 0; !d.After(today); d, i = d.AddDate(0, 0, 1), i+1 {
		v := vals[i%len(vals)]
		days = append(days, CalDay{Day: d, Value: v, Of: v + 20, Recorded: i%7 != 6, Today: d.Equal(today),
			InWindow: d.After(today.AddDate(0, 0, -7)), Href: "/day/" + d.Format("2006-01-02")})
	}
	return Calendar{Label: "Prompts that got memory per day", What: "prompts got memory", OfWhat: "Prompts",
		Steps: [4]int64{1, 3, 6, 10}, StepLabels: [5]string{"Prompts that got memory", "1–2", "3–5", "6–9", "10+"},
		Days: days, Window: "last 7 days", More: "backsight stats days --days 400"}
}

var cellRE = regexp.MustCompile(`<a class="hm-c[^"]*"[^>]*>`)

func cells(out string) []string { return cellRE.FindAllString(out, -1) }

func TestCalendarGrid(t *testing.T) {
	out := string(calFixture(0, 2, 5, 9, 10).HTML())
	links := cells(out)
	if len(links) != 397 {
		t.Fatalf("day links = %d, want 397", len(links))
	}
	if n := strings.Count(out, `<span class="hm-c pad`); n != 2 {
		t.Errorf("pads = %d, want 2 (the two days after today)", n)
	}
	zero := 0
	for _, a := range links {
		switch {
		case strings.Contains(a, "(today)"):
			if !strings.Contains(a, `tabindex="0"`) {
				t.Errorf("today is not the tab stop: %s", a)
			}
		case !strings.Contains(a, `tabindex="-1"`):
			t.Errorf("a day other than today is a tab stop: %s", a)
		}
		if strings.Contains(a, `tabindex="0"`) {
			zero++
		}
	}
	if zero != 1 {
		t.Errorf("tab stops = %d, want 1", zero)
	}
	// the first recorded days carry the values 0, 2, 5, 9, 10 in order.
	for i, want := range []string{"q0", "q1", "q2", "q3", "q4"} {
		if !strings.Contains(links[i], `class="hm-c `+want) {
			t.Errorf("day %d (value %d) is not %s: %s", i, []int{0, 2, 5, 9, 10}[i], want, links[i])
		}
	}
	// every seventh day is not recorded: none, whatever its value.
	if !strings.Contains(links[6], `class="hm-c none`) || !strings.Contains(links[6], "nothing recorded") {
		t.Errorf("an unrecorded day is not none: %s", links[6])
	}
	if strings.Contains(links[6], " q") {
		t.Errorf("an unrecorded day is also coloured: %s", links[6])
	}
	if n := strings.Count(out, `<span class="hm-wk hm-old"`); n != 44 {
		t.Errorf("old weeks = %d, want 44", n)
	}
	if n := strings.Count(out, `<span class="hm-wk" `); n != 13 {
		t.Errorf("recent weeks = %d, want 13", n)
	}
	// the last 13 weeks are not old: the oldest of them is Monday Aug 3 2026.
	if !strings.Contains(out, `<span class="hm-wk hm-old" aria-hidden="true">Jul 27</span>`) || !strings.Contains(out, `<span class="hm-wk" aria-hidden="true">Aug 3</span>`) {
		t.Error("the old/recent boundary is not between Jul 27 and Aug 3")
	}
	if n := strings.Count(out, ` inwin`); n < 7 {
		t.Errorf("window days = %d, want at least 7", n)
	}
	if strings.Count(out, `class="hm-c q`) < 100 || !strings.Contains(out, `<span class="hm-wk hm-old" aria-hidden="true">Sep 29</span>`) {
		t.Error("the grid lacks its week labels or cells")
	}
}

func TestCalendarSteps(t *testing.T) {
	// each measure's own steps: the first value of each colour.
	for name, tc := range map[string]struct {
		steps [4]int64
		vals  []int64
	}{
		"got":      {[4]int64{1, 3, 6, 10}, []int64{0, 1, 2, 3, 5, 6, 9, 10, 99}},
		"used":     {[4]int64{1, 2, 3, 5}, []int64{0, 1, 1, 2, 3, 4, 5, 6, 99}},
		"sessions": {[4]int64{1, 2, 4, 6}, []int64{0, 1, 1, 2, 3, 4, 5, 6, 99}},
	} {
		c := calFixture(tc.vals...)
		c.Steps = tc.steps
		for i := range c.Days {
			c.Days[i].Recorded = true
		}
		got := cells(string(c.HTML()))
		for i, v := range tc.vals {
			q := 0
			for k, s := range tc.steps {
				if v >= s {
					q = k + 1
				}
			}
			if want := fmt.Sprintf(`class="hm-c q%d`, q); !strings.Contains(got[i], want) {
				t.Errorf("%s: value %d should be q%d: %s", name, v, q, got[i])
			}
		}
	}
}

func TestCalendarLabels(t *testing.T) {
	c := calFixture(9)
	// the design's example: Wed, Oct 29 (2025) with 9 prompts of 29.
	for i := range c.Days {
		if c.Days[i].Day.Format("2006-01-02") == "2025-10-29" {
			c.Days[i].Value, c.Days[i].Of = 9, 29
		}
	}
	out := string(c.HTML())
	must(t, out,
		`aria-label="Wed, Oct 29: 9 prompts got memory, of 29" title="Wed, Oct 29: 9 prompts got memory, of 29"`,
		`aria-label="Fri, Oct 30: 9 prompts got memory, of 29 (today)" title="Fri, Oct 30: 9 prompts got memory, of 29 (today)"`,
		`aria-label="Sun, Oct 5: nothing recorded"`, `href="/day/2025-10-29"`)
	// without an "of", the label ends at the measure.
	c.OfWhat = ""
	if out := string(c.HTML()); strings.Contains(out, ", of ") || !strings.Contains(out, `aria-label="Wed, Oct 29: 9 prompts got memory"`) {
		t.Errorf("an unset OfWhat still writes the denominator")
	}
}

func TestCalendarLegendAndMonths(t *testing.T) {
	out := string(calFixture(1).HTML())
	must(t, out, `<span>Prompts that got memory:</span>`,
		`<span class="hm-c none" aria-hidden="true"></span><span>none recorded</span>`,
		`<span class="hm-c q0" aria-hidden="true"></span><span>0</span>`,
		`<span class="hm-c q1" aria-hidden="true"></span><span>1–2</span>`,
		`<span class="hm-c q4" aria-hidden="true"></span><span>10+</span>`,
		`<span class="hm-c q2 today" aria-hidden="true"></span><span>today</span>`,
		`<span>in the window (last 7 days)</span>`,
		`role="group" aria-label="Prompts that got memory per day"`)
	// Sep 2025 holds one Monday only: no label.
	must(t, out, `<span class="hm-w1"></span><span class="hm-w4">Oct</span><span class="hm-w4">Nov</span><span class="hm-w5">Dec</span>`)
}

func TestCalendarTable(t *testing.T) {
	out := string(calFixture(0, 2, 5).HTML())
	_, body, found := strings.Cut(out, "<tbody>")
	if !found {
		t.Fatal("no table body")
	}
	if n := strings.Count(body, "<tr>"); n != MaxTableRows {
		t.Fatalf("table rows = %d, want %d", n, MaxTableRows)
	}
	// the latest day first, the 120th day is Jul 3 2026.
	first := strings.Index(body, "Oct 30")
	last := strings.Index(body, "Fri, Jul 3<")
	if first < 0 || last < 0 || first > last {
		t.Errorf("rows are not the latest first: Oct 30 at %d, Jul 3 at %d", first, last)
	}
	if strings.Contains(body, "Thu, Jul 2<") {
		t.Error("the table holds more than the latest 120 days")
	}
	must(t, out, `<p class="tbl-more">The latest 120 days are here. <code>backsight stats days --days 400</code> lists every day.</p>`,
		`<th scope="col">Day</th>`, `<th class="r" scope="col">Prompts that got memory</th>`, `<th class="r" scope="col">Prompts</th>`,
		`<td><a href="/day/2026-10-30">Fri, Oct 30</a></td>`)
	// a short calendar needs no "more".
	short := calFixture(1)
	short.Days = short.Days[len(short.Days)-10:]
	if out := string(short.HTML()); strings.Contains(out, "tbl-more") || strings.Count(out, "<td><a href") != 10 {
		t.Errorf("a calendar of 10 days: %s", out)
	}
}

func TestCalendarEdges(t *testing.T) {
	if out := string(Calendar{}.HTML()); !strings.Contains(out, `class="empty"`) || strings.Contains(out, "hm-c") {
		t.Errorf("no days: %s", out)
	}
	// a calendar starting on a Wednesday pads the two days before it.
	c := calFixture(1)
	c.Days = c.Days[2:]
	out := string(c.HTML())
	if n := strings.Count(out, `<span class="hm-c pad`); n != 4 {
		t.Errorf("pads = %d, want 2 before and 2 after", n)
	}
	// more days than marks keep the latest.
	big := calFixture(1)
	for len(big.Days) < 700 {
		big.Days = append([]CalDay{{Day: big.Days[0].Day.AddDate(0, 0, -1), Recorded: true}}, big.Days...)
	}
	if n := len(cells(string(big.HTML()))); n > MaxMarks {
		t.Errorf("day links = %d, over %d", n, MaxMarks)
	}
}

// markupCalendar is a calendar whose every text is markup.
func markupCalendar() string {
	c := Calendar{Label: markup, What: markup, OfWhat: markup, StepLabels: [5]string{markup, markup, markup, markup, markup},
		Steps: [4]int64{1, 2, 3, 4}, Window: markup, More: markup}
	for i := 130; i > 0; i-- {
		c.Days = append(c.Days, CalDay{Day: time.Date(2026, 10, 31-i, 0, 0, 0, 0, time.UTC), Value: 1, Of: 1, Recorded: true, Href: markup})
	}
	return string(c.HTML())
}

func calStatusOutputs() []string {
	small := calFixture(0, 1, 4, 7, 12)
	small.Days = small.Days[len(small.Days)-30:]
	var rows []StatusRow
	for i := 0; i < 9; i++ {
		rows = append(rows, StatusRow{Name: fmt.Sprintf("Check %d of the daily upkeep", i), Days: statusDays(90, func(d int) string {
			return []string{"ok", "watch", "bad", "none"}[(d*i)%4]
		})})
	}
	few := baseStatus(StatusRow{Name: "A", Days: statusDays(6, func(i int) string { return []string{"ok", "watch", "bad", "none"}[i%4] })})
	return []string{string(calFixture(0, 2, 5, 9, 10).HTML()), string(small.HTML()), string(Calendar{}.HTML()),
		markupCalendar(), string(baseStatus(rows...).HTML()), string(few.HTML()), markupStatus(), string(StatusStrip{}.HTML())}
}
