//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// openRoot makes a directory directly under C:\, which grants the Users
// group read access by inheritance (a temp dir under the profile does not).
func openRoot(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp(`C:\`, "bs-test-fsutil-")
	if err != nil {
		t.Skipf("cannot create a directory under C:\\: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func protected(t *testing.T, path string) bool {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	return c&windows.SE_DACL_PROTECTED != 0
}

func TestMakePrivateLeavesOnlyTheUserSystemAndAdministrators(t *testing.T) {
	root := openRoot(t)
	dir := filepath.Join(root, "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := OtherReaders(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(before, func(s string) bool {
		return strings.HasSuffix(s, `\Users`) || strings.HasSuffix(s, `\Authenticated Users`)
	}) {
		t.Fatalf("before MakePrivate OtherReaders = %v: the fixture shows nothing, the test proves nothing", before)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := MakePrivate(dir); err != nil {
		t.Fatal(err)
	}
	if r, err := OtherReaders(dir); err != nil || len(r) != 0 {
		t.Fatalf("OtherReaders(dir) = %v, %v after MakePrivate", r, err)
	}
	if !protected(t, dir) {
		t.Error("the DACL still inherits from the parent")
	}
	// what was there, and what is created later, inherits the private ACL
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "secrets.toml")
	if err := os.WriteFile(f, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "old.txt"), sub, f} {
		if r, err := OtherReaders(p); err != nil || len(r) != 0 {
			t.Errorf("OtherReaders(%s) = %v, %v", p, r, err)
		}
	}
	// the owner still works
	if b, err := os.ReadFile(f); err != nil || string(b) != "k" {
		t.Errorf("the owner reads %q, %v", b, err)
	}
	if err := os.WriteFile(f, []byte("k2"), 0o600); err != nil {
		t.Errorf("the owner writes: %v", err)
	}
}

func TestMakePrivateOnAFile(t *testing.T) {
	root := openRoot(t)
	f := filepath.Join(root, "server.env")
	if err := os.WriteFile(f, []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, _ := OtherReaders(f); len(r) == 0 {
		t.Fatal("the fixture file is private already")
	}
	if err := MakePrivate(f); err != nil {
		t.Fatal(err)
	}
	if r, err := OtherReaders(f); err != nil || len(r) != 0 {
		t.Errorf("OtherReaders = %v, %v", r, err)
	}
	if !protected(t, f) {
		t.Error("not protected")
	}
	if err := MakePrivate(filepath.Join(root, "absent")); err == nil {
		t.Error("MakePrivate of an absent path succeeded")
	}
}

// WriteAtomic's temp file takes the directory's inherited ACL, so a secret
// written into a private directory is private from its first byte.
func TestAtomicWriteInAPrivateDirIsPrivate(t *testing.T) {
	root := openRoot(t)
	if err := MakePrivate(root); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "secrets.toml")
	if err := WriteAtomic(f, []byte("api_key = \"k\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := OtherReaders(f); err != nil || len(r) != 0 {
		t.Errorf("OtherReaders = %v, %v", r, err)
	}
}

func TestMkdirPrivateCreatesPrivateAndLeavesAnExistingOneAlone(t *testing.T) {
	root := openRoot(t)
	d := filepath.Join(root, "config")
	if err := MkdirPrivate(d); err != nil {
		t.Fatal(err)
	}
	if r, err := OtherReaders(d); err != nil || len(r) != 0 {
		t.Errorf("a created directory: readers %v, %v", r, err)
	}
	// an existing directory is not touched
	e := filepath.Join(root, "existing")
	if err := os.Mkdir(e, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MkdirPrivate(e); err != nil {
		t.Fatal(err)
	}
	if r, _ := OtherReaders(e); len(r) == 0 {
		t.Error("an existing directory was changed")
	}
}
