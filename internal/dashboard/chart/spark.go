package chart

import (
	"html/template"
	"math"
	"strings"
)

// Spark is the small trend line of a tile: no axis, legend, table or title.
type Spark struct {
	Label string
	Y     []*float64 // nil or non-finite: missing
	Fmt   Num
}

const sparkSummaryOver = 31

// HTML draws the sparkline.
func (c Spark) HTML() template.HTML {
	n := len(c.Y)
	type pt struct {
		x, y float64
		ok   bool
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	present := 0
	for _, p := range c.Y {
		if p != nil && finite(*p) {
			lo, hi = math.Min(lo, *p), math.Max(hi, *p)
			present++
		}
	}
	span := hi - lo
	flat := present == 0 || !finite(span) || span == 0
	pts := make([]pt, n)
	for i, p := range c.Y {
		if p == nil || !finite(*p) {
			continue
		}
		x := 4 + float64(i)*180/float64(max(n-1, 1))
		y := 16.0
		if !flat {
			y = 28 - (*p-lo)/span*24
		}
		pts[i] = pt{x, y, true}
	}

	var b strings.Builder
	b.WriteString(`<svg class="spark" viewBox="0 0 188 32" preserveAspectRatio="none" role="img" aria-label="`)
	b.WriteString(esc(c.ariaLabel(lo, hi, present)))
	b.WriteString(`">`)
	b.WriteString(`<line class="sb" x1="0" y1="31" x2="188" y2="31"></line>`)

	var runs [][]pt
	var run []pt
	flush := func() {
		if len(run) > 1 {
			runs = append(runs, run)
		}
		run = nil
	}
	for _, p := range pts {
		if !p.ok {
			flush()
			continue
		}
		run = append(run, p)
	}
	flush()
	if len(runs) > 0 {
		var d strings.Builder
		for _, r := range runs {
			if d.Len() > 0 {
				d.WriteByte(' ')
			}
			d.WriteString("M " + f(r[0].x) + " 31.0")
			for _, p := range r {
				d.WriteString(" L " + f(p.x) + " " + f(p.y))
			}
			d.WriteString(" L " + f(r[len(r)-1].x) + " 31.0 Z")
		}
		b.WriteString(`<path class="sa" d="` + d.String() + `"></path>`)
		for _, r := range runs {
			var ps []string
			for _, p := range r {
				ps = append(ps, f(p.x)+","+f(p.y))
			}
			b.WriteString(`<polyline class="sl" points="` + strings.Join(ps, " ") + `"></polyline>`)
		}
	}
	for i := n - 1; i >= 0; i-- {
		if pts[i].ok {
			b.WriteString(`<circle class="se" cx="` + f(pts[i].x) + `" cy="` + f(pts[i].y) + `" r="3"></circle>`)
			break
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func (c Spark) ariaLabel(lo, hi float64, present int) string {
	val := func(p *float64) string {
		if p == nil || !finite(*p) {
			return dash
		}
		return c.Fmt.format(*p)
	}
	if len(c.Y) > sparkSummaryOver {
		if present == 0 {
			return c.Label + ": " + dash
		}
		var first, last *float64
		for _, p := range c.Y {
			if p != nil && finite(*p) {
				if first == nil {
					first = p
				}
				last = p
			}
		}
		return c.Label + ": from " + val(first) + " to " + val(last) +
			", lowest " + c.Fmt.format(lo) + ", highest " + c.Fmt.format(hi)
	}
	vals := make([]string, len(c.Y))
	for i, p := range c.Y {
		vals[i] = val(p)
	}
	if len(vals) == 0 {
		return c.Label + ": " + dash
	}
	return c.Label + ": " + strings.Join(vals, ", ")
}
