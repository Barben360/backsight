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

func TestReadHashed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	data, h, err := ReadHashed(p)
	if err != nil || data != nil || h != Absent {
		t.Errorf("missing file: %q %q %v", data, h, err)
	}
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, h, err = ReadHashed(p)
	want, _ := HashFile(p)
	// sha256("abc")
	if err != nil || string(data) != "abc" || h != want || h != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("%q %q %v", data, h, err)
	}
}

func TestHashFileOfDirectoryFails(t *testing.T) {
	if _, err := HashFile(t.TempDir()); err == nil {
		t.Error("a directory has no content hash")
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// Dotfile managers symlink settings.json: the link must survive a write.
func TestWritesFollowSymlinks(t *testing.T) {
	for name, write := range map[string]func(link string, h string) error{
		"WriteAtomic":        func(link, _ string) error { return WriteAtomic(link, []byte("new"), 0o600) },
		"ReplaceIfUnchanged": func(link, h string) error { return ReplaceIfUnchanged(link, []byte("new"), 0o600, h) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "dotfiles", "settings.json")
			_ = os.MkdirAll(filepath.Dir(target), 0o700)
			_ = os.WriteFile(target, []byte("old"), 0o600)
			link := filepath.Join(dir, "settings.json")
			symlinkOrSkip(t, target, link)
			h, _ := HashFile(link)
			if err := write(link, h); err != nil {
				t.Fatal(err)
			}
			if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the symlink was replaced by a regular file")
			}
			if read(t, target) != "new" {
				t.Errorf("target not updated: %q", read(t, target))
			}
		})
	}
}

func TestWriteRefusesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "settings.json")
	symlinkOrSkip(t, filepath.Join(dir, "gone", "settings.json"), link)
	if err := WriteAtomic(link, []byte("x"), 0o600); err == nil {
		t.Error("a dangling symlink must be refused, not replaced")
	}
	if fi, _ := os.Lstat(link); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the dangling link must be left as it was")
	}
}

func TestReplaceIfUnchangedKeepsExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	p := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(p, []byte("old"), 0o644)
	_ = os.Chmod(p, 0o644)
	h, _ := HashFile(p)
	if err := ReplaceIfUnchanged(p, []byte("new"), 0o600, h); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Errorf("user file mode changed to %o", fi.Mode().Perm())
	}
}

func TestReplaceIfUnchangedDeletedAfterHash(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(p, []byte("old"), 0o600)
	h, _ := HashFile(p)
	_ = os.Remove(p)
	if err := ReplaceIfUnchanged(p, []byte("new"), 0o600, h); !errors.Is(err, ErrConflict) {
		t.Errorf("want ErrConflict, got %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("a file the user deleted must not be recreated")
	}
}

func TestRenameFailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil { // a non-empty dir cannot be renamed over
		t.Fatal(err)
	}
	if err := WriteAtomic(target, []byte("x"), 0o600); err == nil {
		t.Fatal("rename over a non-empty directory must fail")
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != "f" {
		t.Errorf("leftovers: %v", names)
	}
}

// A writer that lands while our temp file is being synced must win: the
// hash is checked again right before the rename (M1 review).
func TestReplaceIfUnchangedLosesNoWriteDuringSync(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude.json")
	_ = os.WriteFile(p, []byte(`{"a":1}`), 0o600)
	h, _ := HashFile(p)
	syncFile = func(f *os.File) error {
		_ = os.WriteFile(p, []byte(`{"oauth":"NEW"}`), 0o600)
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = (*os.File).Sync })
	err := ReplaceIfUnchanged(p, []byte(`{"a":1,"x":2}`), 0o600, h)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err %v, want a conflict", err)
	}
	if got := read(t, p); got != `{"oauth":"NEW"}` {
		t.Errorf("the concurrent write was lost: %s", got)
	}
	if left := entries(t, filepath.Dir(p)); len(left) != 1 {
		t.Errorf("temp file left behind: %v", left)
	}
}
