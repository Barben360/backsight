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
	// byMachine is the same sums per PC (By PC, M8).
	byMachine map[string]machines.Figures
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
	o.byMachine = map[string]machines.Figures{}
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
		pc := o.byMachine[machine]
		addFigures(&pc, f)
		o.byMachine[machine] = pc
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
	tagStatusThisPC      = "status_this_pc"      // pipeline.flow, context.per_session: the status stays this PC's
	tagAverage           = "average"             // context.per_session: total characters over total sessions
	tagApproximate       = "approximate"         // recall.latency: a range from merged buckets
)

// mergeFunc returns, for the ids of its family that merge, a merged copy of
// the family's run of this PC over w; it never writes obs or met (the
// memo's) and returns them as they are when nothing is added.
type mergeFunc func(e env, w Window, o others, obs map[string]Observation, met map[string]Metric) (map[string]Observation, map[string]Metric)

// mergers are the families whose runs merge, by name.
var mergers = map[string]mergeFunc{"reach": mergeRates, "quality": mergeRates, "pipeline": mergeFlow, "cost": mergeCost}

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

// mergeCost merges the cost family's two figures that merge: context per
// session (M4) and recall latency (M5). The others stay this PC's.
func mergeCost(e env, w Window, o others, obs map[string]Observation, met map[string]Metric) (map[string]Observation, map[string]Metric) {
	if o.pcs == 0 {
		return obs, met
	}
	obs2, met2 := maps.Clone(obs), maps.Clone(met)
	if to, ok := obs["context.per_session"]; ok {
		if mo, mm, ok := mergeContext(e, o, to, met["context.per_session"]); ok {
			obs2["context.per_session"], met2["context.per_session"] = mo, mm
		}
	}
	if to, ok := obs["recall.latency"]; ok {
		if mo, mm, ok := mergeLatency(e, w, o, to, met["recall.latency"]); ok {
			obs2["recall.latency"], met2["recall.latency"] = mo, mm
		}
	}
	return obs2, met2
}

// mergeContext is M4: the value is the total characters over the total
// sessions of the PCs (num and den). The observation stays this PC's (its
// p95 and watch line) so the status is this PC's; the series, histogram and
// percentiles too. A session of another PC that began before the window adds
// its characters but not its count (R7).
func mergeContext(e env, o others, to Observation, tm Metric) (Observation, Metric, bool) {
	chars, sessions := o.total.Context[0], o.total.Context[1]
	if to.Absent && chars == 0 && sessions == 0 {
		return to, tm, false
	}
	mo := to
	if !to.Absent {
		chars, sessions = chars+tm.chars, sessions+to.Den
	}
	mo.Absent, mo.Reason = false, ""
	mm := tm
	mm.Num, mm.Den, mm.Value = i64(chars), i64(sessions), ratio(chars, sessions)
	mm.Extra = maps.Clone(tm.Extra)
	if mm.Extra == nil {
		mm.Extra = map[string]any{}
	}
	mm.Extra["average"] = ratio(chars, sessions)
	mm.Tags = append(slices.Clone(tm.Tags), tagStatusThisPC, tagAverage)
	if e.q.Project != "" {
		mm.Tags = append(mm.Tags, tagOthersNotFiltered)
	}
	return mo, mm, true
}

// edgeOf is the upper edge of a latency bucket as the status engine reads
// it: the bucket's own edge, 5001 for the open one (so that strictly above
// 5000 is bad whatever the thresholds).
func edgeOf(bucket int) float64 {
	if _, hi := machines.Range(bucket); hi != nil {
		return float64(*hi)
	}
	return float64(machines.Edges[len(machines.Edges)-1] + 1)
}

// rangeOf is the (lo, hi] of a latency bucket in milliseconds, hi null for
// the open one.
func rangeOf(bucket int) *[2]*int64 {
	lo, hi := machines.Range(bucket)
	return &[2]*int64{&lo, hi}
}

// bucketRanges are the typical (p50) and slowest-1-in-20 (p95) ranges of
// the counts b, each null below the samples it needs (as p50p95).
func bucketRanges(b [9]int64) (p50, p95 *[2]*int64) {
	var n int64
	for _, c := range b {
		n += c
	}
	if n >= minForP50 {
		if i, ok := machines.Rank(b, 50); ok {
			p50 = rangeOf(i)
		}
	}
	if n >= MinForP95 {
		if i, ok := machines.Rank(b, 95); ok {
			p95 = rangeOf(i)
		}
	}
	return p50, p95
}

