//go:build !windows

// Package detach starts a program that outlives its caller: a hook's
// background flush, a server start, the native server's supervisor.
package detach

import (
	"os"
	"os/exec"
	"syscall"
)

// Start starts exe with args in its own session, its output going to out
// (nil: discarded), and does not wait for it.
func Start(out *os.File, exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	if out != nil {
		cmd.Stdout, cmd.Stderr = out, out
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // its own session: not killed with the hook
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
