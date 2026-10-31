//go:build windows

package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// A scanner or an editor holds the lock file for a moment: releasing and
// breaking still remove it.
func TestLockRemovalWaitsForAHandleThatHoldsTheFile(t *testing.T) {
	old := fsutil.RetryFor
	fsutil.RetryFor = 3 * time.Second
	t.Cleanup(func() { fsutil.RetryFor = old })
	p := filepath.Join(t.TempDir(), "locks", "x.lock")
	l, err := TryAcquire(p, "test")
	if err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(p) // plain open: no FILE_SHARE_DELETE
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(400 * time.Millisecond); _ = r.Close() }()
	if err := l.Release(); err != nil {
		t.Fatalf("Release while the file is held: %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("the lock file is still there")
	}
}
