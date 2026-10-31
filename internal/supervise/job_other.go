//go:build !windows

package supervise

import (
	"errors"
	"os"
	"syscall"
)

// joinJob: no Job Object here; the tests' children are in process groups
// of their own, which stop and kill reach.
func joinJob(string) error { return nil }

// InJob: no Job Object here (the Linux host asks systemd's cgroups).
func InJob(string, int) (bool, error) { return false, nil }

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
