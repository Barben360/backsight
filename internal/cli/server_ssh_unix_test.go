//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/token"
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
		if _, err := exec.LookPath(s[0]); err != nil {
			continue
		}
		if !runsPathTar(s, bin) {
			t.Logf("%s runs its own tar, not PATH's: its run is left out", s[0])
			continue
		}
		shells = append(shells, s)
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

// runsPathTar reports whether sh runs the tar found on PATH: a busybox
// built to prefer its own applets runs its tar whatever PATH says, so the
// stand-in above could never mark that the copy started.
func runsPathTar(sh []string, bin string) bool {
	mark := filepath.Join(bin, "probe-started")
	defer func() { _ = os.Remove(mark) }()
	cmd := exec.Command(sh[0], append(sh[1:], "-c", "tar --version >/dev/null 2>&1; :")...)
	cmd.Env = append(os.Environ(), "TAR_STARTED="+mark, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if cmd.Run() != nil {
		return false
	}
	_, err := os.Stat(mark)
	return err == nil
}

// loginShellSSH is an sshCLI standing for sshd: it runs the last argv word,
// the remote command line, as `shell -c <line>` of the user's login shell,
// with the given environment and the stdin ssh was given.
type loginShellSSH struct{ shell, env []string }

func (l loginShellSSH) Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, l.shell[0], append(append([]string{}, l.shell[1:]...), "-c", args[len(args)-1])...)
	cmd.WaitDelay = 5 * time.Second // a stray background child cannot hold the pipes open
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = l.env, stdin, stdout, stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	}
	return -1, err
}

// The remote steps (probe, install, token handback, the shim's own words)
// give the same answers whichever login shell sshd hands them to. A shell
// absent from PATH skips its subtest, unless BACKSIGHT_REQUIRE_SHELLS (a
// comma list) names it: then it fails.
func TestRemoteStepsUnderEveryLoginShell(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the host is a Linux one")
	}
	required := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("BACKSIGHT_REQUIRE_SHELLS"), ",") {
		required[strings.TrimSpace(s)] = true
	}
	for _, shell := range []string{"sh", "dash", "bash", "zsh", "fish", "tcsh", "csh"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				if required[shell] {
					t.Fatalf("%s is required (BACKSIGHT_REQUIRE_SHELLS) and not on PATH", shell)
				}
				t.Skipf("no %s", shell)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			home, tmp := t.TempDir(), t.TempDir()
			var env []string
			for _, e := range os.Environ() {
				k, _, _ := strings.Cut(e, "=")
				switch k {
				case "HOME", "TMPDIR", "SSH_CONNECTION", "BACKSIGHT_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
					"BACKSIGHT_INSTALL_PUBKEY", "BACKSIGHT_RELEASE_DIR", "BACKSIGHT_RELEASE_URL":
					continue
				}
				env = append(env, e)
			}
			env = append(env, "HOME="+home, "TMPDIR="+tmp, "SSH_CONNECTION=100.101.1.9 50022 100.101.1.2 22")
			withSSH(t, loginShellSSH{shell: []string{shell}, env: env})

			f, err := probeHost(ctx, "u@h")
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			wantShim := filepath.Join(home, ".local/share/backsight/bin/current")
			if f.Addr != "100.101.1.2" || f.OS != "Linux" || f.Shim != wantShim {
				t.Errorf("probe = %+v; want Addr 100.101.1.2, OS Linux, Shim %s", f, wantShim)
			}

			file := filepath.Join(home, "handback-token")
			if err := os.WriteFile(file, []byte(token.Prefix+"x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			remote, err := remoteScript("handback", handbackScript, file)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if _, err := sshRun(ctx, "u@h", remote, nil, &out, nil); err != nil {
				t.Fatalf("handback: %v", err)
			}
			if out.String() != "BACKSIGHT-TOKEN "+token.Prefix+"x\n" {
				t.Errorf("handback printed %q", out.String())
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Errorf("the token file is still there: %v", err)
			}

			echo := filepath.Join(home, "echo-args")
			if err := os.WriteFile(echo, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			words := []string{"setup", "--answer", "provider=mock", "--json", "a b$HOME"}
			remote, err = remoteShim(echo, words...)
			if err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if _, err := sshRun(ctx, "u@h", remote, nil, &out, nil); err != nil {
				t.Fatalf("shim: %v", err)
			}
			if got := out.String(); got != strings.Join(words, "\n")+"\n" {
				t.Errorf("the shim received %q, want the words verbatim", got)
			}

			if !hasVerifier() {
				if required[shell] {
					t.Fatal("neither minisign nor OpenSSL 3 is installed, and BACKSIGHT_REQUIRE_SHELLS names this shell: the install step would check nothing")
				}
				t.Skip("neither minisign nor OpenSSL 3 is installed")
			}
			src := t.TempDir()
			signedRelease(t, src, "9.9.9", trustTestKey(t), relOpts{platform: "linux_" + runtime.GOARCH})
			work := t.TempDir()
			archive, _, err := stageRelease(ctx, release.Dir{Path: src}, "9.9.9", runtime.GOARCH, work)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := installOnHost(ctx, "u@h", work, archive); err != nil {
				t.Fatalf("install: %v", err)
			}
			want := filepath.Join(home, ".local/share/backsight/bin/9.9.9/backsight")
			if got, err := os.Readlink(wantShim); err != nil || got != want {
				t.Errorf("shim -> %q (%v), want %s", got, err, want)
			}
			if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
				t.Errorf("TMPDIR keeps %d entries after the install", len(ents))
			}
		})
	}
}
