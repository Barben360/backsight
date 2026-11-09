package index

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Barben360/backsight/internal/paths"
)

// modes is the permission of each of path's files that exist: the main
// file and its companions.
func modes(t *testing.T, path string) map[string]os.FileMode {
	t.Helper()
	out := map[string]os.FileMode{}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if st, err := os.Stat(path + suffix); err == nil {
			out[filepath.Base(path+suffix)] = st.Mode().Perm()
		}
	}
	return out
}

// The index and its -wal and -shm are its owner's alone (0600), as every
// other state file, whatever the umask: created so, and tightened when an
// older backsight left them 0644.
func TestIndexFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	ix, d := openT(t)
	if _, err := ix.sh.w.Exec("CREATE TABLE IF NOT EXISTS t_private (x)"); err != nil {
		t.Fatal(err)
	}
	got := modes(t, ix.Path())
	if len(got) < 3 {
		t.Fatalf("files %v: want the index, its -wal and -shm", got)
	}
	for f, m := range got {
		if m != 0o600 {
			t.Errorf("%s: mode %v, want 0600", f, m)
		}
	}
	_ = ix.Close()

	// An index an older binary left readable by others is tightened.
	path := filepath.Join(d.IndexDir(), FileName)
	for f := range modes(t, path) {
		if err := os.Chmod(filepath.Join(d.IndexDir(), f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ix2, err := Open(context.Background(), Options{Dirs: paths.Dirs{State: d.State}, Home: t.TempDir(), Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix2.Close() }()
	for f, m := range modes(t, path) {
		if m != 0o600 {
			t.Errorf("reopened: %s mode %v, want 0600", f, m)
		}
	}
}

// A rebuild writes a temp file and renames it over the index: the new
// index is private too.
func TestRebuiltIndexIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	r := newRecEnv(t)
	r.writeTel("2025-10", 20, 23)
	mustClose(t, r.openUp())
	if _, err := r.rebuild(RebuildOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	got := modes(t, filepath.Join(r.d.IndexDir(), FileName))
	if _, ok := got[FileName]; !ok {
		t.Fatalf("no index after the rebuild: %v", got)
	}
	for f, m := range got {
		if m != 0o600 {
			t.Errorf("%s: mode %v, want 0600", f, m)
		}
	}
}
