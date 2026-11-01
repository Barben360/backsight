package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

const (
	sqlA = "CREATE TABLE a(x)"
	sqlB = "CREATE TABLE b(x)"
)

var clock = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func regA() fstest.MapFS {
	return fstest.MapFS{"0001-a.sql": {Data: []byte(sqlA)}}
}

func regB() fstest.MapFS {
	r := regA()
	r["0002-b.sql"] = &fstest.MapFile{Data: []byte(sqlB)}
	return r
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func tempDirs(t *testing.T) paths.Dirs {
	t.Helper()
	return paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
}

func opts(t *testing.T, d paths.Dirs, reg fs.FS) Options {
	t.Helper()
	return Options{
		Dirs: d, Home: t.TempDir(), Getenv: func(string) string { return "" },
		Now: func() time.Time { return clock }, BinaryVersion: "v9.9.9-test", Migrations: reg,
	}
}

func mustOpen(t *testing.T, o Options) *Index {
	t.Helper()
	ix, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func mustClose(t *testing.T, ix *Index) {
	t.Helper()
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
}

// build opens and closes: the file is checkpointed, its header current.
func build(t *testing.T, o Options) {
	t.Helper()
	mustClose(t, mustOpen(t, o))
}

func mainFile(d paths.Dirs) string { return filepath.Join(d.IndexDir(), FileName) }

// headerVersion is user_version from a closed file's header (offset 60),
// read without SQLite so it cannot create a journal.
func headerVersion(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 64 {
		t.Fatalf("%s: %d bytes", path, len(raw))
	}
	return int(binary.BigEndian.Uint32(raw[60:64]))
}

func fileSum(t *testing.T, path string) (string, time.Time) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return hexSum(raw), st.ModTime()
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (%v)", path, err)
	}
}

type migRow struct {
	ID, SHA string
	At      int64
	Bin     string
}

func migrationRows(t *testing.T, ix *Index) []migRow {
	t.Helper()
	var out []migRow
	err := ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT id, sha256, applied_at, binary_version FROM schema_migrations ORDER BY id")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r migRow
			if err := rows.Scan(&r.ID, &r.SHA, &r.At, &r.Bin); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func liveVersion(t *testing.T, ix *Index) int {
	t.Helper()
	var v int
	if err := ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow("PRAGMA user_version").Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	return v
}

func hasTable(t *testing.T, ix *Index, name string) bool {
	t.Helper()
	var n int
	if err := ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", name).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestRegistryRules(t *testing.T) {
	reg, err := LoadRegistry(regB())
	if err != nil {
		t.Fatal(err)
	}
	if Head(reg) != 2 || len(reg) != 2 {
		t.Fatalf("head %d, %d migrations", Head(reg), len(reg))
	}
	want := []Migration{
		{ID: "0001-a", Num: 1, SQL: sqlA, SHA256: hexSum([]byte(sqlA))},
		{ID: "0002-b", Num: 2, SQL: sqlB, SHA256: hexSum([]byte(sqlB))},
	}
	if !slices.Equal(reg, want) {
		t.Errorf("registry %+v, want %+v", reg, want)
	}

	for name, tc := range map[string]struct {
		fs   fstest.MapFS
		want []string // in the error
	}{
		"gap":          {fstest.MapFS{"0001-a.sql": {}, "0003-c.sql": {}}, []string{"0003-c.sql", "number 2 expected"}},
		"no first":     {fstest.MapFS{"0002-b.sql": {}}, []string{"0002-b.sql", "number 1 expected"}},
		"duplicate":    {fstest.MapFS{"0001-a.sql": {}, "0001-b.sql": {}}, []string{"0001-b.sql"}},
		"short number": {fstest.MapFS{"1-a.sql": {}}, []string{"1-a.sql"}},
		"upper case":   {fstest.MapFS{"0001-A.sql": {}}, []string{"0001-A.sql"}},
		"underscore":   {fstest.MapFS{"0001_a.sql": {}}, []string{"0001_a.sql"}},
		"not sql":      {fstest.MapFS{"0001-a.sql": {}, "notes.txt": {}}, []string{"notes.txt"}},
		"directory":    {fstest.MapFS{"0001-a.sql": {}, "sub/0002-b.sql": {}}, []string{"sub"}},
	} {
		_, err := LoadRegistry(tc.fs)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q does not name %s", name, err, w)
			}
		}
	}

	empty, err := LoadRegistry(fstest.MapFS{})
	if err != nil || Head(empty) != 0 {
		t.Errorf("empty: head %d, %v", Head(empty), err)
	}
}

