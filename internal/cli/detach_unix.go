//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetachedTo starts `backsight <args>` in its own session, its output
// going to out (nil: discarded), and does not wait for it.
func startDetachedTo(out *os.File, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
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
