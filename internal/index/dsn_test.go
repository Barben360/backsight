package index

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestURIPathWindowsShapes: every Windows path form becomes a URI SQLite
// accepts: a drive path as it is, a UNC share with an empty authority
// (file:////srv/...), the \\?\ prefixes stripped. A pure string test: it
// runs on every OS.
func TestURIPathWindowsShapes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`C:\Users\me\state\index\metrics.db`, "file:C:/Users/me/state/index/metrics.db"},
		{`\\srv\share\state\metrics.db`, "file:////srv/share/state/metrics.db"},
		{`\\?\C:\Users\me\metrics.db`, "file:C:/Users/me/metrics.db"},
		{`\\?\UNC\srv\share\metrics.db`, "file:////srv/share/metrics.db"},
		{`C:\a?b#c%d\metrics.db`, "file:C:/a%3Fb%23c%25d/metrics.db"},
	} {
		if got := uriPath(tc.in, true); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"/home/me/state/metrics.db", "file:/home/me/state/metrics.db"},
		{"//net/share/metrics.db", "file:////net/share/metrics.db"},
		{`/odd\name/metrics.db`, `file:/odd\name/metrics.db`}, // a backslash is a name byte here
	} {
		if got := uriPath(tc.in, false); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestDSNsOpenADoubleSlashPath: another spelling of a file's path opens
// that file through every DSN builder: a path that starts with two
// slashes (a UNC share's form), where "file://x" would read x as a URI
// authority and fail; on Windows the \\?\ form of a drive path.
func TestDSNsOpenADoubleSlashPath(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "m.db")
	db, err := sql.Open(driverName, "file:"+plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE marker(x INTEGER); INSERT INTO marker VALUES (42)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	slashed := "/" + plain
	if runtime.GOOS == "windows" {
		slashed = `\\?\` + plain
	}
	if !strings.HasPrefix(slashed, "//") && !strings.HasPrefix(slashed, `\\?\`) {
		t.Fatalf("%s is not another spelling", slashed)
	}
	// In this order, as an open does: the writer first, which puts the
	// file in WAL mode; the read-only reader cannot (its journal_mode
	// pragma on a rollback-journal file is a write it is refused).
	for _, c := range []struct{ name, dsn string }{
		{"writer", dsn(slashed, FSInfo{Kind: FSLocal}, false)},
		{"reader", dsn(slashed, FSInfo{Kind: FSLocal}, true)},
		{"probe", probeDSN(slashed, false)},
		{"lock", lockDSN(slashed)},
	} {
		d := c.dsn
		t.Run(c.name, func(t *testing.T) {
			db, err := sql.Open(driverName, d)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			var x int
			if err := db.QueryRow(`SELECT x FROM marker`).Scan(&x); err != nil || x != 42 {
				t.Fatalf("%s: x=%d err=%v", d, x, err)
			}
		})
	}
	if _, err := os.Stat(plain); err != nil {
		t.Fatal(err)
	}
}

// TestWriterCheckpointPolicy: on Windows the writer keeps 64 MiB of pages
// and checkpoints once the WAL holds 256 MiB (65536 pages) instead of 1000
// pages: a first build's batch each dirties thousands of pages, and each
// checkpoint flushes them with FlushFileBuffers (Task 12b). Elsewhere the
// writer keeps SQLite's defaults (Linux unchanged), and the reader keeps
// them everywhere.
func TestWriterCheckpointPolicy(t *testing.T) {
	ix := mustOpen(t, opts(t, tempDirs(t), nil))
	defer mustClose(t, ix)
	read := func(db *sql.DB) (ckpt, cache int) {
		t.Helper()
		if err := db.QueryRow("PRAGMA wal_autocheckpoint").Scan(&ckpt); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("PRAGMA cache_size").Scan(&cache); err != nil {
			t.Fatal(err)
		}
		return ckpt, cache
	}
	wantCkpt, wantCache := 1000, -2000
	if runtime.GOOS == "windows" {
		wantCkpt, wantCache = 65536, -65536
	}
	if ckpt, cache := read(ix.sh.w); ckpt != wantCkpt || cache != wantCache {
		t.Errorf("writer: wal_autocheckpoint %d, cache_size %d; want %d, %d", ckpt, cache, wantCkpt, wantCache)
	}
	if ckpt, cache := read(ix.sh.r); ckpt != 1000 || cache != -2000 {
		t.Errorf("reader: wal_autocheckpoint %d, cache_size %d; want SQLite's 1000, -2000", ckpt, cache)
	}
}
