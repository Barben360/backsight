package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// The index is derived: a corrupt file is set aside and built again from
// the sources, at most once a day; a second failure within the day stops
// (doctor reports it, `stats index rebuild` lifts it).
const (
	checkEvery   = 24 * time.Hour // between two quick_checks at open
	rebuildEvery = 24 * time.Hour // a failure sooner after an automatic rebuild stops
)

var (
	// ErrUnreadable: the index is corrupt again within a day of its
	// automatic rebuild, or could not be set aside; it is left in place.
	ErrUnreadable = errors.New("the metrics index is unreadable")
	// ErrDiskFull: a write found the disk full; its transaction was rolled
	// back.
	ErrDiskFull = errors.New("the disk is full: the metrics index was left as it was")
	// errQuickCheck: PRAGMA quick_check found a problem.
	errQuickCheck = errors.New("the metrics index failed its integrity check")
)

// onQuickCheck runs before each quick_check (tests count them).
var onQuickCheck = func() {}

// afterConnect, when set, runs between Open's connect and its identity
// check (tests replace the file there).
var afterConnect func(path string)

// renameFile is os.Rename; tests make it fail as Windows does on a file
// held open.
var renameFile = os.Rename

// holdWhileRenaming: the old file stays locked while the new one is
// renamed over it, so that a process connecting meanwhile waits, then
// finds the replacement (Open's identity check). Windows refuses to rename
// over an open file: there the lock is released first, and a process that
// opens the file in between makes the rename fail instead.
var holdWhileRenaming = runtime.GOOS != "windows"

// classify is err's class, the only form an index error is stored or
// shown in: corrupt | notadb | full | busy | readonly | io | other ("" for
// nil).
func classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrDiskFull):
		return "full"
	case errors.Is(err, ErrUnreadable), errors.Is(err, errQuickCheck):
		return "corrupt"
	case errors.Is(err, ErrInUse):
		return "busy"
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_CORRUPT:
			return "corrupt"
		case sqlite3.SQLITE_NOTADB:
			return "notadb"
		case sqlite3.SQLITE_FULL:
			return "full"
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return "busy"
		case sqlite3.SQLITE_READONLY:
			return "readonly"
		case sqlite3.SQLITE_IOERR, sqlite3.SQLITE_CANTOPEN, sqlite3.SQLITE_PERM, sqlite3.SQLITE_NOLFS:
			return "io"
		}
		return "other"
	}
	if errors.Is(err, syscall.ENOSPC) {
		return "full"
	}
	var pe *fs.PathError
	var le *os.LinkError
	var sc *os.SyscallError
	if errors.As(err, &pe) || errors.As(err, &le) || errors.As(err, &sc) ||
		errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
		return "io"
	}
	return "other"
}

func isCorrupt(err error) bool {
	c := classify(err)
	return c == "corrupt" || c == "notadb"
}

// corruptErr is a corruption of path (SQLITE_CORRUPT, SQLITE_NOTADB, a
// failed quick_check): the file Open sets aside.
type corruptErr struct {
	path string
	err  error
}

func (e *corruptErr) Error() string { return e.err.Error() }
func (e *corruptErr) Unwrap() error { return e.err }

// asCorrupt is err as path's corruption when it is one, else err.
func asCorrupt(path string, err error) error {
	var ce *corruptErr
	if err == nil || errors.As(err, &ce) || !isCorrupt(err) {
		return err
	}
	return &corruptErr{path: path, err: err}
}

// openRecovering is open; a corrupt file is set aside and a fresh one
// opened (its rows come back with the caller's catch-up). A corruption
// again, in this call or within rebuildEvery, stops with ErrUnreadable:
// the bound on i only guards against a status that does not keep the
// stamp.
func openRecovering(ctx context.Context, o Options) (*Index, []string, error) {
	for i := 0; ; i++ {
		ix, applied, err := open(ctx, o)
		var ce *corruptErr
		if i == 2 || !errors.As(err, &ce) {
			return ix, applied, err
		}
		if err := setAside(ctx, o, ce); err != nil {
			return nil, nil, err
		}
	}
}

