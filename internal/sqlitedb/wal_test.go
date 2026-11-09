package sqlitedb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProbeWALPassesAndLeavesNothing: a folder that takes WAL passes, and
// the scratch file, its companions and a leftover of a crashed run are
// gone afterwards.
func TestProbeWALPassesAndLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ProbeWALName()+"-wal")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ProbeWAL(dir) {
		t.Fatal("a local temp folder refused WAL")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("probe left %v", left)
	}
}

// TestProbeWALFailsAndLeavesNothing: a probe that cannot write its file
// fails, and leaves nothing of its own; a missing folder fails too.
func TestProbeWALFailsAndLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	obstacle := filepath.Join(dir, ProbeWALName()) // a directory where the scratch file goes
	if err := os.MkdirAll(filepath.Join(obstacle, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if ProbeWAL(dir) {
		t.Fatal("the probe passed over a directory in its file's place")
	}
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != ProbeWALName() { // only the obstacle
		t.Errorf("probe left %v", names)
	}
	if ProbeWAL(filepath.Join(dir, "missing")) {
		t.Error("a missing folder passed the probe")
	}
}

// TestHeaderIsWAL: a WAL database's header says so; a rollback-journal
// one, a short file and a missing one do not.
func TestHeaderIsWAL(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, wal bool) string {
		p := filepath.Join(dir, name)
		db := openDB(t, DSN(p, Mode{WAL: wal}, time.Second, ""))
		if _, err := db.Exec("CREATE TABLE t(x INTEGER)"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		return p
	}
	short := filepath.Join(dir, "short.db")
	if err := os.WriteFile(short, []byte("SQLite format 3"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{mk("wal.db", true), true},
		{mk("delete.db", false), false},
		{short, false},
		{filepath.Join(dir, "none.db"), false},
	} {
		if got := HeaderIsWAL(tc.path); got != tc.want {
			t.Errorf("HeaderIsWAL(%s) = %v, want %v", filepath.Base(tc.path), got, tc.want)
		}
	}
}

// TestApplyOneWaitsThenSeesTheHolderCommitted: another connection holds
// the write lock, applies the same migration and commits. ApplyOne
// applies nothing and reports it (false, nil), whether it got the lock
// after the holder committed, or the holder took the lock again at once
// and ApplyOne's wait ran out (it then reads the version without the
// lock). schema_migrations keeps the holder's one row.
func TestApplyOneWaitsThenSeesTheHolderCommitted(t *testing.T) {
	m := Migration{ID: "0001-a", Num: 1, SQL: "CREATE TABLE a(x INTEGER)", SHA256: "abc"}
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		busy    time.Duration
		relocks bool // the holder takes the lock again right after its commit
	}{
		{"lock freed", 5 * time.Second, false},
		{"lock taken again", 100 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "m.db")
			holder := openDB(t, DSN(path, Mode{WAL: true}, 5*time.Second, ""))
			holder.SetMaxOpenConns(1)
			htx, err := holder.BeginTx(ctx, nil) // BEGIN IMMEDIATE
			if err != nil {
				t.Fatal(err)
			}
			w := openDB(t, DSN(path, Mode{WAL: true}, tc.busy, ""))
			w.SetMaxOpenConns(1)
			type result struct {
				done bool
				err  error
			}
			got := make(chan result, 1)
			go func() {
				done, err := ApplyOne(ctx, w, m, 1, at, "v-waiter")
				got <- result{done, err}
			}()
			time.Sleep(30 * time.Millisecond) // let ApplyOne reach the lock
			for _, q := range []string{
				`CREATE TABLE IF NOT EXISTS schema_migrations(id TEXT PRIMARY KEY, sha256 TEXT NOT NULL, applied_at INTEGER NOT NULL, binary_version TEXT NOT NULL)`,
				m.SQL,
				`INSERT INTO schema_migrations VALUES ('0001-a', 'abc', 0, 'v-holder')`,
				`PRAGMA user_version = 1`,
			} {
				if _, err := htx.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if err := htx.Commit(); err != nil {
				t.Fatal(err)
			}
			var again interface{ Rollback() error }
			if tc.relocks {
				tx, err := holder.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				again = tx
			}
			r := <-got
			if again != nil {
				_ = again.Rollback()
			}
			if r.done || r.err != nil {
				t.Fatalf("ApplyOne = %v, %v; want false, nil", r.done, r.err)
			}
			var n int
			var by string
			if err := holder.QueryRow("SELECT count(*), max(binary_version) FROM schema_migrations").Scan(&n, &by); err != nil {
				t.Fatal(err)
			}
			if n != 1 || by != "v-holder" {
				t.Errorf("schema_migrations: %d rows, by %s; want the holder's one", n, by)
			}
			if v := userVersion(t, holder); v != 1 {
				t.Errorf("user_version %d, want 1", v)
			}
		})
	}
}
