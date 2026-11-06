package index

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProbe makes the WAL probe answer err, counting its calls, with a
// fresh verdict cache; both are put back after the test.
func fakeProbe(t *testing.T, err error) *int {
	t.Helper()
	old := walProbe
	n := new(int)
	walProbe = func(string) error { *n++; return err }
	resetWALVerdicts()
	t.Cleanup(func() { walProbe = old; resetWALVerdicts() })
	return n
}

func TestFailedWALProbeOpensInDeleteMode(t *testing.T) {
	calls := fakeProbe(t, errors.New("readonly"))
	ix, d := openT(t)
	defer mustClose(t, ix)
	if got := pragma(t, ix.sh.w, "journal_mode"); got != "delete" {
		t.Errorf("writer journal_mode %s", got)
	}
	if got := pragma(t, ix.sh.r, "journal_mode"); got != "delete" {
		t.Errorf("reader journal_mode %s", got)
	}
	if got := pragma(t, ix.sh.w, "synchronous"); got != "2" { // FULL
		t.Errorf("synchronous %s, want 2 (FULL)", got)
	}
	if ix.FS().Kind != FSLocal {
		t.Errorf("the folder is still local: %+v", ix.FS())
	}
	if _, err := ix.sh.w.Exec("CREATE TABLE t(x); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.IndexDir(), FileName+"-wal")); err == nil {
		t.Error("a wal file exists")
	}
	if *calls != 1 {
		t.Errorf("probe called %d times", *calls)
	}
}

func TestPassingWALProbeKeepsWAL(t *testing.T) {
	calls := fakeProbe(t, nil)
	ix, _ := openT(t)
	defer mustClose(t, ix)
	if got := pragma(t, ix.sh.w, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode %s", got)
	}
	if got := pragma(t, ix.sh.w, "synchronous"); got != "1" { // NORMAL
		t.Errorf("synchronous %s, want 1 (NORMAL)", got)
	}
	if *calls != 1 {
		t.Errorf("probe called %d times", *calls)
	}
}

// The verdict holds for the process: a second open of the folder does not
// probe again, and keeps the first one's answer.
func TestWALVerdictIsCachedPerFolder(t *testing.T) {
	calls := fakeProbe(t, errors.New("readonly"))
	o := opts(t, tempDirs(t), nil)
	mustClose(t, mustOpen(t, o))
	ix := mustOpen(t, o)
	defer mustClose(t, ix)
	if *calls != 1 {
		t.Errorf("probe called %d times for two opens", *calls)
	}
	if got := pragma(t, ix.sh.w, "journal_mode"); got != "delete" {
		t.Errorf("second open journal_mode %s", got)
	}
}

// A file already in WAL mode proves its folder took WAL once, and a
// rollback-journal connection cannot open it anyway: no probe, WAL kept.
func TestExistingWALFileIsNotProbed(t *testing.T) {
	fakeProbe(t, nil)
	o := opts(t, tempDirs(t), nil)
	mustClose(t, mustOpen(t, o))
	resetWALVerdicts()
	calls := fakeProbe(t, errors.New("readonly"))
	ix := mustOpen(t, o)
	defer mustClose(t, ix)
	if *calls != 0 {
		t.Errorf("probe called %d times on a WAL file", *calls)
	}
	if got := pragma(t, ix.sh.w, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode %s", got)
	}
}

func TestNonLocalFolderIsNotProbed(t *testing.T) {
	calls := fakeProbe(t, nil)
	old := detectFS
	detectFS = func(string, string, func(string) string) FSInfo { return FSInfo{FSNetwork, "nfs"} }
	t.Cleanup(func() { detectFS = old })
	ix, _ := openT(t)
	defer mustClose(t, ix)
	if *calls != 0 {
		t.Errorf("probe called %d times", *calls)
	}
}

// The real probe works in a folder that takes WAL, and leaves nothing.
func TestRealWALProbe(t *testing.T) {
	dir := t.TempDir()
	// A leftover of a crashed run is cleared too.
	stale := filepath.Join(dir, probeName()+"-wal")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeWAL(dir); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("probe left %v", left)
	}
}

// A probe that fails says so, and still leaves nothing.
func TestRealWALProbeFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	// A directory where the probe's file goes: the open fails.
	obstacle := filepath.Join(dir, probeName())
	if err := os.MkdirAll(filepath.Join(obstacle, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := probeWAL(dir)
	if err == nil {
		t.Fatal("the probe should fail")
	}
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != probeName() { // only the obstacle
		t.Errorf("probe left %v", names)
	}
	if err := probeWAL(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing folder should fail the probe")
	}
}
