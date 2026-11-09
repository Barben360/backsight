package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"sync"
)

// ErrInUse: another connection holds the file Seize wanted whole.
var ErrInUse = errors.New("the database is in use")

// Seize takes path's exclusive lock for a replacement and returns its
// release (idempotent). Any other connection: ErrInUse, nothing written
// (the lock is taken with a read only). Then the file leaves WAL mode,
// which folds and removes its -wal: no -wal of the old file is left for
// the new one, and closing this connection after the rename removes no
// file by name. A file SQLite cannot read (corrupt, not a database)
// cannot be locked through it either, and is of no use to whoever holds
// it: it is replaced as it is, with a no-op release. A missing file needs
// no lock.
func Seize(ctx context.Context, path string) (release func(), err error) {
	noop := func() {}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return noop, nil
	}
	db, err := sql.Open(driverName, LockDSN(path))
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
	release = func() { // idempotent
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
	switch Code(err) {
	case "busy":
		return nil, ErrInUse
	case "corrupt", "notadb":
		return func() {}, nil
	}
	return nil, err
}
