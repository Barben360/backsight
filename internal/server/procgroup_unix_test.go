//go:build !windows

package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// During a restore docker runs in its own process group, so a terminal's
// Ctrl-C does not kill a rollback's docker compose; by default it does not.
func TestDockerRunnerOwnGroup(t *testing.T) {
	if c := (DockerRunner{OwnGroup: true}).command(context.Background(), "version"); c.SysProcAttr == nil || !c.SysProcAttr.Setpgid {
		t.Errorf("own group: %+v", c.SysProcAttr)
	}
	if c := (DockerRunner{}).command(context.Background(), "version"); c.SysProcAttr != nil && c.SysProcAttr.Setpgid {
		t.Error("the default runner left the process group")
	}
	if r, ok := (DockerRunner{}).InOwnGroup().(DockerRunner); !ok || !r.OwnGroup {
		t.Errorf("InOwnGroup: %#v", r)
	}
}

// TerminateGroup sends SIGTERM to the child's whole process group (the
// child and what it started in it), which it can trap to clean up.
func TestTerminateGroup(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", `trap 'echo child > `+dir+`/child; exit 30' TERM; (trap 'echo grandchild > `+dir+`/grandchild; exit 0' TERM; sleep 30 & wait) & sleep 30 & wait`)
	OwnGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := TerminateGroup(cmd.Process); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 30 {
		t.Errorf("the child did not trap SIGTERM: %v", err)
	}
	for _, f := range []string{"child", "grandchild"} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("%s got no SIGTERM", f)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// A docker whose context ends returns within dockerWaitDelay, even when a
// child it started (the Compose plugin of an older CLI, which outlives a
// killed docker) still holds its output: a hook's bounded look at a hung
// daemon must not run on to the hook's own deadline.
func TestDockerRunnerReturnsSoonAfterItsContextEnds(t *testing.T) {
	bin := t.TempDir()
	// The child keeps the output pipe open for 10 s after docker is killed.
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nsleep 10 &\nsleep 10\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	const budget = 200 * time.Millisecond
	for name, call := range map[string]func(context.Context) error{
		"Run": func(ctx context.Context) error { _, err := (DockerRunner{}).Run(ctx, "compose", "ps"); return err },
		"RunStdout": func(ctx context.Context) error {
			_, err := (DockerRunner{}).RunStdout(ctx, "compose", "ps")
			return err
		},
		"Stream": func(ctx context.Context) error {
			var out bytes.Buffer
			return (DockerRunner{}).Stream(ctx, &out, &out, "compose", "pull")
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		start := time.Now()
		err := call(ctx)
		cancel()
		if took := time.Since(start); err == nil || took > budget+dockerWaitDelay+time.Second {
			t.Errorf("%s returned after %s (budget %s, wait delay %s): %v", name, took, budget, dockerWaitDelay, err)
		}
	}
}
