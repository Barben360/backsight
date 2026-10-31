package metrics

import (
	"database/sql"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

// The verdict: is memory working, on four axes, always over the last 7
// days against the 7 before, whatever the view's window, for the query's
// project. Per axis the first rule that applies wins: learning (found and
// right), too early, can't judge (right), attention, ok. The attention
// list and the info items come with it.

// axisMetrics are the metrics each axis reads.
var axisMetrics = map[string][]string{
	"found":   {"recall.hit_rate", "recall.empty_reasons"},
	"right":   {"used.turn_rate", "declare.coverage", "verdict.wrong_rate"},
	"cost":    {"context.per_session", "recall.latency"},
	"flowing": {"pipeline.flow", "local.backlog", "server.queue", "bank.consolidation", "hooks.failures", "jobs.last_run", "server.reach", "doctor.check"},
}

// axisOrder is the order of the axes on the banner and in the attention
// list.
var axisOrder = []string{"found", "right", "cost", "flowing"}

// judgedMetrics are the metrics whose counts make an axis too early: the
// axis's metrics that have a status rule (recall.empty_reasons has none).
// Flowing is judged from day 1, never too early.
var judgedMetrics = map[string][]string{
	"found": {"recall.hit_rate"},
	"right": {"used.turn_rate", "declare.coverage", "verdict.wrong_rate"},
	"cost":  {"context.per_session", "recall.latency"},
}

// familyAxis is the axis of a family's items in the attention list.
var familyAxis = map[string]string{"reach": "found", "quality": "right", "cost": "cost", "bank": "flowing", "pipeline": "flowing"}

const (
	verdictDays     = 7
	fallbackDays    = 28
	learningPrompts = 20 // typed prompts in the last 7 days
	attentionShown  = 5  // items listed; the rest are counted
	axisItems       = 3  // attention items on an axis's banner
)

// tagJudged28d marks Right's metrics judged on 28 days for want of turns.
const tagJudged28d = "judged_28d"

// rootCauseChecks are the doctor checks a server that does not answer
// fails anyway; their items fold into the root cause.
var rootCauseChecks = map[string]bool{"server": true, "server-reach": true}

// assessment is the verdict with what it was judged on: the metrics of the
// last 7 days and of the 7 before, and Right's 28-day metrics when it fell
// back to them.
type assessment struct {
	Verdict Verdict
	Items   []Item // every item, sorted
	Info    []InfoItem
	Now     map[string]Metric
	Prev    map[string]Metric
	Right28 map[string]Metric // nil unless Right is judged on 28 days
	Window  Window            // the verdict's last 7 days
	Query   Query             // the query judged, its Window zeroed
	ran28   bool              // Right's 28-day figures were computed
}

// metricsFor is the verdict's own metrics of ids for the view's query q
// when q asks for the verdict's very figures: its window is the verdict's
// (the default 7-day view) and every other field (project, machines,
// path, developer mode, paging) is the one the verdict was judged with,
// so the view need not compute the families again. A window built up to a
// minute before now is the verdict's own (assess takes it as it is), and
// its figures are computed over exactly that window inside the same
// transaction, so they are the view's. False for any other query, and for
// an id the verdict did not compute (a developer metric).
func (a assessment) metricsFor(q Query, ids []string) (map[string]Metric, bool) {
	w := q.Window
	q.Window = Window{}
	if q != a.Query || !w.Since.Equal(a.Window.Since) || !w.Until.Equal(a.Window.Until) {
		return nil, false
	}
	out := make(map[string]Metric, len(ids))
	for _, id := range ids {
		m, ok := a.Now[id]
		if !ok {
			return nil, false
		}
		out[id] = m
	}
	return out, true
}

// verdict judges e's data for the overview: the verdict, the first
// attentionShown attention items, how many more there are, and the info
// items. It reads inside e's transaction and ignores e.q.Window.
func verdict(e env) (Verdict, []Item, int, []InfoItem, error) {
	a, err := assess(e)
	if err != nil {
		return Verdict{}, nil, 0, nil, err
	}
	items, more := a.Items, 0
	if len(items) > attentionShown {
		items, more = items[:attentionShown], len(items)-attentionShown
	}
	return a.Verdict, items, more, a.Info, nil
}

// judgingRun holds one assessment's inputs while its rules run.
type judgingRun struct {
	e                  env // q.Window is the last 7 days
	w                  Window
	cur, prev          map[string]Metric
	rightW             Window // Right's window: w, or the last 28 days
	rightCur, rightPrv map[string]Metric
}

func assess(e env) (assessment, error) {
	loc := e.q.Window.Loc
	if loc == nil {
		loc = time.UTC
	}
	// The view's own window when it is the last 7 days, built a moment
	// before now (within the minute the server reads allow): its figures
	// are then the verdict's and can be reused.
	w := e.q.Window
	if w.Length() != verdictDays*day || w.Until.Sub(e.now).Abs() > time.Minute {
		w = e.q.Window.span(e.now.Add(-verdictDays*day).In(loc), e.now.In(loc))
	}
	w.Loc = loc
	e.q.Window = w // the figures as of now are this window's
	var ids, twoWindow []string
	for _, d := range Catalog {
		if d.Dev {
			continue
		}
		ids = append(ids, d.ID)
		if !d.ErrorClass && (d.Absolute.On != "" || d.Relative) {
			twoWindow = append(twoWindow, d.ID)
		}
	}
	cur, err := computeMetrics(e, w, ids)
	if err != nil {
		return assessment{}, err
	}
	prev, err := computeMetrics(e, w.Previous(), twoWindow)
	if err != nil {
		return assessment{}, err
	}
	r := &judgingRun{e: e, w: w, cur: cur, prev: prev, rightW: w, rightCur: cur, rightPrv: prev}
	judged := e.q
	judged.Window = Window{}
	out := assessment{Now: cur, Prev: prev, Window: w, Query: judged}

	learning, learnText, err := r.learning()
	if err != nil {
		return assessment{}, err
	}
	// Learning needs prompts to read (R26): not on a server-only host or
	// without any prompt, and not for Right while feedback is off.
	learns := map[string]bool{}
	if learning && cur["recall.hit_rate"].Status != StatusNoData {
		learns["found"] = true
		learns["right"] = !structuralNoData(cur, judgedMetrics["right"])
	}
	// Right, too early on 7 days for want of turns, is judged on the last
	// 28 against the 28 before, when that is not too early too; with
	// nothing to read at all (feedback off, server only) 28 days hold
	// nothing more.
	var right28Early *Reason
	if !learns["right"] && hasTooFew(cur, judgedMetrics["right"]) {
		if early, _ := tooEarly(cur, judgedMetrics["right"]); early {
			out.ran28 = true
			w28 := w.Last(fallbackDays * day)
			c28, err := computeMetrics(e, w28, axisMetrics["right"])
			if err != nil {
				return assessment{}, err
			}
			p28, err := computeMetrics(e, w28.Previous(), axisMetrics["right"])
			if err != nil {
				return assessment{}, err
			}
			if early28, reason := tooEarly(c28, judgedMetrics["right"]); !early28 {
				for id, m := range c28 {
					m.Tags = append(m.Tags, tagJudged28d)
					c28[id] = m
				}
				r.rightW, r.rightCur, r.rightPrv = w28, c28, p28
				out.Right28 = c28
			} else if hasTooFew(c28, judgedMetrics["right"]) {
				right28Early = &reason // the 28-day counts say how far it is
			}
		}
	}

	root, err := r.rootCause()
	if err != nil {
		return assessment{}, err
	}
	items, err := r.items(root != nil)
	if err != nil {
		return assessment{}, err
	}
	if root != nil {
		items = append(items, *root)
	}

	axes := map[string]Axis{}
	var cantJudge *Item
	for _, name := range axisOrder {
		ax := Axis{State: AxisOK, WindowDays: verdictDays}
		var state *Reason
		cur, prev := r.cur, r.prev
		if name == "right" {
			cur, prev = r.rightCur, r.rightPrv
			if out.Right28 != nil {
				ax.WindowDays = fallbackDays
			}
		}
		early, earlyReason := false, Reason{}
		if judged, ok := judgedMetrics[name]; ok {
			early, earlyReason = tooEarly(cur, judged)
		}
		switch {
		case learns[name]:
			ax.State = AxisLearning
			state = &Reason{Metric: axisMetrics[name][0], Severity: SeverityInfo, Text: learnText}
		case early:
			ax.State = AxisTooEarly
			if name == "right" && right28Early != nil {
				earlyReason, ax.WindowDays = *right28Early, fallbackDays
			}
			state = &earlyReason
		case name == "right" && cantJudgeOn(cur["declare.coverage"]):
			ax.State = AxisCantJudge
			c := cur["declare.coverage"]
			d, _ := DefOf("declare.coverage")
			text := cantJudgeSentence(*c.Value)
			state = &Reason{Metric: d.ID, Severity: SeverityWatch, Text: text}
			cantJudge = &Item{Metric: d.ID, Severity: SeverityWatch, Axis: "right", Text: text, Why: d.Action, Command: d.Command}
		case name == "flowing" && (root != nil || anyAlarm(cur, axisMetrics[name])):
			ax.State = AxisAttention
		case name != "flowing" && axisAlarm(cur, prev, axisMetrics[name]):
			ax.State = AxisAttention
		}
		if state != nil {
			ax.Reasons = append(ax.Reasons, *state)
		}
		if name == "right" && out.Right28 != nil {
			ax.Reasons = append(ax.Reasons, Reason{Metric: "used.turn_rate", Severity: SeverityInfo, Text: judged28Sentence})
		}
		axes[name] = ax
	}
	if cantJudge != nil { // one item for the coverage, the can't-judge one
		items = slices.DeleteFunc(items, func(it Item) bool { return it.Metric == "declare.coverage" })
		items = append(items, *cantJudge)
	}
	sortItems(items)
	for _, name := range axisOrder {
		ax := axes[name]
		if ax.State != AxisOK {
			ax.Reasons = withItems(ax.Reasons, items, name, root)
		}
		if ax.Reasons == nil {
			ax.Reasons = []Reason{}
		}
		axes[name] = ax
	}
	out.Verdict = Verdict{WindowDays: verdictDays, Learning: axes["found"].State == AxisLearning || axes["right"].State == AxisLearning, Axes: axes}
	out.Items = items
	out.Info = infoItems(cur)
	return out, nil
}

// structuralNoData: every metric of ids reads no_data for a reason more
// prompts will not change (feedback off, a server-only host).
func structuralNoData(ms map[string]Metric, ids []string) bool {
	return !slices.ContainsFunc(ids, func(id string) bool {
		m := ms[id]
		return m.Status != StatusNoData || m.Reason == ReasonNoEvents
	})
}

// learning reports whether this PC (or the project) is still learning:
// fewer than 7 days since the first typed prompt, or fewer than 20 typed
// prompts in the last 7 days; with the banner's sentence. Which axes it
// holds is the caller's (Found and Right, when they have prompts to read).
func (r *judgingRun) learning() (bool, string, error) {
	// Two reads on the time index: the first typed prompt (read in time
	// order, it stops at the first one) and the window's count; one
	// aggregate over both would scan every prompt ever written.
	var first sql.NullInt64
	var n int64
	err := r.e.tx.QueryRowContext(r.e.ctx, `
SELECT ts FROM tel_prompt
WHERE (reason IS NULL OR reason <> 'machine') AND ts < ? AND `+projectClause+` ORDER BY ts LIMIT 1`,
		r.w.Until.UnixMilli(), r.e.q.Project, r.e.q.Project).Scan(&first)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", err
	}
	err = r.e.tx.QueryRowContext(r.e.ctx, `
SELECT COUNT(*) FROM tel_prompt
WHERE (reason IS NULL OR reason <> 'machine') AND ts >= ? AND ts < ? AND `+projectClause,
		r.w.Since.UnixMilli(), r.w.Until.UnixMilli(), r.e.q.Project, r.e.q.Project).Scan(&n)
	if err != nil {
		return false, "", err
	}
	age := time.Duration(-1)
	if first.Valid {
		age = r.e.now.Sub(time.UnixMilli(first.Int64))
	}
	if age >= verdictDays*day && n >= learningPrompts {
		return false, "", nil
	}
	d := 1 // the day of the learning week, at most its 7th: a PC learning
	// for want of prompts long after its first one is not "day 30 of 7"
	if age > 0 {
		d = min(int(age/day)+1, verdictDays)
	}
	return true, learningSentence(d, n), nil
}

