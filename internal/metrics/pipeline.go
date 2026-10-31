package metrics

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// The pipeline family: does what Claude learns reach the server and become
// memory. Sends and their backlog, hook failures, jobs and reach come from
// this PC's telemetry and state; the queue and the extraction delay from
// the server's operations; the health checks from the doctor.

var pipelineIDs = []string{"pipeline.flow", "retain.outcomes", "local.backlog", "server.queue", "server.extraction_delay",
	"hooks.failures", "server.reach", "doctor.check", "jobs.last_run"}

func init() { register("pipeline", pipelineIDs, pipeline) }

// The cadences of the background jobs: how often `jobs tick` and `jobs
// maintain` are due.
const (
	tickCadence     = 15 * time.Minute
	maintainCadence = 24 * time.Hour
)

// PipelineServerNeeds are the server reads of a view holding the pipeline
// family (loadServer keys): the operations queue, the failed and the
// completed operations, and the live doctor run.
func PipelineServerNeeds() []string { return []string{"queue", "failed", "completed", "doctor"} }

// failedWindow is how far back server.queue counts failed operations,
// whatever the view's window.
const failedWindow = 7 * day

// The retain statuses of a document.
var retainStatuses = []string{"ok", "failed", "quarantined", "queued", "retry"}

// isCurrent reports whether w is the view's own window. Figures as of now
// (the backlog, the queue, the doctor, the jobs) cannot be read for any
// other: the previous window reads no_events (no previous value, no delta).
func isCurrent(e env, w Window) bool {
	return w.Since.Equal(e.q.Window.Since) && w.Until.Equal(e.q.Window.Until)
}

// serverReasons is the first no_data reason of the reads of keys, "" when
// all succeeded.
func serverReasons(e env, keys ...string) string {
	for _, k := range keys {
		if r := serverReason(e, k); r != "" {
			return r
		}
	}
	return ""
}

func pipeline(e env, w Window) (map[string]Observation, map[string]Metric, error) {
	obs := make(map[string]Observation, len(pipelineIDs))
	met := make(map[string]Metric, len(pipelineIDs))
	absent := func(id, reason string) {
		obs[id], met[id] = Observation{Absent: true, Reason: reason}, Metric{}
	}
	age, err := backlogAge(e.tx, e.now)
	if err != nil {
		return nil, nil, err
	}
	if err := pipelineRetain(e, w, age, obs, met); err != nil {
		return nil, nil, err
	}
	local, err := readLocal(e)
	if err != nil {
		return nil, nil, err
	}
	if local == nil || !isCurrent(e, w) {
		absent("local.backlog", ReasonNoEvents)
	} else {
		pipelineLocalBacklog(local, age, obs, met)
	}
	if local == nil {
		absent("server.reach", ReasonNoEvents)
	} else if err := pipelineReach(e, w, local, age, obs, met); err != nil {
		return nil, nil, err
	}
	if err := pipelineHooks(e, w, obs, met); err != nil {
		return nil, nil, err
	}
	if reason := serverReasons(e, "queue", "failed"); reason != "" {
		absent("server.queue", reason)
	} else if !isCurrent(e, w) {
		absent("server.queue", ReasonNoEvents)
	} else {
		pipelineQueue(e, obs, met)
	}
	if reason := serverReasons(e, "completed"); reason != "" {
		absent("server.extraction_delay", reason)
	} else {
		pipelineDelay(e, w, obs, met)
	}
	if !isCurrent(e, w) || !doctorRead(e) {
		absent("doctor.check", ReasonNoEvents)
	} else {
		pipelineDoctor(e, obs, met)
	}
	if err := pipelineJobs(e, w, obs, met); err != nil {
		return nil, nil, err
	}
	return obs, met, nil
}

