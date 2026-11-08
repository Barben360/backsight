package dashboard

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// The widgets of block 6 on Cost, Bank, Quality and the Memory page: the
// yield of new memories, where a memory came from, and the developer cards.

// BoostReplayCommand is what the learned-ranking card offers to try another
// weight: a key `backsight feedback replay --set` accepts.
const BoostReplayCommand = "backsight feedback replay --set boost_weight=0.15"

const (
	// allByShows lists every memory, the most shown first.
	allByShows = "/memories?sort=shown"
)

// pts is a share as whole points.
func pts(x float64) int { return int(math.Round(x * 100)) }

// dayLabel is the date of day ("2006-01-02") as the pages write it, with no
// zone: it names a calendar day, not an instant.
func dayLabel(day string) string {
	t, err := time.Parse(dayKey, day)
	if err != nil {
		return day
	}
	return t.Format("Jan 2")
}

// isAre is the verb of a count.
func isAre(n int64) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// yieldNoteReasons are the reasons that have a sentence of their own beside
// the weeks the card still shows.
var yieldNoteReasons = map[string]bool{
	metrics.ReasonRecordCatchingUp: true, metrics.ReasonNoRecord: true, metrics.ReasonNoCompleteWeek: true,
}

// YieldCard is whether the memories made each week turned out useful: the
// headline of the latest complete week, a chart of the weeks and their table.
type YieldCard struct {
	Card
	Rows []YieldRow
	Min  string // the memories a week needs before it shows a share
}

// YieldRow is a week of the table: the memories made, how many were used,
// the share and the tokens per memory used; a week with nothing to count
// says why in Note instead.
type YieldRow struct {
	Week, New, Used, Share, ShareClass, Tokens, Note string
}

func (d PageData) yieldOf(r *metrics.Result) YieldCard {
	v := d.view(r, "memory.yield")
	l := d.L
	h := headOf(v, d.CLI, l, "", d.P)
	h.Scope = "both" // memories come from the server, their uses from this PC
	c := YieldCard{Card: Card{Head: h, Dev: d.devIDs(v)}}
	if !v.Found || len(v.M.Series) == 0 {
		c.Note = d.noData(v)
		return c
	}
	withheld, _ := extraAs[string](v.M, "withheld")
	if v.M.Status == metrics.StatusNoData && yieldNoteReasons[v.M.Reason] {
		s := views.NoDataWords[v.M.Reason]
		c.Note = NoData{Text: strings.ToUpper(s[:1]) + s[1:] + "."}
	} else {
		c.Note = d.noData(v)
	}
	c.Min = l.Int(int64(v.Def.MinN))

	var latest *metrics.Point
	for i := range v.M.Series {
		if p := &v.M.Series[i]; p.Complete != nil && *p.Complete {
			latest = p
		}
	}
	if latest != nil && hasValue(v) && v.M.Num != nil && v.M.Den != nil {
		c.Leads = []Lead{{Big: l.Percent(*v.M.Value), Label: fmt.Sprintf(
			"of the memories made the week of %s, the latest complete week, were used within 28 days (%s of %s)",
			dayLabel(latest.Week), l.Int(*v.M.Num), l.Int(*v.M.Den))}}
		if tk, ok := extraAs[float64](v.M, "tokens_per_used"); ok {
			c.Leads = append(c.Leads, Lead{Big: l.Size(tk, ""), Label: "tokens per memory used, that week"})
		}
	}
	if u, _ := extraAs[int64](v.M, "unresolved"); u > 0 && withheld == "" {
		c.Extras = []string{format.Count(l, u, "memory", "memories") + " used " + isAre(u) +
			" not in the record of new memories yet, so the shares may read low."}
	}

	weeks := make([]chart.CohortWeek, 0, len(v.M.Series))
	notOver := 0
	for _, p := range v.M.Series {
		weeks = append(weeks, chart.CohortWeek{Label: dayLabel(p.Week), Num: countOf(p.Num), Den: countOf(p.Den), Share: p.Value,
			Complete: p.Complete != nil && *p.Complete, NotRecorded: p.Complete != nil && *p.Complete && p.Den == nil, Title: d.yieldTitle(p)})
		if p.Complete == nil || !*p.Complete {
			notOver++
		}
	}
	if withheld == "" { // a share withheld is not drawn as "too few"
		label := fmt.Sprintf("Share of each week's new memories used within 28 days, last %d weeks", len(weeks))
		switch notOver {
		case 0:
		case 1:
			label += "; the latest week is not complete yet"
		default:
			label += fmt.Sprintf("; the %d latest are not complete yet", notOver)
		}
		c.Charts = []PathChart{{HTML: chart.Cohort{Label: label, Weeks: weeks, More: d.more()}.HTML()}}
	}
	for i := len(v.M.Series) - 1; i >= 0; i-- {
		c.Rows = append(c.Rows, d.yieldRow(v, v.M.Series[i]))
	}
	return c
}

