package metrics

import (
	"database/sql"
	"slices"
	"time"
)

// memory.yield: of the memories created in a week, the share Claude used
// within 28 days of their day. Weeks are the seven dates Monday to Sunday of
// the view's zone; a memory is in a week by its created_day (a UTC date),
// a server day (a UTC bucket) by its date. The memories and their number
// come from the server (its series, else the days this PC recorded), the
// uses from this PC's credits.

const yieldID = "memory.yield"

// yieldReach is how long after its day began a credit still counts for a
// memory: 29 days at 00:00 UTC, so the last moment is 28 days after the day
// ends.
const yieldReach = 29

// yieldMaxUnresolved is the share of used memories without a row beyond which the
// shares are withheld.
const yieldMaxUnresolved = 0.05

const dateLayout = "2006-01-02"

// The record's and the server's operations whose tokens a memory costs.
var yieldOps = []string{"retain", "consolidation"}

// yieldWeek is one week of the window.
type yieldWeek struct {
	monday   string
	dates    []string // the seven dates, Monday to Sunday
	complete bool
	source   string // server | record | none
	den      *int64 // memories created; nil: some day neither the series nor the record holds
	num      int64
	tokens   *int64
}

// yieldRun is what memory.yield reads over a window.
type yieldRun struct {
	started                             bool // some day list was read
	weeks                               []*yieldWeek
	unresolved, gone, pending, resolved int64
}

// yieldSpan is how far back the weeks go from the window's end: the
// server's longest series, whatever the view's window (a short window must
// not leave every cohort incomplete).
var yieldSpan = 90 * day

// yieldWindow is the span the weeks cover: yieldSpan ending at w's end.
func yieldWindow(w Window) Window {
	return Window{Since: w.Until.Add(-yieldSpan), Until: w.Until, TZ: w.TZ, Loc: w.Loc}
}

// readYield is the run over w's end, read once per transaction.
func readYield(e env, w Window) (*yieldRun, error) {
	w = yieldWindow(w)
	k := [2]int64{w.Since.UnixNano(), w.Until.UnixNano()}
	if e.memo != nil {
		if run, ok := e.memo.yield[k]; ok {
			return run, nil
		}
	}
	run, err := loadYield(e, w)
	if err == nil && e.memo != nil {
		if e.memo.yield == nil {
			e.memo.yield = map[[2]int64]*yieldRun{}
		}
		e.memo.yield[k] = run
	}
	return run, err
}

