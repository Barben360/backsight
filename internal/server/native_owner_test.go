package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/scheduler"
)

// twoHomes are two native installs of the same server.project, as a
// second home (BACKSIGHT_HOME, a test install) beside the first: they
// share the user's unit directory. other is installed (its units and
// server.env written); mine wrote nothing there. sys records what mine
// asked systemd.
func twoHomes(t *testing.T) (mine, other Native, sys *fakeSys) {
	t.Helper()
	other, _, _ = nativeFixture(t, secrets)
	installed(t, other, secrets)
	mine, sys, _ = nativeFixture(t, secrets)
	mine.UnitDir = other.UnitDir
	return mine, other, sys
}

// unitFiles reads the units of n's project in its unit directory.
func unitFiles(t *testing.T, n Native) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, u := range []string{n.DBUnit(), n.ServerUnit()} {
		raw, err := os.ReadFile(filepath.Join(n.UnitDir, u))
		if err != nil {
			t.Fatal(err)
		}
		out[u] = string(raw)
	}
	return out
}

// nativeOps are the native runtime's operations that write, enable,
// start, stop or delete the units (Windows: the task and the supervisor).
func nativeOps() map[string]func(context.Context, Native) error {
	return map[string]func(context.Context, Native) error{
		"write": func(_ context.Context, n Native) error {
			f := Files{Env: []byte("# changed\n"), Units: map[string][]byte{}}
			for _, name := range n.host().fileNames() {
				f.Units[name] = []byte("# changed\n")
			}
			_, err := n.Write(f)
			return err
		},
		"start":           func(ctx context.Context, n Native) error { return n.Start(ctx) },
		"start database":  func(ctx context.Context, n Native) error { return n.StartDatabase(ctx) },
		"stop":            func(ctx context.Context, n Native) error { return n.Stop(ctx) },
		"stop api":        func(ctx context.Context, n Native) error { return n.StopAPI(ctx) },
		"stop on purpose": func(ctx context.Context, n Native) error { return n.StopOnPurpose(ctx) },
		"delete":          func(ctx context.Context, n Native) error { return n.Delete(ctx) },
		"unregister":      func(ctx context.Context, n Native) error { return n.Unregister(ctx) },
	}
}

// foreign checks err is the refusal of other's server: a
// *ForeignNativeError naming other's directory and server.project.
func foreign(t *testing.T, err error, other string) {
	t.Helper()
	var fe *ForeignNativeError
	if !errors.As(err, &fe) {
		t.Fatalf("got %v, want a ForeignNativeError", err)
	}
	if fe.Other != other {
		t.Errorf("the error names %q, want %q", fe.Other, other)
	}
	for _, want := range []string{other, "server.project"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not name %q", err, want)
		}
	}
}

// Issue #158: a second home of the same project would rewrite the first
// install's units to its own directory, then start, stop or disable
// them. Every operation on them refuses: nothing written, nothing asked
// of systemd.
func TestNativeRefusesUnitsAnotherInstallOwns(t *testing.T) {
	for name, op := range nativeOps() {
		t.Run(name, func(t *testing.T) {
			mine, other, sys := twoHomes(t)
			before := unitFiles(t, other)
			foreign(t, op(context.Background(), mine), other.Root())
			if got := unitFiles(t, other); !reflect.DeepEqual(got, before) {
				t.Errorf("the other install's units changed:\n%v", got)
			}
			if len(sys.Calls) > 0 {
				t.Errorf("asked systemd %q", sys.lines())
			}
			if !exists(mine.Root()) || exists(mine.EnvFile()) {
				t.Errorf("this install's directory changed (root kept %v, server.env written %v)", exists(mine.Root()), exists(mine.EnvFile()))
			}
		})
	}
}

// The same install, whatever path reaches its directory, drives its
// units; so does a home whose old directory is gone (moved or renamed)
// or holds no server.env any more (an old copy nothing runs): it takes
// them over.
func TestNativeDrivesItsOwnUnits(t *testing.T) {
	ctx := context.Background()
	disabled := func(t *testing.T, n Native, sys *fakeSys) {
		t.Helper()
		if err := n.Stop(ctx); err != nil {
			t.Fatalf("refused: %v", err)
		}
		want := []string{"systemctl --user disable --now backsight-server.service backsight-db.service"}
		if got := sys.lines(); !reflect.DeepEqual(got, want) {
			t.Errorf("Stop ran %q, want %q", got, want)
		}
	}
	t.Run("same home", func(t *testing.T) {
		n, sys, _ := nativeFixture(t, secrets)
		installed(t, n, secrets)
		disabled(t, n, sys)
	})
	t.Run("through a symlink", func(t *testing.T) {
		n, sys, _ := nativeFixture(t, secrets)
		installed(t, n, secrets)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(n.Cfg.Server.ComposeDir, link); err != nil {
			t.Skip("no symlinks here:", err)
		}
		n.Cfg.Server.ComposeDir = link
		disabled(t, n, sys)
	})
	t.Run("old directory gone", func(t *testing.T) {
		mine, other, sys := twoHomes(t)
		if err := os.RemoveAll(other.Root()); err != nil {
			t.Fatal(err)
		}
		disabled(t, mine, sys)
	})
	t.Run("old copy without server.env", func(t *testing.T) {
		mine, other, sys := twoHomes(t)
		if err := os.Remove(other.EnvFile()); err != nil {
			t.Fatal(err)
		}
		disabled(t, mine, sys)
	})
	t.Run("no units", func(t *testing.T) {
		n, _, _ := nativeFixture(t, secrets)
		if err := n.CheckOwner(ctx); err != nil {
			t.Errorf("refused without units: %v", err)
		}
	})
}

