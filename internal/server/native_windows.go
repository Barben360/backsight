//go:build windows

package server

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Barben360/backsight/internal/fsutil"
)

// userPg0Home is %USERPROFILE%\.pg0 of this process's user, from its
// token: pg0 ignores the USERPROFILE variable (decision 0080).
func userPg0Home() (string, error) {
	dir, err := windows.GetCurrentProcessToken().GetUserProfileDirectory()
	if err != nil {
		return "", err
	}
	return winJoin(dir, ".pg0"), nil
}

// crossVolume: a rename failed because its two paths are on different
// volumes.
func crossVolume(err error) bool { return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) }

// winProcess is a Toolhelp snapshot's process.
type winProcess struct {
	pid, parent uint32
	exe         string
}

// processes is a Toolhelp snapshot of every process (no CIM or WMI, E34
// ruling 7: both are denied to a standard user over SSH).
func processes() ([]winProcess, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var out []winProcess
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		out = append(out, winProcess{pid: pe.ProcessID, parent: pe.ParentProcessID, exe: windows.UTF16ToString(pe.ExeFile[:])})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return out, nil
}

// imageOf is pid's program, its full Win32 path; "" when the process is
// gone or not this user's to query.
func imageOf(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

// pgRoot is <Root>\pg\ in its long form (a root under an 8.3 profile name,
// RUNNER~1, matches the images' long paths), for a case-blind prefix test.
func (n Native) pgRoot() string {
	root := n.join(n.Root(), "pg")
	if long, err := fsutil.LongPathName(root); err == nil {
		root = long
	}
	return strings.ToLower(strings.TrimRight(root, `\`) + `\`)
}

// underPG: image is a program of <Root>\pg\. The image is the path the
// process was started by, which can hold 8.3 names (RUNNER~1): its long
// form is compared, as root's.
func (n Native) underPG(image, root string) bool {
	if image == "" {
		return false
	}
	if long, err := fsutil.LongPathName(image); err == nil {
		image = long
	}
	return strings.HasPrefix(strings.ToLower(image), root)
}

// straysWindows are requirement 3's processes on Windows (decision 0080):
// every postgres.exe whose image is under <Root>\pg\, which the
// supervisor's Job Object does not hold, and whose parent is not one of
// them (a backend: its postmaster is the stray). postmaster.pid's pid
// comes first, the others by pid.
func (n Native) straysWindows() ([]Stray, error) {
	procs, err := processes()
	if err != nil {
		return nil, err
	}
	root := n.pgRoot()
	ours := map[uint32]uint32{} // pid → parent
	for _, p := range procs {
		if strings.EqualFold(p.exe, "postgres.exe") && n.underPG(imageOf(p.pid), root) {
			ours[p.pid] = p.parent
		}
	}
	h := n.host()
	var out []Stray
	for pid, parent := range ours {
		if _, backend := ours[parent]; backend || h.Owns(int(pid)) {
			continue
		}
		out = append(out, Stray{PID: int(pid)})
	}
	first := n.pidFileOwner()
	slices.SortFunc(out, func(a, b Stray) int {
		switch {
		case strconv.Itoa(a.PID) == first:
			return -1
		case strconv.Itoa(b.PID) == first:
			return 1
		}
		return a.PID - b.PID
	})
	return out, nil
}

// strayRunning: pid still runs a postgres of <Root>\pg\ (StopStrays'
// wait).
func (n Native) strayRunning(pid int) bool { return n.underPG(imageOf(uint32(pid)), n.pgRoot()) }