// setAside moves the corrupt file ce.path to <path>.corrupt (replacing an
// older one) and removes its -wal, -shm and -journal, unless:
//   - an update is uncommitted: ErrUpdateInProgress (the fresh file is a
//     migration), nothing moved;
//   - the last automatic rebuild is less than rebuildEvery old: StoppedAt
//     stamped, the file left in place, ErrUnreadable;
//   - this process holds the file, another connection does, or a rebuild
//     runs (rebuild.lock): ce comes back, nothing moved or stamped (the
//     quick_check stays due).
//
// RebuiltAt is stamped before anything moves, so that a status that
// cannot be written stops the recovery instead of letting it loop.
func setAside(ctx context.Context, o Options, ce *corruptErr) error {
	if err := updateGate(o.Dirs); err != nil {
		return err
	}
	mu.Lock() // no open of this process connects meanwhile
	defer mu.Unlock()
	if _, held := files[ce.path]; held {
		return ce
	}
	lock, err := rebuildLock(o.Dirs) // no rebuild runs meanwhile
	if errors.Is(err, ErrInUse) {
		return ce
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	defer func() { _ = lock.Release() }()
	now := o.Now()
	stop := false
	if err := updateStatus(o.Dirs, func(st *recoveryState) bool {
		if st.RebuiltAt.IsZero() || now.Sub(st.RebuiltAt) >= rebuildEvery {
			return false
		}
		stop = true
		if !st.StoppedAt.IsZero() && !st.StoppedAt.Before(st.RebuiltAt) {
			return false // stopped already since that rebuild
		}
		st.StoppedAt = now
		return true
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	if stop {
		return ErrUnreadable
	}
	release, err := seize(ctx, ce.path)
	if errors.Is(err, ErrInUse) {
		return ce
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	defer release()
	if err := updateStatus(o.Dirs, func(st *recoveryState) bool {
		st.RebuiltAt, st.StoppedAt = now, time.Time{}
		return true
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	if !holdWhileRenaming {
		release()
	}
	if err := removeCompanions(ce.path); err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	if err := renameFile(ce.path, ce.path+".corrupt"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	return nil
}

// lockDSN connects to a file to take it whole: no wait, and exclusive
// locking (no shared memory: the lock fails while any other connection
// has the file open in WAL mode, even idle).
func lockDSN(path string) string {
	return "file:" + uriEscaper.Replace(filepath.ToSlash(path)) + "?_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)"
}

// seize takes path's exclusive lock for a replacement and returns its
// release (idempotent). Any other connection: ErrInUse, nothing written (the lock is
// taken with a read only). Then the file leaves WAL mode, which folds and
// removes its -wal: no -wal of the old file is left for the new one, and
// closing this connection after the rename removes no file by name. A
// file SQLite cannot read (corrupt, not a database) cannot be locked
// through it either, and is of no use to whoever holds it: it is replaced
// as it is, with a no-op release. A missing file needs no lock.
func seize(ctx context.Context, path string) (func(), error) {
	noop := func() {}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return noop, nil
	}
	db, err := sql.Open(driverName, lockDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return seizeErr(err)
	}
	var once sync.Once
	release := func() { // idempotent
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			_ = db.Close()
		})
	}
	exec := func(q string) error { _, err := conn.ExecContext(ctx, q); return err }
	var n int64
	var mode string
	for _, step := range []func() error{
		func() error { return exec("BEGIN EXCLUSIVE") },
		func() error { return conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n) },
		func() error { return exec("COMMIT") },
		func() error {
			if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
				return err
			}
			if mode != "delete" {
				return ErrInUse
			}
			return nil
		},
		func() error { return exec("BEGIN EXCLUSIVE") },
	} {
		if err := step(); err != nil {
			release()
			return seizeErr(err)
		}
	}
	return release, nil
}

func seizeErr(err error) (func(), error) {
	switch {
	case classify(err) == "busy":
		return nil, ErrInUse
	case isCorrupt(err):
		return func() {}, nil
	}
	return nil, err
}

// removeCompanions removes path's -wal, -shm and -journal.
func removeCompanions(path string) error {
	var errs []error
	for _, sfx := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(path + sfx); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// dailyCheck runs PRAGMA quick_check when the last one that passed is
// checkEvery old (or unknown, or in the future), and stamps CheckedAt
// when it passes. Only a corruption, or the context's end, is an error:
// a check that cannot run (busy) is tried at the next open.
func (ix *Index) dailyCheck(ctx context.Context) error {
	now := ix.opts.Now()
	st, err := readStatus(ix.opts.Dirs)
	if err == nil && !st.CheckedAt.IsZero() && !now.Before(st.CheckedAt) && now.Sub(st.CheckedAt) < checkEvery {
		return nil
	}
	// A status that cannot be written leaves CheckedAt as it was: the
	// check then runs at every open (a cost, never a wrong verdict).
	onQuickCheck()
	if err := quickCheck(ctx, ix.sh.r); err != nil {
		if isCorrupt(err) {
			return err
		}
		return ctx.Err()
	}
	_ = updateStatus(ix.opts.Dirs, func(st *recoveryState) bool {
		st.CheckedAt = now
		return true
	})
	return nil
}

// quickCheck is PRAGMA quick_check: a single "ok" row, else
// errQuickCheck.
func quickCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(got) != 1 || got[0] != "ok" {
		return errQuickCheck
	}
	return nil
}

// statementError maps what a catch-up's statements returned: SQLITE_FULL
// becomes ErrDiskFull (the transaction was rolled back, nothing else
// changed); a corruption makes the quick_check due, so that the next open
// checks the file and sets it aside.
func (ix *Index) statementError(err error) error {
	switch classify(err) {
	case "full":
		return ErrDiskFull
	case "corrupt", "notadb":
		_ = updateStatus(ix.opts.Dirs, func(st *recoveryState) bool {
			if st.CheckedAt.IsZero() {
				return false
			}
			st.CheckedAt = time.Time{}
			return true
		})
	}
	return err
}
