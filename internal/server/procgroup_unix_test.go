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
	// Each shell marks itself ready once its trap is set; the sleeps only
	// keep them alive until the signal (out of any test's reach).
	cmd := exec.Command("sh", "-c", `trap 'echo child > `+dir+`/child; exit 30' TERM; (trap 'echo grandchild > `+dir+`/grandchild; exit 0' TERM; : > `+dir+`/grandchild.ready; sleep 600 & wait) & : > `+dir+`/child.ready; sleep 600 & wait`)
	OwnGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"child.ready", "grandchild.ready"} {
		waitFile(filepath.Join(dir, f))
	}
	if err := TerminateGroup(cmd.Process); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 30 {
		t.Errorf("the child did not trap SIGTERM: %v", err)
	}
	// The child's trap ran before it exited; the grandchild's runs as it
	// gets the signal (a grandchild that never gets it hangs the test).
	if _, err := os.Stat(filepath.Join(dir, "child")); err != nil {
		t.Errorf("the child got no SIGTERM: %v", err)
	}
	waitFile(filepath.Join(dir, "grandchild"))
}

// waitFile waits until path exists, without a deadline of its own (go
// test's -timeout is the guard).
func waitFile(path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A docker whose context ends returns, even when a child it started (the
// Compose plugin of an older CLI, which outlives a killed docker) still
// holds its output: a hook's bounded look at a hung daemon must not run on
// to the hook's own deadline. The child holds the output until the call
// returned, so a call that waited for it would never return (go test's
// -timeout ends the test).
func TestDockerRunnerReturnsSoonAfterItsContextEnds(t *testing.T) {
	bin, dir := t.TempDir(), t.TempDir()
	// docker and its child both run while the hold file exists; docker
	// says it started once the child holds its output.
	script := `#!/bin/sh
(while [ -e "$BS_TEST_HOLD" ]; do sleep 0.05; done) &
: > "$BS_TEST_STARTED"
while [ -e "$BS_TEST_HOLD" ]; do sleep 0.05; done
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
		hold, started := filepath.Join(dir, name+".hold"), filepath.Join(dir, name+".started")
		if err := os.WriteFile(hold, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BS_TEST_HOLD", hold)
		t.Setenv("BS_TEST_STARTED", started)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- call(ctx) }()
		waitFile(started)
		cancel()
		if err := <-done; err == nil {
			t.Errorf("%s of a docker killed with its context: no error", name)
		}
		_ = os.Remove(hold) // the child goes
	}
}
