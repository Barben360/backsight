package index

import (
	"context"
	"database/sql"
	"time"

	"github.com/Barben360/backsight/internal/session"
)

// The queries jobs maintain's memory-docs step asks of the record: the
// days already listed, this PC's recent sends and the operations already
// written.

// ListedDays is every UTC day of the memory_day view, ascending, once each;
// complete when a day's latest line is complete (the record covers the
// whole bank up to it). A day that is not a date is left out.
func (ix *Index) ListedDays(ctx context.Context) (days []string, complete bool, err error) {
	err = ix.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT day, coalesce(complete, 0) FROM memory_day ORDER BY day`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var day string
			var c int64
			if err := rows.Scan(&day, &c); err != nil {
				return err
			}
			if _, perr := time.Parse(time.DateOnly, day); perr != nil {
				continue
			}
			days = append(days, day)
			complete = complete || c == 1
		}
		return rows.Err()
	})
	return days, complete, err
}

// SentDocument is a session document this PC sent with status ok, at the
// latest such send.
type SentDocument struct {
	Document, Session string
	SentAt            time.Time
}

// SentDocuments lists the session documents (session.OfDocument) retained
// ok at or after since, by document; notes, imports and any other
// document are never returned (their ids may hold a path).
func (ix *Index) SentDocuments(ctx context.Context, since time.Time) ([]SentDocument, error) {
	var out []SentDocument
	err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT document, max(ts) FROM tel_retain
			WHERE status = 'ok' AND document IS NOT NULL AND ts >= ?
			GROUP BY document ORDER BY document`, since.UnixMilli())
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
			if sid, ok := session.OfDocument(doc); ok {
				out = append(out, SentDocument{Document: doc, Session: sid, SentAt: time.UnixMilli(ts).UTC()})
			}
		}
		return rows.Err()
	})
	return out, err
}

// RecordedOp is what the record holds of a retain operation: its latest
// status, and whether a line of it holds the document's unit count.
type RecordedOp struct {
	Status   string
	HasUnits bool
}

// RecordedOperations is every operation of the doc_op view by id.
func (ix *Index) RecordedOperations(ctx context.Context) (map[string]RecordedOp, error) {
	out := map[string]RecordedOp{}
	err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT d.operation_id, coalesce(d.status, ''),
			EXISTS (SELECT 1 FROM doc_ops o WHERE o.operation_id = d.operation_id AND o.units IS NOT NULL)
			FROM doc_op d`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, status string
			var units int64
			if err := rows.Scan(&id, &status, &units); err != nil {
				return err
			}
			out[id] = RecordedOp{Status: status, HasUnits: units == 1}
		}
		return rows.Err()
	})
	return out, err
}