// tooEarly reports whether every metric of ids is too_few or no_data, with
// the sentence: the counts of the too_few metric closest to its min_n,
// else the reason of the first no_data one.
func tooEarly(ms map[string]Metric, ids []string) (bool, Reason) {
	var best string
	var bestFrac float64
	noData := ""
	for _, id := range ids {
		m := ms[id]
		switch m.Status {
		case StatusTooFew:
			d, _ := DefOf(id)
			frac := float64(den0(m)) / float64(max(1, d.MinN))
			if best == "" || frac > bestFrac {
				best, bestFrac = id, frac
			}
		case StatusNoData:
			if noData == "" {
				noData = id
			}
		default:
			return false, Reason{}
		}
	}
	if best != "" {
		d, _ := DefOf(best)
		return true, Reason{Metric: best, Severity: SeverityInfo, Text: tooEarlySentence(den0(ms[best]), countUnits[best], d.MinN)}
	}
	if noData == "" {
		return false, Reason{}
	}
	return true, Reason{Metric: noData, Severity: SeverityInfo, Text: noDataSentence(ms[noData].Reason)}
}

// hasTooFew reports whether one of ids is too_few (rather than no_data).
func hasTooFew(ms map[string]Metric, ids []string) bool {
	return slices.ContainsFunc(ids, func(id string) bool { return ms[id].Status == StatusTooFew })
}

