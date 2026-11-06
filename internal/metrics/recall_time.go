package metrics

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/session"
)

// pipeline.time_to_recall: for each session of this PC, how long from its
// last prompt until a memory made from it comes back in another session,
// in three steps: sent, turned into memories, shown again.
//
//	T0 the session's last prompt (the session ended: T0 is at least 30
//	   minutes before now, and falls in the window)
//	T1 its first successful send at or after T0
//	T2 the update of the first completed operation of its documents created
//	   at most 120 s before T1 (clock skew) whose memory count is not 0
//	T3 the first show at or after T2 of a memory made from its documents

const timeToRecallID = "pipeline.time_to_recall"

func init() { register("recall_time", []string{timeToRecallID}, timeToRecall) }

// Timings of the figure.
const (
	sessionEnded = 30 * time.Minute  // a session quieter for this long has ended
	opSkew       = 120 * time.Second // an operation may be created this long before its send
	neverAfter   = 24 * time.Hour    // sent for this long with no memory made: never
)

// The hops, in order, and the funnel's keys: the names the views read.
var (
	HopNames   = []string{"last_to_sent", "sent_to_extracted", "extracted_to_recalled", "whole_way"}
	FunnelKeys = []string{"ended", "sent", "waiting_or_failed", "extracted", "never", "on_server", "recalled", "not_recalled", "unresolved", "before_record", "gone"}
	// NeverStates are the states of a never_extracted row.
	NeverStates = []string{"failed", "no_memory", "none"}
)

// Hop is one step of the way: the typical and the slowest 1 in 20 duration
// (ms) over the sessions that went through it, with their number.
type Hop struct {
	Name string   `json:"name"`
	P50  *float64 `json:"p50"`
	P95  *float64 `json:"p95"`
	N    int64    `json:"n"`
}

// NeverExtractedRow is a session sent a day ago or more that gave no
// memory: why.
type NeverExtractedRow struct {
	Session string    `json:"session"`
	Project string    `json:"project"`
	Sent    time.Time `json:"sent"`
	State   string    `json:"state"`
	Retries int       `json:"retries"`
}

// neverExtractedRows is how many rows the table keeps; the sessions view's
// filter keeps every session.
const neverExtractedRows = 50

// recallSession is one session of the window and the times of its way, in
// unix ms (-1: not reached).
type recallSession struct {
	id, project    string
	t0, t1, t2, t3 int64
	failed         bool // an operation of its send failed
	retries        int
	noMemory       bool // an operation of its send completed with 0 memories
	beforeRecord   bool // sent before the operations record began: what became of it is not known
}

// recallRun is what timeToRecall reads: the sessions that ended in the
// window, and the shown memories whose document the day lists do not give.
type recallRun struct {
	sessions         []*recallSession
	unresolved, gone int64
}

// stale reports whether s was sent a day ago or more and gave no memory.
func (s *recallSession) stale(now time.Time) bool {
	return s.t1 >= 0 && s.t2 < 0 && now.Sub(time.UnixMilli(s.t1)) >= neverAfter
}

// never is stale for a send the operations record covers; one from before
// the record began is before_record, not never.
func (s *recallSession) never(now time.Time) bool { return s.stale(now) && !s.beforeRecord }

func (s *recallSession) state() string {
	switch {
	case s.failed:
		return "failed"
	case s.noMemory:
		return "no_memory"
	}
	return "none"
}

// readRecall is the run over w, read once per transaction.
func readRecall(e env, w Window) (*recallRun, error) {
	k := [2]int64{w.Since.UnixNano(), w.Until.UnixNano()}
	if e.memo != nil {
		if run, ok := e.memo.recall[k]; ok {
			return run, nil
		}
	}
	run, err := loadRecall(e, w)
	if err == nil && e.memo != nil {
		if e.memo.recall == nil {
			e.memo.recall = map[[2]int64]*recallRun{}
		}
		e.memo.recall[k] = run
	}
	return run, err
}

