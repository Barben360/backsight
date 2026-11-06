package metrics

import (
	"database/sql"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/machines"
)

// The "several PCs" switch: machines "this" (this PC's figures) or "all"
// (merged with the other PCs' published days, ingested into the pc_* tables
// of the index). The views that merge are mergingViews; the verdict and
// every list stay this PC's (assess runs on env.this()).

// mergingViews are the views whose figures merge with the other PCs'
// (overview, quality, cost, pipeline, days, releases). Bank and system have
// nothing to merge; projects, sessions and memories answer this PC's.
var mergingViews = []string{"overview", "quality", "cost", "pipeline", "days", "releases"}

// thisOnlyViews always answer this PC's figures and say so.
var thisOnlyViews = []string{"projects", "sessions", "memories"}

// sharingOn: this PC publishes its figures and reads the others' (feedback
// on and the ledger shared).
func sharingOn(c config.Config) bool { return c.Feedback.Enabled && c.Feedback.ShareLedger }

// machines is the env's switch, "this" when unset.
func (e env) machines() string {
	if e.q.Machines == "all" {
		return "all"
	}
	return "this"
}

// this is e answering this PC alone; it shares e's memo, whose key holds
// the switch.
func (e env) this() env {
	e.q.Machines = "this"
	return e
}

// merging reports whether the figures of this view merge the other PCs':
// all PCs asked for, a view that merges, sharing on.
func (e env) merging() bool {
	return e.q.Machines == "all" && slices.Contains(mergingViews, e.view) && sharingOn(e.src.Config)
}

// others are the other PCs' figures a merged window adds (M1): the sums of
// the days of the counted PCs whose 12:00 UTC is in the window, for one
// binary version (” = the day totals).
type others struct {
	total machines.Figures
	flow  machines.Pair // pipeline.flow, from the day totals only
	// byBucket and flowByBucket are those sums by bucket of w.Buckets(),
	// each day in the bucket holding its noon; nil for a window drawn by
	// the hour (other PCs send days, not hours).
	byBucket     []machines.Figures
	flowByBucket []machines.Pair
	// pcs is how many counted other PCs have a day in the window; 0 means
	// nothing is added and a merged run is this PC's own.
	pcs int
}

type othersKey struct {
	since, until int64
	version      string
}

// othersAt reads pc_days once per (window, version) in the transaction. A
// day D counts in w when D 12:00 UTC is in [since, until): exactly N days in
// an N-day window in every zone. Only counted PCs (pc_docs.at not more than
// 30 days before now, nor more than 24 h after it; stale ones included).
// A window of more than 24 h and at most 25 h (the day page of a 25 h local
// day) can hold two noons: it takes the first.
func othersAt(e env, w Window, version string) (others, error) {
	k := othersKey{w.Since.UnixNano(), w.Until.UnixNano(), version}
	if e.memo != nil {
		if o, ok := e.memo.others[k]; ok {
			return o, nil
		}
	}
	o, err := readOthers(e, w, version)
	if err == nil && e.memo != nil {
		if e.memo.others == nil {
			e.memo.others = map[othersKey]others{}
		}
		e.memo.others[k] = o
	}
	return o, err
}

// maxOneNoon is the longest window that takes a single day (25 h: the
// longest local day).
const maxOneNoon = 25 * time.Hour

