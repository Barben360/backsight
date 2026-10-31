package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// The no_data reasons a metric can carry.
const (
	ReasonNoEvents          = "no_events"
	ReasonFeedbackOff       = "feedback_off"
	ReasonServerOnlyHost    = "server_only_host"
	ReasonServerDown        = "server_down"
	ReasonServerUnsupported = "server_unsupported"
)

// tagNotFiltered marks a server-scope metric under a project filter: the
// server's numbers cover every project.
const tagNotFiltered = "not_filtered"

// env is one view's run: its read transaction, source, query and clock,
// the server data read before the transaction, the view's server section
// (nil when it reads nothing from the server) and what the transaction
// already computed.
type env struct {
	ctx    context.Context
	tx     *sql.Tx
	src    Source
	q      Query
	now    time.Time
	srv    *serverData
	server *ServerState
	memo   *memo
}

// memo is what one view's transaction already computed, so that a view
// and its verdict, which read many overlapping windows (a window, the one
// before, the four 7-day baselines, the same again for the previous
// window), run each family once per window: the index does not change
// within the transaction, nor the server data or the clock. It assumes one
// Query per env: of the query only the window may change between runs
// (assess judges the last 7 days with the view's other fields), and the
// key holds the window; an env must never be reused for another project,
// path or machines, whose figures would be served from this memo.
type memo struct {
	runs      map[runKey]familyRun
	backlog   *time.Duration
	docStatus map[string]string    // document_status
	stale     *feedback.StaleLists // feedback.Stale of the index's ledger, as of now
	ledger    *feedback.Ledger     // the index's ledger
	projects  map[string]string    // every memory's project (memoryProjects)
}

// newMemo is an empty memo.
func newMemo() *memo { return &memo{runs: map[runKey]familyRun{}} }

// runKey is one family run: the window read and the view's own window
// (figures as of now are read only for the latter).
type runKey struct {
	family                       string
	since, until, qSince, qUntil int64
}

type familyRun struct {
	obs map[string]Observation
	met map[string]Metric
}

// run is fam over w, computed once per transaction. Callers read the maps
// and never write them.
func (e env) run(fam *family, w Window) (map[string]Observation, map[string]Metric, error) {
	if e.memo == nil {
		return fam.f(e, w)
	}
	k := runKey{fam.name, w.Since.UnixNano(), w.Until.UnixNano(), e.q.Window.Since.UnixNano(), e.q.Window.Until.UnixNano()}
	if r, ok := e.memo.runs[k]; ok {
		return r.obs, r.met, nil
	}
	obs, met, err := fam.f(e, w)
	if err == nil {
		e.memo.runs[k] = familyRun{obs, met}
	}
	return obs, met, err
}

// backlogAge is backlogAgeOf as of e.now, read once per transaction.
func (e env) backlogAge() (time.Duration, error) {
	if e.memo != nil && e.memo.backlog != nil {
		return *e.memo.backlog, nil
	}
	status, err := e.documentStatus()
	if err != nil {
		return 0, err
	}
	age, err := backlogAgeOf(e.tx, e.now, status)
	if err == nil && e.memo != nil {
		e.memo.backlog = &age
	}
	return age, err
}

// computeFunc computes a family's metrics over one window: each id's
// observation (what Judge reads) and its metric (value, num, den, series,
// extra, tags); computeMetrics judges them.
type computeFunc func(e env, w Window) (map[string]Observation, map[string]Metric, error)

// family is one registered family: its ids and how to compute them.
type family struct {
	name string
	ids  []string
	f    computeFunc
}

// registry maps each catalog id to the family that computes it.
type registry struct{ byID map[string]*family }

// families is the registry the family files fill from their init().
var families = &registry{byID: map[string]*family{}}

// register adds a family's ids to the registry; called from init() in each
// family file. An id outside the catalog or registered twice panics.
func register(name string, ids []string, f computeFunc) { families.register(name, ids, f) }

func (r *registry) register(name string, ids []string, f computeFunc) {
	fam := &family{name: name, ids: slices.Clone(ids), f: f}
	for _, id := range ids {
		if _, ok := DefOf(id); !ok {
			panic(fmt.Sprintf("metrics: family %s registers %q, which is not in the catalog", name, id))
		}
		if old, ok := r.byID[id]; ok {
			panic(fmt.Sprintf("metrics: %q is registered by both %s and %s", id, old.name, name))
		}
		r.byID[id] = fam
	}
}

// computeMetrics computes ids over w inside e's transaction, each family
// once per window it needs: w, w.Previous() (previous value and delta)
// and, when one of its ids is relative, the four 7-day w.Baselines(). It
// fills Status, Reason, Borderline, CI, MDC (rates with Den ≥ min_n),
// Previous (when the previous window has Den ≥ min_n), Delta (value −
// previous value, when both windows have Den ≥ min_n), Drill and Tags, and
// marks the series points of w's partial buckets. An id no family
// registered yet reads no_data, no_events.
func computeMetrics(e env, w Window, ids []string) (map[string]Metric, error) {
	return families.compute(e, w, ids)
}