func den0(m Metric) int64 {
	if m.Den == nil {
		return 0
	}
	return *m.Den
}

// cantJudgeOn: the coverage has its 20 turns and even its upper bound is
// under 80%.
func cantJudgeOn(c Metric) bool {
	d, _ := DefOf("declare.coverage")
	return c.Status != StatusNoData && c.Value != nil && den0(c) >= int64(d.MinN) && c.CI != nil && c.CI[1] < *d.Absolute.Watch
}

func alarm(s Status) bool { return s == StatusWatch || s == StatusBad }

// anyAlarm: one of ids is at watch or bad now.
func anyAlarm(cur map[string]Metric, ids []string) bool {
	return slices.ContainsFunc(ids, func(id string) bool { return alarm(cur[id].Status) })
}

// twoWindows is the rule of the non-error metrics: watch or bad in both
// windows; recall.latency at bad counts at once.
func twoWindows(id string, cur, prev map[string]Metric) bool {
	if id == "recall.latency" && cur[id].Status == StatusBad {
		return true
	}
	return alarm(cur[id].Status) && alarm(prev[id].Status)
}

// axisAlarm is Found's, Right's and Cost's attention rule.
func axisAlarm(cur, prev map[string]Metric, ids []string) bool {
	return slices.ContainsFunc(ids, func(id string) bool { return twoWindows(id, cur, prev) })
}

