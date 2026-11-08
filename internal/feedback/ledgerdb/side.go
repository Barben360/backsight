package ledgerdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Barben360/backsight/internal/sqlitedb"
)

// SideFileName is the file a binary of schema head uses beside a
// ledger.db a newer binary migrated, which it never writes.
func SideFileName(head int) string { return fmt.Sprintf("ledger-v%d.db", head) }

// target is the file a writer opens.
type target struct {
	path    string // ledger.db, or the side file beside a newer one
	side    bool
	known   bool // version was read; false: the probe failed, the writer reads it
	version int  // path's user_version, 0 when absent
}

// resolve picks the writer's file: ledger.db, or the side file of head
// when ledger.db is newer. It creates nothing.
func resolve(ctx context.Context, o Options, head int) (target, error) {
	t := target{path: filepath.Join(o.Dirs.FeedbackDir(), FileName)}
	if err := t.probe(ctx); err != nil {
		return target{}, err
	}
	if t.known && t.version > head {
		return sideTarget(ctx, o, head)
	}
	return t, nil
}

// sideTarget is the side file of head.
func sideTarget(ctx context.Context, o Options, head int) (target, error) {
	t := target{path: filepath.Join(o.Dirs.FeedbackDir(), SideFileName(head)), side: true}
	if err := t.probe(ctx); err != nil {
		return target{}, err
	}
	return t, nil
}

// probe reads t.path's version. A read that fails while a -wal exists
// (one a killed process left, say) leaves it unknown instead: the writer,
// which recovers the WAL, reads it. Otherwise a corrupt file is the
// file's corruption (set aside by OpenWriter).
func (t *target) probe(ctx context.Context) error {
	v, err := fileVersion(ctx, t.path)
	if err == nil {
		t.version, t.known = v, true
		return nil
	}
	if _, serr := os.Stat(t.path + "-wal"); serr == nil {
		t.version, t.known = 0, false
		return nil
	}
	return asCorrupt(t.path, err)
}

// fileVersion is path's user_version through a read-only connection that
// changes nothing (0: no file). An idle WAL file opens immutable, so that
// no -wal or -shm is created beside it.
func fileVersion(ctx context.Context, path string) (int, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	db, err := sql.Open(driverName, sqlitedb.ProbeDSN(path, idleWAL(path), writerBusy))
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// idleWAL: path is a WAL database with no -wal beside it, so its main
// file holds every commit.
func idleWAL(path string) bool {
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return sqlitedb.HeaderIsWAL(path)
}