func (r *registry) compute(e env, w Window, ids []string) (map[string]Metric, error) {
	out := make(map[string]Metric, len(ids))
	var order []*family
	asked := map[*family][]Def{}
	for _, id := range ids {
		d, ok := DefOf(id)
		if !ok {
			return nil, fmt.Errorf("metrics: unknown metric %q", id)
		}
		fam := r.byID[id]
		if fam == nil {
			out[id] = finish(d, Observation{Absent: true, Reason: ReasonNoEvents}, Metric{}, nil, e.q)
			continue
		}
		if _, seen := asked[fam]; !seen {
			order = append(order, fam)
		}
		asked[fam] = append(asked[fam], d)
	}
	for _, fam := range order {
		defs := asked[fam]
		obs, met, err := e.run(fam, w)
		if err != nil {
			return nil, fmt.Errorf("metrics: %s: %w", fam.name, err)
		}
		prevObs, prev, err := e.run(fam, w.Previous())
		if err != nil {
			return nil, fmt.Errorf("metrics: %s, previous window: %w", fam.name, err)
		}
		var bases []map[string]Observation
		if slices.ContainsFunc(defs, func(d Def) bool { return d.Relative }) {
			for _, b := range w.Baselines() {
				bo, _, err := e.run(fam, b)
				if err != nil {
					return nil, fmt.Errorf("metrics: %s, baseline window: %w", fam.name, err)
				}
				bases = append(bases, bo)
			}
		}
		for _, d := range defs {
			o, ok := obs[d.ID]
			if !ok {
				return nil, fmt.Errorf("metrics: family %s computed no %s", fam.name, d.ID)
			}
			if d.Relative && o.Baseline == nil {
				o.Baseline = baselineOf(d.ID, bases)
			}
			var p *Metric
			po, computed := prevObs[d.ID]
			if m, ok := prev[d.ID]; ok && computed && enough(d, po) {
				p = &m
			}
			out[d.ID] = finish(d, o, met[d.ID], p, e.q)
		}
	}
	markPartial(w, out)
	return out, nil
}

// markPartial flags the day and hour points of ms whose bucket w covers
// only in part (its first and last, cut by the window's edges), so a
// per-day count is never read as a dip.
func markPartial(w Window, ms map[string]Metric) {
	partial := map[string]bool{}
	for _, b := range w.Buckets() {
		if b.Partial {
			partial[b.Key] = true
		}
	}
	if len(partial) == 0 {
		return
	}
	for _, m := range ms {
		for i := range m.Series {
			p := &m.Series[i]
			if k := p.Day + p.Hour; k != "" && partial[k] {
				p.Partial = true
			}
		}
	}
}

// baselineOf is id's point value and denominator in each baseline window,
// newest first; a window without the id, or absent, counts no trials.
func baselineOf(id string, bases []map[string]Observation) []BaselineValue {
	out := make([]BaselineValue, 0, len(bases))
	for _, b := range bases {
		o, ok := b[id]
		if !ok || o.Absent || o.Den <= 0 {
			out = append(out, BaselineValue{})
			continue
		}
		num := max(0, min(o.Num, o.Den))
		out = append(out, BaselineValue{Value: float64(num) / float64(o.Den), Den: o.Den})
	}
	return out
}

// enough reports whether an observation of d has the trials d needs to be
// read as a figure: not absent, Den ≥ min_n (for a percentile row, min_n
// is the n of the percentile shown).
func enough(d Def, o Observation) bool { return !o.Absent && o.Den >= int64(d.MinN) }

// finish judges one metric and fills what every metric carries. prev is
// the previous window's metric, nil when that window has too few trials;
// the delta needs this window to have enough too.
func finish(d Def, o Observation, m Metric, prev *Metric, q Query) Metric {
	j := Judge(d, o)
	m.Status, m.Reason, m.Borderline, m.CI = j.Status, j.Reason, j.Borderline, j.CI
	if m.Unit == "" {
		m.Unit = d.Unit
	}
	if m.Aggregation == "" {
		m.Aggregation = d.Aggregation
	}
	m.MDC = nil
	if d.Unit == "ratio" && !o.Absent && o.Den > 0 && o.Den >= int64(d.MinN) {
		v := MDC(o.Den)
		m.MDC = &v
	}
	m.Previous, m.Delta = nil, nil
	if prev != nil && prev.Value != nil {
		p := *prev.Value
		m.Previous = &p
		if m.Value != nil && enough(d, o) {
			delta := *m.Value - p
			m.Delta = &delta
		}
	}
	m.Drill = d.Drill
	m.Series = slices.Clone(m.Series) // the family run's own stays as computed (memo)
	tags := make([]string, 0, len(m.Tags)+1)
	tags = append(tags, m.Tags...)
	if d.Scope == "server" && q.Project != "" && !o.Filtered && !slices.Contains(tags, tagNotFiltered) {
		tags = append(tags, tagNotFiltered)
	}
	m.Tags = tags
	return m
}

// errNoIndex: a source without an index cannot run a view.
var errNoIndex = errors.New("metrics: the source has no index")

// withTx runs a view for q: the server reads first (loadServer, over
// q.Window), then fn in one read transaction of the index, where the
// server state's LastOK is read from local_status.
func withTx(ctx context.Context, src Source, q Query, need []string, fn func(e env) error) error {
	if src.Index == nil {
		return errNoIndex
	}
	w := q.Window
	srv, state := loadServer(ctx, src, w, need)
	now := src.now()
	return src.Index.ReadTx(ctx, func(tx *sql.Tx) error {
		if state != nil {
			last, err := reachLastOK(ctx, tx, w.Loc)
			if err != nil {
				return err
			}
			state.LastOK = last
		}
		return fn(env{ctx: ctx, tx: tx, src: src, q: q, now: now, srv: srv, server: state, memo: newMemo()})
	})
}

// reachLastOK is local_status.reach_last_ok in loc, nil when unknown.
func reachLastOK(ctx context.Context, tx *sql.Tx, loc *time.Location) (*time.Time, error) {
	var ms sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT reach_last_ok FROM local_status WHERE id = 1`).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !ms.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if loc == nil {
		loc = time.UTC
	}
	t := time.UnixMilli(ms.Int64).In(loc)
	return &t, nil
}
