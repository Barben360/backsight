package index

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	closed bool
}

// OpensOf is how many times this process opened an index under stateDir.
func OpensOf(stateDir string) int64 {
	mu.Lock()
	defer mu.Unlock()
	return opens[stateDir]
}

// Open opens (creating it if needed) the index file under o.Dirs.
func Open(ctx context.Context, o Options) (*Index, error) {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.BinaryVersion == "" {
		o.BinaryVersion = buildinfo.Version
	}
	dir := o.Dirs.IndexDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, FileName)

	mu.Lock()
	defer mu.Unlock()
	if sh, ok := files[path]; ok {
		sh.refs++
		opens[o.Dirs.State]++
		return &Index{sh: sh, path: path, opts: o}, nil
	}
	fs := detectFS(dir, o.Home, o.Getenv)
	w, err := sql.Open(driverName, dsn(path, fs, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := w.PingContext(ctx); err != nil { // creates the file
		_ = w.Close()
		return nil, err
	}
	r, err := sql.Open(driverName, dsn(path, fs, true))
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	sh := &shared{w: w, r: r, fs: fs, refs: 1}
	files[path] = sh
	opens[o.Dirs.State]++
	return &Index{sh: sh, path: path, opts: o}, nil
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

// ReadTx runs fn in a read-only transaction on the reader pool.
func (ix *Index) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := ix.sh.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}