// yieldTitle is the sentence of a week's hover and focus text.
func (d PageData) yieldTitle(p metrics.Point) string {
	l := d.L
	week := "Week of " + dayLabel(p.Week) + ": "
	switch {
	case p.Complete == nil || !*p.Complete:
		return week + "not complete yet"
	case p.Den == nil:
		return week + views.NoDataWords[metrics.ReasonNoRecord]
	case *p.Den == 0:
		return week + "no memory made"
	case p.Value == nil:
		return week + format.Count(l, *p.Den, "new memory", "new memories") + ", too few for a share (" + l.Int(countOf(p.Num)) + " used)"
	}
	s := week + l.Percent(*p.Value) + " used within 28 days (" + l.Int(countOf(p.Num)) + " of " + l.Int(*p.Den) + ")"
	if p.TokensPerUsed != nil {
		s += " · " + l.Size(*p.TokensPerUsed, "") + " tokens per memory used"
	}
	return s
}

// completeUntil is the last day of the week starting on monday and the day
// its 28 days after that are over, both as "Jan 2".
func completeUntil(monday string) (last, until string) {
	t, err := time.Parse(dayKey, monday)
	if err != nil {
		return "", ""
	}
	end := t.AddDate(0, 0, 6)
	return end.Format("Jan 2"), end.AddDate(0, 0, 28).Format("Jan 2")
}

func (d PageData) yieldRow(v MetricView, p metrics.Point) YieldRow {
	l := d.L
	row := YieldRow{Week: dayLabel(p.Week), New: dash, Used: dash, Share: dash, Tokens: dash}
	switch {
	case p.Complete == nil || !*p.Complete:
		last, until := completeUntil(p.Week)
		row.Note = "not complete until " + until + " (its last day, " + last + ", plus 28 days)"
	case p.Den == nil:
		row.Note = views.NoDataWords[metrics.ReasonNoRecord]
	default:
		row.New = l.Int(*p.Den)
		if *p.Den > 0 {
			row.Used = l.Int(countOf(p.Num))
		}
		switch {
		case p.Value != nil:
			row.Share = l.Percent(*p.Value)
		case *p.Den > 0 && *p.Den < int64(v.Def.MinN):
			row.Share, row.ShareClass = "too few", "st-few"
		}
		if p.TokensPerUsed != nil {
			row.Tokens = l.Size(*p.TokensPerUsed, "")
		}
	}
	return row
}

// OriginCard is where a memory came from: the session it was made in and
// what became of it, or why there is none.
type OriginCard struct {
	Head Head
	Rows []OriginRow // a session of this PC
	Text string      // the one sentence of the other states
	Note string
}

// OriginRow is a row of the origin's list: a session and its project, or a
// moment between words.
type OriginRow struct {
	Label     string
	Session   *Ref
	Project   *Ref
	Pre, Post string
	Time      format.Time
}

// originTime is a moment the way the origin list shows it, in the reader's zone.
func (d PageData) originTime(t *time.Time) format.Time {
	at := t.In(zoneOr(d.Loc))
	return format.Time{Text: at.Format("Jan 2"), Datetime: at.Format(time.RFC3339), Title: at.Format("2006-01-02 15:04")}
}

// gapMS is the milliseconds from a to b, nothing negative.
func gapMS(a, b time.Time) float64 { return math.Max(0, float64(b.Sub(a).Milliseconds())) }

