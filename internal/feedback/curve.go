package feedback

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The README's chart of how a memory's rank moves: the boost Ledger.Boost
// gives after n shows (0 to curveMaxShown, before decay) at five credit
// rates, an uncredited show counting as a confident miss one time in
// curveMissEvery. scripts/feedback-curve writes it for both GitHub themes.
const (
	curveMaxShown  = 50
	curveMissEvery = 3

	curveWidth, curveHeight = 720, 400
	curveLeft, curveRight   = 72, curveWidth - 152
	curveTop, curveBottom   = 44, curveHeight - 64
	curveFontSize           = 16
	curveLineHeight         = 20
	curveFont               = `-apple-system, BlinkMacSystemFont, 'Segoe UI', 'Noto Sans', Helvetica, Arial, sans-serif`
)

// curveLines are the chart's lines, bottom to top: a credit rate, its
// label and its dash pattern; 10 % is neutral (one show in ten credited,
// three misses for each credit, is the prior's share) and the one solid
// line.
var curveLines = []struct {
	rate        float64
	label, dash string
}{
	{0, "never", "2 5"},
	{0.05, "5 %", "8 5"},
	{0.1, "10 % (neutral)", "none"},
	{0.25, "25 %", "14 5 3 5"},
	{1, "always", "22 6"},
}

// curveTheme is a theme's colours: its background, text, axes and zero
// line, and one colour per line of curveLines (a diverging scheme around
// the neutral gray).
type curveTheme struct {
	bg, text, axis string
	lines          [5]string
}

var curveThemes = map[string]curveTheme{
	"light": {bg: "#ffffff", text: "#1f2328", axis: "#59636e",
		lines: [5]string{"#9f1f15", "#e0602b", "#6b6b66", "#3b86de", "#163f7a"}},
	"dark": {bg: "#0d1117", text: "#e6edf3", axis: "#9198a1",
		lines: [5]string{"#ffa198", "#d9533f", "#a3a39c", "#5598e7", "#b6d6fb"}},
}

// curveBoost is Ledger.Boost under p for a memory shown n times and
// credited at rate, with no decay.
func curveBoost(p Params, rate float64, n int) float64 {
	shown := float64(n)
	l := Ledger{Memories: map[string]Entry{"m": {
		Shown:   n,
		Credits: rate * shown,
		Misses:  (1 - rate) * shown / curveMissEvery,
	}}}
	p.MissHalfLife, p.CreditHalfLife = 0, 0
	return l.Boost("m", p, time.Time{})
}

