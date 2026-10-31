//go:build !windows

package supervise

import "syscall"

// childAttr starts a child as the leader of a process group of its own;
// no token to restrict here.
func childAttr(bool) (*syscall.SysProcAttr, func(), error) {
	return &syscall.SysProcAttr{Setpgid: true}, func() {}, nil
}

// toolAttr is a short tool's: nothing special.
func toolAttr() *syscall.SysProcAttr { return nil }