func (d PageData) originOf(o *metrics.Origin) *OriginCard {
	if o == nil {
		return nil
	}
	c := &OriginCard{Head: fixedHead("memory-origin", "Where did it come from?", "", "")}
	switch o.State {
	case metrics.OriginThisPC:
		c.Head.Scope = "machine"
		sess, proj := sesslink(o.Session), d.projectLink(o.Project)
		c.Rows = append(c.Rows, OriginRow{Label: "Session", Session: &sess, Project: &proj})
		ended := OriginRow{Label: "Ended", Pre: "not known"}
		if o.Ended != nil {
			ended = OriginRow{Label: "Ended", Time: d.originTime(o.Ended)}
			if o.Sent != nil {
				ended.Post = ", sent " + chart.Span(gapMS(*o.Ended, *o.Sent)) + " later"
			}
		}
		became := OriginRow{Label: "Became memory", Pre: "not known"}
		if o.Sent != nil && o.Extracted != nil {
			became.Pre = chart.Span(gapMS(*o.Sent, *o.Extracted)) + " after it was sent"
		}
		first := OriginRow{Label: "First shown", Pre: "not yet"}
		if o.FirstShown != nil {
			first = OriginRow{Label: "First shown", Time: d.originTime(o.FirstShown)}
			if o.Extracted != nil {
				first.Post = ", " + chart.Span(gapMS(*o.Extracted, *o.FirstShown)) + " later"
			}
		} else {
			c.Note = "Never shown so far: it waits for a prompt on its topic. Its counts and its shows timeline are empty."
		}
		c.Rows = append(c.Rows, ended, became, first)
	case metrics.OriginOtherPC:
		c.Head.Scope = "server"
		c.Text = "From a session on another PC"
		if o.Created != nil && *o.Created != "" {
			c.Text += " (made " + dayLabel(*o.Created) + ")"
		}
		c.Text += ". Its turns are on that PC's dashboard."
	case metrics.OriginNoSession:
		c.Text = "Stored on its own: it did not come from a session."
	default:
		c.Text = "Not known yet: the next daily upkeep looks it up on the server."
	}
	return c
}

// BankDev are the developer cards of the Bank page.
type BankDev struct {
	Active        Card
	Concentration ConcentrationCard
}

// ConcentrationCard is how unevenly the shows fall: the index and the
// memories that took most.
type ConcentrationCard struct {
	Card
	Sentence TopSentence
	Rows     []TopRow
	More     string
}

// TopSentence is "The 10 most shown memories took 31 % of the 1,906 shows."
type TopSentence struct{ Memories, Share, Shows string }

// TopRow is a memory of the most shown.
type TopRow struct {
	N, Memory, Shown, Share, Used string
	Project                       Ref
}

func (d PageData) bankDevOf(r *metrics.Result) *BankDev {
	if !d.Frame.Prefs.Dev {
		return nil
	}
	l := d.L
	active, conc := d.view(r, "bank.active_share"), d.view(r, "shows.concentration")
	dev := &BankDev{}

	h := headOf(active, d.CLI, l, "", d.P)
	h.Chip = "developer"
	dev.Active = Card{Head: h, Note: d.noData(active), Dev: d.devIDs(active)}
	if dev.Active.Note.Text == "" {
		since := r.Window.Since.In(zoneOr(d.Loc)).Format("Jan 2")
		num, den := countOf(active.M.Num), countOf(active.M.Den)
		dev.Active.Figs = []Fig{
			{Label: "Shown at least once", Value: l.Percent(*active.M.Value),
				Sub: l.Int(num) + " of the " + l.Int(den) + " memories there were on " + since},
			{Label: "Never shown in the window", Value: l.Int(max(0, den-num)), Sub: "not a fault: most memory waits for its topic"},
		}
		dev.Active.Foot = "Memories made during the window are left out of both counts."
		if u, _ := extraAs[int64](active.M, "unresolved"); u > 0 {
			dev.Active.Extras = []string{format.Count(l, u, "memory", "memories") + " shown " + isAre(u) +
				" not in the record of new memories yet, so counted as older."}
		}
	}

	h = headOf(conc, d.CLI, l, "", d.P)
	h.Chip = "developer"
	dev.Concentration.Card = Card{Head: h, Note: d.noData(conc), Dev: d.devIDs(conc)}
	if dev.Concentration.Note.Text == "" {
		dev.Concentration.Leads = []Lead{{Big: l.Fixed(*conc.M.Value, 2),
			Label: "Gini of shows per memory shown (0: all shown alike, 1: one memory takes every show)"}}
		top, _ := extraAs[float64](conc.M, "top10_share")
		shows, _ := extraAs[int64](conc.M, "shows")
		rows := r.Tables.TopShown
		noun := "The most shown memory"
		if len(rows) > 1 {
			noun = "The " + l.Int(int64(len(rows))) + " most shown memories"
		}
		dev.Concentration.Sentence = TopSentence{Memories: noun, Share: l.Percent(top), Shows: l.Int(shows)}
		for i, row := range rows {
			dev.Concentration.Rows = append(dev.Concentration.Rows, TopRow{N: l.Int(int64(i + 1)), Memory: row.Memory,
				Project: d.projectLink(row.Project), Shown: l.Int(row.Shown), Share: l.Fixed(row.Share*100, 1) + " %", Used: l.Int(row.Used)})
		}
		dev.Concentration.More = allByShows
	}
	return dev
}

