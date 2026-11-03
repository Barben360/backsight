package metrics

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// secrets.suspects: what the last memory scan found that looks like a key,
// token or password. doctor.history: each doctor check's status on each
// local day of the window, read from the daily upkeep's runs.

const (
	secretsID       = "secrets.suspects"
	doctorHistoryID = "doctor.history"
	tagOldScan      = "old_scan"
	oldScanDays     = 30 // a scan older than this many days is tagged, not judged
)

func init() {
	register("secrets", []string{secretsID}, secrets)
	register("doctor_history", []string{doctorHistoryID}, doctorHistory)
}

// secrets is the latest scan's count, whatever the window; as of now, so
// only the view's own window reads it.
func secrets(e env, w Window) (map[string]Observation, map[string]Metric, error) {
	absent := func() (map[string]Observation, map[string]Metric, error) {
		return map[string]Observation{secretsID: {Absent: true, Reason: ReasonNoEvents}}, map[string]Metric{secretsID: {}}, nil
	}
	if !isCurrent(e, w) {
		return absent()
	}
	var ts int64
	var count, live, forgotten, pages, ms sql.NullInt64
	err := e.tx.QueryRowContext(e.ctx, `
SELECT ts, count, live, forgotten, pages, ms FROM scans
ORDER BY ts DESC, source_file DESC, "offset" DESC LIMIT 1`).Scan(&ts, &count, &live, &forgotten, &pages, &ms)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !count.Valid) {
		return absent()
	}
	if err != nil {
		return nil, nil, err
	}
	loc := viewLoc(e)
	age := int64(localDays(time.UnixMilli(ts).In(loc), e.now.In(loc)))
	m := Metric{Value: ptr(float64(count.Int64)), Extra: map[string]any{
		"scanned_at": timeOrNil(sql.NullInt64{Int64: ts, Valid: true}, loc), "age_days": age,
		"live": intOrNil(live), "forgotten": intOrNil(forgotten), "pages": intOrNil(pages), "ms": intOrNil(ms)}}
	if age > oldScanDays {
		m.Tags = []string{tagOldScan}
	}
	return map[string]Observation{secretsID: {Err: ruleSecrets(count.Int64)}}, map[string]Metric{secretsID: m}, nil
}

// intOrNil is a nullable count as an Extra value: nil when absent, never 0.
func intOrNil(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// localDays is the number of calendar days from a to b, both in one zone.
func localDays(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return int(time.Date(by, bm, bd, 12, 0, 0, 0, time.UTC).Sub(time.Date(ay, am, ad, 12, 0, 0, 0, time.UTC)) / day)
}

// agoWords says how long ago a scan of age calendar days was.
func agoWords(age int64) string {
	switch age {
	case 0:
		return "today"
	case 1:
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", age)
}

// secretsWhy is the attention item's why: the last scan, when, and what it found.
func secretsWhy(n, age int64) string {
	found := fmt.Sprintf("%s that look like", count(n, "memory", "memories"))
	if n == 1 {
		found = "1 memory that looks like"
	}
	return fmt.Sprintf("The last scan, %s, found %s a key, token or password. Memory is shown to Claude in later sessions; the scan says which ones and how to forget them.", agoWords(age), found)
}

// secretsBanner is the banner's reason: the count and the day of the scan.
func secretsBanner(n int64, scanned time.Time, loc *time.Location) string {
	return fmt.Sprintf("%s in memory (scan of %s).", count(n, "possible secret", "possible secrets"), scanned.In(loc).Format("Jan 2"))
}

// The day statuses of the doctor's history.
const (
	dayOK, dayWatch, dayBad, dayNone = "ok", "watch", "bad", "none"
)

// DayStatuses are the statuses a doctor history day reads.
var DayStatuses = []string{dayOK, dayWatch, dayBad, dayNone}

// doctorDayStatus maps a doctor status to a day's: ok and info read ok,
// warn watch, error bad; "" for any other.
func doctorDayStatus(s string) string {
	switch s {
	case "ok", "info":
		return dayOK
	case "warn":
		return dayWatch
	case "error":
		return dayBad
	}
	return ""
}

// doctorRun is the doctor history over a window: one row per check, the
// runs read and the days no run happened.
type doctorRun struct {
	runs        int64
	daysWithout int64
	rows        []DoctorHistoryRow
}

// doctorDays are the local dates a window covers, first to last.
func doctorDays(w Window) []string {
	loc := w.Loc
	if loc == nil {
		loc = time.UTC
	}
	since, last := w.Since.In(loc), w.Until.In(loc).Add(-time.Nanosecond).Format(dateLayout)
	var out []string
	for d := time.Date(since.Year(), since.Month(), since.Day(), 12, 0, 0, 0, loc); d.Format(dateLayout) <= last; d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(dateLayout))
	}
	return out
}

