package dashboard

import (
	"html"
	"html/template"
	"math"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// Lead is a number at the top of a card: the value, its unit and interval,
// and what it counts.
type Lead struct{ Big, Unit, CI, Label string }

// Card is a widget of the Quality and Cost pages. A card whose numbers are
// missing carries Note, the sentence that stands where they would be, and
// no chart.
type Card struct {
	Head    Head
	Leads   []Lead
	Word    Word // the status of the first number; "" none
	Note    NoData
	Charts  []PathChart // Name is "" for a chart that needs none
	Figs    []Fig
	Extras  []string // lines under the charts: what the numbers are made of
	Hint    string
	HintCmd string
	Foot    string   // a muted line under everything; "" none
	Dev     []string // the ids of the card's metrics, in developer mode
	// Lede is a muted line right under the head: what the card keeps this PC's
	// on all PCs; "" none. State is the same as a boxed sentence (escaped markup).
	Lede  string
	State template.HTML
}

// view is metric id of r framed for the page: the zone its times are shown
// in, and the developer's detail when the reader asked for it.
func (d PageData) view(r *metrics.Result, id string) MetricView {
	v, _ := metricOf(r, id) // the ids are the catalog's
	v.ShowID, v.Loc = d.Frame.Prefs.Dev, d.Loc
	return v
}

// hasValue is whether the metric shows a number.
func hasValue(v MetricView) bool { return v.Found && v.M.Value != nil }

// lead is the headline of metric v; false when it has no number.
func (d PageData) lead(v MetricView, label string) (Lead, bool) {
	if !hasValue(v) {
		return Lead{}, false
	}
	p := d.L.ValueParts(v.Def, v.M)
	return Lead{Big: p.Big, Unit: p.Unit, CI: p.CI, Label: label}, true
}

// frame is the page's frame with a window to name when it has none.
func (d PageData) frame() Frame {
	f := d.Frame
	if f.Window == "" {
		f.Window = "window"
	}
	return f
}

// noData is the sentence for a card whose metrics all lack a number: the
// first one's reason. Zero when one of them has a number.
func (d PageData) noData(first MetricView, rest ...MetricView) NoData {
	vs := append([]MetricView{first}, rest...)
	for _, v := range vs {
		if hasValue(v) {
			return NoData{}
		}
	}
	n, ok := noDataOf(vs[0], d.frame(), d.L, d.Loc)
	if !ok {
		return NoData{Text: "No data."}
	}
	return n
}

// reasonOnly is the sentence for a card that is empty because nothing
// happened, or because memory feedback is off or the server is out: what
// the metric says when it names such a cause. Zero when a metric with a
// number or a plain lack of events explains it (the card then says its own
// sentence).
func (d PageData) reasonOnly(v MetricView) NoData {
	if v.Found && v.M.Status == metrics.StatusNoData && v.M.Reason != metrics.ReasonNoEvents && v.M.Reason != "" {
		n, _ := noDataOf(v, d.frame(), d.L, d.Loc)
		return n
	}
	return NoData{}
}

// emptyWindow is the sentence of a card with nothing in its window; the
// metric is the one whose reason it would tell.
func (d PageData) emptyWindow(v MetricView) NoData {
	if n := d.reasonOnly(v); n.Text != "" {
		return n
	}
	v.Found, v.M.Status, v.M.Reason = true, metrics.StatusNoData, metrics.ReasonNoEvents
	n, _ := noDataOf(v, d.frame(), d.L, d.Loc)
	return n
}

// word is the status word of v, none when it is not found.
func word(v MetricView) Word {
	if !v.Found {
		return Word{}
	}
	w, c := statusWord(v.Def, v.M, false)
	return Word{w, c}
}

// devIDs are the ids a card names in developer mode; none otherwise.
func (d PageData) devIDs(vs ...MetricView) []string {
	if !d.Frame.Prefs.Dev {
		return nil
	}
	ids := make([]string, len(vs))
	for i, v := range vs {
		ids[i] = v.ID
	}
	return ids
}

// more is the command that prints a chart's numbers.
func (d PageData) more() string { return d.CLI + " --json" }

// extrasOf are the lines under the numbers of the given metrics: what they
// are made of (details) and their breakdowns.
func extrasOf(vs ...MetricView) []string {
	var out []string
	for _, v := range vs {
		out = append(out, detailsOf(v)...)
		out = append(out, foundLines(v, views.ExtraLines)...)
	}
	return out
}

// labelled is lines of metric v, each starting with the metric's label.
func labelled(v MetricView) []string {
	lines := detailsOf(v)
	for i, s := range lines {
		lines[i] = views.Label(v.Def) + ": " + s
	}
	return lines
}

// QualityPage is the Quality page: its cards in the order they are read.
type QualityPage struct {
	Used, Outcomes, Hit, Empty, Search Card
	Reported                           ReportedCard
	Ranking                            *Card       // developer mode
	Dev                                *QualityDev // developer mode
}

// ReportedCard is the table of memories reported wrong or out of date.
type ReportedCard struct {
	Card
	Rows []ReportedLine
}

// ReportedLine is a row of that table.
type ReportedLine struct {
	Memory, Session, Project, Verdict string
	When                              format.Time
}

// emptyOrder are the reasons a prompt got nothing, bottom of the bars first.
var emptyOrder = []string{"error", "no_candidates", "below_floor", "all_seen", "capped", "unknown"}

func qualityOf(d PageData) QualityPage {
	r := d.V["quality"]
	if r == nil {
		r = &metrics.Result{}
	}
	l, p := d.L, d.P
	used, cov := d.view(r, "used.turn_rate"), d.view(r, "declare.coverage")
	hit, empty := d.view(r, "recall.hit_rate"), d.view(r, "recall.empty_reasons")
	search, searchEmpty := d.view(r, "search.rate"), d.view(r, "search.empty_rate")
	wrong := d.view(r, "verdict.wrong_rate")
	var q QualityPage

	q.Used = Card{Head: headOf(used, d.CLI, l, "", p), Word: word(used), Note: d.noData(used, cov), Dev: d.devIDs(used, cov)}
	if q.Used.Note.Text == "" {
		q.Used.Extras = extrasOf(used)
		q.Used.Leads = leadsOf(d, [2]MetricView{used, cov}, [2]string{"used by Claude", "said what it used"})
		q.Used.Charts = []PathChart{{HTML: linesOf(l, "Used by Claude, and how often it said what it used, per day", d.more(), true, used, cov)}}
	}

	q.Outcomes = d.outcomesCard(r, used)

	// Reported wrong or out of date.
	rp := p
	rp.Has = "wrong"
	head := fixedHead("reported", "Which memories were reported wrong or out of date?", "machine", cliFor("sessions", "", rp))
	head.Drill, head.DrillText = drillHref(wrong.M.Drill, p), "See the events →"
	q.Reported = ReportedCard{Card: Card{Head: head, Word: word(wrong), Dev: d.devIDs(wrong)}}
	if ld, ok := d.lead(wrong, "reported wrong or out of date"); ok {
		q.Reported.Leads = []Lead{ld}
	}
	for _, row := range r.Tables.Reported {
		q.Reported.Rows = append(q.Reported.Rows, ReportedLine{Memory: row.MemoryID, Session: row.Session, Project: row.Project,
			Verdict: views.Word(views.VerdictWords, row.Verdict), When: format.Rel(r.GeneratedAt, row.At, d.Loc)})
	}
	if len(q.Reported.Rows) == 0 {
		q.Reported.Note = d.reasonOnly(wrong)
		if q.Reported.Note.Text == "" {
			q.Reported.Note = NoData{Text: "Nothing was reported wrong or out of date in the " + d.frame().Window + "."}
		}
	}

	q.Hit = Card{Head: headOf(hit, d.CLI, l, "", p), Word: word(hit), Note: d.noData(hit), Dev: d.devIDs(hit)}
	if q.Hit.Note.Text == "" {
		q.Hit.Extras = extrasOf(hit)
		q.Hit.Leads = leadsOf(d, [2]MetricView{hit, {}}, [2]string{"of prompts got memory", ""})
		q.Hit.Charts = []PathChart{{HTML: linesOf(l, "Prompts that got memory, per day", d.more(), true, hit)}}
	}

	q.Empty = Card{Head: headOf(empty, d.CLI, l, "", p), Note: d.noData(empty), Dev: d.devIDs(empty)}
	if q.Empty.Note.Text == "" {
		words := wordsOf(emptyOrder, views.EmptyWords.Replace)
		q.Empty.Charts = []PathChart{{HTML: partsOf(l, "Prompts that got nothing, by why, per day", d.more(), empty, emptyOrder, words,
			[]string{"f-failed", "f-seq1", "f-seq2", "f-seq3", "f-aside", "f-neutral"}, "")}}
		q.Empty.Extras = extrasOf(empty)
		switch topEmpty(empty) {
		case "error":
			q.Empty.Hint = "Most prompts got nothing because the memory could not be searched, not because it is empty. Check the server:"
			q.Empty.HintCmd = "backsight doctor"
		case "no_candidates":
			q.Empty.Hint = "Most prompts get nothing because nothing close enough is in memory."
			if p.Project != "" {
				q.Empty.Hint += " Importing a project's past sessions helps:"
				q.Empty.HintCmd = "backsight retain backfill --project " + quoteArg(p.Project) + " --plan"
			}
		}
	}

	q.Search = Card{Head: headOf(search, d.CLI, l, "", p), Word: word(search), Note: d.noData(search, searchEmpty), Dev: d.devIDs(search, searchEmpty)}
	if q.Search.Note.Text == "" {
		q.Search.Leads = leadsOf(d, [2]MetricView{search, searchEmpty}, [2]string{"of prompts", "found nothing"})
		q.Search.Charts = []PathChart{{HTML: linesOf(l, "Prompts where Claude searched memory, and searches that found nothing, per day", d.more(), true, search, searchEmpty)}}
	}

	if d.Frame.Prefs.Dev {
		q.Ranking = d.rankingCard(r)
		q.Dev = d.qualityDevOf(r)
	}
	return q
}

// leadsOf are the headlines of the metrics that have a number.
func leadsOf(d PageData, vs [2]MetricView, labels [2]string) []Lead {
	var out []Lead
	for i, v := range vs {
		if v.ID == "" {
			continue
		}
		if ld, ok := d.lead(v, labels[i]); ok {
			out = append(out, ld)
		}
	}
	return out
}

// topEmpty is the reason most prompts got nothing; "" when none did.
func topEmpty(v MetricView) string {
	counts, _ := extraAs[map[string]int64](v.M, "empty")
	best, top := "", int64(0)
	for _, k := range emptyOrder {
		if counts[k] > top {
			best, top = k, counts[k]
		}
	}
	return best
}

// outcomesCard is what became of the memories shown, per day.
func (d PageData) outcomesCard(r *metrics.Result, used MetricView) Card {
	term := ""
	if d.Frame.Prefs.Dev {
		term = "used.share_confident"
	}
	h := withInfo(fixedHead("outcomes", "What did Claude do with the memories shown?", "machine", d.CLI),
		"About this chart", "Where memory went, per day", term,
		"Each memory shown, by what Claude said about it at the end of the turn. Pending: the turn has not ended yet. Memories forgotten since are left out.")
	c := Card{Head: h}
	var total int64
	for _, row := range r.Tables.OutcomeMix {
		total += row.Used + row.Ignored + row.NotDeclared + row.ShownOnly + row.Pending
	}
	if total == 0 {
		c.Note = d.emptyWindow(used)
		return c
	}
	rows := r.Tables.OutcomeMix
	c.Foot = format.Count(d.L, total, "memory", "memories") + " shown"
	c.Charts = []PathChart{{HTML: outcomesOf(d.L, d.more(), rows)}}
	if paths := outcomePaths(d.L, d.more(), rows); d.Frame.Prefs.Dev && len(paths) > 0 {
		c.Charts[0].Name = "All paths"
		c.Charts = append(c.Charts, paths...)
	}
	return c
}

// rankingCard is the developer's view of how memory is ranked.
func (d PageData) rankingCard(r *metrics.Result) *Card {
	h := fixedHead("ranking", "How well is memory ranked?", "machine", d.CLI)
	h.Chip = "developer"
	c := Card{Head: h}
	var vs []MetricView
	for _, id := range []string{"used.share_confident", "used.rank", "used.by_path", "search.rescue_rate", "used.rubber_stamp", "recall.by_lang"} {
		vs = append(vs, d.view(r, id))
	}
	c.Dev = d.devIDs(vs...)
	// Without a number among them, the card says why (feedback off, ...) and draws nothing.
	if c.Note = d.noData(vs[0], vs[1:]...); c.Note.Text != "" {
		return &c
	}
	for _, v := range vs {
		c.Figs = append(c.Figs, figOf(v, d.L))
	}
	if rank := d.view(r, "used.rank"); hasValue(rank) {
		c.Charts = []PathChart{{HTML: rankHistOf(d.L, d.more(), rank)}}
	}
	c.Extras = extrasOf(slices.DeleteFunc(slices.Clone(vs), func(v MetricView) bool {
		return v.ID != "used.rank" && v.ID != "used.by_path" && v.ID != "recall.by_lang"
	})...)
	return &c
}

// CostPage is the Cost page: the strip of key figures, then the cards.
type CostPage struct {
	Strip                   []Fig
	Context, Split, Latency Card
	Extraction              ExtractionCard
	Yield                   YieldCard
	PerSession              *Card // developer mode
}

// ExtractionCard is the server's model spend: the tokens by day and the
// table of what each task cost.
type ExtractionCard struct {
	Card
	Rows  []ExtractionLine
	Total ExtractionLine
	More  string // the command listing every task, under a capped table
	Shown int    // rows shown of Count
	Count int
}

// ExtractionLine is a row of that table.
type ExtractionLine struct{ Task, Input, Cached, Output, Cost string }

// taskSum is a task's tokens and cost over the window.
type taskSum struct {
	op                    string
	input, cached, output int64
	cost                  float64
	priced                bool
}

// foldTasks adds the day-by-task rows up per task, in the order the tasks
// are first seen.
func foldTasks(rows []metrics.ExtractionRow) []taskSum {
	var out []taskSum
	idx := map[string]int{}
	for _, r := range rows {
		i, ok := idx[r.Operation]
		if !ok {
			i = len(out)
			idx[r.Operation] = i
			out = append(out, taskSum{op: r.Operation})
		}
		t := &out[i]
		t.input, t.cached, t.output = t.input+r.Input, t.cached+r.Cached, t.output+r.Output
		if r.Cost != nil {
			t.cost, t.priced = t.cost+*r.Cost, true
		}
	}
	return out
}

// partOrder are the parts of a session's context, bottom of the bars first.
var (
	splitOrder = []string{"briefing", "prompts"}
	splitWords = map[string]string{"briefing": "at session start", "prompts": "before prompts"}
)

func costOf(d PageData) CostPage {
	r := d.V["cost"]
	if r == nil {
		r = &metrics.Result{}
	}
	l, p := d.L, d.P
	perSession, perPrompt := d.view(r, "context.per_session"), d.view(r, "context.per_prompt")
	rl, sl := d.view(r, "recall.latency"), d.view(r, "search.latency")
	tokens, price := d.view(r, "extraction.tokens"), d.view(r, "extraction.cost")
	var c CostPage
	c.Strip = d.strip(r, perSession, perPrompt, rl, sl, price)
	all := r.Scope.Machines == "all" && r.Scope.PCs != nil // the page adds up the PCs

	c.Context = Card{Head: headOf(perSession, d.CLI, l, "", p), Word: word(perSession), Note: d.noData(perSession), Dev: d.devIDs(perSession)}
	average := mergedTile(r, perSession.ID) && slices.Contains(perSession.M.Tags, "average")
	if all {
		// The chart and its numbers are this PC's: other PCs send a total and a
		// count, not sizes. The command is the page's, with no flag.
		c.Context.Head.CLI = d.CLI
		c.Context.State = d.contextState(perSession, average)
		if d.Own != nil {
			if om, ok := d.Own.Metrics[perSession.ID]; ok && om.Den != nil {
				c.Context.Head.Info.BasedOn = basedOn(perSession.Def, om, l)
			}
		}
	}
	if c.Context.Note.Text == "" {
		lv := perSession
		if p95, ok := extraAs[float64](perSession.M, "p95"); ok && average {
			lv.M.Value = &p95 // the card's number is this PC's high value, not the average
			lv.M.CI = nil
		}
		if ld, ok := d.lead(lv, "high value: 1 session in 20 gets more"); ok {
			if tk, ok := extraAs[int64](perSession.M, "tokens_p95"); ok {
				ld.CI = "≈ " + l.Int(tk) + " tokens"
			}
			c.Context.Leads = []Lead{ld}
		}
		c.Context.Extras = extrasOf(perSession)
		c.Context.Charts = []PathChart{{HTML: contextHistOf(l, d.more(), perSession)}}
	}

	c.Split = Card{Head: fixedHead("context-split", "Where is that context added?", "machine", d.CLI), Note: d.noData(perSession), Dev: d.devIDs(perSession)}
	if all {
		c.Split.Lede = "This PC’s only, with all PCs too: other PCs do not send where context was added."
	}
	if c.Split.Note.Text == "" {
		c.Split.Charts = []PathChart{{HTML: partsOf(l, "Characters added, per day, at session start and before prompts", d.more(), perSession,
			splitOrder, splitWords, []string{"f-seq1", "f-seq2"}, "")}}
	}

	c.Latency = Card{Head: headOf(rl, d.CLI, l, "", p), Word: word(rl), Note: d.noData(rl, sl), Dev: d.devIDs(rl, sl)}
	if mergedTile(r, rl.ID) {
		d.mergedLatency(&c.Latency, r, rl, sl)
	} else if c.Latency.Note.Text == "" {
		for _, v := range []MetricView{rl, sl} {
			if ld, ok := d.lead(v, "slowest 1 in 20: "+views.Label(v.Def)); ok {
				c.Latency.Leads = append(c.Latency.Leads, ld)
			}
		}
		c.Latency.Extras = slices.Concat(labelled(rl), labelled(sl))
		c.Latency.Charts = []PathChart{{HTML: latencyOf(l, d.more(), rl, sl)}}
		c.Latency.Hint = "Too slow? This checks the server and its search index:"
		c.Latency.HintCmd = "backsight doctor"
	}

	c.Extraction.Card = Card{Head: headOf(tokens, d.CLI, l, "", p), Word: word(tokens), Note: d.noData(tokens), Dev: d.devIDs(tokens, price)}
	if all {
		c.Extraction.Lede = "The server’s own figures, already every PC’s: the same with this PC or all PCs."
	}
	if c.Extraction.Note.Text == "" {
		if ld, ok := d.lead(tokens, "tokens"); ok {
			c.Extraction.Leads = []Lead{ld}
		}
		c.Extraction.Extras = extrasOf(tokens, price)
		from, _ := extraAs[string](tokens.M, "record_from")
		c.Extraction.Charts = []PathChart{{HTML: extractionChart(l, d.more(), r.Tables.Extraction, tokens.M.Series) +
			recordBlock(tokens.M.Series, from, false)}}
		cur, _ := price.M.Extra["currency"].(string)
		money := func(v float64, ok bool) string {
			if !ok {
				return dash
			}
			return l.Money(v, cur)
		}
		tasks := foldTasks(r.Tables.Extraction)
		var all taskSum
		for _, t := range tasks {
			all.input, all.cached, all.output = all.input+t.input, all.cached+t.cached, all.output+t.output
			all.cost, all.priced = all.cost+t.cost, all.priced || t.priced
		}
		if price.M.Value != nil { // the price of the whole window, as the tokens card's figure says it
			all.cost, all.priced = *price.M.Value, true
		}
		line := func(name string, t taskSum) ExtractionLine {
			return ExtractionLine{Task: name, Input: l.Int(t.input), Cached: l.Int(t.cached), Output: l.Int(t.output), Cost: money(t.cost, t.priced)}
		}
		c.Extraction.Count = len(tasks)
		for i, t := range tasks {
			if i == chart.MaxTableRows {
				break
			}
			c.Extraction.Rows = append(c.Extraction.Rows, line(views.Word(views.OperationWords, t.op), t))
		}
		c.Extraction.Shown = len(c.Extraction.Rows)
		c.Extraction.More = d.more()
		if len(tasks) > 0 {
			c.Extraction.Total = line("all tasks", all)
		}
	}

	c.Yield = d.yieldOf(r)

	if d.Frame.Prefs.Dev {
		v := d.view(r, "extraction.per_session")
		pc := Card{Head: headOf(v, d.CLI, l, "", p), Note: d.noData(v)}
		if pc.Note.Text == "" {
			pc.Figs = []Fig{figOf(v, l)}
		}
		c.PerSession = &pc
	}
	return c
}

// strip is the row of key figures at the top of the Cost page. On all PCs
// each figure carries the icon of whose it is, and the merged ones say how.
func (d PageData) strip(r *metrics.Result, perSession, perPrompt, rl, sl, price MetricView) []Fig {
	l := d.L
	all := r.Scope.Machines == "all" && r.Scope.PCs != nil
	var out []Fig
	for _, v := range []MetricView{perSession, perPrompt, rl, sl, price} {
		f := figOf(v, l)
		p50, ok := extraAs[float64](v.M, "p50")
		switch {
		case v.ID == "extraction.cost":
			f.Sub = "server, all PCs · " + d.frame().Window
		case hasValue(v) && ok && v.Def.Unit == "ms":
			f.Sub = "slowest 1 in 20 · typical " + l.Duration(p50)
		case hasValue(v) && ok:
			f.Sub = "high · typical " + l.Size(p50, "chars")
			if tk, ok := extraAs[int64](v.M, "tokens_p95"); ok {
				f.Sub += " · ≈ " + l.Int(tk) + " tokens"
			}
		}
		if all {
			d.allPCsFig(&f, r, v, ok)
		}
		if !hasValue(v) { // the reason there is no number
			n := d.noData(v)
			f.Sub = strings.TrimSpace(n.Text + n.Code + n.Close)
		}
		out = append(out, f)
	}
	return out
}

// allPCsFig gives a figure of the strip, on all PCs, its icon and the words
// that say whose number it is: the merged speed is a range from the PCs'
// buckets, the merged context an average beside this PC's high value, and the
// others stay this PC's (the server's cost is every PC's).
func (d PageData) allPCsFig(f *Fig, r *metrics.Result, v MetricView, hasP50 bool) {
	l := d.L
	f.Icon = "pc"
	switch {
	case v.ID == "extraction.cost":
		f.Icon = "server"
	case mergedTile(r, v.ID):
		f.Icon = "pcs"
	}
	if !hasValue(v) {
		return
	}
	switch {
	case v.ID == "context.per_session" && slices.Contains(v.M.Tags, "average"):
		f.Delta = nil // the change of an average of totals is not this PC's high value's
		f.Sub = "average, all PCs"
		if p95, ok := extraAs[float64](v.M, "p95"); ok {
			f.Sub += " · this PC’s high value " + l.Size(p95, "")
		}
	case v.ID == "recall.latency" && slices.Contains(v.M.Tags, "approximate"):
		f.Delta = bucketDelta(r, v.M)
		if lo, hi, ok := views.RangeMS(v.M, "range_ms"); ok {
			f.Value = views.RangeText(lo, hi)
		}
		f.Sub, f.SubApprox = "slowest 1 in 20, all PCs", true
	case v.ID == "context.per_prompt" && hasP50:
		f.Sub += " · this PC"
	case v.ID == "search.latency" && hasP50:
		f.Sub = "slowest 1 in 20 · this PC"
	}
}

// contextState is the boxed sentence of the Context card on all PCs: the
// chart is this PC's, and the all PCs' average goes with it.
func (d PageData) contextState(v MetricView, average bool) template.HTML {
	text := html.EscapeString("This chart is this PC’s: other PCs send a total and a count, not sizes.")
	if average && hasValue(v) {
		text += " All PCs: <b>" + html.EscapeString(d.L.Int(int64(math.Round(*v.M.Value)))) + "</b> characters per session on average"
		if v.M.Den != nil {
			text += ", over " + html.EscapeString(format.Count(d.L, *v.M.Den, "session", "sessions"))
		}
		text += "."
	}
	return template.HTML(`<div class="state st-few"><span class="i i-pcs"></span><span>` + text + `</span></div>`)
}

// DLRow is a row of a card's description list: the text around a moment,
// the moment itself when it has one, and the class that colours the row.
type DLRow struct {
	Label, Pre, Post, Class string
	ID                      string // a short machine id, shown before Pre; "" none
	Time                    format.Time
	Mono, Num               bool
}

// textRow is a row of plain text.
func textRow(label, text string) DLRow { return DLRow{Label: label, Pre: text} }

// wordOr is the word for k, else fallback: what a log or the server wrote
// that the dashboard has no word for is never printed as it came.
func wordOr(words map[string]string, k, fallback string) string {
	if w, ok := words[k]; ok {
		return w
	}
	return fallback
}

// head is the frame of the card showing metric v.
func (d PageData) head(v MetricView) Head { return headOf(v, d.CLI, d.L, "", d.P) }

// cardPill is the status of v as a card's pill; none without a number.
func cardPill(v MetricView) Word {
	if !hasValue(v) {
		return Word{}
	}
	return word(v)
}

// sentenceOf is a no-data sentence as one line of text, for a figure.
func sentenceOf(n NoData) string { return strings.TrimSpace(n.Text + n.Code + n.Close) }

// latencyWhat is the ⓘ's account of the merged speed.
const latencyWhat = "Per day, the slowest 1 in 20 prompts across the PCs. Each PC sends how many prompts fell in each time bucket (50, 100, 250, 500 ms, 1, 2, 2.5, 5 s, more); added up, they place the slowest 1 in 20 between two bucket edges, drawn as a bar from one edge to the other."

// mergedLatency draws the Latency card of the Cost page on all PCs: the chip,
// the ⓘ, the days as the bucket that holds the slowest 1 in 20 with this PC's
// exact value as a ring, the PCs one by one, and the link to this PC's events.
func (d PageData) mergedLatency(c *Card, r *metrics.Result, rl, sl MetricView) {
	l := d.L
	h := &c.Head
	h.PCs = allPCsChip(r.Scope)
	h.OthersNF = othersNotFiltered(rl.M)
	if h.Drill != "" {
		h.DrillText = "This PC’s events →"
		for _, s := range tileSpecs {
			if s.id == rl.ID {
				h.DrillText = thisPCLink(d.Own, s, l)
			}
		}
		h.FootNote = "Other PCs send counts per bucket, not prompts: their events stay on them."
	}
	if c.Note.Text != "" {
		return
	}
	limits := ""
	if def, ok := metrics.DefOf(rl.ID); ok && def.Absolute.Watch != nil && def.Absolute.Bad != nil {
		limits = "Watch over " + l.Duration(*def.Absolute.Watch) + ", attention over " + l.Duration(*def.Absolute.Bad) +
			". Both are bucket edges, so the status is exact."
	}
	based := dash
	if rl.M.Den != nil {
		based = format.Count(l, *rl.M.Den, "prompt", "prompts")
		if pcs := r.Scope.PCs; pcs != nil && pcs.Active > 0 {
			based += " on " + plural(pcs.Active, "PC", "PCs")
		}
	}
	h.Info = Info{Label: "About this chart", Name: "Time memory adds to a prompt, all PCs", What: latencyWhat, Limits: limits, BasedOn: based,
		Term: rl.ID + " p95 from merged buckets"}
	byPC := byPCOf(l, r.Tables.ByPC)
	if bars, ok := latencyRangesOf(l, d.more(), rl, byPC); ok {
		c.Charts = []PathChart{{HTML: bars}}
		return
	}
	// A window drawn by the hour keeps this PC's series: lines, and the PCs below.
	h.Note = "Drawn by the hour from this PC’s figures: the other PCs send whole days."
	c.Charts = []PathChart{{HTML: latencyOf(l, d.more(), rl, sl) + template.HTML(byPC)}}
}
