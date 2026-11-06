package chart

import (
	"html/template"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Y is one value of a series. Den > 0 makes it a rate (Num of Den), merged
// as the sum of Num over the sum of Den when positions are bucketed.
//
// A position whose Y values all have a nil V is a gap, whatever their Den.
// Otherwise, when every Den is positive, the drawn value is the sum of Num
// over the sum of Den (V is expected to equal Num/Den); else the merge of V.
type Y struct {
	V        *float64
	Num, Den int64
}

// Series is one line.
type Series struct {
	Name, Class string
	Y           []Y
	// Dashed draws the line dashed and without dots (the second line of a pair).
	Dashed bool
	// Band draws the 95 % Wilson interval of a rate series (every Den > 0)
	// under the line, from each position's merged sum of Num over sum of Den.
	Band bool
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
	// Markers are vertical lines at positions of X; Bands shade runs of
	// positions with no record; SourceAt (when > 0) is the position where the
	// source of the data changes. All three are in positions of X, not buckets.
	Markers  []Mark2
	Bands    []Band
	SourceAt int
}

const (
	plotL, plotTop, plotBase = 44.0, 12.0, 200.0
	plotH                    = plotBase - plotTop
)

type val struct {
	v        float64
	ok       bool
	num, den int64 // set when v is a rate ΣNum/ΣDen
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
	case count == 0:
		return val{}
	case mode == MergeRate && allDen:
		out = float64(num) / float64(den)
		return val{out, finite(out), num, den}
	case mode == MergeMax:
		out = top
	case mode == MergeSum:
		out = sum
	default:
		out = sum / float64(count)
	}
	return val{v: out, ok: finite(out)}
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
	ov := newOverlay(c.Markers, c.Bands, c.SourceAt, n)
	ov.draw(&b, x, func(j int) (float64, float64) {
		return math.Max(left, x(j)-step/2), math.Min(right, x(j)+step/2)
	})
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
			bx0, bx1 := math.Max(left, x(j)-bw/2), math.Min(right, x(j)+bw/2)
			b.WriteString(`<rect class="nb" x="` + f(bx0) + `" y="` + f(plotBase-h) + `" width="` + f(bx1-bx0) + `" height="` + f(h) + `"></rect>`)
		}
	}
	for s, ser := range c.Series {
		if ser.Band {
			b.WriteString(bandPath(vals[s], x, y))
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
		if ser.Dashed {
			cls = strings.TrimSpace(cls + " ln-2")
		}
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
			if v.ok && !ser.Dashed {
				b.WriteString(`<circle class="pt ` + cls + `" cx="` + f(x(j)) + `" cy="` + f(y(v.v)) + `" r="3"></circle>`)
			}
		}
	}
	b.WriteString(`<line class="bl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(plotBase) + `" y2="` + f(plotBase) + `"></line>`)
	for _, l := range pickLabels(xs, x, W) {
		b.WriteString(`<text class="ta-m" x="` + f(l.x) + `" y="218">` + esc(xs[l.j]) + `</text>`)
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
	hasBand := false
	for _, ser := range c.Series {
		cls := known(ser.Class)
		if ser.Dashed {
			cls = strings.TrimSpace(cls + " ln-2")
		}
		legend = append(legend, legendItem{cls, ser.Name})
		head = append(head, ser.Name)
		hasBand = hasBand || ser.Band
	}
	if hasBand {
		legend = append(legend, legendItem{"band-sw", "interval (95 %)"})
	}
	if c.N != nil {
		legend = append(legend, legendItem{"nb-sw", "faint bars: " + c.NName})
		head = append(head, c.NName)
	}
	mkCol := ov.markerColumn(m)
	if mkCol != nil {
		head = append(head, "Marker")
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
		if mkCol != nil {
			row = append(row, mkCol[j])
		}
		rows[j] = row
	}
	return template.HTML(figureLegend(c.Label, ov.size(c.Size), !focus, b.String(), legendSpans(legend)+ov.legend(), false,
		tableData{head: head, rows: rows, more: c.More}))
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

// bandPath is the 95 % interval of a rate series as one closed path per run
// of positions that have one; a gap breaks it like the line.
func bandPath(row []val, x func(int) float64, y func(float64) float64) string {
	var d strings.Builder
	var run []int
	flush := func() {
		if len(run) > 1 {
			for i, j := range run {
				_, hi := wilson(row[j].num, row[j].den)
				cmd := " L "
				if i == 0 {
					cmd = "M "
				}
				d.WriteString(cmd + f(x(j)) + " " + f(y(hi)))
			}
			for i := len(run) - 1; i >= 0; i-- {
				lo, _ := wilson(row[run[i]].num, row[run[i]].den)
				d.WriteString(" L " + f(x(run[i])) + " " + f(y(lo)))
			}
			d.WriteString(" Z ")
		}
		run = run[:0]
	}
	for j, v := range row {
		if v.ok && v.den > 0 {
			run = append(run, j)
		} else {
			flush()
		}
	}
	flush()
	if d.Len() == 0 {
		return ""
	}
	return `<path class="band" d="` + strings.TrimSpace(d.String()) + `"></path>`
}

// wilson is the 95 % Wilson score interval of num successes in den trials,
// the same as metrics.Wilson (a test pins them equal).
func wilson(num, den int64) (lo, hi float64) {
	const z = 1.96
	if den <= 0 {
		return 0, 0
	}
	num = max(0, min(num, den))
	n := float64(den)
	p := float64(num) / n
	d := 1 + z*z/n
	centre := (p + z*z/(2*n)) / d
	half := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n)) / d
	lo, hi = max(0, centre-half), min(1, centre+half)
	if num == 0 {
		lo = 0
	}
	if num == den {
		hi = 1
	}
	return lo, hi
}

