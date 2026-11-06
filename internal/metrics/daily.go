package metrics

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/machines"
)

// Daily is this PC's figures for every complete UTC day before at's UTC date, at most machines.MaxDays of them
// ending the day before, from the first day with a row; days whose figures are all 0 are left out; nil on a server-only host.
//
// The ratios are the families' own daily series (reach, quality, pipeline) over one UTC window, so their
// definitions cannot drift from the views'. The typed prompts, the context and the latency come from two
// queries below that keep the WHERE clauses of costSessions and costRecallLatency. Each valid binary version
// runs the ratio families once over its own span (its first to last prompt, whole UTC days).
func Daily(ctx context.Context, s Source, at time.Time) ([]machines.Day, error) {
	if s.Config.ServerOnly() {
		return nil, nil
	}
	if s.Index == nil {
		return nil, errNoIndex
	}
	y, m, d := at.UTC().Date()
	until := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	var out []machines.Day
	err := s.Index.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = dailyIn(env{ctx: ctx, tx: tx, src: s, now: at}, until)
		return err
	})
	return out, err
}

// dayWindow is the UTC window [since, until), long enough that the families bucket it by day.
func dayWindow(since, until time.Time) Window {
	if until.Sub(since) <= hourlyMax {
		since = until.Add(-hourlyMax - day)
	}
	return Window{Since: since, Until: until, TZ: "UTC", Loc: time.UTC, Days: ceilDays(until.Sub(since))}
}

// pairs are the four ratio pairs of one day, from the families' series.
type pairs struct{ hit, used, cov, wrong machines.Pair }

// dayAcc counts what the two queries read for one day.
type dayAcc struct {
	machines.Figures
	seen map[string]bool              // valid versions with a prompt row that day
	vers map[string]*machines.Figures // valid versions' own counts
}

func (a *dayAcc) of(version string) *machines.Figures {
	if !machines.ValidVersion(version) {
		return nil
	}
	f := a.vers[version]
	if f == nil {
		f = &machines.Figures{}
		a.vers[version] = f
	}
	return f
}

