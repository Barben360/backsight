package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// Migration is one forward-only schema step, a NNNN-slug.sql file. The
// registry is append-only: a released migration is never edited,
// renumbered or removed (checkPins holds the released ones).
type Migration struct {
	ID     string // "NNNN-slug", the file name without .sql
	Num    int
	SQL    string
	SHA256 string // hex, of the file's bytes
}

// Embedded is the registry built into the binary (Options.Migrations nil).
// Empty until the first schema ships.
var Embedded fs.FS = embed.FS{}

// ErrUpdateInProgress: the index needs a migration while `update apply`
// has not reached its commit point (a rollback must never meet a migrated
// index).
var ErrUpdateInProgress = errors.New("an update is being applied, try again in a minute")

var migrationName = regexp.MustCompile(`^([0-9]{4})-[a-z0-9]+(-[a-z0-9]+)*\.sql$`)

// LoadRegistry reads the migrations at the root of fsys: every entry must
// be a NNNN-slug.sql file, numbered from 0001 with no gap.
func LoadRegistry(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("index migrations: %w", err)
	}
	reg := make([]Migration, 0, len(entries))
	for _, e := range entries { // sorted by name: by number
		name := e.Name()
		g := migrationName.FindStringSubmatch(name)
		if g == nil || !e.Type().IsRegular() {
			return nil, fmt.Errorf("index migration %s: not a NNNN-slug.sql file", name)
		}
		n, _ := strconv.Atoi(g[1])
		if want := len(reg) + 1; n != want {
			return nil, fmt.Errorf("index migration %s: number %d expected next (numbered from 1, with no gap)", name, want)
		}
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("index migration %s: %w", name, err)
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

// checkPins holds the released migrations: each one pinned, at its pinned
// sha256, and no pinned one gone.
func checkPins(reg []Migration, pins map[string]string) error {
	var errs []error
	known := map[string]bool{}
	for _, m := range reg {
		known[m.ID] = true
		pin, ok := pins[m.ID]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("index migration %s is not pinned: pin sha256 %s", m.ID, m.SHA256))
		case pin != m.SHA256:
			errs = append(errs, fmt.Errorf("index migration %s was edited after its release (sha256 %s, pinned %s): add a new migration instead", m.ID, m.SHA256, pin))
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
		errs = append(errs, fmt.Errorf("pinned index migration %s is missing: a released migration is never removed or renamed", id))
	}
	return errors.Join(errs...)
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
// one newer than this binary is left alone. plan only lists them.
func Migrate(ctx context.Context, o Options, plan bool) (MigrateResult, error) {
	o = o.withDefaults()
	t, err := resolve(ctx, o)
	if err != nil {
		return MigrateResult{}, err
	}
	res := MigrateResult{Built: t.built, Newer: t.side, File: t.path, Pending: []string{}, Applied: []string{}}
	if !t.built || t.side {
		return res, nil
	}
	for _, m := range t.pending() {
		res.Pending = append(res.Pending, m.ID)
	}
	if plan || len(res.Pending) == 0 {
		return res, nil
	}
	ix, applied, err := open(ctx, o)
	if err != nil {
		return res, err
	}
	res.Applied = append(res.Applied, applied...)
	return res, ix.Close()
}

// target is the file an open works on.
type target struct {
	reg     []Migration
	head    int
	built   bool   // metrics.db exists
	path    string // metrics.db, or the side file beside a newer one
	side    bool
	version int // path's user_version, 0 when absent
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
	fsys := o.Migrations
	if fsys == nil {
		fsys = Embedded
	}
	reg, err := LoadRegistry(fsys)
	if err != nil {
		return target{}, err
	}
	t := target{reg: reg, head: Head(reg), path: filepath.Join(o.Dirs.IndexDir(), FileName)}
	if t.version, t.built, err = fileVersion(ctx, t.path); err != nil {
		return target{}, err
	}
	if t.version > t.head {
		t.side = true
		t.path = filepath.Join(o.Dirs.IndexDir(), SideFileName(t.head))
		if t.version, _, err = fileVersion(ctx, t.path); err != nil {
			return target{}, err
		}
	}
	return t, nil
}

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
	m, err := state.ReadUpdateMarker(d)
	if err != nil || (m != nil && !m.Committed) {
		return ErrUpdateInProgress
	}
	return nil
}

// applyPending applies each migration in its own transaction and returns
// the ones this call applied.
func applyPending(ctx context.Context, w *sql.DB, pending []Migration, o Options) ([]string, error) {
	applied := []string{}
	for _, m := range pending {
		done, err := applyOne(ctx, w, m, o)
		if err != nil {
			return applied, fmt.Errorf("index migration %s: %w", m.ID, err)
		}
		if done {
			applied = append(applied, m.ID)
		}
	}
	return applied, nil
}

func applyOne(ctx context.Context, w *sql.DB, m Migration, o Options) (bool, error) {
	tx, err := w.BeginTx(ctx, nil) // BEGIN IMMEDIATE: the writer DSN's _txlock
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return false, err
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
			[]any{m.ID, m.SHA256, o.Now().UTC().UnixMilli(), o.BinaryVersion}},
		{fmt.Sprintf("PRAGMA user_version = %d", m.Num), nil},
	}
	for _, s := range steps {
		if _, err := tx.ExecContext(ctx, s.q, s.args...); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}
