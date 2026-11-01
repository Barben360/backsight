//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/output"
)

// openFolder is a directory directly under C:\, which grants the Users
// group read access by inheritance (a temp directory in the profile does
// not, and would prove nothing).
func openFolder(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp(`C:\`, "bs-test-token-")
	if err != nil {
		t.Skipf("cannot create a directory under C:\\: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	if r, _ := fsutil.OtherReaders(d); !slices.ContainsFunc(r, func(s string) bool {
		return strings.HasSuffix(s, `\Users`) || strings.HasSuffix(s, `\Authenticated Users`) || s == "Everyone"
	}) {
		t.Fatalf("%s is readable by %v only: the fixture proves nothing", d, r)
	}
	return d
}

// daclProtected reports that path's DACL does not inherit from its folder.
func daclProtected(t *testing.T, path string) bool {
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

// The connection token holds the API key: written with --out into a
// folder other users can read, it is still readable by you, SYSTEM and
// Administrators only, its DACL not inherited.
func TestTokenFileIsPrivateInAnOpenFolder(t *testing.T) {
	tokenHost(t)
	f := filepath.Join(openFolder(t), "join.token")
	if code, out, errOut := run(t, "server", "token", "--out", f); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if r, err := fsutil.OtherReaders(f); err != nil || len(r) != 0 {
		t.Errorf("the token file is readable by %v (%v)", r, err)
	}
	if !daclProtected(t, f) {
		t.Error("the token file's DACL inherits from its folder")
	}
}

// The flags that write a secret where the user chose say how the file is
// kept private on Windows, never a mode bit that does not apply there.
func TestSecretOutFlagsSayHowTheFileIsPrivate(t *testing.T) {
	for _, args := range [][]string{{"server", "token", "--help"}, {"server", "expose", "--help"}, {"server", "runtime", "fingerprint", "--help"}} {
		_, out, _ := run(t, args...)
		if !strings.Contains(out, "(private: you, SYSTEM and Administrators only)") || strings.Contains(out, "0600") {
			t.Errorf("%v:\n%s", args, out)
		}
	}
}
