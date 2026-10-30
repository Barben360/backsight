package index

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/Barben360/backsight/internal/buildinfo"
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
	return o
}

// shared is the one pair of handles per file per process.
type shared struct {
	w, r *sql.DB
	fs   FSInfo
	refs int
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
// included) waits for an uncommitted update; (5) the file is opened and
// each pending migration applied in its own transaction.
func Open(ctx context.Context, o Options) (*Index, error) {
	ix, _, err := open(ctx, o.withDefaults())
	return ix, err
}

// open is Open, with the migrations this call applied.
func open(ctx context.Context, o Options) (*Index, []string, error) {
	t, err := resolve(ctx, o)
	if err != nil {
		return nil, nil, err
	}
	pending := t.pending()
	if len(pending) > 0 {
		if err := updateGate(o.Dirs); err != nil {
			return nil, nil, err
		}
	}
	sh, err := acquire(ctx, t.path, o)
	if err != nil {
		return nil, nil, err
	}
	ix := &Index{sh: sh, path: t.path, opts: o, side: t.side}
	applied, err := applyPending(ctx, sh.w, pending, o)
	if err == nil {
		err = sh.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ix.schema)
	}
	if err != nil {
		_ = ix.Close()
		return nil, nil, err
	}
	mu.Lock()
	opens[o.Dirs.State]++
	mu.Unlock()
	return ix, applied, nil
}

// acquire takes a reference on path's shared handles, opening them
// (creating the file) for the first one.
func acquire(ctx context.Context, path string, o Options) (*shared, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	if sh, ok := files[path]; ok {
		sh.refs++
		return sh, nil
	}
	fs := detectFS(dir, o.Home, o.Getenv)
	w, err := openWriter(ctx, path, fs)
	if err != nil {
		return nil, err
	}
	r, err := sql.Open(driverName, dsn(path, fs, true))
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	sh := &shared{w: w, r: r, fs: fs, refs: 1}
	files[path] = sh
	return sh, nil
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

// busyWait is the busy timeout of the DSN.
const busyWait = 5 * time.Second

func isBusy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqlite3.SQLITE_BUSY
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

// ReadTx runs fn in a read-only transaction on the reader pool.
func (ix *Index) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := ix.sh.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}
