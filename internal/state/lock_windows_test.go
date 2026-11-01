//go:build windows

package state

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

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

// The delete that refused the create finishes before the name is checked:
// the create is tried again and the lock is taken (CI's SaveWithin failed
// with "Access is denied" here).
func TestALockDeletedWhileRefusedIsTakenAgain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	if _, err := TryAcquire(p, "owner"); err != nil {
		t.Fatal(err)
	}
	name, _ := windows.UTF16PtrFromString(p)
	reader, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
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
	// the last handle closes right after the refusal: the name is gone
	old := refusedHook
	refusedHook = func() { _ = windows.CloseHandle(reader); refusedHook = old }
	t.Cleanup(func() { refusedHook = old })
	l, err := TryAcquire(p, "next")
	if err != nil {
		t.Fatalf("the delete finished, the lock must be taken: %v", err)
	}
	if l.Owner().Command != "next" {
		t.Errorf("owner %+v", l.Owner())
	}
	_ = l.Release()
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

// liveChild is a process that runs until the test ends (cmd.exe waiting
// on a pipe nobody writes): someone else's PID, alive.
func liveChild(t *testing.T) int {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/c", "pause")
	cmd.Stdin = r
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = w.Close() })
	return cmd.Process.Pid
}

// Windows reuses PIDs within minutes: a lock left by a killed process (a
// tick killed at logoff, a hook child killed at its deadline) names a PID
// another process may run under by now. A live process created after the
// lock's Since got that PID, and the lock is stale; the same process with
// a Since after its creation, and the genuine owner, keep it held. A lock
// that carries its owner's creation time (StartTicks) is compared with it
// exactly, whatever Since says.
func TestALockWhosePIDWindowsReusedIsStale(t *testing.T) {
	before := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	pid := liveChild(t)
	p := filepath.Join(t.TempDir(), "outbox.lock")
	writeOwner(t, p, Owner{PID: pid, Command: "jobs tick", Since: before})
	if h := Inspect(p); h == nil || !h.Stale {
		t.Errorf("a live PID whose process was created after the lock was taken: %+v, want stale", h)
	}
	if MaintenanceHeld(p) {
		t.Error("a reused PID holds the lock")
	}
	writeOwner(t, p, Owner{PID: pid, Command: "jobs tick", Since: time.Now().UTC().Truncate(time.Second)})
	if h := Inspect(p); h == nil || h.Stale {
		t.Errorf("the PID's process created before the lock was taken: %+v, want held", h)
	}
	created, ok := startTicks(pid)
	if !ok || created == 0 {
		t.Fatalf("no creation time for the live child %d", pid)
	}
	for name, c := range map[string]struct {
		o     Owner
		stale bool
	}{
		"its own creation time, Since far before": {Owner{PID: pid, Command: "x", Since: before, StartTicks: created}, false},
		"another creation time, Since after":      {Owner{PID: pid, Command: "x", Since: time.Now().UTC(), StartTicks: created + 1}, true},
	} {
		writeOwner(t, p, c.o)
		if h := Inspect(p); h == nil || h.Stale != c.stale {
			t.Errorf("%s: %+v, want stale %v", name, h, c.stale)
		}
	}
	own := filepath.Join(t.TempDir(), "own.lock")
	l, err := TryAcquire(own, "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if l.Owner().StartTicks == 0 {
		t.Errorf("the lock does not record its owner's creation time: %+v", l.Owner())
	}
	if h := Inspect(own); h == nil || h.Stale {
		t.Errorf("the genuine owner's lock: %+v, want held", h)
	}
}

// A process this user may not open (another user's: ACCESS_DENIED) tells
// no creation time: a lock naming its PID stays held whatever its Since,
// never broken under a live owner (decision: the safe side).
func TestALockOnAProcessThatRefusesAccessStaysHeld(t *testing.T) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		h, oerr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID)
		if oerr == nil {
			_ = windows.CloseHandle(h)
			continue
		}
		if !errors.Is(oerr, windows.ERROR_ACCESS_DENIED) {
			continue
		}
		p := filepath.Join(t.TempDir(), "x.lock")
		writeOwner(t, p, Owner{PID: int(e.ProcessID), Command: "x", Since: time.Now().UTC().Add(-24 * time.Hour)})
		if h := Inspect(p); h == nil || h.Stale {
			t.Errorf("pid %d (%s) refuses access: %+v, want held", e.ProcessID, windows.UTF16ToString(e.ExeFile[:]), h)
		}
		return
	}
	t.Skip("every process here can be opened (an elevated runner): no access-denied process to check")
}
