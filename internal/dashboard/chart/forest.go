package chart

import (
	"html/template"
	"math"
	"strconv"
	"strings"
)

// ForestRow is one stratum (or the pooled difference): Diff and CI, the 95 %
// interval, are in percentage points, B minus the other version. A row with a
// FewText is not drawn as an interval: it says why in that sentence.
type ForestRow struct {
	Name    string
	Diff    *float64
	CI      *[2]float64
	Pooled  bool
	FewText string
}

// Forest draws the differences of version B against the other with their 95 %
// intervals, one row each, and the zero line.
type Forest struct {
	Label, B string
	Rows     []ForestRow
	More     string
}

const (
	forestL, forestR, forestZero = 360.0, 960.0, 660.0
	forestPerPoint               = 10.0 // user units per percentage point: the plot spans ±30 points
)

func forestX(pts float64) float64 {
	return clamp(forestZero+pts*forestPerPoint, forestL, forestR)
}

// points writes a number of points signed, with a true minus: "+16", "−6", "0".
func points(v float64) string {
	r := math.Round(v)
	switch {
	case r > 0:
		return "+" + strconv.FormatFloat(r, 'f', 0, 64)
	case r < 0:
		return "−" + strconv.FormatFloat(-r, 'f', 0, 64)
	}
	return "0"
}

// HTML draws the forest, or a sentence when there is no row.
func (c Forest) HTML() template.HTML {
	rows := c.Rows
	if len(rows) > MaxMarks {
		rows = rows[:MaxMarks]
	}
	if len(rows) == 0 {
		return template.HTML(emptyHTML("No comparison to show."))
	}
	ys := make([]float64, len(rows))
	for i, r := range rows {
		ys[i] = 30 + 44*float64(i)
		if r.Pooled {
			ys[i] += 8
		}
	}
	base := ys[len(ys)-1] + 18
	var b strings.Builder
	for _, v := range []float64{-30, -15, 0, 15, 30} {
		x := forestZero + v*forestPerPoint
		label := points(v) + " pts"
		if v == 0 {
			label = "0 pts"
		}
		b.WriteString(`<line class="gl" x1="` + f(x) + `" x2="` + f(x) + `" y1="10" y2="` + f(base) + `"></line>`)
		b.WriteString(`<text class="ta-m" x="` + f(x) + `" y="` + f(base+18) + `">` + label + `</text>`)
	}
	b.WriteString(`<line class="zero" x1="` + f(forestZero) + `" x2="` + f(forestZero) + `" y1="10" y2="` + f(base) + `"></line>`)
	head := []string{"Group", "Difference", "95 % interval", "Reading"}
	table := make([][]string, 0, len(rows))
	for i, r := range rows {
		y := ys[i]
		b.WriteString(`<text class="ta-e lbl" x="340" y="` + f(y+4) + `">` + esc(r.Name) + `</text>`)
		drawn := r.FewText == "" && r.Diff != nil && r.CI != nil && finite(*r.Diff) && finite(r.CI[0]) && finite(r.CI[1])
		if !drawn {
			text := r.FewText
			if text == "" {
				text = dash
			}
			b.WriteString(`<text class="few-t" x="372" y="` + f(y+4) + `">` + esc(text) + `</text>`)
			table = append(table, []string{r.Name, dash, dash, text})
			continue
		}
		lo, hi := math.Min(r.CI[0], r.CI[1]), math.Max(r.CI[0], r.CI[1])
		reading := "no clear difference"
		if lo > 0 || hi < 0 {
			reading = "differs"
		}
		ci := points(lo) + " to " + points(hi)
		title := r.Name + ": " + points(*r.Diff) + " pts (" + ci + "), " + reading
		x := forestX(*r.Diff)
		b.WriteString(`<line class="ci-ln" x1="` + f(forestX(lo)) + `" x2="` + f(forestX(hi)) + `" y1="` + f(y) + `" y2="` + f(y) + `"><title>` + esc(title) + `</title></line>`)
		if r.Pooled {
			b.WriteString(`<path class="ci-pool" d="M ` + f(x-9) + ` ` + f(y) + ` L ` + f(x) + ` ` + f(y-8) + ` L ` + f(x+9) + ` ` + f(y) + ` L ` + f(x) + ` ` + f(y+8) + ` Z"><title>` + esc(title) + `</title></path>`)
		} else {
			b.WriteString(`<path class="ci-pt" d="M ` + f(x-5) + ` ` + f(y-5) + ` H ` + f(x+5) + ` V ` + f(y+5) + ` H ` + f(x-5) + ` Z"><title>` + esc(title) + `</title></path>`)
		}
		table = append(table, []string{r.Name, points(*r.Diff) + " pts", ci, reading})
	}
	b.WriteString(`<line class="bl" x1="` + f(forestL) + `" x2="` + f(forestR) + `" y1="` + f(base) + `" y2="` + f(base) + `"></line>`)
	b.WriteString(`<text class="lbl ta-e" x="652" y="` + f(base+34) + `">←  lower in ` + esc(c.B) + `</text>`)
	b.WriteString(`<text class="lbl" x="668" y="` + f(base+34) + `">higher in ` + esc(c.B) + ` →</text>`)

	legend := `<span><svg viewBox="-6 -6 12 12" aria-hidden="true"><path class="ci-pt" d="M -5 -5 H 5 V 5 H -5 Z"></path></svg>one group</span>` +
		`<span><svg viewBox="-9 -9 18 18" aria-hidden="true"><path class="ci-pool" d="M -9 0 L 0 -8 L 9 0 L 0 8 Z"></path></svg>matched, pooled</span>`
	sz := Size{W: 1000, H: int(base) + 38, Class: "vb1000"}
	return template.HTML(figureLegend(c.Label, sz, true, b.String(), legend, false, tableData{head: head, rows: table, more: c.More}))
}
