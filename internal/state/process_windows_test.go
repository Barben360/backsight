//go:build windows

package state

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A process that has exited is dead even while a handle to it keeps its
// object, and its PID, alive.
func TestAnExitedProcessIsDeadWhileAHandleHoldsIt(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if processAlive(cmd.Process.Pid) {
		t.Error("an exited process held by a handle is reported alive")
	}
}

func TestALiveProcessIsAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("this test process is reported dead")
	}
	if processAlive(0) || processAlive(-1) {
		t.Error("pid 0 or -1 reported alive")
	}
}

// Another user's process cannot be opened: it is alive, never stale.
func TestAProcessThatRefusesAccessIsAlive(t *testing.T) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		h, oerr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, e.ProcessID)
		if oerr == nil {
			_ = windows.CloseHandle(h)
			continue
		}
		if errors.Is(oerr, windows.ERROR_ACCESS_DENIED) {
			if !processAlive(int(e.ProcessID)) {
				t.Errorf("pid %d (%s) refuses access and is reported dead", e.ProcessID, windows.UTF16ToString(e.ExeFile[:]))
			}
			return
		}
	}
	t.Skip("every process here can be opened (an elevated runner): no access-denied process to check")
}
