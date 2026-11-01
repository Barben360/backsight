//go:build windows

package state

import (
	"time"

	"golang.org/x/sys/windows"
)

// On Windows a lock's owner is told by its process's creation time
// (GetProcessTimes): Windows reuses PIDs within minutes, so a lock left by
// a killed process (a tick killed at logoff, a hook child killed at its
// deadline) would otherwise stay held by whatever runs under its PID now.
// StartTicks holds the creation time, a FILETIME (100 ns since 1601),
// compared exactly; a creation time names one process across boots, so no
// boot id goes with it.
//
// A process this user may not open (another user's: OpenProcess answers
// ERROR_ACCESS_DENIED, which processAlive counts as alive) tells no
// creation time: ownerAlive then keeps the lock held on its PID alone.
// That is the safe side: breaking a lock under a live owner lets two
// writers in (a torn outbox, two maintenances), while a lock held too long
// costs a delay that ends with that process, and doctor names it.

// startTicks is process pid's creation time as a FILETIME count; ok is
// false when it cannot be told (the process is gone, or not this user's to
// query).
func startTicks(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return ProcessCreated(h)
}

// ProcessCreated is the creation time of the process h (opened with at
// least PROCESS_QUERY_LIMITED_INFORMATION) as a FILETIME count; ok is
// false when GetProcessTimes fails.
func ProcessCreated(h windows.Handle) (int64, bool) {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0, false
	}
	return int64(created.HighDateTime)<<32 | int64(created.LowDateTime), true
}

// processStart is when process pid was created; ok is false when it
// cannot be told. Only for a lock written without StartTicks (an older
// build): the wall clock may have moved since.
func processStart(pid int) (time.Time, bool) {
	ticks, ok := startTicks(pid)
	if !ok {
		return time.Time{}, false
	}
	ft := windows.Filetime{HighDateTime: uint32(ticks >> 32), LowDateTime: uint32(ticks)}
	return time.Unix(0, ft.Nanoseconds()), true
}

// bootID is empty: a creation time needs no boot to be read against.
func bootID() string { return "" }
