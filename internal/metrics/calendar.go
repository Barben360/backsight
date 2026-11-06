package metrics

import (
	"fmt"
	"strings"
	"time"
)

// calendarMonths is how far back the days calendar reaches.
const calendarMonths = 13

// calendar is the days view's calendar: one row per local day (in the view's
// zone) from the Monday on or before today less 13 months plus a day, to
// today, earliest first, whatever the view's window. Prompts are the typed
// ones (a machine's are not), with memory those that got some; a session
// counts for a day when one of its prompt lines (typed or not) is of that
// day; recorded is true when any prompt line is. The days are visited by
// date, not by 24 h steps, so the days the clocks change have one row each.
// Two aggregating reads cover the whole span, the prompts and the used turns.
func calendar(e env) ([]CalendarDay, error) {
	loc := viewLoc(e)
	now := e.now.In(loc)
	y, m, d := now.Date()
	// The same day 13 months ago (its month's last day when it has none), plus a day.
	bm := time.Date(y, m-calendarMonths, 1, 0, 0, 0, 0, loc)
	bd := min(d, time.Date(bm.Year(), bm.Month()+1, 0, 0, 0, 0, 0, loc).Day())
	back := time.Date(bm.Year(), bm.Month(), bd+1, 0, 0, 0, 0, loc)
	start := time.Date(back.Year(), back.Month(), back.Day()-(int(back.Weekday())+6)%7, 0, 0, 0, 0, loc) // the Monday on or before
	end := time.Date(y, m, d+1, 0, 0, 0, 0, loc)

	var out []CalendarDay
	rowOf := map[int64]int{} // a local date's number (days since 1970-01-01) to its row
	for day := start; day.Before(end); day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc) {
		y, m, d := day.Date()
		rowOf[time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()/86400] = len(out)
		out = append(out, CalendarDay{Day: day.Format(dateLayout)})
	}
	dayOf := localDayExpr(start, end, loc)

	// One grouped read of the prompts: the SQL places each on its local date
	// (the zone's offset is a constant between two changes of the clocks), so
	// a few hundred rows come back instead of every prompt.
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT `+dayOf("ts")+`, COUNT(*),
       SUM(CASE WHEN reason IS NULL OR reason <> 'machine' THEN 1 ELSE 0 END),
       SUM(CASE WHEN (reason IS NULL OR reason <> 'machine') AND decision = 'recall' AND COALESCE(injected, 0) > 0 THEN 1 ELSE 0 END),
       COUNT(DISTINCT NULLIF(session, ''))
FROM tel_prompt WHERE ts >= ? AND ts < ? AND `+projectClause+`
GROUP BY 1`,
		start.UnixMilli(), end.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var date, lines, typed, withMemory, sessions int64
		if err := rows.Scan(&date, &lines, &typed, &withMemory, &sessions); err != nil {
			return nil, err
		}
		if i, ok := rowOf[date]; ok && lines > 0 {
			out[i].Prompts, out[i].PromptsWithMemory, out[i].Sessions, out[i].Recorded = typed, withMemory, sessions, true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	used, err := usedTurnsPerDay(e, start, end, dayOf("first"))
	if err != nil {
		return nil, err
	}
	for date, n := range used {
		if i, ok := rowOf[date]; ok {
			out[i].UsedTurns = n
		}
	}
	if e.merging() {
		if err := calendarOthers(e, start, end, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// localDayExpr returns the SQL expression, for a unix ms column, of the
// number of days since 1970-01-01 of the local date it falls on in loc, for
// instants in [start, end): the column plus the zone's offset, which is
// constant between two changes of the clocks, so one CASE arm per such
// stretch.
func localDayExpr(start, end time.Time, loc *time.Location) func(col string) string {
	const dayMs = 24 * 60 * 60 * 1000
	var arms []string // "WHEN col < until THEN col + offset" arms, then the last offset
	var last int64
	for at := start; ; {
		_, off := at.In(loc).Zone()
		last = int64(off) * 1000
		_, until := at.In(loc).ZoneBounds()
		if until.IsZero() || !until.Before(end) {
			break
		}
		arms = append(arms, fmt.Sprintf("WHEN %%[1]s < %d THEN %%[1]s + %d", until.UnixMilli(), last))
		at = until
	}
	tmpl := "(%[1]s + " + fmt.Sprint(last) + ") / " + fmt.Sprint(dayMs)
	if len(arms) > 0 {
		tmpl = "(CASE " + strings.Join(arms, " ") + " ELSE %[1]s + " + fmt.Sprint(last) + " END) / " + fmt.Sprint(dayMs)
	}
	return func(col string) string { return fmt.Sprintf(tmpl, col) }
}

// usedTurnsPerDay counts, per local date number, the main-agent turns of the
// query's project that declared and credited some memory used, whose
// earliest non-briefing show is in [start, end): mainTurns' definition of
// such a turn, read from the used credits outward rather than from every
// show of the span. day is the SQL expression of a date number over first.
func usedTurnsPerDay(e env, start, end time.Time, day string) (map[int64]int64, error) {
	since, until := start.UnixMilli(), end.UnixMilli()
	rows, err := e.tx.QueryContext(e.ctx, `
WITH u AS (
  SELECT DISTINCT session, turn FROM credits WHERE verdict = 'used' AND session IS NOT NULL AND turn IS NOT NULL),
t AS (
  SELECT session, turn,
         (SELECT MIN(s.ts) FROM shows s WHERE s.session = u.session AND s.turn = u.turn
            AND COALESCE(s.path, '') <> 'briefing' AND COALESCE(s.agent, '') = '') AS first
  FROM u),
f AS (
  SELECT t.first AS first, `+finalMainOutcome("t")+` AS outcome
  FROM t
  WHERE t.first >= ? AND t.first < ? AND `+sessionProject("t")+`)
SELECT `+day+`, COUNT(*) FROM f WHERE outcome = 'declared' GROUP BY 1`,
		since, until, e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int64{}
	for rows.Next() {
		var date, n int64
		if err := rows.Scan(&date, &n); err != nil {
			return nil, err
		}
		out[date] = n
	}
	return out, rows.Err()
}
