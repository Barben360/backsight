//go:build !windows

package server

import (
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
