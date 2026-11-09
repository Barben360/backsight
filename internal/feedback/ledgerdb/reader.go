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
	return openReader(o, false, busy, true)
}

// Read opens o's ledger.db as OpenReader does, runs fn on the reader and
// closes it, without OpenReader's check of its own: each call of fn's
// checks the file in the read transaction it reads in, so a call waits
// for a writer's lock once at most (one busy window, never two). For a
// hook's single read; its errors are OpenReader's and the reader's.
func Read(o Options, busy time.Duration, fn func(*Reader) error) error {
	return openAndRead(o, false, busy, fn)
}

// ReadSide is Read of the side file this binary's writers use beside a
// ledger.db a newer binary migrated (ErrMissing when there is none): for
// the commands that meet ErrSchema there, never the hooks, which read
// ledger.json, exported from that side file.
func ReadSide(o Options, busy time.Duration, fn func(*Reader) error) error {
	return openAndRead(o, true, busy, fn)
}

func openAndRead(o Options, side bool, busy time.Duration, fn func(*Reader) error) error {
	r, err := openReader(o, side, busy, false)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return fn(r)
}

// openReader opens o's ledger.db, or the side file of its head; check:
// the file is checked in a transaction of its own (else at each read).
func openReader(o Options, side bool, busy time.Duration, check bool) (*Reader, error) {
	head, err := Head(o)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	name := FileName
	if side {
		name = SideFileName(head)
	}
	path := filepath.Join(o.Dirs.FeedbackDir(), name)
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
	if !check {
		return r, nil
	}
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
	err := r.read(func(tx *sql.Tx) error {
		tail, err := readTail(tx, r.dir())
		if err != nil {
			return err
		}
		return getEntries(tx, out, ids, tail)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetAny is Get and Any in one read transaction and one read of the
// journal tail: recall's lookup of its hits, with whether the ledger
// holds anything at all.
func (r *Reader) GetAny(ids []string) (map[string]feedback.Entry, bool, error) {
	out := map[string]feedback.Entry{}
	var has bool
	err := r.read(func(tx *sql.Tx) error {
		tail, err := readTail(tx, r.dir())
		if err != nil {
			return err
		}
		if err := getEntries(tx, out, ids, tail); err != nil {
			return err
		}
		if len(out) > 0 {
			has = true
			return nil
		}
		has, err = anyEntry(tx, tail)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return out, has, nil
}

// getEntries adds to out the entries of ids that have one: their rows,
// with tail's lines naming them applied.
func getEntries(tx *sql.Tx, out map[string]feedback.Entry, ids []string, tail []feedback.Event) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	if err := loadEntries(tx, out, ids); err != nil {
		return err
	}
	l := feedback.Ledger{Memories: out}
	applyTail(&l, tail, func(id string) bool { return want[id] })
	for id := range out { // an import of the tail names others
		if !want[id] {
			delete(out, id)
		}
	}
	return nil
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
		has, err = anyEntry(tx, tail)
		return err
	})
	return has, err
}

// anyEntry is Any inside tx, tail being its journal tail.
func anyEntry(tx *sql.Tx, tail []feedback.Event) (bool, error) {
	named := namedIDs(tail)
	// More rows than named ids: one is not named.
	unnamed, err := func() (bool, error) {
		rows, err := tx.Query(`SELECT memory_id FROM memories LIMIT ?`, len(named)+1)
		if err != nil {
			return false, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return false, err
			}
			if _, ok := slices.BinarySearch(named, id); !ok {
				return true, nil
			}
		}
		return false, rows.Err()
	}()
	if err != nil || unnamed {
		return unnamed, err
	}
	l := feedback.Ledger{Memories: map[string]feedback.Entry{}}
	if err := loadEntries(tx, l.Memories, named); err != nil {
		return false, err
	}
	applyTail(&l, tail, nil)
	return len(l.Memories) > 0, nil
}

// Stamp is what the reader reads, cheaply: the database's build and
// generation (StampFile "ledger.db#<build_id>", Size the generation) and
// each month file's name and size, the tail being read from them; no
// modification time, so a zero Newest. Two equal stamps read the same
// ledger: a fold that changes a row bumps the generation, a rebuild
// draws a new build_id, a line appended grows its file.
func (r *Reader) Stamp() (feedback.Stamp, error) {
	var st feedback.Stamp
	err := r.read(func(tx *sql.Tx) error {
		m, err := readMeta(context.Background(), tx)
		if err != nil {
			return err
		}
		st.Files = append(st.Files, feedback.StampFile{Name: FileName + "#" + m.buildID, Size: m.generation})
		files, err := feedback.MonthFiles(r.dir())
		if err != nil {
			return err
		}
		for _, name := range files {
			fi, err := os.Stat(filepath.Join(r.dir(), name))
			if err != nil {
				return err
			}
			st.Files = append(st.Files, feedback.StampFile{Name: name, Size: fi.Size()})
		}
		return nil
	})
	if err != nil {
		return feedback.Stamp{}, err
	}
	return st, nil
}

// dir is the folder of the reader's file, which holds the journal.
func (r *Reader) dir() string { return filepath.Dir(r.path) }

// beforeRead runs as each read transaction starts (a test seam).
var beforeRead = func() {}

// read runs fn in one read transaction, after checking the file is at
// this binary's head and was folded once.
func (r *Reader) read(fn func(tx *sql.Tx) error) error {
	beforeRead()
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
