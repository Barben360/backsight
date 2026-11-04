package metrics

import (
	"database/sql"
	"math"
	"slices"
	"strings"
	"time"
)

// Gates of a comparison (R23): a stratum needs this many distinct active
// days and units in each version; a stratum needs sessions on both sides
// to have a clustered interval, the pooled comparison this many per
// version.
const (
	compareMinDays     = 7
	compareMinUnits    = 30
	compareMinSessions = 2
	comparePooledG     = 10
	retryDays          = 6
)

// CompareMeasures are the measures --compare reads, the default first.
var CompareMeasures = []string{"used.turn_rate", "recall.hit_rate", "verdict.wrong_rate"}

// cmpUnit is one observation of a measure: a declared turn that showed
// memory (hit: a used credit), a prompt recall (hit: it injected), or a
// verdict (hit: wrong or outdated), with the version, the stratum (the
// session's project and the path of the turn's earliest non-briefing
// show), the session it belongs to and when it happened.
type cmpUnit struct {
	version, project, path, session string
	at                              time.Time
	hit                             bool
}

// Compare compares two binary versions on measure (used.turn_rate when
// empty) over the query's window: the units of each version are grouped
// into strata and pooled as compareOf says; the settings in effect under
// each version are read from the config records. A version without a unit
// in the window is a too_few comparison, not an error.
func Compare(e env, versions [2]string, measure string) (Comparison, error) {
	if measure == "" {
		measure = CompareMeasures[0]
	}
	loc := viewLocOf(e.q.Window)
	units, err := compareUnits(e, versions, measure)
	if err != nil {
		return Comparison{}, err
	}
	c := compareOf(units, versions[0], versions[1], measure, loc)
	c.SettingsDiffer, c.SettingsUnknown, err = compareSettings(e, units, versions)
	return c, err
}

// pathRankSQL orders the paths of two shows at the same instant.
const pathRankSQL = `CASE s.path WHEN 'prompt' THEN 0 WHEN 'gate' THEN 1 WHEN 'search' THEN 2 WHEN 'read' THEN 3 WHEN 'failure' THEN 4 WHEN 'sources' THEN 5 ELSE 6 END`

// turnPathSQL is the path of the earliest non-briefing show of the main
// agent in the turn of the row aliased a, ties in pathRankSQL's order.
func turnPathSQL(a string) string {
	return `COALESCE((SELECT s.path FROM shows s WHERE s.session = ` + a + `.session AND s.turn = ` + a + `.turn
    AND COALESCE(s.agent, '') = '' AND COALESCE(s.path, '') <> 'briefing' AND COALESCE(s.path, '') <> ''
  ORDER BY s.ts, ` + pathRankSQL + `, s.path LIMIT 1), 'unknown')`
}

// turnAttrSQL is column (project or binary_version) of the turn of the row
// aliased a.
func turnAttrSQL(a, col string) string {
	return `COALESCE((SELECT MIN(t.` + col + `) FROM turns t WHERE t.session = ` + a + `.session AND t.turn = ` + a + `.turn), '')`
}

