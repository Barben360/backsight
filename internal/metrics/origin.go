package metrics

import (
	"database/sql"
	"errors"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/session"
)

// memoryOrigin says where memory id came from. It reads the memory_doc
// view (the latest day-list line of the memory) and the local telemetry,
// nothing else: the server is not asked, and the document id is read only
// to find its session, never returned. No line yet: unknown. A session's
// document is this PC's when the session has prompts here (then the times
// of its way, as pipeline.time_to_recall reads them, and the memory's first
// show), another PC's otherwise; any other document, and an observation
// (no document), is no session's.
func memoryOrigin(e env, id string) (*Origin, error) {
	var kind, doc, sess, created sql.NullString
	err := e.tx.QueryRowContext(e.ctx, `SELECT doc_kind, document, session, created_day FROM memory_doc WHERE memory_id = ?`, id).
		Scan(&kind, &doc, &sess, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return &Origin{State: OriginUnknown}, nil
	}
	if err != nil {
		return nil, err
	}
	o := &Origin{State: OriginNoSession, DocKind: kind.String}
	sid := sess.String
	if sid == "" && kind.String == "session" {
		sid, _ = session.OfDocument(doc.String)
	}
	if kind.String != "session" || sid == "" {
		return o, nil
	}
	o.Session = sid
	s, err := originSession(e, sid)
	if err != nil {
		return nil, err
	}
	if s == nil {
		o.State = OriginOtherPC
		if created.Valid {
			o.Created = &created.String
		}
		return o, nil
	}
	o.State, o.Project = OriginThisPC, s.project
	loc := viewLoc(e)
	at := func(ms int64) *time.Time {
		if ms < 0 {
			return nil
		}
		t := time.UnixMilli(ms).In(loc)
		return &t
	}
	o.Ended, o.Sent, o.Extracted = at(s.t0), at(s.t1), at(s.t2)
	var first sql.NullInt64
	if err := e.tx.QueryRowContext(e.ctx, `SELECT MIN(ts) FROM shows WHERE memory_id = ? AND COALESCE(path, '') <> ?`, id, feedback.PathSources).Scan(&first); err != nil {
		return nil, err
	}
	if first.Valid {
		o.FirstShown = at(first.Int64)
	}
	return o, nil
}

// originSession is the way of session sid on this PC (its last prompt, its
// send and the extraction, read by the same readers as the time to recall),
// nil when it has no prompt here.
func originSession(e env, sid string) (*recallSession, error) {
	s := &recallSession{id: sid, t1: -1, t2: -1, t3: -1}
	var project sql.NullString
	err := e.tx.QueryRowContext(e.ctx, `
SELECT ts, project FROM tel_prompt WHERE session = ? ORDER BY ts DESC, source_file DESC, "offset" DESC LIMIT 1`, sid).Scan(&s.t0, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.project = project.String
	by := map[string]*recallSession{sid: s}
	if err := readSends(e, by, s.t0); err != nil {
		return nil, err
	}
	if s.t1 >= 0 {
		if err := readOps(e, by, s.t1); err != nil {
			return nil, err
		}
	}
	return s, nil
}