func TestMigrationsApplyInOrderAndAreRecorded(t *testing.T) {
	d := tempDirs(t)
	ix := mustOpen(t, opts(t, d, regB()))
	defer mustClose(t, ix)
	if ix.Schema() != 2 || liveVersion(t, ix) != 2 {
		t.Errorf("schema %d, user_version %d", ix.Schema(), liveVersion(t, ix))
	}
	if ix.Side() || ix.Path() != mainFile(d) {
		t.Errorf("side %v, path %s", ix.Side(), ix.Path())
	}
	at := clock.UnixMilli()
	want := []migRow{
		{"0001-a", hexSum([]byte(sqlA)), at, "v9.9.9-test"},
		{"0002-b", hexSum([]byte(sqlB)), at, "v9.9.9-test"},
	}
	if got := migrationRows(t, ix); !slices.Equal(got, want) {
		t.Errorf("rows %+v, want %+v", got, want)
	}
	if !hasTable(t, ix, "a") || !hasTable(t, ix, "b") {
		t.Error("a migration's tables are missing")
	}
}

func TestEachMigrationIsItsOwnTransaction(t *testing.T) {
	d := tempDirs(t)
	bad := regA()
	bad["0002-b.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE b(x); CREATE TABLE a(x);")}
	_, err := Open(context.Background(), opts(t, d, bad))
	if err == nil || !strings.Contains(err.Error(), "0002-b") {
		t.Fatalf("open: %v", err)
	}
	if n := OpensOf(d.State); n != 0 {
		t.Errorf("a failed open counted: %d", n)
	}
	if v := headerVersion(t, mainFile(d)); v != 1 {
		t.Errorf("user_version %d, want 1", v)
	}
	ix := mustOpen(t, opts(t, d, regA()))
	defer mustClose(t, ix)
	if hasTable(t, ix, "b") {
		t.Error("the failed migration's first statement was kept")
	}
	if got := migrationRows(t, ix); len(got) != 1 || got[0].ID != "0001-a" {
		t.Errorf("rows %+v", got)
	}
}

func TestReopeningAppliesNothing(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, regB()))
	later := opts(t, d, regB())
	later.Now = func() time.Time { return clock.Add(time.Hour) }
	res, err := Migrate(context.Background(), later, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Built || len(res.Applied) != 0 || len(res.Pending) != 0 {
		t.Errorf("result %+v", res)
	}
	ix := mustOpen(t, later)
	defer mustClose(t, ix)
	for _, r := range migrationRows(t, ix) {
		if r.At != clock.UnixMilli() {
			t.Errorf("%s reapplied at %d", r.ID, r.At)
		}
	}
}

