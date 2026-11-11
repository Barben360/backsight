package metrics

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	case "releases":
		return Releases(ctx, s, q)
	case "projects":
		return Projects(ctx, s, q)
	case "sessions":
		return Sessions(ctx, s, q)
	case "memories":
		return Memories(ctx, s, q)
	case "days":
		return Days(ctx, s, q)
	case "defs":
		return Defs(ctx, s, q)
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

// overviewIDs are the overview's metrics: every axis's, the bank tiles, the
// review-queue counts and the time to recall (no axis's metric: it carries
// no verdict and no attention item).
func overviewIDs() []string {
	var ids []string
	for _, axis := range axisOrder {
		ids = append(ids, axisMetrics[axis]...)
	}
	return append(ids, append([]string{"bank.size", "bank.growth", timeToRecallID}, staleIDs...)...)
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
		// The verdict leaves out the time to recall (no rule reads it): the
		// overview makes it, for the same window, beside the verdict's figures.
		m, reused := a.metricsFor(e.q, slices.DeleteFunc(slices.Clone(ids), verdictSkips))
		if reused {
			ttr, err := computeMetrics(e, e.q.Window, []string{timeToRecallID})
			if err != nil {
				return err
			}
			maps.Copy(m, ttr)
		} else if m, err = computeMetrics(e, e.q.Window, ids); err != nil {
			return err
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
		if r.verdictFigures, err = verdictFigures(e, a, m, reused); err != nil {
			return err
		}
		r.Attention = a.Items
		if len(r.Attention) > attentionShown {
			r.Attention, r.AttentionMore = r.Attention[:attentionShown], len(r.Attention)-attentionShown
		}
		act, err := activity(e, e.q.Window)
		if err != nil {
			return err
		}
		r.Tables.Activity = &act
		if r.Tables.Lately, err = lately(e, e.q.Window); err != nil {
			return err
		}
		if r.Tables.Helped, err = helped(e, e.q.Window); err != nil {
			return err
		}
		// The split by coding tool, of this PC's activity, when the
		// figures are of every tool on it.
		if e.q.Client == "" && e.q.Machines != "all" {
			r.Tables.ByClient, err = byClient(e, e.q.Window)
		}
		return err
	})
}

// verdictFigures are the figures the verdict's axis lines quote: the
// verdict's last 7 days whatever the view's window (the banner says "last
// 7 days"), in the view's scope (this PC, or every PC added up), the time
// to recall included. The view's own figures when its window is the
// verdict's (reused); else the axes' metrics over the verdict's window,
// figures as of now included (the window is made the view's for them, as
// assess does). Right's are its 28-day figures when it was judged on them
// (this PC's, as the verdict).
func verdictFigures(e env, a assessment, view map[string]Metric, reused bool) (map[string]Metric, error) {
	var out map[string]Metric
	if reused {
		out = maps.Clone(view)
	} else {
		ev := e
		ev.q.Window = a.Window
		ids := []string{timeToRecallID}
		for _, axis := range axisOrder {
			ids = append(ids, axisMetrics[axis]...)
		}
		var err error
		if out, err = computeMetrics(ev, a.Window, ids); err != nil {
			return nil, err
		}
	}
	if a.Right28 == nil {
		return out, nil
	}
	for _, id := range axisMetrics["right"] {
		if m, ok := a.Right28[id]; ok && e.q.Machines != "all" {
			out[id] = m
		} else if m, ok := out[id]; ok && !slices.Contains(m.Tags, tagJudged28d) {
			m.Tags = append(slices.Clone(m.Tags), tagJudged28d)
			out[id] = m
		}
	}
	return out, nil
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
		if e.merging() {
			if r.Tables.ByPC, err = byPC(e, e.q.Window, m); err != nil {
				return err
			}
		}
		r.Tables.Extraction, err = extractionRows(e, e.q.Window)
		return err
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
		r.Tables.TopShown, err = topShown(e, e.q.Window)
		return err
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
		r.Tables.NeverExtracted, err = neverExtracted(e, e.q.Window)
		return err
	})
}

// systemIDs are the system page's metrics.
var systemIDs = []string{"doctor.check", "server.reach", "jobs.last_run", "local.backlog", secretsID, doctorHistoryID}

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
		dh, err := readDoctorHistory(e, e.q.Window)
		if err != nil {
			return err
		}
		r.Tables.DoctorHistory = dh.rows
		info, err := systemInfo(e)
		if err != nil {
			return err
		}
		r.Tables.System = &info
		mt, err := machinesTable(e)
		if err != nil {
			return err
		}
		r.Tables.Machines = mt
		return nil
	})
}

// runView runs fill for the view name inside one withTx (the server reads
// of need first), with the envelope every view carries, and makes every
// list of the result marshal as [] rather than null.
func runView(ctx context.Context, src Source, q Query, name string, need []string, fill func(e env, r *Result) error) (Result, error) {
	// Sharing off answers this PC's figures, and the browse views always
	// do: the scope says so.
	// A client filter counts this PC's activity: other PCs send no client.
	machines := "this"
	if q.Machines == "all" && q.Client == "" && sharingOn(src.Config) && !slices.Contains(thisOnlyViews, name) {
		machines = "all"
	}
	q.Machines = machines
	prev := q.Window.Previous()
	// build is the view's answer for one switch, in the transaction's env.
	build := func(e env, machines string) (Result, error) {
		r := Result{View: name, ViewSchema: ViewSchema, Window: q.Window, Previous: Span{Since: prev.Since, Until: prev.Until},
			Scope: Scope{Machines: machines, Server: src.ServerHost, Project: q.Project, Client: q.Client}, Index: src.Fresh}
		e.view = name
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
				return Result{}, err
			}
			r.Scope.ServerOK, r.Scope.ServerOKAt = ok, at
		}
		pcs, others, err := scopePCs(e)
		if err != nil {
			return Result{}, err
		}
		r.Scope.PCs = pcs
		r.Scope.OthersUnfiltered = machines == "all" && q.Project != "" && others
		if r.Scope.Clients, err = scopeClients(e, q.Window); err != nil {
			return Result{}, err
		}
		if err := fill(e, &r); err != nil {
			return Result{}, err
		}
		normalise(&r)
		return r, nil
	}
	var r Result
	err := withTx(ctx, src, q, need, func(e env) error {
		var err error
		if r, err = build(e, machines); err != nil {
			return err
		}
		if machines == "all" && q.WithOwn {
			// This PC's own answer, from the same transaction and memo: the families this PC's run shares
			// with the merged one are not computed again.
			own, err := build(e.this(), "this")
			if err != nil {
				return err
			}
			r.own = &own
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return r, nil
}

// viewLoc is the zone the view's times are read in.
func viewLoc(e env) *time.Location { return viewLocOf(e.q.Window) }

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