// A moved home's first render rewrites the units to its own directory.
func TestNativeTakesOverTheUnitsOfAMovedHome(t *testing.T) {
	mine, other, _ := twoHomes(t)
	if err := os.RemoveAll(other.Root()); err != nil {
		t.Fatal(err)
	}
	rendered(t, mine, secrets)
	for u, content := range unitFiles(t, mine) {
		if strings.Contains(content, other.Root()) || !strings.Contains(content, mine.Root()) {
			t.Errorf("%s still names the old directory:\n%s", u, content)
		}
	}
}

// Only one unit left, either one, tells the owner.
func TestNativeReadsTheOwnerOffEitherUnit(t *testing.T) {
	for _, keep := range []string{"backsight-db.service", "backsight-server.service"} {
		t.Run(keep, func(t *testing.T) {
			mine, other, _ := twoHomes(t)
			for _, u := range []string{other.DBUnit(), other.ServerUnit()} {
				if u != keep {
					if err := os.Remove(filepath.Join(other.UnitDir, u)); err != nil {
						t.Fatal(err)
					}
				}
			}
			foreign(t, mine.CheckOwner(context.Background()), other.Root())
		})
	}
}

// A unit that cannot be read is not known to be this install's: refused,
// nothing asked of systemd.
func TestNativeRefusesAnUnreadableUnit(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	n, sys, _ := nativeFixture(t, secrets)
	installed(t, n, secrets)
	unit := filepath.Join(n.UnitDir, n.DBUnit())
	if err := os.Chmod(unit, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unit, 0o644) })
	err := n.Stop(context.Background())
	if err == nil || !strings.Contains(err.Error(), unit) {
		t.Fatalf("got %v, want a refusal naming %s", err, unit)
	}
	if len(sys.Calls) > 0 {
		t.Errorf("asked systemd %q", sys.lines())
	}
}

// Reading the units' state, their logs or doctor's facts is never
// refused.
func TestNativeReadsAreNotGuarded(t *testing.T) {
	mine, _, sys := twoHomes(t)
	sys.Out = func(c []string) ([]byte, error) {
		return []byte("ActiveState=active\nSubState=running\n\nActiveState=active\nSubState=running\n"), nil
	}
	if _, err := mine.Status(context.Background()); err != nil {
		t.Errorf("status refused: %v", err)
	}
	if _, err := mine.Logs(context.Background(), "db", 5); err != nil {
		t.Errorf("logs refused: %v", err)
	}
}

// otherShim is another install's program (its shim), which the task it
// registered runs.
func otherShim(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "other-home", "data", "bin", "backsight.exe")
	touch(t, p, 0o755)
	return p
}

