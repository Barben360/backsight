package ledgerdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/sqlitedb"
	"github.com/Barben360/backsight/internal/state"
)

// clock is the tests' fixed now, to the second (every filesystem keeps
// an mtime set to it exactly).
var clock = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func tempOpts(t *testing.T) Options {
	t.Helper()
	return Options{
		Dirs:          paths.Dirs{State: filepath.Join(t.TempDir(), "state")},
		Getenv:        func(string) string { return "" },
		BinaryVersion: "v-test",
		Now:           func() time.Time { return clock },
	}
}

func mainPath(o Options) string { return filepath.Join(o.Dirs.FeedbackDir(), FileName) }

func mustOpenWriter(t *testing.T, o Options) *Writer {
	t.Helper()
	w, err := OpenWriter(context.Background(), o)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	return w
}

func mustCloseWriter(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatalf("Writer.Close: %v", err)
	}
}

// seedRows builds o's ledger.db holding entries, written through the row
// mapping, and the generation row a fold writes.
func seedRows(t *testing.T, o Options, entries map[string]feedback.Entry, meta map[string]string) {
	t.Helper()
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for id, e := range entries {
		if _, err := tx.Exec(upsertMemory, rowFromEntry(id, e).args()...); err != nil {
			t.Fatal(err)
		}
	}
	m := map[string]string{"generation": "1"}
	for k, v := range meta {
		m[k] = v
	}
	for k, v := range m {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func mustOpenReader(t *testing.T, o Options) *Reader {
	t.Helper()
	r, err := OpenReader(o, time.Second)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// headerVersion is path's user_version, read from its header (bytes
// 60-63) without opening it.
func headerVersion(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 64 {
		t.Fatalf("%s: %d bytes, no header", path, len(b))
	}
	return int(binary.BigEndian.Uint32(b[60:64]))
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (%v)", path, err)
	}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// forceFS makes the folder detection answer kind; put back after the test.
func forceFS(t *testing.T, kind sqlitedb.FSKind) {
	t.Helper()
	old := detectFS
	detectFS = func(string, string, func(string) string) sqlitedb.FSInfo { return sqlitedb.FSInfo{Kind: kind} }
	t.Cleanup(func() { detectFS = old })
}

// fakeProbe makes the WAL probe answer ok, counting its calls, with a
// fresh verdict cache; both are put back after the test.
func fakeProbe(t *testing.T, ok bool) *int {
	t.Helper()
	old := walProbe
	n := new(int)
	walProbe = func(string) bool { *n++; return ok }
	resetWALVerdicts()
	t.Cleanup(func() { walProbe = old; resetWALVerdicts() })
	return n
}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// sameEntry compares two entries field by field, times with Equal and
// floats exactly.
func sameEntry(t *testing.T, id string, got, want feedback.Entry) {
	t.Helper()
	eqT := func(field string, g, w time.Time) {
		if !g.Equal(w) {
			t.Errorf("%s.%s = %v, want %v", id, field, g, w)
		}
	}
	if got.Shown != want.Shown || got.Wrong != want.Wrong {
		t.Errorf("%s: shown %d wrong %d, want %d %d", id, got.Shown, got.Wrong, want.Shown, want.Wrong)
	}
	if got.Credits != want.Credits || got.Misses != want.Misses {
		t.Errorf("%s: credits %v misses %v, want %v %v", id, got.Credits, got.Misses, want.Credits, want.Misses)
	}
	eqT("CreditsAt", got.CreditsAt, want.CreditsAt)
	eqT("MissesAt", got.MissesAt, want.MissesAt)
	eqT("LastShown", got.LastShown, want.LastShown)
	eqT("LastCredited", got.LastCredited, want.LastCredited)
	eqT("UnhiddenAt", got.UnhiddenAt, want.UnhiddenAt)
	if got.Imported != want.Imported {
		t.Errorf("%s: imported %+v, want %+v", id, got.Imported, want.Imported)
	}
	switch {
	case (got.Hidden == nil) != (want.Hidden == nil):
		t.Errorf("%s: hidden %+v, want %+v", id, got.Hidden, want.Hidden)
	case got.Hidden != nil:
		eqT("Hidden.Since", got.Hidden.Since, want.Hidden.Since)
		if got.Hidden.Verdict != want.Hidden.Verdict || got.Hidden.Note != want.Hidden.Note {
			t.Errorf("%s: hidden %+v, want %+v", id, *got.Hidden, *want.Hidden)
		}
	}
}

// onlyErr: err is want, and none of the reader's other three errors.
func onlyErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: %v, want %v", what, err, want)
	}
	for _, other := range []error{ErrMissing, ErrBusy, ErrUnreadable, ErrSchema} {
		if !errors.Is(other, want) && errors.Is(err, other) {
			t.Errorf("%s: %v is also %v", what, err, other)
		}
	}
}

func TestRowRoundTripIsExact(t *testing.T) {
	zone := time.FixedZone("x", -7*3600)
	at := func(d, ns int) time.Time { return time.Date(2026, 10, d, 9, 30, 15, ns, zone) }
	full := feedback.Entry{
		Shown:        7,
		Credits:      2.718281828459045,
		CreditsAt:    at(1, 123456789),
		Misses:       0.1 + 0.2, // not 0.3: the bits must come back
		MissesAt:     at(2, 1),
		LastShown:    at(3, 999999999),
		LastCredited: at(4, 42),
		Wrong:        2,
		Hidden:       &feedback.Hidden{Since: at(5, 7), Verdict: "wrong", Note: "doc-1"},
		UnhiddenAt:   at(6, 8),
		Imported:     feedback.Imported{Shown: 3, Credits: 1.5},
	}
	o := tempOpts(t)
	seedRows(t, o, map[string]feedback.Entry{"m-full": full, "m-zero": {}}, nil)
	got, err := mustOpenReader(t, o).Get([]string{"m-full", "m-zero", "m-none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Get returned %d entries, want 2 (m-none has none): %v", len(got), got)
	}
	sameEntry(t, "m-full", got["m-full"], full)
	sameEntry(t, "m-zero", got["m-zero"], feedback.Entry{})
	for _, ts := range []time.Time{got["m-zero"].CreditsAt, got["m-zero"].LastShown, got["m-zero"].UnhiddenAt} {
		if !ts.IsZero() {
			t.Errorf("a zero time came back as %v", ts)
		}
	}
}

// TestReaderMethodsOverRows: Hidden is the hidden entries only, All every
// entry with the history import time, Any whether there is one; Get
// takes more ids than one statement holds.
func TestReaderMethodsOverRows(t *testing.T) {
	o := tempOpts(t)
	seedRows(t, o, nil, nil)
	r := mustOpenReader(t, o)
	if has, err := r.Any(); err != nil || has {
		t.Errorf("Any on an empty ledger: %v, %v", has, err)
	}
	if all, err := r.All(); err != nil || len(all.Memories) != 0 || all.Memories == nil {
		t.Errorf("All on an empty ledger: %+v, %v", all, err)
	}

	o = tempOpts(t)
	since := time.Date(2026, 9, 1, 0, 0, 0, 5, time.UTC)
	imported := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	entries := map[string]feedback.Entry{"h": {Shown: 1, Hidden: &feedback.Hidden{Since: since}}}
	var ids []string
	for i := range 1200 {
		id := fmt.Sprintf("m%04d", i)
		entries[id] = feedback.Entry{Shown: i}
		ids = append(ids, id)
	}
	seedRows(t, o, entries, map[string]string{"history_imported_ns": fmt.Sprint(imported.UnixNano())})
	r = mustOpenReader(t, o)
	if has, err := r.Any(); err != nil || !has {
		t.Errorf("Any: %v, %v", has, err)
	}
	hidden, err := r.Hidden()
	if err != nil || len(hidden) != 1 || hidden["h"].Hidden == nil || !hidden["h"].Hidden.Since.Equal(since) {
		t.Errorf("Hidden: %+v, %v", hidden, err)
	}
	all, err := r.All()
	if err != nil || len(all.Memories) != len(entries) || all.Schema != feedback.LedgerSchema || !all.HistoryImported.Equal(imported) {
		t.Errorf("All: %d memories, schema %d, imported %v, %v", len(all.Memories), all.Schema, all.HistoryImported, err)
	}
	got, err := r.Get(append(ids, "none"))
	if err != nil || len(got) != 1200 || got["m1199"].Shown != 1199 || got["m0000"].Shown != 0 {
		t.Errorf("Get of 1201 ids: %d entries, m1199 %+v, %v", len(got), got["m1199"], err)
	}
	if got, err := r.Get(nil); err != nil || len(got) != 0 {
		t.Errorf("Get(nil): %v, %v", got, err)
	}
}

func TestReaderNeverCreatesOrMigrates(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		o := tempOpts(t)
		_, err := OpenReader(o, 20*time.Millisecond)
		onlyErr(t, "missing", err, ErrMissing)
		absent(t, mainPath(o))
	})
	t.Run("user_version 0", func(t *testing.T) {
		o := tempOpts(t)
		if err := os.MkdirAll(o.Dirs.FeedbackDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", sqlitedb.URI(mainPath(o)))
		if err != nil {
			t.Fatal(err)
		}
		// schema_migrations marks a file migrations ran on: without it, a
		// file at user_version 0 is one being built (ErrMissing).
		if _, err := db.Exec("CREATE TABLE other(x INTEGER); CREATE TABLE schema_migrations(id TEXT)"); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		_, err = OpenReader(o, 20*time.Millisecond)
		onlyErr(t, "user_version 0", err, ErrSchema)
		if v := headerVersion(t, mainPath(o)); v != 0 {
			t.Errorf("user_version %d after the reader, want 0", v)
		}
	})
	t.Run("user_version 0, being built", func(t *testing.T) {
		o := tempOpts(t)
		writeFile(t, mainPath(o), nil) // created, not yet migrated
		_, err := OpenReader(o, 20*time.Millisecond)
		onlyErr(t, "a file being built", err, ErrMissing)
	})
	t.Run("newer", func(t *testing.T) {
		o := tempOpts(t)
		o.Migrations = withTestMigration(t)
		seedRows(t, o, nil, nil)
		o.Migrations = nil
		_, err := OpenReader(o, 20*time.Millisecond)
		onlyErr(t, "newer", err, ErrSchema)
		if v := headerVersion(t, mainPath(o)); v != 2 {
			t.Errorf("user_version %d after the reader, want 2", v)
		}
	})
	t.Run("never folded", func(t *testing.T) {
		o := tempOpts(t)
		setSeam(t, &beforeFoldCommit, func() error { return errors.New("crash before the first build's commit") })
		if _, err := OpenWriter(context.Background(), o); err == nil {
			t.Fatal("OpenWriter succeeded without its first build")
		}
		_, err := OpenReader(o, 20*time.Millisecond)
		onlyErr(t, "no generation row", err, ErrMissing)
	})
}

