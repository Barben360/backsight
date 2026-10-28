package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

// On a client of a remote server every command that manages the server's
// lifecycle refuses, names the server's host and the command to run there,
// and touches neither Docker nor the install.
func TestRemoteModeRefusalsNameTheHost(t *testing.T) {
	home := isolate(t)
	dk := withCompose(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	_ = os.MkdirAll(filepath.Dir(cfg), 0o700)
	_ = os.WriteFile(cfg, []byte("[server]\nmode = \"remote\"\nurl = \"http://100.101.1.2:8888\"\n"), 0o600)
	before := treeOf(t, home)
	for _, tc := range []struct {
		args []string
		cmd  string
		code int
	}{
		{[]string{"server", "render"}, "server render", output.Usage},
		{[]string{"server", "install"}, "server install", output.Usage},
		{[]string{"server", "start"}, "server start", output.Usage},
		{[]string{"server", "stop"}, "server stop", output.Usage},
		{[]string{"server", "status"}, "server status", output.Usage},
		{[]string{"server", "upgrade"}, "server upgrade", output.Usage},
		{[]string{"server", "token"}, "server token", output.Usage},
		{[]string{"server", "expose", "--loopback"}, "server expose", output.Usage},
		{[]string{"server", "llm", "check"}, "server llm check", output.Usage},
		{[]string{"backup", "create"}, "backup create", output.Usage},
		{[]string{"restore", filepath.Join(home, "x.tar.gz.age")}, "restore", output.Usage},
		{[]string{"uninstall", "--stop"}, "uninstall --stop", output.Blocked},
		{[]string{"uninstall", "--delete-data"}, "uninstall --delete-data", output.Blocked},
	} {
		code, _, errOut := run(t, tc.args...)
		if code != tc.code || !strings.Contains(errOut, "100.101.1.2") || !strings.Contains(errOut, "backsight "+tc.cmd) {
			t.Errorf("%v: exit %d (want %d): %s", tc.args, code, tc.code, errOut)
		}
	}
	if len(dk.calls) != 0 {
		t.Errorf("docker calls: %q", dk.calls)
	}
	sameTree(t, before, treeOf(t, home))
}
