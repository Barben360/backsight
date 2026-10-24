package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteAtomicCreatesParentsAndSetsMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "f.json")
	if err := WriteAtomic(p, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if read(t, p) != "v1" {
		t.Error("content")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %o", fi.Mode().Perm())
		}
	}
}

func TestWriteAtomicReplaces(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	_ = WriteAtomic(p, []byte("old"), 0o600)
	if err := WriteAtomic(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if read(t, p) != "new" || len(entries(t, filepath.Dir(p))) != 1 {
		t.Errorf("content %q, dir %v", read(t, p), entries(t, filepath.Dir(p)))
	}
}

// A failure before the rename must leave the old file whole and no temp file.
func TestWriteAtomicFailureKeepsOldFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	_ = WriteAtomic(p, []byte("old"), 0o600)
	boom := errors.New("disk full")
	syncFile = func(*os.File) error { return boom }
	t.Cleanup(func() { syncFile = (*os.File).Sync })
	if err := WriteAtomic(p, []byte("new content"), 0o600); !errors.Is(err, boom) {
		t.Fatalf("want the sync error, got %v", err)
	}
	if read(t, p) != "old" {
		t.Errorf("old file damaged: %q", read(t, p))
	}
	if es := entries(t, filepath.Dir(p)); len(es) != 1 {
		t.Errorf("temp file left behind: %v", es)
	}
}

func TestReplaceIfUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	_ = WriteAtomic(p, []byte("user v1"), 0o644)
	h, err := HashFile(p)
	if err != nil || h == Absent {
		t.Fatalf("hash %q %v", h, err)
	}
	// Someone edits the file between our read and our write.
	_ = os.WriteFile(p, []byte("user v2"), 0o644)
	if err := ReplaceIfUnchanged(p, []byte("ours"), 0o644, h); !errors.Is(err, ErrConflict) {
		t.Errorf("want ErrConflict, got %v", err)
	}
	if read(t, p) != "user v2" {
		t.Error("a conflicting write must not touch the file")
	}
	h2, _ := HashFile(p)
	if err := ReplaceIfUnchanged(p, []byte("ours"), 0o644, h2); err != nil || read(t, p) != "ours" {
		t.Errorf("err %v, content %q", err, read(t, p))
	}
}

func TestReplaceIfUnchangedAbsent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "new.json")
	if h, _ := HashFile(p); h != Absent {
		t.Fatalf("missing file hash = %q", h)
	}
	if err := ReplaceIfUnchanged(p, []byte("x"), 0o600, Absent); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceIfUnchanged(p, []byte("y"), 0o600, Absent); !errors.Is(err, ErrConflict) {
		t.Errorf("file appeared meanwhile: want ErrConflict, got %v", err)
	}
}
