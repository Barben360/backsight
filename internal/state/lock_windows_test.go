//go:build windows

package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"golang.org/x/sys/windows"
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

// A lock file being deleted (its owner released it while a reader still
// has it open) is held for a moment, not an error: Windows refuses to
// create the name with access denied until the last handle closes, and a
// hook that gave up there lost its session update.
func TestALockBeingDeletedIsHeldNotAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, err := TryAcquire(p, "owner")
	if err != nil {
		t.Fatal(err)
	}
	name, _ := windows.UTF16PtrFromString(p)
	reader, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(reader) }()
	// The classic delete: the name stays until the reader closes.
	del, err := windows.CreateFile(name, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	one := byte(1)
	err = windows.SetFileInformationByHandle(del, windows.FileDispositionInfo, &one, 1)
	_ = windows.CloseHandle(del)
	if err != nil {
		t.Fatal(err)
	}
	_ = l // released by the delete above
	_, err = TryAcquire(p, "next")
	var held *HeldError
	if !errors.As(err, &held) || held.Stale {
		t.Fatalf("a lock being deleted: %v, want held and not stale", err)
	}
	_ = windows.CloseHandle(reader)
	reader = windows.InvalidHandle
	next, err := TryAcquire(p, "next")
	if err != nil {
		t.Fatalf("once deleted the lock is free: %v", err)
	}
	_ = next.Release()
}

// A directory nobody may create in is a genuine refusal, not a held lock.
func TestARefusedLockIsAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Deny everyone FILE_ADD_FILE (0x2) in the directory.
	sd, err := windows.SecurityDescriptorFromString("D:P(D;;0x2;;;WD)(A;;GA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sd, _ := windows.SecurityDescriptorFromString("D:P(A;;GA;;;WD)")
		dacl, _, _ := sd.DACL()
		_ = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	})
	_, err = TryAcquire(filepath.Join(dir, "x.lock"), "a")
	var held *HeldError
	if err == nil || errors.As(err, &held) {
		t.Fatalf("a refused create: %v, want a plain error", err)
	}
}