// compareUnits reads the units of measure whose version is one of the two
// asked, in the window; the bounds are in the SQL.
func compareUnits(e env, versions [2]string, measure string) ([]cmpUnit, error) {
	w := e.q.Window
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	var rows *sql.Rows
	var err error
	switch measure {
	case "recall.hit_rate":
		// A prompt with no session id reads as session '': all of them
		// share one cluster (the cautious reading: a larger variance).
		rows, err = e.tx.QueryContext(e.ctx, `
SELECT COALESCE(session, ''), COALESCE(project, ''), binary_version, ts, 'prompt', COALESCE(injected, 0) > 0
FROM tel_prompt
WHERE decision = 'recall' AND ts >= ? AND ts < ? AND binary_version IN (?, ?) AND `+projectClause,
			since, until, versions[0], versions[1], e.q.Project, e.q.Project)
	case "verdict.wrong_rate":
		rows, err = e.tx.QueryContext(e.ctx, `
WITH c AS (
  SELECT c.session, c.turn, c.verdict,
         COALESCE((SELECT MIN(s.ts) FROM shows s WHERE s.session = c.session AND s.turn = c.turn AND s.memory_id = c.memory_id), c.ts) AS at
  FROM credits c
  WHERE c.memory_id IS NOT NULL AND c.verdict IN ('used', 'wrong', 'outdated') AND c.ts >= ?)
SELECT COALESCE(c.session, ''), `+turnAttrSQL("c", "project")+`, `+turnAttrSQL("c", "binary_version")+`, c.at, `+turnPathSQL("c")+`, c.verdict <> 'used'
FROM c WHERE c.at >= ? AND c.at < ?`, since, since, until)
	default: // used.turn_rate
		rows, err = e.tx.QueryContext(e.ctx, `
WITH g AS (
  SELECT session, turn, MIN(ts) AS at FROM shows
  WHERE COALESCE(agent, '') = '' AND COALESCE(path, '') <> 'briefing' AND ts < ?
  GROUP BY session, turn HAVING MIN(ts) >= ?)
SELECT COALESCE(g.session, ''), `+turnAttrSQL("g", "project")+`, `+turnAttrSQL("g", "binary_version")+`, g.at, `+turnPathSQL("g")+`,
       EXISTS (SELECT 1 FROM credits c WHERE c.session = g.session AND c.turn = g.turn AND c.verdict = 'used')
FROM g JOIN turn_final_stop f ON f.session = g.session AND f.turn = g.turn AND COALESCE(f.agent, '') = '' AND f.outcome = 'declared'`,
			until, since)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []cmpUnit
	for rows.Next() {
		var u cmpUnit
		var at int64
		if err := rows.Scan(&u.session, &u.project, &u.version, &at, &u.path, &u.hit); err != nil {
			return nil, err
		}
		if u.version != versions[0] && u.version != versions[1] {
			continue
		}
		if measure == "used.turn_rate" || measure == "verdict.wrong_rate" {
			if at < since || at >= until || (e.q.Project != "" && u.project != e.q.Project) {
				continue
			}
		}
		u.at = time.UnixMilli(at)
		out = append(out, u)
	}
	return out, rows.Err()
}

// side is one version's units in a stratum.
type side struct {
	units []cmpUnit
	hits  int64
	days  map[string]bool
	sess  map[string]bool
	first string // first active day, YYYY-MM-DD
}

func (s *side) n() int64 { return int64(len(s.units)) }

// p is the success rate; only read for a side with units.
func (s *side) p() float64 { return float64(s.hits) / float64(len(s.units)) }

type stratum struct {
	project, path string
	v             [2]*side
	eligible      bool
	why           string
	w             float64
}

// compareOf is the comparison of the units of versions a and b: strata
// (project, path), each eligible when both versions have compareMinDays
// distinct active days (in loc) and compareMinUnits units in it and
// compareMinSessions sessions; weights w = nA nB / (nA + nB); pooled
// difference D = sum(w d) / sum(w); the variance of D clusters the units
// by session (a session's units are summed over the strata, G/(G-1)
// corrected); the interval is D +- 1.96 SE, [D, D] when the variance is
// zero. The comparison is shown when a stratum is eligible and each
// version has comparePooledG sessions in the eligible strata; otherwise
// it is too_few.
func compareOf(units []cmpUnit, a, b, measure string, loc *time.Location) Comparison {
	c := Comparison{A: a, B: b, Measure: measure, Strata: []StratumRow{}, SettingsDiffer: []SettingDiff{}}
	byKey := map[[2]string]*stratum{}
	var raw [2]Rate
	for _, u := range units {
		vi := 0
		switch u.version {
		case a:
		case b:
			vi = 1
		default:
			continue
		}
		raw[vi].Den++
		k := [2]string{u.project, u.path}
		s := byKey[k]
		if s == nil {
			s = &stratum{project: u.project, path: u.path}
			for i := range s.v {
				s.v[i] = &side{days: map[string]bool{}, sess: map[string]bool{}}
			}
			byKey[k] = s
		}
		sd := s.v[vi]
		sd.units = append(sd.units, u)
		day := u.at.In(loc).Format("2006-01-02")
		sd.days[day] = true
		if sd.first == "" || day < sd.first {
			sd.first = day
		}
		sd.sess[u.session] = true
		if u.hit {
			sd.hits++
			raw[vi].Num++
		}
	}
	c.RawA, c.RawB = raw[0], raw[1]
	strata := make([]*stratum, 0, len(byKey))
	for _, s := range byKey {
		strata = append(strata, s)
	}
	slices.SortFunc(strata, func(x, y *stratum) int {
		if r := strings.Compare(x.project, y.project); r != 0 {
			return r
		}
		return strings.Compare(x.path, y.path)
	})

	var eligible []*stratum
	for _, s := range strata {
		s.why = s.gate()
		if s.why == "" {
			s.eligible = true
			s.w = float64(s.v[0].n()) * float64(s.v[1].n()) / float64(s.v[0].n()+s.v[1].n())
			eligible = append(eligible, s)
		}
	}

	for _, s := range strata {
		row := StratumRow{Project: s.project, Path: s.path, Eligible: s.eligible, Why: s.why, Reading: "too_few",
			A: StratumSide{s.v[0].hits, s.v[0].n(), len(s.v[0].days)}, B: StratumSide{s.v[1].hits, s.v[1].n(), len(s.v[1].days)}}
		if s.eligible {
			d, variance := pooled([]*stratum{s})
			row.Weight, row.Diff = ptr(s.w), ptr(d)
			row.CI, row.Reading = interval(d, variance)
		}
		c.Strata = append(c.Strata, row)
	}

	var sessions [2]map[string]bool
	sessions[0], sessions[1] = map[string]bool{}, map[string]bool{}
	for _, s := range eligible {
		for i := range sessions {
			for k := range s.v[i].sess {
				sessions[i][k] = true
			}
		}
	}
	c.SessionsA, c.SessionsB = len(sessions[0]), len(sessions[1])

	if len(eligible) > 0 && c.SessionsA >= comparePooledG && c.SessionsB >= comparePooledG {
		d, variance := pooled(eligible)
		var pa, pb, wsum float64
		for _, s := range eligible {
			pa += s.w * s.v[0].p()
			pb += s.w * s.v[1].p()
			wsum += s.w
		}
		c.Diff, c.PooledA, c.PooledB = ptr(d), ptr(pa/wsum), ptr(pb/wsum)
		var ci *[2]float64
		ci, c.Reading = interval(d, variance)
		c.CI = ci
		if c.RawA.Den > 0 && c.RawB.Den > 0 {
			rawDiff := float64(c.RawB.Num)/float64(c.RawB.Den) - float64(c.RawA.Num)/float64(c.RawA.Den)
			c.RawDisagrees = sign(rawDiff) != sign(d)
		}
		return c
	}
	c.Reading = "too_few"
	c.TooFew = tooFew(strata, eligible, c, a, b, loc)
	return c
}

func sign(f float64) int {
	switch {
	case f > 0:
		return 1
	case f < 0:
		return -1
	}
	return 0
}

// gate is the first gate the stratum misses (days, units, sessions), ""
// when it is eligible.
func (s *stratum) gate() string {
	for _, sd := range s.v {
		if len(sd.days) < compareMinDays {
			return "days"
		}
	}
	for _, sd := range s.v {
		if sd.n() < compareMinUnits {
			return "units"
		}
	}
	for _, sd := range s.v {
		if len(sd.sess) < compareMinSessions {
			return "sessions"
		}
	}
	return ""
}

// pooled is the weighted difference of the strata (weights s.w, all
// eligible) and the cluster-robust variance of that difference. D = sum
// c_i y_i with c_i = w_s / (W n_Bs) for a B unit and -w_s / (W n_As) for an
// A unit; Var = G/(G-1) sum_g (sum_{i in g} c_i
// (y_i - p_sv))^2 over the sessions g of the strata.
func pooled(ss []*stratum) (d, variance float64) {
	var wsum float64
	for _, s := range ss {
		wsum += s.w
	}
	sums := map[string]float64{}
	for _, s := range ss {
		d += s.w * (s.v[1].p() - s.v[0].p()) / wsum
		for vi, sd := range s.v {
			c := s.w / (wsum * float64(sd.n()))
			if vi == 0 {
				c = -c
			}
			p := sd.p()
			for _, u := range sd.units {
				y := 0.0
				if u.hit {
					y = 1
				}
				sums[u.session] += c * (y - p)
			}
		}
	}
	g := len(sums)
	if g < 2 {
		return d, 0
	}
	var sq float64
	for _, v := range sums {
		sq += v * v
	}
	return d, sq * float64(g) / float64(g-1)
}

// interval is D +- 1.96 SE and its reading: [D, D] when the variance is
// zero (every unit of each version alike in each stratum).
func interval(d, variance float64) (*[2]float64, string) {
	if variance <= 0 || math.IsNaN(variance) || math.IsInf(variance, 0) {
		reading := "no_clear_difference"
		if d != 0 {
			reading = "differs"
		}
		return &[2]float64{d, d}, reading
	}
	se := math.Sqrt(variance)
	ci := [2]float64{d - 1.96*se, d + 1.96*se}
	if ci[0] > 0 || ci[1] < 0 {
		return &ci, "differs"
	}
	return &ci, "no_clear_difference"
}

// tooFew names the first gate unmet in the stratum with the most units of
// the scarcer version (among the eligible ones when some are, for a
// comparison held back by its sessions), and the version that misses it.
func tooFew(strata, eligible []*stratum, c Comparison, a, b string, loc *time.Location) *TooFew {
	pool := strata
	if len(eligible) > 0 {
		pool = eligible
	}
	names := [2]string{a, b}
	if len(pool) == 0 {
		return &TooFew{Version: a, Why: "days"}
	}
	best := pool[0]
	for _, s := range pool[1:] {
		if min(s.v[0].n(), s.v[1].n()) > min(best.v[0].n(), best.v[1].n()) {
			best = s
		}
	}
	why := best.why
	if len(eligible) > 0 {
		why = "sessions"
	}
	// the version with the least of what the gate counts (A on a tie)
	count := func(i int) int {
		switch why {
		case "days":
			return len(best.v[i].days)
		case "units":
			return int(best.v[i].n())
		}
		if len(eligible) > 0 {
			return [2]int{c.SessionsA, c.SessionsB}[i]
		}
		return len(best.v[i].sess)
	}
	vi := 0
	if count(1) < count(0) {
		vi = 1
	}
	t := &TooFew{Version: names[vi], Days: len(best.v[vi].days), Units: int(best.v[vi].n()), Why: why}
	if why == "days" && best.v[vi].first != "" {
		if d, err := time.ParseInLocation("2006-01-02", best.v[vi].first, loc); err == nil {
			t.RetryAfter = ptr(d.AddDate(0, 0, retryDays))
		}
	}
	return t
}

// compareSettings is the numeric settings whose value is not one single
// value over the two versions' spans. A version's spans' records are the
// one in effect at the start of its units' span (the latest at or before
// it, else the first one up to the span's end) and every record inside the
// span; unknown when a version has no unit or no such record. A setting is
// flagged when those records, A's and B's together, do not all give it the
// same value (absent counts as a value). A and B are the values at the
// end of each span; ChangedAt is the first record of the chain from the
// earliest to the latest of those records that gives the setting a value
// other than the previous record's.
func compareSettings(e env, units []cmpUnit, versions [2]string) ([]SettingDiff, bool, error) {
	out := []SettingDiff{}
	var first, last [2]int64
	for _, u := range units {
		vi := 0
		if u.version == versions[1] {
			vi = 1
		}
		ms := u.at.UnixMilli()
		if first[vi] == 0 || ms < first[vi] {
			first[vi] = ms
		}
		if ms > last[vi] {
			last[vi] = ms
		}
	}
	var spans [2][]cfgAt
	lo, maxTs := cfgRec{}, int64(0)
	for i := range spans {
		if first[i] == 0 {
			return out, true, nil
		}
		r, ok, err := configInEffect(e, first[i], last[i])
		if err != nil || !ok {
			return out, true, err
		}
		recs := []cfgRec{r}
		after, err := configAfter(e, r, last[i])
		if err != nil {
			return nil, false, err
		}
		recs = append(recs, after...)
		for _, x := range recs {
			v, err := configValuesOf(e, x)
			if err != nil {
				return nil, false, err
			}
			spans[i] = append(spans[i], cfgAt{rec: x, values: v})
		}
		if lo == (cfgRec{}) || r.before(lo) {
			lo = r
		}
		maxTs = max(maxTs, recs[len(recs)-1].ts)
	}
	// the values each key takes over the records of the two spans
	type val struct {
		has bool
		v   float64
	}
	seen := map[string]map[val]bool{}
	for _, sp := range spans {
		for _, r := range sp {
			for k := range r.values {
				if seen[k] == nil {
					seen[k] = map[val]bool{}
				}
			}
		}
	}
	for k := range seen {
		for _, sp := range spans {
			for _, r := range sp {
				v, has := r.values[k]
				seen[k][val{has, v}] = true
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for k, vals := range seen {
		if len(vals) > 1 {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return out, false, nil
	}
	slices.Sort(keys)
	// the chain of records from the earliest to the latest, for ChangedAt
	lv, err := configValuesOf(e, lo)
	if err != nil {
		return nil, false, err
	}
	chain := []cfgAt{{rec: lo, values: lv}}
	rest, err := configAfter(e, lo, maxTs)
	if err != nil {
		return nil, false, err
	}
	for _, r := range rest {
		v, err := configValuesOf(e, r)
		if err != nil {
			return nil, false, err
		}
		chain = append(chain, cfgAt{rec: r, values: v})
	}
	endOf := func(i int) map[string]float64 { return spans[i][len(spans[i])-1].values }
	for _, k := range keys {
		d := SettingDiff{Key: k}
		if v, ok := endOf(0)[k]; ok {
			d.A = ptr(v)
		}
		if v, ok := endOf(1)[k]; ok {
			d.B = ptr(v)
		}
		for i := 1; i < len(chain); i++ {
			pv, phas := chain[i-1].values[k]
			v, has := chain[i].values[k]
			if has != phas || v != pv {
				d.ChangedAt = ptr(time.UnixMilli(chain[i].rec.ts).In(viewLocOf(e.q.Window)))
				break
			}
		}
		out = append(out, d)
	}
	return out, false, nil
}

// cfgRec identifies a config record.
type cfgRec struct {
	ts   int64
	file string
	off  int64
}

func (r cfgRec) before(o cfgRec) bool {
	if r.ts != o.ts {
		return r.ts < o.ts
	}
	if r.file != o.file {
		return r.file < o.file
	}
	return r.off < o.off
}

// configInEffect is the latest config record at or before start, else the
// first one up to end.
func configInEffect(e env, start, end int64) (cfgRec, bool, error) {
	var r cfgRec
	err := e.tx.QueryRowContext(e.ctx, `SELECT ts, source_file, "offset" FROM tel_config WHERE ts <= ?
ORDER BY ts DESC, source_file DESC, "offset" DESC LIMIT 1`, start).Scan(&r.ts, &r.file, &r.off)
	if err == nil {
		return r, true, nil
	}
	if err != sql.ErrNoRows {
		return r, false, err
	}
	err = e.tx.QueryRowContext(e.ctx, `SELECT ts, source_file, "offset" FROM tel_config WHERE ts <= ?
ORDER BY ts, source_file, "offset" LIMIT 1`, end).Scan(&r.ts, &r.file, &r.off)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	return r, err == nil, err
}

func configValuesOf(e env, r cfgRec) (map[string]float64, error) {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT key, value FROM tel_config_values WHERE source_file = ? AND "offset" = ?`, r.file, r.off)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	m := map[string]float64{}
	for rows.Next() {
		var k string
		var v float64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

type cfgAt struct {
	rec    cfgRec
	values map[string]float64
}

// configAfter are the config records after lo up to time hiTs, in time
// order.
func configAfter(e env, lo cfgRec, hiTs int64) ([]cfgRec, error) {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT ts, source_file, "offset" FROM tel_config
WHERE (ts, source_file, "offset") > (?, ?, ?) AND ts <= ?
ORDER BY ts, source_file, "offset"`, lo.ts, lo.file, lo.off, hiTs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var recs []cfgRec
	for rows.Next() {
		var r cfgRec
		if err := rows.Scan(&r.ts, &r.file, &r.off); err != nil {
			return nil, err
		}
		recs = append(recs, r)
	}
	return recs, rows.Err()
}
