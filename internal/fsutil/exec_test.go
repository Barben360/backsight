package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func put(t *testing.T, dir, name string, data string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// What can be run is decided by the OS: an execute bit on Unix, the .exe
// name on Windows; an empty file and a directory never are.
func TestExecutableByOS(t *testing.T) {
	d := t.TempDir()
	if runtime.GOOS == "windows" {
		for name, want := range map[string]bool{"x.exe": true, "X.EXE": true, "x": false, "x.cmd": false, "x.exe.txt": false} {
			if got := Executable(put(t, d, name, "MZ", 0o644)); got != want {
				t.Errorf("Executable(%s) = %v, want %v", name, got, want)
			}
		}
		if Executable(put(t, d, "empty.exe", "", 0o644)) {
			t.Error("an empty .exe is executable")
		}
	} else {
		if !Executable(put(t, d, "run", "#!/bin/sh\n", 0o755)) {
			t.Error("0755 is not executable")
		}
		if !Executable(put(t, d, "user", "#!/bin/sh\n", 0o700)) {
			t.Error("0700 is not executable")
		}
		if Executable(put(t, d, "plain", "#!/bin/sh\n", 0o644)) {
			t.Error("0644 is executable")
		}
		if Executable(put(t, d, "empty", "", 0o755)) {
			t.Error("an empty file is executable")
		}
		if err := os.Symlink(filepath.Join(d, "gone"), filepath.Join(d, "dangling")); err != nil {
			t.Fatal(err)
		}
		if Executable(filepath.Join(d, "dangling")) {
			t.Error("a dangling link is executable")
		}
	}
	if Executable(d) {
		t.Error("a directory is executable")
	}
	if Executable(filepath.Join(d, "absent.exe")) {
		t.Error("an absent file is executable")
	}
}

// On Unix a private path has no group or other read bit; MakePrivate sets
// 0700 or 0600 whatever it was.
func TestMakePrivateUnixModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes are Unix's; the Windows test checks the DACL")
	}
	d := t.TempDir()
	dir := filepath.Join(d, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := put(t, d, "f", "x", 0o644)
	if r, err := OtherReaders(dir); err != nil || len(r) != 2 || r[0] != "group" || r[1] != "others" {
		t.Fatalf("OtherReaders(0755) = %v, %v", r, err)
	}
	for _, p := range []string{dir, f} {
		if err := MakePrivate(p); err != nil {
			t.Fatal(err)
		}
		if r, err := OtherReaders(p); err != nil || len(r) != 0 {
			t.Errorf("OtherReaders(%s) = %v, %v after MakePrivate", p, r, err)
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", st.Mode().Perm())
	}
	if err := MakePrivate(filepath.Join(d, "absent")); err == nil {
		t.Error("MakePrivate of an absent path succeeded")
	}
}
