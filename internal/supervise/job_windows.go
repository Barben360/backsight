//go:build windows

package supervise

import (
	"errors"
	"fmt"
	"os"
	"sync"
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

// tree is a child's own unnamed kill-on-close Job Object, nested in the
// supervisor's: kill ends the child and what it started (the venv
// launcher's python, postgres' backends), and closing it once the child
// exited ends whatever it left. A zero job (the assignment failed) leaves
// the child alone to kill. kill and close may race (the loop kills while
// the child's Wait closes): a kill after the close does nothing.
type tree struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

// ownTree puts the child, started suspended (childAttr), in a job of its
// own before resumeChild lets it run: nothing it starts can escape the
// job. On failure the child has no tree and the error says why.
func ownTree(p *os.Process) (*tree, error) {
	job, err := newJob(p.Pid)
	return &tree{job: job}, err
}

func newJob(pid int) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("creating the child's job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("setting the child's job object: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("opening the child: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("nesting the child's job object in the supervisor's: %w", err)
	}
	return job, nil
}

// resumeChild lets the suspended child run; tests make it fail.
var resumeChild = func(p *os.Process) error { return resume(uint32(p.Pid)) }

// resume resumes the threads of the suspended process pid (Go's exec
// closes the main thread's handle).
func resume(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if err != nil {
			return err
		}
		_, rerr := windows.ResumeThread(th)
		_ = windows.CloseHandle(th)
		if rerr != nil {
			return rerr
		}
		resumed++
	}
	if resumed == 0 {
		return fmt.Errorf("no thread of process %d found", pid)
	}
	return nil
}

// kill ends the child's job, else the child alone; after close, nothing
// (the child exited, its job is gone).
func (t *tree) kill(p *os.Process) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if t.job != 0 {
		return windows.TerminateJobObject(t.job, 1)
	}
	err := p.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

// close closes the job: what the child left running dies with it.
func (t *tree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed && t.job != 0 {
		_ = windows.CloseHandle(t.job)
	}
	t.closed = true
}