func dailyIn(e env, until time.Time) ([]machines.Day, error) {
	first, err := firstRow(e)
	if err != nil || first.IsZero() {
		return nil, err
	}
	start := first.UTC().Truncate(day)
	if lo := until.AddDate(0, 0, -machines.MaxDays); start.Before(lo) {
		start = lo
	}
	if !start.Before(until) {
		return nil, nil
	}
	w := dayWindow(start, until)
	e.q.Window = w

	accs, spans, err := readDaily(e, start, until)
	if err != nil {
		return nil, err
	}
	total, flow, err := ratioSeries(e, w, true)
	if err != nil {
		return nil, err
	}
	byVersion := map[string]map[string]pairs{}
	for v, sp := range spans {
		vw := dayWindow(sp[0], sp[1])
		ve := e
		ve.q.version, ve.q.Window = v, vw
		if byVersion[v], _, err = ratioSeries(ve, vw, false); err != nil {
			return nil, err
		}
	}

	var out []machines.Day
	for _, b := range w.DayBuckets() {
		a := accs[b.Key]
		if a == nil {
			a = &dayAcc{}
		}
		d := machines.Day{Day: b.Key, Figures: a.Figures, Flow: flow[b.Key]}
		p := total[b.Key]
		d.Hit, d.Used, d.Coverage, d.Wrong = p.hit, p.used, p.cov, p.wrong
		if !nonZero(d.Figures) && d.Flow == (machines.Pair{}) {
			continue
		}
		type entry struct {
			name string
			f    machines.Figures
		}
		var es []entry
		names := map[string]bool{}
		for v := range a.seen {
			names[v] = true
		}
		for v := range byVersion {
			if _, ok := byVersion[v][b.Key]; ok {
				names[v] = true
			}
		}
		for v := range names {
			f := machines.Figures{}
			if c := a.vers[v]; c != nil {
				f = *c
			}
			vp := byVersion[v][b.Key]
			f.Hit, f.Used, f.Coverage, f.Wrong = vp.hit, vp.used, vp.cov, vp.wrong
			if a.seen[v] || nonZero(f) {
				es = append(es, entry{v, f})
			}
		}
		slices.SortFunc(es, func(x, y entry) int {
			if x.f.Prompts != y.f.Prompts {
				return int(y.f.Prompts - x.f.Prompts)
			}
			return strings.Compare(x.name, y.name)
		})
		if len(es) > machines.MaxVersions {
			es = es[:machines.MaxVersions]
		}
		if len(es) > 0 {
			d.Versions = make(map[string]machines.Figures, len(es))
			for _, x := range es {
				d.Versions[x.name] = x.f
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func nonZero(f machines.Figures) bool {
	if f.Prompts != 0 || f.Hit != (machines.Pair{}) || f.Used != (machines.Pair{}) || f.Coverage != (machines.Pair{}) ||
		f.Wrong != (machines.Pair{}) || f.Context != (machines.Pair{}) {
		return true
	}
	for _, n := range f.Latency {
		if n != 0 {
			return true
		}
	}
	return false
}

// firstRow is the time of the earliest row the figures read, zero when there is none.
func firstRow(e env) (time.Time, error) {
	var ms sql.NullInt64
	err := e.tx.QueryRowContext(e.ctx, `
SELECT MIN(t) FROM (
  SELECT MIN(ts) AS t FROM tel_prompt UNION ALL SELECT MIN(ts) FROM tel_briefing UNION ALL
  SELECT MIN(ts) FROM tel_retain UNION ALL SELECT MIN(ts) FROM shows UNION ALL SELECT MIN(ts) FROM credits)`).Scan(&ms)
	if err != nil || !ms.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(ms.Int64), nil
}

// ratioSeries runs the families' ratios over w (for e.q.version) and returns their per-day pairs, and with
// withFlow the per-day pipeline.flow pairs (the pipeline family has no version filter). A day a family has no
// point for (it read no_data) is absent.
func ratioSeries(e env, w Window, withFlow bool) (map[string]pairs, map[string]machines.Pair, error) {
	out := map[string]pairs{}
	set := func(m Metric, put func(p *pairs, v machines.Pair)) {
		for _, pt := range m.Series {
			if pt.Num == nil || pt.Den == nil {
				continue
			}
			p := out[pt.Day]
			put(&p, machines.Pair{*pt.Num, *pt.Den})
			out[pt.Day] = p
		}
	}
	if _, none, err := reachAbsent(e, "tel_prompt"); err != nil {
		return nil, nil, err
	} else if !none {
		obs, met := map[string]Observation{}, map[string]Metric{}
		if err := reachPrompts(e, w, obs, met); err != nil {
			return nil, nil, err
		}
		set(met["recall.hit_rate"], func(p *pairs, v machines.Pair) { p.hit = v })
	}
	ids := []string{"used.turn_rate", "declare.coverage", "verdict.wrong_rate"}
	_, met, err := e.runIDs(families.byID[ids[0]], w, ids)
	if err != nil {
		return nil, nil, err
	}
	set(met["used.turn_rate"], func(p *pairs, v machines.Pair) { p.used = v })
	set(met["declare.coverage"], func(p *pairs, v machines.Pair) { p.cov = v })
	set(met["verdict.wrong_rate"], func(p *pairs, v machines.Pair) { p.wrong = v })

	flow := map[string]machines.Pair{}
	if withFlow {
		obs, met := map[string]Observation{}, map[string]Metric{}
		if err := pipelineRetain(e, w, 0, obs, met); err != nil {
			return nil, nil, err
		}
		for _, pt := range met["pipeline.flow"].Series {
			if pt.Num != nil && pt.Den != nil {
				flow[pt.Day] = machines.Pair{*pt.Num, *pt.Den}
			}
		}
	}
	return out, flow, nil
}

// readDaily reads the prompts and briefings of [since, until) once: per UTC day the typed prompts, the
// context and the latency counts (in total and per valid version), and per valid version the span of its
// prompts as whole UTC days. A session's characters count on the day of each event; the session itself on
// the day of its first event, for the version of that event.
func readDaily(e env, since, until time.Time) (map[string]*dayAcc, map[string][2]time.Time, error) {
	accs := map[string]*dayAcc{}
	acc := func(ts int64) *dayAcc {
		k := time.UnixMilli(ts).UTC().Format("2006-01-02")
		a := accs[k]
		if a == nil {
			a = &dayAcc{seen: map[string]bool{}, vers: map[string]*machines.Figures{}}
			accs[k] = a
		}
		return a
	}
	type first struct {
		ts      int64
		version string
	}
	sessions := map[string]first{}
	touch := func(session string, ts int64, version string) {
		if f, ok := sessions[session]; !ok || ts < f.ts {
			sessions[session] = first{ts, version}
		}
	}
	spans := map[string][2]time.Time{}

	// Typed prompts (the Releases view's n), context and the recall latency: the WHERE clauses of
	// costSessions (a session, not a machine prompt) and costRecallLatency (a recall with ms).
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT ts, COALESCE(session, ''), COALESCE(chars, 0), decision, ms, reason IS NULL OR reason <> 'machine', COALESCE(binary_version, '')
FROM tel_prompt WHERE ts >= ? AND ts < ?`, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ts, chars int64
		var session, version string
		var decision sql.NullString
		var ms sql.NullInt64
		var typed bool
		if err := rows.Scan(&ts, &session, &chars, &decision, &ms, &typed, &version); err != nil {
			return nil, nil, err
		}
		a := acc(ts)
		v := a.of(version)
		figs := []*machines.Figures{&a.Figures}
		if v != nil {
			figs = append(figs, v)
			a.seen[version] = true
			at := time.UnixMilli(ts).UTC().Truncate(day)
			sp, ok := spans[version]
			if !ok || at.Before(sp[0]) {
				sp[0] = at
			}
			if !ok || at.After(sp[1]) {
				sp[1] = at
			}
			spans[version] = sp
		}
		for _, f := range figs {
			if typed {
				f.Prompts++
			}
			if decision.String == "recall" && ms.Valid {
				f.Latency[machines.Bucket(ms.Int64)]++
			}
			if typed && session != "" {
				f.Context[0] += chars
			}
		}
		if typed && session != "" {
			touch(session, ts, version)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	b, err := e.tx.QueryContext(e.ctx, `
SELECT ts, session, COALESCE(chars, 0), COALESCE(binary_version, '') FROM tel_briefing
WHERE ts >= ? AND ts < ? AND COALESCE(session, '') <> ''`, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = b.Close() }()
	for b.Next() {
		var ts, chars int64
		var session, version string
		if err := b.Scan(&ts, &session, &chars, &version); err != nil {
			return nil, nil, err
		}
		a := acc(ts)
		a.Context[0] += chars
		if v := a.of(version); v != nil {
			v.Context[0] += chars
		}
		touch(session, ts, version)
	}
	if err := b.Err(); err != nil {
		return nil, nil, err
	}
	for _, f := range sessions {
		a := acc(f.ts)
		a.Context[1]++
		if v := a.of(f.version); v != nil {
			v.Context[1]++
		}
	}
	for v, sp := range spans {
		spans[v] = [2]time.Time{max2(sp[0], since), min2(sp[1].Add(day), until)}
	}
	return accs, spans, nil
}
