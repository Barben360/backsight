package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/retain"
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
			ok, err := reachRecordedOK(e)
			if err != nil {
				return err
			}
			r.Scope.ServerOK = ok
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

// reachRecordedOK is, for a view that reads nothing from the server, what
// this PC last recorded of it: an answer, and no failure since.
func reachRecordedOK(e env) (bool, error) {
	l, err := readLocal(e)
	if err != nil || l == nil {
		return false, err
	}
	return l.reachLastOK.Valid && !l.reachFailingSince.Valid, nil
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

// activity counts the window's prompts, recalls, accepted credit calls and
// send events: the overview's agreement counts.
func activity(e env, w Window) (Activity, error) {
	a := Activity{Credits: map[string]int64{}, Retains: map[string]int64{}}
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	err := e.tx.QueryRowContext(e.ctx, `
SELECT COALESCE(SUM(CASE WHEN reason IS NULL OR reason <> 'machine' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN reason = 'machine' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN decision = 'recall' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN decision = 'recall' AND COALESCE(injected, 0) > 0 THEN 1 ELSE 0 END), 0)
FROM tel_prompt WHERE ts >= ? AND ts < ? AND `+projectClause,
		since, until, e.q.Project, e.q.Project).Scan(&a.Prompts, &a.Machine, &a.Recalls, &a.WithHits)
	if err != nil {
		return Activity{}, err
	}
	for _, c := range []struct {
		query string
		into  map[string]int64
	}{
		{`SELECT COALESCE(verdict, ''), COUNT(*) FROM tel_credit WHERE ok = 1 AND ts >= ? AND ts < ? AND ` + projectClause + ` GROUP BY 1`, a.Credits},
		{`SELECT COALESCE(status, ''), COUNT(*) FROM tel_retain WHERE ts >= ? AND ts < ? AND ` + projectClause + ` GROUP BY 1`, a.Retains},
	} {
		if err := countBy(e, c.query, c.into, since, until, e.q.Project, e.q.Project); err != nil {
			return Activity{}, err
		}
	}
	return a, nil
}

// countBy reads (key, count) rows of query into into, an empty key as
// unknownKey.
func countBy(e env, query string, into map[string]int64, args ...any) error {
	rows, err := e.tx.QueryContext(e.ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		into[orUnknown(k)] += n
	}
	return rows.Err()
}

// sessionRows are the sessions active in w (a prompt, a briefing, a show
// or a credit in it), for the query's project, with their counts over w:
// typed prompts, those that got memory, the memories shown (any path but
// the sources answers), the memories credited used and reported wrong or
// outdated, and the characters added (typed prompts' recalls and
// briefings). The project is the session's (its telemetry's, else its
// turns'). Sent is left to the caller.
func sessionRows(e env, w Window) ([]SessionRow, error) {
	since, until, p := w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project
	rows, err := e.tx.QueryContext(e.ctx, `
WITH a AS (
  SELECT session, ts, project,
         CASE WHEN reason IS NULL OR reason <> 'machine' THEN 1 ELSE 0 END AS prompts,
         CASE WHEN (reason IS NULL OR reason <> 'machine') AND decision = 'recall' AND COALESCE(injected, 0) > 0 THEN 1 ELSE 0 END AS pwm,
         0 AS shown, 0 AS used, 0 AS wrong,
         CASE WHEN reason IS NULL OR reason <> 'machine' THEN COALESCE(chars, 0) ELSE 0 END AS chars
  FROM tel_prompt WHERE ts >= ? AND ts < ? AND COALESCE(session, '') <> '' AND `+projectClause+`
  UNION ALL
  SELECT session, ts, project, 0, 0, 0, 0, 0, COALESCE(chars, 0)
  FROM tel_briefing WHERE ts >= ? AND ts < ? AND COALESCE(session, '') <> '' AND `+projectClause+`
  UNION ALL
  SELECT s.session, s.ts, NULL, 0, 0, 1, 0, 0, 0
  FROM shows s WHERE s.ts >= ? AND s.ts < ? AND COALESCE(s.session, '') <> '' AND COALESCE(s.path, '') <> ? AND `+sessionProject("s")+`
  UNION ALL
  SELECT c.session, c.ts, NULL, 0, 0, 0,
         CASE WHEN c.verdict = 'used' THEN 1 ELSE 0 END, CASE WHEN c.verdict IN ('wrong', 'outdated') THEN 1 ELSE 0 END, 0
  FROM credits c WHERE c.ts >= ? AND c.ts < ? AND COALESCE(c.session, '') <> '' AND c.memory_id IS NOT NULL AND `+sessionProject("c")+`)
SELECT session, COALESCE(MAX(project), (SELECT MAX(t.project) FROM turns t WHERE t.session = a.session), ''),
       MIN(ts), MAX(ts), SUM(prompts), SUM(pwm), SUM(shown), SUM(used), SUM(wrong), SUM(chars)
FROM a GROUP BY session`,
		since, until, p, p, since, until, p, p, since, until, feedback.PathSources, p, p, since, until, p, p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SessionRow
	for rows.Next() {
		var s SessionRow
		var first, last int64
		if err := rows.Scan(&s.ID, &s.Project, &first, &last, &s.Prompts, &s.PromptsWithMemory, &s.Shown, &s.Used, &s.Wrong, &s.ContextChars); err != nil {
			return nil, err
		}
		s.Start, s.Last = time.UnixMilli(first).In(w.Loc), time.UnixMilli(last).In(w.Loc)
		out = append(out, s)
	}
	return out, rows.Err()
}

// sentStatus is the status of the session's latest send, "" when none.
func sentStatus(e env, session string) (string, error) {
	var st sql.NullString
	err := e.tx.QueryRowContext(e.ctx, `SELECT status FROM tel_retain WHERE session = ?
ORDER BY ts DESC, source_file DESC, "offset" DESC LIMIT 1`, session).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return st.String, err
}

// lately is the overview's latest sessions: the latestRows sessions with
// the latest activity in w, those with a wrong or outdated report first
// (the latest first within each group).
func lately(e env, w Window) ([]SessionRow, error) {
	all, err := sessionRows(e, w)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(all, func(a, b SessionRow) int {
		if c := b.Last.Compare(a.Last); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	out := all[:min(latelyRows, len(all))]
	slices.SortStableFunc(out, func(a, b SessionRow) int {
		switch {
		case a.Wrong > 0 && b.Wrong == 0:
			return -1
		case a.Wrong == 0 && b.Wrong > 0:
			return 1
		}
		return 0
	})
	for i := range out {
		if out[i].Sent, err = sentStatus(e, out[i].ID); err != nil {
			return nil, err
		}
	}
	return append([]SessionRow{}, out...), nil
}

// outcomeMix is the outcome of the shows of w per day (or hour) of their
// show time: used (settled credit), ignored (confident_miss), not declared
// (unknown), shown only (shown), pending (not settled yet); forgotten
// shows are left out (used.share_confident counts them). The shows are
// those of the query's path, every path but the sources answers when it
// names none; split by path too in developer mode. Without the journal's
// tables to read (feedback off, a server-only host) there are no rows.
func outcomeMix(e env, w Window) ([]OutcomeDay, error) {
	out := []OutcomeDay{}
	if e.src.Config.ServerOnly() || !e.src.Config.Feedback.Enabled {
		return out, nil
	}
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	rows, err := e.tx.QueryContext(e.ctx, `
WITH st AS (
  SELECT key, outcome, MIN(settle_ts) AS at FROM settles
  WHERE key IS NOT NULL AND show_at >= ? AND show_at < ? GROUP BY key)
SELECT s.key, COALESCE(s.path, ''), MIN(s.ts), st.outcome
FROM shows s LEFT JOIN st ON st.key = s.key
WHERE s.ts >= ? AND s.ts < ? AND s.key IS NOT NULL AND COALESCE(s.path, '') <> ? AND (? = '' OR s.path = ?) AND `+sessionProject("s")+`
GROUP BY s.key`,
		since, until, since, until, feedback.PathSources, e.q.Path, e.q.Path, e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	buckets := w.Buckets()
	for _, b := range buckets {
		d := OutcomeDay{Day: b.Key, Partial: b.Partial}
		if e.q.Dev {
			d.ByPath = map[string]OutcomeDay{}
		}
		out = append(out, d)
	}
	add := func(d *OutcomeDay, outcome sql.NullString) {
		switch {
		case !outcome.Valid:
			d.Pending++
		case outcome.String == feedback.OutcomeCredit:
			d.Used++
		case outcome.String == feedback.OutcomeMiss:
			d.Ignored++
		case outcome.String == feedback.OutcomeUnknown:
			d.NotDeclared++
		case outcome.String == feedback.OutcomeShown:
			d.ShownOnly++
		} // forgotten, and an outcome outside the closed set, are left out
	}
	for rows.Next() {
		var (
			key, path string
			ts        int64
			outcome   sql.NullString
		)
		if err := rows.Scan(&key, &path, &ts, &outcome); err != nil {
			return nil, err
		}
		i := bucketIndex(buckets, time.UnixMilli(ts).In(w.Loc))
		if i < 0 {
			continue
		}
		add(&out[i], outcome)
		if e.q.Dev {
			p := out[i].ByPath[path]
			p.Day = out[i].Day
			add(&p, outcome)
			out[i].ByPath[path] = p
		}
	}
	return out, rows.Err()
}

// reported lists the wrong and outdated credits of w (each at its memory's
// earliest show in its turn, else at its own time, as verdict.wrong_rate
// places them), the latest first, at most the query's limit; the project
// is the session's.
func reported(e env, w Window) ([]ReportedRow, error) {
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT memory_id, verdict, session, turn, project, at FROM (
  SELECT c.memory_id, c.verdict, COALESCE(c.session, '') AS session, COALESCE(c.turn, '') AS turn,
         COALESCE((SELECT MAX(tp.project) FROM turns tp WHERE tp.session = c.session AND tp.turn = c.turn), '') AS project,
         COALESCE((SELECT MIN(s.ts) FROM shows s
           WHERE s.session = c.session AND s.turn = c.turn AND s.memory_id = c.memory_id), c.ts) AS at,
         c.source_file, c."offset", c.pos
  FROM credits c
  WHERE c.memory_id IS NOT NULL AND c.verdict IN ('wrong', 'outdated') AND c.ts >= ? AND `+sessionProject("c")+`)
WHERE at >= ? AND at < ?
ORDER BY at DESC, source_file DESC, "offset" DESC, pos DESC LIMIT ?`,
		since, e.q.Project, e.q.Project, since, until, e.q.EffectiveLimit())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ReportedRow{}
	for rows.Next() {
		var r ReportedRow
		var at int64
		if err := rows.Scan(&r.MemoryID, &r.Verdict, &r.Session, &r.Turn, &r.Project, &at); err != nil {
			return nil, err
		}
		r.At = time.UnixMilli(at).In(w.Loc)
		out = append(out, r)
	}
	return out, rows.Err()
}

// extractionRows are the server's extraction tokens per day (or hour) and
// operation of w: each named operation read, and "other" (the rest of the
// total) when every one was read; days without tokens are left out. The
// cost is null without prices. No rows when the total cannot be read.
func extractionRows(e env, w Window) []ExtractionRow {
	out := []ExtractionRow{}
	all, reason := llmRead(e, "llm:")
	if reason != "" {
		return out
	}
	buckets := w.Buckets()
	width := llmBucketWidth(llmPeriod(viewSpan(e.now, w)))
	_, allDays, _ := inWindow(all, width, w.Since, w.Until, w.Loc, buckets)
	cfg := e.src.Config.LLM
	prices := retain.PricesFor(cfg.Model, cfg.PriceInput, cfg.PriceCached, cfg.PriceOutput, cfg.Currency)
	other := slices.Clone(allDays)
	read := 0
	byOp := map[string][]tokens{}
	for _, op := range extractionOps {
		bs, reason := llmRead(e, "llm:"+op)
		if reason != "" {
			continue
		}
		read++
		_, days, _ := inWindow(bs, width, w.Since, w.Until, w.Loc, buckets)
		byOp[op] = days
		for i, t := range days {
			other[i].input = max(0, other[i].input-t.input)
			other[i].cached = max(0, other[i].cached-t.cached)
			other[i].output = max(0, other[i].output-t.output)
		}
	}
	ops := slices.Clone(extractionOps)
	if read == len(extractionOps) {
		byOp["other"] = other
		ops = append(ops, "other")
	}
	for i, b := range buckets {
		for _, op := range ops {
			days, ok := byOp[op]
			if !ok {
				continue
			}
			t := days[i]
			if t.input == 0 && t.cached == 0 && t.output == 0 {
				continue
			}
			row := ExtractionRow{Day: b.Key, Operation: op, Input: t.input, Cached: t.cached, Output: t.output}
			if prices.Known() {
				row.Cost = ptr(prices.Cost(t.input, t.cached, t.output, 0))
			}
			out = append(out, row)
		}
	}
	return out
}

// byProject is the server's memory count per project tag, the largest
// first; no rows when the tags cannot be read.
func byProject(e env) []ProjectCount {
	out := []ProjectCount{}
	if serverReason(e, "tags") != "" {
		return out
	}
	for _, t := range e.srv.Tags {
		if p, ok := strings.CutPrefix(t.Tag, projectTagPrefix); ok {
			out = append(out, ProjectCount{Project: p, Memories: int64(t.Count)})
		}
	}
	slices.SortFunc(out, func(a, b ProjectCount) int {
		if a.Memories != b.Memories {
			return int(b.Memories - a.Memories)
		}
		return strings.Compare(a.Project, b.Project)
	})
	return out
}

// readLedger is the index's ledger rows as a feedback.Ledger (what
// feedback.Stale reads).
func readLedger(e env) (feedback.Ledger, error) {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT memory_id, shown, credits, wrong, hidden_since, imported_shown, last_shown FROM ledger`)
	if err != nil {
		return feedback.Ledger{}, err
	}
	defer func() { _ = rows.Close() }()
	l := feedback.Ledger{Memories: map[string]feedback.Entry{}}
	for rows.Next() {
		var (
			id                     string
			shown, wrong, imported sql.NullInt64
			credits                sql.NullFloat64
			hiddenSince, lastShown sql.NullInt64
		)
		if err := rows.Scan(&id, &shown, &credits, &wrong, &hiddenSince, &imported, &lastShown); err != nil {
			return feedback.Ledger{}, err
		}
		m := feedback.Entry{Shown: int(shown.Int64), Credits: credits.Float64, Wrong: int(wrong.Int64),
			Imported: feedback.Imported{Shown: int(imported.Int64)}}
		if lastShown.Valid {
			m.LastShown = time.UnixMilli(lastShown.Int64)
		}
		if hiddenSince.Valid {
			m.Hidden = &feedback.Hidden{Since: time.UnixMilli(hiddenSince.Int64)}
		}
		l.Memories[id] = m
	}
	return l, rows.Err()
}

// staleLists is feedback.Stale of the index's ledger as of now, read once
// per transaction (the stale counts do not depend on the window).
func (e env) staleLists() (feedback.StaleLists, error) {
	if e.memo != nil && e.memo.stale != nil {
		return *e.memo.stale, nil
	}
	l, err := readLedger(e)
	if err != nil {
		return feedback.StaleLists{}, err
	}
	lists := feedback.Stale(l, e.src.Config.Feedback.StaleShows, e.now)
	if e.memo != nil {
		e.memo.stale = &lists
	}
	return lists, nil
}

// review is the three review lists of feedback.Stale (memory stale's own),
// at most the query's limit each, with the command `memory stale` gives
// for each: forget, restore, purge. The ledger has no project: the lists
// are this PC's whatever the filter (as the stale counts, tagged not
// filtered); a row's project is its memory's latest known one.
func review(e env) (Review, error) {
	l, err := readLedger(e)
	if err != nil {
		return Review{}, err
	}
	lists := feedback.Stale(l, e.src.Config.Feedback.StaleShows, e.now)
	limit := e.q.EffectiveLimit()
	rows := func(ids []string, command func(id string) string) []ReviewRow {
		out := []ReviewRow{}
		for _, id := range ids[:min(limit, len(ids))] {
			m := l.Memories[id]
			r := ReviewRow{ID: id, Project: memoryProject(e.tx, id), Shown: int64(m.Shown), Command: command(id)}
			if !m.LastShown.IsZero() {
				r.LastShown = m.LastShown.In(viewLoc(e))
			}
			out = append(out, r)
		}
		return out
	}
	return Review{
		NeverUsed: rows(lists.NeverUsed, func(id string) string { return "backsight forget " + id }),
		Hidden:    rows(lists.Hidden, func(id string) string { return "backsight forget --restore " + id }),
		Reported:  rows(lists.Reported, func(id string) string { return "backsight forget " + id + " --purge" }),
	}, nil
}

// failedOperations are the failed operations read from the server, the
// latest first: their type (the operation's, else the task's), age since
// their last change and retries; never their error message. No rows when
// they cannot be read.
func failedOperations(e env) []OperationRow {
	out := []OperationRow{}
	if serverReason(e, "failed") != "" {
		return out
	}
	for _, o := range e.srv.Failed {
		typ := o.Type
		if typ == "" {
			typ = o.TaskType
		}
		out = append(out, OperationRow{Type: typ, AgeMS: max(0, e.now.Sub(opTime(o)).Milliseconds()), Retries: int64(o.RetryCount)})
	}
	return out
}

// jobRows are the jobs' last runs from jobs.last_run, in the order of
// jobCadences; a job that never ran has no row.
func jobRows(m Metric) []JobRow {
	out := []JobRow{}
	for _, j := range jobCadences {
		run, ok := m.Extra[j.name].(map[string]any)
		if !ok {
			continue
		}
		r := JobRow{Name: j.name}
		r.Status, _ = run["status"].(string)
		r.LastAt, _ = run["last_at"].(time.Time)
		r.MS, _ = run["ms"].(int64)
		r.AgeMS, _ = run["age_ms"].(int64)
		out = append(out, r)
	}
	return out
}

// doctorRows are the doctor's checks as doctorEntries keeps them (id,
// status, a `backsight` fix; never a message); no rows without a run.
func doctorRows(e env) []DoctorRow {
	out := []DoctorRow{}
	if !doctorRead(e) {
		return out
	}
	for _, c := range doctorEntries(e.srv.Doctor) {
		out = append(out, DoctorRow(c))
	}
	return out
}

// systemInfo is the system page: the server's version, this PC's runtime
// and mode, what it recorded of the server, its backups and update checks
// (local_status), and the index's state.
func systemInfo(e env) (SystemInfo, error) {
	loc := viewLoc(e)
	info := SystemInfo{Runtime: e.src.Config.RuntimeKind(), Mode: e.src.Config.Server.Mode}
	if serverReason(e, "version") == "" {
		info.ServerVersion = e.srv.Version.APIVersion
	}
	var (
		lastOK, failing, open, created, verified, checked sql.NullInt64
		failures, newer                                   sql.NullInt64
		backupOK, checkFailed                             sql.NullBool
	)
	err := e.tx.QueryRowContext(e.ctx, `
SELECT reach_last_ok, reach_failing_since, breaker_open_until, breaker_failures,
       backup_created_at, backup_verified_at, backup_ok, update_checked_at, update_newer, update_check_failed
FROM local_status WHERE id = 1`).Scan(&lastOK, &failing, &open, &failures, &created, &verified, &backupOK, &checked, &newer, &checkFailed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SystemInfo{}, err
	}
	at := func(ms sql.NullInt64) time.Time {
		if !ms.Valid {
			return time.Time{}
		}
		return time.UnixMilli(ms.Int64).In(loc)
	}
	info.Reach = ReachInfo{LastOK: at(lastOK), FailingSince: at(failing), BreakerOpenUntil: at(open), BreakerFailures: int(failures.Int64)}
	info.Backup = BackupInfo{CreatedAt: at(created), VerifiedAt: at(verified), OK: backupOK.Bool}
	info.Update = UpdateInfo{CheckedAt: at(checked), Newer: int(newer.Int64), CheckFailed: checkFailed.Bool}
	if err := e.tx.QueryRowContext(e.ctx, `PRAGMA user_version`).Scan(&info.Index.Schema); err != nil {
		return SystemInfo{}, err
	}
	fr := e.src.Fresh
	info.Index.LagSeconds, info.Index.Partial = fr.LagSeconds, fr.Partial
	if !fr.IngestedAt.IsZero() {
		info.Index.IngestedAt = fr.IngestedAt.In(loc)
	}
	return info, nil
}