func TestReaderBusyIsErrBusy(t *testing.T) {
	forceFS(t, sqlitedb.FSNetwork) // the rollback journal: a writer's lock keeps readers out
	o := tempOpts(t)
	seedRows(t, o, map[string]feedback.Entry{"m": {Shown: 1}}, nil)
	if sqlitedb.HeaderIsWAL(mainPath(o)) {
		t.Fatal("ledger.db is in WAL mode on a network folder")
	}
	holder, err := sql.Open("sqlite", sqlitedb.DSN(mainPath(o), sqlitedb.Mode{}, time.Second, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	ctx := context.Background()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(o, 20*time.Millisecond)
	if err == nil {
		_, err = r.Get([]string{"m"})
		_ = r.Close()
	}
	onlyErr(t, "a held file", err, ErrBusy)
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	got, err := mustOpenReader(t, o).Get([]string{"m"})
	if err != nil || got["m"].Shown != 1 {
		t.Errorf("after the lock: %v, %v", got, err)
	}
}

// TestReadWaitsOneBusyWindow: Read checks the file in the transaction
// it reads in, so a writer that takes the file as the read starts costs
// one busy wait: one transaction, ErrBusy. OpenReader's own check would
// be a second transaction, and a second wait.
func TestReadWaitsOneBusyWindow(t *testing.T) {
	forceFS(t, sqlitedb.FSNetwork) // the rollback journal: a writer's lock keeps readers out
	o := tempOpts(t)
	seedRows(t, o, map[string]feedback.Entry{"m": {Shown: 1}}, nil)
	txs := 0
	setSeam(t, &beforeRead, func() { txs++ })
	getAny := func(r *Reader) error {
		got, has, err := r.GetAny([]string{"m"})
		if err == nil && (!has || got["m"].Shown != 1) {
			t.Errorf("GetAny: %+v, %v", got, has)
		}
		return err
	}
	if err := Read(o, 20*time.Millisecond, getAny); err != nil || txs != 1 {
		t.Fatalf("Read: %v in %d transaction(s), want one", err, txs)
	}

	holder, err := sql.Open("sqlite", sqlitedb.DSN(mainPath(o), sqlitedb.Mode{}, time.Second, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	ctx := context.Background()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	txs = 0
	setSeam(t, &beforeRead, func() { // the writer arrives as the transaction starts
		if txs++; txs == 1 {
			if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
				t.Error(err)
			}
		}
	})
	err = Read(o, 20*time.Millisecond, getAny)
	onlyErr(t, "a writer arriving", err, ErrBusy)
	if txs != 1 {
		t.Errorf("%d transaction(s), each a busy wait: want one", txs)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func TestReaderErrorsMapToFour(t *testing.T) {
	t.Run("garbage", func(t *testing.T) {
		o := tempOpts(t)
		writeFile(t, mainPath(o), bytes.Repeat([]byte("not a database "), 100))
		_, err := OpenReader(o, 20*time.Millisecond)
		onlyErr(t, "garbage", err, ErrUnreadable)
	})
	t.Run("a directory in its place", func(t *testing.T) {
		o := tempOpts(t)
		if err := os.MkdirAll(mainPath(o), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := OpenReader(o, 20*time.Millisecond)
		onlyErr(t, "a directory", err, ErrUnreadable)
	})
	t.Run("an unreadable folder", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("folder permissions are ACLs on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root reads any folder")
		}
		o := tempOpts(t)
		seedRows(t, o, map[string]feedback.Entry{"m": {Shown: 1}}, nil)
		dir := o.Dirs.FeedbackDir()
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		_, err := OpenReader(o, 20*time.Millisecond)
		onlyErr(t, "an unreadable folder", err, ErrUnreadable)
	})
}

func TestWriterJournalAndSync(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		forceFS(t, sqlitedb.FSLocal)
		fakeProbe(t, true)
		w := mustOpenWriter(t, tempOpts(t))
		defer mustCloseWriter(t, w)
		if got := pragma(t, w.db, "journal_mode"); got != "wal" {
			t.Errorf("journal_mode %s, want wal", got)
		}
		if got := pragma(t, w.db, "synchronous"); got != "1" {
			t.Errorf("synchronous %s, want 1 (NORMAL)", got)
		}
	})
	t.Run("network", func(t *testing.T) {
		forceFS(t, sqlitedb.FSNetwork)
		calls := fakeProbe(t, true)
		w := mustOpenWriter(t, tempOpts(t))
		defer mustCloseWriter(t, w)
		if got := pragma(t, w.db, "journal_mode"); got != "delete" {
			t.Errorf("journal_mode %s, want delete", got)
		}
		if got := pragma(t, w.db, "synchronous"); got != "2" {
			t.Errorf("synchronous %s, want 2 (FULL)", got)
		}
		if *calls != 0 {
			t.Errorf("a network folder was probed %d times", *calls)
		}
	})
	t.Run("local folder refusing WAL, once per folder", func(t *testing.T) {
		forceFS(t, sqlitedb.FSLocal)
		calls := fakeProbe(t, false)
		o := tempOpts(t)
		mustCloseWriter(t, mustOpenWriter(t, o))
		w := mustOpenWriter(t, o)
		defer mustCloseWriter(t, w)
		if got := pragma(t, w.db, "journal_mode"); got != "delete" {
			t.Errorf("journal_mode %s, want delete", got)
		}
		if *calls != 1 {
			t.Errorf("probe called %d times for two opens of one folder", *calls)
		}
	})
	t.Run("a WAL file is not probed", func(t *testing.T) {
		forceFS(t, sqlitedb.FSLocal)
		fakeProbe(t, true)
		o := tempOpts(t)
		mustCloseWriter(t, mustOpenWriter(t, o))
		calls := fakeProbe(t, false)
		w := mustOpenWriter(t, o)
		defer mustCloseWriter(t, w)
		if got := pragma(t, w.db, "journal_mode"); got != "wal" || *calls != 0 {
			t.Errorf("journal_mode %s after %d probes, want wal and none", got, *calls)
		}
	})
}

func TestNoMigrationDuringAnUpdate(t *testing.T) {
	uncommitted := func(t *testing.T, d paths.Dirs) {
		t.Helper()
		if err := state.WriteUpdateMarker(d, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("missing file", func(t *testing.T) {
		o := tempOpts(t)
		uncommitted(t, o.Dirs)
		_, err := OpenWriter(context.Background(), o)
		if !errors.Is(err, ErrUpdateInProgress) {
			t.Fatalf("OpenWriter: %v, want ErrUpdateInProgress", err)
		}
		absent(t, mainPath(o))
		absent(t, o.Dirs.FeedbackDir())
	})
	t.Run("pending migration", func(t *testing.T) {
		o := tempOpts(t)
		writeFile(t, mainPath(o), readBytes(t, fixturePath(1)))
		uncommitted(t, o.Dirs)
		o.Migrations = withTestMigration(t)
		_, err := OpenWriter(context.Background(), o)
		if !errors.Is(err, ErrUpdateInProgress) {
			t.Fatalf("OpenWriter: %v, want ErrUpdateInProgress", err)
		}
		if v := headerVersion(t, mainPath(o)); v != 1 {
			t.Errorf("user_version %d, want 1", v)
		}
		// Committed, the same open migrates.
		if err := state.WriteUpdateMarker(o.Dirs, state.UpdateMarker{From: "v1", To: "v2", Committed: true}); err != nil {
			t.Fatal(err)
		}
		mustCloseWriter(t, mustOpenWriter(t, o))
		if v := headerVersion(t, mainPath(o)); v != 2 {
			t.Errorf("user_version %d after the commit point, want 2", v)
		}
	})
	t.Run("at the head", func(t *testing.T) {
		o := tempOpts(t)
		mustCloseWriter(t, mustOpenWriter(t, o))
		uncommitted(t, o.Dirs)
		mustCloseWriter(t, mustOpenWriter(t, o)) // nothing to migrate: it opens
	})
}

// TestNewerSchemaUsesTheSideFile: a ledger.db a newer binary migrated is
// never written, whatever its journal mode (a rollback-journal file on a
// folder that now takes WAL would be converted by a writer's connection).
func TestNewerSchemaUsesTheSideFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		built sqlitedb.FSKind // where the newer binary built it
	}{{"WAL", sqlitedb.FSLocal}, {"rollback journal", sqlitedb.FSNetwork}} {
		t.Run(tc.name, func(t *testing.T) {
			fakeProbe(t, true)
			forceFS(t, tc.built)
			o := tempOpts(t)
			o.Migrations = withTestMigration(t)
			mustCloseWriter(t, mustOpenWriter(t, o))
			main := mainPath(o)
			before := readBytes(t, main)
			forceFS(t, sqlitedb.FSLocal)
			o.Migrations = nil
			side := filepath.Join(o.Dirs.FeedbackDir(), SideFileName(1))
			for i := range 2 { // built, then reused
				w := mustOpenWriter(t, o)
				if w.path != side || !w.side {
					t.Errorf("open %d: writes %s (side %v), want %s", i, w.path, w.side, side)
				}
				if got := pragma(t, w.db, "user_version"); got != "1" {
					t.Errorf("open %d: the side file is at user_version %s, want 1", i, got)
				}
				mustCloseWriter(t, w)
			}
			if got := readBytes(t, main); !bytes.Equal(got, before) {
				t.Error("ledger.db's bytes changed")
			}
			absent(t, main+"-wal")
			absent(t, main+"-shm")
			if v := headerVersion(t, side); v != 1 {
				t.Errorf("%s at user_version %d, want 1", SideFileName(1), v)
			}
		})
	}
}

func TestSideFileName(t *testing.T) {
	if got := SideFileName(3); got != "ledger-v3.db" {
		t.Errorf("SideFileName(3) = %s", got)
	}
}

func TestCorruptFileIsSetAsideOncePerDay(t *testing.T) {
	o := tempOpts(t)
	main := mainPath(o)
	corrupt := main + ".corrupt"
	first := bytes.Repeat([]byte("first garbage "), 100)
	writeFile(t, main, first)
	old := clock.Add(-48 * time.Hour) // set aside, its mtime must become now
	if err := os.Chtimes(main, old, old); err != nil {
		t.Fatal(err)
	}
	// A -wal beside it (one commit of a page other than the header's):
	// the read-only probe leaves the version to the writer, which finds
	// the corruption; the fresh file must not inherit the -wal.
	writeFile(t, main+"-wal", staleWAL(t))
	mustCloseWriter(t, mustOpenWriter(t, o))
	// (The writer's connection may have checkpointed the -wal's page into
	// it: its start is the garbage file's.)
	aside := readBytes(t, corrupt)
	if !bytes.HasPrefix(aside, first) {
		t.Fatal("ledger.db.corrupt does not hold the garbage file")
	}
	if fi, err := os.Stat(corrupt); err != nil || !fi.ModTime().Equal(clock) {
		t.Errorf("ledger.db.corrupt mtime %v (%v), want %v", fi.ModTime(), err, clock)
	}
	if v := headerVersion(t, main); v != 1 {
		t.Errorf("the fresh ledger.db is at user_version %d, want 1", v)
	}

	second := bytes.Repeat([]byte("second garbage "), 100)
	writeFile(t, main, second)
	_, err := OpenWriter(context.Background(), o)
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("a second garbage file within the day: %v, want ErrUnreadable", err)
	}
	if !bytes.Equal(readBytes(t, main), second) || !bytes.Equal(readBytes(t, corrupt), aside) {
		t.Error("the second garbage file was moved within the day")
	}

	day := clock.Add(-25 * time.Hour)
	if err := os.Chtimes(corrupt, day, day); err != nil {
		t.Fatal(err)
	}
	mustCloseWriter(t, mustOpenWriter(t, o))
	if !bytes.Equal(readBytes(t, corrupt), second) {
		t.Error("after a day, the second garbage file was not set aside")
	}
	if v := headerVersion(t, main); v != 1 {
		t.Errorf("the fresh ledger.db is at user_version %d, want 1", v)
	}
}

// TestSetAsideFromTheFuture: a .corrupt stamped after now (the clock
// stepped back since) is not "within the day": the corrupt ledger.db is
// set aside at once, not left failing until the clock passes the stamp.
func TestSetAsideFromTheFuture(t *testing.T) {
	o := tempOpts(t)
	main := mainPath(o)
	corrupt := main + ".corrupt"
	writeFile(t, corrupt, []byte("set aside in the clock's future"))
	future := clock.Add(time.Hour)
	if err := os.Chtimes(corrupt, future, future); err != nil {
		t.Fatal(err)
	}
	garbage := bytes.Repeat([]byte("garbage "), 200)
	writeFile(t, main, garbage)
	w, err := OpenWriter(context.Background(), o)
	if err != nil {
		t.Fatalf("a corrupt ledger.db beside a .corrupt from the future: %v, want it set aside", err)
	}
	mustCloseWriter(t, w)
	if !bytes.HasPrefix(readBytes(t, corrupt), garbage) {
		t.Error("ledger.db.corrupt does not hold the garbage file")
	}
	if fi, err := os.Stat(corrupt); err != nil || !fi.ModTime().Equal(clock) {
		t.Errorf("ledger.db.corrupt mtime %v (%v), want now %v", fi.ModTime(), err, clock)
	}
	if v := headerVersion(t, main); v != 1 {
		t.Errorf("the fresh ledger.db is at user_version %d, want 1", v)
	}
}

// TestForcedSetAsideIgnoresTheDay: with ForceSetAside (a command run on
// purpose) a corrupt file is set aside within a day of the last
// set-aside; without it (the tick) the bound holds.
func TestForcedSetAsideIgnoresTheDay(t *testing.T) {
	o := tempOpts(t)
	appendLines(t, o, settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeCredit)))
	main := mainPath(o)
	corrupt := main + ".corrupt"
	writeFile(t, corrupt, []byte("set aside an hour ago"))
	hour := clock.Add(-time.Hour)
	if err := os.Chtimes(corrupt, hour, hour); err != nil {
		t.Fatal(err)
	}
	garbage := bytes.Repeat([]byte("garbage "), 200)
	writeFile(t, main, garbage)
	if _, err := OpenWriter(context.Background(), o); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("the tick's open within the day: %v, want ErrUnreadable", err)
	}
	forced := o
	forced.ForceSetAside = true
	w := mustOpenWriter(t, forced)
	mustFold(t, w, testParams)
	mustCloseWriter(t, w)
	if !bytes.Equal(readBytes(t, corrupt), garbage) {
		t.Error("the forced open did not set the garbage file aside")
	}
	matchesOracle(t, "the fresh file", o, testParams)

	// The bound still holds for the tick after a forced set-aside.
	writeFile(t, main, garbage)
	if _, err := OpenWriter(context.Background(), o); !errors.Is(err, ErrUnreadable) {
		t.Errorf("the tick's open after a forced set-aside: %v, want ErrUnreadable", err)
	}
}

// staleWAL is a -wal holding one commit of a table's page, not of the
// header page: beside a garbage file, the header still reads as garbage.
func staleWAL(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stale.db")
	db, err := sql.Open("sqlite", sqlitedb.DSN(p, sqlitedb.Mode{WAL: true}, time.Second, "&_pragma=wal_autocheckpoint(0)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	for _, q := range []string{
		"CREATE TABLE stale(x INTEGER)",
		"PRAGMA wal_checkpoint(TRUNCATE)",
		"INSERT INTO stale VALUES (1)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return readBytes(t, p+"-wal") // read while open: the frame is still there
}

func resetWALVerdicts() {
	walMu.Lock()
	defer walMu.Unlock()
	clear(walVerdict)
}
