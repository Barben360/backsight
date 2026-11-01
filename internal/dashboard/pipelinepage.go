package dashboard

import (
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// PipelinePage is the Pipeline page: what is wrong, if anything, then the
// cards in the order they are read.
type PipelinePage struct {
	Alert   *Alert
	Flow    Card
	Backlog BacklogCard
	Queue   QueueCard
	Delay   Card
	Hooks   HooksCard
	Jobs    JobsCard
}

// Alert is the sentence at the top of a pipeline with something wrong: Class
// is st-watch or st-bad.
type Alert struct{ Class, Icon, Lead, Rest string }

// BacklogCard is what waits on this PC and what to run about it.
type BacklogCard struct {
	Card
	Rows        []BacklogRow
	Oldest      string // how long the oldest has waited; "" nothing waits
	OldestClass string // the class that colours it; "" none
	Why         string
}

// BacklogRow is a kind of thing that waits: its count, the command that
// sends it, or a muted word when none is needed.
type BacklogRow struct{ What, Count, Cmd, Muted string }

// QueueCard is the server's queue and the tasks that failed.
type QueueCard struct {
	Card
	Counts       []Lead
	Rows         []QueueRow
	Shown, Count int
	More         string
	None         string // a sentence for a queue with no failed task
}

// QueueRow is a failed server task: what it was, how long ago, how often it ran again.
type QueueRow struct{ Task, Age, Retries string }

// HooksCard is how Claude Code's steps stopped.
type HooksCard struct {
	Card
	Rows       []HookRow
	WindowHead string
}

// HookRow is a way a step stopped, in the last 24 hours and in the window;
// Kind is the stored name, which the page writes in words.
type HookRow struct{ Kind, Day, Window string }

// JobsCard is the last run of each background job.
type JobsCard struct {
	Card
	Rows []JobLine
}

// JobLine is the last run of a job.
type JobLine struct {
	Job, Code, Result, Class, Took string
	When                           format.Time
}

// hookClasses colour the ways a step stopped, in the order of the rows.
var hookClasses = []string{"f-seq1", "f-seq2", "f-seq3", "f-neutral", "f-aside"}

func pipelineOf(d PageData) PipelinePage {
	r := d.V["pipeline"]
	if r == nil {
		r = &metrics.Result{}
	}
	l := d.L
	flow, outcomes := d.view(r, "pipeline.flow"), d.view(r, "retain.outcomes")
	backlog, queue := d.view(r, "local.backlog"), d.view(r, "server.queue")
	delay, hooks, jobs := d.view(r, "server.extraction_delay"), d.view(r, "hooks.failures"), d.view(r, "jobs.last_run")
	var p PipelinePage

	p.Alert = d.pipelineAlert(flow, backlog, hooks)

	h := d.head(flow)
	h.Pill = cardPill(flow)
	p.Flow = Card{Head: h, Note: d.noData(flow, outcomes), Dev: d.devIDs(flow, outcomes)}
	if p.Flow.Note.Text == "" {
		if flow.M.Num != nil && flow.M.Den != nil && flow.M.Value != nil {
			p.Flow.Leads = []Lead{{Big: l.Int(*flow.M.Num) + " of " + l.Int(*flow.M.Den), CI: "· " + l.Value(flow.Def, flow.M), Label: "sent"}}
		}
		p.Flow.Charts = []PathChart{
			{Name: "Share sent, per day", HTML: linesOf(l, "Share of sessions sent, per day", d.more(), true, flow)},
			{Name: outcomes.Def.Question, HTML: flowOf(l, d.more(), outcomes)},
		}
		p.Flow.Extras = extrasOf(flow, outcomes)
		if flow.M.Status == metrics.StatusWatch || flow.M.Status == metrics.StatusBad {
			p.Flow.Hint, p.Flow.HintCmd = "Sessions not reaching the server? This checks both ends:", "backsight doctor"
		}
	}

	h = d.head(backlog)
	h.Pill = cardPill(backlog)
	p.Backlog.Card = Card{Head: h, Note: d.noData(backlog), Dev: d.devIDs(backlog)}
	if p.Backlog.Note.Text == "" {
		if ld, ok := d.lead(backlog, "waiting to be sent"); ok {
			p.Backlog.Leads = []Lead{ld}
		}
		notes, _ := extraAs[int64](backlog.M, "notes_queued")
		sessions, _ := extraAs[int64](backlog.M, "sessions_waiting")
		aside, _ := extraAs[int64](backlog.M, "quarantined")
		oldest, _ := extraAs[int64](backlog.M, "oldest_ms")
		row := func(what string, n int64, cmd, muted string) BacklogRow {
			b := BacklogRow{What: what, Count: l.Int(n)}
			if n > 0 {
				b.Cmd, b.Muted = cmd, muted
			}
			return b
		}
		p.Backlog.Rows = []BacklogRow{
			row("sessions", sessions, "backsight retain flush", ""),
			row("notes", notes, "", "sent with the next session"),
			row("set aside after an error", aside, "backsight retain retry-quarantine", ""),
		}
		if oldest > 0 {
			p.Backlog.Oldest = views.Age(oldest)
			if w := word(backlog); w.Class == "st-watch" || w.Class == "st-bad" {
				p.Backlog.OldestClass = w.Class
			}
		}
		p.Backlog.Extras = detailsOf(backlog)
		p.Backlog.Why = "Sessions that are not sent never become memories. Nothing is lost while they wait; a day-old backlog means sending is stuck."
	}

	h = d.head(queue)
	h.Pill = cardPill(queue)
	p.Queue.Card = Card{Head: h, Note: d.noData(queue), Dev: d.devIDs(queue)}
	if p.Queue.Note.Text == "" {
		if ld, ok := d.lead(queue, "waiting or running"); ok {
			p.Queue.Leads = []Lead{ld}
		}
		pending, _ := extraAs[int64](queue.M, "pending")
		running, _ := extraAs[int64](queue.M, "processing")
		failed, _ := extraAs[int64](queue.M, "failed")
		capped, _ := extraAs[bool](queue.M, "failed_capped")
		failedBig := l.Int(failed)
		if capped {
			failedBig += "+"
		}
		p.Queue.Counts = []Lead{{Big: l.Int(pending), Label: "queued"}, {Big: l.Int(running), Label: "running"}, {Big: failedBig, Label: "failed in the last 7 days"}}
		rows := r.Tables.FailedOperations
		for _, o := range rows[:min(len(rows), chart.MaxTableRows)] {
			// What the server calls a task is shown only as a word the dashboard knows.
			p.Queue.Rows = append(p.Queue.Rows, QueueRow{Task: wordOr(views.OperationWords, o.Type, views.OperationWords["other"]),
				Age: views.Age(o.AgeMS), Retries: l.Int(o.Retries)})
		}
		p.Queue.Shown, p.Queue.Count, p.Queue.More = len(p.Queue.Rows), len(rows), d.more()
		switch {
		case len(rows) > 0 || failed > 0:
			p.Queue.Hint, p.Queue.HintCmd = "No error text is shown here. This names the fix:", "backsight doctor"
		default:
			p.Queue.None = "No server task failed in the last 7 days."
		}
	}

	h = d.head(delay)
	h.Pill = cardPill(delay)
	p.Delay = Card{Head: h, Note: d.noData(delay), Dev: d.devIDs(delay)}
	if p.Delay.Note.Text == "" {
		if p50, ok := extraAs[float64](delay.M, "p50"); ok {
			p.Delay.Figs = append(p.Delay.Figs, Fig{Label: "typical", Value: l.Duration(p50)})
		}
		slow := Fig{Label: "slowest 1 in 20", Value: l.Value(delay.Def, delay.M)}
		if w := delay.Def.Absolute.Watch; w != nil {
			slow.Sub = "watch over " + l.Duration(*w)
		}
		p.Delay.Figs = append(p.Delay.Figs, slow)
		if delay.M.Den != nil {
			p.Delay.Figs = append(p.Delay.Figs, Fig{Label: "based on", Value: l.Int(*delay.M.Den), Sub: "last completed server tasks"})
		}
	}

	p.Hooks = d.hooksOf(hooks)

	h = d.head(jobs)
	h.Pill = cardPill(jobs)
	p.Jobs.Card = Card{Head: h, Note: d.noData(jobs), Dev: d.devIDs(jobs)}
	if p.Jobs.Rows = jobLines(d, r); len(p.Jobs.Rows) == 0 && p.Jobs.Note.Text == "" {
		p.Jobs.Note = NoData{Text: "No data."}
	} else if len(p.Jobs.Rows) > 0 {
		p.Jobs.Note = NoData{}
	}
	return p
}

// jobLines are the last runs of the jobs: the age as the metric writes it,
// the result as a word, whatever the log held.
func jobLines(d PageData, r *metrics.Result) []JobLine {
	var out []JobLine
	for _, j := range r.Tables.Jobs {
		when := format.Rel(r.GeneratedAt, j.LastAt, d.Loc)
		when.Text = views.Age(j.AgeMS) + " ago"
		class := "st-few"
		switch j.Status {
		case "done":
			class = "st-ok"
		case "partial", "started":
			class = "st-watch"
		case "failed", "error":
			class = "st-bad"
		}
		out = append(out, JobLine{Job: wordOr(views.JobWords, j.Name, "background job"), Code: "jobs " + j.Name, When: when,
			Result: wordOr(views.JobStatusWords, j.Status, "not recorded"), Class: class, Took: d.L.Duration(float64(j.MS))})
	}
	return out
}

// pipelineAlert is the sentence for a pipeline that is not fine: what
// failed, how long the oldest item has waited, the steps that crashed. Nil
// when the three metrics are fine or have nothing to say.
func (d PageData) pipelineAlert(flow, backlog, hooks MetricView) *Alert {
	class := ""
	for _, v := range []MetricView{flow, backlog, hooks} {
		switch {
		case v.M.Status == metrics.StatusBad:
			class = "st-bad"
		case v.M.Status == metrics.StatusWatch && class == "":
			class = "st-watch"
		}
	}
	if class == "" {
		return nil
	}
	l := d.L
	var sentences []string
	var lead []string
	if n, _ := extraAs[int64](flow.M, "failed"); n > 0 {
		lead = append(lead, format.Count(l, n, "session could not be sent", "sessions could not be sent"))
	}
	if n, _ := extraAs[int64](flow.M, "set_aside"); n > 0 {
		lead = append(lead, l.Int(n)+" set aside after an error")
	}
	if len(lead) > 0 {
		sentences = append(sentences, strings.Join(lead, "; ")+".")
	}
	if ms, _ := extraAs[int64](backlog.M, "oldest_ms"); ms > 0 && backlog.M.Status != metrics.StatusOK {
		sentences = append(sentences, "The oldest item has waited on this PC for "+views.Age(ms)+".")
	}
	last24, _ := extraAs[map[string]int64](hooks.M, "last_24h")
	var crashed int64
	for _, n := range last24 {
		crashed += n
	}
	if crashed > 0 {
		sentences = append(sentences, format.Count(l, crashed, "step crashed", "steps crashed")+" in the last 24 h.")
	}
	if len(sentences) == 0 {
		return nil
	}
	icon := "i-watch"
	if class == "st-bad" {
		icon = "i-bad"
	}
	return &Alert{Class: class, Icon: icon, Lead: sentences[0], Rest: strings.Join(sentences[1:], " ")}
}

// hooksOf is the card of the steps that crashed: how each stopped, in the
// last 24 hours and over the window, and the days it happened.
func (d PageData) hooksOf(hooks MetricView) HooksCard {
	l := d.L
	h := d.head(hooks)
	h.Pill = cardPill(hooks)
	c := HooksCard{Card: Card{Head: h, Note: d.noData(hooks), Dev: d.devIDs(hooks)}}
	if c.Note.Text != "" {
		return c
	}
	window, _ := extraAs[map[string]int64](hooks.M, "by_kind")
	day, _ := extraAs[map[string]int64](hooks.M, "last_24h")
	var kinds []string
	for k := range window {
		kinds = append(kinds, k)
	}
	for k := range day {
		if _, ok := window[k]; !ok {
			kinds = append(kinds, k)
		}
	}
	kinds = slices.DeleteFunc(kinds, func(k string) bool { return window[k] == 0 && day[k] == 0 })
	slices.SortFunc(kinds, func(a, b string) int {
		if window[a] != window[b] {
			return int(window[b] - window[a])
		}
		return strings.Compare(a, b)
	})
	if len(kinds) == 0 {
		c.Note = NoData{Text: "No step failed in the " + d.frame().Window + "."}
		return c
	}
	if ld, ok := d.lead(hooks, "steps crashed"); ok {
		c.Leads = []Lead{ld}
	}
	for _, k := range kinds {
		c.Rows = append(c.Rows, HookRow{Kind: k, Day: l.Int(day[k]), Window: l.Int(window[k])})
	}
	c.WindowHead = strings.ToUpper(d.frame().Window[:1]) + d.frame().Window[1:]
	inWindow := slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return window[k] == 0 })
	name := func(k string) string { return wordOr(views.FailureKindWords, k, views.FailureKindWords["unknown"]) }
	c.Charts = []PathChart{{HTML: partsOf(l, "Steps that crashed, by how they stopped, per day", d.more(), hooks, inWindow, wordsOf(inWindow, name), hookClasses, "")}}
	c.Extras = extrasOf(hooks)
	c.Hint, c.HintCmd = "To report it, this prints a redacted summary and a link (nothing is sent):", "backsight report"
	return c
}