// QualityDev are the developer cards of the Quality page beside the ranking.
type QualityDev struct {
	Contradicted, SetChanged, Blocks Card
	Lang                             LangCard
}

// LangCard is the language test: how often prompts of each language got
// memory and the languages that fall far behind the most used one.
type LangCard struct {
	Card
	Rows  []LangRow
	Lines []LangLine
	Small string // the sentence on languages too small to test; "" none
}

// LangRow is a language of the test's table.
type LangRow struct {
	Name, Got, Interval, Prompts string
	Few                          bool
}

// LangLine is a language the test finds behind (or ahead): a sentence.
type LangLine struct{ Text string }

// languageNames are the languages prompts are read as; any other is shown
// by its code.
var languageNames = map[string]string{
	"en": "English", "fr": "French", "de": "German", "es": "Spanish", "it": "Italian", "pt": "Portuguese",
	"nl": "Dutch", "sv": "Swedish", "da": "Danish", "no": "Norwegian", "fi": "Finnish", "pl": "Polish",
	"cs": "Czech", "ru": "Russian", "uk": "Ukrainian", "tr": "Turkish", "ar": "Arabic", "he": "Hebrew",
	"hi": "Hindi", "ja": "Japanese", "ko": "Korean", "zh": "Chinese", "vi": "Vietnamese", "id": "Indonesian",
}

func languageName(code string) string {
	if n, ok := languageNames[code]; ok {
		return n
	}
	return views.Word(views.LanguageWords, code)
}

// The language test as recall.by_lang's extra holds it (metrics' langTest).
type (
	langBy struct {
		Num, Den int64
		CI       *[2]float64
	}
	langGap struct {
		Lang     string
		Num, Den int64
		Gap      float64
		Interval [2]float64
	}
	langLeft struct {
		Lang     string
		Num, Den int64
	}
	langTest struct {
		Reference string
		RefNum    int64 `json:"reference_num"`
		RefDen    int64 `json:"reference_den"`
		Gaps      []langGap
		LeftOut   []langLeft `json:"left_out"`
	}
)

func (d PageData) qualityDevOf(r *metrics.Result) *QualityDev {
	if !d.Frame.Prefs.Dev {
		return nil
	}
	l := d.L
	dev := &QualityDev{}
	dev.Contradicted = d.devFigure(r, "injection.contradicted", "Next prompt looked like a correction",
		func(num, den int64) string { return l.Int(num) + " of " + l.Int(den) + " prompts that got memory" })
	dev.Contradicted.Head.Pill = Word{"info only", "st-info"}
	dev.Contradicted.Hint = "The pattern that spots a correction has never been measured for precision, so this never gets a status. The sessions:"
	dev.Contradicted.HintCmd = "backsight stats sessions --has contradicted"

	dev.SetChanged = d.devFigure(r, "boost.set_changed", "Recalls it changed",
		func(num, den int64) string {
			return l.Int(num) + " of " + l.Int(den) + " recalls showed a different set"
		})
	dev.SetChanged.Hint, dev.SetChanged.HintCmd = "What another weight would do:", BoostReplayCommand
	if m := d.view(r, "boost.set_changed"); m.Found && slices.Contains(m.M.Tags, "partial") {
		dev.SetChanged.Extras = []string{"Recalls from before the learned ranking was recorded are not counted, so this reads low until the index is rebuilt (backsight stats index rebuild)."}
	}

	dev.Blocks = d.devFigure(r, "stopcheck.block_yield", "Held turns that then credited a memory",
		func(num, den int64) string { return l.Int(num) + " of " + l.Int(den) + " held turns" })
	if m := d.view(r, "stopcheck.block_yield"); m.Found {
		if by, ok := extraAs[map[string]int64](m.M, "by_outcome"); ok {
			var parts []string
			for _, o := range []string{"used", "confirmed", "corrected", "none", "refused"} {
				parts = append(parts, o+" "+l.Int(by[o]))
			}
			dev.Blocks.Extras = []string{"After the question: " + strings.Join(parts, ", ") + "."}
		}
	}
	dev.Blocks.Hint, dev.Blocks.HintCmd = "By where the memories were shown:", "backsight stats quality --dev"

	dev.Lang = d.langOf(r)
	return dev
}

