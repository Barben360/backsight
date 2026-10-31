package index

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

// LocalStatus is what this PC's own state files say, counts and times
// only: the notes outbox and quarantine, the server's reach, the circuit
// breaker, the last backup verify, the last update check (failed or not,
// never its error) and the backfill.
type LocalStatus struct {
	NotesQueued, Quarantined, SessionsWaiting, BreakerFailures, UpdateNewer, BackfillSessions int
	NotesOldestAt, QuarantineOldestAt, ReachLastOK, ReachFailingSince, BreakerOpenUntil,
	BackupCreatedAt, BackupVerifiedAt, UpdateCheckedAt time.Time
	BackupOK, UpdateCheckFailed, BackfillPaused bool
}

// localCols are local_status's columns but id and ingested_at, in the
// order of localValues.
const localCols = `notes_queued, notes_oldest_at, quarantined, quarantine_oldest_at, sessions_waiting,
	reach_last_ok, reach_failing_since, breaker_failures, breaker_open_until, backup_created_at,
	backup_verified_at, backup_ok, update_checked_at, update_newer, update_check_failed,
	backfill_sessions, backfill_paused`

// localValues is ls as local_status stores it: int64, unix ms or NULL,
// 0/1.
func localValues(ls LocalStatus) []any {
	n := func(i int) any { return int64(i) }
	b := func(v bool) any {
		if v {
			return int64(1)
		}
		return int64(0)
	}
	return []any{n(ls.NotesQueued), msOrNil(ls.NotesOldestAt), n(ls.Quarantined), msOrNil(ls.QuarantineOldestAt),
		n(ls.SessionsWaiting), msOrNil(ls.ReachLastOK), msOrNil(ls.ReachFailingSince), n(ls.BreakerFailures),
		msOrNil(ls.BreakerOpenUntil), msOrNil(ls.BackupCreatedAt), msOrNil(ls.BackupVerifiedAt), b(ls.BackupOK),
		msOrNil(ls.UpdateCheckedAt), n(ls.UpdateNewer), b(ls.UpdateCheckFailed), n(ls.BackfillSessions), b(ls.BackfillPaused)}
}

// refreshLocal replaces the local_status row with what
// Options.LocalStatus reads (nil: not refreshed), only when a value
// changed: its ingested_at is the time the row last changed, and a
// catch-up that finds the same values takes no write lock. Its error
// keeps the row and is recorded once (local_status_unreadable) until a
// good read clears it. Only the index's own errors come back.
func (ix *Index) refreshLocal(ctx context.Context) error {
	if ix.opts.LocalStatus == nil {
		return nil
	}
	ls, err := ix.opts.LocalStatus()
	if err != nil {
		return ix.recordOnce(ctx, stateSource, 0, errLocalStatusUnreadable)
	}
	vals := localValues(ls)
	same := false
	if err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		old := make([]any, len(vals))
		ptrs := make([]any, len(vals))
		for i := range old {
			ptrs[i] = &old[i]
		}
		err := tx.QueryRowContext(ctx, `SELECT `+localCols+` FROM local_status WHERE id = 1`).Scan(ptrs...)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		flagged, err := hasErrorRow(ctx, tx, stateSource, 0)
		same = !flagged && slices.Equal(old, vals)
		return err
	}); err != nil || same {
		return err
	}
	tx, err := ix.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := clearError(ctx, tx, stateSource); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO local_status(id, ingested_at, `+localCols+`)
		VALUES (1, ?`+strings.Repeat(", ?", len(vals))+`)`, append([]any{ix.opts.Now().UnixMilli()}, vals...)...)
	if err != nil {
		return err
	}
	return tx.Commit()
}
