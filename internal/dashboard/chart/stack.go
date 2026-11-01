package chart

import (
	"html/template"
	"math"
	"strings"
)

// Stack is a stacked bar chart or a stacked area chart of counts. Parts are
// listed bottom-up; a nil, negative or non-finite value counts as 0.
type Stack struct {
	Label  string
	Size   Size
	XName  string
	X      []string
	Parts  []Series
	Hatch  string // the part Class drawn hatched; "" none
	Valued string // the part Class whose count is printed above each bar where > 0; "" none
	Fmt    Num
	Empty  string
	More   string
}

const (
	hatchStep = 6.0
	// maxHatchLines bounds the hatch lines of one chart; a hatched bar past
	// the budget keeps its outline only.
	maxHatchLines = 600
	// minHatchWidth is the narrowest bar worth hatching.
	minHatchWidth = 4.0
	// fineBelow is the size under which a rect carries seg-fine (no stroke).
	fineBelow = 4.0
)

// stackData is the bucketed counts of a Stack: totals[j] and counts[p][j].
type stackData struct {
	xs     []string
	counts [][]float64
	totals []float64
	max    float64
}

func (c Stack) data() (stackData, bool) {
	n := len(c.X)
	k := bucketSize(n)
	m := (n + k - 1) / k
	d := stackData{xs: make([]string, m), totals: make([]float64, m), counts: make([][]float64, len(c.Parts))}
	for p := range d.counts {
		d.counts[p] = make([]float64, m)
	}
	for j := 0; j < m; j++ {
		lo, hi := j*k, min((j+1)*k, n)
		d.xs[j] = c.X[lo]
		if hi-lo > 1 && c.X[hi-1] != c.X[lo] {
			d.xs[j] += "–" + c.X[hi-1]
		}
		for p, part := range c.Parts {
			ys := make([]Y, 0, hi-lo)
			for i := lo; i < hi; i++ {
				ys = append(ys, part.at(i))
			}
			v := merge(MergeSum, ys)
			if v.ok && v.v > 0 {
				d.counts[p][j] = v.v
				d.totals[j] += v.v
			}
		}
		if finite(d.totals[j]) {
			d.max = math.Max(d.max, d.totals[j])
		}
	}
	return d, d.max > 0
}

func (c Stack) title(d stackData, j int) string {
	var parts []string
	for p, part := range c.Parts {
		if v := d.counts[p][j]; v > 0 {
			parts = append(parts, part.Name+": "+c.Fmt.format(v))
		}
	}
	if len(parts) == 0 {
		return d.xs[j] + " · none"
	}
	return d.xs[j] + " · " + strings.Join(parts, ", ")
}