// weeksOf are the weeks overlapping w (the yield span), oldest first.
func weeksOf(w Window) []*yieldWeek {
	civil := func(t time.Time) time.Time {
		y, m, d := t.In(w.Loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	first := civil(w.Since)
	first = first.AddDate(0, 0, -((int(first.Weekday()) + 6) % 7))
	last := civil(w.Until.Add(-time.Nanosecond))
	var out []*yieldWeek
	for mon := first; !mon.After(last); mon = mon.AddDate(0, 0, 7) {
		wk := &yieldWeek{monday: mon.Format(dateLayout), source: "none"}
		for i := range 7 {
			wk.dates = append(wk.dates, mon.AddDate(0, 0, i).Format(dateLayout))
		}
		// every member's 28 days after its day have passed
		wk.complete = !w.Until.Before(mon.AddDate(0, 0, 6+yieldReach))
		out = append(out, wk)
	}
	return out
}

func loadYield(e env, w Window) (*yieldRun, error) {
	run := &yieldRun{weeks: weeksOf(w)}
	if err := e.tx.QueryRowContext(e.ctx, `SELECT EXISTS (SELECT 1 FROM memory_days)`).Scan(&run.started); err != nil {
		return nil, err
	}
	if !run.started || len(run.weeks) == 0 {
		return run, nil
	}
	if err := readCreated(e, run); err != nil {
		return nil, err
	}
	if err := readTokens(e, run); err != nil {
		return nil, err
	}
	if err := readCohorts(e, run); err != nil {
		return nil, err
	}
	return run, readUsedWithoutRow(e, w, run)
}

// bounds are the first and last date of the weeks, and the first date's
// instant (unix ms) and the one after the last.
func (r *yieldRun) bounds() (first, last string, fromMs, toMs int64) {
	first = r.weeks[0].dates[0]
	last = r.weeks[len(r.weeks)-1].dates[6]
	f, _ := time.Parse(dateLayout, first)
	l, _ := time.Parse(dateLayout, last)
	return first, last, f.UnixMilli(), l.AddDate(0, 0, 1).UnixMilli()
}

// readCreated sets each week's memories created: per day the server's
// series, else the day line this PC recorded (server_day: the latest line
// of the bucket); a week with a day neither holds has none.
func readCreated(e env, r *yieldRun) error {
	perDay, held := map[string]int64{}, map[string]string{}
	if serverReason(e, yieldSeriesKey) == "" {
		// The series counts back from now, so its first bucket may cover
		// only part of its day; a week that starts there may read low. The
		// weeks that old are rarely complete cohorts anyway.
		for _, b := range e.srv.YieldSeries.Buckets {
			d := b.Time.UTC().Format(dateLayout)
			perDay[d] += b.World + b.Experience + b.Observation
			held[d] = "server"
		}
	}
	_, _, from, to := r.bounds()
	recorded, err := readRecordedNew(e, from, to)
	if err != nil {
		return err
	}
	for _, n := range recorded {
		if d := n.Bucket.Format(dateLayout); held[d] == "" {
			perDay[d], held[d] = n.total(), sourceRecord
		}
	}
	for _, wk := range r.weeks {
		var sum int64
		wk.source = "server"
		for _, d := range wk.dates {
			switch held[d] {
			case "":
				wk.source = "none"
			case "record":
				if wk.source == "server" {
					wk.source = "record"
				}
			}
			sum += perDay[d]
			if wk.source == "none" {
				break
			}
		}
		if wk.source != "none" {
			wk.den = &sum
		}
	}
	return nil
}

// readTokens sets each week's extraction tokens (input + output + thoughts of
// retain and consolidation, the one reading of tokens, as extraction.tokens): the server's stats for the
// days they hold, else the record's day lines. nil when the week has none.
func readTokens(e env, r *yieldRun) error {
	type key struct{ op, day string }
	sums, held := map[key]tokens{}, map[key]bool{}
	for _, op := range yieldOps {
		if _, reason := llmRead(e, yieldLLMPrefix+op); reason != "" {
			continue
		}
		for _, b := range e.srv.YieldLLM[op] {
			k := key{op, b.Time.UTC().Format(dateLayout)}
			t := sums[k]
			t.add(b)
			sums[k], held[k] = t, true
		}
	}
	_, _, from, to := r.bounds()
	rows, err := readRecordedLLM(e, from, to, yieldOps)
	if err != nil {
		return err
	}
	recorded := map[key]tokens{}
	for _, r := range rows {
		recorded[key{r.Op, r.Bucket.Format(dateLayout)}] = r.T
	}
	for _, wk := range r.weeks {
		var sum tokens
		var any bool
		for _, d := range wk.dates {
			for _, op := range yieldOps {
				k := key{op, d}
				switch {
				case held[k]:
					sum.input, sum.output, any = sum.input+sums[k].input, sum.output+sums[k].output, true
				default:
					if t, ok := recorded[k]; ok {
						sum.input, sum.output, any = sum.input+t.input, sum.output+t.output, true
					}
				}
			}
		}
		if any {
			wk.tokens = i64(sum.total())
		}
	}
	return nil
}

// readCohorts sets each week's numerator: the memories whose latest day-list
// row is created on one of its dates and that have a used credit (any agent)
// before their day began + 29 days.
func readCohorts(e env, r *yieldRun) error {
	first, last, _, _ := r.bounds()
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT d.created_day, COUNT(*) FROM memory_doc d
WHERE d.created_day >= ? AND d.created_day <= ?
  AND EXISTS (SELECT 1 FROM credits c WHERE c.memory_id = d.memory_id AND c.verdict = 'used'
              AND c.ts < (CAST(strftime('%s', d.created_day) AS INTEGER) + ?) * 1000)
GROUP BY d.created_day ORDER BY d.created_day`, first, last, int64(yieldReach)*86400)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d string
		var n int64
		if err := rows.Scan(&d, &n); err != nil {
			return err
		}
		for _, wk := range r.weeks {
			if slices.Contains(wk.dates, d) {
				wk.num += n
			}
		}
	}
	return rows.Err()
}

// latestComplete is the latest complete week, nil when none.
func (r *yieldRun) latestComplete() *yieldWeek {
	for _, wk := range slices.Backward(r.weeks) {
		if wk.complete {
			return wk
		}
	}
	return nil
}

// readUsedWithoutRow sorts the memories credited used in the weeks' span
// (briefing pages aside):
// with a day-list row (resolved), or without one: gone (X's coverage),
// pending (first shown after the days listed, in no complete week) or
// unresolved.
func readUsedWithoutRow(e env, w Window, r *yieldRun) error {
	_, _, from, _ := r.bounds()
	covered, err := coveredUntil(e)
	if err != nil {
		return err
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT x.has, CASE WHEN x.has THEN NULL ELSE (SELECT MIN(f.ts) FROM shows f WHERE f.memory_id = x.memory_id) END
FROM (SELECT c.memory_id, EXISTS (SELECT 1 FROM memory_docs d WHERE d.memory_id = c.memory_id) AS has
FROM (SELECT DISTINCT memory_id FROM credits WHERE verdict = 'used' AND memory_id IS NOT NULL AND `+notPage("memory_id")+` AND ts >= ? AND ts < ?) c) x
ORDER BY x.memory_id`, from, w.Until.UnixMilli())
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	latestLast := ""
	if wk := r.latestComplete(); wk != nil {
		latestLast = wk.dates[6]
	}
	for rows.Next() {
		var has bool
		var first sql.NullInt64
		if err := rows.Scan(&has, &first); err != nil {
			return err
		}
		switch {
		case has:
			r.resolved++
		case first.Valid && goneBy(covered, first.Int64):
			r.gone++
		case first.Valid && covered.Valid && latestLast != "" && covered.String >= latestLast &&
			time.UnixMilli(first.Int64).UTC().Format(dateLayout) > covered.String:
			r.pending++
		default:
			r.unresolved++
		}
	}
	return rows.Err()
}

// withheld reports whether the shares are withheld: more than 5 % of the
// credited memories are unresolved while the day lists catch up.
func (r *yieldRun) withheld() bool {
	if r.unresolved == 0 {
		return false
	}
	return float64(r.unresolved)/float64(r.resolved+r.unresolved) > yieldMaxUnresolved
}

// yield fills memory.yield.
func yield(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	run, err := readYield(e, w)
	if err != nil {
		return err
	}
	extra := map[string]any{"tokens": (*int64)(nil), "tokens_per_used": (*float64)(nil),
		"unresolved": run.unresolved, "gone": run.gone, "pending": run.pending, "resolved_used": run.resolved, "withheld": nil}
	if !run.started { // the record has not started, or the server cannot list by creation day
		obs[yieldID], met[yieldID] = Observation{Absent: true, Reason: ReasonNoEvents}, Metric{Extra: extra}
		return nil
	}
	withheld := run.withheld()
	if withheld {
		extra["withheld"] = ReasonRecordCatchingUp
	}
	m := Metric{Extra: extra}
	if run.unresolved > 0 {
		m.Tags = []string{tagPartial}
	}
	if e.q.Project != "" { // the weeks count the whole bank's new memories
		m.Tags = append(m.Tags, tagNotFiltered)
	}
	for _, wk := range run.weeks {
		p := Point{Week: wk.monday, Source: wk.source, Complete: ptr(wk.complete)}
		if wk.complete && wk.den != nil {
			p.Num, p.Den = i64(wk.num), wk.den
			if *wk.den >= int64(minYieldN()) && !withheld {
				p.Value = ratioAtMostOne(wk.num, *wk.den) // a forgotten memory can still be credited
				lo, hi := Wilson(wk.num, *wk.den)
				p.CI = &[2]float64{lo, hi}
			}
			p.Tokens = wk.tokens
			if wk.tokens != nil && wk.num > 0 {
				p.TokensPerUsed = ptr(float64(*wk.tokens) / float64(wk.num))
			}
		}
		m.Series = append(m.Series, p)
	}
	// The status when no share is shown says why: no complete week, a
	// record still catching up, a week the server and the record do not
	// hold; too_few only for a real small number of new memories.
	o := Observation{Absent: true, Reason: ReasonNoCompleteWeek}
	if wk := run.latestComplete(); wk != nil {
		p := m.Series[slices.IndexFunc(m.Series, func(p Point) bool { return p.Week == wk.monday })]
		m.Value, m.Num, m.Den = p.Value, p.Num, p.Den
		extra["tokens"], extra["tokens_per_used"] = p.Tokens, p.TokensPerUsed
		switch {
		case withheld:
			o.Reason = ReasonRecordCatchingUp
		case p.Den == nil:
			o.Reason = ReasonNoRecord
		default:
			o = Observation{Num: *p.Num, Den: *p.Den}
		}
	}
	obs[yieldID], met[yieldID] = o, m
	return nil
}

// minYieldN is the catalog's min_n of memory.yield: a week needs that many
// new memories to show a share.
func minYieldN() int {
	d, _ := DefOf(yieldID)
	return d.MinN
}
