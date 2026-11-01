package dashboard

import (
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// BankPage is the Bank page: the strip of figures, then the cards in the
// order they are read.
type BankPage struct {
	StripTitle    string // the question of the strip, its accessible name
	Strip         []Fig
	Growth        Card
	Projects      ProjectsCard
	Consolidation ConsolidationCard
	Notes, Churn  Card
	Review        ReviewCard
}

// ProjectsCard is the memories of each project, a bar beside each.
type ProjectsCard struct {
	Card
	Bars         []ProjectBar
	Shown, Count int
	More         string // the command listing every project, under a capped table
}

// ConsolidationCard is whether the summaries are being built.
type ConsolidationCard struct {
	Card
	Rows []DLRow
	Info string
}

// ReviewCard is the memories worth a look, three lists behind one head.
type ReviewCard struct {
	Card
	Tabs []ReviewTab
}

// ReviewTab is one list of the review queue.
type ReviewTab struct {
	Title, Count, Question string
	Open                   bool
	Rows                   []ReviewLine
	Note                   NoData
	Foot                   string // "Showing 50 of 60 memories." under a capped list
	More                   string
}

// ReviewLine is a row of a review list.
type ReviewLine struct {
	ID, Project, Shown, Command string
	Last                        format.Time
}

// reviewTabs are the review lists: the metric that counts each, its list in
// the view's table, and the sentence for a list with nothing in it.
var reviewTabs = []struct {
	id    string
	rows  func(r *metrics.Review) []metrics.ReviewRow
	empty string
}{
	{"stale.never_used", func(r *metrics.Review) []metrics.ReviewRow { return r.NeverUsed }, "No memory is shown often without being used."},
	{"stale.hidden", func(r *metrics.Review) []metrics.ReviewRow { return r.Hidden }, "No memory is hidden."},
	{"stale.reported", func(r *metrics.Review) []metrics.ReviewRow { return r.Reported }, "No memory is reported wrong."},
}

// churnOrder are the actions on a memory, bottom of the bars first.
var churnOrder = []string{"forget", "retire", "hide", "unhide"}

func bankOf(d PageData) BankPage {
	r := d.V["bank"]
	if r == nil {
		r = &metrics.Result{}
	}
	l := d.L
	size, growth := d.view(r, "bank.size"), d.view(r, "bank.growth")
	byProject, cons := d.view(r, "bank.by_project"), d.view(r, "bank.consolidation")
	notes, churn := d.view(r, "notes.stored"), d.view(r, "bank.churn")
	var b BankPage

	b.StripTitle = size.Def.Question
	b.Strip = d.bankStrip(r, size, growth)

	b.Growth = Card{Head: d.head(growth), Note: d.noData(growth), Dev: d.devIDs(growth)}
	if b.Growth.Note.Text == "" {
		if ld, ok := d.lead(growth, "new memories a day"); ok {
			b.Growth.Leads = []Lead{ld}
		}
		b.Growth.Extras = extrasOf(growth)
		b.Growth.Charts = []PathChart{{HTML: kindsOf(l, d.more(), growth)}}
	}

	// bank.by_project's value is how many projects hold memories.
	b.Projects.Card = Card{Head: d.head(byProject), Dev: d.devIDs(byProject)}
	switch rows := r.Tables.ByProject; {
	case !hasValue(byProject):
		b.Projects.Note = d.noData(byProject)
	case len(rows) == 0:
		b.Projects.Note = NoData{Text: "No memory carries a project yet."}
	default:
		if ld, ok := d.lead(byProject, "projects hold memories"); ok {
			b.Projects.Leads = []Lead{ld}
		}
		b.Projects.Count = len(rows)
		b.Projects.Bars = projectBars(l, rows[:min(len(rows), chart.MaxTableRows)])
		b.Projects.Shown, b.Projects.More = len(b.Projects.Bars), d.more()
	}

	b.Consolidation.Card = Card{Head: d.head(cons), Note: d.noData(cons), Dev: d.devIDs(cons)}
	b.Consolidation.Head.Pill = cardPill(cons)
	if b.Consolidation.Note.Text == "" {
		pending, _ := extraAs[int64](cons.M, "pending")
		failed, _ := extraAs[int64](cons.M, "failed")
		built := textRow("Last built", "not yet")
		if at, ok := extraAs[time.Time](cons.M, "last_consolidated_at"); ok && !at.IsZero() {
			built = DLRow{Label: "Last built", Time: format.Rel(r.GeneratedAt, at, d.Loc)}
		}
		b.Consolidation.Rows = []DLRow{
			textRow("Waiting", format.Count(l, pending, "memory", "memories")),
			textRow("Failed", l.Int(failed)),
			built,
		}
		b.Consolidation.Info = "Building summaries groups facts into one memory. It runs on the server after each send."
		if failed > 0 {
			b.Consolidation.Hint, b.Consolidation.HintCmd = "Failed summaries need the server looked at:", "backsight doctor"
		}
	}

	b.Notes = Card{Head: d.head(notes), Note: d.noData(notes), Dev: d.devIDs(notes)}
	if b.Notes.Note.Text == "" {
		if ld, ok := d.lead(notes, "saved or updated"); ok {
			b.Notes.Leads = []Lead{ld}
		}
		byResult, _ := extraAs[map[string]int64](notes.M, "by_result")
		for _, k := range []string{"saved", "updated", "refused"} {
			b.Notes.Figs = append(b.Notes.Figs, Fig{Label: k, Value: l.Int(byResult[k])})
		}
		byKind, _ := extraAs[map[string]int64](notes.M, "by_kind")
		if line := countsLine("by kind: ", byKind, nil); line != "" {
			b.Notes.Extras = []string{line}
		}
	}

	b.Churn = Card{Head: d.head(churn), Note: d.noData(churn), Dev: d.devIDs(churn)}
	if b.Churn.Note.Text == "" {
		if ld, ok := d.lead(churn, "memories retired, hidden, restored or forgotten"); ok {
			b.Churn.Leads = []Lead{ld}
		}
		b.Churn.Charts = []PathChart{{HTML: partsOf(l, "Memories retired, hidden, restored or forgotten, per week", d.more(), churn, churnOrder,
			wordsOf(churnOrder, func(k string) string { return wordOr(views.RetireWords, k, k) }), []string{"f-seq1", "f-seq2", "f-seq3", "f-neutral"}, "")}}
	}

	b.Review = d.reviewOf(r)
	return b
}

// bankStrip is the row of figures at the top: the memories, each kind with
// what was added to it, the documents. Without a number for the total, one
// figure that says why.
func (d PageData) bankStrip(r *metrics.Result, size, growth MetricView) []Fig {
	l := d.L
	total := Fig{Label: views.Label(size.Def), Icon: "server", Value: dash}
	if d.Frame.Prefs.Dev {
		total.Dev = size.ID
	}
	if !hasValue(size) {
		total.Sub = sentenceOf(d.noData(size))
		return []Fig{total}
	}
	total.Value = l.Value(size.Def, size.M)
	added := map[string]int64{}
	var sum int64
	if hasValue(growth) {
		for _, k := range glanceKinds {
			added[k.key], _ = extraAs[int64](growth.M, k.key)
			sum += added[k.key]
		}
		total.Sub = "+" + l.Int(sum) + " " + growthWindow(r, growth.M, d.P)
	}
	out := []Fig{total}
	for _, k := range glanceKinds {
		n, ok := extraAs[int64](size.M, k.key)
		if !ok {
			continue
		}
		f := Fig{Label: k.one, Swatch: k.class, Value: l.Int(n)}
		if hasValue(growth) {
			f.Sub = "+" + l.Int(added[k.key])
		}
		out = append(out, f)
	}
	if docs, ok := extraAs[int64](size.M, "documents"); ok {
		out = append(out, Fig{Label: "Documents", Icon: "doc", Value: l.Int(docs), Sub: "sessions and notes sent"})
	}
	return out
}

// countsLine is "prefix a 3, b 2": the keys by count, most first, a tie by
// the stored name; words names a key (nil: as stored). "" without a count.
func countsLine(prefix string, counts map[string]int64, words map[string]string) string {
	keys := make([]string, 0, len(counts))
	for k, n := range counts {
		if n > 0 {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		if counts[a] != counts[b] {
			return int(counts[b] - counts[a])
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		name := k
		if words != nil {
			name = wordOr(words, k, k)
		}
		parts[i] = name + " " + views.Grouped(counts[k])
	}
	if len(parts) == 0 {
		return ""
	}
	return prefix + strings.Join(parts, ", ")
}

// reviewOf is the review queue: the memories shown often and never used,
// hidden, reported wrong. The lists are this PC's whatever the project.
func (d PageData) reviewOf(r *metrics.Result) ReviewCard {
	h := withInfo(fixedHead("review", "Which memories need a look?", "machine", "backsight memory stale"),
		"About the review queue", "Review queue", "stale.never_used · stale.hidden · stale.reported",
		"Memories shown often but never used, hidden, or reported wrong. Counts are recent-weighted counts: older shows weigh less. These never change the verdict.")
	c := ReviewCard{Card: Card{Head: h}}
	var vs []MetricView
	for i, t := range reviewTabs {
		v := d.view(r, t.id)
		vs = append(vs, v)
		c.Head.NotFiltered = c.Head.NotFiltered || slices.Contains(v.M.Tags, "not_filtered")
		tab := ReviewTab{Title: views.Label(v.Def), Question: v.Def.Question, Open: i == 0}
		var rows []metrics.ReviewRow
		if r.Tables.Review != nil {
			rows = t.rows(r.Tables.Review)
		}
		if hasValue(v) {
			tab.Count = d.L.Value(v.Def, v.M)
		}
		for _, row := range rows {
			line := ReviewLine{ID: row.ID, Project: row.Project, Shown: d.L.Int(row.Shown), Command: row.Command, Last: format.Time{Text: dash}}
			if row.LastShown != nil && !row.LastShown.IsZero() {
				line.Last = format.Rel(r.GeneratedAt, *row.LastShown, d.Loc)
			}
			tab.Rows = append(tab.Rows, line)
		}
		switch {
		case len(rows) > 0:
			if hasValue(v) && int(*v.M.Value+0.5) > len(rows) {
				tab.Foot = "Showing " + d.L.Int(int64(len(rows))) + " of " + d.L.Int(int64(*v.M.Value+0.5)) + " memories."
				tab.More = v.Def.Command
			}
		case hasValue(v):
			tab.Note = NoData{Text: t.empty}
		default:
			tab.Note = d.noData(v)
		}
		c.Tabs = append(c.Tabs, tab)
	}
	c.Dev = d.devIDs(vs...)
	return c
}