type xLabel struct {
	j int
	x float64
}

// pickLabels chooses at most six x labels (first, last, evenly between),
// centred on their position but kept inside the svg, and thins them until
// no two overlap at an estimated 7 user units per character.
func pickLabels(xs []string, x func(int) float64, width float64) []xLabel {
	return pickLabelsMax(xs, x, width, 6)
}

// pickLabelsMax is pickLabels with the most labels as a parameter.
func pickLabelsMax(xs []string, x func(int) float64, width float64, most int) []xLabel {
	m := len(xs)
	if m <= 1 {
		if m == 1 {
			return []xLabel{{0, clamp(x(0), 0, width)}}
		}
		return nil
	}
	for cnt := min(m, most); cnt >= 2; cnt-- {
		var out []xLabel
		ok := true
		for i := 0; i < cnt; i++ {
			j := int(math.Round(float64(i) * float64(m-1) / float64(cnt-1)))
			if len(out) > 0 && out[len(out)-1].j == j {
				continue
			}
			hw := float64(utf8.RuneCountInString(xs[j])) * 7 / 2
			cx := clamp(x(j), hw+1, math.Max(hw+1, width-hw-1))
			if len(out) > 0 {
				prev := out[len(out)-1]
				phw := float64(utf8.RuneCountInString(xs[prev.j])) * 7 / 2
				if cx-prev.x < hw+phw+8 {
					ok = false
					break
				}
			}
			out = append(out, xLabel{j, cx})
		}
		if ok {
			return out
		}
	}
	return []xLabel{{0, clamp(x(0), 0, width)}}
}

// Mark2 is a vertical marker at position X of a chart's X: Kind is "release"
// (a new version) or "config" (settings changed); Label is drawn beside the
// line and Title is the sentence of its hover and focus text.
type Mark2 struct {
	X                  int
	Kind, Label, Title string
}

// Band shades the positions From to To (both included) of a chart's X: the
// days with no record.
type Band struct{ From, To int }

// legendSpans writes legend entries as figure does.
func legendSpans(items []legendItem) string {
	var l strings.Builder
	for _, it := range items {
		l.WriteString(`<span><i class="sw ` + it.class + `"></i>` + esc(it.name) + `</span>`)
	}
	return l.String()
}

// overlay is what a time chart adds to its data: markers, record gaps and the
// place where the source changes. Positions are those of X; they are mapped
// to the buckets the chart draws.
type overlay struct {
	markers    []Mark2
	bands      []Band
	source     int // 0: none
	sourceDays int // positions from the source change to the end
	k          int // positions per bucket
}

