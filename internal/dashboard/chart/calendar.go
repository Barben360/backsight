package chart

import (
	"html/template"
	"strconv"
	"strings"
	"time"
)

// CalDay is one day of a Calendar. Value is the count the calendar colours by
// and Of the count it is a part of (the prompts, when Value is the prompts that
// got memory). A day that is not Recorded had no activity at all.
type CalDay struct {
	Day                       time.Time
	Value, Of                 int64
	Recorded, InWindow, Today bool
	Href                      string
}

// Calendar is a grid of days, a week to a column, one link to a day per square.
// Days are consecutive, oldest first; the grid starts on the Monday on or before
// the first day and ends on the Sunday on or after the last. Steps are the first
// values of the four colours (q1 to q4); StepLabels name the measure (0) and the
// four ranges (1 to 4) in the legend. A non-empty OfWhat adds ", of <Of>" to a
// day's label and a column of that name to the table.
type Calendar struct {
	Label, What, OfWhat string
	Steps               [4]int64
	StepLabels          [5]string
	Days                []CalDay
	Window              string
	More                string
}

// recentWeeks is how many of the latest weeks stay on a small screen; the
// older ones carry hm-old.
const recentWeeks = 13

func (c Calendar) quartile(v int64) string {
	q := 0
	for i, s := range c.Steps {
		if v >= s {
			q = i + 1
		}
	}
	return "q" + strconv.Itoa(q)
}

func dayName(t time.Time) string { return t.Format("Mon, Jan 2") }

func (c Calendar) label(d CalDay) string {
	s := dayName(d.Day) + ": "
	if !d.Recorded {
		s += "nothing recorded"
	} else {
		s += strconv.FormatInt(d.Value, 10) + " " + c.What
		if c.OfWhat != "" {
			s += ", of " + strconv.FormatInt(d.Of, 10)
		}
	}
	if d.Today {
		s += " (today)"
	}
	return s
}

