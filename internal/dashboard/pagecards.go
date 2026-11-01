package dashboard

import (
	"slices"

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
	Dev     []string // the ids of the card's metrics, in developer mode
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
func (d PageData) noData(vs ...MetricView) NoData {
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
	Ranking                            *Card // developer mode
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
var emptyOrder = []string{"no_candidates", "below_floor", "all_seen", "capped", "unknown"}

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
			[]string{"f-seq1", "f-seq2", "f-seq3", "f-aside", "f-neutral"}, "")}}
		q.Empty.Extras = extrasOf(empty)
		if topEmpty(empty) == "no_candidates" {
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
	h := withInfo(fixedHead("outcomes", "What did Claude do with the memories shown?", "machine", d.CLI),
		"About this chart", "Where memory went, per day", "used.share_confident",
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
		v := d.view(r, id)
		vs = append(vs, v)
		c.Figs = append(c.Figs, figOf(v, d.L))
	}
	c.Charts = []PathChart{{HTML: rankHistOf(d.L, d.more(), d.view(r, "used.rank"))}}
	c.Extras = extrasOf(slices.DeleteFunc(slices.Clone(vs), func(v MetricView) bool {
		return v.ID != "used.rank" && v.ID != "used.by_path" && v.ID != "recall.by_lang"
	})...)
	return &c
}

// CostPage is the Cost page.
type CostPage struct {
	Context, Split, PerPrompt, Latency Card
	Extraction                         ExtractionCard
	PerSession                         *Card // developer mode
}

// ExtractionCard is the server's model spend: the tokens by day and the
// table behind them.
type ExtractionCard struct {
	Card
	Rows []ExtractionLine
}

// ExtractionLine is a row of that table.
type ExtractionLine struct{ Day, Task, Input, Cached, Output, Cost string }

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

	c.Context = Card{Head: headOf(perSession, d.CLI, l, "", p), Word: word(perSession), Note: d.noData(perSession), Dev: d.devIDs(perSession)}
	if c.Context.Note.Text == "" {
		if ld, ok := d.lead(perSession, "high value: 1 session in 20 gets more"); ok {
			if tk, ok := extraAs[int64](perSession.M, "tokens_p95"); ok {
				ld.CI = "≈ " + l.Int(tk) + " tokens"
			}
			c.Context.Leads = []Lead{ld}
		}
		c.Context.Extras = extrasOf(perSession)
		c.Context.Charts = []PathChart{{HTML: contextHistOf(l, d.more(), perSession)}}
	}

	c.Split = Card{Head: fixedHead("context-split", "Where is that context added?", "machine", d.CLI), Note: d.noData(perSession), Dev: d.devIDs(perSession)}
	if c.Split.Note.Text == "" {
		c.Split.Charts = []PathChart{{HTML: partsOf(l, "Characters added, per day, at session start and before prompts", d.more(), perSession,
			splitOrder, splitWords, []string{"f-seq1", "f-seq2"}, "")}}
	}

	c.PerPrompt = Card{Head: headOf(perPrompt, d.CLI, l, "", p), Word: word(perPrompt), Note: d.noData(perPrompt)}
	if c.PerPrompt.Note.Text == "" {
		f := figOf(perPrompt, l)
		if p50, ok := extraAs[float64](perPrompt.M, "p50"); ok {
			f.Sub = "high · typical " + l.Size(p50, "chars")
		}
		c.PerPrompt.Figs = []Fig{f}
	}

	c.Latency = Card{Head: headOf(rl, d.CLI, l, "", p), Word: word(rl), Note: d.noData(rl, sl), Dev: d.devIDs(rl, sl)}
	if c.Latency.Note.Text == "" {
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
	if c.Extraction.Note.Text == "" {
		if ld, ok := d.lead(tokens, "tokens"); ok {
			c.Extraction.Leads = []Lead{ld}
		}
		c.Extraction.Extras = extrasOf(tokens, price)
		c.Extraction.Charts = []PathChart{{HTML: extractionOf(l, d.more(), r.Tables.Extraction)}}
		c.Extraction.Figs = []Fig{figOf(price, l)}
		cur, _ := price.M.Extra["currency"].(string)
		for _, row := range r.Tables.Extraction {
			cost := dash
			if row.Cost != nil {
				cost = l.Money(*row.Cost, cur)
			}
			c.Extraction.Rows = append(c.Extraction.Rows, ExtractionLine{Day: dayCell(row.Day), Task: views.Word(views.OperationWords, row.Operation),
				Input: l.Int(row.Input), Cached: l.Int(row.Cached), Output: l.Int(row.Output), Cost: cost})
		}
	}

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

// dayCell is a bucket key as a table shows it: the day, or the day and hour.
func dayCell(key string) string {
	if len(key) > len(dayKey) && key[len(dayKey)] == 'T' {
		return key[:len(dayKey)] + " " + xName(key)
	}
	return key
}
