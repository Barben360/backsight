package metrics

import (
	"database/sql"
	"slices"
	"time"
)

// The developer metrics on the bank: how much of it ever comes up
// (bank.active_share) and how unevenly the shows fall on the memories
// (shows.concentration, with the table of the most shown). Both read the
// journal's shows (the shows table holds every path, the answers from
// sources included, and no import).

const (
	activeShareID   = "bank.active_share"
	concentrationID = "shows.concentration"
	topShownRows    = 10
)

// shownCount is one memory and how often it was shown in a window.
type shownCount struct {
	id string
	n  int64
}

// showCounts are the shows of each memory shown in w (the project filter is
// the memory's), the most shown first, ties by id.
func showCounts(e env, w Window) ([]shownCount, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT memory_id, COUNT(*) FROM shows
WHERE memory_id IS NOT NULL AND memory_id <> '' AND `+notPage("memory_id")+` AND ts >= ? AND ts < ? AND `+projectClause+`
GROUP BY memory_id ORDER BY COUNT(*) DESC, memory_id`,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []shownCount
	for rows.Next() {
		var c shownCount
		if err := rows.Scan(&c.id, &c.n); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// gini is the Gini index of the counts (any order): 0 when all are equal,
// toward 1 when one takes everything. G = 2·Σ i·x_i / (n·Σx) − (n+1)/n over
// the counts sorted ascending, i from 1.
func gini(counts []int64) float64 {
	xs := slices.Clone(counts)
	slices.Sort(xs)
	var weighted, sum float64
	for i, x := range xs {
		weighted += float64(i+1) * float64(x)
		sum += float64(x)
	}
	n := float64(len(xs))
	return 2*weighted/(n*sum) - (n+1)/n
}

// bankConcentration fills shows.concentration.
func bankConcentration(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	if e.src.Config.ServerOnly() {
		obs[concentrationID], met[concentrationID] = Observation{Absent: true, Reason: ReasonServerOnlyHost}, Metric{}
		return nil
	}
	counts, err := showCounts(e, w)
	if err != nil {
		return err
	}
	if len(counts) == 0 {
		obs[concentrationID], met[concentrationID] = Observation{Absent: true, Reason: ReasonNoEvents}, Metric{}
		return nil
	}
	var all, top int64
	ns := make([]int64, len(counts))
	for i, c := range counts {
		all += c.n
		ns[i] = c.n
		if i < topShownRows {
			top += c.n
		}
	}
	g := gini(ns)
	obs[concentrationID] = Observation{Den: int64(len(counts))}
	met[concentrationID] = Metric{Value: ptr(g), Extra: map[string]any{
		"gini": g, "top10_share": float64(top) / float64(all), "shows": all}}
	return nil
}

// ratioAtMostOne is num/den, at most 1: the count of unresolved memories
// is counted as older, so it can exceed the bank measured.
func ratioAtMostOne(num, den int64) *float64 {
	v := ratio(num, den)
	if v != nil {
		*v = min(*v, 1)
	}
	return v
}

// bankActiveShare fills bank.active_share: of the bank as it was when the
// window began, the share that came up in it. The numerator is the distinct
// memories shown, less those created from the window's first UTC date on
// (day precision: the day lists give no hour). A shown memory without a
// day-list row counts as older, unless the record covers its first show's
// day, when it is gone (forgotten before it was listed: no longer in the
// bank the denominator measures).
func bankActiveShare(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	absent := func(reason string) error {
		obs[activeShareID], met[activeShareID] = Observation{Absent: true, Reason: reason}, Metric{}
		return nil
	}
	if e.src.Config.ServerOnly() {
		return absent(ReasonServerOnlyHost)
	}
	for _, k := range []string{"stats", "series"} {
		if reason := serverReason(e, k); reason != "" {
			return absent(reason)
		}
	}
	if !e.q.Window.Since.Equal(w.Since) || !e.q.Window.Until.Equal(w.Until) { // the server's figures are as of now
		return absent(ReasonNoEvents)
	}
	if !e.srv.Stats.Present["nodes_by_fact_type"] {
		return absent(ReasonServerUnsupported)
	}
	den, err := bankAtSince(e, w)
	if err != nil {
		return err
	}
	covered, err := coveredUntil(e)
	if err != nil {
		return err
	}
	sinceDay := w.Since.UTC().Format(dateLayout)
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT d.memory_id IS NOT NULL, d.created_day, (SELECT MIN(f.ts) FROM shows f WHERE f.memory_id = c.memory_id)
FROM (SELECT DISTINCT memory_id FROM shows WHERE memory_id IS NOT NULL AND memory_id <> '' AND `+notPage("memory_id")+` AND ts >= ? AND ts < ?) c
LEFT JOIN memory_doc d ON d.memory_id = c.memory_id
ORDER BY c.memory_id`, w.Since.UnixMilli(), w.Until.UnixMilli())
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var num, unresolved, gone int64
	var shown bool
	for rows.Next() {
		var has bool
		var created sql.NullString
		var first int64
		if err := rows.Scan(&has, &created, &first); err != nil {
			return err
		}
		shown = true
		switch {
		case has:
			if !created.Valid || created.String < sinceDay {
				num++
			}
		case goneBy(covered, first):
			gone++
		default:
			num++
			unresolved++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !shown {
		return absent(ReasonNoEvents)
	}
	m := Metric{Num: i64(num), Den: i64(den), Value: ratioAtMostOne(num, den),
		Extra: map[string]any{"unresolved": unresolved, "gone": gone}}
	if unresolved > 0 {
		m.Tags = []string{tagPartial}
	}
	if e.q.Project != "" { // the bank's size is the whole bank's
		m.Tags = append(m.Tags, tagNotFiltered)
	}
	obs[activeShareID], met[activeShareID] = Observation{Num: num, Den: den}, m
	return nil
}

// bankAtSince is the bank's size when the window began: the server's total
// now less the memories created from the window's first UTC date on (the
// series' days, then the record's for the days before its first one).
func bankAtSince(e env, w Window) (int64, error) {
	var total int64
	for _, n := range e.srv.Stats.NodesByFactType {
		total += int64(n)
	}
	sinceDay := w.Since.UTC().Format(dateLayout)
	var firstDay string
	for _, b := range e.srv.Series.Buckets {
		d := b.Time.UTC().Format(dateLayout)
		if firstDay == "" || d < firstDay {
			firstDay = d
		}
		if d >= sinceDay {
			total -= b.World + b.Experience + b.Observation
		}
	}
	if firstDay != "" && firstDay > sinceDay {
		from, _ := time.Parse(dateLayout, sinceDay)
		to, _ := time.Parse(dateLayout, firstDay)
		rows, err := readRecordedNew(e, from.UnixMilli(), to.UnixMilli())
		if err != nil {
			return 0, err
		}
		for _, r := range rows {
			total -= r.total()
		}
	}
	return max(0, total), nil
}

// topShown is the memories shown most in w: their project (the latest show's
// carrying one), shows, share of all the shows and credits of verdict used.
func topShown(e env, w Window) ([]TopShownRow, error) {
	counts, err := showCounts(e, w)
	if err != nil {
		return nil, err
	}
	var all int64
	for _, c := range counts {
		all += c.n
	}
	out := []TopShownRow{}
	for _, c := range counts[:min(len(counts), topShownRows)] {
		r := TopShownRow{Memory: c.id, Shown: c.n, Share: float64(c.n) / float64(all)}
		var project sql.NullString
		if err := e.tx.QueryRowContext(e.ctx, `
SELECT (SELECT sh.project FROM shows sh WHERE sh.memory_id = ? AND sh.project IS NOT NULL
          ORDER BY sh.ts DESC, sh.source_file DESC, sh."offset" DESC, sh.pos DESC LIMIT 1),
       (SELECT COUNT(*) FROM credits c WHERE c.memory_id = ? AND c.verdict = 'used' AND c.ts >= ? AND c.ts < ?)`,
			c.id, c.id, w.Since.UnixMilli(), w.Until.UnixMilli()).Scan(&project, &r.Used); err != nil {
			return nil, err
		}
		r.Project = project.String
		out = append(out, r)
	}
	return out, nil
}
