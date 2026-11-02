package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/testtool"
)

// skipLinuxUnits: these tests fake the Linux native runtime; the task
// host's refusals are the server package's (TestTaskHostRefuses…).
const skipLinuxUnits = "linux-only: fakes the Linux native runtime (systemd units); the task host's refusals are internal/server's"

// otherNativeInstall makes the units of n's project another install's,
// as a first install of the same project leaves them in the user's unit
// directory: they run from another home's native directory, which still
// holds its server.env. It returns that directory and the units' content.
func otherNativeInstall(t *testing.T, n server.Native) (other string, units map[string]string) {
	t.Helper()
	other = filepath.Join(t.TempDir(), "other-home", "data", "server", "native")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "server.env"), []byte("# the other install's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	units = map[string]string{}
	for _, u := range []string{n.DBUnit(), n.ServerUnit()} {
		p := testUnitFile(u)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		content := strings.ReplaceAll(string(raw), n.Root(), other)
		if content == string(raw) {
			t.Fatalf("%s does not name %s", p, n.Root())
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		units[p] = content
	}
	return other, units
}

// systemctlChanges are the calls to systemd that change a unit's state.
func systemctlChanges(sys *nativeSysFake) []string {
	sys.mu.Lock()
	defer sys.mu.Unlock()
	var out []string
	for _, c := range sys.calls {
		for _, verb := range []string{" enable", " disable", " start", " stop", " restart", " daemon-reload"} {
			if strings.HasPrefix(c, "systemctl ") && strings.Contains(c, verb) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// Issue #158: a second home on the native runtime with the same
// server.project would rewrite the first install's units, then start,
// stop or disable them. Each command that does refuses: exit 20, the
// other directory and server.project named, nothing changed, nothing
// asked of systemd.
func TestNativeCommandsRefuseUnitsAnotherInstallOwns(t *testing.T) {
	testtool.SkipWindows(t, skipLinuxUnits)
	for _, args := range [][]string{
		{"server", "start"},
		{"server", "stop"},
		{"server", "install"},
		{"server", "render"},
		{"uninstall"},
		{"uninstall", "--delete-data", "--answer", "confirm_bank=claude-memory"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home, n, _, sys, _ := nativeHost(t)
			other, units := otherNativeInstall(t, n)
			sys.mu.Lock()
			sys.calls = nil
			sys.mu.Unlock()
			before := treeOf(t, home)
			code, out, errOut := run(t, args...)
			refusedUnchanged(t, home, other, before, code, out, errOut)
			for p, want := range units {
				if raw, err := os.ReadFile(p); err != nil || string(raw) != want {
					t.Errorf("%s changed (%v)", p, err)
				}
			}
			if ch := systemctlChanges(sys); len(ch) > 0 {
				t.Errorf("asked systemd %q", ch)
			}
		})
	}
}

// Read-only commands are not refused: status and doctor still answer,
// and doctor names the other install.
func TestNativeReadsAreNotRefusedOnAnotherInstallsUnits(t *testing.T) {
	testtool.SkipWindows(t, skipLinuxUnits)
	_, n, _, _, _ := nativeHost(t)
	other, _ := otherNativeInstall(t, n)
	if _, out, errOut := run(t, "server", "status"); strings.Contains(out+errOut, "another backsight install") {
		t.Errorf("status refused:\n%s%s", out, errOut)
	}
	_, out, errOut := run(t, "doctor")
	if !strings.Contains(out+errOut, "native-owner") || !strings.Contains(out+errOut, other) {
		t.Errorf("doctor does not report the other install's units:\n%s%s", out, errOut)
	}
}

// The same install, and a home whose old directory is gone (moved),
// drive the units: server stop disables them.
func TestNativeStopDrivesItsOwnUnits(t *testing.T) {
	testtool.SkipWindows(t, skipLinuxUnits)
	for _, moved := range []bool{false, true} {
		t.Run(map[bool]string{false: "same home", true: "moved home"}[moved], func(t *testing.T) {
			_, n, _, sys, _ := nativeHost(t)
			if moved {
				other, _ := otherNativeInstall(t, n)
				if err := os.RemoveAll(other); err != nil {
					t.Fatal(err)
				}
			}
			if code, out, errOut := run(t, "server", "stop"); code != output.OK {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			if ch := strings.Join(systemctlChanges(sys), "\n"); !strings.Contains(ch, "disable --now") {
				t.Errorf("server stop did not disable the units:\n%s", ch)
			}
		})
	}
}
