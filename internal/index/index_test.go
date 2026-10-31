package index

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/paths"
)

func openT(t *testing.T) (*Index, paths.Dirs) {
	t.Helper()
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	ix, err := Open(context.Background(), Options{Dirs: d, Home: t.TempDir(), Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix, d
}

func TestOpenMakesAPrivateDirectory(t *testing.T) {
	ix, d := openT(t)
	st, err := os.Stat(d.IndexDir())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(d.IndexDir(), FileName)); err != nil {
		t.Errorf("no file: %v", err)
	}
	if ix.Path() != filepath.Join(d.IndexDir(), FileName) {
		t.Errorf("path %s", ix.Path())
	}
}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestConnectionPragmas(t *testing.T) {
	ix, _ := openT(t)
	for name, want := range map[string]string{
		"journal_mode": "wal", "busy_timeout": "5000", "foreign_keys": "1",
		"synchronous": "1", "journal_size_limit": "67108864",
	} {
		if got := pragma(t, ix.sh.w, name); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	// _txlock=immediate: BeginTx alone takes the write lock.
	tx, err := ix.sh.w.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	raw, err := sql.Open(driverName, "file:"+ix.Path()+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_, err = raw.Exec("BEGIN IMMEDIATE")
	if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") {
		t.Errorf("second writer should be busy, got %v", err)
	}
}

func TestReaderIsReadOnly(t *testing.T) {
	ix, _ := openT(t)
	err := ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE t(x)")
		return err
	})
	if err == nil {
		t.Error("a read tx must refuse writes")
	}
}

func TestOneWriterPerProcess(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	o := Options{Dirs: d, Home: t.TempDir(), Getenv: func(string) string { return "" }}
	a, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if a.sh.w != b.sh.w {
		t.Error("two opens must share one writer")
	}
	if n := a.sh.w.Stats().MaxOpenConnections; n != 1 {
		t.Errorf("max open %d", n)
	}
	if OpensOf(d.State) != 2 {
		t.Errorf("opens %d", OpensOf(d.State))
	}
	_ = a.Close()
	if _, err := b.sh.w.Exec("CREATE TABLE t(x)"); err != nil {
		t.Errorf("b unusable after a.Close: %v", err)
	}
	if err := b.ReadTx(context.Background(), func(tx *sql.Tx) error { return nil }); err != nil {
		t.Error(err)
	}
	_ = b.Close()
	if err := b.sh.w.Ping(); err == nil {
		t.Error("last Close must close the handle")
	}
	// Reopen works once everything is closed.
	c, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

func TestNetworkFolderOpensWithoutWAL(t *testing.T) {
	old := detectFS
	detectFS = func(string, string, func(string) string) FSInfo { return FSInfo{FSNetwork, "nfs"} }
	t.Cleanup(func() { detectFS = old })
	ix, d := openT(t)
	if got := pragma(t, ix.sh.w, "journal_mode"); got != "delete" {
		t.Errorf("journal_mode %s", got)
	}
	if _, err := ix.sh.w.Exec("CREATE TABLE t(x); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.IndexDir(), FileName+"-wal")); err == nil {
		t.Error("a wal file exists on a network folder")
	}
	if got := pragma(t, ix.sh.r, "journal_mode"); got != "delete" {
		t.Errorf("reader journal_mode %s", got)
	}
	if fs := ix.FS(); fs.Kind != FSNetwork || fs.Name != "nfs" {
		t.Errorf("fs %+v", fs)
	}
}

func TestFailedOpenIsNotCounted(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	// A directory where the database file should be makes the first ping fail.
	if err := os.MkdirAll(filepath.Join(d.IndexDir(), FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), Options{Dirs: d, Getenv: func(string) string { return "" }}); err == nil {
		t.Fatal("open should fail")
	}
	if n := OpensOf(d.State); n != 0 {
		t.Errorf("failed open counted: %d", n)
	}
}

func TestDSN(t *testing.T) {
	loc := FSInfo{Kind: FSLocal}
	w := dsn("/p/m.db", loc, false)
	for _, s := range []string{"file:/p/m.db?", "_pragma=journal_mode(WAL)", "_pragma=busy_timeout(5000)", "_pragma=foreign_keys(on)", "_pragma=synchronous(NORMAL)", "_pragma=journal_size_limit(67108864)", "_txlock=immediate"} {
		if !strings.Contains(w, s) {
			t.Errorf("%s lacks %s", w, s)
		}
	}
	if strings.Contains(w, "mode=ro") {
		t.Error("writer is read-only")
	}
	r := dsn("/p/m.db", loc, true)
	if !strings.Contains(r, "mode=ro") || !strings.Contains(r, "_txlock=deferred") || strings.Contains(r, "immediate") {
		t.Errorf("reader dsn %s", r)
	}
	if n := dsn("/p/m.db", FSInfo{FSSynced, "dropbox"}, false); !strings.Contains(n, "journal_mode(DELETE)") {
		t.Errorf("synced dsn %s", n)
	}
}