// taskXMLFor is the task XML schtasks /Query /XML prints of a server task
// that runs program.
func taskXMLFor(t *testing.T, program string, args ...string) []byte {
	t.Helper()
	x, err := scheduler.TaskXML(scheduler.TaskSpec{Description: "backsight memory server (bs)", UserSID: taskTestSID,
		Triggers: []scheduler.Trigger{{Kind: scheduler.TriggerLogon}}, Program: program,
		Args: append([]string{"server", "supervise"}, args...)})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// changedTask lists what the task host did to the task or the
// supervisor: anything but a query or a status.
func changedTask(f *fakeTask) []string {
	var out []string
	for _, e := range f.ev.all() {
		if !strings.Contains(e, "/Query") && e != "pipe status" {
			out = append(out, e)
		}
	}
	return out
}

// Issue #158 on Windows: the task \<project>\server another install
// registered (its program is that install's shim) is never registered
// again, enabled, run, disabled or deleted, and its supervisor is never
// stopped.
func TestTaskHostRefusesAnotherInstallsTask(t *testing.T) {
	for name, op := range nativeOps() {
		t.Run(name, func(t *testing.T) {
			h, f := taskFixture(t)
			shim := otherShim(t)
			f.registered, f.xml = true, taskXMLFor(t, shim, "--backsight-home", filepath.Dir(filepath.Dir(filepath.Dir(shim))))
			f.start()
			err := op(context.Background(), h.n)
			foreign(t, err, filepath.Dir(filepath.Dir(shim)))
			if ch := changedTask(f); len(ch) > 0 {
				t.Errorf("acted on the task or the supervisor: %q", ch)
			}
			if h.n.Stopped() {
				t.Error("the refused command marked the server stopped")
			}
		})
	}
}

// A supervisor that answers from another install's native directory
// (Status.Root) is that install's: never stopped or started from here,
// even without a task (server.start_on_session).
func TestTaskHostRefusesAnotherInstallsSupervisor(t *testing.T) {
	for name, op := range nativeOps() {
		t.Run(name, func(t *testing.T) {
			h, f := taskFixture(t)
			other := filepath.Join(t.TempDir(), "other", "data", "server", "native")
			touch(t, filepath.Join(other, "server.env"), 0o600)
			f.start()
			f.st.Root = other
			foreign(t, op(context.Background(), h.n), other)
			if ch := changedTask(f); len(ch) > 0 {
				t.Errorf("acted on the task or the supervisor: %q", ch)
			}
		})
	}
}

// This install's own task and supervisor are driven; so are those of a
// home whose program or directory is gone (moved): taken over. A
// supervisor too old to say its root, or a task whose XML names no
// program, cannot be told: driven, as before.
func TestTaskHostDrivesItsOwnTaskAndSupervisor(t *testing.T) {
	cases := map[string]func(t *testing.T, h taskHost, f *fakeTask){
		"its own task": func(t *testing.T, h taskHost, f *fakeTask) {
			f.xml = taskXMLFor(t, h.n.shim(), scheduler.TaskDirArgs(h.n.Env)...)
		},
		"its own task, other case": func(t *testing.T, h taskHost, f *fakeTask) {
			if runtime.GOOS != "windows" {
				t.Skip("paths ignore case on Windows only")
			}
			f.xml = taskXMLFor(t, strings.ToUpper(h.n.shim()))
		},
		"a moved home's task": func(t *testing.T, h taskHost, f *fakeTask) {
			f.xml = taskXMLFor(t, filepath.Join(t.TempDir(), "gone", "data", "bin", "backsight.exe"))
		},
		"a task naming no program": func(t *testing.T, h taskHost, f *fakeTask) { f.xml = []byte("<Task/>") },
		"its own supervisor":       func(t *testing.T, h taskHost, f *fakeTask) { f.st.Root = h.n.Root() },
		"a moved home's supervisor": func(t *testing.T, h taskHost, f *fakeTask) {
			f.st.Root = filepath.Join(t.TempDir(), "gone", "native")
		},
		"an old copy's supervisor, server.env deleted": func(t *testing.T, h taskHost, f *fakeTask) {
			f.st.Root = filepath.Join(t.TempDir(), "copy", "native")
			if err := os.MkdirAll(f.st.Root, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a supervisor that names no root": func(t *testing.T, h taskHost, f *fakeTask) { f.st.Root = "" },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			h, f := taskFixture(t)
			f.registered = true
			f.start()
			set(t, h, f)
			if err := h.n.Stop(context.Background()); err != nil {
				t.Fatalf("refused: %v", err)
			}
			got := changedTask(f)
			if len(got) < 2 || got[0] != `schtasks /Change /TN \bs\server /DISABLE` || got[1] != "pipe stop" {
				t.Errorf("Stop did %q, want the task disabled, then the supervisor stopped", got)
			}
		})
	}
}

// A task's arguments, as Windows splits them, read back what TaskXML
// quoted.
func TestSplitWindowsArgsReadsBackEscapeArg(t *testing.T) {
	for _, argv := range [][]string{
		{"--headless", `C:\Users\Me\AppData\Local\backsight\data\bin\backsight.exe`, "server", "supervise"},
		{"--headless", `C:\Users\Jean Dupont\b s\backsight.exe`, "server", "supervise", "--backsight-home", `C:\a "b"\c\`},
		{"", `a\\b`, `trailing\`, `q"`, `\\"x`},
	} {
		quoted := make([]string, len(argv))
		for i, a := range argv {
			quoted[i] = scheduler.EscapeArg(a)
		}
		if got := splitWindowsArgs(strings.Join(quoted, " ")); !reflect.DeepEqual(got, argv) {
			t.Errorf("splitWindowsArgs(%q) = %q, want %q", strings.Join(quoted, " "), got, argv)
		}
	}
}