// CurveSVG draws the chart under p for theme "light" or "dark" (any other
// is light), its y axis spanning the boost's bound, every coordinate
// rounded to 2 decimals.
func CurveSVG(p Params, theme string) []byte {
	c, ok := curveThemes[theme]
	if !ok {
		c = curveThemes["light"]
	}
	lo, hi := -0.01, 0.01
	if a, b := p.PriorCredits, p.PriorMisses; a+b > 0 && p.Weight > 0 {
		lo, hi = -p.Weight*a/(a+b), p.Weight*b/(a+b)
	}
	x := func(n int) float64 {
		return curveLeft + float64(n)*(curveRight-curveLeft)/curveMaxShown
	}
	y := func(v float64) float64 {
		return curveTop + (hi-v)/(hi-lo)*(curveBottom-curveTop)
	}

	var s strings.Builder
	fmt.Fprintf(&s, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" font-family="%s" font-size="%d">`+"\n",
		curveWidth, curveHeight, curveWidth, curveHeight, curveFont, curveFontSize)
	s.WriteString("<title>How a memory's rank moves</title>\n")
	fmt.Fprintf(&s, "<desc>%s</desc>\n", curveDesc(p))
	fmt.Fprintf(&s, `<rect width="%d" height="%d" fill="%s"/>`+"\n", curveWidth, curveHeight, c.bg)

	// Axes, ticks and their labels.
	fmt.Fprintf(&s, `<path d="M%s %sV%sH%s" fill="none" stroke="%s" stroke-width="1.5"/>`+"\n",
		num(curveLeft), num(curveTop), num(curveBottom), num(curveRight), c.axis)
	for i := int(math.Ceil(lo/0.02 - 1e-9)); i <= int(math.Floor(hi/0.02+1e-9)); i++ {
		v := float64(i) * 0.02
		ty := y(v)
		fmt.Fprintf(&s, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1.5"/>`+"\n",
			num(curveLeft-5), num(ty), num(curveLeft), num(ty), c.axis)
		fmt.Fprintf(&s, `<text x="%s" y="%s" fill="%s" text-anchor="end">%s</text>`+"\n",
			num(curveLeft-9), num(ty+5.5), c.text, signed(v))
	}
	for n := 0; n <= curveMaxShown; n += 10 {
		fmt.Fprintf(&s, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1.5"/>`+"\n",
			num(x(n)), num(curveBottom), num(x(n)), num(curveBottom+5), c.axis)
		fmt.Fprintf(&s, `<text x="%s" y="%s" fill="%s" text-anchor="middle">%d</text>`+"\n",
			num(x(n)), num(curveBottom+24), c.text, n)
	}
	fmt.Fprintf(&s, `<text x="%s" y="%s" fill="%s" text-anchor="middle">times shown</text>`+"\n",
		num((curveLeft+curveRight)/2), num(curveHeight-14), c.text)
	fmt.Fprintf(&s, `<text x="%s" y="%s" fill="%s" text-anchor="end">boost</text>`+"\n",
		num(curveLeft-9), num(curveTop-20), c.text)
	fmt.Fprintf(&s, `<line class="zero" x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1.5"/>`+"\n",
		num(curveLeft), num(y(0)), num(curveRight), num(y(0)), c.axis)

	// The lines, then their labels at their right ends, nudged apart.
	ends := make([]float64, len(curveLines))
	for i, line := range curveLines {
		pts := make([]string, 0, curveMaxShown+1)
		for n := 0; n <= curveMaxShown; n++ {
			pts = append(pts, num(x(n))+","+num(y(curveBoost(p, line.rate, n))))
		}
		ends[i] = y(curveBoost(p, line.rate, curveMaxShown)) + 5.5
		fmt.Fprintf(&s, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2" stroke-dasharray="%s"/>`+"\n",
			strings.Join(pts, " "), c.lines[i], line.dash)
	}
	for i, ly := range spread(ends, curveLineHeight, curveBottom+5.5) {
		fmt.Fprintf(&s, `<text class="label" x="%s" y="%s" fill="%s">%s</text>`+"\n",
			num(curveRight+8), num(ly), c.text, curveLines[i].label)
	}
	s.WriteString("</svg>\n")
	return []byte(s.String())
}

// spread is ys moved apart, in their order, so that no two are closer than
// gap, none below bottom, each as near its place as that allows.
func spread(ys []float64, gap, bottom float64) []float64 {
	order := make([]int, len(ys))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(ys[a], ys[b]) })
	out := slices.Clone(ys)
	for k := 1; k < len(order); k++ {
		out[order[k]] = max(out[order[k]], out[order[k-1]]+gap)
	}
	last := len(order) - 1
	out[order[last]] = min(out[order[last]], bottom)
	for k := last - 1; k >= 0; k-- {
		out[order[k]] = min(out[order[k]], out[order[k+1]]-gap)
	}
	return out
}

// curveDesc is the chart in words, from its data: the axes, the miss
// share, and each line's way and value at curveMaxShown.
func curveDesc(p Params) string {
	ends := make([]string, len(curveLines))
	for i, line := range curveLines {
		switch b := curveBoost(p, line.rate, curveMaxShown); {
		case math.Abs(b) <= 1e-12:
			ends[i] = line.label + ": it stays at 0"
		case b > 0:
			ends[i] = line.label + ": it rises to " + signed(b)
		default:
			ends[i] = line.label + ": it sinks to " + signed(b)
		}
	}
	return fmt.Sprintf("Line chart of the boost recall adds to a memory's score against the times it was shown, from 0 to %d shows, before decay, "+
		"one uncredited show in %d counting as a miss. One line per share of shows credited, with its value at %d shows: %s.",
		curveMaxShown, curveMissEvery, curveMaxShown, strings.Join(ends, "; "))
}

// signed is v with 2 decimals: a real minus sign, a plus sign above 0,
// and 0 for what rounds to it.
func signed(v float64) string {
	switch r := math.Round(v*100) / 100; {
	case r < 0:
		return fmt.Sprintf("−%.2f", -r)
	case r > 0:
		return fmt.Sprintf("+%.2f", r)
	}
	return "0"
}

// num is v rounded to 2 decimals, without trailing zeros or a minus zero.
func num(v float64) string {
	r := math.Round(v*100)/100 + 0
	if r == 0 {
		return "0"
	}
	return strconv.FormatFloat(r, 'f', -1, 64)
}
