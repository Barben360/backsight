package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
)

// checkTools answers as the bundled Postgres tools: pg_controldata prints
// the identifier held in the data directory's fake-sysid file; effect does
// what the tool would.
type checkTools struct {
	calls  []string
	envs   [][]string
	effect func(path string, args []string)
}

func (f *checkTools) Run(_ context.Context, env []string, _ io.Reader, path string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{path}, args...), " "))
	f.envs = append(f.envs, env)
	if f.effect != nil {
		f.effect(path, args)
	}
	if filepath.Base(path) == "pg_controldata" && len(args) == 2 {
		id, err := os.ReadFile(filepath.Join(args[1], "fake-sysid"))
		if err == nil {
			return []byte("pg_control version number:            1800\nDatabase system identifier:           " + strings.TrimSpace(string(id)) + "\n"), nil
		}
	}
	return nil, nil
}

// nativeCheckHost is a native install's config with no database yet: n
// names its paths, the tools and /proc are fakes.
func nativeCheckHost(t *testing.T) (server.Native, *checkTools) {
	t.Helper()
	home := isolate(t)
	d := pathsOf(home)
	if err := os.MkdirAll(filepath.Dir(d.ConfigFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.ConfigFile(), []byte("[server]\nruntime = \"native\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatal(err)
	}
	tools := &checkTools{}
	oldTools, oldProc := nativeTools, nativeProc
	nativeTools, nativeProc = tools, t.TempDir()
	t.Cleanup(func() { nativeTools, nativeProc = oldTools, oldProc })
	return server.Native{Cfg: cfg, Dirs: d}, tools
}

// withInstance gives n a database whose identifier is id, recording rec
// as its identity ("" records none).
func withInstance(t *testing.T, n server.Native, id, rec string) {
	t.Helper()
	if err := os.MkdirAll(n.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"PG_VERSION": "18\n", "fake-sysid": id + "\n"} {
		if err := os.WriteFile(filepath.Join(n.DataDir(), name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if rec != "" {
		if err := os.WriteFile(n.IdentityFile(), []byte(rec+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// checkRefuses runs native-check and wants exit 20 with exactly msg.
func checkRefuses(t *testing.T, msg string) {
	t.Helper()
	code, out, errOut := run(t, "server", "native-check")
	if code != output.Blocked {
		t.Errorf("exit %d, want %d\n%s%s", code, output.Blocked, out, errOut)
	}
	if !strings.Contains(errOut, "backsight: "+msg+"\n") {
		t.Errorf("stderr\n%s\nwant\n%s", errOut, msg)
	}
}

func TestNativeCheckRefusesAMissingInstance(t *testing.T) {
	n, _ := nativeCheckHost(t)
	withInstance(t, n, "111", "111")
	if err := os.RemoveAll(n.InstanceDir()); err != nil {
		t.Fatal(err)
	}
	checkRefuses(t, "the native database "+n.InstanceDir()+" is missing: refusing to start an empty memory in its place. Put it back, or restore a backup: backsight restore <archive>")
}

func TestNativeCheckRefusesAnEmptyInstance(t *testing.T) {
	n, _ := nativeCheckHost(t)
	if err := os.MkdirAll(n.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.IdentityFile(), []byte("111\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkRefuses(t, "the native database "+n.InstanceDir()+" is empty: refusing to start an empty memory in its place. Put it back, or restore a backup: backsight restore <archive>")
}

func TestNativeCheckRefusesAnotherIdentity(t *testing.T) {
	n, _ := nativeCheckHost(t)
	withInstance(t, n, "222", "111")
	checkRefuses(t, "the native database "+n.InstanceDir()+" is not the one backsight recorded (identifier 222, recorded 111): refusing to start it. Put the right one back, or restore a backup: backsight restore <archive>")
}

func TestNativeCheckRefusesAnUnrecordedInstance(t *testing.T) {
	n, _ := nativeCheckHost(t)
	withInstance(t, n, "111", "")
	checkRefuses(t, "the native database "+n.InstanceDir()+" has no recorded identity ("+n.IdentityFile()+" is missing): refusing to start it. Restore a backup to adopt one: backsight restore <archive>")
}

func TestNativeCheckPassesAFirstInstall(t *testing.T) {
	nativeCheckHost(t)
	if code, out, errOut := run(t, "server", "native-check"); code != output.OK {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestNativeCheckPassesTheRecordedInstance(t *testing.T) {
	n, _ := nativeCheckHost(t)
	withInstance(t, n, "7702339718603615110", "7702339718603615110")
	if code, out, errOut := run(t, "server", "native-check"); code != output.OK {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

// writeProc puts a process in the fake /proc.
func writeProc(t *testing.T, pid string, argv []string, cgroup, cwd string) {
	t.Helper()
	dir := filepath.Join(nativeProc, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::/user.slice/user-1000.slice/user@1000.service/app.slice/"+cgroup+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cwd != "" {
		if err := os.Symlink(cwd, filepath.Join(dir, "cwd")); err != nil {
			t.Fatal(err)
		}
	}
}

// Requirement 3 (ruling R22): a postgres on the instance's data directory
// outside the db unit is reported and stopped with the bundled pg_ctl;
// one in the db unit, or on another data directory, is left alone.
func TestNativeCheckStopsAStrayPostgres(t *testing.T) {
	n, tools := nativeCheckHost(t)
	withInstance(t, n, "111", "111")
	pg := filepath.Join(n.Home(), ".pg0", "installation", "18.1.0", "bin", "postgres")
	writeProc(t, "4243", []string{pg, "-D", n.DataDir()}, "backsight-db.service", n.DataDir())
	writeProc(t, "4244", []string{pg, "-D", "/elsewhere/data"}, "foo.scope", "/elsewhere/data")

	code, out, errOut := run(t, "server", "native-check")
	if code != output.OK || strings.Contains(out+errOut, "stopping it") {
		t.Errorf("exit %d without a stray\n%s%s", code, out, errOut)
	}
	for _, c := range tools.calls {
		if prog := filepath.Base(strings.Fields(c)[0]); prog == "pg_ctl" || prog == "pg0" {
			t.Errorf("stopped a postgres that is no stray: %s", c)
		}
	}

	writeProc(t, "4242", []string{pg, "-D", n.DataDir(), "-p", "5432"}, "foo.scope", n.DataDir())
	tools.effect = func(path string, _ []string) {
		if filepath.Base(path) == "pg_ctl" {
			_ = os.RemoveAll(filepath.Join(nativeProc, "4242"))
		}
	}
	tools.calls, tools.envs = nil, nil
	code, out, errOut = run(t, "server", "native-check")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	report := "a postgres (pid 4242) runs on " + n.DataDir() + " outside backsight-db.service: stopping it, never adopting it"
	if !strings.Contains(out+errOut, report) {
		t.Errorf("no report %q in\n%s%s", report, out, errOut)
	}
	if strings.Contains(out+errOut, "pid 4243") || strings.Contains(out+errOut, "pid 4244") {
		t.Errorf("reported a postgres that is no stray:\n%s%s", out, errOut)
	}
	inst := filepath.Join(n.Home(), ".pg0", "installation", "18.1.0")
	want := filepath.Join(inst, "bin", "pg_ctl") + " stop -D " + n.DataDir() + " -m fast"
	i := slices.IndexFunc(tools.calls, func(c string) bool { return strings.HasPrefix(c, want) })
	if i < 0 {
		t.Fatalf("no %q among\n%s", want, strings.Join(tools.calls, "\n"))
	}
	if !slices.Contains(tools.envs[i], "LD_LIBRARY_PATH="+filepath.Join(inst, "lib")) {
		t.Errorf("pg_ctl env %v", tools.envs[i])
	}
	for _, c := range tools.calls {
		if filepath.Base(strings.Fields(c)[0]) == "pg0" {
			t.Errorf("a stray stopped with pg0 (ruling R22): %s", c)
		}
	}
}
