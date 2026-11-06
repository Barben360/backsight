package chart

import (
	"html/template"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// StatusDay is the result of one check on one day: ok, watch, bad or none (no
// run that day). Any other value reads as none.
type StatusDay struct {
	Day    time.Time
	Status string
}

// StatusRow is the days of one check, oldest first.
type StatusRow struct {
	Name string
	Days []StatusDay
}

// StatusStrip is one row per check, one mark per day, the mark's shape and
// colour giving the status. TickFmt writes the x labels (default "Oct 21").
type StatusStrip struct {
	Label   string
	Size    Size
	Rows    []StatusRow
	TickFmt func(time.Time) string
	More    string
}

const (
	stL        = 222.0
	stRight    = 980.0
	stGrid     = 210.0
	stName     = 200.0
	stFirst    = 20.0
	stGap      = 27.0
	stMaxRows  = 100
	stRunWord  = "no upkeep run"
	stNoRunKey = "none"
)

// statusWord names each status for people.
var statusWord = map[string]string{"ok": "ok", "watch": "watch", "bad": "attention", stNoRunKey: stRunWord}

// statusRank orders statuses from the best to the worst; a day without a run
// is the least.
var statusRank = map[string]int{stNoRunKey: 0, "ok": 1, "watch": 2, "bad": 3}

func normStatus(s string) string {
	if _, ok := statusRank[s]; ok {
		return s
	}
	return stNoRunKey
}

// statusPath is the outline of a status centred on (x, y), with a space after
// each command.
func statusPath(status string, x, y float64) string {
	switch status {
	case "ok":
		return "M " + f(x) + " " + f(y-4.5) + " A 4.5 4.5 0 1 1 " + f(x) + " " + f(y+4.5) + " A 4.5 4.5 0 1 1 " + f(x) + " " + f(y-4.5) + " Z"
	case "watch":
		return "M " + f(x) + " " + f(y-6) + " L " + f(x+6) + " " + f(y+5) + " L " + f(x-6) + " " + f(y+5) + " Z"
	case "bad":
		return "M " + f(x-2.5) + " " + f(y-6) + " H " + f(x+2.5) + " L " + f(x+6) + " " + f(y-2.5) + " V " + f(y+2.5) +
			" L " + f(x+2.5) + " " + f(y+6) + " H " + f(x-2.5) + " L " + f(x-6) + " " + f(y+2.5) + " V " + f(y-2.5) + " Z"
	}
	return "M " + f(x) + " " + f(y-4) + " A 4 4 0 1 1 " + f(x) + " " + f(y+4) + " A 4 4 0 1 1 " + f(x) + " " + f(y-4) + " Z"
}

var statusClass = map[string]string{"ok": "mkp f-st-ok", "watch": "mkp f-st-watch", "bad": "mkp f-st-bad", stNoRunKey: "norun"}

// civil is the number of days from the epoch to t's calendar date.
func civil(t time.Time) int {
	return int(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

type statusMark struct {
	status string
	x      float64
	row    int
	text   string
}

// HTML draws the strip, or an empty sentence when there is no check or no day.
func (c StatusStrip) HTML() template.HTML {
	lo, hi, any := 0, 0, false
	for _, r := range c.Rows {
		for _, d := range r.Days {
			n := civil(d.Day)
			if !any || n < lo {
				lo = n
			}
			if !any || n > hi {
				hi = n
			}
			any = true
		}
	}
	if !any {
		return template.HTML(emptyHTML("No health check has run yet."))
	}
	pos := func(n int) float64 {
		if hi == lo {
			return stRight
		}
		return stL + float64(n-lo)/float64(hi-lo)*(stRight-stL)
	}

	nrows := min(len(c.Rows), stMaxRows)
	titled := 0
	for _, r := range c.Rows[:nrows] {
		if utf8.RuneCountInString(r.Name) > maxName {
			titled++
		}
	}
	per := max(1, (MaxMarks-titled)/nrows)

	var marks []statusMark
	for i, r := range c.Rows[:nrows] {
		days := r.Days
		k := (len(days) + per - 1) / per // days to a bucket
		if k < 1 {
			k = 1
		}
		for s := 0; s < len(days); s += k {
			g := days[s:min(s+k, len(days))]
			if len(g) == 1 {
				st := normStatus(g[0].Status)
				marks = append(marks, statusMark{st, pos(civil(g[0].Day)), i,
					day(g[0].Day) + " · " + r.Name + " · " + statusWord[st]})
				continue
			}
			worst, runs := stNoRunKey, 0
			for _, d := range g {
				st := normStatus(d.Status)
				if st != stNoRunKey {
					runs++
				}
				if statusRank[st] > statusRank[worst] {
					worst = st
				}
			}
			x := (pos(civil(g[0].Day)) + pos(civil(g[len(g)-1].Day))) / 2
			marks = append(marks, statusMark{worst, x, i, day(g[0].Day) + "–" + day(g[len(g)-1].Day) + " · " + r.Name +
				" · " + statusWord[worst] + ", " + runWord(runs)})
		}
	}

	base := stFirst + float64(nrows-1)*stGap + 26
	sz := Size{c.Size.W, int(base) + 38, c.Size.Class}
	focus := len(marks) <= FocusMax
	var b strings.Builder
	for i, r := range c.Rows[:nrows] {
		ry := stFirst + float64(i)*stGap
		b.WriteString(`<line class="gl" x1="` + f(stGrid) + `" x2="` + f(stRight) + `" y1="` + f(ry) + `" y2="` + f(ry) + `"></line>`)
		b.WriteString(`<text class="ta-e lbl" x="` + f(stName) + `" y="` + f(ry+4) + `">`)
		if rs := []rune(r.Name); len(rs) > maxName {
			b.WriteString(esc(string(rs[:maxName-1])+"…") + `<title>` + esc(r.Name) + `</title>`)
		} else {
			b.WriteString(esc(r.Name))
		}
		b.WriteString(`</text>`)
	}
	for _, m := range marks {
		b.WriteString(`<path class="` + statusClass[m.status] + `" d="` + statusPath(m.status, m.x, stFirst+float64(m.row)*stGap) + `"`)
		if focus {
			b.WriteString(` tabindex="0" aria-label="` + esc(m.text) + `"`)
		}
		b.WriteString(`><title>` + esc(m.text) + `</title></path>`)
	}
	b.WriteString(`<line class="bl" x1="` + f(stL) + `" x2="` + f(stRight) + `" y1="` + f(base) + `" y2="` + f(base) + `"></line>`)
	tickFmt := c.TickFmt
	if tickFmt == nil {
		tickFmt = day
	}
	var first time.Time
	for _, r := range c.Rows {
		for _, d := range r.Days {
			if civil(d.Day) == lo {
				first = d.Day
			}
		}
	}
	var tl []string
	var tx []float64
	for i := 0; i < 5; i++ {
		n := lo + (hi-lo)*i/4
		if i > 0 && n == lo+(hi-lo)*(i-1)/4 {
			continue
		}
		tl = append(tl, tickFmt(first.AddDate(0, 0, n-lo)))
		tx = append(tx, pos(n))
	}
	for _, l := range pickLabels(tl, func(i int) float64 { return tx[i] }, float64(c.Size.W)) {
		b.WriteString(`<text class="ta-m" x="` + f(l.x) + `" y="` + f(base+20) + `">` + esc(tl[l.j]) + `</text>`)
	}

	var lg strings.Builder
	for _, s := range []string{"ok", "watch", "bad", stNoRunKey} {
		name := statusWord[s]
		if s == stNoRunKey {
			name += " that day"
		}
		lg.WriteString(`<span><svg viewBox="-6 -6 12 12" aria-hidden="true"><path class="` +
			map[string]string{"ok": "f-st-ok", "watch": "f-st-watch", "bad": "f-st-bad", stNoRunKey: "norun"}[s] +
			`" d="` + statusPath(s, 0, 0) + `"></path></svg>` + esc(name) + `</span>`)
	}

	// the table: one row per check, its tally and its last change.
	var rows [][]string
	for _, r := range c.Rows {
		n := map[string]int{}
		now, change := "", dash
		prev := ""
		for _, d := range r.Days {
			st := normStatus(d.Status)
			n[st]++
			if st == stNoRunKey {
				continue
			}
			if prev != "" && prev != st {
				change = day(d.Day) + ": " + statusWord[prev] + " → " + statusWord[st]
			}
			prev, now = st, st
		}
		if now == "" {
			now = dash
		} else {
			now = statusWord[now]
		}
		rows = append(rows, []string{r.Name, now, strconv.Itoa(n["ok"]), strconv.Itoa(n["watch"]),
			strconv.Itoa(n["bad"]), strconv.Itoa(n[stNoRunKey]), change})
	}
	return template.HTML(figureLegend(c.Label, sz, !focus, b.String(), lg.String(), false,
		tableData{head: []string{"Check", "Now", "ok", "watch", "attention", "No run", "Last change"}, rows: rows, more: c.More, text: true}))
}

func runWord(n int) string {
	if n == 1 {
		return "1 run"
	}
	return strconv.Itoa(n) + " runs"
}
