package dashboard

import (
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// calendarMeasure is what the squares are coloured by: the key the page's hm
// parameter names, the measure's words, the lower bound of each of the four
// colours and the ranges the legend writes.
type calendarMeasure struct {
	key, name, what string
	steps           [4]int64
	ranges          [4]string
	of              string // names the count a day's value is a part of; "" none
}

// calendarMeasures are the measures in the order the choice lists them; the
// first is the default (prompts that got memory).
var calendarMeasures = []calendarMeasure{
	{"got", "Prompts that got memory", "prompts got memory", [4]int64{1, 3, 6, 10}, [4]string{"1–2", "3–5", "6–9", "10+"}, "Prompts"},
	{"used", "Turns where Claude used memory", "turns where Claude used memory", [4]int64{1, 2, 3, 5}, [4]string{"1", "2", "3–4", "5+"}, ""},
	{"sessions", "Sessions", "sessions", [4]int64{1, 2, 4, 6}, [4]string{"1", "2–3", "4–5", "6+"}, ""},
}

// measureOf is the measure called key, the default for any other word.
func measureOf(key string) calendarMeasure {
	for _, m := range calendarMeasures {
		if m.key == key {
			return m
		}
	}
	return calendarMeasures[0]
}

// valueOf is a day's count of the measure and the count it is part of.
func (m calendarMeasure) valueOf(d metrics.CalendarDay) (value, of int64) {
	switch m.key {
	case "used":
		return d.UsedTurns, 0
	case "sessions":
		return d.Sessions, 0
	}
	return d.PromptsWithMemory, d.Prompts
}

// CalendarCard is the "Which days did memory help?" card: the choice of what
// to colour by and the calendar, drawn by the chart package.
type CalendarCard struct {
	Head   Head
	Colour []Choice // the measures, as links back to this page
	Chart  template.HTML
	Note   string // under the head: what adding up the PCs leaves this PC's; "" none
}

// calendarAllPCs is the sentence under the head of the calendar on all PCs.
const calendarAllPCs = "The calendar is drawn from the PCs’ daily figures added up. “Colour by sessions” is this PC’s only: other PCs send no sessions, so with all PCs that link shows this PC’s and says so."

// calendarHint is the sentence under the calendar's legend.
const calendarHint = "One Tab stop: arrow keys move from day to day, Enter opens it. On a small screen, the last 13 weeks; older days are in the table."

// calendarHead is the card's frame and its ⓘ; the command it shows is the
// days view's, with the filters the calendar follows (it spans 13 months,
// whatever the window).
func calendarHead(p views.Params) Head {
	h := fixedHead("calendar", "Which days did memory help?", "machine", cliFor("days", "", views.Params{Project: p.Project, TZ: p.TZ, Machines: p.Machines}))
	h.Info = Info{
		Label: "About the days calendar", Name: "Which days did memory help?",
		What: "One square per day for the last 13 months, coloured by the count you pick. A square links to that day.",
		Paras: []string{
			"A dashed square: nothing was recorded that day (no prompt on this PC). A grey square: there was activity, and the count is 0.",
			"Today has a dark outline; the days of the selected window have a coloured ring.",
		},
		Term: "days view · recall.hit_rate num · used.turn_rate num",
	}
	return h
}

// colourLinks are the choices of what to colour by: links to this page with
// hm set, the rest of its query kept, landing on the card.
func colourLinks(path, hm string) []Choice {
	base, pairs := splitPath(without(path, "hm"))
	out := make([]Choice, 0, len(calendarMeasures))
	for _, m := range calendarMeasures {
		q := strings.Join(append(append([]string(nil), pairs...), "hm="+m.key), "&")
		out = append(out, Choice{Label: m.name, Href: base + "?" + q + "#" + widgetID("calendar", ""), Current: m.key == hm})
	}
	return out
}

// calendarOf is the card of the days view r, coloured by the measure hm names
// (the default for any other word). win is the page's window, whose phrase is
// window: its squares carry a ring; the last day of the calendar is today.
func calendarOf(r *metrics.Result, win metrics.Window, p views.Params, hm, path, window string) CalendarCard {
	m := measureOf(hm)
	c := CalendarCard{Head: calendarHead(p), Colour: colourLinks(path, m.key)}
	var rows []metrics.CalendarDay
	if r != nil {
		rows = r.Tables.Calendar
		if c.Head.PCs = allPCsChip(r.Scope); c.Head.PCs != nil {
			c.Note = calendarAllPCs
		}
	}
	loc := zoneOr(win.Loc)
	var since, until time.Time
	if !win.Since.IsZero() {
		since, until = win.Since.In(loc), win.Until.In(loc).Add(-time.Nanosecond)
	}
	first, last := "", ""
	what := m.what
	if c.Head.PCs != nil && m.key == "sessions" {
		what, m.name = "sessions on this PC", "Sessions, this PC" // the others send none
	}
	cal := chart.Calendar{
		What: what, OfWhat: m.of, Steps: m.steps, Window: window, More: "backsight stats days", Hint: calendarHint,
		StepLabels: [5]string{m.name, m.ranges[0], m.ranges[1], m.ranges[2], m.ranges[3]},
	}
	for i, row := range rows {
		day, err := time.ParseInLocation("2006-01-02", row.Day, loc)
		if err != nil {
			continue
		}
		v, of := m.valueOf(row)
		d := chart.CalDay{Day: day, Value: v, Of: of, Recorded: row.Recorded, Today: i == len(rows)-1,
			InWindow: !since.IsZero() && !day.Before(dateOf(since)) && !day.After(dateOf(until)),
			Href:     "/days/" + url.PathEscape(row.Day) + dayScope(p)}
		cal.Days = append(cal.Days, d)
		if first == "" {
			first = day.Format("Jan 2 2006")
		}
		last = day.Format("Jan 2 2006")
	}
	cal.Label = m.name + " per day, " + first + " to " + last + ", one square per day; the table below lists the values"
	c.Chart = cal.HTML()
	return c
}

// dateOf is the start of t's day, in t's zone.
func dateOf(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
}
