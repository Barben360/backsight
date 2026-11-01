package chart

import (
	"html/template"
	"math"
	"strings"
)

// Bin is one bar of a histogram.
type Bin struct {
	Label string
	N     int64
}

// Marker is a vertical reference line; At is a position in bins (2.5 is the
// middle of the third bin) and Class is "thr-w", "thr" or "mk".
type Marker struct {
	Label string
	At    float64
	Class string
}

// Hist is a histogram of counts, one bar per bin. A negative count is 0.
type Hist struct {
	Label   string
	Size    Size
	XName   string
	Bins    []Bin
	Markers []Marker
	Class   string // the bar class, "f-used" when not a known class
	Unit    string // what a count counts, in each title ("times shown")
	Fmt     Num
	Empty   string
	More    string
}

const (
	histTop = 14.0
	// histHead keeps room above the tallest bar for the marker labels.
	histHead = 10.0
	// maxHistLabels is the most x labels of a histogram.
	maxHistLabels = 12
)

// bins merges adjacent bins until at most MaxMarks remain: counts add up and
// the label reads first–last.
func (c Hist) bins() []Bin {
	n := len(c.Bins)
	k := bucketSize(n)
	out := make([]Bin, 0, (n+k-1)/k)
	for lo := 0; lo < n; lo += k {
		hi := min(lo+k, n)
		b := Bin{Label: c.Bins[lo].Label}
		if hi-lo > 1 && c.Bins[hi-1].Label != b.Label {
			b.Label += "–" + c.Bins[hi-1].Label
		}
		for _, x := range c.Bins[lo:hi] {
			if x.N > 0 {
				b.N += x.N
			}
		}
		out = append(out, b)
	}
	return out
}

func (c Hist) title(b Bin) string {
	s := b.Label + ": " + c.Fmt.format(float64(b.N))
	if c.Unit != "" {
		s += " " + c.Unit
	}
	return s
}

// HTML draws the histogram, or the empty sentence when no bin has a count.
func (c Hist) HTML() template.HTML {
	bins := c.bins()
	var max int64
	for _, b := range bins {
		if b.N > max {
			max = b.N
		}
	}
	if max == 0 {
		return template.HTML(emptyHTML(c.Empty))
	}
	W := float64(c.Size.W)
	left, right := plotL, W-8
	m := len(bins)
	slot := (right - left) / float64(m)
	bw := math.Max(slot-4, 1)
	ymax := math.Ceil(float64(max)/4) * 4
	usable := plotBase - histTop - histHead
	y := func(v float64) float64 { return plotBase - clamp(v/ymax, 0, 1)*usable }
	cls := known(c.Class)
	if cls == "" {
		cls = "f-used"
	}

	var b strings.Builder
	for g := 0; g <= 4; g++ {
		gy := y(ymax * float64(g) / 4)
		b.WriteString(`<line class="gl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(gy) + `" y2="` + f(gy) + `"></line>`)
		b.WriteString(`<text class="ta-e" x="38" y="` + f(gy+4) + `">` + esc(c.Fmt.format(ymax*float64(g)/4)) + `</text>`)
	}
	focus := m <= FocusMax
	for j, bin := range bins {
		x := left + slot*float64(j) + (slot-bw)/2
		top := y(float64(bin.N))
		h := plotBase - top
		if bin.N > 0 && h < 1 {
			h = 1
		}
		t := c.title(bin)
		b.WriteString(`<rect class="` + cls + `" x="` + f(x) + `" y="` + f(plotBase-h) + `" width="` + f(bw) + `" height="` + f(h) + `" rx="2"`)
		if focus {
			b.WriteString(` tabindex="0" aria-label="` + esc(t) + `"`)
		}
		b.WriteString(`><title>` + esc(t) + `</title></rect>`)
	}
	for _, mk := range c.Markers {
		if !finite(mk.At) {
			continue
		}
		mc := known(mk.Class)
		if mc == "" {
			mc = "thr-w"
		}
		x := clamp(left+mk.At*slot, left, right)
		b.WriteString(`<line class="` + mc + `" x1="` + f(x) + `" x2="` + f(x) + `" y1="` + f(histTop) + `" y2="` + f(plotBase) + `"></line>`)
		hw := float64(len([]rune(mk.Label))) * 7 / 2
		tx := clamp(x, hw+1, math.Max(hw+1, W-hw-1))
		b.WriteString(`<text class="lbl ta-m" x="` + f(tx) + `" y="12">` + esc(mk.Label) + `</text>`)
	}
	b.WriteString(`<line class="bl" x1="` + f(left) + `" x2="` + f(right) + `" y1="` + f(plotBase) + `" y2="` + f(plotBase) + `"></line>`)
	labels := make([]string, m)
	for j := range bins {
		labels[j] = bins[j].Label
	}
	centre := func(j int) float64 { return left + slot*(float64(j)+0.5) }
	for _, l := range pickLabelsMax(labels, centre, W, maxHistLabels) {
		b.WriteString(`<text class="ta-m" x="` + f(l.x) + `" y="218">` + esc(labels[l.j]) + `</text>`)
	}

	unit := c.Unit
	if unit == "" {
		unit = "Count"
	}
	rows := make([][]string, m)
	for j, bin := range bins {
		rows[j] = []string{bin.Label, c.Fmt.format(float64(bin.N))}
	}
	return template.HTML(figure(c.Label, c.Size, !focus, b.String(), nil, tableData{head: []string{c.XName, unit}, rows: rows, more: c.More}))
}
