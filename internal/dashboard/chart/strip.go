package chart

import (
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Mark is one event of a strip. Shape is used, ignored, undeclared, shown,
// pending or retired; a retired mark is a line across every row, whatever its
// Row. Text is its title; a mark of another Shape on a Row that does not exist
// is not drawn.
type Mark struct {
	Row   int
	At    time.Time
	Shape string
	Text  string
}

// Strip is a timeline with one row per kind of place and one mark per event.
type Strip struct {
	Label    string
	Size     Size
	Rows     []string
	From, To time.Time
	Ticks    []time.Time
	TickFmt  func(time.Time) string
	Marks    []Mark
	// Created, when not the zero time and inside [From, To], draws a line
	// labelled "created".
	Created time.Time
	Empty   string
	More    string
}

// ShapeWords names each shape for people (legend, table and bucket titles).
var ShapeWords = map[string]string{
	"used": "used by Claude", "ignored": "shown, then ignored", "undeclared": "not declared",
	"shown": "shown only", "pending": "pending", "retired": "retired",
}

const (
	// stripL is where From sits and stripR where To sits; the gridlines start a
	// little left of stripL and the row names end at stripName.
	stripL, stripR = 176.0, 992.0
	stripGrid      = 170.0
	stripName      = 160.0
	// maxName is the most runes of a row name drawn; a longer one ends in "…".
	maxName  = 21
	rowGap   = 38.0
	rowFirst = 26.0
	// maxRetired is the most retirement lines of one strip (the latest).
	maxRetired = 50
)

// shapeOrder breaks ties between shapes: the first listed wins.
var shapeOrder = []string{"used", "ignored", "undeclared", "shown", "pending"}

var shapeClass = map[string]string{
	"used": "f-used", "ignored": "f-ignored", "undeclared": "f-undeclared", "shown": "f-shown", "pending": "mk-pend",
}

// shapePath is the outline of a shape centred on (x, y), written with a space
// after each command so no coordinate reads as a code.
func shapePath(shape string, x, y float64) string {
	circle := func(r float64) string {
		rs := strconv.FormatFloat(r, 'f', -1, 64)
		return "M " + f(x) + " " + f(y-r) + " A " + rs + " " + rs + " 0 1 1 " + f(x) + " " + f(y+r) +
			" A " + rs + " " + rs + " 0 1 1 " + f(x) + " " + f(y-r) + " Z"
	}
	switch shape {
	case "used":
		return circle(6)
	case "ignored":
		return "M " + f(x) + " " + f(y-7) + " L " + f(x+7) + " " + f(y) + " L " + f(x) + " " + f(y+7) + " L " + f(x-7) + " " + f(y) + " Z"
	case "undeclared":
		return "M " + f(x-5) + " " + f(y-5) + " H " + f(x+5) + " V " + f(y+5) + " H " + f(x-5) + " Z"
	case "shown":
		return circle(3.5)
	}
	return circle(5)
}

func day(t time.Time) string { return t.Format("Jan 2") }

// drawn is one thing the plot shows: a mark, a bucket of marks or a retirement.
type drawn struct {
	shape string
	x     float64
	row   int
	text  string
}

func (c Strip) pos(t time.Time) float64 {
	if !c.To.After(c.From) {
		return stripR
	}
	frac := float64(t.Sub(c.From)) / float64(c.To.Sub(c.From))
	if !finite(frac) {
		frac = 1
	}
	return stripL + clamp(frac, 0, 1)*(stripR-stripL)
}

func shows(n int) string {
	if n == 1 {
		return "1 show"
	}
	return strconv.Itoa(n) + " shows"
}

// bucket folds the marks of one row that fall in one time bucket into one.
func (c Strip) bucket(ms []Mark, row int) drawn {
	counts := map[string]int{}
	first, last := ms[0].At, ms[0].At
	var sx float64
	for _, m := range ms {
		counts[m.Shape]++
		if m.At.Before(first) {
			first = m.At
		}
		if m.At.After(last) {
			last = m.At
		}
		sx += c.pos(m.At)
	}
	// most frequent first; a tie goes to the earlier of shapeOrder.
	order := append([]string(nil), shapeOrder...)
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	var parts []string
	for _, s := range order {
		if counts[s] > 0 {
			parts = append(parts, strconv.Itoa(counts[s])+" "+ShapeWords[s])
		}
	}
	days := day(first)
	if day(last) != days {
		days += "–" + day(last)
	}
	return drawn{shape: order[0], x: sx / float64(len(ms)), row: row,
		text: days + " · " + shows(len(ms)) + ": " + strings.Join(parts, ", ")}
}

// HTML draws the strip, or the empty sentence when nothing is drawable.
func (c Strip) HTML() template.HTML {
	if len(c.Rows) == 0 {
		return template.HTML(emptyHTML(c.Empty))
	}
	var retired []Mark
	byRow := make([][]Mark, len(c.Rows))
	var all []Mark // every drawable mark, for the table
	for _, m := range c.Marks {
		switch {
		case m.Shape == "retired":
			retired = append(retired, m)
		case shapeClass[m.Shape] != "" && m.Row >= 0 && m.Row < len(c.Rows):
			byRow[m.Row] = append(byRow[m.Row], m)
		default:
			continue
		}
		all = append(all, m)
	}
	if len(all) == 0 {
		return template.HTML(emptyHTML(c.Empty))
	}
	byTime := func(ms []Mark) {
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].At.Before(ms[j].At) })
	}
	byTime(all)
	byTime(retired)
	if len(retired) > maxRetired {
		retired = retired[len(retired)-maxRetired:]
	}

	// the plot: retirements first, then the shows, bucketed when a row has too many.
	var lines, marks []drawn
	for _, m := range retired {
		lines = append(lines, drawn{shape: "retired", x: c.pos(m.At), text: m.Text})
	}
	// every row drawn has a name (a title when truncated) and at least one
	// mark, so the rows drawn are bounded to keep the titles within MaxMarks;
	// the others stay in the table.
	nrows := min(len(c.Rows), (MaxMarks-len(retired))/4)
	titled := 0
	for _, name := range c.Rows[:nrows] {
		if utf8.RuneCountInString(name) > maxName {
			titled++
		}
	}
	per := (MaxMarks - len(retired) - titled) / nrows
	for row, ms := range byRow[:nrows] {
		byTime(ms)
		if len(ms) <= per {
			for _, m := range ms {
				marks = append(marks, drawn{m.Shape, c.pos(m.At), row, m.Text})
			}
			continue
		}
		groups := make([][]Mark, per)
		for _, m := range ms {
			frac := 1.0
			if c.To.After(c.From) {
				frac = float64(m.At.Sub(c.From)) / float64(c.To.Sub(c.From))
			}
			k := 0
			if finite(frac) {
				k = int(clamp(math.Floor(frac*float64(per)), 0, float64(per-1)))
			}
			groups[k] = append(groups[k], m)
		}
		for _, g := range groups {
			if len(g) > 0 {
				marks = append(marks, c.bucket(g, row))
			}
		}
	}

	W := float64(c.Size.W)
	base := math.Max(196, rowFirst+float64(nrows-1)*rowGap+18) // 196 up to five rows, as designed
	sz := Size{c.Size.W, int(base) + 40, c.Size.Class}
	focus := len(lines)+len(marks) <= FocusMax
	attrs := func(b *strings.Builder, text string) {
		if focus {
			b.WriteString(` tabindex="0" aria-label="` + esc(text) + `"`)
		}
	}
	var b strings.Builder
	for i, name := range c.Rows[:nrows] {
		ry := rowFirst + float64(i)*rowGap
		b.WriteString(`<line class="gl" x1="` + f(stripGrid) + `" x2="` + f(stripR) + `" y1="` + f(ry) + `" y2="` + f(ry) + `"></line>`)
		b.WriteString(`<text class="ta-e lbl" x="` + f(stripName) + `" y="` + f(ry+4) + `">`)
		if r := []rune(name); len(r) > maxName {
			b.WriteString(esc(string(r[:maxName-1])+"…") + `<title>` + esc(name) + `</title>`)
		} else {
			b.WriteString(esc(name))
		}
		b.WriteString(`</text>`)
	}
	if !c.Created.IsZero() && !c.Created.Before(c.From) && !c.Created.After(c.To) {
		x := c.pos(c.Created)
		b.WriteString(`<line class="mk" x1="` + f(x) + `" x2="` + f(x) + `" y1="8" y2="` + f(base) + `"></line>`)
		if x+4+7*7 > float64(c.Size.W) {
			b.WriteString(`<text class="ta-e lbl" x="` + f(x-4) + `" y="` + f(base+12) + `">created</text>`)
		} else {
			b.WriteString(`<text class="lbl" x="` + f(x+4) + `" y="` + f(base+12) + `">created</text>`)
		}
	}
	if n := len(lines); n > 0 {
		// the latest retirement is named, to the left of its line.
		b.WriteString(`<text class="ta-e lbl" x="` + f(lines[n-1].x-4) + `" y="` + f(base+12) + `">` + esc(ShapeWords["retired"]) + `</text>`)
	}
	for _, l := range lines {
		b.WriteString(`<line class="thr-w" x1="` + f(l.x) + `" x2="` + f(l.x) + `" y1="8" y2="` + f(base) + `"`)
		attrs(&b, l.text)
		b.WriteString(`><title>` + esc(l.text) + `</title></line>`)
	}
	for _, m := range marks {
		b.WriteString(`<path class="mkp ` + shapeClass[m.shape] + `" d="` + shapePath(m.shape, m.x, rowFirst+float64(m.row)*rowGap) + `"`)
		attrs(&b, m.text)
		b.WriteString(`><title>` + esc(m.text) + `</title></path>`)
	}
	b.WriteString(`<line class="bl" x1="` + f(stripL) + `" x2="` + f(stripR) + `" y1="` + f(base) + `" y2="` + f(base) + `"></line>`)
	tickFmt := c.TickFmt
	if tickFmt == nil {
		tickFmt = day
	}
	var tl []string
	var tx []float64
	for _, t := range c.Ticks {
		if t.Before(c.From) || t.After(c.To) {
			continue
		}
		tl, tx = append(tl, tickFmt(t)), append(tx, c.pos(t))
	}
	for _, l := range pickLabels(tl, func(i int) float64 { return tx[i] }, W) {
		b.WriteString(`<text class="ta-m" x="` + f(l.x) + `" y="` + f(base+32) + `">` + esc(tl[l.j]) + `</text>`)
	}

	var lg strings.Builder
	for _, s := range shapeOrder {
		lg.WriteString(`<span><svg width="12" height="12" viewBox="-6 -6 12 12" aria-hidden="true"><path class="` + shapeClass[s] +
			`" d="` + legendPath(s) + `"></path></svg>` + esc(ShapeWords[s]) + `</span>`)
	}
	if len(lines) > 0 {
		lg.WriteString(`<span><i class="sw sw-dash"></i>` + esc(ShapeWords["retired"]) + `</span>`)
	}

	if hidden := len(c.Rows) - nrows; hidden > 0 {
		word := " more rows"
		if hidden == 1 {
			word = " more row"
		}
		lg.WriteString(`<span>` + strconv.Itoa(hidden) + word + ` in the table</span>`)
	}
	rows := make([][]string, len(all))
	for i, m := range all {
		where := "all"
		if m.Shape != "retired" {
			where = c.Rows[m.Row]
		}
		rows[i] = []string{m.At.Format("Jan 2, 15:04"), where, ShapeWords[m.Shape], m.Text}
	}
	return template.HTML(figureLegend(c.Label, sz, !focus, b.String(), lg.String(), true,
		tableData{head: []string{"When", "Where", "What", "Detail"}, rows: rows, more: c.More, text: true}))
}

// legendPath is a shape centred on the origin of the legend's 12 unit box.
func legendPath(shape string) string {
	switch shape {
	case "used":
		return "M 0 -5 A 5 5 0 1 1 0 5 A 5 5 0 1 1 0 -5 Z"
	case "ignored":
		return "M 0 -6 L 6 0 L 0 6 L -6 0 Z"
	case "undeclared":
		return "M -4.5 -4.5 H 4.5 V 4.5 H -4.5 Z"
	case "shown":
		return "M 0 -3 A 3 3 0 1 1 0 3 A 3 3 0 1 1 0 -3 Z"
	}
	return "M 0 -4.5 A 4.5 4.5 0 1 1 0 4.5 A 4.5 4.5 0 1 1 0 -4.5 Z"
}