// loadRecall reads the sessions of the window and their way.
func loadRecall(e env, w Window) (*recallRun, error) {
	run := &recallRun{}
	bySession := map[string]*recallSession{}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT session, MAX(ts), CASE WHEN ? = '' THEN COALESCE(project, '') ELSE ? END FROM tel_prompt
WHERE ts >= ? AND COALESCE(session, '') <> ''
GROUP BY session HAVING MAX(ts) < ? AND MAX(ts) <= ? AND (? = '' OR COUNT(CASE WHEN project = ? THEN 1 END) > 0)`,
		// the prompts from the window's start: a session's last one is among
		// them, of any project; the project only picks the sessions that ran in it
		e.q.Project, e.q.Project, w.Since.UnixMilli(), w.Until.UnixMilli(), e.now.Add(-sessionEnded).UnixMilli(), e.q.Project, e.q.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	minT0 := int64(-1)
	for rows.Next() {
		s := &recallSession{t1: -1, t2: -1, t3: -1}
		if err := rows.Scan(&s.id, &s.t0, &s.project); err != nil {
			return nil, err
		}
		bySession[s.id] = s
		run.sessions = append(run.sessions, s)
		if minT0 < 0 || s.t0 < minT0 {
			minT0 = s.t0
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(run.sessions) > 0 {
		if err := readSends(e, bySession, minT0); err != nil {
			return nil, err
		}
		if err := readOps(e, bySession, minT1(bySession)); err != nil {
			return nil, err
		}
		if err := readRecalls(e, bySession); err != nil {
			return nil, err
		}
	}
	if err := readUnresolved(e, w, run); err != nil {
		return nil, err
	}
	slices.SortFunc(run.sessions, func(a, b *recallSession) int { return strings.Compare(a.id, b.id) })
	return run, nil
}

// readSends sets T1: the first successful send at or after T0 of a
// document of the session.
func readSends(e env, by map[string]*recallSession, minT0 int64) error {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT document, ts FROM tel_retain
WHERE status = 'ok' AND document IS NOT NULL AND ts >= ? ORDER BY ts, source_file, "offset"`, minT0)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var doc string
		var ts int64
		if err := rows.Scan(&doc, &ts); err != nil {
			return err
		}
		sid, ok := session.OfDocument(doc)
		if s := by[sid]; ok && s != nil && s.t1 < 0 && ts >= s.t0 {
			s.t1 = ts
		}
	}
	return rows.Err()
}

// minT1 is the earliest send of the sessions, -1 when none was sent.
func minT1(by map[string]*recallSession) int64 {
	m := int64(-1)
	for _, s := range by {
		if s.t1 >= 0 && (m < 0 || s.t1 < m) {
			m = s.t1
		}
	}
	return m
}

