package index

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// corrupt overwrites bytes 4096–8191 of a closed index with 0xFF: page 2,
// the root of schema_migrations, which an open at head never reads, so
// only a quick_check sees it.
func corrupt(t *testing.T, path string) {
	t.Helper()
	corruptPage(t, path, 2)
}

// corruptPage overwrites page n (4 KiB pages, from 1) with 0xFF.
func corruptPage(t *testing.T, path string, n int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xFF}, 4096), (n-1)*4096); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// recEnv is a state directory with telemetry, opened and closed at will
// under a settable clock.
type recEnv struct {
	t     *testing.T
	d     paths.Dirs
	clock *clockT
}

func newRecEnv(t *testing.T) *recEnv {
	t.Helper()
	r := &recEnv{t: t, d: tempDirs(t), clock: &clockT{t: clock}}
	r.writeTel("2026-09", 1, 3)
	r.writeTel("2026-10", 4, 6)
	if err := os.MkdirAll(r.d.IndexDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	return r
}

// writeTel writes prompt lines numbered from … to into the month file.
func (r *recEnv) writeTel(month string, from, to int) {
	r.t.Helper()
	var b strings.Builder
	for n := from; n <= to; n++ {
		b.WriteString(promptLine(n, 0))
	}
	writeFile(r.t, filepath.Join(r.d.TelemetryDir(), "events-"+month+".jsonl"), b.String())
}

func (r *recEnv) opts() Options {
	o := opts(r.t, r.d, nil)
	o.Now = r.clock.Now
	return o
}

func (r *recEnv) main() string { return mainFile(r.d) }

// openUp opens the index and catches it up in full.
func (r *recEnv) openUp() *Index {
	r.t.Helper()
	ix := mustOpen(r.t, r.opts())
	r.t.Cleanup(func() { _ = ix.Close() })
	if _, err := ix.CatchUp(context.Background(), ViewBudget); err != nil {
		r.t.Fatal(err)
	}
	return ix
}

// build opens, catches up and closes: the file is checkpointed.
func (r *recEnv) build() {
	r.t.Helper()
	mustClose(r.t, r.openUp())
}

func (r *recEnv) status() recoveryState {
	r.t.Helper()
	st, err := readStatus(r.d)
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func (r *recEnv) setStatus(fn func(*recoveryState)) {
	r.t.Helper()
	st := r.status()
	fn(&st)
	if err := writeStatus(r.d, st); err != nil {
		r.t.Fatal(err)
	}
}

// fresh is the dump of a new index built over a copy of the telemetry.
func (r *recEnv) fresh() string {
	r.t.Helper()
	return freshDump(r.t, &ingestEnv{t: r.t, d: r.d})
}

func (r *recEnv) asideFiles() []string {
	r.t.Helper()
	ents, err := os.ReadDir(r.d.IndexDir())
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".corrupt") {
			out = append(out, e.Name())
		}
	}
	return out
}

func sameTime(a, b time.Time) bool { return a.Equal(b) }

// companionsAtRename records, at each rename from or to path, which of
// path's -wal, -shm and -journal existed: one left there would be taken
// for the new file's own.
func companionsAtRename(t *testing.T, path string) *[]string {
	t.Helper()
	seen := &[]string{}
	prev := renameFile
	renameFile = func(from, to string) error {
		if from == path || to == path {
			for _, sfx := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Stat(path + sfx); err == nil {
					*seen = append(*seen, sfx)
				}
			}
		}
		return prev(from, to)
	}
	t.Cleanup(func() { renameFile = prev })
	return seen
}

func TestNotADatabaseIsSetAsideAndRebuilt(t *testing.T) {
	r := newRecEnv(t)
	writeFile(t, r.main(), "not a database")
	writeFile(t, r.main()+"-wal", "a stale write-ahead log")
	writeFile(t, r.main()+"-journal", "a stale rollback journal")
	ix := r.openUp()
	if got := readFile(t, r.main()+".corrupt"); got != "not a database" {
		t.Errorf("metrics.db.corrupt = %q", got)
	}
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Errorf("recovered index differs from a fresh build:\n%s\nwant\n%s", got, want)
	}
	mustClose(t, ix)
	absent(t, r.main()+"-wal")
	absent(t, r.main()+"-journal")
	st := r.status()
	if !sameTime(st.RebuiltAt, clock) || !st.StoppedAt.IsZero() {
		t.Errorf("status %+v", st)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(statusPath(r.d))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("status.json mode %v", info.Mode().Perm())
		}
	}
}

