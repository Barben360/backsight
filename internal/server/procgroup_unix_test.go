//go:build !windows

package server

import (
	"context"
	"testing"
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
