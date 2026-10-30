package index

import (
	"context"
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

// refreshLocal replaces the local_status row with what
// Options.LocalStatus reads (nil: not refreshed). Its error keeps the row
// and is recorded once (local_status_unreadable) until a good read clears
// it. Only the index's own errors come back.
func (ix *Index) refreshLocal(ctx context.Context) error {
	if ix.opts.LocalStatus == nil {
		return nil
	}
	ls, err := ix.opts.LocalStatus()
	if err != nil {
		return ix.recordOnce(ctx, stateSource, 0, errLocalStatusUnreadable)
	}
	tx, err := ix.sh.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := clearError(ctx, tx, stateSource); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO local_status(id, ingested_at, notes_queued, notes_oldest_at,
		quarantined, quarantine_oldest_at, sessions_waiting, reach_last_ok, reach_failing_since, breaker_failures,
		breaker_open_until, backup_created_at, backup_verified_at, backup_ok, update_checked_at, update_newer,
		update_check_failed, backfill_sessions, backfill_paused)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ix.opts.Now().UnixMilli(), ls.NotesQueued, msOrNil(ls.NotesOldestAt), ls.Quarantined, msOrNil(ls.QuarantineOldestAt),
		ls.SessionsWaiting, msOrNil(ls.ReachLastOK), msOrNil(ls.ReachFailingSince), ls.BreakerFailures,
		msOrNil(ls.BreakerOpenUntil), msOrNil(ls.BackupCreatedAt), msOrNil(ls.BackupVerifiedAt), ls.BackupOK,
		msOrNil(ls.UpdateCheckedAt), ls.UpdateNewer, ls.UpdateCheckFailed, ls.BackfillSessions, ls.BackfillPaused)
	if err != nil {
		return err
	}
	return tx.Commit()
}
