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
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

// Migration is one forward-only schema step, a NNNN-slug.sql file
// (sqlitedb.Migration). The registry is append-only: a released migration
// is never edited, renumbered or removed (checkPins holds the released
// ones).
type Migration = sqlitedb.Migration

// ErrUpdateInProgress: the index needs a migration while `update apply`
// has not reached its commit point (a rollback must never meet a migrated
// index). It wraps sqlitedb.ErrUpdateInProgress.
var ErrUpdateInProgress = fmt.Errorf("%w, try again in a minute", sqlitedb.ErrUpdateInProgress)

// LoadRegistry reads the index migrations at the root of fsys
// (sqlitedb.LoadRegistry): every entry a NNNN-slug.sql file, numbered from
// 1 with no gap, that leaves its transaction to the runner.
func LoadRegistry(fsys fs.FS) ([]Migration, error) { return sqlitedb.LoadRegistry(fsys, "index") }

// Head is the highest migration number of reg (0: none).
func Head(reg []Migration) int { return sqlitedb.Head(reg) }

// checkPins holds the released migrations: each one pinned, at its pinned
// sha256, and no pinned one gone.
func checkPins(reg []Migration, pins map[string]string) error {
	return sqlitedb.CheckPins(reg, pins, "index")
}

// SideFileName is the file a binary of schema head uses beside an index
// a newer binary migrated, which it never writes.
func SideFileName(head int) string { return fmt.Sprintf("metrics-v%d.db", head) }

// MigrateResult is what Migrate found and did.
type MigrateResult struct {
	Built   bool     `json:"built"` // an index file existed
	Newer   bool     `json:"newer"` // the index is newer than this binary: its side file is used
	File    string   `json:"file"`
	Pending []string `json:"pending"`
	Applied []string `json:"applied"`
}

// Migrate applies the pending migrations of an existing index and ingests
// nothing: a missing index stays missing (the first open builds it), and
// one newer than this binary is left alone. plan only lists them. A
// failure (the wait for an update aside) is recorded in status.json as a
// time and a class, and cleared by the next run that succeeds; a plan
// records and clears nothing.
func Migrate(ctx context.Context, o Options, plan bool) (MigrateResult, error) {
	o = o.withDefaults()
	res, err := migrateIndex(ctx, o, plan)
	if plan || errors.Is(err, ErrUpdateInProgress) {
		return res, err
	}
	_ = updateStatus(o.Dirs, func(st *recoveryState) bool {
		switch {
		case err != nil:
			st.MigrateFailedAt, st.MigrateReason = o.Now(), classify(err)
		case st.MigrateFailedAt.IsZero() && st.MigrateReason == "":
			return false
		default:
			st.MigrateFailedAt, st.MigrateReason = time.Time{}, ""
		}
		return true
	})
	return res, err
}

func migrateIndex(ctx context.Context, o Options, plan bool) (MigrateResult, error) {
	t, err := resolve(ctx, o)
	if err != nil {
		return MigrateResult{}, err
	}
	res := MigrateResult{Built: t.built, Newer: t.side, File: t.path, Pending: []string{}, Applied: []string{}}
	if !t.built || t.side {
		return res, nil
	}
	if !t.known && plan { // only the writer could read it
		return res, t.probeErr
	}
	if t.known {
		for _, m := range t.pending() {
			res.Pending = append(res.Pending, m.ID)
		}
		if plan || len(res.Pending) == 0 {
			return res, nil
		}
	}
	ix, applied, err := openTarget(ctx, o, t)
	if err != nil {
		return res, err
	}
	countOpen(o)
	res.Applied = append(res.Applied, applied...)
	if !t.known {
		res.Pending = append(res.Pending, applied...)
	}
	res.Newer, res.File = ix.Side(), ix.Path() // a newer binary may have migrated it meanwhile
	return res, ix.Close()
}

// errNewer: the writer found metrics.db above this binary's head, migrated
// by a newer binary after the probe read it.
var errNewer = errors.New("the index was just migrated by a newer backsight: run the command again")

// target is the file an open works on.
type target struct {
	reg      []Migration
	head     int
	built    bool   // metrics.db exists
	path     string // metrics.db, or the side file beside a newer one
	side     bool
	known    bool  // version was read; false: the probe failed, the writer reads it
	version  int   // path's user_version, 0 when absent
	probeErr error // why it is not known
}

func (t target) pending() []Migration {
	var out []Migration
	for _, m := range t.reg {
		if m.Num > t.version {
			out = append(out, m)
		}
	}
	return out
}

// resolve reads the registry and picks the file: metrics.db, or the side
// file of this binary's head when metrics.db is newer. It creates nothing.
func resolve(ctx context.Context, o Options) (target, error) {
	reg, err := registry(o)
	if err != nil {
		return target{}, err
	}
	t := target{reg: reg, head: Head(reg), path: filepath.Join(o.Dirs.IndexDir(), FileName)}
	if err := t.probe(ctx); err != nil {
		return target{}, err
	}
	t.built = t.built || !t.known
	if t.known && t.version > t.head {
		return sideTarget(ctx, t)
	}
	return t, nil
}

