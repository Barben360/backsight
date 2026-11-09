package ledgerdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

// FileName is the ledger database inside Dirs.FeedbackDir.
const FileName = "ledger.db"

// driverName is the database/sql driver sqlitedb registers.
const driverName = "sqlite"

// writerBusy is the writer's busy timeout: how long it waits for another
// process's transaction.
const writerBusy = 5 * time.Second

// Options says where the ledger database lives and how it is opened.
type Options struct {
	Dirs          paths.Dirs
	Home          string              // sync-folder prefixes
	Getenv        func(string) string // nil: os.Getenv
	BinaryVersion string              // "": buildinfo.Version
	Migrations    fs.FS               // nil: Embedded
	Now           func() time.Time    // nil: time.Now
}

func (o Options) withDefaults() Options {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.BinaryVersion == "" {
		o.BinaryVersion = buildinfo.Version
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

var (
	// ErrMissing: ledger.db does not exist, or was migrated and never
	// folded (no generation row).
	ErrMissing = errors.New("ledger.db does not exist")
	// ErrBusy: another process held ledger.db past the busy timeout.
	ErrBusy = errors.New("ledger.db is locked")
	// ErrUnreadable: ledger.db, or its folder, cannot be read: corrupt,
	// not a database, no permission, an I/O error.
	ErrUnreadable = errors.New("ledger.db cannot be read")
	// ErrSchema: ledger.db is at another schema version than this
	// binary's head (older, waiting for its migration, or newer).
	ErrSchema = errors.New("ledger.db has another schema")
	// ErrUpdateInProgress: ledger.db needs building or a migration while
	// `update apply` has not reached its commit point.
	ErrUpdateInProgress = sqlitedb.ErrUpdateInProgress
)

// Writer is a read-write connection to the ledger database: ledger.db,
// or the side file of this binary's head beside a ledger.db a newer
// binary migrated (which it never writes).
type Writer struct {
	db   *sql.DB
	path string
	side bool
	opts Options
}

// OpenWriter opens the ledger database under o.Dirs, creating and
// migrating it first: (1) ledger.db's user_version, read only; (2) above
// this binary's head, the side file of that head is used instead, built
// if missing; (3) creating a missing file, or a pending migration, waits
// for an uncommitted update (ErrUpdateInProgress, nothing created or
// written); (4) the file is opened, its version read again (above the
// head: back to 2, once) and each pending migration applied in its own
// transaction; (5) a file this open created is built: the whole journal
// folded in, imports kept, in one transaction after the migrations'
// (until it commits, readers find no generation row: ErrMissing). A
// corrupt file is set aside and a fresh one built, once a day at most
// (recover.go).
func OpenWriter(ctx context.Context, o Options) (*Writer, error) {
	o = o.withDefaults()
	reg, err := registry(o)
	if err != nil {
		return nil, err
	}
	for again := false; ; again = true {
		w, err := openWriter(ctx, o, reg)
		var ce *corruptErr
		if !errors.As(err, &ce) {
			return w, err
		}
		if again { // the fresh file is corrupt too
			return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
		}
		if err := setAside(ctx, o, ce); err != nil {
			return nil, err
		}
	}
}

// openWriter opens the target: ledger.db, or its side file. A newer
// binary may migrate ledger.db after the probe: the writer then finds it
// above the head and lets it go, and the second and last pass opens the
// side file.
func openWriter(ctx context.Context, o Options, reg []sqlitedb.Migration) (*Writer, error) {
	head := sqlitedb.Head(reg)
	t, err := resolve(ctx, o, head)
	if err != nil {
		return nil, err
	}
	w, err := openTarget(ctx, o, reg, t)
	if !errors.Is(err, errNewer) || t.side {
		return w, err
	}
	if t, err = sideTarget(ctx, o, head); err != nil {
		return nil, err
	}
	return openTarget(ctx, o, reg, t)
}

// openTarget opens t.path, creating it, and migrates it to the head.
func openTarget(ctx context.Context, o Options, reg []sqlitedb.Migration, t target) (*Writer, error) {
	if t.known && t.version < sqlitedb.Head(reg) { // before the file is created
		if err := sqlitedb.UpdateGate(o.Dirs); err != nil {
			return nil, err
		}
	}
	dir := filepath.Dir(t.path)
	if err := fsutil.MkdirPrivate(dir); err != nil {
		return nil, err
	}
	_, serr := os.Lstat(t.path)
	created := errors.Is(serr, fs.ErrNotExist)
	db, err := openDB(ctx, t.path, useWAL(t.path, detectFS(dir, o.Home, o.Getenv)))
	if err != nil {
		return nil, asCorrupt(t.path, err)
	}
	w := &Writer{db: db, path: t.path, side: t.side, opts: o}
	if err := w.migrate(ctx, reg); err != nil {
		_ = db.Close()
		return nil, asCorrupt(t.path, err)
	}
	if created {
		if err := w.firstBuild(ctx); err != nil {
			_ = db.Close()
			return nil, asCorrupt(t.path, err)
		}
	}
	return w, nil
}

// errNewer: the writer found ledger.db above this binary's head, migrated
// by a newer binary after the probe read it.
var errNewer = errors.New("ledger.db was just migrated by a newer backsight")

// migrate brings w's file to the head of reg. The version the writer
// reads is the authority (the probe's may be stale, or unknown); each
// migration reads it again under the write lock (sqlitedb.ApplyOne).
func (w *Writer) migrate(ctx context.Context, reg []sqlitedb.Migration) error {
	head := sqlitedb.Head(reg)
	newer := func(v int) error {
		if !w.side {
			return errNewer
		}
		return fmt.Errorf("%w: %s is at schema %d, above %d", ErrSchema, filepath.Base(w.path), v, head)
	}
	var v int
	if err := w.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > head {
		return newer(v)
	}
	var pending []sqlitedb.Migration
	for _, m := range reg {
		if m.Num > v {
			pending = append(pending, m)
		}
	}
	if len(pending) > 0 {
		if err := sqlitedb.UpdateGate(w.opts.Dirs); err != nil {
			return err
		}
	}
	for _, m := range pending {
		_, err := sqlitedb.ApplyOne(ctx, w.db, m, head, w.opts.Now(), w.opts.BinaryVersion)
		if errors.Is(err, sqlitedb.ErrNewer) {
			return newer(head + 1)
		}
		if err != nil {
			return fmt.Errorf("ledger migration %s: %w", m.ID, err)
		}
	}
	return nil
}

// Close closes the writer's connection.
func (w *Writer) Close() error { return w.db.Close() }

// openDB opens path's writer and its connection, creating the file: WAL
// or the rollback journal, and synchronous to match (sqlitedb.DSN). Two
// processes setting up a new file at once both hold a read lock and want
// the write lock to set its journal mode: SQLite answers one of them
// SQLITE_BUSY at once, the busy timeout aside (waiting would deadlock),
// so that one tries again.
func openDB(ctx context.Context, path string, wal bool) (*sql.DB, error) {
	db, err := sql.Open(driverName, sqlitedb.DSN(path, sqlitedb.Mode{WAL: wal}, writerBusy, ""))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	deadline := time.Now().Add(writerBusy)
	for {
		err := db.PingContext(ctx)
		if err == nil {
			return db, nil
		}
		if sqlitedb.Code(err) != "busy" || time.Now().After(deadline) {
			_ = db.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = db.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// detectFS says where a folder lives (the seam of the tests).
var detectFS = sqlitedb.DetectFS

// walProbe tries the write-ahead log in a folder (the seam of the tests).
var walProbe = sqlitedb.ProbeWAL

// walVerdict is the WAL probe's answer per folder, for the life of the
// process (guarded by walMu).
var (
	walMu      sync.Mutex
	walVerdict = map[string]bool{}
)

// useWAL decides the journal mode of the writer of path. Only a local
// folder can take WAL, and some still refuse it (sqlitedb.ProbeWAL): its
// folder is probed once per process. A file already in WAL mode skips
// the probe: its folder took WAL once, and the rollback journal could
// not open it now anyway.
func useWAL(path string, fsi sqlitedb.FSInfo) bool {
	if fsi.Kind != sqlitedb.FSLocal {
		return false
	}
	if sqlitedb.HeaderIsWAL(path) {
		return true
	}
	dir := filepath.Dir(path)
	walMu.Lock()
	defer walMu.Unlock()
	if v, ok := walVerdict[dir]; ok {
		return v
	}
	v := walProbe(dir)
	walVerdict[dir] = v
	return v
}
