package chart

import (
	"html/template"
	"math"
	"strconv"
	"strings"
)

// CohortWeek is one weekly cohort: Num of Den, drawn as Share (a fraction)
// once the week is Complete. Title is the hover and focus sentence, with the
// counts; empty, it is "<Label>: <Num> of <Den>".
type CohortWeek struct {
	Label    string
	Num, Den int64
	Share    *float64
	Complete bool
	Title    string
}

// Cohort draws weekly cohorts as bars of a share: a complete week with a
// share is a bar, an incomplete one a dashed band without a value, a week
// with nothing in it a dash and one with too few a note.
type Cohort struct {
	Label string
	Weeks []CohortWeek
	More  string
}

// cohortTops are the y axis tops in percent, each divisible by four.
var cohortTops = []float64{4, 8, 12, 20, 40, 60, 80, 100}

func (w CohortWeek) share() (float64, bool) {
	if w.Share == nil || !finite(*w.Share) {
		return 0, false
	}
	return clamp(*w.Share, 0, 1) * 100, true
}

func (w CohortWeek) title() string {
	if w.Title != "" {
		return w.Title
	}
	return w.Label + ": " + strconv.FormatInt(w.Num, 10) + " of " + strconv.FormatInt(w.Den, 10)
}

func percent(v float64) string { return strconv.FormatFloat(math.Round(v), 'f', 0, 64) + " %" }

// HTML draws the cohorts, or a sentence when there is no week.
func (c Cohort) HTML() template.HTML {
	weeks := c.Weeks
	if len(weeks) > MaxMarks {
		weeks = weeks[len(weeks)-MaxMarks:]
	}
	if len(weeks) == 0 {
		return template.HTML(emptyHTML("No weeks to show."))
	}
	const left, right, base, top = 48.0, 992.0, 200.0, 24.0
	maxShare := 0.0
	for _, w := range weeks {
		if s, ok := w.share(); ok && w.Complete && w.Den > 0 {
			maxShare = math.Max(maxShare, s)
		}
	}
	axis := cohortTops[len(cohortTops)-1]
	for _, t := range cohortTops {
		if t >= maxShare {
			axis = t
			break
		}
	}
	y := func(v float64) float64 { return base - clamp(v/axis, 0, 1)*(base-top) }
	n := float64(len(weeks))
	slot := (right - left) / n
	bw := slot * 0.62
	centre := func(i int) float64 { return left + slot*(float64(i)+0.5) }

	var b strings.Builder
	for g := 0; g <= 4; g++ {
		v := axis / 4 * float64(g)
		b.WriteString(`<line class="gl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(y(v)) + `" y2="` + f(y(v)) + `"></line>`)
		b.WriteString(`<text class="ta-e" x="40" y="` + f(y(v)+4) + `">` + percent(v) + `</text>`)
	}
	var vi []int
	var vt []string
	first, last := -1, -1
	table := make([][]string, len(weeks))
	for i, w := range weeks {
		x := centre(i) - bw/2
		s, hasShare := w.share()
		row := []string{w.Label, strconv.FormatInt(w.Den, 10), dash, dash}
		switch {
		case !w.Complete:
			// A week not over has no count to give (Den 0): it is still drawn
			// as not over, never as a week without memories.
			b.WriteString(`<rect class="inc" x="` + f(x) + `" y="` + f(top+40) + `" width="` + f(bw) + `" height="` + f(base-top-40) + `" rx="2"></rect>`)
			row[3] = "not complete yet"
			if w.Den == 0 {
				row[1] = dash
			}
			if first < 0 {
				first = i
			}
			last = i
		case w.Den == 0:
			b.WriteString(`<text class="lbl ta-m" x="` + f(centre(i)) + `" y="` + f(base-6) + `">` + dash + `</text>`)
		case !hasShare:
			b.WriteString(`<text class="few-t ta-m" x="` + f(centre(i)) + `" y="` + f(base-6) + `">too few</text>`)
			row[2], row[3] = strconv.FormatInt(w.Num, 10), "too few"
		default:
			b.WriteString(`<rect class="f-used" x="` + f(x) + `" y="` + f(y(s)) + `" width="` + f(bw) + `" height="` + f(base-y(s)) + `" rx="2"></rect>`)
			vi, vt = append(vi, i), append(vt, percent(s))
			row[2], row[3] = strconv.FormatInt(w.Num, 10), percent(s)
		}
		table[i] = row
	}
	for _, l := range pickLabels(vt, func(k int) float64 { return centre(vi[k]) }, 1000) {
		b.WriteString(`<text class="val ta-m" x="` + f(l.x) + `" y="` + f(y(shareOf(weeks[vi[l.j]]))-6) + `">` + esc(vt[l.j]) + `</text>`)
	}
	b.WriteString(`<line class="bl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(base) + `" y2="` + f(base) + `"></line>`)
	labels := make([]string, len(weeks))
	for i, w := range weeks {
		labels[i] = w.Label
	}
	for _, l := range pickLabels(labels, centre, 1000) {
		b.WriteString(`<text class="ta-m" x="` + f(l.x) + `" y="218">` + esc(labels[l.j]) + `</text>`)
	}
	if first >= 0 {
		b.WriteString(`<text class="lbl ta-m" x="` + f((centre(first)+centre(last))/2) + `" y="236">not complete: their 28 days are not over</text>`)
	}
	focus := len(weeks) <= FocusMax
	for i, w := range weeks {
		t := esc(w.title())
		b.WriteString(`<rect class="hit" x="` + f(left+slot*float64(i)) + `" y="` + f(plotTop) + `" width="` + f(slot) + `" height="` + f(base-plotTop) + `"`)
		if focus {
			b.WriteString(` tabindex="0" aria-label="` + t + `"`)
		}
		b.WriteString(`><title>` + t + `</title></rect>`)
	}

	legend := `<span><i class="sw f-used"></i>used within 28 days</span>` +
		`<span><svg viewBox="0 0 12 12" aria-hidden="true"><rect class="inc" x="1" y="1" width="10" height="10"></rect></svg>week not complete yet</span>` +
		`<span>` + dash + ` : no memory made that week</span>`
	head := []string{"Week of", "New memories", "Used within 28 days", "Share"}
	sz := Size{W: 1000, H: 240, Class: "vb1000"}
	return template.HTML(figureLegend(c.Label, sz, !focus, b.String(), legend, false, tableData{head: head, rows: table, more: c.More}))
}

func shareOf(w CohortWeek) float64 { s, _ := w.share(); return s }