// readDoctorHistory reads the doctor_runs rows of [since, until) and folds
// them into days, once per window and transaction.
func readDoctorHistory(e env, w Window) (*doctorRun, error) {
	k := [2]int64{w.Since.UnixMilli(), w.Until.UnixMilli()}
	if e.memo != nil {
		if r, ok := e.memo.doctor[k]; ok {
			return r, nil
		}
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT source_file, "offset", ts, check_id, status FROM doctor_runs
WHERE ts >= ? AND ts < ? AND status IS NOT NULL
ORDER BY ts, source_file, "offset", check_id`, k[0], k[1])
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	loc := w.Loc
	if loc == nil {
		loc = time.UTC
	}
	type run struct {
		file   string
		offset int64
	}
	seen := map[run]bool{}
	last := map[string]map[string]string{} // check -> day -> status of the day's last run
	for rows.Next() {
		var r run
		var ts int64
		var check, status string
		if err := rows.Scan(&r.file, &r.offset, &ts, &check, &status); err != nil {
			return nil, err
		}
		st := doctorDayStatus(status)
		if st == "" {
			continue
		}
		seen[r] = true
		if last[check] == nil {
			last[check] = map[string]string{}
		}
		last[check][time.UnixMilli(ts).In(loc).Format(dateLayout)] = st // later runs overwrite earlier ones
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := &doctorRun{runs: int64(len(seen)), rows: []DoctorHistoryRow{}}
	days := doctorDays(w)
	withRun := map[string]bool{}
	for _, byDay := range last {
		for d := range byDay {
			withRun[d] = true
		}
	}
	out.daysWithout = int64(len(days) - len(withRun))
	for check, byDay := range last {
		row := DoctorHistoryRow{Check: check, Now: dayNone, Days: make([]DoctorDay, len(days))}
		prev := ""
		for i, d := range days {
			st := byDay[d]
			if st == "" {
				row.Days[i] = DoctorDay{Day: d, Status: dayNone}
				continue
			}
			row.Days[i] = DoctorDay{Day: d, Status: st}
			switch st {
			case dayOK:
				row.OK++
			case dayWatch:
				row.Watch++
			case dayBad:
				row.Bad++
			}
			if prev != "" && prev != st {
				row.LastChange = &DoctorChange{Day: d, From: prev, To: st}
			}
			prev, row.Now = st, st
		}
		out.rows = append(out.rows, row)
	}
	slices.SortFunc(out.rows, func(a, b DoctorHistoryRow) int {
		if fa, fb := a.Watch+a.Bad > 0, b.Watch+b.Bad > 0; fa != fb {
			if fa {
				return -1
			}
			return 1
		}
		switch {
		case a.Check < b.Check:
			return -1
		case a.Check > b.Check:
			return 1
		}
		return 0
	})
	if e.memo != nil {
		if e.memo.doctor == nil {
			e.memo.doctor = map[[2]int64]*doctorRun{}
		}
		e.memo.doctor[k] = out
	}
	return out, nil
}

func doctorHistory(e env, w Window) (map[string]Observation, map[string]Metric, error) {
	r, err := readDoctorHistory(e, w)
	if err != nil {
		return nil, nil, err
	}
	if r.runs == 0 {
		return map[string]Observation{doctorHistoryID: {Absent: true, Reason: ReasonNoEvents}}, map[string]Metric{doctorHistoryID: {}}, nil
	}
	return map[string]Observation{doctorHistoryID: {Num: r.runs, Den: r.runs}},
		map[string]Metric{doctorHistoryID: {Value: ptr(float64(r.runs)), Num: ptr(r.runs), Den: ptr(r.runs),
			Extra: map[string]any{"days_without": r.daysWithout}}}, nil
}