// The corrupt file's companions go with it: none is left at the rename
// for the fresh file to take as its own (SQLite removes most of them
// itself; a -shm beside a file that is not a database it leaves).
func TestSetAsideLeavesNoCompanion(t *testing.T) {
	r := newRecEnv(t)
	writeFile(t, r.main(), "not a database")
	writeFile(t, r.main()+"-shm", "a stale shared-memory index")
	left := companionsAtRename(t, r.main())
	mustClose(t, r.openUp())
	if len(*left) != 0 {
		t.Errorf("left beside the new file: %v", *left)
	}
	if got := readFile(t, r.main()+".corrupt"); got != "not a database" {
		t.Errorf("metrics.db.corrupt = %q", got)
	}
}

func TestFailedQuickCheckTriggersRecovery(t *testing.T) {
	r := newRecEnv(t)
	r.build()
	corrupt(t, r.main())
	bad := readFile(t, r.main())
	r.setStatus(func(s *recoveryState) { s.CheckedAt = clock.Add(-25 * time.Hour) })
	ix := r.openUp()
	// Its header aside (it left WAL mode to be moved), it is that file.
	if aside := readFile(t, r.main()+".corrupt"); len(aside) != len(bad) || aside[100:] != bad[100:] {
		t.Error("metrics.db.corrupt is not the corrupt file")
	}
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Errorf("recovered index differs from a fresh build:\n%s\nwant\n%s", got, want)
	}
	if st := r.status(); !sameTime(st.RebuiltAt, clock) || !sameTime(st.CheckedAt, clock) {
		t.Errorf("status %+v", st)
	}
}

// A quick_check younger than 24 h is not run again: the same corruption
// goes unseen at open (the check is daily, not on every view).
func TestQuickCheckRunsAtMostDaily(t *testing.T) {
	n := 0
	prev := onQuickCheck
	onQuickCheck = func() { n++ }
	t.Cleanup(func() { onQuickCheck = prev })
	r := newRecEnv(t)
	r.build()
	if n != 1 {
		t.Fatalf("first open: %d quick_check(s)", n)
	}
	r.clock.Add(time.Hour)
	r.build()
	if n != 1 {
		t.Errorf("1 h later: %d quick_check(s)", n)
	}
	r.clock.Add(24 * time.Hour)
	r.build()
	if n != 2 {
		t.Errorf("25 h later: %d quick_check(s)", n)
	}
	if st := r.status(); !sameTime(st.CheckedAt, clock.Add(25*time.Hour)) {
		t.Errorf("checked_at %v", st.CheckedAt)
	}
}

func TestASecondCorruptionWithin24hStops(t *testing.T) {
	r := newRecEnv(t)
	writeFile(t, r.main(), "not a database")
	r.build()
	r.clock.Add(time.Hour)
	writeFile(t, r.main(), "not a database, again")
	_, err := Open(context.Background(), r.opts())
	if !errors.Is(err, ErrUnreadable) || err.Error() != "the metrics index is unreadable" {
		t.Fatalf("want ErrUnreadable, got %v", err)
	}
	if got := readFile(t, r.main()); got != "not a database, again" {
		t.Errorf("the file moved: %q", got)
	}
	if got := readFile(t, r.main()+".corrupt"); got != "not a database" {
		t.Errorf("metrics.db.corrupt = %q, want the first one", got)
	}
	st := r.status()
	if !sameTime(st.StoppedAt, clock.Add(time.Hour)) || !sameTime(st.RebuiltAt, clock) {
		t.Errorf("status %+v", st)
	}
	// Opening again within the 24 h still stops.
	if _, err := Open(context.Background(), r.opts()); !errors.Is(err, ErrUnreadable) {
		t.Errorf("second try: %v", err)
	}

	r.clock.Add(24 * time.Hour)
	ix := r.openUp()
	if got := readFile(t, r.main()+".corrupt"); got != "not a database, again" {
		t.Errorf("metrics.db.corrupt = %q, want the second one", got)
	}
	if got := r.asideFiles(); len(got) != 1 {
		t.Errorf("aside files %v", got)
	}
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Errorf("recovered index differs from a fresh build")
	}
	st = r.status()
	if !sameTime(st.RebuiltAt, clock.Add(25*time.Hour)) || !st.StoppedAt.IsZero() {
		t.Errorf("status %+v", st)
	}
}

