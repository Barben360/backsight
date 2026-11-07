package chart

import (
	"html/template"
	"math"
	"strconv"
	"strings"
)

// RangeDay is one day of a RangeBars chart: the prompts of all PCs, the bucket
// (Lo, Hi] in milliseconds that holds the slowest 1 in 20 of the day (Hi nil
// for the open bucket, Lo nil for a day with no sample), and this PC's exact
// value of the day when it had one.
type RangeDay struct {
	X   string
	N   int64
	Lo  *float64
	Hi  *float64
	Own *float64
}

// RangeBars draws the slowest 1 in 20 per day as a bar from the lower to the
// upper edge of its bucket, on a log scale of 50 ms to 10 s, with this PC's
// exact value as a ring and the watch and attention limits as lines
// (WatchMS and LimitMS, 0 draws none). The open bucket is drawn to the top of
// the axis. Say words a bucket for the hover sentence ("between 1.0 and 2.0
// s") and the table ("1.0–2.0 s"); Fmt writes this PC's value.
type RangeBars struct {
	Label        string
	NName, NOne  string // what the prompts are called: "prompts", "prompt"
	Days         []RangeDay
	Say          func(lo float64, hi *float64) (between, cell string)
	Fmt          Num
	WatchMS      float64
	LimitMS      float64
	Empty, More  string
	After        string // markup the caller escaped, closing the figure
	LegendBucket string // the legend's words for a bar
}

const (
	rbMinMS = 50.0
	rbMaxMS = 10000.0
)

// rbSize is the viewBox of a half-width chart, so its 12 units of text stay 12 px.
var rbSize = Half

// rbTicks are the gridlines of the log axis, in milliseconds.
var rbTicks = []float64{100, 500, 1000, 2000, 5000, 10000}

// rbY is the y of a time on the log axis, 50 ms at the baseline, 10 s at the top.
func rbY(ms float64) float64 {
	r := math.Log(clamp(ms, rbMinMS, rbMaxMS)/rbMinMS) / math.Log(rbMaxMS/rbMinMS)
	return plotBase - r*plotH
}

// rbPos is a day, or the days of one bucket of a long window, merged.
type rbPos struct {
	x      string
	n      int64
	lo     float64
	hi     *float64
	has    bool
	own    float64
	hasOwn bool
}

