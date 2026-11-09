package ledgerdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

// maxIDs is the most ids one IN (…) statement binds (SQLite's historic
// limit of host parameters).
const maxIDs = 999

// Reader reads ledger.db through a read-only connection: it never
// creates, migrates or writes the file (SQLite may create the -wal and
// -shm of a WAL file). Each call is one read transaction, and every error
// it returns is ErrMissing, ErrBusy, ErrUnreadable or ErrSchema.
type Reader struct {
	db   *sql.DB
	path string
	head int
}

// OpenReader opens o's ledger.db read-only, waiting at most busy for a
// writer's lock, and checks it: at this binary's head (else ErrSchema)
// and folded once (a generation row, else ErrMissing). There is no
// journal-mode pragma (SQLite reads the mode from the file's header), no
// folder detection and no WAL probe.
func OpenReader(o Options, busy time.Duration) (*Reader, error) {
	head, err := Head(o)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	path := filepath.Join(o.Dirs.FeedbackDir(), FileName)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, ErrMissing
	} else if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	db, err := sql.Open(driverName, sqlitedb.ReaderDSN(path, busy))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	db.SetMaxOpenConns(1)
	r := &Reader{db: db, path: path, head: head}
	if err := r.read(func(*sql.Tx) error { return nil }); err != nil {
		_ = db.Close()
		return nil, err
	}
	return r, nil
}

// Close closes the reader's connection.
func (r *Reader) Close() error { return r.db.Close() }

// Get is the entries of ids that have one: their rows, with the journal
// tail's lines naming them applied.
func (r *Reader) Get(ids []string) (map[string]feedback.Entry, error) {
	out := map[string]feedback.Entry{}
	if len(ids) == 0 {
		return out, nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	err := r.read(func(tx *sql.Tx) error {
		if err := loadEntries(tx, out, ids); err != nil {
			return err
		}
		tail, err := readTail(tx, r.dir())
		if err != nil {
			return err
		}
		l := feedback.Ledger{Memories: out}
		applyTail(&l, tail, func(id string) bool { return want[id] })
		return nil
	})
	if err != nil {
		return nil, err
	}
	for id := range out { // an import of the tail names others
		if !want[id] {
			delete(out, id)
		}
	}
	return out, nil
}

// Hidden is every entry with Hidden set, the journal tail applied: its
// hides and unhides, and the rows of every id it names.
func (r *Reader) Hidden() (map[string]feedback.Entry, error) {
	out := map[string]feedback.Entry{}
	err := r.read(func(tx *sql.Tx) error {
		if err := scanEntries(tx, out, `SELECT `+memoryColumns+` FROM memories WHERE hidden_since IS NOT NULL`); err != nil {
			return err
		}
		tail, err := readTail(tx, r.dir())
		if err != nil {
			return err
		}
		if err := loadEntries(tx, out, namedIDs(tail)); err != nil {
			return err
		}
		l := feedback.Ledger{Memories: out}
		applyTail(&l, tail, nil)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for id, e := range out {
		if e.Hidden == nil {
			delete(out, id)
		}
	}
	return out, nil
}

// All is the whole ledger, the journal tail applied: every entry, the
// latest applied event's time as its watermark, and when earlier history
// was last imported.
func (r *Reader) All() (feedback.Ledger, error) {
	l := feedback.Ledger{Schema: feedback.LedgerSchema, Memories: map[string]feedback.Entry{}}
	err := r.read(func(tx *sql.Tx) error {
		var err error
		if l.Watermark, err = metaTime(tx, "watermark_ns"); err != nil {
			return err
		}
		if l.HistoryImported, err = metaTime(tx, "history_imported_ns"); err != nil {
			return err
		}
		if err := scanEntries(tx, l.Memories, `SELECT `+memoryColumns+` FROM memories`); err != nil {
			return err
		}
		tail, err := readTail(tx, r.dir())
		if err != nil {
			return err
		}
		applyTail(&l, tail, nil)
		return nil
	})
	if err != nil {
		return feedback.Ledger{}, err
	}
	return l, nil
}

// Any reports whether All would hold any entry: a row the journal tail
// does not name, or an entry left once the tail is applied to the rows
// it names.
func (r *Reader) Any() (bool, error) {
	var has bool
	err := r.read(func(tx *sql.Tx) error {
		tail, err := readTail(tx, r.dir())
		if err != nil {
			return err
		}
		named := namedIDs(tail)
		// More rows than named ids: one is not named.
		rows, err := tx.Query(`SELECT memory_id FROM memories LIMIT ?`, len(named)+1)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			if _, ok := slices.BinarySearch(named, id); !ok {
				has = true
				return nil
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		l := feedback.Ledger{Memories: map[string]feedback.Entry{}}
		if err := loadEntries(tx, l.Memories, named); err != nil {
			return err
		}
		applyTail(&l, tail, nil)
		has = len(l.Memories) > 0
		return nil
	})
	return has, err
}

// dir is the folder of the reader's file, which holds the journal.
func (r *Reader) dir() string { return filepath.Dir(r.path) }

// read runs fn in one read transaction, after checking the file is at
// this binary's head and was folded once.
func (r *Reader) read(fn func(tx *sql.Tx) error) error {
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, nil) // a deferred BEGIN: the snapshot starts at the first read
	if err != nil {
		return r.class(err)
	}
	defer func() { _ = tx.Rollback() }()
	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return r.class(err)
	}
	if v != r.head {
		// At user_version 0 without schema_migrations, the file was just
		// created and its first migration has not committed: a first
		// build in progress, not another schema.
		var migrated bool
		if v == 0 {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE name = 'schema_migrations')`).Scan(&migrated); err != nil {
				return r.class(err)
			}
			if !migrated {
				return fmt.Errorf("%w: it is being built", ErrMissing)
			}
		}
		return fmt.Errorf("%w: schema %d, this binary reads %d", ErrSchema, v, r.head)
	}
	var gen string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'generation'`).Scan(&gen); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: it was never folded", ErrMissing)
	case err != nil:
		return r.class(err)
	}
	if err := fn(tx); err != nil {
		return r.class(err)
	}
	return nil
}

// class maps err to the reader's four errors: busy is ErrBusy, a file
// gone since the open ErrMissing, any other ErrUnreadable.
func (r *Reader) class(err error) error {
	for _, known := range []error{ErrMissing, ErrBusy, ErrUnreadable, ErrSchema} {
		if errors.Is(err, known) {
			return err
		}
	}
	switch sqlitedb.Code(err) {
	case "busy":
		return fmt.Errorf("%w: %w", ErrBusy, err)
	case "cantopen":
		if _, serr := os.Stat(r.path); errors.Is(serr, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w", ErrMissing, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrUnreadable, err)
}

// scanEntries adds the entries query q returns (memoryColumns) to out.
func scanEntries(tx *sql.Tx, out map[string]feedback.Entry, q string, args ...any) error {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var rw row
		if err := rows.Scan(rw.dest()...); err != nil {
			return err
		}
		id, e := entryFromRow(rw)
		out[id] = e
	}
	return rows.Err()
}

// metaTime is meta key's unix-nanosecond time (zero: no row, or 0).
func metaTime(tx *sql.Tx, key string) (time.Time, error) {
	var s string
	err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	ns, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("meta %s: %w", key, err)
	}
	return fromNS(ns), nil
}