// A corruption error from a statement makes the next open run the
// quick_check, which sets the file aside.
func TestCorruptionSeenByACatchUpIsCheckedAtTheNextOpen(t *testing.T) {
	r := newRecEnv(t)
	r.build()
	corruptPage(t, r.main(), 4) // tel_prompt's root
	ix := mustOpen(t, r.opts())
	r.writeTel("2026-10", 4, 9)
	_, err := ix.CatchUp(context.Background(), ViewBudget)
	if c := classify(err); c != "corrupt" {
		t.Fatalf("catch-up: %v (%s)", err, c)
	}
	mustClose(t, ix)
	if st := r.status(); !st.CheckedAt.IsZero() {
		t.Errorf("the check is not due: %+v", st)
	}
	ix = r.openUp()
	if len(r.asideFiles()) != 1 {
		t.Errorf("aside files %v", r.asideFiles())
	}
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Errorf("recovered index differs from a fresh build")
	}
}

// onNetworkFS makes every open of the test take the rollback journal:
// an idle connection then holds no lock that SQLite would see.
func onNetworkFS(t *testing.T) {
	t.Helper()
	old := detectFS
	detectFS = func(string, string, func(string) string) FSInfo { return FSInfo{FSNetwork, "nfs"} }
	t.Cleanup(func() { detectFS = old })
}

// A file another handle of this process holds is never moved from under
// it: in WAL mode its connections refuse the lock, in DELETE mode (idle,
// no lock) the process's own registry does.
func TestRecoveryLeavesAFileThisProcessHolds(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(fmt.Sprintf("network=%v", network), func(t *testing.T) {
			if network {
				onNetworkFS(t)
			}
			r := newRecEnv(t)
			r.build()
			held := mustOpen(t, r.opts())
			defer mustClose(t, held)
			corrupt(t, r.main())
			r.setStatus(func(s *recoveryState) { s.CheckedAt = clock.Add(-25 * time.Hour) })
			_, err := Open(context.Background(), r.opts())
			if c := classify(err); c != "corrupt" {
				t.Fatalf("open: %v (%s)", err, c)
			}
			if got := r.asideFiles(); len(got) != 0 {
				t.Errorf("set aside under a live handle: %v", got)
			}
			if st := r.status(); !st.RebuiltAt.IsZero() {
				t.Errorf("status %+v", st)
			}
		})
	}
}

