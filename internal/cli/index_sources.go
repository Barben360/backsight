package cli

import (
	"io"
	"io/fs"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// indexRegistry is the migrations every cli caller passes to the index;
// tests swap it for a registry of another head.
var indexRegistry = func() fs.FS { return index.Embedded }

// indexOptions are the metrics index's options for every cli caller:
// the ledger through its default reader, the local status from this PC's
// state files, progress to progress (nil: silent).
func indexOptions(d paths.Dirs, home string, progress io.Writer) index.Options {
	return index.Options{Dirs: d, Home: home, Migrations: indexRegistry(), LocalStatus: localStatusOf(d), Progress: progress}
}

// localStatusOf reads the local_status row's sources: reach.json and
// breaker.json (unreadable: nothing known), the notes outbox and its
// quarantine, and state.json's backup verify, update check (whether it
// failed, never its error) and backfill. Counts and times only.
func localStatusOf(d paths.Dirs) func() (index.LocalStatus, error) {
	return func() (index.LocalStatus, error) {
		var ls index.LocalStatus
		r := state.LoadReach(d)
		ls.ReachLastOK, ls.ReachFailingSince, ls.SessionsWaiting = r.LastOK, r.FailingSince, r.SessionsWaiting
		b := state.LoadBreaker(d)
		ls.BreakerFailures, ls.BreakerOpenUntil = b.Failures, b.OpenUntil

		ob := notesOutbox(d)
		q := retain.Outbox{Path: ob.QuarantinePath}
		var err error
		if ls.NotesQueued, err = ob.Pending(); err != nil {
			return index.LocalStatus{}, err
		}
		if ls.NotesOldestAt, err = ob.Oldest(); err != nil {
			return index.LocalStatus{}, err
		}
		if ls.Quarantined, err = ob.Quarantined(); err != nil {
			return index.LocalStatus{}, err
		}
		if ls.QuarantineOldestAt, err = q.Oldest(); err != nil {
			return index.LocalStatus{}, err
		}
		st, err := state.Load(d.StateFile())
		if err != nil {
			return index.LocalStatus{}, err
		}
		if v := st.BackupVerify; v != nil {
			ls.BackupCreatedAt, ls.BackupVerifiedAt, ls.BackupOK = v.CreatedAt, v.VerifiedAt, v.OK
		}
		if u := st.UpdateCheck; u != nil {
			ls.UpdateCheckedAt, ls.UpdateNewer, ls.UpdateCheckFailed = u.CheckedAt, u.Newer, u.Error != ""
		}
		if bf := st.Backfill; bf != nil {
			ls.BackfillSessions, ls.BackfillPaused = len(bf.Sessions), bf.Paused != ""
		}
		return ls, nil
	}
}
