package metrics

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// The page views: each asks the server only for what it shows, then
// computes everything (every window, the verdict) in one read transaction
// of the index, and returns a Result whose lists are never null.

// Views are the view names, the page views first.
var Views = []string{"overview", "quality", "cost", "bank", "pipeline", "system", "releases", "projects", "sessions", "memories", "days", "defs"}

// ErrUnknownView: Run was given a name that is not a view it serves.
var ErrUnknownView = errors.New("unknown view")

// Run computes the view named view.
func Run(ctx context.Context, view string, s Source, q Query) (Result, error) {
	switch view {
	case "overview":
		return Overview(ctx, s, q)
	case "quality":
		return Quality(ctx, s, q)
	case "cost":
		return Cost(ctx, s, q)
	case "bank":
		return Bank(ctx, s, q)
	case "pipeline":
		return Pipeline(ctx, s, q)
	case "system":
		return System(ctx, s, q)
	}
	return Result{}, fmt.Errorf("%w %q", ErrUnknownView, view)
}

// overviewTiles are the overview's six tiles, the only metrics of the
// overview drawn with their series.
var overviewTiles = []string{"recall.hit_rate", "used.turn_rate", "verdict.wrong_rate", "context.per_session", "recall.latency", "pipeline.flow"}

// staleIDs are the review-queue counts.
var staleIDs = []string{"stale.never_used", "stale.hidden", "stale.reported"}

// latelyRows is how many sessions the overview lists.
const latelyRows = 5

// overviewIDs are the overview's metrics: every axis's, the bank tiles
// and the review-queue counts.
func overviewIDs() []string {
	var ids []string
	for _, axis := range axisOrder {
		ids = append(ids, axisMetrics[axis]...)
	}
	return append(ids, append([]string{"bank.size", "bank.growth"}, staleIDs...)...)
}

// overviewNeeds are the overview's server reads: what Flowing judges (the
// queue, the failed and completed operations, the consolidation's stats,
// the doctor), the bank tiles (the size's read, the time series) and the
// summary's extraction cost (all operations' LLM stats, no split).
func overviewNeeds(q Query) []string {
	return append(sizeKeys(q), "stats", "series", "queue", "failed", "completed", "doctor", "llm:")
}

// Overview is "is my memory working?": the verdict on the last 7 days, the
// summary, the tiles, the attention list, the latest sessions.
func Overview(ctx context.Context, s Source, q Query) (Result, error) {
	return runView(ctx, s, q, "overview", overviewNeeds(q), func(e env, r *Result) error {
		a, err := assess(e)
		if err != nil {
			return err
		}
		ids := append(overviewIDs(), "extraction.cost") // the cost is the summary's only
		m, reused := a.metricsFor(e.q, ids)
		if !reused {
			if m, err = computeMetrics(e, e.q.Window, ids); err != nil {
				return err
			}
		}
		sum := summary(m)
		delete(m, "extraction.cost")
		for id, met := range m {
			if !slices.Contains(overviewTiles, id) {
				met.Series = nil
			}
			if a.Right28 != nil && slices.Contains(axisMetrics["right"], id) {
				met.Tags = append(slices.Clone(met.Tags), tagJudged28d)
			}
			m[id] = met
		}
		v := a.Verdict
		r.Verdict, r.Summary, r.Metrics, r.Info = &v, &sum, m, a.Info
		r.Attention = a.Items
		if len(r.Attention) > attentionShown {
			r.Attention, r.AttentionMore = r.Attention[:attentionShown], len(r.Attention)-attentionShown
		}
		act, err := activity(e, e.q.Window)
		if err != nil {
			return err
		}
		r.Tables.Activity = &act
		r.Tables.Lately, err = lately(e, e.q.Window)
		return err
	})
}

// Quality is "is the memory shown right?": the reach and quality metrics
// (the developer ones too), the outcomes of the shows per day, the wrong
// and outdated reports. It reads nothing from the server.
func Quality(ctx context.Context, s Source, q Query) (Result, error) {
	return runView(ctx, s, q, "quality", nil, func(e env, r *Result) error {
		m, err := computeMetrics(e, e.q.Window, slices.Concat(reachIDs, qualityIDs))
		if err != nil {
			return err
		}
		r.Metrics = m
		if r.Tables.OutcomeMix, err = outcomeMix(e, e.q.Window); err != nil {
			return err
		}
		r.Tables.Reported, err = reported(e, e.q.Window)
		return err
	})
}