// telemetryAbsent reports why a telemetry metric has nothing to read: a
// server-only host, or no row at all in any of tables.
func telemetryAbsent(e env, tables ...string) (string, bool, error) {
	if e.src.Config.ServerOnly() {
		return ReasonServerOnlyHost, true, nil
	}
	for _, t := range tables {
		var any bool
		if err := e.tx.QueryRowContext(e.ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s)`, t)).Scan(&any); err != nil {
			return "", false, err
		}
		if any {
			return "", false, nil
		}
	}
	return ReasonNoEvents, true, nil
}

// backlogAge is the age of the oldest thing waiting on this PC: the oldest
// queued note, or the oldest document whose last status is queued or retry,
// counted from the first of the events of its current wait (the ones since
// its last status that was neither), so that a retry every few minutes does
// not reset it. Set-aside items are not waiting. 0 when nothing waits; never
// negative (a clock ahead of now reads 0).
func backlogAge(tx *sql.Tx, now time.Time) (time.Duration, error) {
	var oldest sql.NullInt64
	err := tx.QueryRow(`
SELECT MIN(t) FROM (
  SELECT notes_oldest_at AS t FROM local_status WHERE id = 1 AND notes_oldest_at IS NOT NULL
  UNION ALL
  SELECT (SELECT MIN(r.ts) FROM tel_retain r
          WHERE r.document = d.document
            AND r.ts > COALESCE((SELECT MAX(x.ts) FROM tel_retain x
                                 WHERE x.document = d.document AND x.status NOT IN ('queued', 'retry')), -1))
  FROM document_status d WHERE d.status IN ('queued', 'retry'))`).Scan(&oldest)
	if err != nil {
		return 0, err
	}
	if !oldest.Valid {
		return 0, nil
	}
	return max(0, now.Sub(time.UnixMilli(oldest.Int64))), nil
}

// localRow is the local_status row, nil when this PC never wrote one.
type localRow struct {
	notesQueued, sessionsWaiting, quarantined        int64
	reachLastOK, reachFailingSince, breakerOpenUntil sql.NullInt64
}

func readLocal(e env) (*localRow, error) {
	var l localRow
	err := e.tx.QueryRowContext(e.ctx, `
SELECT COALESCE(notes_queued, 0), COALESCE(sessions_waiting, 0), COALESCE(quarantined, 0),
       reach_last_ok, reach_failing_since, breaker_open_until
FROM local_status WHERE id = 1`).Scan(&l.notesQueued, &l.sessionsWaiting, &l.quarantined,
		&l.reachLastOK, &l.reachFailingSince, &l.breakerOpenUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// timeOrNil is a local_status time in loc, nil when unset.
func timeOrNil(ms sql.NullInt64, loc *time.Location) any {
	if !ms.Valid {
		return nil
	}
	return time.UnixMilli(ms.Int64).In(loc)
}

// docEvent is one document of the window: its last event in the window and
// its last status overall.
type docEvent struct {
	lastTS int64
	status string
}

// pipelineRetain fills pipeline.flow and retain.outcomes: the documents
// with a send in the window, each counted once, at its last status overall.
func pipelineRetain(e env, w Window, age time.Duration, obs map[string]Observation, met map[string]Metric) error {
	ids := []string{"pipeline.flow", "retain.outcomes"}
	reason, none, err := telemetryAbsent(e, "tel_retain")
	if err != nil {
		return err
	}
	if none {
		for _, id := range ids {
			obs[id], met[id] = Observation{Absent: true, Reason: reason}, Metric{}
		}
		return nil
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT r.document, r.ts, r.status, r.ms, ds.status
FROM tel_retain r JOIN document_status ds ON ds.document = r.document
WHERE r.ts >= ? AND r.ts < ? AND `+projectClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	docs := map[string]*docEvent{}
	var okMS []int64
	for rows.Next() {
		var doc string
		var ts int64
		var status, last sql.NullString
		var ms sql.NullInt64
		if err := rows.Scan(&doc, &ts, &status, &ms, &last); err != nil {
			return err
		}
		d := docs[doc]
		if d == nil {
			d = &docEvent{status: last.String}
			docs[doc] = d
		}
		d.lastTS = max(d.lastTS, ts)
		if status.String == "ok" && ms.Valid {
			okMS = append(okMS, ms.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	buckets := w.Buckets()
	byStatus := map[string]int64{}
	for _, s := range retainStatuses {
		byStatus[s] = 0
	}
	days := make([]map[string]int64, len(buckets))
	for i := range days {
		days[i] = map[string]int64{}
	}
	for _, d := range docs {
		byStatus[d.status]++
		if i := bucketIndex(buckets, time.UnixMilli(d.lastTS).In(w.Loc)); i >= 0 {
			days[i][d.status]++
		}
	}
	total := int64(len(docs))
	okN, failed, setAside := byStatus["ok"], byStatus["failed"], byStatus["quarantined"]
	waiting := byStatus["queued"] + byStatus["retry"]

	flow := Metric{Num: i64(okN), Den: i64(total), Value: ratio(okN, total), Extra: map[string]any{
		"ok": okN, "failed": failed, "set_aside": setAside, "waiting": waiting,
		"backlog_oldest_ms": age.Milliseconds()}}
	out := Metric{Value: ptr(float64(total)), Extra: map[string]any{"by_status": byStatus}}
	_, p95 := p50p95(okMS)
	out.Extra["p95_ms"] = p95
	for i, b := range buckets {
		var n int64
		for _, c := range days[i] {
			n += c
		}
		p := pointOf(w, b.Key)
		p.Value, p.Num, p.Den = ratio(days[i]["ok"], n), i64(days[i]["ok"]), i64(n)
		flow.Series = append(flow.Series, p)
		p = pointOf(w, b.Key)
		p.Value = ptr(float64(n))
		if n > 0 {
			p.Parts = days[i]
		}
		out.Series = append(out.Series, p)
	}
	obs["pipeline.flow"] = Observation{Num: okN, Den: total, Err: rulePipelineFlow(failed, setAside, age)}
	met["pipeline.flow"] = flow
	obs["retain.outcomes"], met["retain.outcomes"] = Observation{Den: total}, out
	return nil
}

// pipelineLocalBacklog fills local.backlog from the local status, as of now.
func pipelineLocalBacklog(l *localRow, age time.Duration, obs map[string]Observation, met map[string]Metric) {
	const id = "local.backlog"
	obs[id] = Observation{Err: ruleLocalBacklog(age)}
	met[id] = Metric{Value: ptr(float64(l.notesQueued + l.sessionsWaiting + l.quarantined)), Extra: map[string]any{
		"notes_queued": l.notesQueued, "sessions_waiting": l.sessionsWaiting, "quarantined": l.quarantined,
		"oldest_ms": age.Milliseconds()}}
}

// pipelineReach fills server.reach: the typed prompts the breaker skipped
// in the window, and this PC's record of the server's answers.
func pipelineReach(e env, w Window, l *localRow, age time.Duration, obs map[string]Observation, met map[string]Metric) error {
	const id = "server.reach"
	var skipped int64
	err := e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(*) FROM tel_prompt WHERE decision = 'skip' AND reason = 'breaker' AND ts >= ? AND ts < ? AND `+projectClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project).Scan(&skipped)
	if err != nil {
		return err
	}
	open := l.breakerOpenUntil.Valid && time.UnixMilli(l.breakerOpenUntil.Int64).After(e.now)
	obs[id] = Observation{Err: ruleServerReach(l.reachFailingSince.Valid, open, age)}
	met[id] = Metric{Value: ptr(float64(skipped)), Extra: map[string]any{
		"last_ok": timeOrNil(l.reachLastOK, w.Loc), "failing_since": timeOrNil(l.reachFailingSince, w.Loc),
		"breaker_open_until": timeOrNil(l.breakerOpenUntil, w.Loc), "skipped_prompts": skipped}}
	return nil
}

// pipelineHooks fills hooks.failures: the failures counted in the window,
// judged on the last 24 h, whatever the window. The kinds are the index's
// closed set (an unknown kind is stored as other), so no free text reaches
// a view.
func pipelineHooks(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	const id = "hooks.failures"
	reason, none, err := telemetryAbsent(e, "tel_hook_failure", "tel_prompt")
	if err != nil {
		return err
	}
	if none {
		obs[id], met[id] = Observation{Absent: true, Reason: reason}, Metric{}
		return nil
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT ts, COALESCE(kind, '') FROM tel_hook_failure
WHERE ((ts >= ? AND ts < ?) OR (ts >= ? AND ts < ?)) AND `+projectClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.now.Add(-day).UnixMilli(), e.now.UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	buckets := w.Buckets()
	byKind, last24 := map[string]int64{}, map[string]int64{}
	days := make([]map[string]int64, len(buckets))
	for i := range days {
		days[i] = map[string]int64{}
	}
	var total int64
	for rows.Next() {
		var ts int64
		var kind string
		if err := rows.Scan(&ts, &kind); err != nil {
			return err
		}
		t := time.UnixMilli(ts)
		if !t.Before(w.Since) && t.Before(w.Until) {
			total++
			byKind[kind]++
			if i := bucketIndex(buckets, t.In(w.Loc)); i >= 0 {
				days[i][kind]++
			}
		}
		if !t.Before(e.now.Add(-day)) && t.Before(e.now) {
			last24[kind]++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m := Metric{Value: ptr(float64(total)), Extra: map[string]any{"by_kind": byKind, "last_24h": last24}}
	for i, b := range buckets {
		p := pointOf(w, b.Key)
		var n int64
		for _, c := range days[i] {
			n += c
		}
		p.Value = ptr(float64(n))
		if n > 0 {
			p.Parts = days[i]
		}
		m.Series = append(m.Series, p)
	}
	obs[id], met[id] = Observation{Den: total, Err: ruleHookFailures(last24)}, m
	return nil
}

// pipelineQueue fills server.queue from the operations list (never the
// stats, which undercount the queued retains): the queued and running
// totals, and the operations failed in the last 7 days, whatever the
// window, beside every failed one read.
func pipelineQueue(e env, obs map[string]Observation, met map[string]Metric) {
	const id = "server.queue"
	var failed int64
	for _, o := range e.srv.Failed {
		t := opTime(o)
		if !t.Before(e.now.Add(-failedWindow)) && t.Before(e.now) {
			failed++
		}
	}
	obs[id] = Observation{Err: ruleServerQueue(failed)}
	met[id] = Metric{Value: ptr(float64(e.srv.Pending + e.srv.Processing)), Extra: map[string]any{
		"pending": int64(e.srv.Pending), "processing": int64(e.srv.Processing),
		"failed": failed, "failed_total": int64(max(e.srv.FailedTotal, len(e.srv.Failed))),
		"failed_capped": e.srv.FailedTotal > len(e.srv.Failed)}}
}

// pipelineDelay fills server.extraction_delay: the creation-to-last-update
// time of the completed retains updated in the window, among the latest 200
// the server was asked for (completedPages pages, most recent first).
func pipelineDelay(e env, w Window, obs map[string]Observation, met map[string]Metric) {
	const id = "server.extraction_delay"
	var in []hindsight.Operation
	for _, o := range e.srv.Completed {
		if o.Type == "retain" && !o.CreatedAt.IsZero() && !o.UpdatedAt.Before(w.Since) && o.UpdatedAt.Before(w.Until) {
			in = append(in, o)
		}
	}
	delays := make([]int64, len(in))
	for i, o := range in {
		delays[i] = max(0, o.UpdatedAt.Sub(o.CreatedAt).Milliseconds())
	}
	p50, p95 := p50p95(delays)
	obs[id] = Observation{Den: int64(len(delays)), P95: p95}
	met[id] = Metric{Value: p95, Extra: map[string]any{"p50": p50, "p95": p95}}
}

// doctorRead reports whether the view holds a doctor run that answered.
func doctorRead(e env) bool {
	return e.srv != nil && e.srv.Have["doctor"] && e.srv.DoctorErr == nil
}

// Doctor statuses, from the quietest to the loudest.
var doctorStatuses = []string{"ok", "info", "warn", "error"}

// doctorEntries is the doctor's checks as the dashboard keeps them: one per
// id (the first appearance's place), with the worst status among those that
// share it and the fix of a check of that status. A fix that is not a
// `backsight ...` command is dropped, and a check of an unknown status is
// ignored: the doctor's message text never enters a view.
func doctorEntries(checks []DoctorCheck) []DoctorCheck {
	var out []DoctorCheck
	at := map[string]int{}
	for _, c := range checks {
		r := slices.Index(doctorStatuses, c.Status)
		if r < 0 {
			continue
		}
		if !strings.HasPrefix(c.Fix, "backsight ") || strings.TrimSpace(c.Fix) == "backsight" {
			c.Fix = ""
		}
		i, seen := at[c.ID]
		switch {
		case !seen:
			at[c.ID] = len(out)
			out = append(out, DoctorCheck{ID: c.ID, Status: c.Status, Fix: c.Fix})
		case r > slices.Index(doctorStatuses, out[i].Status):
			out[i].Status, out[i].Fix = c.Status, c.Fix
		case r == slices.Index(doctorStatuses, out[i].Status) && out[i].Fix == "":
			out[i].Fix = c.Fix
		}
	}
	return out
}

// pipelineDoctor fills doctor.check from the doctor run.
func pipelineDoctor(e env, obs map[string]Observation, met map[string]Metric) {
	const id = "doctor.check"
	entries := doctorEntries(e.srv.Doctor)
	by := map[string]int64{}
	for _, s := range doctorStatuses {
		by[s] = 0
	}
	for _, c := range entries {
		by[c.Status]++
	}
	extra := map[string]any{}
	for s, n := range by {
		extra[s] = n
	}
	obs[id] = Observation{Err: ruleDoctor(entries)}
	met[id] = Metric{Value: ptr(float64(by["error"])), Extra: extra}
}

// jobCadences are the jobs the dashboard follows, with how often each is
// due.
var jobCadences = []struct {
	name    string
	cadence time.Duration
}{{"tick", tickCadence}, {"maintain", maintainCadence}}

// pipelineJobs fills jobs.last_run: the latest run of each job, as of now.
func pipelineJobs(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	const id = "jobs.last_run"
	reason, none, err := telemetryAbsent(e, "tel_job")
	if err != nil {
		return err
	}
	if !none && !isCurrent(e, w) {
		reason, none = ReasonNoEvents, true
	}
	if none {
		obs[id], met[id] = Observation{Absent: true, Reason: reason}, Metric{}
		return nil
	}
	extra := map[string]any{}
	status := StatusOK
	var tickAge *float64
	var seen bool
	for _, j := range jobCadences {
		var ts int64
		var st sql.NullString
		var ms sql.NullInt64
		err := e.tx.QueryRowContext(e.ctx, `
SELECT ts, status, ms FROM tel_job WHERE name = ? AND ts <= ? ORDER BY ts DESC, source_file DESC, "offset" DESC LIMIT 1`, j.name, e.now.UnixMilli()).Scan(&ts, &st, &ms)
		if errors.Is(err, sql.ErrNoRows) {
			extra[j.name] = nil
			continue
		}
		if err != nil {
			return err
		}
		seen = true
		last := time.UnixMilli(ts)
		age := max(0, e.now.Sub(last))
		var active bool
		err = e.tx.QueryRowContext(e.ctx, `
SELECT EXISTS(SELECT 1 FROM tel_prompt WHERE ts > ? AND ts <= ? AND (reason IS NULL OR reason <> 'machine'))
    OR EXISTS(SELECT 1 FROM tel_tool WHERE ts > ? AND ts <= ?)`,
			last.Add(j.cadence).UnixMilli(), e.now.UnixMilli(), last.Add(j.cadence).UnixMilli(), e.now.UnixMilli()).Scan(&active)
		if err != nil {
			return err
		}
		status = worst(status, ruleJobs(age, j.cadence, active))
		extra[j.name] = map[string]any{"last_at": last.In(w.Loc), "status": st.String, "ms": ms.Int64, "age_ms": age.Milliseconds()}
		if j.name == "tick" {
			tickAge = ptr(float64(age.Milliseconds()))
		}
	}
	if !seen {
		obs[id], met[id] = Observation{Absent: true, Reason: ReasonNoEvents}, Metric{}
		return nil
	}
	obs[id], met[id] = Observation{Err: status}, Metric{Value: tickAge, Extra: extra}
	return nil
}
