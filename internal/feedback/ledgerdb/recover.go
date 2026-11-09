package ledgerdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	if afterSeize != nil {
		afterSeize()
	}
	// Another process may have set the file aside between the day check
	// and the lock: the file is then gone (no lock was needed), or a
	// fresh one stands where it was, its .corrupt just stamped. Its work
	// stands; the caller opens what is there.
	if _, err := os.Lstat(ce.path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if fi, err := os.Stat(aside); err == nil && within(fi.ModTime(), o.Now()) {
		return nil
	}
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

// afterSeize, when set (tests), runs once setAside holds the file.
var afterSeize func()

// recoverCorrupt answers err, a corruption a fold, a rebuild or an
// export met past the header (the open read that fine): w's connection
// is closed, for Seize wants the file whole, the file set aside, and w
// opened again on a fresh file built from the journal, which keeps the
// corrupt file's imports cut when that could still be read. When the
// set-aside cannot be done (another connection holds the file; one was
// set aside less than a day ago), w is opened again on the file as it
// is and the error, the corruption's and the set-aside's, returned: the
// next call tries again. When no file can be opened again, w is broken:
// every later call returns why.
func (w *Writer) recoverCorrupt(ctx context.Context, err error) error {
	var ce *corruptErr
	if !errors.As(asCorrupt(w.path, err), &ce) {
		return err
	}
	_ = w.db.Close()
	cut := storedCut(ctx, w.path)
	serr := setAside(ctx, w.opts, ce)
	nw, oerr := openWriterCut(ctx, w.opts, cut)
	if oerr != nil {
		w.broken = fmt.Errorf("%w: %s was closed after a corruption and could not be opened again: %w", ErrUnreadable, filepath.Base(w.path), oerr)
		if serr != nil {
			return fmt.Errorf("%w (it could not be set aside: %w)", w.broken, serr)
		}
		return w.broken
	}
	*w = *nw
	if serr != nil {
		return fmt.Errorf("%w (it could not be set aside: %w)", ce, serr)
	}
	return nil
}

// storedCut is the imports cut (imports_dropped_before_ns) a corrupt
// file still gives, read only; 0 when it gives none, or cannot be read.
func storedCut(ctx context.Context, path string) int64 {
	db, err := sql.Open(driverName, sqlitedb.ReaderDSN(path, 100*time.Millisecond))
	if err != nil {
		return 0
	}
	defer func() { _ = db.Close() }()
	var s string
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'imports_dropped_before_ns'`).Scan(&s); err != nil {
		return 0
	}
	cut, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return cut
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
