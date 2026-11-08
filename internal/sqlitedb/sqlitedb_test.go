package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// TestDSNModes: the writer takes WAL with synchronous NORMAL, or the
// rollback journal with FULL, the write lock at BEGIN and the extra
// pragmas; the index's reader takes mode=ro with a deferred lock; the
// plain reader is mode=ro and the busy timeout only (SQLite reads the
// journal mode from the file's header: a journal_mode pragma would be a
// write a read-only connection is refused).
func TestDSNModes(t *testing.T) {
	const path = "/p/x.db"
	busy := 5 * time.Second
	for _, tc := range []struct {
		name  string
		got   string
		has   []string
		lacks []string
	}{
		{"writer WAL", DSN(path, Mode{WAL: true}, busy, "&_pragma=cache_size(-1)"),
			[]string{"_pragma=journal_mode(WAL)", "_pragma=busy_timeout(5000)", "_pragma=synchronous(NORMAL)", "_txlock=immediate", "&_pragma=cache_size(-1)"},
			[]string{"mode=ro", "synchronous(FULL)"}},
		{"writer DELETE", DSN(path, Mode{}, busy, ""),
			[]string{"_pragma=journal_mode(DELETE)", "_pragma=synchronous(FULL)", "_txlock=immediate"},
			[]string{"mode=ro", "synchronous(NORMAL)", "WAL"}},
		{"index reader", DSN(path, Mode{WAL: true, ReadOnly: true}, busy, ""),
			[]string{"_pragma=journal_mode(WAL)", "mode=ro", "_txlock=deferred"},
			[]string{"_txlock=immediate"}},
	} {
		if !strings.HasPrefix(tc.got, "file:/p/x.db?") {
			t.Errorf("%s: %s does not start with the file URI", tc.name, tc.got)
		}
		for _, h := range tc.has {
			if !strings.Contains(tc.got, h) {
				t.Errorf("%s: %s lacks %s", tc.name, tc.got, h)
			}
		}
		for _, l := range tc.lacks {
			if strings.Contains(tc.got, l) {
				t.Errorf("%s: %s holds %s", tc.name, tc.got, l)
			}
		}
	}
	if got, want := ReaderDSN(path, 20*time.Millisecond), "file:/p/x.db?mode=ro&_pragma=busy_timeout(20)"; got != want {
		t.Errorf("ReaderDSN = %s, want %s", got, want)
	}

	// The plain reader reads a WAL file a writer made, and cannot write it.
	db := filepath.Join(t.TempDir(), "r.db")
	w := openDB(t, DSN(db, Mode{WAL: true}, busy, ""))
	if _, err := w.Exec("CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	r := openDB(t, ReaderDSN(db, 20*time.Millisecond))
	var x int
	if err := r.QueryRow("SELECT x FROM t").Scan(&x); err != nil || x != 7 {
		t.Fatalf("reader: x=%d err=%v", x, err)
	}
	if _, err := r.Exec("INSERT INTO t VALUES (8)"); Code(err) != "readonly" {
		t.Errorf("a write through the reader: %v (%s), want readonly", err, Code(err))
	}
}

// TestLoadRegistryNamesTheNoun: the registry's errors name whose
// migrations they are.
func TestLoadRegistryNamesTheNoun(t *testing.T) {
	gap := fstest.MapFS{"0001-a.sql": {Data: []byte("CREATE TABLE a(x)")}, "0003-c.sql": {}}
	for _, noun := range []string{"ledger", "index"} {
		_, err := LoadRegistry(gap, noun)
		if err == nil || !strings.Contains(err.Error(), noun+" migration 0003-c.sql") || !strings.Contains(err.Error(), "number 2 expected") {
			t.Errorf("%s: a gap gives %v", noun, err)
		}
	}
	reg, err := LoadRegistry(fstest.MapFS{"0001-a.sql": {Data: []byte("CREATE TABLE a(x)")}}, "ledger")
	if err != nil || Head(reg) != 1 {
		t.Fatalf("registry %+v, %v", reg, err)
	}
	if err := CheckPins(reg, map[string]string{}, "ledger"); err == nil || !strings.Contains(err.Error(), "ledger migration 0001-a is not pinned") {
		t.Errorf("an unpinned migration: %v", err)
	}
	if err := CheckPins(reg, map[string]string{"0001-a": reg[0].SHA256}, "ledger"); err != nil {
		t.Errorf("pinned: %v", err)
	}
}

// TestApplyOneRecordsTheMigration: a migration runs in its own
// transaction, recorded in schema_migrations with the time and binary
// given, and user_version becomes its number; applied already, it is
// skipped; a file above head is ErrNewer, nothing written.
func TestApplyOneRecordsTheMigration(t *testing.T) {
	ctx := context.Background()
	w := openDB(t, DSN(filepath.Join(t.TempDir(), "m.db"), Mode{WAL: true}, 5*time.Second, ""))
	w.SetMaxOpenConns(1)
	m := Migration{ID: "0001-a", Num: 1, SQL: "CREATE TABLE a(x INTEGER)", SHA256: "abc"}
	at := time.Date(2026, 11, 8, 12, 30, 0, 0, time.FixedZone("x", 3600))
	done, err := ApplyOne(ctx, w, m, 1, at, "v1.2.3")
	if err != nil || !done {
		t.Fatalf("ApplyOne = %v, %v", done, err)
	}
	var id, sum, version string
	var appliedAt int64
	if err := w.QueryRow("SELECT id, sha256, applied_at, binary_version FROM schema_migrations").Scan(&id, &sum, &appliedAt, &version); err != nil {
		t.Fatal(err)
	}
	if id != "0001-a" || sum != "abc" || !time.UnixMilli(appliedAt).Equal(at) || version != "v1.2.3" {
		t.Errorf("row %s %s %d %s", id, sum, appliedAt, version)
	}
	if v := userVersion(t, w); v != 1 {
		t.Errorf("user_version %d, want 1", v)
	}
	if _, err := w.Exec("INSERT INTO a VALUES (1)"); err != nil {
		t.Errorf("the migration's table: %v", err)
	}
	if done, err := ApplyOne(ctx, w, m, 1, at, "v1.2.3"); done || err != nil {
		t.Errorf("again: %v, %v; want skipped", done, err)
	}
	next := Migration{ID: "0002-b", Num: 2, SQL: "CREATE TABLE b(x INTEGER)"}
	if done, err := ApplyOne(ctx, w, next, 0, at, "v1.2.3"); done || !errors.Is(err, ErrNewer) {
		t.Errorf("above head: %v, %v; want ErrNewer", done, err)
	}
	if v := userVersion(t, w); v != 1 {
		t.Errorf("user_version %d after ErrNewer, want 1", v)
	}
}

// TestUpdateGateRefusesAnUncommittedMarker: no marker or a committed one
// lets a migration through; an uncommitted one, or one that cannot be
// read, refuses it.
func TestUpdateGateRefusesAnUncommittedMarker(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	if err := UpdateGate(d); err != nil {
		t.Errorf("no marker: %v", err)
	}
	if err := state.WriteUpdateMarker(d, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateGate(d); !errors.Is(err, ErrUpdateInProgress) {
		t.Errorf("uncommitted: %v, want ErrUpdateInProgress", err)
	}
	if err := state.WriteUpdateMarker(d, state.UpdateMarker{From: "v1", To: "v2", Committed: true}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateGate(d); err != nil {
		t.Errorf("committed: %v", err)
	}
	if err := os.WriteFile(d.UpdateMarker(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateGate(d); !errors.Is(err, ErrUpdateInProgress) {
		t.Errorf("unreadable: %v, want ErrUpdateInProgress", err)
	}
}

// TestSeizeIsErrInUseWhileOpen: a WAL file another connection has open,
// even idle, cannot be seized; once it is closed, Seize takes it and
// leaves it in rollback-journal mode, its -wal folded; a missing file
// needs no lock.
func TestSeizeIsErrInUseWhileOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	other := openDB(t, DSN(path, Mode{WAL: true}, 5*time.Second, ""))
	if _, err := other.Exec("CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	release, err := Seize(ctx, path)
	if !errors.Is(err, ErrInUse) || release != nil {
		t.Fatalf("while open: %v (release %v), want ErrInUse", err, release != nil)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	release, err = Seize(ctx, path)
	if err != nil {
		t.Fatalf("closed: %v", err)
	}
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the -wal is left: %v", err)
	}
	probe := openDB(t, URI(path)+"?_pragma=busy_timeout(0)")
	if _, err := probe.Exec("SELECT count(*) FROM t"); Code(err) != "busy" {
		t.Errorf("a read while seized: %v, want busy", err)
	}
	release()
	release() // idempotent
	var mode string
	if err := probe.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Errorf("after release: journal_mode %q, %v; want delete", mode, err)
	}
	release, err = Seize(ctx, filepath.Join(t.TempDir(), "none.db"))
	if err != nil || release == nil {
		t.Fatalf("missing file: %v", err)
	}
	release()
}

// TestCodeClasses: SQLite's errors come back as the class callers store
// and show.
func TestCodeClasses(t *testing.T) {
	dir := t.TempDir()
	// busy: a rollback-journal file another connection holds exclusive.
	busyPath := filepath.Join(dir, "busy.db")
	holder := openDB(t, DSN(busyPath, Mode{}, 0, ""))
	holder.SetMaxOpenConns(1)
	if _, err := holder.Exec("CREATE TABLE t(x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	waiter := openDB(t, DSN(busyPath, Mode{}, 0, ""))
	_, busyErr := waiter.Exec("INSERT INTO t VALUES (2)")

	// notadb: a file that is no database.
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, notadbErr := openDB(t, ReaderDSN(garbage, 0)).Exec("SELECT count(*) FROM sqlite_schema")

	// readonly: a write through a read-only connection.
	_, readonlyErr := openDB(t, ReaderDSN(busyPath, time.Second)).Exec("CREATE TABLE u(x INTEGER)")

	// corrupt: a table page overwritten.
	corruptPath := filepath.Join(dir, "corrupt.db")
	c := openDB(t, DSN(corruptPath, Mode{}, time.Second, ""))
	if _, err := c.Exec("PRAGMA page_size=4096; CREATE TABLE t(x BLOB)"); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if _, err := c.Exec("INSERT INTO t VALUES (randomblob(1000))"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(corruptPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(strings.Repeat("\xff", 4096)), 4096); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	var n int
	corruptErr := openDB(t, ReaderDSN(corruptPath, time.Second)).QueryRow("SELECT count(*) FROM t").Scan(&n)

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"busy", busyErr, "busy"},
		{"notadb", notadbErr, "notadb"},
		{"readonly", readonlyErr, "readonly"},
		{"corrupt", corruptErr, "corrupt"},
		{"in use", ErrInUse, "busy"},
		{"something else", errors.New("something"), "other"},
	} {
		if got := Code(tc.err); got != tc.want {
			t.Errorf("%s: Code(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