// readOps sets T2 and, for a send with no memory made, why: the operations
// created from 120 s before the send on, each as its latest line says (the
// doc_op view's reading), in the order of creation, then write, then id. A send from before the
// operations record began is marked beforeRecord (no record: all of them).
func readOps(e env, by map[string]*recallSession, minT1 int64) error {
	var start sql.NullInt64
	if err := e.tx.QueryRowContext(e.ctx, `SELECT MIN(ts) FROM doc_ops`).Scan(&start); err != nil {
		return err
	}
	for _, s := range by {
		s.beforeRecord = s.t1 >= 0 && (!start.Valid || s.t1 < start.Int64)
	}
	if minT1 < 0 || !start.Valid {
		return nil
	}
	// The operations whose latest line (as the doc_op view reads it) is a
	// document's and was created in time, oldest first. The inner select
	// only narrows the lines read to the operations that have a line
	// created in time; the latest line of each decides.
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT document, status, created_at, updated_at, COALESCE(retries, 0), units FROM (
  SELECT d.*, ROW_NUMBER() OVER (PARTITION BY d.operation_id ORDER BY d.ts DESC, d.source_file DESC, d."offset" DESC) AS rn
  FROM doc_ops d
  WHERE d.operation_id IN (SELECT operation_id FROM doc_ops WHERE created_at >= ? AND operation_id IS NOT NULL))
WHERE rn = 1 AND document IS NOT NULL AND created_at >= ?
ORDER BY created_at, ts, operation_id`, minT1-opSkew.Milliseconds(), minT1-opSkew.Milliseconds())
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type op struct {
		doc, status      string
		created, retries int64
		updated, units   sql.NullInt64
	}
	var ops []op
	for rows.Next() {
		var o op
		if err := rows.Scan(&o.doc, &o.status, &o.created, &o.updated, &o.retries, &o.units); err != nil {
			return err
		}
		ops = append(ops, o)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range ops {
		sid, ok := session.OfDocument(o.doc)
		s := by[sid]
		if !ok || s == nil || s.t1 < 0 || o.created < s.t1-opSkew.Milliseconds() {
			continue
		}
		switch {
		case o.status == "failed":
			s.failed, s.retries = true, max(s.retries, int(o.retries))
		case o.status == "completed" && o.units.Valid && o.units.Int64 == 0:
			s.noMemory = true
		case o.status == "completed" && s.t2 < 0: // no count yet counts as extracted
			s.t2 = o.created
			if o.updated.Valid {
				s.t2 = o.updated.Int64
			}
		}
	}
	return nil
}

// readRecalls sets T3: the first show at or after T2 of a memory whose
// document is a session's (an observation has none).
func readRecalls(e env, by map[string]*recallSession) error {
	minT2 := int64(-1)
	for _, s := range by {
		if s.t2 >= 0 && (minT2 < 0 || s.t2 < minT2) {
			minT2 = s.t2
		}
	}
	if minT2 < 0 {
		return nil
	}
	of := map[string]*recallSession{} // memory -> the session its document belongs to
	var ids []any
	for id, s := range by {
		if s.t2 >= 0 {
			ids = append(ids, id)
		}
	}
	for len(ids) > 0 { // the memories of these sessions' documents only, in chunks of bound values
		n := min(len(ids), 500)
		rows, err := e.tx.QueryContext(e.ctx, `
SELECT memory_id, document FROM (
  SELECT d.memory_id, d.document, ROW_NUMBER() OVER (PARTITION BY d.memory_id ORDER BY d.ts DESC, d.source_file DESC, d."offset" DESC) AS rn
  FROM memory_docs d
  WHERE d.memory_id IN (SELECT memory_id FROM memory_docs WHERE memory_id IS NOT NULL AND session IN (?`+strings.Repeat(",?", n-1)+`)))
WHERE rn = 1 AND document IS NOT NULL`, ids[:n]...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var mem, doc string
			if err := rows.Scan(&mem, &doc); err != nil {
				_ = rows.Close()
				return err
			}
			if sid, ok := session.OfDocument(doc); ok && by[sid] != nil && by[sid].t2 >= 0 {
				of[mem] = by[sid]
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		ids = ids[n:]
	}
	if len(of) == 0 {
		return nil
	}
	shows, err := e.tx.QueryContext(e.ctx, `
SELECT memory_id, ts FROM shows WHERE memory_id IS NOT NULL AND ts >= ? ORDER BY ts, source_file, "offset", pos`, minT2)
	if err != nil {
		return err
	}
	defer func() { _ = shows.Close() }()
	for shows.Next() {
		var mem string
		var ts int64
		if err := shows.Scan(&mem, &ts); err != nil {
			return err
		}
		if s := of[mem]; s != nil && s.t3 < 0 && ts >= s.t2 {
			s.t3 = ts
		}
	}
	return shows.Err()
}

// readUnresolved counts the memories shown in the window that no day list
// has given a row: gone when the record covers their first show's day (they
// were forgotten before it was listed), unresolved otherwise.
func readUnresolved(e env, w Window, run *recallRun) error {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT (SELECT MIN(f.ts) FROM shows f WHERE f.memory_id = c.memory_id)
FROM (SELECT memory_id FROM shows WHERE memory_id IS NOT NULL AND ts >= ? AND ts < ? GROUP BY memory_id) c
WHERE NOT EXISTS (SELECT 1 FROM memory_docs d WHERE d.memory_id = c.memory_id)`, w.Since.UnixMilli(), w.Until.UnixMilli())
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var firsts []int64
	for rows.Next() {
		var first int64
		if err := rows.Scan(&first); err != nil {
			return err
		}
		firsts = append(firsts, first)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(firsts) == 0 { // none to place: the coverage is not read
		return nil
	}
	covered, err := coveredUntil(e)
	if err != nil {
		return err
	}
	for _, first := range firsts {
		if goneBy(covered, first) {
			run.gone++
		} else {
			run.unresolved++
		}
	}
	return nil
}

// coveredUntil is the last day (UTC date) the day lists cover, NULL when
// the record does not yet cover the bank (the memory_coverage view).
func coveredUntil(e env) (sql.NullString, error) {
	var covered sql.NullString
	err := e.tx.QueryRowContext(e.ctx, `SELECT covered_until FROM memory_coverage`).Scan(&covered)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return covered, err
	}
	return covered, nil
}

