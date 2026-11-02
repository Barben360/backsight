package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

// otherInstall is another backsight install's compose directory: the one
// the project's containers were created from.
func otherInstall(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "other-home", "data", "server")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// refusedUnchanged checks a refusal of another install's project: exit 20,
// the other directory and server.project named, and no file of home
// changed (the lock directories and the command's log aside).
func refusedUnchanged(t *testing.T, home, other string, before map[string]string, code int, out, errOut string) {
	t.Helper()
	if code != output.Blocked {
		t.Fatalf("exit %d, want %d\n%s%s", code, output.Blocked, out, errOut)
	}
	for _, want := range []string{other, "server.project", "nothing was changed"} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("output does not say %q:\n%s%s", want, out, errOut)
		}
	}
	noise := func(p string) bool {
		rel, _ := filepath.Rel(home, p)
		return strings.Contains(rel, "locks") || strings.Contains(rel, "logs") || rel == filepath.Join("backsight", "state")
	}
	after := treeOf(t, home)
	for p, v := range after {
		if was, ok := before[p]; (!ok || was != v) && !noise(p) {
			t.Errorf("%s changed", p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok && !noise(p) {
			t.Errorf("%s removed", p)
		}
	}
}

// Issue #144: a second home without server.project drove the machine's
// main install. Every command that changes the server's containers
// refuses a project whose containers come from another install's
// compose directory: exit 20, nothing changed, the other directory named
// with the way out.
func TestServerCommandsRefuseAProjectAnotherInstallOwns(t *testing.T) {
	for _, args := range [][]string{
		{"server", "start"},
		{"server", "stop"},
		{"server", "install"},
		{"server", "expose", "--private", "172.17.0.1", "--answer", "cleartext_lan=yes"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := devInstall(t, true)
			f := withCompose(t)
			other := otherInstall(t)
			f.owners = []string{other}
			before := treeOf(t, home)
			code, out, errOut := run(t, args...)
			refusedUnchanged(t, home, other, before, code, out, errOut)
			if f.ownerChecks == 0 || len(f.calls) > 0 {
				t.Errorf("%d owner checks, then docker ran %q", f.ownerChecks, f.calls)
			}
		})
	}
}

// server upgrade and restore stop the server and replace its database:
// refused before the safety backup, the pull or any stop.
func TestUpgradeAndRestoreRefuseAProjectAnotherInstallOwns(t *testing.T) {
	t.Run("server upgrade", func(t *testing.T) {
		home, dk, _, _ := upgradeSetup(t)
		other := otherInstall(t)
		dk.owners = []string{other}
		before := treeOf(t, home)
		code, out, errOut := run(t, "server", "upgrade")
		refusedUnchanged(t, home, other, before, code, out, errOut)
		if len(dk.pulls) > 0 || len(dk.images) > 0 {
			t.Errorf("pulled %q, started %q", dk.pulls, dk.images)
		}
	})
	t.Run("restore", func(t *testing.T) {
		home, archive, dk, _ := restoreSetup(t)
		other := otherInstall(t)
		dk.owners = []string{other}
		before := treeOf(t, home)
		code, out, errOut := run(t, "restore", archive, "--no-safety", "--non-interactive")
		refusedUnchanged(t, home, other, before, code, out, errOut)
		for _, c := range dk.calls {
			if strings.Contains(c, " stop") || strings.Contains(c, "dropdb") || strings.Contains(c, " up ") {
				t.Errorf("docker ran %q", c)
			}
		}
	})
}

// uninstall --delete-data would remove another install's server with its
// memories: refused before anything is removed.
func TestUninstallRefusesAProjectAnotherInstallOwns(t *testing.T) {
	m := installMachine(t)
	other := otherInstall(t)
	m.dk.owners = []string{other}
	before := treeOf(t, m.home)
	code, out, errOut := run(t, "uninstall", "--delete-data", "--answer", "confirm_bank=claude-memory")
	refusedUnchanged(t, m.home, other, before, code, out, errOut)
	if sub := m.dk.subcommands(); len(sub) > 0 {
		t.Errorf("compose ran %q", sub)
	}
	for _, c := range m.dk.calls {
		if c[0] == "volume" {
			t.Errorf("docker ran %q", c)
		}
	}
}

// Read-only commands are not refused: status still answers.
func TestServerStatusIsNotRefusedOnAnotherInstallsProject(t *testing.T) {
	devInstall(t, true)
	f := withCompose(t)
	f.owners = []string{otherInstall(t)}
	if _, out, errOut := run(t, "server", "status"); strings.Contains(out+errOut, "another backsight install") {
		t.Errorf("status refused:\n%s%s", out, errOut)
	}
}

// The same home, or one whose old directory is gone (moved), proceeds.
func TestServerStartDrivesItsOwnProject(t *testing.T) {
	for name, owner := range map[string]func(home string) string{
		"same directory": composeDir,
		"moved home":     func(string) string { return filepath.Join(t.TempDir(), "gone", "data", "server") },
	} {
		t.Run(name, func(t *testing.T) {
			home := devInstall(t, true)
			f := withCompose(t)
			f.owners = []string{owner(home)}
			if code, out, errOut := run(t, "server", "start"); code != output.OK {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			if got := strings.Join(f.subcommands(), " | "); got != "up -d --wait" {
				t.Errorf("compose ran %s", got)
			}
		})
	}
}

// A key rotation restarts the server on a new key: never another
// install's, and the key stays as it was.
func TestRotateRefusesAProjectAnotherInstallOwns(t *testing.T) {
	home, _, dk := tokenHost(t)
	other := otherInstall(t)
	dk.owners = []string{other}
	before := treeOf(t, home)
	code, out, errOut := run(t, "server", "token", "--rotate", "--out", filepath.Join(t.TempDir(), "join.token"))
	refusedUnchanged(t, home, other, before, code, out, errOut)
	if len(dk.keys) > 0 {
		t.Errorf("the server was started %d times", len(dk.keys))
	}
}

// A refused start leaves even a dead owner's maintenance lock in place:
// nothing was changed (review of #157).
func TestRefusedStartKeepsAStaleMaintenanceLock(t *testing.T) {
	home := devInstall(t, true)
	f := withCompose(t)
	other := otherInstall(t)
	f.owners = []string{other}
	writeDeadMaintenanceLock(t, home)
	if code, out, errOut := run(t, "server", "start"); code != output.Blocked || !strings.Contains(out+errOut, other) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(pathsOf(home).MaintenanceLock()); err != nil {
		t.Errorf("the refused start removed the stale maintenance lock: %v", err)
	}
}
