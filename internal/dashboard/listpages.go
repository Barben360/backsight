package dashboard

import (
	"html/template"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// Ref is a named thing the page links to. Text is what is shown (cut),
// Full the whole name for the hover (control characters removed), Href its
// page; Href is "" for a name that cannot be a path segment. None marks a
// project the row does not have.
type Ref struct {
	Text, Full, Href string
	None             bool
}

// pathHref is prefix + the id as one path segment + query. "" for an id
// that is empty or would read as a relative path ("." and "..").
func pathHref(prefix, id, query string) string {
	if id == "" || id == "." || id == ".." {
		return ""
	}
	return prefix + url.PathEscape(id) + query
}

// windowQuery is the query that keeps the page's window on a link to a
// page that reads it.
func windowQuery(p views.Params, keys ...string) string {
	q := url.Values{}
	for _, k := range keys {
		switch {
		case k == "days" && p.DaysSet:
			q.Set(k, strconv.Itoa(p.Days))
		case k == "since" && p.Since != "":
			q.Set(k, p.Since)
		case k == "until" && p.Until != "":
			q.Set(k, p.Until)
		case k == "tz" && p.TZ != "":
			q.Set(k, p.TZ)
		}
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// idLink is an id as the chip shows it: 8 characters, the whole id on hover.
func idLink(prefix, id string) Ref {
	return Ref{Text: views.ShortID(id), Full: views.Plain(id, ""), Href: pathHref(prefix, id, "")}
}

// projectLink is a project name as the tables show it. The link keeps the
// page's window: the project page reads it.
func (d PageData) projectLink(name string) Ref {
	if name == "" {
		return Ref{None: true}
	}
	return Ref{Text: views.Clip(name), Full: views.Plain(name, ""),
		Href: pathHref("/projects/", name, windowQuery(d.P, "days", "since", "until", "tz"))}
}

// resultOf is the page's first view result, an empty one when it has none.
func (d PageData) resultOf(view string) *metrics.Result {
	if r := d.V[view]; r != nil {
		return r
	}
	return &metrics.Result{}
}

// rel is t read against now; "—" for no time.
func (d PageData) rel(now time.Time, t *time.Time) format.Time {
	if t == nil || t.IsZero() {
		return format.Time{Text: dash}
	}
	return format.Rel(now, *t, d.Loc)
}

// Cell is a number of a table: its text, the count it rests on (N), the
// class that colours it and, in the pooled row, the lowest and highest
// project (Range).
type Cell struct{ Text, N, Class, Range string }

// SessionsTable is a table of sessions; WhenHead names the time column.
type SessionsTable struct {
	WhenHead string
	Rows     []SessionLine
}

// SessionLine is a row of a sessions table.
type SessionLine struct {
	When                                      format.Time
	ID, Project                               Ref
	Prompts, Got, Shown, Used, Wrong, Context string
	WrongClass                                string
	Sent                                      Word
}

// sentClass colours how a session went to the server.
var sentClass = map[string]string{"ok": "st-ok", "queued": "st-watch", "retry": "st-watch", "failed": "st-bad", "quarantined": "st-bad"}

// sessionTable is rows as a table. started: the time column is the clock
// time of the start (a page about one day), else how long ago.
func (d PageData) sessionTable(now time.Time, rows []metrics.SessionRow, started bool) SessionsTable {
	l := d.L
	t := SessionsTable{WhenHead: "When"}
	if started {
		t.WhenHead = "Started"
	}
	for _, s := range rows {
		line := SessionLine{
			When: format.Time{Text: dash}, ID: idLink("/sessions/", s.ID), Project: d.projectLink(s.Project),
			Prompts: l.Int(s.Prompts), Got: l.Int(s.PromptsWithMemory), Shown: l.Int(s.Shown), Used: l.Int(s.Used),
			Wrong: l.Int(s.Wrong), Context: l.Size(float64(s.ContextChars), "chars"),
			Sent: Word{views.Word(views.SentWords, s.Sent), sentClass[s.Sent]},
		}
		if !s.Start.IsZero() {
			line.When = format.Rel(now, s.Start, d.Loc)
			if started {
				line.When.Text = s.Start.In(zoneOr(d.Loc)).Format("15:04")
			}
		}
		if s.Wrong > 0 {
			line.WrongClass = "st-watch"
		}
		t.Rows = append(t.Rows, line)
	}
	return t
}

// sessionCount is "34 sessions" from the view's page total; "" without one.
func (d PageData) sessionCount(p *metrics.Page) string {
	if p == nil {
		return ""
	}
	return format.Count(d.L, int64(p.Total), "session", "sessions")
}

// SessionsCard is a card of sessions; Note says why there are none.
type SessionsCard struct {
	Head   Head
	Filter []Choice
	Table  SessionsTable
	Note   NoData
}

// ----- /projects -----

// ProjectsPage is the Projects page: one table, the pooled row last.
type ProjectsPage struct {
	Head   Head
	Note   NoData
	Rows   []ProjectLine
	Pooled *ProjectLine
	Foot   string
}

// ProjectLine is a row of the Projects table; the pooled row has no links
// and no spark, and its cells carry the lowest and highest project.
type ProjectLine struct {
	Project                                       Ref
	Label                                         string // the pooled row's name
	Sessions, Prompts, Got, Used, Wrong, Memories Cell
	Last                                          format.Time
	Spark                                         template.HTML
}

// share is num of den as a percent with its count, or the plain counts
// when den is under the metric's smallest sample.
func share(l format.Locale, num, den int64, minN int) Cell {
	switch {
	case den <= 0:
		return Cell{Text: dash}
	case den < int64(minN):
		return Cell{Text: l.Int(num) + " of " + l.Int(den) + ": too few", Class: "st-few"}
	}
	return Cell{Text: l.Percent(float64(num) / float64(den)), N: "(n " + l.Int(den) + ")"}
}

// extremes is "lowest 1 · highest 7" for a column of the pooled row; ""
// when the view gave none.
func extremes(l format.Locale, lo, hi map[string]float64, key string) string {
	a, ok1 := lo[key]
	b, ok2 := hi[key]
	if !ok1 || !ok2 {
		return ""
	}
	return "lowest " + l.Int(int64(math.Round(a))) + " · highest " + l.Int(int64(math.Round(b)))
}

func projectsOf(d PageData) ProjectsPage {
	r := d.resultOf("projects")
	l := d.L
	minN := 0
	if def, ok := metrics.DefOf("recall.hit_rate"); ok {
		minN = def.MinN
	}
	p := ProjectsPage{Head: fixedHead("projects", "Projects", "machine", d.CLI)}
	p.Head.Pager = pagerOf(d.Frame.Path, r.Tables.Page)
	line := func(row metrics.ProjectRow) ProjectLine {
		memories := Cell{Text: dash}
		if row.MemoriesOnServer != nil {
			memories.Text = l.Int(*row.MemoriesOnServer)
		}
		return ProjectLine{
			Sessions: Cell{Text: l.Int(row.Sessions)}, Prompts: Cell{Text: l.Int(row.Prompts)},
			Got: share(l, row.PromptsWithMemory, row.Prompts, minN), Used: Cell{Text: l.Int(row.UsedTurns)},
			Wrong: Cell{Text: l.Int(row.ReportedWrong)}, Memories: memories,
			Last: d.rel(r.GeneratedAt, row.LastActivity),
		}
	}
	for _, row := range r.Tables.Projects {
		ln := line(row)
		ln.Project = d.projectLink(row.Project)
		ln.Spark = promptsSpark(l, row.Series, "Prompts per day, "+views.Clip(row.Project))
		p.Rows = append(p.Rows, ln)
	}
	if po := r.Tables.Pooled; po != nil {
		ln := line(po.Totals)
		ln.Label, ln.Last = po.Label, format.Time{}
		if ln.Label == "" {
			ln.Label = "all projects pooled"
		}
		ln.Sessions.Range = extremes(l, po.Min, po.Max, "sessions")
		ln.Prompts.Range = extremes(l, po.Min, po.Max, "prompts")
		ln.Got.Range = extremes(l, po.Min, po.Max, "prompts_with_memory")
		ln.Used.Range = extremes(l, po.Min, po.Max, "used_turns")
		ln.Wrong.Range = extremes(l, po.Min, po.Max, "reported_wrong")
		p.Pooled = &ln
	}
	if len(p.Rows) == 0 && p.Pooled == nil {
		p.Note = NoData{Text: "No project has any activity in this window."}
		return p
	}
	p.Foot = "Pooled row: the whole window at once; beside each total, the lowest and highest project count. Memories come from the server and are not filtered by window."
	return p
}

// promptsSpark is the small line of a project's prompts per day. The
// window's first day is cut short and would draw as a dip: it is left out.
func promptsSpark(l format.Locale, series []metrics.Point, label string) template.HTML {
	if len(series) > 0 && series[0].Partial {
		series = series[1:]
	}
	y := make([]*float64, len(series))
	for i, pt := range series {
		y[i] = pt.Value
	}
	return chart.Spark{Label: label, Y: y, Fmt: unitFormat("count", l)}.HTML()
}

// ----- /sessions -----

// SessionsPage is the Sessions page.
type SessionsPage struct{ SessionsCard }

func sessionsOf(d PageData) SessionsPage {
	r := d.resultOf("sessions")
	c := SessionsCard{Head: fixedHead("sessions", "Sessions", "machine", d.CLI), Filter: filterLinks(d.Frame.Path, "has")}
	c.Head.Pager = pagerOf(d.Frame.Path, r.Tables.Page)
	c.Table = d.sessionTable(r.GeneratedAt, r.Tables.Sessions, false)
	c.Head.Aside = d.sessionCount(r.Tables.Page)
	if len(c.Table.Rows) == 0 {
		c.Note = NoData{Text: "No session in this window."}
		if d.P.Has != "" {
			c.Note.Text = "No session in this window matches this filter."
		}
	}
	return SessionsPage{c}
}

// ----- /memories -----

// MemoriesPage is the Memories page.
type MemoriesPage struct {
	Head          Head
	Badges        []CountBadge
	Order, Filter []Choice
	Rows          []MemoryLine
	Note          NoData
	Foot          string
}

// CountBadge is a count that links to the list it counts.
type CountBadge struct{ Count, Label, Href string }

// MemoryLine is a row of the Memories table.
type MemoryLine struct {
	ID, Project                Ref
	Shown, Used, Ignored, Rate string
	Ranking, Boost             string // Boost: the number, in developer mode only
	LastShown, LastUsed        format.Time
	NeverShown, NeverUsed      bool
	State                      Word
}

// badges are the stale counts a memories page links to, by the state
// filter each one opens.
var badges = []struct{ id, state string }{
	{"stale.never_used", "never_used"}, {"stale.hidden", "hidden"}, {"stale.reported", "wrong"},
}

// rankingWord says what the learned ranking does to a memory.
func rankingWord(boost float64) string {
	switch r := math.Round(boost*100) / 100; {
	case r > 0:
		return "raised"
	case r < 0:
		return "lowered"
	}
	return "unchanged"
}

func memoriesOf(d PageData) MemoriesPage {
	r := d.resultOf("memories")
	l := d.L
	p := MemoriesPage{Head: fixedHead("memories", "Memories", "machine", d.CLI),
		Order: filterLinks(d.Frame.Path, "sort"), Filter: filterLinks(d.Frame.Path, "state")}
	p.Head = withInfo(p.Head, "About these counts", "Recent-weighted counts", "",
		"Shown, used and ignored weigh recent shows more than old ones, so a memory that was useful last month and not since drifts down.")
	p.Head.Pager = pagerOf(d.Frame.Path, r.Tables.Page)
	// The badges link to the state filter that lists what they count.
	values, links := views.StateValues(), filterLinks(d.Frame.Path, "state")[1:]
	for _, b := range badges {
		v := d.view(r, b.id)
		if !hasValue(v) {
			continue
		}
		for i, sv := range values {
			if sv == b.state && i < len(links) {
				p.Badges = append(p.Badges, CountBadge{Count: l.Int(int64(math.Round(*v.M.Value))), Label: views.Word(views.MemoryStateWords, b.state), Href: links[i].Href})
			}
		}
	}
	for _, m := range r.Tables.Memories {
		state := Word{Word: "active"}
		if m.State != "" {
			state = Word{Word: views.Word(views.MemoryStateWords, m.State)}
		}
		if m.State == "wrong" {
			state.Class = "st-watch"
		}
		ln := MemoryLine{
			ID: idLink("/memories/", m.ID), Project: d.projectLink(m.Project), Shown: l.Int(m.Shown),
			Used: l.Fixed(m.Used, 1), Ignored: l.Fixed(m.Ignored, 1), Rate: dash, Ranking: rankingWord(m.Boost),
			LastShown: d.rel(r.GeneratedAt, m.LastShown), LastUsed: d.rel(r.GeneratedAt, m.LastUsed),
			NeverShown: m.LastShown == nil || m.LastShown.IsZero(), NeverUsed: m.LastUsed == nil || m.LastUsed.IsZero(),
			State: state,
		}
		if m.Rate != nil {
			ln.Rate = l.Percent(*m.Rate)
		}
		if d.Frame.Prefs.Dev {
			ln.Boost = signed(m.Boost)
		}
		p.Rows = append(p.Rows, ln)
	}
	if len(p.Rows) == 0 {
		p.Note = NoData{Text: "No memory has been shown in this window."}
		if d.P.State != "" {
			p.Note.Text = "No memory in this window matches this filter."
		}
	}
	p.Foot = "Counts are recent-weighted: older uses count less."
	return p
}

// ----- /days -----

// DaysPage is the Days page.
type DaysPage struct {
	Head Head
	Note NoData
	Rows []DayLine
}

// DayLine is a row of the Days table.
type DayLine struct {
	Day                                                   Ref
	Sessions, Prompts, Lookups, Got, Used, Wrong, Context string
	Sent, Failed                                          string
	FailedClass                                           string
}

func daysOf(d PageData) DaysPage {
	r := d.resultOf("days")
	l := d.L
	p := DaysPage{Head: fixedHead("days", "Days", "machine", d.CLI)}
	p.Head.Pager = pagerOf(d.Frame.Path, r.Tables.Page)
	for _, day := range r.Tables.Days {
		ln := DayLine{
			Day:      Ref{Text: views.Plain(day.Day, ""), Full: views.Plain(day.Day, ""), Href: pathHref("/days/", day.Day, windowQuery(d.P, "tz"))},
			Sessions: l.Int(day.Sessions), Prompts: l.Int(day.Prompts), Lookups: l.Int(day.Recalls), Got: l.Int(day.PromptsWithMemory),
			Used: l.Int(day.UsedTurns), Wrong: l.Int(day.Wrong), Context: l.Size(float64(day.ContextChars), "chars"),
			Sent: l.Int(day.SentOK), Failed: l.Int(day.SentFailed),
		}
		if day.SentFailed > 0 {
			ln.FailedClass = "st-bad"
		}
		p.Rows = append(p.Rows, ln)
	}
	if len(p.Rows) == 0 {
		p.Note = NoData{Text: "No day in this window has any activity."}
	}
	return p
}

// ----- /projects/{name} -----

// ProjectPage is the page about one project.
type ProjectPage struct {
	Name               Ref
	Tiles              []TileGroup
	NoMemories         bool
	Command            string
	Attribution        string
	TopUsed, NeverUsed MemoryListCard
	Sessions           SessionsCard
}

// MemoryListCard is a short list of memories of a project.
type MemoryListCard struct {
	Head       Head
	Rows       []MemoryListLine
	Note       NoData
	HasCommand bool // the list has a command column
}

// MemoryListLine is a row of such a list.
type MemoryListLine struct {
	ID      Ref
	Shown   string
	Last    format.Time
	Command string
}

func (d PageData) memoryList(head Head, rows []metrics.ReviewRow, now time.Time, hasCommand bool, empty string) MemoryListCard {
	c := MemoryListCard{Head: head, HasCommand: hasCommand}
	for _, row := range rows {
		c.Rows = append(c.Rows, MemoryListLine{ID: idLink("/memories/", row.ID), Shown: d.L.Int(row.Shown),
			Last: d.rel(now, row.LastShown), Command: row.Command})
	}
	if len(c.Rows) == 0 {
		c.Note = NoData{Text: empty}
	}
	return c
}

// tileParamsProject are the page's parameters for tiles about one project: its
// links and commands carry the project.
func tileParamsProject(p views.Params, name string) views.Params {
	p.Project = name
	return p
}

func projectOf(d PageData) ProjectPage {
	r := d.resultOf("projects")
	var pg ProjectPage
	t := r.Tables.Project
	name := d.ID
	if t != nil && t.Project != "" {
		name = t.Project
	}
	pg.Name = d.projectLink(name)
	pg.Tiles = tilesOf(r, tileParamsProject(d.P, name), d.L, d.Loc, d.Frame.Prefs.Dev)

	pg.Sessions = SessionsCard{Head: fixedHead("project-sessions", "Sessions of this project", "machine", d.CLI)}
	pg.Sessions.Head.Pager = pagerOf(d.Frame.Path, r.Tables.Page)
	if t == nil {
		pg.Sessions.Note = NoData{Text: "No session of this project in this window."}
		return pg
	}
	pg.NoMemories, pg.Command = t.NoMemories, t.Command
	if t.AttributionDisagreements > 0 {
		pg.Attribution = d.L.Int(t.AttributionDisagreements) + " memories are attributed to another project by the server."
		if t.AttributionDisagreements == 1 {
			pg.Attribution = "1 memory is attributed to another project by the server."
		}
	}
	pg.TopUsed = d.memoryList(fixedHead("project-top-used", "Most used", "machine", d.CLI), t.TopUsed, r.GeneratedAt, false,
		"No memory of this project has been used in this window.")
	pg.NeverUsed = d.memoryList(fixedHead("project-never-used", "Often shown, never used", "machine", d.CLI), t.NeverUsed, r.GeneratedAt, true,
		"No memory of this project is shown often without being used.")
	pg.Sessions.Table = d.sessionTable(r.GeneratedAt, t.Sessions, false)
	if len(pg.Sessions.Table.Rows) == 0 {
		pg.Sessions.Note = NoData{Text: "No session of this project in this window."}
	}
	return pg
}

// ----- /days/{date} -----

// DayPage is the page about one day.
type DayPage struct {
	Tiles      []TileGroup
	Note       string
	Prev, Next Ref // the days around it; Href "" at the end of the history
	Sessions   SessionsCard
}

func dayOf(d PageData) DayPage {
	r := d.resultOf("days")
	var pg DayPage
	t := r.Tables.Day
	day := d.ID
	if t != nil && t.Day != "" {
		day = t.Day
	}
	p := d.P
	if at, err := time.Parse("2006-01-02", day); err == nil {
		// The tiles are the day's: their links list that day's sessions.
		p.Days, p.DaysSet = 0, false
		p.Since, p.Until = day, at.AddDate(0, 0, 1).Format("2006-01-02")
		tz := windowQuery(d.P, "tz")
		pg.Prev = Ref{Text: "← " + at.AddDate(0, 0, -1).Format("Jan 2"), Href: "/days/" + at.AddDate(0, 0, -1).Format("2006-01-02") + tz}
		today := r.GeneratedAt.In(zoneOr(d.Loc))
		if at.Before(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)) {
			pg.Next = Ref{Text: at.AddDate(0, 0, 1).Format("Jan 2") + " →", Href: "/days/" + at.AddDate(0, 0, 1).Format("2006-01-02") + tz}
		}
	}
	pg.Tiles = tilesOf(r, p, d.L, d.Loc, d.Frame.Prefs.Dev)
	pg.Note = "One day is often too few to judge; the numbers are shown with their counts."

	pg.Sessions = SessionsCard{Head: fixedHead("day-sessions", "Sessions that day", "machine", d.CLI)}
	pg.Sessions.Head.Pager = pagerOf(d.Frame.Path, r.Tables.Page)
	pg.Sessions.Head.Aside = d.sessionCount(r.Tables.Page)
	if t != nil {
		// Links to projects from here keep the day, not the page's window.
		dd := d
		dd.P = p
		pg.Sessions.Table = dd.sessionTable(r.GeneratedAt, t.Sessions, true)
	}
	if len(pg.Sessions.Table.Rows) == 0 {
		pg.Sessions.Note = NoData{Text: "No sessions that day."}
	}
	return pg
}