// registry is o's migrations (nil: Embedded).
func registry(o Options) ([]Migration, error) {
	fsys := o.Migrations
	if fsys == nil {
		fsys = Embedded
	}
	return LoadRegistry(fsys)
}

// sideTarget is t moved to the side file of its head.
func sideTarget(ctx context.Context, t target) (target, error) {
	t.side = true
	t.path = filepath.Join(filepath.Dir(t.path), SideFileName(t.head))
	built := t.built
	err := t.probe(ctx)
	t.built = built // of metrics.db
	return t, err
}

// probe reads t.path's version. A read that fails while a -wal exists
// (one a killed process left, say) leaves it unknown instead: the writer,
// which recovers the WAL, reads it under its lock.
func (t *target) probe(ctx context.Context) error {
	v, built, err := probeVersion(ctx, t.path)
	t.version, t.built, t.known, t.probeErr = v, built, err == nil, err
	if err != nil {
		if _, serr := os.Stat(t.path + "-wal"); serr != nil {
			return asCorrupt(t.path, err)
		}
		t.version = 0
	}
	return nil
}

// probeVersion is fileVersion; tests make it fail.
var probeVersion = fileVersion

// fileVersion is path's user_version through a read-only connection that
// changes nothing (0, false: no file).
func fileVersion(ctx context.Context, path string) (int, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	db, err := sql.Open(driverName, probeDSN(path, idleWAL(path)))
	if err != nil {
		return 0, true, err
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, true, fmt.Errorf("%s: %w", path, err)
	}
	return v, true, nil
}

// idleWAL: path is a WAL database (header bytes 18 and 19 are 2) with no
// -wal beside it, so its main file holds every commit.
func idleWAL(path string) bool {
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var h [20]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return false
	}
	return h[18] == 2 && h[19] == 2
}

// updateGate refuses a migration while an update is uncommitted, or when
// its marker cannot be read (the safe side).
func updateGate(d paths.Dirs) error {
	if err := sqlitedb.UpdateGate(d); err != nil {
		return ErrUpdateInProgress
	}
	return nil
}

// migrate brings the file ix holds to t's head. The version the writer
// reads is the authority (the probe's may be stale, or missing): above
// the head on metrics.db, nothing is written and errNewer sends the open
// to the side file.
func (ix *Index) migrate(ctx context.Context, t target) ([]string, error) {
	v, err := writerVersion(ctx, ix.sh.w)
	if err != nil {
		return nil, err
	}
	if v > t.head && !t.side {
		return nil, errNewer
	}
	t.version, t.known = v, true
	pending := t.pending()
	if len(pending) > 0 {
		if err := updateGate(ix.opts.Dirs); err != nil {
			return nil, err
		}
	}
	applied, err := applyPending(ctx, ix.sh.w, pending, t.head, ix.opts)
	if err != nil {
		return nil, err
	}
	if beforeFinalRead != nil {
		beforeFinalRead(ix.path)
	}
	if err := ix.sh.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ix.schema); err != nil {
		return applied, err
	}
	// A newer binary may have migrated between the two reads: the handle
	// must not write there (beginWrite compares with ix.schema).
	if ix.schema > t.head && !t.side {
		return nil, errNewer
	}
	return applied, nil
}

// beforeFinalRead, when set, runs between the migrations and migrate's
// last read of user_version (tests migrate the file there).
var beforeFinalRead func(path string)

// writerVersion reads the committed user_version through the writer's
// connection, the -wal included, without the write lock: an open never
// waits for another process's batch (a tick catching up holds the lock
// for a whole batch, past the busy timeout on a slow machine). A
// migration half way in another process is not seen; each migration reads
// the version again under the lock (sqlitedb.ApplyOne, which without the
// lock after a busy timeout reads it once more), and a write transaction
// that finds it changed since the open stops with ErrSchemaChanged
// (beginWrite).
func writerVersion(ctx context.Context, w *sql.DB) (int, error) {
	var v int
	err := w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// applyPending applies each migration in its own transaction and returns
// the ones this call applied. A file found above head is left alone
// (errNewer).
func applyPending(ctx context.Context, w *sql.DB, pending []Migration, head int, o Options) ([]string, error) {
	applied := []string{}
	for _, m := range pending {
		done, err := sqlitedb.ApplyOne(ctx, w, m, head, o.Now(), o.BinaryVersion)
		if errors.Is(err, sqlitedb.ErrNewer) {
			return applied, errNewer
		}
		if err != nil {
			return applied, fmt.Errorf("index migration %s: %w", m.ID, err)
		}
		if done {
			applied = append(applied, m.ID)
		}
	}
	return applied, nil
}
