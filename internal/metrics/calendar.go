package metrics

import (
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
// Two reads cover the whole span, the prompts and the turns.
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
	index := map[string]int{}
	for day := start; day.Before(end); day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc) {
		key := day.Format(dateLayout)
		index[key] = len(out)
		out = append(out, CalendarDay{Day: key})
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT ts, COALESCE(session, ''),
       CASE WHEN reason IS NULL OR reason <> 'machine' THEN 1 ELSE 0 END,
       CASE WHEN (reason IS NULL OR reason <> 'machine') AND decision = 'recall' AND COALESCE(injected, 0) > 0 THEN 1 ELSE 0 END
FROM tel_prompt WHERE ts >= ? AND ts < ? AND `+projectClause,
		start.UnixMilli(), end.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type daySession struct {
		day     int
		session string
	}
	seen := map[daySession]bool{}
	for rows.Next() {
		var ts, typed, withMemory int64
		var session string
		if err := rows.Scan(&ts, &session, &typed, &withMemory); err != nil {
			return nil, err
		}
		i, ok := index[time.UnixMilli(ts).In(loc).Format(dateLayout)]
		if !ok {
			continue
		}
		out[i].Prompts += typed
		out[i].PromptsWithMemory += withMemory
		out[i].Recorded = true
		if k := (daySession{i, session}); session != "" && !seen[k] {
			seen[k] = true
			out[i].Sessions++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	turns, err := mainTurns(e, Window{Since: start, Until: end, Loc: loc})
	if err != nil {
		return nil, err
	}
	for _, t := range turns {
		if i, ok := index[t.first.In(loc).Format(dateLayout)]; ok && t.outcome == "declared" && t.anyUsed {
			out[i].UsedTurns++
		}
	}
	return out, nil
}
