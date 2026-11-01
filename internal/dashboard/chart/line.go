package chart

import (
	"html/template"
	"math"
	"strconv"
	"strings"
)

// Y is one value of a series. Den > 0 makes it a rate (Num of Den), merged
// as the sum of Num over the sum of Den when positions are bucketed.
type Y struct {
	V        *float64
	Num, Den int64
}

// Series is one line.
type Series struct {
	Name, Class string
	Y           []Y
}

// Merge says how bucketed values of a series combine.
type Merge int

const (
	MergeRate Merge = iota
	MergeMean
	MergeMax
	MergeSum
)

// Limit is a horizontal threshold line; Class is "thr" or "thr-w".
type Limit struct {
	Label string
	V     float64
	Class string
}

// Line is a line chart with optional faint n bars and limits.
type Line struct {
	Label  string
	Size   Size
	XName  string
	X      []string
	Series []Series
	N      []int64
	NName  string
	Limits []Limit
	Rate   bool
	Fmt    Num
	Merge  Merge
	Empty  string
	More   string
}

const (
	plotL, plotTop, plotBase = 44.0, 12.0, 200.0
	plotH                    = plotBase - plotTop
)

type val struct {
	v  float64
	ok bool
}

// bucketSize is the smallest k with ceil(positions/k) <= MaxMarks.
func bucketSize(positions int) int {
	return max(1, (positions+MaxMarks-1)/MaxMarks)
}

func (s Series) at(i int) Y {
	if i < len(s.Y) {
		return s.Y[i]
	}
	return Y{}
}

func present(y Y) (float64, bool) {
	if y.V != nil && finite(*y.V) {
		return *y.V, true
	}
	return 0, false
}

func merge(mode Merge, ys []Y) val {
	var sum, top float64
	var count int
	var num, den int64
	allDen := len(ys) > 0
	for _, y := range ys {
		if y.Den > 0 {
			num, den = num+y.Num, den+y.Den
		} else {
			allDen = false
		}
		if v, ok := present(y); ok {
			if count == 0 || v > top {
				top = v
			}
			sum += v
			count++
		}
	}
	var out float64
	switch {
	case mode == MergeRate && allDen:
		out = float64(num) / float64(den)
	case count == 0:
		return val{}
	case mode == MergeMax:
		out = top
	case mode == MergeSum:
		out = sum
	default:
		out = sum / float64(count)
	}
	return val{out, finite(out)}
}