// rootCause is the one item that stands for everything a server that does
// not answer causes, nil when it answers: this PC records it failing, its
// breaker is open, or every server read of the view failed unreachable or
// timed out. Watch, bad once the backlog is older than 24 h.
func (r *judgingRun) rootCause() (*Item, error) {
	e := r.e
	l, err := readLocal(e)
	if err != nil {
		return nil, err
	}
	failing := l != nil && l.reachFailingSince.Valid
	open := l != nil && l.breakerOpenUntil.Valid && time.UnixMilli(l.breakerOpenUntil.Int64).After(e.now)
	if !failing && !open && !serverDown(e) {
		return nil, nil
	}
	var since time.Time
	var notes, sessions int64
	if l != nil {
		notes, sessions = l.notesQueued, l.sessionsWaiting
		switch {
		case l.reachFailingSince.Valid:
			since = time.UnixMilli(l.reachFailingSince.Int64)
		case l.reachLastOK.Valid:
			since = time.UnixMilli(l.reachLastOK.Int64)
		}
	}
	age, err := e.backlogAge()
	if err != nil {
		return nil, err
	}
	sev := SeverityWatch
	if age > day {
		sev = SeverityBad
	}
	d, _ := DefOf("server.reach")
	return &Item{Metric: d.ID, Severity: sev, Axis: "flowing", Text: rootCauseSentence(since, e.now, r.w.Loc, notes, sessions),
		Why: d.Action, Command: "backsight doctor"}, nil
}

