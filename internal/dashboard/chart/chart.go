// Package chart draws the dashboard's charts as inline SVG, from pure Go
// functions. Every function returns template.HTML, so every text and
// attribute value is escaped here. Nothing computed per item reaches a
// style attribute: it is a geometry attribute or a class from Classes.
package chart

import (
	"html"
	"math"
	"strconv"
	"strings"
)

const (
	// MaxMarks caps the marks of one chart; a mark is an element carrying a title.
	MaxMarks = 500
	// MaxTableRows caps the rows of a table fallback.
	MaxTableRows = 120
	// FocusMax is the most positions whose hit rects are each a focus stop.
	FocusMax = 100
)

const dash = "—"

// Num formats a value for people.
type Num func(float64) string

// Size is the viewBox of a chart and the class giving its svg a min-width.
type Size struct {
	W, H  int
	Class string
}

var (
	Half      = Size{560, 230, "vb560"}
	Full      = Size{1000, 230, "vb1000"}
	StripSize = Size{1000, 236, "vb1000"}
)

// Classes lists every class the package writes; the stylesheet defines each.
var Classes = []string{
	"chart", "chart-plot", "vb560", "vb1000", "legend", "sw", "nb-sw",
	"tblfb", "tbl-wrap", "tbl", "tbl-more", "r", "empty",
	"gl", "bl", "nb", "band", "band-sw", "ln", "ln-2", "pt", "hit", "ta-e", "ta-m", "thr", "thr-w", "thr-t",
	"spark", "sb", "sa", "sl", "se",
	"s-used", "s-coverage", "s-hit", "s-search", "s-empty", "s-latency", "s-search-latency", "s-fg",
	"seg", "seg-fine", "hatch-ln", "val",
	"f-used", "f-ignored", "f-undeclared", "f-shown", "f-pending", "f-summary", "f-about", "f-session",
	"f-ok", "f-queued", "f-aside", "f-failed", "f-neutral", "f-seq1", "f-seq2", "f-seq3",
}

// known keeps the tokens of a caller's class list that Classes defines.
func known(class string) string {
	set := make(map[string]bool, len(Classes))
	for _, c := range Classes {
		set[c] = true
	}
	var keep []string
	for _, c := range strings.Fields(class) {
		if set[c] {
			keep = append(keep, c)
		}
	}
	return strings.Join(keep, " ")
}

func esc(s string) string { return html.EscapeString(s) }

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// f writes a geometry number with one decimal; a non-finite value is 0.
func f(x float64) string {
	if !finite(x) {
		return "0.0"
	}
	s := strconv.FormatFloat(x, 'f', 1, 64)
	if s == "-0.0" {
		return "0.0"
	}
	return s
}

func (n Num) format(v float64) string {
	if n == nil {
		return strconv.FormatFloat(v, 'g', 6, 64)
	}
	return n(v)
}

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }

// legendItem is one legend entry: a swatch class (already known) and a name.
type legendItem struct{ class, name string }

// tableData is a figure's table fallback: the first head cell is the label
// column, the others are right-aligned numbers.
type tableData struct {
	head []string
	rows [][]string
	more string // the command listing every row, shown under a capped table
}

// figure wraps a plot, its legend and its table fallback. Only the plot
// scrolls sideways, so the legend and the table stay in view.
func figure(label string, sz Size, focusable bool, svg string, legend []legendItem, t tableData) string {
	var b strings.Builder
	b.WriteString(`<figure class="chart"><div class="chart-plot"><svg class="` + esc(known(sz.Class)) + `" viewBox="0 0 ` +
		strconv.Itoa(sz.W) + ` ` + strconv.Itoa(sz.H) + `" role="img" aria-label="` + esc(label) + `"`)
	if focusable {
		b.WriteString(` tabindex="0"`)
	}
	b.WriteString(`>` + svg + `</svg></div><div class="legend">`)
	for _, l := range legend {
		b.WriteString(`<span><i class="sw ` + l.class + `"></i>` + esc(l.name) + `</span>`)
	}
	b.WriteString(`</div><details class="tblfb"><summary>Show as a table</summary><div class="tbl-wrap"><table class="tbl"><thead><tr>`)
	for i, h := range t.head {
		if i == 0 {
			b.WriteString(`<th>` + esc(h) + `</th>`)
		} else {
			b.WriteString(`<th class="r">` + esc(h) + `</th>`)
		}
	}
	b.WriteString(`</tr></thead><tbody>`)
	rows := t.rows
	if len(rows) > MaxTableRows {
		rows = rows[len(rows)-MaxTableRows:]
	}
	for _, r := range rows {
		b.WriteString(`<tr>`)
		for i, c := range r {
			if i == 0 {
				b.WriteString(`<td>` + esc(c) + `</td>`)
			} else {
				b.WriteString(`<td class="r">` + esc(c) + `</td>`)
			}
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></div>`)
	if len(t.rows) > MaxTableRows {
		b.WriteString(`<p class="tbl-more">Showing the latest ` + strconv.Itoa(MaxTableRows) + ` of ` +
			strconv.Itoa(len(t.rows)) + ` rows.`)
		if t.more != "" {
			b.WriteString(` Every row: <code>` + esc(t.more) + `</code>`)
		}
		b.WriteString(`</p>`)
	}
	b.WriteString(`</details></figure>`)
	return b.String()
}

func emptyHTML(s string) string { return `<p class="empty">` + esc(s) + `</p>` }
