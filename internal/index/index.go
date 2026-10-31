package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
)

// FileName is the index file inside Dirs.IndexDir.
const FileName = "metrics.db"

type Options struct {
	Dirs          paths.Dirs
	Home          string              // for sync-folder prefixes
	Getenv        func(string) string // nil: os.Getenv
	Now           func() time.Time    // nil: time.Now
	BinaryVersion string              // "": buildinfo.Version
	Migrations    fs.FS               // nil: Embedded
	// Ledger is how the ledger table is filled; nil:
	// feedback.CurrentReader over Dirs.FeedbackDir().
	Ledger feedback.LedgerReader
	// LocalStatus reads the local_status row; nil: not refreshed.
	LocalStatus func() (LocalStatus, error)
	// Progress receives a long catch-up's progress (CLI callers pass
	// stderr); nil: silent.
	Progress io.Writer
}

func (o Options) withDefaults() Options {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.BinaryVersion == "" {
		o.BinaryVersion = buildinfo.Version
	}
	if o.Ledger == nil {
		o.Ledger = feedback.CurrentReader{Dir: o.Dirs.FeedbackDir()}
	}
	return o
}

// shared is the one pair of handles per file per process, with its
// catch-up in progress (fmu guards it).
type shared struct {
	w, r   *sql.DB
	fs     FSInfo
	refs   int
	fmu    sync.Mutex
	flight *flight
}

var (
	mu    sync.Mutex
	files = map[string]*shared{}
	opens = map[string]int64{}
)

type Index struct {
	sh     *shared
	path   string
	opts   Options
	side   bool
	schema int
	closed bool
}

// OpensOf is how many times this process opened an index under stateDir.
func OpensOf(stateDir string) int64 {
	mu.Lock()
	defer mu.Unlock()
	return opens[stateDir]
}

// Open opens the index under o.Dirs, building or migrating it first:
// (1) the registry and its head; (2) metrics.db's user_version, read only;
// (3) above the head, the side file of this binary's head is used instead
// and metrics.db never written; (4) a migration (building a missing file
// included) waits for an uncommitted update; (5) the file is opened, its
// version read again under the write lock (above the head: back to 3,
// once), and each pending migration applied in its own transaction; (6) a
// file replaced while it connected is opened again, once; (7) the daily
// quick_check. A corrupt file is set aside and a fresh one opened, once a
// day at most (recover.go).
func Open(ctx context.Context, o Options) (*Index, error) {
	ix, _, err := openRecovering(ctx, o.withDefaults())
	return ix, err
}

// open is Open without the recovery, with the migrations this call
// applied: the file resolved and opened, then its daily quick_check (a
// failure is the file's corruption).
func open(ctx context.Context, o Options) (*Index, []string, error) {
	t, err := resolve(ctx, o)
	if err != nil {
		return nil, nil, err
	}
	ix, applied, err := openTarget(ctx, o, t)
	if err != nil {
		return nil, nil, err
	}
	if err := ix.dailyCheck(ctx); err != nil {
		_ = ix.Close()
		return nil, nil, asCorrupt(ix.path, err)
	}
	countOpen(o)
	return ix, applied, nil
}

// openError is a connect's or a migration's error: a full disk is
// ErrDiskFull, a corruption path's.
func openError(path string, err error) error {
	if classify(err) == "full" {
		return ErrDiskFull
	}
	return asCorrupt(path, err)
}

// countOpen counts a successful open for OpensOf.
func countOpen(o Options) {
	mu.Lock()
	opens[o.Dirs.State]++
	mu.Unlock()
}

// openTarget opens t. A newer binary may have migrated metrics.db after
// the probe: the writer then finds it above the head and lets it go, and
// the second and last pass opens the side file.
func openTarget(ctx context.Context, o Options, t target) (*Index, []string, error) {
	ix, applied, err := openFollow(ctx, o, t)
	if !errors.Is(err, errNewer) || t.side {
		return ix, applied, err
	}
	if t, err = sideTarget(ctx, t); err != nil {
		return nil, nil, err
	}
	return openFollow(ctx, o, t)
}

// errReplaced: the file at the path is no longer the one connected to (a
// rebuild renamed a new file over it meanwhile).
var errReplaced = errors.New("the metrics index was replaced while it was opened")

// openFollow is openOnce, opened again once when the file was replaced
// while it connected: its version is probed again first.
func openFollow(ctx context.Context, o Options, t target) (*Index, []string, error) {
	ix, applied, err := openOnce(ctx, o, t, true)
	if !errors.Is(err, errReplaced) {
		return ix, applied, err
	}
	if err := t.probe(ctx); err != nil {
		return nil, nil, err
	}
	return openOnce(ctx, o, t, false)
}

