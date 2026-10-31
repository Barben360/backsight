package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
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
// and the server data read before the transaction.
type env struct {
	ctx context.Context
	tx  *sql.Tx
	src Source
	q   Query
	now time.Time
	srv *serverData
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
// Previous, Delta (value − previous value), Drill and Tags. An id no
// family registered yet reads no_data, no_events.
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
		obs, met, err := fam.f(e, w)
		if err != nil {
			return nil, fmt.Errorf("metrics: %s: %w", fam.name, err)
		}
		_, prev, err := fam.f(e, w.Previous())
		if err != nil {
			return nil, fmt.Errorf("metrics: %s, previous window: %w", fam.name, err)
		}
		var bases []map[string]Observation
		if slices.ContainsFunc(defs, func(d Def) bool { return d.Relative }) {
			for _, b := range w.Baselines() {
				bo, _, err := fam.f(e, b)
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
			if m, ok := prev[d.ID]; ok {
				p = &m
			}
			out[d.ID] = finish(d, o, met[d.ID], p, e.q)
		}
	}
	return out, nil
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

// finish judges one metric and fills what every metric carries.
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
		if m.Value != nil {
			delta := *m.Value - p
			m.Delta = &delta
		}
	}
	m.Drill = d.Drill
	tags := make([]string, 0, len(m.Tags)+1)
	tags = append(tags, m.Tags...)
	if d.Scope == "server" && q.Project != "" && !slices.Contains(tags, tagNotFiltered) {
		tags = append(tags, tagNotFiltered)
	}
	m.Tags = tags
	return m
}

// errNoIndex: a source without an index cannot run a view.
var errNoIndex = errors.New("metrics: the source has no index")

// withTx runs a view: the server reads first (loadServer), then fn in one
// read transaction of the index, where the server state's LastOK is read
// from local_status. e.q holds w only: the caller sets its filters.
func withTx(ctx context.Context, src Source, need []string, w Window, fn func(e env) error) error {
	if src.Index == nil {
		return errNoIndex
	}
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
		return fn(env{ctx: ctx, tx: tx, src: src, q: Query{Window: w}, now: now, srv: srv})
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