// HTML draws the chart, or the empty sentence when no day has a bucket.
func (c RangeBars) HTML() template.HTML {
	n := len(c.Days)
	k := bucketSize(n)
	m := (n + k - 1) / k
	pos := make([]rbPos, m)
	any := false
	for j := range pos {
		lo, hi := j*k, min((j+1)*k, n)
		p := &pos[j]
		p.x = c.Days[lo].X
		if hi-lo > 1 && c.Days[hi-1].X != c.Days[lo].X {
			p.x += "–" + c.Days[hi-1].X
		}
		for _, d := range c.Days[lo:hi] {
			p.n += d.N
			if d.Lo != nil && finite(*d.Lo) {
				if !p.has || *d.Lo < p.lo {
					p.lo = *d.Lo
				}
				// The merged bar spans the lowest edge to the highest; open stays open.
				switch {
				case !p.has:
					p.hi = d.Hi
				case p.hi != nil && (d.Hi == nil || *d.Hi > *p.hi):
					p.hi = d.Hi
				}
				p.has = true
				any = true
			}
			if d.Own != nil && finite(*d.Own) && (!p.hasOwn || *d.Own > p.own) {
				p.own, p.hasOwn = *d.Own, true
			}
		}
	}
	if !any {
		return template.HTML(emptyHTML(c.Empty))
	}

	W := float64(rbSize.W)
	left, right := plotL, W-8
	step := right - left
	if m > 1 {
		step = (right - left - 16) / float64(m-1)
	}
	x := func(j int) float64 {
		if m == 1 {
			return (left + right) / 2
		}
		return left + 8 + float64(j)*step
	}
	var b strings.Builder
	for _, ms := range rbTicks {
		gy := rbY(ms)
		b.WriteString(`<line class="gl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(gy) + `" y2="` + f(gy) + `"></line>`)
		b.WriteString(`<text class="ta-e" x="38" y="` + f(gy+4) + `">` + esc(c.Fmt.format(ms)) + `</text>`)
	}
	if c.WatchMS > 0 {
		wy := rbY(c.WatchMS)
		b.WriteString(`<line class="thr-w" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(wy) + `" y2="` + f(wy) + `"></line>`)
	}
	if c.LimitMS > 0 {
		ly := rbY(c.LimitMS)
		b.WriteString(`<line class="thr" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(ly) + `" y2="` + f(ly) + `"></line>`)
		b.WriteString(`<text class="thr-t ta-e" x="` + f(right-2) + `" y="` + f(ly-5) + `">` + esc(c.Fmt.format(c.LimitMS)) + `</text>`)
	}
	bw := clamp(step*0.5, 1, 12)
	for j, p := range pos {
		if !p.has {
			continue
		}
		top := plotTop // the open bucket runs to the top of the axis
		if p.hi != nil {
			top = rbY(*p.hi)
		}
		bottom := rbY(p.lo)
		top = math.Min(top, bottom-2) // the lowest bucket is a sliver of the log axis
		b.WriteString(`<rect class="bkt" x="` + f(x(j)-bw/2) + `" y="` + f(top) + `" width="` + f(bw) + `" height="` + f(bottom-top) + `"></rect>`)
	}
	for j, p := range pos {
		if p.hasOwn {
			b.WriteString(`<circle class="bkt-own" cx="` + f(x(j)) + `" cy="` + f(rbY(p.own)) + `" r="3.5"></circle>`)
		}
	}
	b.WriteString(`<line class="bl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(plotBase) + `" y2="` + f(plotBase) + `"></line>`)
	xs := make([]string, m)
	for j, p := range pos {
		xs[j] = p.x
	}
	for _, l := range pickLabels(xs, x, W) {
		b.WriteString(`<text class="ta-m" x="` + f(l.x) + `" y="218">` + esc(xs[l.j]) + `</text>`)
	}

	focus := m <= FocusMax
	rows := make([][]string, m)
	for j, p := range pos {
		between, cell, own := "", dash, dash
		if p.has && c.Say != nil {
			between, cell = c.Say(p.lo, p.hi)
		}
		if p.hasOwn {
			own = c.Fmt.format(p.own)
		}
		rows[j] = []string{p.x, strconv.FormatInt(p.n, 10), cell, own}
		if m > HoverMax {
			continue
		}
		noun := c.NName
		if p.n == 1 && c.NOne != "" {
			noun = c.NOne
		}
		title := p.x + ": " + strconv.FormatInt(p.n, 10) + " " + noun + ", all PCs"
		if p.has {
			title += "; slowest 1 in 20 " + between
		}
		switch {
		case p.hasOwn:
			title += "; this PC alone " + own
		case p.has:
			title += "; this PC had none"
		}
		x0, x1 := math.Max(left, x(j)-step/2), math.Min(right, x(j)+step/2)
		b.WriteString(`<rect class="hit" x="` + f(x0) + `" y="` + f(plotTop) + `" width="` + f(x1-x0) + `" height="` + f(plotH) + `"`)
		if focus {
			b.WriteString(` tabindex="0" aria-label="` + esc(title) + `"`)
		}
		b.WriteString(`><title>` + esc(title) + `</title></rect>`)
	}

	legend := `<span><i class="sw sw-bkt"></i>` + esc(c.LegendBucket) + `</span>` +
		`<span><svg viewBox="-6 -6 12 12" aria-hidden="true"><circle class="bkt-own" r="3.5"></circle></svg>this PC alone (exact)</span>`
	if c.WatchMS > 0 {
		legend += `<span><svg viewBox="-6 -6 12 12" aria-hidden="true"><line class="thr-w" x1="-6" x2="6" y1="0" y2="0"></line></svg>watch ` + esc(c.Fmt.format(c.WatchMS)) + `</span>`
	}
	if c.LimitMS > 0 {
		legend += `<span><svg viewBox="-6 -6 12 12" aria-hidden="true"><line class="thr" x1="-6" x2="6" y1="0" y2="0"></line></svg>limit ` + esc(c.Fmt.format(c.LimitMS)) + `</span>`
	}
	return template.HTML(figureLegend(c.Label, rbSize, !focus, b.String(), legend, true, tableData{
		head: []string{"Day", "Prompts, all PCs", "Slowest 1 in 20, all PCs", "This PC alone"},
		rows: rows, more: c.More, left: []int{2}, after: c.After,
	}))
}