// HTML draws the chart, or the empty sentence when nothing is drawable.
func (c Line) HTML() template.HTML {
	n := len(c.X)
	k := bucketSize(n)
	m := (n + k - 1) / k
	xs := make([]string, m)
	ns := make([]int64, m)
	vals := make([][]val, len(c.Series))
	for s := range vals {
		vals[s] = make([]val, m)
	}
	any := false
	for j := 0; j < m; j++ {
		lo, hi := j*k, min((j+1)*k, n)
		xs[j] = c.X[lo]
		if hi-lo > 1 && c.X[hi-1] != c.X[lo] {
			xs[j] += "–" + c.X[hi-1]
		}
		for i := lo; i < hi && i < len(c.N); i++ {
			ns[j] += c.N[i]
		}
		for s, ser := range c.Series {
			ys := make([]Y, 0, hi-lo)
			for i := lo; i < hi; i++ {
				ys = append(ys, ser.at(i))
			}
			vals[s][j] = merge(c.Merge, ys)
			any = any || vals[s][j].ok
		}
	}
	if !any {
		return template.HTML(emptyHTML(c.Empty))
	}

	W := float64(c.Size.W)
	left, right := plotL, W-8
	top := 1.0 // y axis spans 0..top
	if !c.Rate {
		top = 0
		for _, row := range vals {
			for _, v := range row {
				if v.ok {
					top = math.Max(top, v.v)
				}
			}
		}
		for _, l := range c.Limits {
			if finite(l.V) {
				top = math.Max(top, l.V)
			}
		}
		top = niceMax(top)
	}
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

	var b strings.Builder
	for g := 0; g <= 4; g++ {
		gy := plotBase - float64(g)*plotH/4
		b.WriteString(`<line class="gl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(gy) + `" y2="` + f(gy) + `"></line>`)
		b.WriteString(`<text class="ta-e" x="38" y="` + f(gy+4) + `">` + esc(c.Fmt.format(top/4*float64(g))) + `</text>`)
	}
	if c.N != nil {
		var nmax int64
		for _, v := range ns {
			nmax = max(nmax, v)
		}
		bw := math.Max(step*0.6, 0.5)
		for j, v := range ns {
			h := 0.0
			if nmax > 0 && v > 0 {
				h = float64(v) / float64(nmax) * plotH * 0.3
			}
			b.WriteString(`<rect class="nb" x="` + f(x(j)-bw/2) + `" y="` + f(plotBase-h) + `" width="` + f(bw) + `" height="` + f(h) + `"></rect>`)
		}
	}
	for _, l := range c.Limits {
		if !finite(l.V) {
			continue
		}
		ly := y(l.V)
		b.WriteString(`<line class="` + known(l.Class) + `" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(ly) + `" y2="` + f(ly) + `"></line>`)
		b.WriteString(`<text class="thr-t ta-e" x="` + f(right-2) + `" y="` + f(ly-5) + `">` + esc(l.Label) + `</text>`)
	}
	for s, ser := range c.Series {
		cls := known(ser.Class)
		var run []string
		flush := func() {
			if len(run) > 1 {
				b.WriteString(`<polyline class="ln ` + cls + `" points="` + strings.Join(run, " ") + `"></polyline>`)
			}
			run = run[:0]
		}
		for j, v := range vals[s] {
			if !v.ok {
				flush()
				continue
			}
			run = append(run, f(x(j))+","+f(y(v.v)))
		}
		flush()
		for j, v := range vals[s] {
			if v.ok {
				b.WriteString(`<circle class="pt ` + cls + `" cx="` + f(x(j)) + `" cy="` + f(y(v.v)) + `" r="3"></circle>`)
			}
		}
	}
	b.WriteString(`<line class="bl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(plotBase) + `" y2="` + f(plotBase) + `"></line>`)
	for _, j := range labelIdx(m) {
		b.WriteString(`<text class="ta-m" x="` + f(x(j)) + `" y="218">` + esc(xs[j]) + `</text>`)
	}

	focus := m <= FocusMax
	for j := 0; j < m; j++ {
		title := xs[j]
		for s, ser := range c.Series {
			title += " · " + ser.Name + ": " + c.show(vals[s][j])
		}
		if c.N != nil {
			title += " · " + c.NName + ": " + strconv.FormatInt(ns[j], 10)
		}
		x0 := math.Max(left, x(j)-step/2)
		x1 := math.Min(right, x(j)+step/2)
		b.WriteString(`<rect class="hit" x="` + f(x0) + `" y="` + f(plotTop) + `" width="` + f(x1-x0) + `" height="` + f(plotH) + `"`)
		if focus {
			b.WriteString(` tabindex="0" aria-label="` + esc(title) + `"`)
		}
		b.WriteString(`><title>` + esc(title) + `</title></rect>`)
	}

	var legend []legendItem
	head := []string{c.XName}
	for _, ser := range c.Series {
		legend = append(legend, legendItem{known(ser.Class), ser.Name})
		head = append(head, ser.Name)
	}
	if c.N != nil {
		legend = append(legend, legendItem{"nb-sw", "faint bars: " + c.NName})
		head = append(head, c.NName)
	}
	rows := make([][]string, m)
	for j := range rows {
		row := []string{xs[j]}
		for s := range c.Series {
			row = append(row, c.show(vals[s][j]))
		}
		if c.N != nil {
			row = append(row, strconv.FormatInt(ns[j], 10))
		}
		rows[j] = row
	}
	return template.HTML(figure(c.Label, c.Size, !focus, b.String(), legend, tableData{head, rows, c.More}))
}

func (c Line) show(v val) string {
	if !v.ok {
		return dash
	}
	return c.Fmt.format(v.v)
}

// niceMax is the smallest of 1, 2, 2.5, 5 times a power of ten above m.
func niceMax(m float64) float64 {
	if !finite(m) || m <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(m)))
	for _, mult := range []float64{1, 2, 2.5, 5, 10} {
		if c := mult * p; finite(c) && c > m {
			return c
		}
	}
	return m
}

// labelIdx picks at most six x labels: first, last, evenly between.
func labelIdx(m int) []int {
	if m <= 1 {
		return []int{0}
	}
	cnt := min(m, 6)
	var out []int
	for i := 0; i < cnt; i++ {
		j := int(math.Round(float64(i) * float64(m-1) / float64(cnt-1)))
		if len(out) == 0 || out[len(out)-1] != j {
			out = append(out, j)
		}
	}
	return out
}