// devFigure is a card of one metric whose figure is its share, with the
// counts under it in the words sub gives. It names no scope: the design
// draws these cards with the developer chip alone.
func (d PageData) devFigure(r *metrics.Result, id, label string, sub func(num, den int64) string) Card {
	v := d.view(r, id)
	h := headOf(v, d.CLI, d.L, "", d.P)
	h.Chip, h.Scope = "developer", ""
	c := Card{Head: h, Note: d.noData(v), Dev: d.devIDs(v)}
	if c.Note.Text == "" {
		c.Figs = []Fig{{Label: label, Value: d.L.Percent(*v.M.Value), Sub: sub(countOf(v.M.Num), countOf(v.M.Den))}}
	}
	return c
}

func (d PageData) langOf(r *metrics.Result) LangCard {
	v := d.view(r, "recall.by_lang")
	l := d.L
	h := fixedHead("language-test", "Does memory show up as often in every language?", "", d.CLI)
	h.Chip = "developer"
	if v.Found && v.M.Status == metrics.StatusWatch {
		h.Pill = Word{"watch", "st-watch"}
	}
	c := LangCard{Card: Card{Head: h, Dev: d.devIDs(v)}}
	test, ok := extraAs[langTest](v.M, "test")
	if !ok || test.Reference == "" {
		c.Note = NoData{Text: "No prompt has a language to compare yet."}
		return c
	}
	by, _ := extraAs[map[string]langBy](v.M, "by_lang")
	minN := int64(v.Def.MinN)
	row := func(code string, num, den int64) LangRow {
		lr := LangRow{Name: languageName(code), Prompts: l.Int(den)}
		if den < minN {
			lr.Got, lr.Few = l.Int(num)+" of "+l.Int(den), true
			return lr
		}
		lr.Got = l.Percent(float64(num) / float64(den))
		if b, ok := by[code]; ok && b.CI != nil {
			lr.Interval = strings.Replace(l.Interval(*b.CI), " %)", ")", 1)
		}
		return lr
	}
	c.Rows = append(c.Rows, row(test.Reference, test.RefNum, test.RefDen))
	for _, g := range test.Gaps {
		c.Rows = append(c.Rows, row(g.Lang, g.Num, g.Den))
		if line, ok := langLine(g, test.Reference); ok {
			c.Lines = append(c.Lines, line)
		}
	}
	for _, g := range test.LeftOut {
		c.Rows = append(c.Rows, row(g.Lang, g.Num, g.Den))
	}
	if len(test.LeftOut) > 0 {
		c.Small = "Languages with fewer than " + l.Int(minN) + " prompts show counts and are left out of the test."
	}
	return c
}

// langLine is the sentence for a language whose gap to the reference is over
// the limit with an interval that excludes 0, in either direction.
func langLine(g langGap, ref string) (LangLine, bool) {
	lo, hi := g.Interval[0], g.Interval[1]
	if !metrics.LangBehind(g.Gap, lo, hi) {
		return LangLine{}, false
	}
	dir, n, a, b := "less", pts(g.Gap), pts(lo), pts(hi)
	if g.Gap < 0 {
		dir, n, a, b = "more", -n, -b, -a
	}
	return LangLine{Text: fmt.Sprintf("%s prompts get memory %d pts %s often than %s (%d to %d pts): a gap over %d pts whose interval excludes 0.",
		languageName(g.Lang), n, dir, languageName(ref), a, b, metrics.LangGapPoints)}, true
}
