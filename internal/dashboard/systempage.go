package dashboard

import (
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// SystemPage is the System page: the memory server, this PC and what needs
// a look side by side, then the health checks.
type SystemPage struct {
	Server ServerCard
	PC     PCCard
	Look   LookCard
	Checks ChecksCard
}

// ServerCard is the memory server as this PC sees it.
type ServerCard struct {
	Card
	Rows []DLRow
}

// PCCard is this PC: the program, the update, the jobs, the backup, the index.
type PCCard struct {
	Card
	Rows []DLRow
	Cmd  string // the command that updates; "" when nothing newer is out
}

// LookCard is what needs a look: the view's items, then the checks that
// are not fine.
type LookCard struct {
	Card
	Items []LookLine
	Info  string
}

// LookLine is something that needs a look: its status, a sentence, and
// the command that helps.
type LookLine struct {
	Pill      Word
	Text, Why string
	Cmd       string
}

// ChecksCard is the health checks, the worst first.
type ChecksCard struct {
	Card
	Rows []CheckLine
}

// CheckLine is a health check.
type CheckLine struct{ Status, Class, Check, Fix string }

// doctorClass is the class that colours the status of a check.
var doctorClass = map[string]string{"ok": "st-ok", "info": "st-info", "warn": "st-watch", "error": "st-bad"}

func systemOf(d PageData) SystemPage {
	r := d.V["system"]
	if r == nil {
		r = &metrics.Result{}
	}
	reach, doctor := d.view(r, "server.reach"), d.view(r, "doctor.check")
	jobs, backlog := d.view(r, "jobs.last_run"), d.view(r, "local.backlog")
	info := r.Tables.System
	if info == nil {
		info = &metrics.SystemInfo{}
	}
	var p SystemPage
	p.Server = d.serverCard(r, reach, *info)
	p.PC = d.pcCard(r, jobs, backlog, *info)
	p.Look = d.lookCard(r, doctor)
	p.Checks = d.checksCard(r, doctor)
	return p
}

// serverCard is the memory server: where it is, what it runs, when it last
// answered, whether recall is paused, how many prompts that cost.
func (d PageData) serverCard(r *metrics.Result, reach MetricView, s metrics.SystemInfo) ServerCard {
	l := d.L
	h := d.head(reach)
	h.Title, h.Scope = "Memory server", "server" // the card is the server's, with this PC's record of its answers
	c := ServerCard{Card: Card{Head: h, Note: d.noData(reach), Dev: d.devIDs(reach)}}
	if c.Note.Text != "" {
		return c
	}
	// The pill is the server's answer when this page read it, else what this PC recorded.
	switch read := d.Frame.ServerRead; {
	case read != nil && read.OK:
		c.Head.Pill = Word{"answering", "st-ok"}
	case read != nil:
		c.Head.Pill = Word{"not answering", "st-watch"}
	default:
		c.Head.Pill = cardPill(reach)
	}
	if sc := d.Frame.Scope; sc != nil && sc.Server != "" {
		c.Rows = append(c.Rows, DLRow{Label: "Address", Pre: sc.Server, Mono: true})
	}
	version := s.ServerVersion
	if version == "" {
		version = dash
	}
	c.Rows = append(c.Rows, textRow("Version", version),
		textRow("Runs as", wordOr(views.RuntimeWords, s.Runtime, dash)+", "+wordOr(views.ModeWords, s.Mode, dash)))
	last := textRow("Last answer", "no answer recorded yet")
	if !s.Reach.LastOK.IsZero() {
		last = DLRow{Label: "Last answer", Time: format.Rel(r.GeneratedAt, s.Reach.LastOK, d.Loc)}
	}
	c.Rows = append(c.Rows, last)
	if !s.Reach.FailingSince.IsZero() {
		c.Rows = append(c.Rows, DLRow{Label: "Not answering since", Time: format.Rel(r.GeneratedAt, s.Reach.FailingSince, d.Loc), Class: "st-watch"})
	}
	pause := textRow("Pausing after errors", "no")
	if until := s.Reach.BreakerOpenUntil; until.After(r.GeneratedAt) {
		pause = DLRow{Label: "Pausing after errors", Pre: "yes, until ", Time: format.Rel(r.GeneratedAt, until, d.Loc),
			Post: " after " + views.CountOf(int64(s.Reach.BreakerFailures), "failure", "failures"), Class: "st-watch"}
	}
	c.Rows = append(c.Rows, pause)
	if hasValue(reach) {
		skipped, _ := extraAs[int64](reach.M, "skipped_prompts")
		c.Rows = append(c.Rows, textRow("Prompts skipped", l.Int(skipped)+" in the "+d.frame().Window))
	}
	return c
}

// pcCard is this PC.
func (d PageData) pcCard(r *metrics.Result, jobs, backlog MetricView, s metrics.SystemInfo) PCCard {
	l := d.L
	h := withInfo(fixedHead("this-pc", "This PC", "machine", ""), "About this PC", "This PC", "",
		views.Label(jobs.Def)+": "+jobs.Def.Question, views.Label(backlog.Def)+": "+backlog.Def.Question)
	c := PCCard{Card: Card{Head: h, Dev: d.devIDs(jobs, backlog)}}
	c.Rows = append(c.Rows, textRow("backsight", d.Frame.Version))

	update := textRow("Update", "none newer")
	switch {
	case s.Update.CheckFailed:
		update.Pre = "the last check failed"
	case s.Update.Newer > 0:
		update.Pre, update.Class = views.CountOf(int64(s.Update.Newer), "newer release", "newer releases"), "st-info"
		c.Cmd = "/backsight:update"
	}
	c.Rows = append(c.Rows, update)

	jobsRow := textRow("Background jobs", "none recorded yet")
	var runs []string
	for _, j := range r.Tables.Jobs {
		runs = append(runs, wordOr(views.JobWords, j.Name, "background job")+": "+wordOr(views.JobStatusWords, j.Status, "not recorded")+", "+views.Age(j.AgeMS)+" ago")
	}
	if len(runs) > 0 {
		jobsRow.Pre = strings.Join(runs, " · ")
	}
	c.Rows = append(c.Rows, jobsRow)

	if hasValue(backlog) {
		notes, _ := extraAs[int64](backlog.M, "notes_queued")
		sessions, _ := extraAs[int64](backlog.M, "sessions_waiting")
		aside, _ := extraAs[int64](backlog.M, "quarantined")
		text := "0 waiting"
		if n := notes + sessions + aside; n > 0 {
			text = l.Int(n) + " waiting: " + views.CountOf(notes, "note", "notes") + ", " + views.CountOf(sessions, "session", "sessions") + ", " + l.Int(aside) + " set aside"
		}
		c.Rows = append(c.Rows, textRow("Waiting to be sent", text))
	}

	backup := textRow("Last backup", "none yet")
	if !s.Backup.CreatedAt.IsZero() {
		backup = DLRow{Label: "Last backup", Time: format.Rel(r.GeneratedAt, s.Backup.CreatedAt, d.Loc), Post: " · not verified"}
		if s.Backup.OK {
			backup.Post = " · verified"
		}
	}
	c.Rows = append(c.Rows, backup)

	idx := DLRow{Label: "History index", Pre: "updated "}
	if s.Index.IngestedAt.IsZero() {
		idx.Pre = "never read"
	} else {
		idx.Time = format.Rel(r.GeneratedAt, s.Index.IngestedAt, d.Loc)
		switch lag := s.Index.LagSeconds; {
		case lag <= 0:
			idx.Post = ", up to date"
		case lag < 60:
			idx.Post = ", " + l.Int(lag) + " s behind"
		default:
			idx.Post = ", " + views.Age(lag*1000) + " behind"
		}
	}
	if s.Index.Partial {
		idx.Post += ", still catching up"
	}
	c.Rows = append(c.Rows, idx)
	return c
}

// checkRank orders the health checks, the worst first.
var checkRank = map[string]int{"error": 0, "warn": 1, "info": 2, "ok": 3}

// lookCard is what needs a look: the items the view returns, then the
// checks that are a warning or a problem.
func (d PageData) lookCard(r *metrics.Result, doctor MetricView) LookCard {
	h := fixedHead("attention", "What needs a look here?", "", "")
	c := LookCard{Card: Card{Head: h, Dev: d.devIDs(doctor)}}
	var warn, bad int
	for _, it := range r.Attention {
		word, class := pillOf(it.Severity)
		c.Items = append(c.Items, LookLine{Pill: Word{word, class}, Text: it.Text, Why: it.Why, Cmd: it.Command})
		switch it.Severity {
		case metrics.SeverityBad:
			bad++
		case metrics.SeverityWatch:
			warn++
		}
	}
	for _, status := range []string{"error", "warn"} {
		for _, row := range r.Tables.Doctor {
			if row.Status != status {
				continue
			}
			w := Word{"watch", "st-watch"}
			if status == "error" {
				w, bad = Word{"attention", "st-bad"}, bad+1
			} else {
				warn++
			}
			c.Items = append(c.Items, LookLine{Pill: w, Text: metrics.DoctorName(row.ID) + ": " + wordOr(views.DoctorStatusWords, status, status), Cmd: row.Fix})
		}
	}
	var counts []string
	if bad > 0 {
		counts = append(counts, views.CountOf(int64(bad), "problem", "problems"))
	}
	if warn > 0 {
		counts = append(counts, views.CountOf(int64(warn), "warning", "warnings"))
	}
	c.Head.Aside = strings.Join(counts, ", ")
	switch {
	case len(c.Items) > 0:
	case hasValue(doctor):
		c.Note = NoData{Text: "Nothing needs a look."}
	default:
		c.Note = d.noData(doctor)
	}
	c.Info = "Warnings that do not affect memory stay on this page; they never change the verdict."
	return c
}

// checksCard is the doctor's checks, the worst first, the doctor's own order
// within a status.
func (d PageData) checksCard(r *metrics.Result, doctor MetricView) ChecksCard {
	h := d.head(doctor)
	c := ChecksCard{Card: Card{Head: h, Dev: d.devIDs(doctor)}}
	rows := slices.Clone(r.Tables.Doctor)
	rank := func(status string) int {
		if n, ok := checkRank[status]; ok {
			return n
		}
		return len(checkRank)
	}
	slices.SortStableFunc(rows, func(a, b metrics.DoctorRow) int { return rank(a.Status) - rank(b.Status) })
	for _, row := range rows {
		c.Rows = append(c.Rows, CheckLine{Status: wordOr(views.DoctorStatusWords, row.Status, "unknown"), Class: wordOr(doctorClass, row.Status, "st-few"),
			Check: metrics.DoctorName(row.ID), Fix: row.Fix})
	}
	if len(c.Rows) == 0 {
		if c.Note = d.noData(doctor); c.Note.Text == "" {
			c.Note = NoData{Text: "No check ran."}
		}
	}
	return c
}