// goneBy reports whether a memory without a row, first shown at firstMs
// (unix ms), is gone: the record covers the day it was first shown, so it
// was forgotten before that day was listed.
func goneBy(covered sql.NullString, firstMs int64) bool {
	return covered.Valid && time.UnixMilli(firstMs).UTC().Format("2006-01-02") <= covered.String
}

// neverRowsOf are the sessions that never gave a memory, newest send
// first.
func neverRowsOf(e env, run *recallRun) []NeverExtractedRow {
	var out []NeverExtractedRow
	for _, s := range run.sessions {
		if s.never(e.now) {
			out = append(out, NeverExtractedRow{Session: s.id, Project: s.project, Sent: time.UnixMilli(s.t1).In(viewLoc(e)),
				State: s.state(), Retries: s.retries})
		}
	}
	slices.SortFunc(out, func(a, b NeverExtractedRow) int {
		if c := b.Sent.Compare(a.Sent); c != 0 {
			return c
		}
		return strings.Compare(a.Session, b.Session)
	})
	return out
}

// neverExtracted is the pipeline view's table: the 50 newest.
func neverExtracted(e env, w Window) ([]NeverExtractedRow, error) {
	run, err := readRecall(e, w)
	if err != nil {
		return nil, err
	}
	rows := neverRowsOf(e, run)
	return rows[:min(len(rows), neverExtractedRows)], nil
}

// neverSessions are the sessions of every never_extracted row of w, as a
// set: what --has never_extracted keeps.
func neverSessions(e env, w Window) (map[string]bool, error) {
	run, err := readRecall(e, w)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range neverRowsOf(e, run) {
		out[r.Session] = true
	}
	return out, nil
}

// timeToRecall fills pipeline.time_to_recall. The figure is the whole way's
// typical time; the judged part is the slowest 1 in 20 of the sent-to-memory
// step. It never changes the verdict (it is no axis's metric) and has no
// attention item.
func timeToRecall(e env, w Window) (map[string]Observation, map[string]Metric, error) {
	reason, none, err := telemetryAbsent(e, "tel_prompt")
	if err != nil {
		return nil, nil, err
	}
	if none {
		return map[string]Observation{timeToRecallID: {Absent: true, Reason: reason}}, map[string]Metric{timeToRecallID: {}}, nil
	}
	run, err := readRecall(e, w)
	if err != nil {
		return nil, nil, err
	}
	var lastSent, sentExt, extRecalled, whole []int64
	f := map[string]int64{"ended": int64(len(run.sessions)), "unresolved": run.unresolved, "gone": run.gone}
	for _, s := range run.sessions {
		if s.t1 >= 0 {
			f["sent"]++
			lastSent = append(lastSent, max(0, s.t1-s.t0))
		}
		switch {
		case s.t2 >= 0:
			f["extracted"]++
			sentExt = append(sentExt, max(0, s.t2-s.t1))
		case s.never(e.now):
			f["never"]++
		case s.stale(e.now): // sent before the operations record began
			f["before_record"]++
		case s.t1 >= 0:
			f["on_server"]++
		}
		if s.t3 >= 0 {
			f["recalled"]++
			extRecalled = append(extRecalled, max(0, s.t3-s.t2))
			whole = append(whole, max(0, s.t3-s.t0))
		}
	}
	f["waiting_or_failed"] = f["ended"] - f["sent"]
	f["not_recalled"] = f["extracted"] - f["recalled"]
	for _, k := range FunnelKeys { // every key present, 0 when none
		f[k] += 0
	}
	var hops []Hop
	for i, xs := range [][]int64{lastSent, sentExt, extRecalled, whole} {
		p50, p95 := p50p95(xs)
		hops = append(hops, Hop{Name: HopNames[i], P50: p50, P95: p95, N: int64(len(xs))})
	}
	m := Metric{Value: hops[3].P50, Num: i64(f["recalled"]), Den: i64(f["sent"]),
		Extra: map[string]any{"funnel": f, "hops": hops}}
	if run.unresolved > 0 || f["before_record"] > 0 {
		m.Tags = []string{tagPartial}
	}
	o := Observation{Den: hops[1].N, P95: hops[1].P95}
	return map[string]Observation{timeToRecallID: o}, map[string]Metric{timeToRecallID: m}, nil
}
