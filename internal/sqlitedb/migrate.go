package sqlitedb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Migration is one forward-only schema step, a NNNN-slug.sql file. A
// registry is append-only: a released migration is never edited,
// renumbered or removed (CheckPins holds the released ones).
type Migration struct {
	ID     string // "NNNN-slug", the file name without .sql
	Num    int
	SQL    string
	SHA256 string // hex, of the file's bytes
}

// ErrNewer: ApplyOne found the file above the head it was given, migrated
// by a newer binary; nothing was written.
var ErrNewer = errors.New("the database was migrated by a newer backsight")

var migrationName = regexp.MustCompile(`^([0-9]{4})-[a-z0-9]+(-[a-z0-9]+)*\.sql$`)

// LoadRegistry reads the migrations at the root of fsys, which holds the
// .sql files themselves (for an embedded directory, pass fs.Sub of it):
// every entry must be a NNNN-slug.sql file, numbered from 1 with no gap,
// that leaves its transaction to the runner (no BEGIN, COMMIT, END,
// ROLLBACK, SAVEPOINT or RELEASE statement). noun names the registry in
// its errors ("index", "ledger").
func LoadRegistry(fsys fs.FS, noun string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("%s migrations: %w", noun, err)
	}
	reg := make([]Migration, 0, len(entries))
	for _, e := range entries { // sorted by name: by number
		name := e.Name()
		g := migrationName.FindStringSubmatch(name)
		if g == nil || !e.Type().IsRegular() {
			return nil, fmt.Errorf("%s migration %s: not a NNNN-slug.sql file", noun, name)
		}
		n, _ := strconv.Atoi(g[1])
		if want := len(reg) + 1; n != want {
			return nil, fmt.Errorf("%s migration %s: number %d expected next (numbered from 1, with no gap)", noun, name, want)
		}
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("%s migration %s: %w", noun, name, err)
		}
		if kw, ok := txControl(string(raw)); ok {
			return nil, fmt.Errorf("%s migration %s: a %s statement: each migration runs in the transaction the runner opens", noun, name, kw)
		}
		sum := sha256.Sum256(raw)
		reg = append(reg, Migration{
			ID: strings.TrimSuffix(name, ".sql"), Num: n, SQL: string(raw), SHA256: hex.EncodeToString(sum[:]),
		})
	}
	return reg, nil
}

// Head is the highest migration number of reg (0: none).
func Head(reg []Migration) int {
	if len(reg) == 0 {
		return 0
	}
	return reg[len(reg)-1].Num
}

// CheckPins holds the released migrations: each one pinned, at its pinned
// sha256, and no pinned one gone. noun names the registry in its errors.
func CheckPins(reg []Migration, pins map[string]string, noun string) error {
	var errs []error
	known := map[string]bool{}
	for _, m := range reg {
		known[m.ID] = true
		pin, ok := pins[m.ID]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s migration %s is not pinned: pin sha256 %s", noun, m.ID, m.SHA256))
		case pin != m.SHA256:
			errs = append(errs, fmt.Errorf("%s migration %s was edited after its release (sha256 %s, pinned %s): add a new migration instead", noun, m.ID, m.SHA256, pin))
		}
	}
	var gone []string
	for id := range pins {
		if !known[id] {
			gone = append(gone, id)
		}
	}
	slices.Sort(gone)
	for _, id := range gone {
		errs = append(errs, fmt.Errorf("pinned %s migration %s is missing: a released migration is never removed or renamed", noun, id))
	}
	return errors.Join(errs...)
}

// ApplyOne applies m in its own transaction through w, a writer whose DSN
// begins with the write lock (BEGIN IMMEDIATE), and reports whether this
// call applied it. Under the lock it reads user_version again: above
// head, ErrNewer (a newer binary migrated the file while this one
// waited); at m.Num or above, nothing to do. It records m in
// schema_migrations (applied at now, by binaryVersion) and sets
// user_version to m.Num. A lock still held past the busy timeout is
// SQLITE_BUSY, unless the holder committed m meanwhile.
func ApplyOne(ctx context.Context, w *sql.DB, m Migration, head int, now time.Time, binaryVersion string) (bool, error) {
	tx, err := w.BeginTx(ctx, nil) // BEGIN IMMEDIATE: the writer DSN's _txlock
	if err != nil {
		if !isBusy(err) {
			return false, err
		}
		// The lock stayed held past the busy timeout (the process that
		// holds it runs batches back to back, and SQLite's busy handler
		// sleeps between its retries, so a waiter can starve). If that
		// process committed this migration meanwhile, there is nothing
		// left to wait for.
		var v int
		verr := w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
		switch {
		case verr != nil:
			return false, err
		case v > head:
			return false, ErrNewer
		case v >= m.Num:
			return false, nil
		}
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return false, err
	}
	if v > head { // a newer binary migrated it while this one waited
		return false, ErrNewer
	}
	if v >= m.Num { // applied while this process waited for the lock
		return false, nil
	}
	steps := []struct {
		q    string
		args []any
	}{
		{`CREATE TABLE IF NOT EXISTS schema_migrations(id TEXT PRIMARY KEY, sha256 TEXT NOT NULL, applied_at INTEGER NOT NULL, binary_version TEXT NOT NULL)`, nil},
		{m.SQL, nil},
		{`INSERT INTO schema_migrations(id, sha256, applied_at, binary_version) VALUES (?, ?, ?, ?)`,
			[]any{m.ID, m.SHA256, now.UTC().UnixMilli(), binaryVersion}},
		{fmt.Sprintf("PRAGMA user_version = %d", m.Num), nil},
	}
	for _, s := range steps {
		if _, err := tx.ExecContext(ctx, s.q, s.args...); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func isBusy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqlite3.SQLITE_BUSY
}
