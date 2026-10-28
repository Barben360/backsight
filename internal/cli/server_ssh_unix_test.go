//go:build unix

package cli

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// An ssh killed mid-copy hangs up the host's session: the install script
// removes its temp directory on a signal too (dash runs no EXIT trap on
// a signal's death), whichever sh the host has.
func TestInstallScriptCleansUpWhenKilled(t *testing.T) {
	shells := [][]string{{"sh"}}
	for _, s := range [][]string{{"dash"}, {"busybox", "sh"}, {"zsh", "--emulate", "sh"}} {
		if _, err := exec.LookPath(s[0]); err == nil {
			shells = append(shells, s)
		}
	}
	for _, sh := range shells {
		for name, sig := range map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "TERM": syscall.SIGTERM} {
			t.Run(sh[0]+"/"+name, func(t *testing.T) {
				tmp := t.TempDir()
				pr, pw, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = pw.Close() }()
				cmd := exec.Command(sh[0], append(sh[1:], "-c", installScript, "backsight-install")...)
				cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
				cmd.Stdin = pr
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				_ = pr.Close()
				// tar waits on stdin once mktemp made the directory.
				for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
					if ents, _ := os.ReadDir(tmp); len(ents) > 0 {
						break
					}
					if time.Now().After(deadline) {
						_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
						t.Fatal("the script made no temp directory")
					}
				}
				time.Sleep(50 * time.Millisecond)
				_ = syscall.Kill(-cmd.Process.Pid, sig)
				if err := cmd.Wait(); err == nil {
					t.Error("the script exited 0 after a signal")
				}
				if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
					t.Errorf("the temp directory survived SIG%s", name)
				}
			})
		}
	}
}
