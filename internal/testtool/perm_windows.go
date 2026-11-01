//go:build windows

package testtool

import (
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Barben360/backsight/internal/fsutil"
)

// DenyWrite adds a deny entry (write data, append data: create a file or a
// subdirectory in a directory) for this user to path's ACL, until the
// returned function or the end of the test removes it. fsutil's retry on
// access denied is cut short meanwhile.
func DenyWrite(t testing.TB, path string) (restore func()) {
	t.Helper()
	return deny(t, path, "(WD,AD)")
}

// DenyRead adds a deny entry (read data) for this user to path's ACL.
func DenyRead(t testing.TB, path string) (restore func()) {
	t.Helper()
	return deny(t, path, "(RD)")
}

// ShareWithOthers grants the Users group read access to path (the owner
// can change the ACL).
func ShareWithOthers(t testing.TB, path string) {
	t.Helper()
	if out, err := exec.Command("icacls", path, "/grant", "*S-1-5-32-545:(R)").CombinedOutput(); err != nil {
		t.Fatalf("icacls /grant: %v\n%s", err, out)
	}
}

func deny(t testing.TB, path, rights string) func() {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	who := "*" + u.User.Sid.String()
	if out, err := exec.Command("icacls", path, "/deny", who+":"+rights).CombinedOutput(); err != nil {
		t.Fatalf("icacls /deny: %v\n%s", err, out)
	}
	old := fsutil.RetryFor
	fsutil.RetryFor = 50 * time.Millisecond
	done := false
	restore := func() {
		if done {
			return
		}
		done = true
		fsutil.RetryFor = old
		_ = exec.Command("icacls", path, "/remove:d", who).Run()
	}
	t.Cleanup(restore)
	return restore
}