func readOthers(e env, w Window, version string) (others, error) {
	lo, hi := w.Since, w.Until
	if l := w.Length(); l > day && l <= maxOneNoon {
		hi = min2(hi, firstNoon(lo).Add(day))
	}
	var buckets []Bucket
	var o others
	if w.Length() > hourlyMax {
		buckets = w.Buckets()
		o.byBucket = make([]machines.Figures, len(buckets))
		o.flowByBucket = make([]machines.Pair, len(buckets))
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT d.machine, d.noon, d.prompts, d.hit_n, d.hit_d, d.used_n, d.used_d, d.cov_n, d.cov_d, d.wrong_n, d.wrong_d,
       d.ctx_chars, d.ctx_sessions, COALESCE(d.flow_n, 0), COALESCE(d.flow_d, 0),
       d.lat0, d.lat1, d.lat2, d.lat3, d.lat4, d.lat5, d.lat6, d.lat7, d.lat8
FROM pc_days d JOIN pc_docs c ON c.machine = d.machine
WHERE d.version = ? AND d.noon >= ? AND d.noon < ? AND c.at >= ? AND c.at <= ?`,
		version, lo.UnixMilli(), hi.UnixMilli()+1,
		e.now.Add(-machines.DropAfter).UnixMilli(), e.now.Add(machines.MaxSkew).UnixMilli())
	if err != nil {
		return others{}, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var machine string
		var noon int64
		var v [22]int64
		dst := []any{&machine, &noon}
		for i := range v {
			dst = append(dst, &v[i])
		}
		if err := rows.Scan(dst...); err != nil {
			return others{}, err
		}
		at := time.UnixMilli(noon)
		if at.Before(lo) || !at.Before(hi) {
			continue
		}
		f := machines.Figures{Prompts: v[0], Hit: machines.Pair{v[1], v[2]}, Used: machines.Pair{v[3], v[4]},
			Coverage: machines.Pair{v[5], v[6]}, Wrong: machines.Pair{v[7], v[8]}, Context: machines.Pair{v[9], v[10]}}
		copy(f.Latency[:], v[13:22])
		flow := machines.Pair{v[11], v[12]}
		seen[machine] = true
		addFigures(&o.total, f)
		o.flow = addPair(o.flow, flow)
		if buckets != nil {
			if i := bucketIndex(buckets, at.In(w.Loc)); i >= 0 {
				addFigures(&o.byBucket[i], f)
				o.flowByBucket[i] = addPair(o.flowByBucket[i], flow)
			}
		}
	}
	o.pcs = len(seen)
	return o, rows.Err()
}

// firstNoon is the first 12:00 UTC at or after t.
func firstNoon(t time.Time) time.Time {
	u := t.UTC()
	n := time.Date(u.Year(), u.Month(), u.Day(), 12, 0, 0, 0, time.UTC)
	if n.Before(u) {
		n = n.AddDate(0, 0, 1)
	}
	return n
}

func addPair(a, b machines.Pair) machines.Pair { return machines.Pair{a[0] + b[0], a[1] + b[1]} }

func addFigures(a *machines.Figures, b machines.Figures) {
	a.Prompts += b.Prompts
	a.Hit, a.Used, a.Coverage = addPair(a.Hit, b.Hit), addPair(a.Used, b.Used), addPair(a.Coverage, b.Coverage)
	a.Wrong, a.Context = addPair(a.Wrong, b.Wrong), addPair(a.Context, b.Context)
	for i := range a.Latency {
		a.Latency[i] += b.Latency[i]
	}
}

// The tags a merged metric carries.
const (
	tagOthersNotFiltered = "others_not_filtered" // a project filter: the other PCs' whole figures are added
	tagSeriesThisPC      = "series_this_pc"      // a window drawn by the hour: the series stay this PC's
	tagStatusThisPC      = "status_this_pc"      // pipeline.flow: the status stays this PC's
)

// mergeFunc returns, for the ids of its family that merge, a merged copy of
// the family's run of this PC over w; it never writes obs or met (the
// memo's) and returns them as they are when nothing is added.
type mergeFunc func(e env, w Window, o others, obs map[string]Observation, met map[string]Metric) (map[string]Observation, map[string]Metric)

// mergers are the families whose runs merge, by name. context and latency
// (family cost) join in Task 9.
var mergers = map[string]mergeFunc{"reach": mergeRates, "quality": mergeRates, "pipeline": mergeFlow}

// rates are the four ratio metrics M2 merges and the pair of a day's
// figures each adds.
var rates = []struct {
	id   string
	pair func(machines.Figures) machines.Pair
}{
	{"recall.hit_rate", func(f machines.Figures) machines.Pair { return f.Hit }},
	{"used.turn_rate", func(f machines.Figures) machines.Pair { return f.Used }},
	{"declare.coverage", func(f machines.Figures) machines.Pair { return f.Coverage }},
	{"verdict.wrong_rate", func(f machines.Figures) machines.Pair { return f.Wrong }},
}

// mergeRates merges M2: numerators and denominators add, the value follows,
// the series add day by day; the interval, MDC, status, previous and delta
// come from the merged observation through the status engine (finish).
func mergeRates(e env, w Window, o others, obs map[string]Observation, met map[string]Metric) (map[string]Observation, map[string]Metric) {
	if o.pcs == 0 {
		return obs, met
	}
	obs2, met2 := maps.Clone(obs), maps.Clone(met)
	for _, r := range rates {
		to, ok := obs[r.id]
		if !ok {
			continue
		}
		mo, mm, ok := mergePair(e, w, to, met[r.id], r.pair(o.total), func(i int) machines.Pair { return r.pair(o.byBucket[i]) })
		if !ok {
			continue
		}
		obs2[r.id], met2[r.id] = mo, mm
	}
	// used.turn_rate: the worst case and the unreliable tag read the merged
	// coverage; pending and the other counts stay this PC's.
	if mo, ok := obs2["used.turn_rate"]; ok && !mo.Absent {
		covNum, covDen := o.total.Coverage[0], o.total.Coverage[1]
		if c, ok := obs["declare.coverage"]; ok && !c.Absent {
			covNum, covDen = covNum+c.Num, covDen+c.Den
		}
		m := met2["used.turn_rate"]
		m.Extra = maps.Clone(m.Extra)
		if m.Extra == nil {
			m.Extra = map[string]any{}
		}
		m.Extra["worst_case"] = ratio(mo.Num, covDen)
		m.Tags = slices.DeleteFunc(slices.Clone(m.Tags), func(t string) bool { return t == tagUnreliable })
		covDef, _ := DefOf("declare.coverage")
		if _, hi := Wilson(covNum, covDen); covDen >= int64(covDef.MinN) && hi < *covDef.Absolute.Watch {
			m.Tags = append(m.Tags, tagUnreliable)
		}
		met2["used.turn_rate"] = m
	}
	return obs2, met2
}

// mergeFlow merges M3: pipeline.flow's numerator, denominator, value and
// series add the other PCs' sent documents; its status stays this PC's
// error-class rule (their failures, set-asides and backlog are not sent), so
// the observation keeps this PC's Err and the metric says so.
func mergeFlow(e env, w Window, o others, obs map[string]Observation, met map[string]Metric) (map[string]Observation, map[string]Metric) {
	to, ok := obs["pipeline.flow"]
	if !ok || o.pcs == 0 {
		return obs, met
	}
	mo, mm, ok := mergePair(e, w, to, met["pipeline.flow"], o.flow, func(i int) machines.Pair { return o.flowByBucket[i] })
	if !ok {
		return obs, met
	}
	mm.Tags = append(mm.Tags, tagStatusThisPC)
	obs2, met2 := maps.Clone(obs), maps.Clone(met)
	obs2["pipeline.flow"], met2["pipeline.flow"] = mo, mm
	return obs2, met2
}

// mergePair adds the pair tot (and bucket(i) in series point i) to one
// rate: this PC's observation and metric, copied. Nothing to add to a
// metric this PC has no data for (absent) leaves it absent with its reason;
// with something to add, this PC's part is empty. ok is false when nothing
// merged.
func mergePair(e env, w Window, to Observation, tm Metric, tot machines.Pair, bucket func(i int) machines.Pair) (Observation, Metric, bool) {
	if to.Absent && tot[1] == 0 {
		return to, tm, false
	}
	num, den := tot[0], tot[1]
	if !to.Absent {
		num, den = num+to.Num, den+to.Den
	}
	mo := to
	mo.Absent, mo.Reason, mo.Num, mo.Den = false, "", num, den
	mm := tm
	mm.Num, mm.Den, mm.Value = i64(num), i64(den), ratio(num, den)
	mm.Tags = slices.Clone(tm.Tags)
	if e.q.Project != "" {
		mm.Tags = append(mm.Tags, tagOthersNotFiltered)
	}
	if w.Length() <= hourlyMax {
		mm.Tags = append(mm.Tags, tagSeriesThisPC)
		return mo, mm, true
	}
	buckets := w.Buckets()
	series := make([]Point, len(buckets))
	for i, b := range buckets {
		p := pointOf(w, b.Key)
		if i < len(tm.Series) {
			p = tm.Series[i]
		}
		var n, d int64
		if p.Num != nil {
			n = *p.Num
		}
		if p.Den != nil {
			d = *p.Den
		}
		add := bucket(i)
		n, d = n+add[0], d+add[1]
		p.Num, p.Den, p.Value = i64(n), i64(d), ratio(n, d)
		series[i] = p
	}
	mm.Series = series
	return mo, mm, true
}

// short is the label of machine id: its first 8 characters, or 13 when
// another counted PC shares them (R47).
func short(id string, ids []string) string {
	if len(id) < 13 {
		return id
	}
	for _, o := range ids {
		if o != id && len(o) >= 8 && o[:8] == id[:8] {
			return id[:13]
		}
	}
	return id[:8]
}

// scopePCs reads the scope of the PCs (M9) and whether at least one other
// PC is counted.
func scopePCs(e env) (*PCs, bool, error) {
	p := &PCs{Sharing: sharingOn(e.src.Config), List: []PCRow{}}
	if !p.Sharing {
		return p, false, nil
	}
	loc := viewLoc(e)
	var self SelfPC
	if e.src.Self != nil {
		self = e.src.Self()
	}
	thisCounts := !e.src.Config.ServerOnly()

	type other struct {
		machine, os, first string
		at                 int64
		until              sql.NullString
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT machine, COALESCE(os, ''), COALESCE(first_seen, ''), at, until_day FROM pc_docs
WHERE at >= ? AND at <= ? ORDER BY first_seen, machine`,
		e.now.Add(-machines.DropAfter).UnixMilli(), e.now.Add(machines.MaxSkew).UnixMilli())
	if err != nil {
		return nil, false, err
	}
	var others []other
	for rows.Next() {
		var o other
		if err := rows.Scan(&o.machine, &o.os, &o.first, &o.at, &o.until); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		others = append(others, o)
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	var ids []string
	if thisCounts && self.Machine != "" {
		ids = append(ids, self.Machine)
	}
	for _, o := range others {
		ids = append(ids, o.machine)
	}

	w := e.q.Window
	if thisCounts {
		row := PCRow{Machine: "", This: true, OS: self.OS, FirstSeen: self.FirstSeen}
		if self.Machine != "" {
			row.Machine = short(self.Machine, ids)
		}
		p.List = append(p.List, row)
		var n int
		if err := e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(*) FROM tel_prompt WHERE ts >= ? AND ts < ? AND (reason IS NULL OR reason <> 'machine')`,
			w.Since.UnixMilli(), w.Until.UnixMilli()).Scan(&n); err != nil {
			return nil, false, err
		}
		if n > 0 {
			p.Active++
		}
	}
	for _, o := range others {
		row := PCRow{Machine: short(o.machine, ids), OS: o.os, FirstSeen: o.first,
			Stale: time.UnixMilli(o.at).Before(e.now.Add(-machines.StaleAfter))}
		if o.until.Valid && o.until.String != "" {
			u := o.until.String
			row.Until = &u
			if p.Until == nil || u < *p.Until {
				p.Until = &u
			}
		}
		if row.Stale {
			p.Stale++
		}
		var n int
		if err := e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(*) FROM pc_days WHERE machine = ? AND version = '' AND prompts > 0 AND noon >= ? AND noon < ?`,
			o.machine, w.Since.UnixMilli(), w.Until.UnixMilli()).Scan(&n); err != nil {
			return nil, false, err
		}
		if n > 0 {
			p.Active++
		}
		p.List = append(p.List, row)
	}
	p.Count = len(p.List)

	if err := e.tx.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM pc_ignored`).Scan(&p.Ignored); err != nil {
		return nil, false, err
	}
	var fetched sql.NullInt64
	switch err := e.tx.QueryRowContext(e.ctx, `SELECT fetched_at FROM pc_fetch WHERE id = 1`).Scan(&fetched); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, false, err
	case fetched.Valid:
		t := time.UnixMilli(fetched.Int64).In(loc)
		p.FetchedAt = &t
	}
	return p, len(others) > 0, nil
}
