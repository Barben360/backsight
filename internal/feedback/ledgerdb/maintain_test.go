package ledgerdb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// sqliteFile makes path a SQLite database of one table, closed.
func sqliteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE t(x)`); err != nil {
		t.Fatal(err)
	}
}

func mustMaintain(t *testing.T, o Options) []string {
	t.Helper()
	removed, err := Maintain(context.Background(), o)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	return removed
}

// TestMaintainRemovesLowerLedgerSideFiles: a side file of a lower head
// than this binary's goes, with its -wal and -shm, once nobody holds it;
// this head's side file, ledger.db and its companions stay.
func TestMaintainRemovesLowerLedgerSideFiles(t *testing.T) {
	o := tempOpts(t)
	dir := feedbackDir(o)
	mustCloseWriter(t, mustOpenWriter(t, o))
	lower := filepath.Join(dir, SideFileName(0))
	head := filepath.Join(dir, SideFileName(1))
	sqliteFile(t, lower)
	sqliteFile(t, head)
	writeFile(t, lower+"-wal", nil)
	writeFile(t, lower+"-shm", nil)
	writeFile(t, head+"-wal", []byte("its log"))
	stray := filepath.Join(dir, "ledger-vx.db")
	writeFile(t, stray, []byte("not a side file"))

	// Held by another connection: Seize refuses, the file is kept.
	raw, err := sql.Open(driverName, "file:"+lower)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if removed := mustMaintain(t, o); len(removed) != 0 {
		t.Errorf("held: removed %v", removed)
	}
	if _, err := os.Stat(lower); err != nil {
		t.Errorf("a held side file was removed: %v", err)
	}
	_ = tx.Rollback()
	_ = raw.Close()

	if removed := mustMaintain(t, o); !slices.Equal(removed, []string{SideFileName(0)}) {
		t.Errorf("released: removed %v, want [%s]", removed, SideFileName(0))
	}
	for _, sfx := range []string{"", "-wal", "-shm", "-journal"} {
		absent(t, lower+sfx)
	}
	for _, p := range []string{head, head + "-wal", mainPath(o), stray} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
		}
	}
	if got := string(readBytes(t, head+"-wal")); got != "its log" {
		t.Errorf("the head's side file's -wal: %q", got)
	}
	if removed := mustMaintain(t, o); len(removed) != 0 {
		t.Errorf("third run removed %v", removed)
	}
}

// No feedback folder: nothing to do, nothing created.
func TestMaintainWithoutAFolder(t *testing.T) {
	o := tempOpts(t)
	if removed := mustMaintain(t, o); len(removed) != 0 {
		t.Errorf("removed %v", removed)
	}
	absent(t, feedbackDir(o))
}
