//go:build windows

package paths

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Barben360/backsight/internal/fsutil"
)

// BACKSIGHT_HOME=C:\x gets the privacy %LOCALAPPDATA% has: an existing loose root is
// tightened, a missing one and bin are left alone.
func TestEnsurePrivateOnWindows(t *testing.T) {
	h, err := os.MkdirTemp(`C:\`, "bs-test-paths-")
	if err != nil {
		t.Skipf("cannot create a directory under C:\\: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(h) })
	d, err := Resolve("windows", "", func(k string) string {
		if k == "BACKSIGHT_HOME" {
			return h
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{d.Data, d.State} { // there already, inheriting Users read
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if who, _ := fsutil.OtherReaders(d.State); len(who) == 0 {
		t.Fatal("the fixture is private already: the test proves nothing")
	}
	if err := d.EnsurePrivate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.Config); err == nil {
		t.Error("a missing root was created: creating is the writers' job")
	}
	for _, p := range []string{d.Data, d.State} {
		if who, err := fsutil.OtherReaders(p); err != nil || len(who) != 0 {
			t.Errorf("%s: readers %v, %v", filepath.Base(p), who, err)
		}
	}
	if _, err := os.Stat(d.Bin); err == nil {
		t.Error("bin was created")
	}
	// again: nothing to do, no error
	if err := d.EnsurePrivate(); err != nil {
		t.Fatal(err)
	}
}
