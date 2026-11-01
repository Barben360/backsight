//go:build windows

package fsutil

import (
	"os"
	"os/exec"
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

// Only an entry that lets an account read counts: write-only does not.
func TestOtherReadersIgnoresAWriteOnlyEntry(t *testing.T) {
	root := openRoot(t)
	f := filepath.Join(root, "drop.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MakePrivate(f); err != nil {
		t.Fatal(err)
	}
	grant := func(rights string) {
		if out, err := exec.Command("icacls", f, "/grant", "*S-1-5-32-545:"+rights).CombinedOutput(); err != nil {
			t.Fatalf("icacls: %v\n%s", err, out)
		}
	}
	grant("(WD)")
	if r, err := OtherReaders(f); err != nil || len(r) != 0 {
		t.Errorf("a write-only entry is reported: %v, %v", r, err)
	}
	grant("(RD)")
	if r, err := OtherReaders(f); err != nil || len(r) != 1 {
		t.Errorf("a read entry: %v, %v, want Users", r, err)
	}
}

// The first directory a nested MkdirPrivate creates is the private one, so
// every level below it is.
func TestMkdirPrivateNestedIsPrivateAtEveryLevel(t *testing.T) {
	root := openRoot(t)
	leaf := filepath.Join(root, "state", "feedback", "x")
	if err := MkdirPrivate(leaf); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(root, "state"), filepath.Join(root, "state", "feedback"), leaf} {
		if r, err := OtherReaders(p); err != nil || len(r) != 0 {
			t.Errorf("%s: readers %v, %v", p, r, err)
		}
	}
}

// A secret written where the user chose (a token --out, a keys export)
// is private even in a folder other users can read: you, SYSTEM and
// Administrators only, inheritance off.
func TestWritePrivateInAnOpenFolder(t *testing.T) {
	root := openRoot(t)
	plain := filepath.Join(root, "plain.txt")
	if err := WriteAtomic(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, _ := OtherReaders(plain); len(r) == 0 {
		t.Fatal("a plain write there is private already: the fixture proves nothing")
	}
	for name, write := range map[string]func(string, []byte) error{"WritePrivate": WritePrivate, "WritePrivateNoFollow": WritePrivateNoFollow} {
		f := filepath.Join(root, name+".token")
		if err := os.WriteFile(f, []byte("an older one"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := write(f, []byte("bst1.secret\n")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r, err := OtherReaders(f); err != nil || len(r) != 0 {
			t.Errorf("%s: readable by %v (%v)", name, r, err)
		}
		if !protected(t, f) {
			t.Errorf("%s: the DACL inherits from the folder", name)
		}
		if b, err := os.ReadFile(f); err != nil || string(b) != "bst1.secret\n" {
			t.Errorf("%s: the owner reads %q, %v", name, b, err)
		}
	}
}

// The temp file a private write fills is private from its creation: no
// other user can open it, even empty, and keep a handle that reads the
// secret once written (a DACL change does not revoke an open handle).
func TestPrivateTempIsPrivateAtCreation(t *testing.T) {
	root := openRoot(t)
	f, err := createPrivateTemp(root, ".join.token.tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if fi, err := f.Stat(); err != nil || fi.Size() != 0 {
		t.Fatalf("not a new empty file: %v %v", fi, err)
	}
	if r, err := OtherReaders(f.Name()); err != nil || len(r) != 0 {
		t.Errorf("readable by %v at creation (%v)", r, err)
	}
	if !protected(t, f.Name()) {
		t.Error("its DACL inherits from the folder at creation")
	}
	if !strings.HasPrefix(filepath.Base(f.Name()), ".join.token.tmp-") || filepath.Dir(f.Name()) != root {
		t.Errorf("name %s", f.Name())
	}
	if _, err := f.WriteString("bst1.secret"); err != nil {
		t.Errorf("the owner writes: %v", err)
	}
}