// serverDown: the view read the server and every read failed unreachable
// or timed out.
func serverDown(e env) bool {
	if e.server == nil || e.server.OK || e.srv == nil || len(e.srv.Err) == 0 {
		return false
	}
	for _, err := range e.srv.Err {
		if enum, _ := serverError(err); enum != "unreachable" && enum != "timeout" {
			return false
		}
	}
	return true
}

// items is the attention list before sorting: every non-dev metric at
// watch or bad (error-class ones at once, the others under the two-window
// rule; Right's on its own window), one item per doctor check in error and
// per warning but update. Under a root cause, what it stands for is left
// out: the backlog, the queue, the reach, the doctor's server checks, and
// pipeline.flow but for its failed and set-aside documents.
func (r *judgingRun) items(root bool) ([]Item, error) {
	var out []Item
	for _, d := range Catalog {
		if d.Dev || d.ID == "doctor.check" || strings.HasPrefix(d.ID, "stale.") {
			continue
		}
		cur, prev, w := r.cur, r.prev, r.w
		if d.Family == "quality" {
			cur, prev, w = r.rightCur, r.rightPrv, r.rightW
		}
		m := cur[d.ID]
		if !alarm(m.Status) || (!d.ErrorClass && !twoWindows(d.ID, cur, prev)) {
			continue
		}
		sev := Severity(m.Status)
		if root {
			switch d.ID {
			case "local.backlog", "server.queue", "server.reach":
				continue
			case "pipeline.flow":
				failed, _ := m.Extra["failed"].(int64)
				setAside, _ := m.Extra["set_aside"].(int64)
				if failed+setAside == 0 {
					continue
				}
				sev = SeverityWatch // the failed and set-aside rule; the wait is the root cause's
			}
		}
		var median float64
		var hasMedian bool
		if d.Relative {
			var err error
			if median, hasMedian, err = r.median(d, w); err != nil {
				return nil, err
			}
		}
		out = append(out, Item{Metric: d.ID, Severity: sev, Axis: familyAxis[d.Family],
			Text: itemText(d, m, w.Days, median, hasMedian), Why: d.Action, Command: d.Command})
	}
	if doctorRead(r.e) {
		d, _ := DefOf("doctor.check")
		for _, c := range doctorEntries(r.e.srv.Doctor) {
			if root && rootCauseChecks[c.ID] {
				continue
			}
			var text string
			switch {
			case c.Status == "error":
				text = doctorErrorSentence(c.ID)
			case c.Status == "warn" && c.ID != "update":
				text = doctorWarnSentence(c.ID)
			default:
				continue
			}
			cmd := c.Fix
			if cmd == "" {
				cmd = d.Command
			}
			out = append(out, Item{Metric: d.ID, Severity: SeverityWatch, Axis: "flowing", Text: text, Why: d.Action, Command: cmd})
		}
	}
	return out, nil
}

