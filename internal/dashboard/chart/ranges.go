package chart

import (
	"html/template"
	"math"
	"strconv"
	"strings"
)

// Hop is one step of the funnel in milliseconds: the typical time (P50) and
// the time 1 in 20 exceeds (P95), over N sessions. A nil value is not drawn.
type Hop struct {
	Name     string
	P50, P95 *float64
	N        int64
}

// Ranges draws each hop as a range from its typical to its slowest-1-in-20
// time on a logarithmic scale of 1 minute to 14 days, and the watch limit
// (WatchMS, in milliseconds; 0 draws none).
type Ranges struct {
	Label   string
	Hops    []Hop
	WatchMS float64
	Empty   string
	More    string
}

const (
	rngL, rngR = 210.0, 992.0
	rngMaxMin  = 20160.0 // 14 days, in minutes
	msPerMin   = 60000.0
)

// rngX is the x of a time in minutes, on the log scale from 1 minute to 14 days.
func rngX(min float64) float64 {
	return rngL + clamp(math.Log10(math.Max(1, min))/math.Log10(rngMaxMin), 0, 1)*(rngR-rngL)
}

// minutes is a duration in milliseconds as minutes, ok when it is a finite value.
func minutes(ms *float64) (float64, bool) {
	if ms == nil || !finite(*ms) {
		return 0, false
	}
	return math.Max(0, *ms) / msPerMin, true
}

// duration writes minutes as "6 min", "2 h 40 min" or "1 d 1 h".
func duration(min float64) string {
	m := int64(math.Round(min))
	switch {
	case m < 60:
		return strconv.FormatInt(m, 10) + " min"
	case m < 1440:
		s := strconv.FormatInt(m/60, 10) + " h"
		if m%60 != 0 {
			s += " " + strconv.FormatInt(m%60, 10) + " min"
		}
		return s
	}
	s := strconv.FormatInt(m/1440, 10) + " d"
	if h := m % 1440 / 60; h != 0 {
		s += " " + strconv.FormatInt(h, 10) + " h"
	}
	return s
}

// Span writes a duration in milliseconds as the chart does: "6 min",
// "2 h 40 min", "1 d 1 h".
func Span(ms float64) string { return duration(math.Max(0, ms) / msPerMin) }

// HTML draws the ranges, or the empty sentence when no hop has a typical time.
func (c Ranges) HTML() template.HTML {
	hops := c.Hops
	if len(hops) > MaxMarks {
		hops = hops[:MaxMarks]
	}
	any := false
	for _, h := range hops {
		_, ok := minutes(h.P50)
		any = any || ok
	}
	if !any {
		return template.HTML(emptyHTML(c.Empty))
	}
	base := 14.0 + 36*float64(len(hops))
	var b strings.Builder
	for _, t := range []struct {
		min   float64
		label string
	}{{1, "1 min"}, {10, "10 min"}, {60, "1 h"}, {360, "6 h"}, {1440, "1 d"}, {10080, "7 d"}} {
		b.WriteString(`<line class="gl" x1="` + f(rngX(t.min)) + `" x2="` + f(rngX(t.min)) + `" y1="8" y2="` + f(base) + `"></line>`)
		b.WriteString(`<text class="ta-m" x="` + f(rngX(t.min)) + `" y="` + f(base+18) + `">` + t.label + `</text>`)
	}
	head := []string{"Step", "Typical", "Slowest 1 in 20", "Sessions"}
	rows := make([][]string, 0, len(hops))
	for i, h := range hops {
		y := 30 + 36*float64(i)
		b.WriteString(`<text class="ta-e lbl" x="200" y="` + f(y+4) + `">` + esc(h.Name) + `</text>`)
		m50, ok50 := minutes(h.P50)
		m95, ok95 := minutes(h.P95)
		row := []string{h.Name, dash, dash, strconv.FormatInt(h.N, 10)}
		if ok50 {
			title := h.Name + ": typical " + duration(m50)
			row[1] = duration(m50)
			if ok95 {
				title += ", slowest 1 in 20 " + duration(m95)
				row[2] = duration(m95)
				b.WriteString(`<line class="rng" x1="` + f(rngX(m50)) + `" x2="` + f(rngX(m95)) + `" y1="` + f(y) + `" y2="` + f(y) + `"><title>` + esc(title) + `</title></line>`)
			}
			b.WriteString(`<circle class="rng-p50" cx="` + f(rngX(m50)) + `" cy="` + f(y) + `" r="6"><title>` + esc(title) + `</title></circle>`)
		}
		rows = append(rows, row)
	}
	if wm, ok := minutes(&c.WatchMS); ok && wm > 0 {
		wx := rngX(wm)
		b.WriteString(`<line class="thr-w" x1="` + f(wx) + `" x2="` + f(wx) + `" y1="8" y2="` + f(base) + `"></line>`)
		b.WriteString(`<text class="lbl" x="` + f(wx+4) + `" y="` + f(base-4) + `">watch ` + esc(duration(wm)) + `</text>`)
	}
	b.WriteString(`<line class="bl" x1="` + f(rngL) + `" x2="` + f(rngR) + `" y1="` + f(base) + `" y2="` + f(base) + `"></line>`)

	legend := `<span><svg viewBox="-6 -6 12 12" aria-hidden="true"><circle class="rng-p50" r="5"></circle></svg>typical (half took less)</span>` +
		`<span><svg viewBox="-6 -6 12 12" aria-hidden="true"><line class="rng" x1="-4" x2="4" y1="0" y2="0"></line></svg>up to the slowest 1 in 20</span>`
	if c.WatchMS > 0 {
		legend += `<span><svg viewBox="-6 -6 12 12" aria-hidden="true"><line class="thr-w" x1="0" x2="0" y1="-6" y2="6"></line></svg>watch limit</span>`
	}
	legend += `<span class="muted">Time scale is logarithmic: each step is about ten times longer.</span>`
	sz := Size{W: 1000, H: int(base) + 28, Class: "vb1000"}
	return template.HTML(figureLegend(c.Label, sz, true, b.String(), legend, false, tableData{head: head, rows: rows, more: c.More}))
}
