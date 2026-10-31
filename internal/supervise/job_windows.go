//go:build windows

package supervise

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The server's per-user named objects (E34 ruling 1): Global\ names, so a
// supervisor started detached from SSH (session 0) and one started by the
// task (the console's session) see each other, carrying the user's SID so
// two users' projects never meet.

// userSID is this process's user, as an SDDL string.
func userSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// objectName is backsight-<project>-<user SID>.
func objectName(project string) (string, error) {
	sid, err := userSID()
	if err != nil {
		return "", err
	}
	return "backsight-" + project + "-" + sid, nil
}

// userOnly is a security descriptor granting the user alone everything,
// inheriting nothing: D:P(A;;GA;;;<user SID>) (E34 W7).
func userOnly() (*windows.SecurityAttributes, string, error) {
	sid, err := userSID()
	if err != nil {
		return nil, "", err
	}
	sddl := "D:P(A;;GA;;;" + sid + ")"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, "", err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, sddl, nil
}

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procIsProcessInJob = kernel32.NewProc("IsProcessInJob")
)

// inJob is IsProcessInJob, which x/sys lacks (E34 W3's spike).
func inJob(process, job windows.Handle) (bool, error) {
	var in int32
	if r, _, e := procIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&in))); r == 0 {
		return false, e
	}
	return in != 0, nil
}

// joinJob puts this process in the project's Job Object, kill-on-close
// and breakaway-ok, so every child it starts dies with it (E29
// requirement 3). Its handle is never closed: the last handle's close
// kills the job's processes, this one included, which only its exit may
// do.
func joinJob(project string) error {
	name, err := objectName(project)
	if err != nil {
		return err
	}
	sa, _, err := userOnly()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(`Global\` + name)
	if err != nil {
		return err
	}
	h, err := windows.CreateJobObject(sa, p)
	if err != nil {
		return fmt.Errorf("creating the job object Global\\%s: %w", name, err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("setting the job object's limits: %w", err)
	}
	if in, err := inJob(windows.CurrentProcess(), h); err == nil && in {
		return nil // already in it (a test running a second Run of a project)
	}
	if err := windows.AssignProcessToJobObject(h, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("joining the job object: %w", err)
	}
	return nil
}

// interrupt sends CTRL_BREAK to the group p leads (childAttr gives every
// child its own); without a console it fails, and the caller kills.
func interrupt(p *os.Process) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.Pid))
}

// kill ends p.
func kill(p *os.Process) error {
	err := p.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
