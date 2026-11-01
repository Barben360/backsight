//go:build !windows

package supervise

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// joinJob: no Job Object here; the tests' children are in process groups
// of their own, which stop and kill reach.
func joinJob(string) error { return nil }

// InJob: no Job Object here (the Linux host asks systemd's cgroups).
func InJob(string, int) (bool, error) { return false, nil }

// InNamedJob: no Job Object here.
func InNamedJob(string, string, int) (bool, error) { return false, nil }

// tree is a child's process group (childAttr makes it its leader).
type tree struct{}

func ownTree(*os.Process) (*tree, error) { return &tree{}, nil }

// resumeChild: children start running here.
func resumeChild(*os.Process) error { return nil }

// kill sends SIGKILL to the group p leads.
func (*tree) kill(p *os.Process) error { return signalGroup(p, syscall.SIGKILL) }

func (*tree) close() {}

// interrupt sends SIGTERM to the group p leads.
func interrupt(p *os.Process) error { return signalGroup(p, syscall.SIGTERM) }

func signalGroup(p *os.Process, sig syscall.Signal) error {
	err := syscall.Kill(-p.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// Job: no Job Object here (Linux runs the native runtime's transient jobs
// in systemd units); Start starts the process alone.
type Job struct{}

func NewJob() (*Job, error)                    { return &Job{}, nil }
func NewNamedJob(string, string) (*Job, error) { return &Job{}, nil }
func (*Job) Start(cmd *exec.Cmd) error         { return cmd.Start() }
func (*Job) Terminate() error                  { return nil }
func (*Job) Close() error                      { return nil }