// HTML draws the calendar, or a sentence when there is no day.
func (c Calendar) HTML() template.HTML {
	days := c.Days
	if len(days) > MaxMarks {
		days = days[len(days)-MaxMarks:]
	}
	if len(days) == 0 {
		return template.HTML(emptyHTML("No days to show."))
	}
	lead := (int(days[0].Day.Weekday()) + 6) % 7
	nW := (lead + len(days) + 6) / 7
	// the roving tab stop is today, else the latest day.
	stop := len(days) - 1
	for i, d := range days {
		if d.Today {
			stop = i
		}
	}

	var weeks strings.Builder
	var months strings.Builder
	type month struct {
		label string
		n     int
	}
	var ms []month
	for w := 0; w < nW; w++ {
		first := w*7 - lead // index in days of this week's Monday
		var monday time.Time
		if first >= 0 {
			monday = days[first].Day
		} else {
			monday = days[0].Day.AddDate(0, 0, first)
		}
		if k := len(ms) - 1; k >= 0 && ms[k].label == monday.Format("Jan 2006") {
			ms[k].n++
		} else {
			ms = append(ms, month{monday.Format("Jan 2006"), 1})
		}
		old := ""
		if w < nW-recentWeeks {
			old = " hm-old"
		}
		weeks.WriteString(`<span class="hm-wk` + old + `" aria-hidden="true">` + esc(monday.Format("Jan 2")) + `</span>`)
		for d := 0; d < 7; d++ {
			i := first + d
			if i < 0 || i >= len(days) {
				weeks.WriteString(`<span class="hm-c pad` + old + `" aria-hidden="true"></span>`)
				continue
			}
			day := days[i]
			cls := "none"
			if day.Recorded {
				cls = c.quartile(day.Value)
			}
			cls += old
			if day.Today {
				cls += " today"
			}
			if day.InWindow {
				cls += " inwin"
			}
			tab := "-1"
			if i == stop {
				tab = "0"
			}
			label := esc(c.label(day))
			weeks.WriteString(`<a class="hm-c ` + cls + `"`)
			if day.Href != "" {
				weeks.WriteString(` href="` + esc(day.Href) + `"`)
			}
			weeks.WriteString(` tabindex="` + tab + `" aria-label="` + label + `" title="` + label + `"></a>`)
		}
	}
	for k, m := range ms {
		label := m.label[:3]
		if k == 0 && m.n < 3 {
			label = ""
		}
		months.WriteString(`<span class="hm-w` + strconv.Itoa(min(m.n, 6)) + `">` + esc(label) + `</span>`)
	}

	var b strings.Builder
	b.WriteString(`<figure class="chart"><div class="hm-box"><div class="hm-wrap" role="group" aria-label="` + esc(c.Label) + `">`)
	b.WriteString(`<span aria-hidden="true"></span><div class="hm-months" aria-hidden="true">` + months.String() + `</div>`)
	b.WriteString(`<div class="hm-dl" aria-hidden="true"><span>Mon</span><span></span><span>Wed</span><span></span><span>Fri</span><span></span><span>Sun</span></div>`)
	b.WriteString(`<div class="hm-dh" aria-hidden="true"><span></span><span>M</span><span>T</span><span>W</span><span>T</span><span>F</span><span>S</span><span>S</span></div>`)
	b.WriteString(`<div class="hm">` + weeks.String() + `</div></div></div>`)

	sw := func(cls, text string) string {
		return `<span class="hm-c ` + cls + `" aria-hidden="true"></span><span>` + esc(text) + `</span>`
	}
	b.WriteString(`<div class="hm-legend"><span class="ramp"><span>` + esc(c.StepLabels[0]) + `:</span>` +
		sw("none", "none recorded") + sw("q0", "0") + sw("q1", c.StepLabels[1]) + sw("q2", c.StepLabels[2]) +
		sw("q3", c.StepLabels[3]) + sw("q4", c.StepLabels[4]) + `</span>` +
		`<span class="ramp">` + sw("q2 today", "today") + `</span>` +
		`<span class="ramp">` + sw("q2 inwin", "in the window ("+c.Window+")") + `</span></div>`)

	// the table: the latest MaxTableRows days, the latest first.
	rows := days
	if len(rows) > MaxTableRows {
		rows = rows[len(rows)-MaxTableRows:]
	}
	b.WriteString(`<details class="tblfb"><summary>Show as a table</summary><div class="tbl-wrap"><table class="tbl"><caption class="sr-only">` +
		esc(c.Label) + `</caption><thead><tr><th scope="col">Day</th><th class="r" scope="col">` + esc(c.StepLabels[0]) + `</th>`)
	if c.OfWhat != "" {
		b.WriteString(`<th class="r" scope="col">` + esc(c.OfWhat) + `</th>`)
	}
	b.WriteString(`</tr></thead><tbody>`)
	num := func(d CalDay, v int64) string {
		if !d.Recorded {
			return dash
		}
		return strconv.FormatInt(v, 10)
	}
	for i := len(rows) - 1; i >= 0; i-- {
		d := rows[i]
		name := esc(dayName(d.Day))
		if d.Href != "" {
			name = `<a href="` + esc(d.Href) + `">` + name + `</a>`
		}
		b.WriteString(`<tr><td>` + name + `</td><td class="r">` + num(d, d.Value) + `</td>`)
		if c.OfWhat != "" {
			b.WriteString(`<td class="r">` + num(d, d.Of) + `</td>`)
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></div>`)
	if len(days) > MaxTableRows {
		b.WriteString(`<p class="tbl-more">The latest ` + strconv.Itoa(MaxTableRows) + ` days are here.`)
		if c.More != "" {
			b.WriteString(` <code>` + esc(c.More) + `</code> lists every day.`)
		}
		b.WriteString(`</p>`)
	}
	b.WriteString(`</details></figure>`)
	return template.HTML(b.String())
}
