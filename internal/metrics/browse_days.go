package metrics

import (
	"context"
	"errors"
	"strings"
	"time"
)

// errBadDay: `days <date>` takes a day as YYYY-MM-DD.
var errBadDay = errors.New("a day is written YYYY-MM-DD")

// Days is the days view: one row per local day of the window (the first and
// last cut by the window), latest day first, paged. A day's counts are
// those of the sessions' activity that day (as the sessions view counts
// them): typed prompts, recalls (typed or not), prompts that got memory,
// the turns where Claude used memory (by the turn's first show), memories
// reported wrong or outdated, characters added, the sessions active, and the
// documents sent ok or failed. With q.ID (YYYY-MM-DD in the window's zone)
// it is that day's page: the six tile metrics over the day and its sessions;
// a day with no data reads zeros, it is not an error.
func Days(ctx context.Context, s Source, q Query) (Result, error) {
	if _, err := checkSort(q, "day", "day"); err != nil {
		return Result{}, err
	}
	if id := strings.TrimSpace(q.ID); id != "" {
		loc := q.Window.Loc
		if loc == nil {
			loc = time.UTC
		}
		start, err := time.ParseInLocation("2006-01-02", id, loc)
		if err != nil {
			return Result{}, errBadDay
		}
		y, m, d := start.Date()
		q.Window = q.Window.span(start, time.Date(y, m, d+1, 0, 0, 0, 0, loc))
		return runView(ctx, s, q, "days", nil, func(e env, r *Result) error {
			var err error
			if r.Metrics, err = computeMetrics(e, e.q.Window, overviewTiles); err != nil {
				return err
			}
			sessions, err := sessionRows(e, e.q.Window)
			if err != nil {
				return err
			}
			sortSessions(sessions)
			lo, hi, page := pageBounds(e.q, len(sessions))
			day := DayPage{Day: id, Sessions: orEmpty(sessions[lo:hi])}
			if err := withSent(e, day.Sessions); err != nil {
				return err
			}
			r.Tables.Day, r.Tables.Page = &day, &page
			return nil
		})
	}
	return runView(ctx, s, q, "days", nil, func(e env, r *Result) error {
		rows, err := dayRows(e, e.q.Window)
		if err != nil {
			return err
		}
		lo, hi, page := pageBounds(e.q, len(rows))
		r.Tables.Days, r.Tables.Page = rows[lo:hi], &page
		return nil
	})
}

// dayRows are the window's local days, latest first. Each day's session
// counts are sessionRows over the day (cut to the window).
func dayRows(e env, w Window) ([]DayRow, error) {
	buckets := w.DayBuckets()
	rows := make([]DayRow, len(buckets))
	for i, b := range buckets {
		rows[i].Day = b.Key
		sessions, err := sessionRows(e, w.span(b.Since, b.Until))
		if err != nil {
			return nil, err
		}
		rows[i].Sessions = int64(len(sessions))
		for _, s := range sessions {
			rows[i].Prompts += s.Prompts
			rows[i].PromptsWithMemory += s.PromptsWithMemory
			rows[i].Wrong += s.Wrong
			rows[i].ContextChars += s.ContextChars
		}
	}
	at := func(ts int64) *DayRow {
		if i := bucketIndex(buckets, time.UnixMilli(ts).In(w.Loc)); i >= 0 {
			return &rows[i]
		}
		return nil
	}
	recalls, err := e.tx.QueryContext(e.ctx, `
SELECT ts FROM tel_prompt WHERE ts >= ? AND ts < ? AND decision = 'recall' AND `+projectClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = recalls.Close() }()
	for recalls.Next() {
		var ts int64
		if err := recalls.Scan(&ts); err != nil {
			return nil, err
		}
		if r := at(ts); r != nil {
			r.Recalls++
		}
	}
	if err := recalls.Err(); err != nil {
		return nil, err
	}
	sends, err := e.tx.QueryContext(e.ctx, `
SELECT ts, status FROM tel_retain WHERE ts >= ? AND ts < ? AND status IN ('ok', 'failed') AND `+projectClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sends.Close() }()
	for sends.Next() {
		var ts int64
		var status string
		if err := sends.Scan(&ts, &status); err != nil {
			return nil, err
		}
		if r := at(ts); r != nil {
			if status == "ok" {
				r.SentOK++
			} else {
				r.SentFailed++
			}
		}
	}
	if err := sends.Err(); err != nil {
		return nil, err
	}
	turns, err := mainTurns(e, w)
	if err != nil {
		return nil, err
	}
	for _, t := range turns {
		if r := at(t.first.UnixMilli()); r != nil && t.outcome == "declared" && t.anyUsed {
			r.UsedTurns++
		}
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 { // latest day first
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}