// Another process's connection (a raw one here) keeps the file in place
// too; the check stays due, and the file is set aside once it is closed.
func TestRecoveryLeavesAFileAnotherConnectionHolds(t *testing.T) {
	r := newRecEnv(t)
	r.build()
	corrupt(t, r.main())
	raw, err := sql.Open(driverName, "file:"+r.main())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM cursors`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	r.setStatus(func(s *recoveryState) { s.CheckedAt = clock.Add(-25 * time.Hour) })
	_, err = Open(context.Background(), r.opts())
	if c := classify(err); c != "corrupt" {
		t.Fatalf("open: %v (%s)", err, c)
	}
	if got := r.asideFiles(); len(got) != 0 {
		t.Errorf("set aside under a live connection: %v", got)
	}
	if st := r.status(); !st.RebuiltAt.IsZero() {
		t.Errorf("status %+v", st)
	}
	_ = raw.Close()
	ix := r.openUp()
	if got := r.asideFiles(); len(got) != 1 {
		t.Errorf("aside files %v", got)
	}
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Error("recovered index differs from a fresh build")
	}
}

// Without a status to hold the stamp, nothing is set aside: the stamp is
// what keeps a recovery from looping.
func TestRecoveryNeedsItsStatus(t *testing.T) {
	r := newRecEnv(t)
	writeFile(t, r.main(), "not a database")
	if err := os.Mkdir(statusPath(r.d), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), r.opts())
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("want ErrUnreadable, got %v", err)
	}
	if got := readFile(t, r.main()); got != "not a database" {
		t.Errorf("the file moved: %q", got)
	}
	absent(t, r.main()+".corrupt")
}

// Recovery builds a new file: it waits for an uncommitted update like
// any migration, and moves nothing meanwhile.
func TestRecoveryWaitsForTheUpdate(t *testing.T) {
	r := newRecEnv(t)
	writeFile(t, r.main(), "not a database")
	if err := state.WriteUpdateMarker(r.d, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), r.opts())
	if !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("want ErrUpdateInProgress, got %v", err)
	}
	if got := readFile(t, r.main()); got != "not a database" {
		t.Errorf("the file moved: %q", got)
	}
	absent(t, r.main()+".corrupt")
	if st := r.status(); !st.RebuiltAt.IsZero() {
		t.Errorf("status %+v", st)
	}
}

func TestFullDiskLeavesTheFileIntact(t *testing.T) {
	r := newRecEnv(t)
	ix := r.openUp()
	before := dump(t, ix)
	var lines strings.Builder
	for n := 100; n < 2100; n++ {
		lines.WriteString(promptLine(n, 0))
	}
	f, err := os.OpenFile(filepath.Join(r.d.TelemetryDir(), "events-2026-10.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(lines.String()); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	var pages int64
	if err := ix.sh.w.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.sh.w.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
		t.Fatal(err)
	}
	_, err = ix.CatchUp(context.Background(), ViewBudget)
	if !errors.Is(err, ErrDiskFull) || err.Error() != "the disk is full: the metrics index was left as it was" {
		t.Fatalf("want ErrDiskFull, got %v", err)
	}
	if got := dump(t, ix); got != before {
		t.Errorf("the index changed:\n%s\nwas\n%s", got, before)
	}
	if got := pragma(t, ix.sh.r, "quick_check"); got != "ok" {
		t.Errorf("quick_check %s", got)
	}
	if _, err := ix.sh.w.Exec("PRAGMA max_page_count = 4294967294"); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.CatchUp(context.Background(), ViewBudget); err != nil {
		t.Fatal(err)
	}
	if got, want := dump(t, ix), r.fresh(); got != want {
		t.Error("after the limit is lifted, the index differs from a fresh build")
	}
}

func TestOpenFollowsAReplacedFile(t *testing.T) {
	r := newRecEnv(t)
	r.build()
	other := newRecEnv(t)
	other.writeTel("2026-09", 50, 52)
	other.writeTel("2026-10", 53, 55)
	other.build()
	old := filepath.Join(t.TempDir(), "old.db")
	oldSum, _ := fileSum(t, r.main())
	done := false
	prev := afterConnect
	afterConnect = func(path string) {
		if done || path != r.main() {
			return
		}
		done = true
		if err := os.Rename(path, old); err != nil {
			t.Error(err)
		}
		if err := os.Rename(other.main(), path); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterConnect = prev })
	ix := mustOpen(t, r.opts())
	e := &ingestEnv{t: t, d: r.d, ix: ix}
	if got := e.ns(); len(got) != 6 || got[0] != 50 {
		t.Errorf("rows %v, want the new file's", got)
	}
	var first int64 // through the writer, which connected first
	if err := ix.sh.w.QueryRow(`SELECT min(n) FROM tel_prompt`).Scan(&first); err != nil || first != 50 {
		t.Errorf("writer reads %d (%v), want the new file's", first, err)
	}
	r.writeTel("2026-10", 4, 9) // the catch-up has lines to write
	if _, err := ix.CatchUp(context.Background(), ViewBudget); err != nil {
		t.Fatal(err)
	}
	mustClose(t, ix)
	if s, _ := fileSum(t, old); s != oldSum {
		t.Error("the old file got a write")
	}
}

func TestStatusFile(t *testing.T) {
	d := tempDirs(t)
	st, err := readStatus(d)
	if err != nil || st != (recoveryState{}) {
		t.Fatalf("absent: %+v %v", st, err)
	}
	absent(t, d.IndexDir())
	writeFile(t, statusPath(d), "{not json")
	if st, err := readStatus(d); err != nil || st != (recoveryState{}) {
		t.Errorf("unparsable: %+v %v", st, err)
	}
	want := recoveryState{CheckedAt: clock, MigrateFailedAt: clock.Add(time.Hour), MigrateReason: "busy"}
	if err := writeStatus(d, want); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, statusPath(d)); got != `{"checked_at":"2026-10-30T12:00:00Z","migrate_failed_at":"2026-10-30T13:00:00Z","migrate_reason":"busy"}`+"\n" {
		t.Errorf("status.json %s", got)
	}
	if got, err := readStatus(d); err != nil || !got.CheckedAt.Equal(want.CheckedAt) || got.MigrateReason != "busy" {
		t.Errorf("read back %+v %v", got, err)
	}
}

func TestClassify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	writeFile(t, path, "not a database")
	_, _, notadb := fileVersion(context.Background(), path)
	_, statErr := os.Stat(filepath.Join(t.TempDir(), "none"))
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{notadb, "notadb"},
		{ErrDiskFull, "full"},
		{ErrUnreadable, "corrupt"},
		{errQuickCheck, "corrupt"},
		{ErrInUse, "busy"},
		{statErr, "io"},
		{fs.ErrPermission, "io"},
		{errors.New("something"), "other"},
	} {
		if got := classify(c.err); got != c.want {
			t.Errorf("classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