func newOverlay(markers []Mark2, bands []Band, source, n int) overlay {
	o := overlay{k: bucketSize(n)}
	for _, m := range markers {
		if len(o.markers) < MaxMarks && m.X >= 0 && m.X < n && (m.Kind == "release" || m.Kind == "config") {
			o.markers = append(o.markers, m)
		}
	}
	for _, bd := range bands {
		lo, hi := max(bd.From, 0), min(bd.To, n-1)
		if lo <= hi && len(o.bands) < MaxMarks {
			o.bands = append(o.bands, Band{lo, hi})
		}
	}
	if source > 0 && source < n {
		o.source, o.sourceDays = source, n-source
	}
	return o
}

const (
	srcLeft  = "from this PC's daily record"
	gapLabel = "no record: the daily upkeep could not reach the server"
)

// size is sz, made taller by the line of source labels when there is one.
func (o overlay) size(sz Size) Size {
	if o.source > 0 {
		sz.H += 20
	}
	return sz
}

// draw writes the bands, the source change and the markers; centre is the x
// of a bucket and span its left and right edges.
func (o overlay) draw(b *strings.Builder, centre func(j int) float64, span func(j int) (float64, float64)) {
	for _, bd := range o.bands {
		x0, _ := span(bd.From / o.k)
		_, x1 := span(bd.To / o.k)
		b.WriteString(`<rect class="gap-band" x="` + f(x0) + `" y="24.0" width="` + f(x1-x0) + `" height="176.0"></rect>`)
		b.WriteString(`<text class="lbl ta-m" x="` + f((x0+x1)/2) + `" y="18">no record</text>`)
	}
	if o.source > 0 {
		sx := centre(o.source / o.k)
		b.WriteString(`<line class="src-mk" x1="` + f(sx) + `" x2="` + f(sx) + `" y1="24.0" y2="200.0"></line>`)
		b.WriteString(`<text class="lbl ta-e" x="` + f(sx-4) + `" y="240">` + esc(srcLeft) + `</text>`)
		b.WriteString(`<text class="lbl" x="` + f(sx+4) + `" y="240">` + esc("kept by the server ("+strconv.Itoa(o.sourceDays)+" days)") + `</text>`)
	}
	for _, m := range o.markers {
		mx := centre(m.X / o.k)
		cls, tcls, tx := "cfg-mk", "mk-t", mx+4
		if m.Kind == "release" {
			cls, tcls, tx = "rel-mk", "mk-t ta-e", mx-4
		}
		b.WriteString(`<line class="` + cls + `" x1="` + f(mx) + `" x2="` + f(mx) + `" y1="14.0" y2="200.0"><title>` + esc(m.Title) + `</title></line>`)
		if m.Label != "" {
			b.WriteString(`<text class="` + tcls + `" x="` + f(tx) + `" y="24">` + esc(m.Label) + `</text>`)
		}
	}
}

// legend is the legend entries the overlay adds.
func (o overlay) legend() string {
	var rel, cfg bool
	for _, m := range o.markers {
		rel, cfg = rel || m.Kind == "release", cfg || m.Kind == "config"
	}
	var l strings.Builder
	if rel {
		l.WriteString(`<span><i class="sw sw-rel"></i>new version</span>`)
	}
	if cfg {
		l.WriteString(`<span><i class="sw sw-cfg"></i>settings changed</span>`)
	}
	if len(o.bands) > 0 {
		l.WriteString(`<span><svg viewBox="0 0 12 12" aria-hidden="true"><rect class="gap-band" x="0" y="0" width="12" height="12"></rect></svg>` + esc(gapLabel) + `</span>`)
	}
	return l.String()
}

// markerColumn is the table's Marker column over m buckets, or nil when there
// is no marker.
func (o overlay) markerColumn(m int) []string {
	if len(o.markers) == 0 {
		return nil
	}
	col := make([]string, m)
	for _, mk := range o.markers {
		j := mk.X / o.k
		if j >= m {
			continue
		}
		if col[j] != "" {
			col[j] += ", "
		}
		col[j] += mk.Label
	}
	for j := range col {
		if col[j] == "" {
			col[j] = dash
		}
	}
	return col
}