func TestCheckPins(t *testing.T) {
	reg, err := LoadRegistry(regB())
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{"0001-a": hexSum([]byte(sqlA)), "0002-b": hexSum([]byte(sqlB))}
	if err := checkPins(reg, pins); err != nil {
		t.Errorf("exact pins: %v", err)
	}

	edited := regB()
	edited["0002-b.sql"].Data = []byte("CREATE TABLE b(y)") // one byte
	ereg, err := LoadRegistry(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPins(ereg, pins); err == nil || !strings.Contains(err.Error(), "0002-b") {
		t.Errorf("edited: %v", err)
	}

	if err := checkPins(reg, map[string]string{"0001-a": pins["0001-a"]}); err == nil || !strings.Contains(err.Error(), "0002-b") {
		t.Errorf("unpinned: %v", err)
	}

	short, err := LoadRegistry(regA())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPins(short, pins); err == nil || !strings.Contains(err.Error(), "0002-b") {
		t.Errorf("removed: %v", err)
	}
}

func TestNoMigrationDuringAnUncommittedUpdate(t *testing.T) {
	ctx := context.Background()
	mark := func(t *testing.T, d paths.Dirs, committed bool) {
		t.Helper()
		if err := state.WriteUpdateMarker(d, state.UpdateMarker{From: "v1", To: "v2", Committed: committed}); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrUpdateInProgress) {
			t.Fatalf("want ErrUpdateInProgress, got %v", err)
		}
		if err.Error() != "an update is being applied, try again in a minute" {
			t.Errorf("text %q", err)
		}
	}

	t.Run("missing index", func(t *testing.T) {
		d := tempDirs(t)
		mark(t, d, false)
		_, err := Open(ctx, opts(t, d, regB()))
		refused(t, err)
		absent(t, mainFile(d))
		absent(t, d.IndexDir())
	})
	t.Run("index below head", func(t *testing.T) {
		d := tempDirs(t)
		build(t, opts(t, d, regA()))
		mark(t, d, false)
		_, err := Open(ctx, opts(t, d, regB()))
		refused(t, err)
		_, err = Migrate(ctx, opts(t, d, regB()), false)
		refused(t, err)
		if v := headerVersion(t, mainFile(d)); v != 1 {
			t.Errorf("user_version %d", v)
		}
		res, err := Migrate(ctx, opts(t, d, regB()), true)
		if err != nil || !slices.Equal(res.Pending, []string{"0002-b"}) {
			t.Errorf("a plan is a read: %+v, %v", res, err)
		}
	})
	t.Run("index at head", func(t *testing.T) {
		d := tempDirs(t)
		build(t, opts(t, d, regB()))
		mark(t, d, false)
		ix := mustOpen(t, opts(t, d, regB()))
		defer mustClose(t, ix)
		if ix.Schema() != 2 {
			t.Errorf("schema %d", ix.Schema())
		}
	})
	t.Run("committed update", func(t *testing.T) {
		d := tempDirs(t)
		build(t, opts(t, d, regA()))
		mark(t, d, true)
		ix := mustOpen(t, opts(t, d, regB()))
		defer mustClose(t, ix)
		if ix.Schema() != 2 || liveVersion(t, ix) != 2 {
			t.Errorf("schema %d", ix.Schema())
		}
	})
	t.Run("unreadable marker", func(t *testing.T) {
		d := tempDirs(t)
		build(t, opts(t, d, regA()))
		if err := os.WriteFile(d.UpdateMarker(), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Open(ctx, opts(t, d, regB()))
		refused(t, err)
		if v := headerVersion(t, mainFile(d)); v != 1 {
			t.Errorf("user_version %d", v)
		}
		other := tempDirs(t)
		if err := os.MkdirAll(other.State, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(other.UpdateMarker(), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = Open(ctx, opts(t, other, regB()))
		refused(t, err)
		absent(t, mainFile(other))
	})
}

func TestAnOlderBinaryUsesASideFile(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, regB()))
	main := mainFile(d)
	sum, mtime := fileSum(t, main)

	ix := mustOpen(t, opts(t, d, regA()))
	if !strings.HasSuffix(ix.Path(), "metrics-v1.db") || filepath.Dir(ix.Path()) != d.IndexDir() {
		t.Errorf("path %s", ix.Path())
	}
	if !ix.Side() || ix.Schema() != 1 || liveVersion(t, ix) != 1 {
		t.Errorf("side %v, schema %d", ix.Side(), ix.Schema())
	}
	if hasTable(t, ix, "b") {
		t.Error("the side file has the newer schema")
	}
	absent(t, main+"-wal")
	absent(t, main+"-shm")
	mustClose(t, ix)

	if s, m := fileSum(t, main); s != sum || !m.Equal(mtime) {
		t.Error("metrics.db changed")
	}
	if v := headerVersion(t, main); v != 2 {
		t.Errorf("metrics.db user_version %d", v)
	}
	absent(t, main+"-wal")

	again := opts(t, d, regA())
	again.Now = func() time.Time { return clock.Add(time.Hour) }
	ix = mustOpen(t, again)
	defer mustClose(t, ix)
	if !ix.Side() {
		t.Error("second open is not on the side file")
	}
	if rows := migrationRows(t, ix); len(rows) != 1 || rows[0].At != clock.UnixMilli() {
		t.Errorf("side file rebuilt: %+v", rows)
	}
}

func TestMigratePlan(t *testing.T) {
	ctx := context.Background()
	t.Run("pending", func(t *testing.T) {
		d := tempDirs(t)
		build(t, opts(t, d, regA()))
		res, err := Migrate(ctx, opts(t, d, regB()), true)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Built || res.Newer || res.File != mainFile(d) || !slices.Equal(res.Pending, []string{"0002-b"}) || len(res.Applied) != 0 {
			t.Errorf("plan %+v", res)
		}
		if v := headerVersion(t, mainFile(d)); v != 1 {
			t.Errorf("a plan migrated: %d", v)
		}
		res, err = Migrate(ctx, opts(t, d, regB()), false)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(res.Applied, []string{"0002-b"}) {
			t.Errorf("apply %+v", res)
		}
		if v := headerVersion(t, mainFile(d)); v != 2 {
			t.Errorf("user_version %d", v)
		}
		if n := OpensOf(d.State); n != 2 {
			t.Errorf("opens %d", n)
		}
	})
	t.Run("missing", func(t *testing.T) {
		d := tempDirs(t)
		res, err := Migrate(ctx, opts(t, d, regB()), false)
		if err != nil {
			t.Fatal(err)
		}
		if res.Built || len(res.Applied) != 0 || len(res.Pending) != 0 {
			t.Errorf("missing %+v", res)
		}
		absent(t, mainFile(d))
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"pending":[]`) || !strings.Contains(string(raw), `"applied":[]`) {
			t.Errorf("json %s", raw)
		}
	})
	t.Run("newer", func(t *testing.T) {
		d := tempDirs(t)
		build(t, opts(t, d, regB()))
		sum, mtime := fileSum(t, mainFile(d))
		res, err := Migrate(ctx, opts(t, d, regA()), false)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Built || !res.Newer || res.File != filepath.Join(d.IndexDir(), "metrics-v1.db") || len(res.Applied) != 0 {
			t.Errorf("newer %+v", res)
		}
		if s, m := fileSum(t, mainFile(d)); s != sum || !m.Equal(mtime) {
			t.Error("metrics.db changed")
		}
		absent(t, res.File)
		absent(t, mainFile(d)+"-wal")
	})
}

func TestSideFileName(t *testing.T) {
	if got := SideFileName(7); got != "metrics-v7.db" {
		t.Errorf("got %s", got)
	}
}

func TestOpenRefusesABadRegistry(t *testing.T) {
	d := tempDirs(t)
	_, err := Open(context.Background(), opts(t, d, fstest.MapFS{"notes.txt": {}}))
	if err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("open: %v", err)
	}
	absent(t, mainFile(d))
}

// Processes setting up and migrating one new file at once: each gets its
// writer (one told SQLITE_BUSY while another sets the journal mode tries
// again), and the one that waited re-reads user_version inside its
// transaction and applies nothing twice. The race is won by chance, so
// each run plays many rounds on fresh files.
func TestConcurrentMigratorsApplyEachOnce(t *testing.T) {
	reg, err := LoadRegistry(regB())
	if err != nil {
		t.Fatal(err)
	}
	o := opts(t, tempDirs(t), regB())
	for round := range 20 {
		path := filepath.Join(t.TempDir(), FileName)
		var (
			wg      sync.WaitGroup
			lock    sync.Mutex
			applied []string
			errs    []error
		)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var got []string
				w, err := openWriter(context.Background(), path, FSInfo{Kind: FSLocal})
				if err == nil {
					got, err = applyPending(context.Background(), w, reg, Head(reg), o)
					err = errors.Join(err, w.Close())
				}
				lock.Lock()
				defer lock.Unlock()
				applied = append(applied, got...)
				errs = append(errs, err)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		slices.Sort(applied)
		if !slices.Equal(applied, []string{"0001-a", "0002-b"}) {
			t.Fatalf("round %d: applied %v", round, applied)
		}
	}
}

// Goroutines opening a missing index at once share one handle and build
// it once.
func TestConcurrentOpensBuildOnce(t *testing.T) {
	d := tempDirs(t)
	o := opts(t, d, regB())
	const n = 6
	var wg sync.WaitGroup
	ixs := make([]*Index, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ixs[i], errs[i] = Open(context.Background(), o)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	for _, ix := range ixs {
		if ix.Schema() != 2 {
			t.Errorf("schema %d", ix.Schema())
		}
	}
	if rows := migrationRows(t, ixs[0]); len(rows) != 2 {
		t.Errorf("rows %+v", rows)
	}
	if got := OpensOf(d.State); got != n {
		t.Errorf("opens %d", got)
	}
	for _, ix := range ixs {
		mustClose(t, ix)
	}
}

// A file another connection holds open keeps its last commits in the -wal:
// the version is read through it, not from the main file's header.
func TestVersionIsReadThroughALiveWAL(t *testing.T) {
	d := tempDirs(t)
	ix := mustOpen(t, opts(t, d, regB()))
	defer mustClose(t, ix)
	if _, err := os.Stat(mainFile(d) + "-wal"); err != nil {
		t.Fatal(err)
	}
	if v := headerVersion(t, mainFile(d)); v >= 2 {
		t.Fatalf("precondition: the bump is already in the main file (%d)", v)
	}
	res, err := Migrate(context.Background(), opts(t, d, regA()), true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Newer {
		t.Errorf("a schema 2 file read as older: %+v", res)
	}
}

// A rollback-journal file (network and synced folders) is read with a
// plain read-only connection, which creates no journal for it.
func TestNetworkFolderMigratesAndSidesWithoutWAL(t *testing.T) {
	old := detectFS
	detectFS = func(string, string, func(string) string) FSInfo { return FSInfo{FSNetwork, "nfs"} }
	t.Cleanup(func() { detectFS = old })
	d := tempDirs(t)
	build(t, opts(t, d, regA()))
	ix := mustOpen(t, opts(t, d, regB()))
	if ix.Schema() != 2 || ix.Side() {
		t.Errorf("schema %d, side %v", ix.Schema(), ix.Side())
	}
	mustClose(t, ix)
	ix = mustOpen(t, opts(t, d, regA()))
	defer mustClose(t, ix)
	if !ix.Side() || ix.Schema() != 1 {
		t.Errorf("schema %d, side %v", ix.Schema(), ix.Side())
	}
	absent(t, mainFile(d)+"-wal")
	absent(t, mainFile(d)+"-shm")
	absent(t, mainFile(d)+"-journal")
}

// A newer binary migrates metrics.db between the probe and the open: the
// writer reads the version again under its lock, writes nothing, and the
// open moves to the side file.
func TestNewerBetweenProbeAndOpen(t *testing.T) {
	ctx := context.Background()
	regOld, err := LoadRegistry(regA())
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []int{1, 0} { // at the head; below it (a pending migration)
		d := tempDirs(t)
		build(t, opts(t, d, regB()))
		main := mainFile(d)
		sum, mtime := fileSum(t, main)
		probed := target{reg: regOld, head: 1, built: true, known: true, version: stale, path: main}
		ix, _, err := openTarget(ctx, opts(t, d, regA()).withDefaults(), probed)
		if err != nil {
			t.Fatalf("stale %d: %v", stale, err)
		}
		if !ix.Side() || ix.Schema() != 1 || filepath.Base(ix.Path()) != "metrics-v1.db" {
			t.Errorf("stale %d: side %v, schema %d, path %s", stale, ix.Side(), ix.Schema(), ix.Path())
		}
		mustClose(t, ix)
		if s, m := fileSum(t, main); s != sum || !m.Equal(mtime) {
			t.Errorf("stale %d: metrics.db changed", stale)
		}
		if v := headerVersion(t, main); v != 2 {
			t.Errorf("stale %d: user_version %d", stale, v)
		}
		absent(t, main+"-wal")
	}
}

// A migration that finds the file above its head (a newer binary got the
// lock first) stops there and writes nothing.
func TestApplyStopsOnANewerFile(t *testing.T) {
	ctx := context.Background()
	d := tempDirs(t)
	build(t, opts(t, d, regB()))
	sum, _ := fileSum(t, mainFile(d))
	reg, err := LoadRegistry(regA())
	if err != nil {
		t.Fatal(err)
	}
	w, err := openWriter(ctx, mainFile(d), FSInfo{Kind: FSLocal})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := applyPending(ctx, w, reg, 1, opts(t, d, regA()).withDefaults())
	if cerr := w.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if !errors.Is(err, errNewer) || len(applied) != 0 {
		t.Errorf("applied %v, err %v", applied, err)
	}
	if s, _ := fileSum(t, mainFile(d)); s != sum {
		t.Error("metrics.db changed")
	}
}

// failProbe makes the read-only probe fail whenever a -wal exists, as it
// may on the files a killed process left.
func failProbe(t *testing.T) {
	t.Helper()
	old := probeVersion
	probeVersion = func(ctx context.Context, path string) (int, bool, error) {
		if _, err := os.Stat(path + "-wal"); err == nil {
			return 0, true, errors.New("injected probe failure")
		}
		return old(ctx, path)
	}
	t.Cleanup(func() { probeVersion = old })
}

// A failed probe beside a -wal is not fatal: the writer reads the
// version, under the same update gate and newer-schema rule.
func TestUnreadableProbeFallsBackToTheWriter(t *testing.T) {
	ctx := context.Background()
	failProbe(t)
	t.Run("at head", func(t *testing.T) {
		d := tempDirs(t)
		held := mustOpen(t, opts(t, d, regB())) // keeps the -wal
		defer mustClose(t, held)
		ix := mustOpen(t, opts(t, d, regB()))
		defer mustClose(t, ix)
		if ix.Schema() != 2 || ix.Side() {
			t.Errorf("schema %d, side %v", ix.Schema(), ix.Side())
		}
		if _, err := Migrate(ctx, opts(t, d, regB()), true); err == nil {
			t.Error("a plan cannot read the version: it must say so")
		}
	})
	t.Run("below head, update uncommitted", func(t *testing.T) {
		d := tempDirs(t)
		held := mustOpen(t, opts(t, d, regA()))
		if err := state.WriteUpdateMarker(d, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
			t.Fatal(err)
		}
		_, err := Open(ctx, opts(t, d, regB()))
		if !errors.Is(err, ErrUpdateInProgress) {
			t.Errorf("want ErrUpdateInProgress, got %v", err)
		}
		_, err = Migrate(ctx, opts(t, d, regB()), false)
		if !errors.Is(err, ErrUpdateInProgress) {
			t.Errorf("migrate: want ErrUpdateInProgress, got %v", err)
		}
		mustClose(t, held)
		if v := headerVersion(t, mainFile(d)); v != 1 {
			t.Errorf("user_version %d", v)
		}
	})
	t.Run("below head", func(t *testing.T) {
		d := tempDirs(t)
		held := mustOpen(t, opts(t, d, regA()))
		defer mustClose(t, held)
		res, err := Migrate(ctx, opts(t, d, regB()), false)
		if err != nil || !slices.Equal(res.Applied, []string{"0002-b"}) || !slices.Equal(res.Pending, []string{"0002-b"}) {
			t.Errorf("migrate %+v, %v", res, err)
		}
	})
	t.Run("newer", func(t *testing.T) {
		d := tempDirs(t)
		held := mustOpen(t, opts(t, d, regB()))
		ix := mustOpen(t, opts(t, d, regA()))
		if !ix.Side() || ix.Schema() != 1 {
			t.Errorf("schema %d, side %v", ix.Schema(), ix.Side())
		}
		mustClose(t, ix)
		mustClose(t, held)
		if v := headerVersion(t, mainFile(d)); v != 2 {
			t.Errorf("metrics.db user_version %d", v)
		}
	})
	t.Run("no wal: still an error", func(t *testing.T) {
		d := tempDirs(t)
		build(t, opts(t, d, regA()))
		old := probeVersion
		probeVersion = func(context.Context, string) (int, bool, error) { return 0, true, errors.New("injected") }
		defer func() { probeVersion = old }()
		if _, err := Open(ctx, opts(t, d, regA())); err == nil {
			t.Error("a failed probe without a -wal must fail the open")
		}
	})
}

// The files a killed process leaves (a copy of a live -wal and -shm)
// open: the version and rows in the -wal are seen, and an older binary
// still goes to its side file.
func TestOpensTheFilesAKilledProcessLeft(t *testing.T) {
	src := tempDirs(t)
	held := mustOpen(t, opts(t, src, regB()))
	crash := func(t *testing.T) paths.Dirs {
		t.Helper()
		d := tempDirs(t)
		if err := os.MkdirAll(d.IndexDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, ext := range []string{"", "-wal", "-shm"} {
			raw, err := os.ReadFile(mainFile(src) + ext)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mainFile(d)+ext, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	same, older := crash(t), crash(t)
	mustClose(t, held)
	if v := headerVersion(t, mainFile(same)); v >= 2 {
		t.Fatalf("precondition: the bump is already in the main file (%d)", v)
	}

	ix := mustOpen(t, opts(t, same, regB()))
	if ix.Schema() != 2 || ix.Side() || len(migrationRows(t, ix)) != 2 {
		t.Errorf("schema %d, side %v", ix.Schema(), ix.Side())
	}
	mustClose(t, ix)

	ix = mustOpen(t, opts(t, older, regA()))
	defer mustClose(t, ix)
	if !ix.Side() || ix.Schema() != 1 {
		t.Errorf("schema %d, side %v", ix.Schema(), ix.Side())
	}
}

func regBad() fstest.MapFS {
	r := regA()
	r["0002-bad.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE broken(")}
	return r
}

func statusOf(t *testing.T, d paths.Dirs) recoveryState {
	t.Helper()
	st, err := readStatus(d)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A migration that fails is recorded in status.json as a class and a
// time, never as text; a later success clears it.
func TestMigrateRecordsItsFailureAndClearsItOnSuccess(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, regA()))
	o := opts(t, d, regBad())
	if _, err := Migrate(context.Background(), o, false); err == nil {
		t.Fatal("a broken migration succeeded")
	}
	st := statusOf(t, d)
	if !st.MigrateFailedAt.Equal(clock) || st.MigrateReason != "other" {
		t.Errorf("%+v", st)
	}
	raw, _ := os.ReadFile(statusPath(d))
	if strings.Contains(string(raw), "broken") || strings.Contains(string(raw), "syntax") {
		t.Errorf("status.json holds error text: %s", raw)
	}
	// planning neither records nor clears
	if _, err := Migrate(context.Background(), opts(t, d, regB()), true); err != nil {
		t.Fatal(err)
	}
	if statusOf(t, d).MigrateFailedAt.IsZero() {
		t.Error("a plan cleared the failure")
	}
	res, err := Migrate(context.Background(), opts(t, d, regB()), false)
	if err != nil || !slices.Equal(res.Applied, []string{"0002-b"}) {
		t.Fatalf("%+v %v", res, err)
	}
	if st := statusOf(t, d); !st.MigrateFailedAt.IsZero() || st.MigrateReason != "" {
		t.Errorf("not cleared: %+v", st)
	}
}

// Waiting for an update is no failure of the migration.
func TestMigrateDoesNotRecordTheUpdateGate(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, regA()))
	if err := state.WriteUpdateMarker(d, state.UpdateMarker{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(context.Background(), opts(t, d, regB()), false); !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("%v", err)
	}
	if !statusOf(t, d).MigrateFailedAt.IsZero() {
		t.Error("the update gate was recorded as a failure")
	}
}

// A success changes nothing on disk when no failure is recorded.
func TestMigrateSuccessWritesNoStatus(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, regA()))
	before := readFile(t, statusPath(d))
	if _, err := Migrate(context.Background(), opts(t, d, regA()), false); err != nil {
		t.Fatal(err)
	}
	if readFile(t, statusPath(d)) != before {
		t.Error("status.json was rewritten")
	}
}

// TestMigrationAppliedMeanwhileIsNotAnError: two processes open a fresh
// index at once; one read the version before the other migrated, then
// meets the other's write lock (its first batch) past the busy timeout.
// The migrations it planned are applied already: it goes on.
func TestMigrationAppliedMeanwhileIsNotAnError(t *testing.T) {
	shortBusyWait(t)
	d := tempDirs(t)
	o := opts(t, d, nil)
	ix := mustOpen(t, o)
	defer mustClose(t, ix)
	reg, err := registry(o)
	if err != nil {
		t.Fatal(err)
	}
	release := holdWriteLock(t, ix.Path(), ix.FS())
	defer release()
	w, err := openWriter(context.Background(), ix.Path(), ix.FS())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	// The stale plan: every migration, as read before the other process's.
	applied, err := applyPending(context.Background(), w, reg, reg[len(reg)-1].Num, o)
	if err != nil || len(applied) != 0 {
		t.Fatalf("applied %v, %v; want none and no error", applied, err)
	}
}

// TestMigrationStillPendingStaysBusy: the same lock on a file nobody
// migrated is the busy error it was.
func TestMigrationStillPendingStaysBusy(t *testing.T) {
	shortBusyWait(t)
	o := opts(t, tempDirs(t), nil)
	reg, err := registry(o)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fresh.db")
	fsi := FSInfo{Kind: FSLocal}
	w, err := openWriter(context.Background(), path, fsi)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	release := holdWriteLock(t, path, fsi)
	defer release()
	applied, err := applyPending(context.Background(), w, reg, reg[len(reg)-1].Num, o)
	if !isBusy(err) || len(applied) != 0 {
		t.Fatalf("applied %v, %v; want none and SQLITE_BUSY", applied, err)
	}
}
