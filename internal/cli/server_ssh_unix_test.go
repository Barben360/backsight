//go:build unix

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// An ssh killed mid-copy hangs up the host's session: the install script
// removes its temp directory on a signal too (dash runs no EXIT trap on
// a signal's death), whichever sh the host has.
func TestInstallScriptCleansUpWhenKilled(t *testing.T) {
	realTar, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("no tar")
	}
	// tar stands in for the copy under way: it marks that it started (the
	// directory made and named, the traps set before), then reads stdin.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tar"), []byte("#!/bin/sh\n: > \"$TAR_STARTED\"\nexec "+shellQuote(realTar)+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shells := [][]string{{"sh"}}
	for _, s := range [][]string{{"dash"}, {"busybox", "sh"}, {"zsh", "--emulate", "sh"}} {
		if _, err := exec.LookPath(s[0]); err == nil {
			shells = append(shells, s)
		}
	}
	for _, sh := range shells {
		for name, sig := range map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "TERM": syscall.SIGTERM} {
			t.Run(sh[0]+"/"+name, func(t *testing.T) {
				tmp, started := t.TempDir(), filepath.Join(t.TempDir(), "started")
				pr, pw, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = pw.Close() }()
				cmd := exec.Command(sh[0], append(sh[1:], "-c", installScript, "backsight-install")...)
				cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "TAR_STARTED="+started, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				cmd.Stdin = pr
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				_ = pr.Close()
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				kill := func(why string) {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					t.Fatalf("%s (killed: %v)", why, <-done)
				}
				for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
					if _, err := os.Stat(started); err == nil {
						break
					}
					if time.Now().After(deadline) {
						kill("tar never started")
					}
				}
				if ents, _ := os.ReadDir(tmp); len(ents) != 1 {
					kill("tar started without the script's temp directory")
				}
				_ = syscall.Kill(-cmd.Process.Pid, sig)
				select {
				case err := <-done:
					if err == nil {
						t.Error("the script exited 0 after a signal")
					}
				case <-time.After(10 * time.Second):
					kill("the script outlived SIG" + name + " by 10s")
				}
				if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
					t.Errorf("the temp directory survived SIG%s", name)
				}
			})
		}
	}
}