// openOnce opens t.path and migrates it. With check, a first connect is
// checked against a replacement: the file at the path before connecting
// must be the one there after (afterConnect runs in between), else
// errReplaced, the handle closed, nothing written. Corruption comes back
// as the file's (asCorrupt), a full disk as ErrDiskFull (the migration's
// transaction rolled back: the file is as it was).
func openOnce(ctx context.Context, o Options, t target, check bool) (*Index, []string, error) {
	if t.known && len(t.pending()) > 0 { // before the file is created
		if err := updateGate(o.Dirs); err != nil {
			return nil, nil, err
		}
	}
	before, berr := os.Stat(t.path)
	sh, fresh, err := acquire(ctx, t.path, o)
	if err != nil {
		return nil, nil, openError(t.path, err)
	}
	ix := &Index{sh: sh, path: t.path, opts: o, side: t.side}
	if check && fresh {
		if afterConnect != nil {
			afterConnect(t.path)
		}
		if after, aerr := os.Stat(t.path); berr == nil && (aerr != nil || !os.SameFile(before, after)) {
			_ = ix.Close()
			return nil, nil, errReplaced
		}
	}
	applied, err := ix.migrate(ctx, t)
	if err != nil {
		_ = ix.Close()
		return nil, nil, openError(t.path, err)
	}
	return ix, applied, nil
}

// acquire takes a reference on path's shared handles, opening them
// (creating the file) for the first one: fresh. It holds the package
// mutex throughout, openWriter's retry included: other opens in this
// process wait up to busyWait.
func acquire(ctx context.Context, path string, o Options) (sh *shared, fresh bool, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	mu.Lock()
	defer mu.Unlock()
	if sh, ok := files[path]; ok {
		sh.refs++
		return sh, false, nil
	}
	fs := detectFS(dir, o.Home, o.Getenv)
	w, err := openWriter(ctx, path, fs)
	if err != nil {
		return nil, false, err
	}
	r, err := sql.Open(driverName, dsn(path, fs, true))
	if err != nil {
		_ = w.Close()
		return nil, false, err
	}
	sh = &shared{w: w, r: r, fs: fs, refs: 1}
	files[path] = sh
	return sh, true, nil
}

// openWriter opens path's writer and its connection, creating the file.
// Two processes setting up a new file at once both hold a read lock and
// want the write lock to set its journal mode: SQLite answers one of them
// SQLITE_BUSY at once, the busy timeout aside (waiting would deadlock), so
// that one tries again.
func openWriter(ctx context.Context, path string, fs FSInfo) (*sql.DB, error) {
	w, err := sql.Open(driverName, dsn(path, fs, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	deadline := time.Now().Add(busyWait)
	for {
		err := w.PingContext(ctx)
		if err == nil {
			return w, nil
		}
		if !isBusy(err) || time.Now().After(deadline) {
			_ = w.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = w.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// busyWait is the busy timeout of the DSN (a var for the tests).
var busyWait = 5 * time.Second

func isBusy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqlite3.SQLITE_BUSY
}

// lockHeld: err is SQLITE_BUSY or SQLITE_LOCKED (any extended code).
func lockHeld(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	c := e.Code() & 0xff
	return c == sqlite3.SQLITE_BUSY || c == sqlite3.SQLITE_LOCKED
}

// Close drops this handle's reference; the files close with the last one.
func (ix *Index) Close() error {
	mu.Lock()
	defer mu.Unlock()
	if ix.closed {
		return nil
	}
	ix.closed = true
	ix.sh.refs--
	if ix.sh.refs > 0 {
		return nil
	}
	delete(files, ix.path)
	return errors.Join(ix.sh.r.Close(), ix.sh.w.Close())
}

func (ix *Index) Path() string { return ix.path }
func (ix *Index) FS() FSInfo   { return ix.sh.fs }

// Schema is the user_version of the file in use, as Open left it.
func (ix *Index) Schema() int { return ix.schema }

// Side reports that the side file is in use: metrics.db is newer than
// this binary.
func (ix *Index) Side() bool { return ix.side }

// FileVersion is the user_version of the file in use, read now through the
// reader pool: above Schema, another backsight migrated the file since this
// handle opened it (a long-lived process checks it on each request).
func (ix *Index) FileVersion(ctx context.Context) (int, error) {
	var v int
	if err := ix.sh.r.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read the version of the metrics index: %w", err)
	}
	return v, nil
}

// ReadTx runs fn in a read-only transaction on the reader pool.
func (ix *Index) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := ix.sh.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// ErrSchemaChanged: another backsight migrated the file after this handle
// opened it; nothing more is written through the handle, and the process
// must open the index again.
var ErrSchemaChanged = errors.New("the metrics index was changed by another version of backsight")

// onWriteTx runs before each write transaction of a handle begins (tests
// count them).
var onWriteTx = func() {}

// errOtherWriter: a write transaction could not begin within the busy
// timeout, another connection holding the write lock (another backsight
// process catching up, in the middle of a batch). It wraps the SQLite
// error, whose class stays busy.
var errOtherWriter = errors.New("another process holds the write lock of the metrics index")

// beginWrite begins a write transaction (BEGIN IMMEDIATE: the writer
// DSN's _txlock) and reads user_version under its lock: a file another
// binary migrated since this handle opened it (a long-lived process, the
// dashboard) is not written, ErrSchemaChanged. A lock still held by
// another connection after the busy timeout is errOtherWriter.
func (ix *Index) beginWrite(ctx context.Context) (*sql.Tx, error) {
	onWriteTx()
	tx, err := ix.sh.w.BeginTx(ctx, nil)
	if lockHeld(err) {
		return nil, fmt.Errorf("%w: %w", errOtherWriter, err)
	}
	if err != nil {
		return nil, err
	}
	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if v != ix.schema {
		_ = tx.Rollback()
		return nil, ErrSchemaChanged
	}
	return tx, nil
}