// median is a relative metric's baseline median for w: the family run on
// each of w's four 7-day baselines, as the status engine reads them.
func (r *judgingRun) median(d Def, w Window) (float64, bool, error) {
	fam := families.byID[d.ID]
	if fam == nil {
		return 0, false, nil
	}
	bases := make([]map[string]Observation, 0, 4)
	for _, b := range w.Baselines() {
		obs, _, err := r.e.run(fam, b)
		if err != nil {
			return 0, false, err
		}
		bases = append(bases, obs)
	}
	m, ok := baselineMedian(d, baselineOf(d.ID, bases))
	return m, ok, nil
}

// sortItems orders the attention list: bad first, then by axis, then by
// metric id; items of one metric keep their order.
func sortItems(items []Item) {
	slices.SortStableFunc(items, func(a, b Item) int {
		if a.Severity != b.Severity {
			if a.Severity == SeverityBad {
				return -1
			}
			if b.Severity == SeverityBad {
				return 1
			}
		}
		if ia, ib := slices.Index(axisOrder, a.Axis), slices.Index(axisOrder, b.Axis); ia != ib {
			return ia - ib
		}
		return strings.Compare(a.Metric, b.Metric)
	})
}

// withItems adds to an axis's reasons up to axisItems attention items of
// the axis, the root cause first; an item saying what a reason already
// says only lends it its command.
func withItems(reasons []Reason, items []Item, axis string, root *Item) []Reason {
	var mine []Item
	if root != nil && axis == root.Axis {
		mine = append(mine, *root)
	}
	for _, it := range items {
		if it.Axis == axis && (root == nil || it != *root) {
			mine = append(mine, it)
		}
	}
	added := 0
	for _, it := range mine {
		if i := slices.IndexFunc(reasons, func(r Reason) bool { return r.Metric == it.Metric && r.Text == it.Text }); i >= 0 {
			reasons[i].Command = it.Command
			continue
		}
		if added == axisItems {
			break
		}
		reasons = append(reasons, Reason{Metric: it.Metric, Severity: it.Severity, Text: it.Text, Command: it.Command})
		added++
	}
	return reasons
}

// infoItems are the stale counts above zero, for the review queue.
func infoItems(cur map[string]Metric) []InfoItem {
	out := []InfoItem{}
	for _, id := range []string{"stale.never_used", "stale.hidden", "stale.reported"} {
		m := cur[id]
		if m.Status == StatusNoData || m.Value == nil || *m.Value <= 0 {
			continue
		}
		out = append(out, InfoItem{Metric: id, Count: int64(*m.Value), Command: "backsight memory stale"})
	}
	return out
}

// summary is the overview's summary line over the view's window, from its
// metrics: prompts that got memory, turns that used it, the characters
// added to Claude's context (every session's, the per-day parts of
// context.per_session summed) with their approximate tokens, and the
// server's extraction cost when prices are set.
func summary(m map[string]Metric) Summary {
	s := Summary{ExtractionScope: "server"}
	if n := m["recall.hit_rate"].Num; n != nil {
		s.PromptsWithMemory = *n
	}
	if n := m["used.turn_rate"].Num; n != nil {
		s.TurnsWithMemoryUsed = *n
	}
	for _, p := range m["context.per_session"].Series {
		for _, c := range p.Parts {
			s.ContextChars += c
		}
	}
	s.TokensApprox = int64(math.Round(float64(s.ContextChars) / 4))
	if c := m["extraction.cost"]; c.Status != StatusNoData {
		s.PriceSet, _ = c.Extra["price_set"].(bool)
		s.Currency, _ = c.Extra["currency"].(string)
		if s.PriceSet && c.Value != nil {
			v := *c.Value
			s.ExtractionCost = &v
		}
	}
	return s
}
