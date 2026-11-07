package dashboard

import (
	"html/template"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
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
	// Machines is the card of the PCs that share their activity; nil when the
	// view carries no table of them.
	Machines *MachinesCard
	Checks   ChecksCard
	Strip    HistoryCard
	Secret   SecretsCard
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
	Runs      []Run // the sentence when it holds code; read instead of Text
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
	p.Machines = d.machinesCard(r)
	p.Checks = d.checksCard(r, doctor)
	p.Strip = d.historyCard(r)
	p.Secret = d.secretsCard(r)
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
	c.Rows = append(c.Rows, d.thisPCRows(r)...)

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
	// A check reads the same word here as in the table of checks.
	for _, status := range []string{"error", "warn"} {
		for _, row := range r.Tables.Doctor {
			if row.Status != status {
				continue
			}
			class := "st-watch"
			if status == "error" {
				class, bad = "st-bad", bad+1
			} else {
				warn++
			}
			c.Items = append(c.Items, LookLine{Pill: Word{views.DoctorStatusWords[status], class}, Text: metrics.DoctorName(row.ID), Cmd: row.Fix})
		}
	}
	// A choice, not a fault: it is told, never counted as a warning.
	if m := r.Tables.Machines; m != nil && !m.Sharing {
		c.Items = append(c.Items, sharingOffItem(m.Off))
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

// HistoryCard is the health checks over time: one row per check, one mark
// per day.
type HistoryCard struct {
	Card
	Chart template.HTML
}

// historyCard is the strip of the daily upkeep's results.
func (d PageData) historyCard(r *metrics.Result) HistoryCard {
	l := d.L
	v := d.view(r, "doctor.history")
	h := d.head(v)
	h.Info.What = "One row per health check, one mark per run of the daily upkeep, by its result."
	h.Info.Limits, h.Info.BasedOn, h.Info.Smallest = "", "", ""
	h.Info.Paras = []string{"No run: a dashed ring means the upkeep did not run that day (the PC was off or asleep). Not a fault."}
	c := HistoryCard{Card: Card{Head: h, Dev: d.devIDs(v)}}
	rows := r.Tables.DoctorHistory
	if !hasValue(v) || len(rows) == 0 {
		if c.Note = d.noData(v); c.Note.Text == "" {
			c.Note = NoData{Text: "No health check has run yet."}
		}
		return c
	}
	loc := d.Loc
	if loc == nil {
		loc = time.UTC
	}
	strip := chart.StatusStrip{Label: "Health check results per daily run, " + d.frame().Window + ", one row per check; the table below lists them",
		Size: chart.Size{W: 1000, H: 300, Class: "vb1000"}, Fold: true, More: d.more()}
	for _, row := range rows {
		sr := chart.StatusRow{Name: metrics.DoctorName(row.Check)}
		for _, day := range row.Days {
			if t, err := time.ParseInLocation("2006-01-02", day.Day, loc); err == nil {
				sr.Days = append(sr.Days, chart.StatusDay{Day: t, Status: day.Status})
			}
		}
		strip.Rows = append(strip.Rows, sr)
	}
	without, _ := extraAs[int64](v.M, "days_without")
	strip.Note = format.Count(l, int64(*v.M.Value), "run", "runs") + " in " + format.Count(l, int64(len(rows[0].Days)), "day", "days") +
		"; " + format.Count(l, without, "day", "days") + " without a run."
	c.Chart = strip.HTML()
	return c
}

// SecretsCard is what the last scan of memory found that looks like a key,
// token or password. Found: the scan found some, and Count, Time and Took
// say how many, when and for how long. Pre, Time and Post are the sentence
// of the other states (Time may be left out); Cmd is the command that runs a
// scan, "" when none is needed.
type SecretsCard struct {
	Card
	Found     bool
	Count     string
	Took      string
	Pre, Post string
	Time      format.Time
	Cmd       string
}

// scanCmd is the command that scans memory.
const scanCmd = "backsight memory scan"

// secretsCard is the card of secrets.suspects in its four states: some
// found, none found, no scan yet, and a clean scan too old to say much.
func (d PageData) secretsCard(r *metrics.Result) SecretsCard {
	l := d.L
	v := d.view(r, "secrets.suspects")
	h := d.head(v)
	h.CLI = "" // the design gives this card none: the scan is the latest, whatever the window
	h.Info.What = "How many memories the last scan flagged as looking like a key, token or password, and when it ran."
	h.Info.Limits = "Watch on Flowing as soon as one is found."
	h.Info.BasedOn, h.Info.Smallest = "", ""
	h.Info.Privacy = "Only the count and the date are kept: never the text, never which memories."
	c := SecretsCard{Card: Card{Head: h, Dev: d.devIDs(v)}}
	if !hasValue(v) {
		c.Head.Pill = Word{"no scan yet", "st-few"}
		c.Pre, c.Cmd = "Memory has never been scanned on this PC.", scanCmd
		return c
	}
	count := int64(*v.M.Value)
	at, _ := extraAs[time.Time](v.M, "scanned_at")
	age, _ := extraAs[int64](v.M, "age_days")
	ms, hasMS := extraAs[float64](v.M, "ms")
	c.Time = format.Rel(r.GeneratedAt, at, d.Loc)
	took := ""
	if hasMS {
		took = scanTook(ms)
	}
	switch {
	case count > 0:
		c.Head.Pill = word(v)
		c.Found, c.Count, c.Took = true, l.Int(count), took
		c.Pre = "The scan lists them in your terminal, with what to do for each. The dashboard never shows which memories they are."
		c.Cmd = scanCmd
	case slices.Contains(v.M.Tags, "old_scan"):
		c.Head.Pill = Word{"old scan", "st-few"}
		c.Pre = "None found, but the last scan is " + format.Count(l, age, "day", "days") + " old: memories added since are not checked."
		c.Time = format.Time{}
		c.Cmd = scanCmd
	default:
		c.Head.Pill = word(v)
		c.Pre, c.Post = "None found by the last scan, ", "."
		if took != "" {
			c.Post = " (" + took + ")."
		}
	}
	return c
}

// scanTook is how long a scan ran: "800 ms", "41 s", "2 min".
func scanTook(ms float64) string {
	switch {
	case ms < 1000:
		return strconv.FormatInt(int64(math.Round(ms)), 10) + " ms"
	case ms < 59500:
		return strconv.FormatInt(int64(math.Round(ms/1000)), 10) + " s"
	}
	return chart.Span(ms)
}

// sharingOffItem is what tells a PC that does not share its activity how to
// turn that on. It is System's alone: sharing off is a choice. off says which
// setting is off (metrics.OffFeedback, OffLedger or OffBoth), so the remedy
// names the one that changes something.
func sharingOffItem(off string) LookLine {
	var named, set []Run
	switch off {
	case metrics.OffFeedback:
		named = []Run{{Code: "feedback.enabled"}, {Text: " is off"}}
		set = []Run{{Text: "set "}, {Code: "enabled = true"}}
	case metrics.OffBoth:
		named = []Run{{Code: "feedback.enabled"}, {Text: " and "}, {Code: "feedback.share_ledger"}, {Text: " are off"}}
		set = []Run{{Text: "set "}, {Code: "enabled = true"}, {Text: " and "}, {Code: "share_ledger = true"}}
	default:
		named = []Run{{Code: "feedback.share_ledger"}, {Text: " is off"}}
		set = []Run{{Text: "set "}, {Code: "share_ledger = true"}}
	}
	runs := []Run{{Text: "This PC does not share its activity: "}}
	runs = append(runs, named...)
	runs = append(runs, Run{Text: ". Your other PCs do not see it, and “all PCs” is off here. To share, "})
	runs = append(runs, set...)
	runs = append(runs, Run{Text: " under "}, Run{Code: "[feedback]"}, Run{Text: " in "}, Run{Code: "config.toml"}, Run{Text: " ("},
		Run{Code: "backsight paths"}, Run{Text: " shows where it is), then send now:"})
	return LookLine{
		Pill: Word{"info", "st-info"},
		Runs: runs,
		Cmd:  "backsight jobs tick",
		Why:  "Off is a valid choice and nothing is broken. Sharing sends daily totals and which memories Claude used: no project, session, memory text or computer name. The same setting shares the counts that rank memory on all your PCs.",
	}
}

// thisPCRows are the rows of This PC that tell which machine it is among the
// PCs and whether it shares: its short id, system and first day, and when it
// last sent. None when the view carries no table of the PCs, or this PC is
// not among them.
func (d PageData) thisPCRows(r *metrics.Result) []DLRow {
	m := r.Tables.Machines
	if m == nil {
		return nil
	}
	var self *metrics.MachineRow
	for i := range m.Rows {
		if m.Rows[i].This {
			self = &m.Rows[i]
		}
	}
	if self == nil {
		return nil
	}
	var rows []DLRow
	var parts []string
	if w := osWord(self.OS); w != "" {
		parts = append(parts, w)
	}
	if self.FirstSeen != "" {
		parts = append(parts, "since "+shortDay(self.FirstSeen, r.GeneratedAt))
	}
	if id := shortMachineID(self.Machine); id != "" || len(parts) > 0 {
		rows = append(rows, DLRow{Label: "Machine", ID: id, Pre: strings.Join(parts, " · ")})
	}
	switch {
	case !m.Sharing:
		rows = append(rows, textRow("Shares its activity", "no"))
	case self.LastSent != nil:
		rows = append(rows, DLRow{Label: "Shares its activity", Pre: "yes, last sent ", Time: format.Rel(r.GeneratedAt, *self.LastSent, d.Loc)})
	default:
		rows = append(rows, textRow("Shares its activity", "yes, nothing sent yet"))
	}
	return rows
}

// machineIDRe is what a short machine id looks like: the start of a lower-case
// UUID, longer only when two PCs start alike.
var machineIDRe = regexp.MustCompile(`^[0-9a-f][0-9a-f-]{7,35}$`)

// shortMachineID is id when it reads as a short machine id, else "": a name
// from another PC's document is never drawn as it came.
func shortMachineID(id string) string {
	if machineIDRe.MatchString(id) {
		return id
	}
	return ""
}

// MachinesCard is the card of the PCs that share their activity on this
// server. Notice says the server does not answer; Items holds the sharing-off
// item; Sub and Foot are the sentences under the table.
type MachinesCard struct {
	Card
	Notice  *Notice
	Items   []LookLine
	Short   bool // the four-column table of a card with nothing to compare
	Rows    []MachineLine
	Ignored []IgnoredLine
	Sub     string
	Foot    []Run
}

// Notice is a boxed sentence with its icon and class.
type Notice struct {
	Icon, Class, Text string
	Runs              []Run // read instead of Text when set
}

// MachineLine is a PC of the table. Counted is the word of "In “all PCs”",
// Why what is said beside it.
type MachineLine struct {
	ID, System, Version string
	This                bool
	First, Sent         format.Time
	Quiet               bool // gone quiet: the last send is drawn as a warning
	Counted             Word
	Why                 string
}

// IgnoredLine is a document set aside: the short id read from its name (""
// when it does not read) and the words for why; nothing else of it.
type IgnoredLine struct{ ID, Why string }

// machinesCard is the card of tables.machines in its states: the PCs, this PC
// only, sharing off and the server not answering. nil without the table.
func (d PageData) machinesCard(r *metrics.Result) *MachinesCard {
	m := r.Tables.Machines
	if m == nil {
		return nil
	}
	now := r.GeneratedAt
	h := fixedHead("machines", "Which PCs share their activity here?", "server", "backsight stats system")
	h.Anchor = "machines"
	h.Info = Info{
		Label: "About Machines", Name: "Machines",
		What:    "Each PC with sharing on sends, once a day, its daily totals to the memory server; “all PCs” adds them up. A PC is named by the start of its machine id, its system and the day it first sent.",
		Rows:    []InfoRow{{Term: "Not here", Runs: []Run{{Text: "A PC with sharing off ("}, {Code: "feedback.enabled"}, {Text: " or "}, {Code: "feedback.share_ledger"}, {Text: ") sends nothing, so no one can list it."}}}},
		Privacy: "The totals hold no project name, session, memory id, text or computer name.",
		Term:    "backsight:metrics:<machine_id>",
	}
	c := &MachinesCard{Card: Card{Head: h}}
	if !m.Sharing {
		c.Head.Info, c.Head.CLI, c.Head.Scope = Info{}, "", ""
		c.Head.Pill = Word{"sharing off", "st-info"}
		c.Items = []LookLine{sharingOffItem(m.Off)}
		c.Head.Note = "The same item shows in “What needs a look here?”, never in “What should I do?”: it is a choice, not a fault."
		return c
	}
	stale := false
	for _, row := range m.Rows {
		ln := MachineLine{ID: shortMachineID(row.Machine), This: row.This, System: dash, Version: dash, Quiet: row.Stale}
		if w := osWord(row.OS); w != "" {
			ln.System = w
		}
		if row.BinaryVersion != "" {
			ln.Version = row.BinaryVersion
		}
		if row.FirstSeen != "" {
			ln.First = format.Time{Text: shortDay(row.FirstSeen, now), Datetime: row.FirstSeen, Title: row.FirstSeen}
		}
		if row.LastSent != nil {
			ln.Sent = sentAt(now, *row.LastSent, d.Loc)
		}
		switch {
		case row.This:
			ln.Counted, ln.Why = Word{"counted", "st-ok"}, "its own figures, live"
		case row.Stale:
			stale = true
			ln.Counted = Word{"gone quiet", "st-watch"}
			if row.Until != nil {
				ln.Counted.Word = "up to " + shortDay(*row.Until, now)
			}
		default:
			ln.Counted = Word{"counted", "st-ok"}
			if !row.Active {
				ln.Why = "no activity in this window"
			}
		}
		c.Rows = append(c.Rows, ln)
	}
	for _, g := range m.Ignored {
		c.Ignored = append(c.Ignored, IgnoredLine{ID: shortMachineID(g.Machine), Why: wordOr(views.IgnoredWords, g.Reason, views.IgnoredWords["not_valid"])})
	}
	only := len(m.Rows) == 1
	aside := views.CountOf(int64(len(m.Rows)), "PC", "PCs")
	switch len(m.Rows) {
	case 0:
		aside = "no PC yet" // this PC runs the server only and no other has sent
	case 1:
		aside = "this PC only"
	}
	if n := len(m.Ignored); n > 0 {
		aside += " · " + views.CountOf(int64(n), "document", "documents") + " ignored"
	}
	c.Head.Aside = aside
	silent := serverSilent(d.Frame)
	c.Short = (only || silent) && len(m.Rows) > 0 && len(c.Ignored) == 0 && !stale
	switch {
	case silent:
		c.Head.Pill = Word{"not answering", "st-watch"}
		text := "Memory server not answering"
		if since := serverSince(d.Frame); !since.IsZero() {
			text += " since " + format.Clock(now, since, d.Loc)
		}
		text += "."
		if m.FetchedAt != nil {
			text += " The list is the one fetched at " + format.Clock(now, *m.FetchedAt, d.Loc) + "."
		} else {
			text += " The list has not been fetched yet."
		}
		c.Notice = &Notice{Icon: "server", Class: "st-watch", Text: text}
		c.Foot = []Run{{Text: "This PC’s figures wait and are sent once the server answers; nothing is lost."}}
		return c
	case len(m.Rows) == 0:
		c.Sub = "No PC shares its activity on this server yet. This PC runs the server only: the PCs that use it appear here after their first daily send, with sharing on (the default)."
		if len(c.Ignored) == 0 {
			return c
		}
	case only:
		c.Sub = "Only this PC shares its activity on this server. Your other PCs appear here after their first daily send, with sharing on (the default). The scope line shows no switch until then."
		if len(c.Ignored) == 0 {
			return c
		}
	}
	every := ", every 6 h. An ignored document is left out whole; what it holds is not shown. A PC that has not sent for 30 days drops off this list."
	if m.FetchedAt == nil {
		c.Foot = []Run{{Text: "Not fetched from the memory server yet; it is fetched every 6 h. An ignored document is left out whole; what it holds is not shown. A PC that has not sent for 30 days drops off this list."}}
	} else {
		c.Foot = []Run{{Text: "Fetched from the memory server "}, {Time: format.Rel(now, *m.FetchedAt, d.Loc)}, {Text: every}}
	}
	return c
}

// sentAt is when a PC last sent, as the pages say it: "2 h ago", "yesterday",
// then "9 d ago" as far as a PC stays in the list (the date stays in the
// title).
func sentAt(now, at time.Time, loc *time.Location) format.Time {
	t := format.Rel(now, at, loc)
	if loc == nil {
		loc = time.UTC
	}
	n, a := now.In(loc), at.In(loc)
	ny, nm, nd := n.Date()
	ay, am, ad := a.Date()
	days := int(time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC).Sub(time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	if days >= 7 {
		t.Text = strconv.Itoa(days) + " d ago"
	}
	return t
}
