package ledgerdb

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

// setAsideEvery bounds the automatic recovery: a corrupt file is set
// aside at most once in that time, judged by the mtime of the
// ledger.db.corrupt it leaves (stamped at the set-aside). The journal
// rebuilds the file, so no other stop rule is needed.
const setAsideEvery = 24 * time.Hour

// holdWhileRenaming: the corrupt file stays locked while it is renamed.
// Windows refuses to rename an open file: there the lock is released
// first.
var holdWhileRenaming = runtime.GOOS != "windows"

// corruptErr is a corruption of path (SQLITE_CORRUPT, SQLITE_NOTADB): the
// file OpenWriter sets aside.
type corruptErr struct {
	path string
	err  error
}

func (e *corruptErr) Error() string { return e.err.Error() }
func (e *corruptErr) Unwrap() error { return e.err }

// asCorrupt is err as path's corruption when it is one, else err.
func asCorrupt(path string, err error) error {
	var ce *corruptErr
	if err == nil || errors.As(err, &ce) {
		return err
	}
	if c := sqlitedb.Code(err); c != "corrupt" && c != "notadb" {
		return err
	}
	return &corruptErr{path: path, err: err}
}

// setAside moves the corrupt file ce.path to <path>.corrupt (replacing an
// older one), after removing its -wal, -shm and -journal, and stamps the
// moved file's mtime with now. It does not when:
//   - an update is uncommitted: ErrUpdateInProgress (the fresh file is a
//     migration), nothing moved;
//   - <path>.corrupt is less than setAsideEvery old: ErrUnreadable, the
//     file left in place;
//   - another connection holds the file (sqlitedb.Seize): ErrBusy,
//     nothing moved.
func setAside(ctx context.Context, o Options, ce *corruptErr) error {
	if err := sqlitedb.UpdateGate(o.Dirs); err != nil {
		return err
	}
	now := o.Now()
	aside := ce.path + ".corrupt"
	if fi, err := os.Stat(aside); err == nil && within(fi.ModTime(), now) {
		return fmt.Errorf("%w: %w (%s was set aside less than a day ago)", ErrUnreadable, ce.err, filepath.Base(aside))
	}
	release, err := sqlitedb.Seize(ctx, ce.path)
	if errors.Is(err, sqlitedb.ErrInUse) {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	defer release()
	if !holdWhileRenaming {
		release()
	}
	if err := removeCompanions(ce.path); err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	if err := fsutil.RenameRetry(ce.path, aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	// The rename kept the corrupt file's own mtime; the day counts from
	// now. A stamp that fails only lets the next corruption be set aside
	// sooner.
	_ = os.Chtimes(aside, now, now)
	return nil
}

// within: at is set, not after now, and less than setAsideEvery before
// it.
func within(at, now time.Time) bool {
	return !at.IsZero() && !now.Before(at) && now.Sub(at) < setAsideEvery
}

// removeCompanions removes path's -wal, -shm and -journal.
func removeCompanions(path string) error {
	var errs []error
	for _, sfx := range []string{"-wal", "-shm", "-journal"} {
		if err := fsutil.RemoveRetry(path + sfx); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