// Cost is what memory costs: context, latency, the server's extraction.
func Cost(ctx context.Context, s Source, q Query) (Result, error) {
	return runView(ctx, s, q, "cost", CostServerNeeds(), func(e env, r *Result) error {
		m, err := computeMetrics(e, e.q.Window, costIDs)
		if err != nil {
			return err
		}
		r.Metrics = m
		r.Tables.Extraction = extractionRows(e, e.q.Window)
		return nil
	})
}

// Bank is what the server holds and what to review.
func Bank(ctx context.Context, s Source, q Query) (Result, error) {
	return runView(ctx, s, q, "bank", BankServerNeeds(), func(e env, r *Result) error {
		m, err := computeMetrics(e, e.q.Window, slices.Concat(bankIDs, staleIDs))
		if err != nil {
			return err
		}
		r.Metrics = m
		r.Tables.ByProject = byProject(e)
		rv, err := review(e)
		if err != nil {
			return err
		}
		r.Tables.Review = &rv
		return nil
	})
}

// Pipeline is whether what Claude learns reaches the server.
func Pipeline(ctx context.Context, s Source, q Query) (Result, error) {
	return runView(ctx, s, q, "pipeline", PipelineServerNeeds(), func(e env, r *Result) error {
		m, err := computeMetrics(e, e.q.Window, pipelineIDs)
		if err != nil {
			return err
		}
		r.Metrics = m
		r.Tables.FailedOperations = failedOperations(e)
		r.Tables.Jobs = jobRows(m["jobs.last_run"])
		r.Tables.Doctor = doctorRows(e)
		return nil
	})
}

// systemIDs are the system page's metrics.
var systemIDs = []string{"doctor.check", "server.reach", "jobs.last_run", "local.backlog"}

// System is the doctor's checks and the state of the services.
func System(ctx context.Context, s Source, q Query) (Result, error) {
	return runView(ctx, s, q, "system", []string{"version", "doctor"}, func(e env, r *Result) error {
		m, err := computeMetrics(e, e.q.Window, systemIDs)
		if err != nil {
			return err
		}
		r.Metrics = m
		r.Tables.Doctor = doctorRows(e)
		r.Tables.Jobs = jobRows(m["jobs.last_run"])
		info, err := systemInfo(e)
		if err != nil {
			return err
		}
		r.Tables.System = &info
		return nil
	})
}

// runView runs fill for the view name inside one withTx (the server reads
// of need first), with the envelope every view carries, and makes every
// list of the result marshal as [] rather than null.
func runView(ctx context.Context, src Source, q Query, name string, need []string, fill func(e env, r *Result) error) (Result, error) {
	machines := q.Machines
	if machines == "" {
		machines = "this"
	}
	prev := q.Window.Previous()
	r := Result{View: name, ViewSchema: ViewSchema, Window: q.Window, Previous: Span{Since: prev.Since, Until: prev.Until},
		Scope: Scope{Machines: machines, Server: src.ServerHost, Project: q.Project}, Index: src.Fresh}
	err := withTx(ctx, src, q, need, func(e env) error {
		r.GeneratedAt = e.now.In(viewLoc(e))
		if !r.Index.IngestedAt.IsZero() {
			r.Index.IngestedAt = r.Index.IngestedAt.In(viewLoc(e))
		}
		r.Server = e.server
		if e.server != nil {
			r.Scope.ServerOK = e.server.OK
		} else {
			ok, at, err := reachRecorded(e)
			if err != nil {
				return err
			}
			r.Scope.ServerOK, r.Scope.ServerOKAt = ok, at
		}
		return fill(e, &r)
	})
	if err != nil {
		return Result{}, err
	}
	normalise(&r)
	return r, nil
}

// viewLoc is the zone the view's times are read in.
func viewLoc(e env) *time.Location {
	if e.q.Window.Loc == nil {
		return time.UTC
	}
	return e.q.Window.Loc
}

// reachRecorded is, for a view that reads nothing from the server, what
// this PC last recorded of it: ok for an answer and no failure since, and
// the time of that last answer (nil when none was recorded).
func reachRecorded(e env) (bool, *time.Time, error) {
	l, err := readLocal(e)
	if err != nil || l == nil || !l.reachLastOK.Valid {
		return false, nil, err
	}
	at := time.UnixMilli(l.reachLastOK.Int64).In(viewLoc(e))
	return !l.reachFailingSince.Valid, &at, nil
}

// normalise makes the result's own lists non-nil; every metric's tags
// (finish) and every axis's reasons (assess) already are.
func normalise(r *Result) {
	if r.Metrics == nil {
		r.Metrics = map[string]Metric{}
	}
	if r.Attention == nil {
		r.Attention = []Item{}
	}
	if r.Info == nil {
		r.Info = []InfoItem{}
	}
}