// frame writes the grid, the baseline and the x labels around the marks that
// draw writes, then the hit rects, and wraps them in the figure.
func (c Stack) frame(d stackData, top float64, centre func(j int) float64, hit func(j int) (x0, x1 float64), draw func(b *strings.Builder)) template.HTML {
	W := float64(c.Size.W)
	left, right := plotL, W-8
	var b strings.Builder
	for g := 0; g <= 4; g++ {
		gy := plotBase - float64(g)*plotH/4
		b.WriteString(`<line class="gl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(gy) + `" y2="` + f(gy) + `"></line>`)
		b.WriteString(`<text class="ta-e" x="38" y="` + f(gy+4) + `">` + esc(c.Fmt.format(top/4*float64(g))) + `</text>`)
	}
	draw(&b)
	b.WriteString(`<line class="bl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(plotBase) + `" y2="` + f(plotBase) + `"></line>`)
	m := len(d.xs)
	for _, l := range pickLabels(d.xs, centre, W) {
		b.WriteString(`<text class="ta-m" x="` + f(l.x) + `" y="218">` + esc(d.xs[l.j]) + `</text>`)
	}
	focus := m <= FocusMax
	for j := 0; j < m; j++ {
		title := c.title(d, j)
		x0, x1 := hit(j)
		b.WriteString(`<rect class="hit" x="` + f(x0) + `" y="` + f(plotTop) + `" width="` + f(x1-x0) + `" height="` + f(plotH) + `"`)
		if focus {
			b.WriteString(` tabindex="0" aria-label="` + esc(title) + `"`)
		}
		b.WriteString(`><title>` + esc(title) + `</title></rect>`)
	}

	var legend []legendItem
	head := []string{c.XName}
	for _, part := range c.Parts {
		legend = append(legend, legendItem{known(part.Class), part.Name})
		head = append(head, part.Name)
	}
	rows := make([][]string, m)
	for j := range rows {
		row := []string{d.xs[j]}
		for p := range c.Parts {
			row = append(row, c.Fmt.format(d.counts[p][j]))
		}
		rows[j] = row
	}
	return template.HTML(figure(c.Label, c.Size, !focus, b.String(), legend, tableData{head: head, rows: rows, more: c.More}))
}

// r1 rounds to the one decimal geometry is written with.
func r1(x float64) float64 {
	if !finite(x) {
		return 0
	}
	return math.Round(x*10) / 10
}

// Bars draws the stacked bars, or the empty sentence when nothing is drawable.
func (c Stack) Bars() template.HTML {
	d, ok := c.data()
	if !ok {
		return template.HTML(emptyHTML(c.Empty))
	}
	m := len(d.xs)
	W := float64(c.Size.W)
	left, right := plotL, W-8
	top := niceMax(d.max)
	y := func(v float64) float64 { return plotBase - clamp(v/top, 0, 1)*plotH }
	slot := (right - left) / float64(m)
	// a bar is at least 1 unit wide so a bucket never vanishes.
	bw := math.Max(math.Min(28, slot*0.62), 1)
	hatchLeft := maxHatchLines
	draw := func(b *strings.Builder) {
		var vj []int
		var vt []string
		var vy []float64
		for j := 0; j < m; j++ {
			x := left + slot*float64(j) + (slot-bw)/2
			var acc float64
			for p, part := range c.Parts {
				v := d.counts[p][j]
				if v <= 0 {
					continue
				}
				y0, y1 := r1(y(acc)), r1(y(acc+v))
				acc += v
				rx, ry, rw, rh := r1(x), y1, r1(bw), r1(y0-y1)
				if rh < 1 {
					// a non-zero part always shows: at least 1 unit tall, sitting on what is below it.
					rh, ry = 1, r1(y0-1)
				}
				cls := known(part.Class)
				if rw < fineBelow || rh < fineBelow {
					// the 2 px surface stroke would eat a thin rect whole: it is drawn without one.
					cls = "seg-fine " + cls
				}
				b.WriteString(`<rect class="seg ` + cls + `" x="` + f(rx) + `" y="` + f(ry) + `" width="` + f(rw) + `" height="` + f(rh) + `"></rect>`)
				if c.Hatch != "" && part.Class == c.Hatch && rw >= minHatchWidth {
					hatchLeft -= hatch(b, rx, ry, rw, rh, hatchLeft)
				}
			}
			for p, part := range c.Parts {
				if c.Valued != "" && part.Class == c.Valued && d.counts[p][j] > 0 {
					vj = append(vj, j)
					vt = append(vt, c.Fmt.format(d.counts[p][j])+" "+strings.ToLower(part.Name))
					vy = append(vy, y(acc)-5)
					break
				}
			}
		}
		// the count labels are thinned like the x labels so none overlaps or clips.
		for _, l := range pickLabels(vt, func(i int) float64 { return left + slot*(float64(vj[i])+0.5) }, W) {
			b.WriteString(`<text class="val ta-m" x="` + f(l.x) + `" y="` + f(vy[l.j]) + `">` + esc(vt[l.j]) + `</text>`)
		}
	}
	centre := func(j int) float64 { return left + slot*(float64(j)+0.5) }
	hit := func(j int) (float64, float64) { return left + slot*float64(j), left + slot*float64(j+1) }
	return c.frame(d, top, centre, hit, draw)
}

// hatch writes 45 degree lines every hatchStep px inside the rect (x, y, w,
// h), all coordinates already rounded, at most budget lines. It returns the
// number written.
func hatch(b *strings.Builder, x, y, w, h float64, budget int) int {
	if w < 1 || h < 1 {
		return 0
	}
	bottom := y + h
	n := 0
	for k := -h + hatchStep/2; k < w && n < budget; k += hatchStep {
		// u - v = k, u across from the left edge, v up from the bottom.
		u0, u1 := math.Max(0, k), math.Min(w, h+k)
		if u1-u0 < 0.5 {
			continue
		}
		b.WriteString(`<line class="hatch-ln" x1="` + f(x+u0) + `" x2="` + f(x+u1) + `" y1="` + f(bottom-(u0-k)) + `" y2="` + f(bottom-(u1-k)) + `"></line>`)
		n++
	}
	return n
}

// Area draws the stacked areas, or the empty sentence when nothing is drawable.
func (c Stack) Area() template.HTML {
	d, ok := c.data()
	if !ok {
		return template.HTML(emptyHTML(c.Empty))
	}
	m := len(d.xs)
	W := float64(c.Size.W)
	left, right := plotL, W-8
	top := niceMax(d.max)
	y := func(v float64) float64 { return plotBase - clamp(v/top, 0, 1)*plotH }
	step := right - left - 16
	if m > 1 {
		step /= float64(m - 1)
	} else {
		step = right - left
	}
	x := func(j int) float64 {
		if m == 1 {
			return (left + right) / 2
		}
		return left + 8 + float64(j)*step
	}
	// one position is drawn as a short band so the area has a width.
	cols := make([]float64, 0, m)
	for j := 0; j < m; j++ {
		cols = append(cols, x(j))
	}
	idx := make([]int, 0, m)
	for j := 0; j < m; j++ {
		idx = append(idx, j)
	}
	if m == 1 {
		cols = []float64{x(0) - 8, x(0) + 8}
		idx = []int{0, 0}
	}
	cum := make([][]float64, len(c.Parts)+1) // cum[p][j]: sum of parts below p
	cum[0] = make([]float64, m)
	for p := range c.Parts {
		cum[p+1] = make([]float64, m)
		for j := 0; j < m; j++ {
			cum[p+1][j] = cum[p][j] + d.counts[p][j]
		}
	}
	draw := func(b *strings.Builder) {
		for p, part := range c.Parts {
			empty := true
			for _, v := range d.counts[p] {
				empty = empty && v <= 0
			}
			if empty {
				continue
			}
			var path strings.Builder
			for i, j := range idx {
				cmd := "L"
				if i == 0 {
					cmd = "M"
				}
				path.WriteString(cmd + " " + f(cols[i]) + " " + f(y(cum[p+1][j])) + " ")
			}
			for i := len(idx) - 1; i >= 0; i-- {
				path.WriteString("L " + f(cols[i]) + " " + f(y(cum[p][idx[i]])) + " ")
			}
			b.WriteString(`<path class="seg ` + known(part.Class) + `" d="` + path.String() + `Z"></path>`)
		}
	}
	hit := func(j int) (float64, float64) {
		return math.Max(left, x(j)-step/2), math.Min(right, x(j)+step/2)
	}
	return c.frame(d, top, x, hit, draw)
}
