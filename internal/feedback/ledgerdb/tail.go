package ledgerdb

import (
	"context"
	"database/sql"
	"path/filepath"

	"github.com/Barben360/backsight/internal/feedback"
)

// readTail is the journal past tx's cursors, as the reader applies it:
// each month file read from its cursor (from 0 without one) up to its
// last newline, the lines the ledger folds as they come (a forget,
// retire, hide, unhide or import: never a settle, which waits for its
// fold), in time order. Current applies the same lines past ledger.json's
// journal_at: a hide reaches the next recall, not the next fold.
func readTail(tx *sql.Tx, dir string) ([]feedback.Event, error) {
	cursors, err := readCursors(context.Background(), tx)
	if err != nil {
		return nil, err
	}
	files, err := feedback.MonthFiles(dir)
	if err != nil {
		return nil, err
	}
	var tail []feedback.Event
	for _, name := range files {
		events, _, _, err := feedback.ReadComplete(filepath.Join(dir, name), cursors[name])
		if err != nil {
			return nil, err
		}
		for _, e := range events {
			if feedback.Folds(e.Kind) {
				tail = append(tail, e)
			}
		}
	}
	return feedback.SortByTime(tail), nil
}

// applyTail applies tail to l as Current does (the half-lives each
// import recorded): a view, written nowhere. Only the events naming an
// id keep reports are applied when keep is not nil.
func applyTail(l *feedback.Ledger, tail []feedback.Event, keep func(id string) bool) {
	for _, e := range tail {
		if keep == nil || namesAny(e, keep) {
			l.Apply(e, feedback.Params{})
		}
	}
}

func namesAny(e feedback.Event, keep func(string) bool) bool {
	for _, id := range eventIDs(e) {
		if keep(id) {
			return true
		}
	}
	return false
}