// mergeLatency is M5: the PCs' samples counted by bucket, the p95 and p50
// read by nearest rank, the value the p95 bucket's upper edge. The
// observation carries that edge (5001 for the open bucket) and no Recent, so
// the status engine gives the exact word at the limits and skips the 24 h
// recovery rule.
func mergeLatency(e env, w Window, o others, to Observation, tm Metric) (Observation, Metric, bool) {
	var b [9]int64
	if !to.Absent {
		b = tm.buckets
	}
	var n int64
	for i := range b {
		b[i] += o.total.Latency[i]
		n += b[i]
	}
	if n == 0 {
		return to, tm, false
	}
	mo := to
	mo.Absent, mo.Reason, mo.Den, mo.P95, mo.Recent, mo.RecentN = false, "", n, nil, nil, 0
	mm := tm
	mm.Den, mm.Value = i64(n), nil
	mm.Extra = maps.Clone(tm.Extra)
	if mm.Extra == nil {
		mm.Extra = map[string]any{}
	}
	p50r, p95r := bucketRanges(b)
	if i, ok := machines.Rank(b, 95); ok && p95r != nil {
		edge := edgeOf(i)
		mo.P95 = &edge
		v := float64(min(int64(edge), machines.Edges[len(machines.Edges)-1]))
		mm.Value = &v
	}
	mm.Extra["range_ms"], mm.Extra["p50_range_ms"], mm.Extra["buckets"] = p95r, p50r, b
	mm.Tags = append(slices.Clone(tm.Tags), tagApproximate)
	if e.q.Project != "" {
		mm.Tags = append(mm.Tags, tagOthersNotFiltered)
	}
	if w.Length() <= hourlyMax {
		mm.Tags = append(mm.Tags, tagSeriesThisPC)
		return mo, mm, true
	}
	buckets := w.Buckets()
	series := make([]Point, len(buckets))
	for i, bk := range buckets {
		p := pointOf(w, bk.Key)
		if i < len(tm.Series) {
			p = tm.Series[i]
		}
		day := p.buckets
		for j := range day {
			day[j] += o.byBucket[i].Latency[j]
		}
		var dn int64
		for _, c := range day {
			dn += c
		}
		p.N, p.ThisP95, p.P50, p.P95, p.Lo, p.Hi = i64(dn), p.P95, nil, nil, nil, nil
		if r, ok := machines.Rank(day, 95); ok {
			lo, hi := machines.Range(r)
			l := float64(lo)
			p.Lo = &l
			if hi != nil {
				h := float64(*hi)
				p.Hi = &h
			}
		}
		series[i] = p
	}
	mm.Series = series
	return mo, mm, true
}

// byPC is the By PC table (M8) of the cost view on all PCs: this PC first
// with its exact p95 and p50 (m is the view's metrics, whose extra keeps
// them), then the other counted PCs by first seen with the ranges of their
// buckets in the window.
func byPC(e env, w Window, m map[string]Metric) ([]ByPCRow, error) {
	o, err := othersAt(e, w, e.q.version)
	if err != nil {
		return nil, err
	}
	type pc struct {
		machine, os string
		at          int64
		until       sql.NullString
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT machine, COALESCE(os, ''), at, until_day FROM pc_docs
WHERE at >= ? AND at <= ? ORDER BY first_seen, machine`,
		e.now.Add(-machines.DropAfter).UnixMilli(), e.now.Add(machines.MaxSkew).UnixMilli())
	if err != nil {
		return nil, err
	}
	var pcs []pc
	for rows.Next() {
		var p pc
		if err := rows.Scan(&p.machine, &p.os, &p.at, &p.until); err != nil {
			_ = rows.Close()
			return nil, err
		}
		pcs = append(pcs, p)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var self SelfPC
	if e.src.Self != nil {
		self = e.src.Self()
	}
	var ids []string
	thisCounts := !e.src.Config.ServerOnly()
	if thisCounts && self.Machine != "" {
		ids = append(ids, self.Machine)
	}
	for _, p := range pcs {
		ids = append(ids, p.machine)
	}
	var out []ByPCRow
	if thisCounts {
		row := ByPCRow{This: true, OS: self.OS}
		if self.Machine != "" {
			row.Machine = short(self.Machine, ids)
		}
		if err := e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(*) FROM tel_prompt WHERE ts >= ? AND ts < ? AND (reason IS NULL OR reason <> 'machine') AND `+projectClause+` AND `+versionClause,
			w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project, e.q.version, e.q.version).Scan(&row.Prompts); err != nil {
			return nil, err
		}
		row.Active = row.Prompts > 0
		row.P95, _ = m["recall.latency"].Extra["p95"].(*float64)
		row.P50, _ = m["recall.latency"].Extra["p50"].(*float64)
		out = append(out, row)
	}
	for _, p := range pcs {
		f := o.byMachine[p.machine]
		row := ByPCRow{Machine: short(p.machine, ids), OS: p.os, Prompts: f.Prompts, Active: f.Prompts > 0,
			Stale: time.UnixMilli(p.at).Before(e.now.Add(-machines.StaleAfter))}
		row.P50RangeMS, row.P95RangeMS = bucketRanges(f.Latency)
		if p.until.Valid && p.until.String != "" {
			u := p.until.String
			row.Until = &u
		}
		out = append(out, row)
	}
	return out, nil
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
