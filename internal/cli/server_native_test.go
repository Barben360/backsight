package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/supervise"
	"github.com/Barben360/backsight/internal/testtool"
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
	if strings.TrimSuffix(filepath.Base(path), ".exe") == "pg_controldata" && len(args) == 2 {
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
	testtool.SkipWindows(t, "linux-only: fakes the Linux native runtime (systemd units, its layout and tools); Windows' task host has its own tests")
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
	testtool.SkipWindows(t, "linux-only: fakes the Linux native runtime (systemd units, its layout and tools); Windows' task host has its own tests")
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
	testtool.SkipWindows(t, "linux-only: a fake /proc (TestStraysOnWindows covers Windows' scan)")
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
	writePidFile(t, n, "4242")
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

// writePidFile writes the data directory's postmaster.pid naming pid.
func writePidFile(t *testing.T, n server.Native, pid string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(n.DataDir(), "postmaster.pid"), []byte(pid+"\n"+n.DataDir()+"\n1730000000\n5432\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// strayHost is a recorded instance with a stray, pid 4242, on its data.
func strayHost(t *testing.T) (server.Native, *checkTools) {
	t.Helper()
	n, tools := nativeCheckHost(t)
	withInstance(t, n, "111", "111")
	writeProc(t, "4242", []string{"postgres", "-D", n.DataDir()}, "foo.scope", n.DataDir())
	return n, tools
}

// pgCtlRuns counts pg_ctl among the tool calls.
func pgCtlRuns(tools *checkTools) int {
	k := 0
	for _, c := range tools.calls {
		if filepath.Base(strings.Fields(c)[0]) == "pg_ctl" {
			k++
		}
	}
	return k
}

// A /proc that cannot be read is a refusal: the guard cannot tell.
func TestNativeCheckRefusesWhenItCannotLookForStrays(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: fakes the Linux native runtime (systemd units, its layout and tools); Windows' task host has its own tests")
	n, _ := nativeCheckHost(t)
	withInstance(t, n, "111", "111")
	nativeProc = filepath.Join(t.TempDir(), "no-proc")
	code, out, errOut := run(t, "server", "native-check")
	if code != output.Blocked || !strings.Contains(errOut, "looking for a postgres on "+n.DataDir()) {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

// A stray still running after the wait is a refusal naming its pid.
func TestNativeCheckRefusesAStrayThatDoesNotStop(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: a fake /proc (TestStraysOnWindows covers Windows' scan)")
	n, tools := strayHost(t)
	writePidFile(t, n, "4242")
	old := nativeStrayWait
	nativeStrayWait = 50 * time.Millisecond
	t.Cleanup(func() { nativeStrayWait = old })
	code, out, errOut := run(t, "server", "native-check")
	if code != output.Blocked || !strings.Contains(errOut, "a postgres still runs on "+n.DataDir()) || !strings.Contains(errOut, "(pid 4242)") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if pgCtlRuns(tools) != 1 {
		t.Errorf("pg_ctl ran %d times: %v", pgCtlRuns(tools), tools.calls)
	}
}

// Ruling R36: pg_ctl stops postmaster.pid's pid, so another pid there is a
// refusal naming both, and nothing is stopped.
func TestNativeCheckStopsNothingWhenThePidFileNamesAnother(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: a fake /proc (TestStraysOnWindows covers Windows' scan)")
	n, tools := strayHost(t)
	writePidFile(t, n, "4243")
	code, out, errOut := run(t, "server", "native-check")
	if code != output.Blocked || !strings.Contains(errOut, "a postgres (pid 4242) runs on "+n.DataDir()) || !strings.Contains(errOut, "names pid 4243") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if pgCtlRuns(tools) != 0 {
		t.Errorf("pg_ctl ran: %v", tools.calls)
	}
}

// server render's text on native names the files it writes: server.env
// and the two units, never compose.yaml.
func TestNativeRenderNamesItsFiles(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: fakes the Linux native runtime (systemd units, its layout and tools); Windows' task host has its own tests")
	home, n, _, _, _ := nativeHost(t)
	code, out, errOut := run(t, "server", "render")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, line := range []string{n.EnvFile() + ": unchanged\n", unitPath(home, n.DBUnit()) + " and " + unitPath(home, n.ServerUnit()) + ": unchanged\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("text lacks %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "compose.yaml") || strings.Contains(out, filepath.Join(n.Cfg.Server.ComposeDir, ".env")) {
		t.Errorf("text names Docker's files:\n%s", out)
	}
}

// A native server that never reports health points at its journal, not
// at docker compose.
func TestNativeHealthTimeoutNamesTheJournal(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: fakes the Linux native runtime (systemd units, its layout and tools); Windows' task host has its own tests")
	_, n, _, _, _ := nativeHost(t)
	oldT := healthTimeout
	healthTimeout = 20 * time.Millisecond
	t.Cleanup(func() { healthTimeout = oldT })
	cfg := n.Cfg
	cfg.Server.URL = "http://127.0.0.1:" + strconv.Itoa(closedPort(t))
	n.Cfg = cfg
	_, err := waitHealthy(context.Background(), runtimeFor(cfg), cfg, config.Secrets{})
	if err == nil || !strings.Contains(err.Error(), "journalctl --user -u "+n.ServerUnit()) || strings.Contains(err.Error(), "docker compose") {
		t.Errorf("err %v", err)
	}
}

// Final review 7: the db unit's guard on an install back on Docker (a
// switch to native that failed, its data left) names server runtime
// clean, and so does the switch's own target, built before config.toml
// names native.
func TestNativeRefusalOnADockerInstallNamesClean(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: fakes the Linux native runtime (systemd units, its layout and tools); Windows' task host has its own tests")
	n, _ := nativeCheckHost(t)
	withInstance(t, n, "222", "111")
	if err := os.WriteFile(n.Dirs.ConfigFile(), []byte("[server]\n"), 0o600); err != nil { // back on docker
		t.Fatal(err)
	}
	code, out, errOut := run(t, "server", "native-check")
	if code != output.Blocked || !strings.Contains(errOut, "delete it with backsight server runtime clean") || strings.Contains(errOut, "backsight restore") {
		t.Errorf("native-check: exit %d\n%s%s", code, out, errOut)
	}
	d := n.Dirs
	cfg, err := config.Load(d.ConfigFile(), d, os.Getenv("HOME"))
	if err != nil {
		t.Fatal(err)
	}
	target, ok := runtimeOfKind(cfg, "native").(server.Native)
	if !ok || !target.NotInUse {
		t.Fatalf("the switch's native target %+v does not know config.toml names docker", target)
	}
	if err := target.CheckInstance(context.Background()); err == nil || !strings.Contains(err.Error(), "backsight server runtime clean") {
		t.Errorf("the switch's target: %v", err)
	}
	native := configOfKind(cfg, "native")
	if n, ok := runtimeOfKind(native, "native").(server.Native); !ok || n.NotInUse {
		t.Errorf("on native, the native runtime is in use: %+v", n)
	}
}

// superviseHost is nativeCheckHost with project p and a supervise.json
// under the native root whose postgres, pg_isready, pg_ctl and API are
// fakes in dir: postgres records its argv, is ready at once and stops on
// pg_ctl.
func superviseHost(t *testing.T, p string) (server.Native, supervise.Config) {
	t.Helper()
	n, _ := nativeCheckHost(t)
	if err := os.WriteFile(n.Dirs.ConfigFile(), []byte("[server]\nruntime = \"native\"\nproject = \""+p+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.Cfg.Server.Project = p
	dir := t.TempDir()
	file := func(name string) string { return filepath.Join(dir, name) }
	pg := testtool.Write(t, dir, "postgres", testtool.Fake{Script: "write " + file("ready") + " x\nwaitfile " + file("pg-stop")})
	isReady := testtool.Write(t, dir, "pg_isready", testtool.Fake{Script: "nofile " + file("ready") + " 2"})
	ctl := testtool.Write(t, dir, "pg_ctl", testtool.Fake{Script: "write " + file("pg-stop") + " x"})
	api := testtool.Write(t, dir, "hindsight-api", testtool.Fake{Script: "waitfile " + file("never")})
	if err := os.MkdirAll(n.Root(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.EnvFile(), []byte("X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := supervise.Config{Project: p, DataDir: n.DataDir(), Postgres: []string{pg, "-D", n.DataDir(), "-c", "fsync=on"}, PGIsReady: []string{isReady},
		PGCtlStop: []string{ctl, "stop"}, API: []string{api}, EnvFile: n.EnvFile(), LogDir: file("logs"), Venv: "1.1.0-abcdefabcdef"}
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(n.Root(), "supervise.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return n, sc
}

// runSupervise runs `server supervise args` in a goroutine; the channel
// gets its exit code.
func runSupervise(t *testing.T, p string, args ...string) <-chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		code, _, _ := run(t, append([]string{"server", "supervise"}, args...)...)
		done <- code
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = supervise.Ask(ctx, p, "stop")
	})
	return done
}

func superviseStatus(t *testing.T, p, db string) supervise.Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		st, err := supervise.Ask(ctx, p, "status")
		cancel()
		if err == nil && st.DB == db {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor of %s: %+v %v, want db %s", p, st, err, db)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func stopSupervise(t *testing.T, p string, done <-chan int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := supervise.Ask(ctx, p, "stop"); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != output.OK {
			t.Errorf("server supervise exited %d after stop, want 0", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server supervise did not exit after stop")
	}
}

// The guard's identity refusal is final: refused, nothing started, the
// reason in <Root>/refused and the status, until stop.
func TestServerSuperviseRefusesAnUnrecordedInstance(t *testing.T) {
	p := "bs-test-" + strconv.Itoa(os.Getpid()) + "-cli-refused"
	n, sc := superviseHost(t, p)
	withInstance(t, n, "111", "")
	done := runSupervise(t, p)
	st := superviseStatus(t, p, "refused")
	if !strings.Contains(st.Refusal, "has no recorded identity") || st.API != "stopped" || st.Venv != sc.Venv {
		t.Errorf("status %+v: want the unrecorded refusal", st)
	}
	if raw, err := os.ReadFile(filepath.Join(n.Root(), "refused")); err != nil || !strings.Contains(string(raw), "has no recorded identity") {
		t.Errorf("refused file %q (%v)", raw, err)
	}
	if got := testtool.Calls(t, sc.Postgres[0]); got != nil {
		t.Errorf("postgres ran %q on a refused instance", got)
	}
	stopSupervise(t, p, done)
}

func TestServerSuperviseRunsSuperviseJSON(t *testing.T) {
	p := "bs-test-" + strconv.Itoa(os.Getpid()) + "-cli-runs"
	_, sc := superviseHost(t, p)
	setBuild(t, "7.7.7", "abc")
	done := runSupervise(t, p, "--no-api")
	st := superviseStatus(t, p, "running")
	if st.Version != "7.7.7" || st.API != "stopped" {
		t.Errorf("status %+v: want backsight's version and no API (--no-api)", st)
	}
	if got := testtool.Calls(t, sc.Postgres[0]); len(got) != 1 || !slices.Equal(got[0], sc.Postgres[1:]) {
		t.Errorf("postgres argv %q, want %q", got, sc.Postgres[1:])
	}
	if got := testtool.Calls(t, sc.API[0]); got != nil {
		t.Errorf("the API ran %q under --no-api", got)
	}
	stopSupervise(t, p, done)
}

// Requirement 1 on Windows (Task 10's ruling): supervise.json whose
// postgres would not run with fsync on is a final refusal naming the file,
// before anything starts; the task starts the supervisor with no backsight
// command after it to check the running database.
func TestServerSuperviseRefusesFsyncOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"fsync=off after on", []string{"-c", "fsync=on", "-c", "fsync=off"}},
		{"no fsync setting", nil},
		{"-F", []string{"-c", "fsync=on", "-F"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := "bs-test-" + strconv.Itoa(os.Getpid()) + "-cli-fsync-" + strconv.Itoa(len(tc.args))
			n, sc := superviseHost(t, p)
			withInstance(t, n, "111", "111")
			sc.Postgres = append([]string{sc.Postgres[0], "-D", n.DataDir()}, tc.args...)
			raw, _ := json.Marshal(sc)
			if err := os.WriteFile(n.SuperviseFile(), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			done := runSupervise(t, p)
			st := superviseStatus(t, p, "refused")
			if !strings.Contains(st.Refusal, n.SuperviseFile()) || !strings.Contains(st.Refusal, "not fsync=on") {
				t.Errorf("refusal %q: want the fsync refusal naming %s", st.Refusal, n.SuperviseFile())
			}
			if got := testtool.Calls(t, sc.Postgres[0]); got != nil {
				t.Errorf("postgres ran %q with fsync not on", got)
			}
			stopSupervise(t, p, done)
		})
	}
}

// The guard's stray report goes to the supervisor's log (nobody reads a
// task's stderr), and the stray is stopped before postgres starts.
func TestServerSuperviseLogsTheStraysItStops(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: a fake /proc (TestStraysOnWindows covers Windows' scan)")
	p := "bs-test-" + strconv.Itoa(os.Getpid()) + "-cli-stray"
	n, sc := superviseHost(t, p)
	withInstance(t, n, "111", "111")
	pg := filepath.Join(n.Home(), ".pg0", "installation", "18.1.0", "bin", "postgres")
	writeProc(t, "4242", []string{pg, "-D", n.DataDir()}, "foo.scope", n.DataDir())
	writePidFile(t, n, "4242")
	tools := nativeTools.(*checkTools)
	tools.effect = func(path string, _ []string) {
		if filepath.Base(path) == "pg_ctl" {
			_ = os.RemoveAll(filepath.Join(nativeProc, "4242"))
		}
	}
	done := make(chan int, 1)
	var errOut string
	go func() {
		var code int
		code, _, errOut = run(t, "server", "supervise")
		done <- code
	}()
	superviseStatus(t, p, "running")
	stopSupervise(t, p, done)
	raw, _ := os.ReadFile(filepath.Join(sc.LogDir, p+"-supervisor.log"))
	report := "a postgres (pid 4242) runs on " + n.DataDir() + " outside " + p + "-db.service: stopping it, never adopting it"
	if !strings.Contains(string(raw), report) {
		t.Errorf("supervisor log lacks %q:\n%s", report, raw)
	}
	if strings.Contains(errOut, "pid 4242") {
		t.Errorf("the stray report went to stderr: %q", errOut)
	}
	if !slices.ContainsFunc(tools.calls, func(c string) bool { return filepath.Base(strings.Fields(c)[0]) == "pg_ctl" }) {
		t.Errorf("the stray was not stopped with pg_ctl: %q", tools.calls)
	}
}

// --backsight-home is BACKSIGHT_HOME for a task, which cannot set it.
func TestServerSuperviseTakesBacksightHome(t *testing.T) {
	p := "bs-test-" + strconv.Itoa(os.Getpid()) + "-cli-home"
	n, _ := superviseHost(t, p)
	withInstance(t, n, "111", "")
	home := os.Getenv("BACKSIGHT_HOME")
	t.Setenv("BACKSIGHT_HOME", filepath.Join(t.TempDir(), "elsewhere"))
	done := runSupervise(t, p, "--backsight-home", home)
	superviseStatus(t, p, "refused")
	stopSupervise(t, p, done)
}

func TestServerSuperviseRefusesAnotherProjectsJSON(t *testing.T) {
	p := "bs-test-" + strconv.Itoa(os.Getpid()) + "-cli-other"
	n, sc := superviseHost(t, p)
	sc.Project = "someone-else"
	raw, _ := json.Marshal(sc)
	if err := os.WriteFile(filepath.Join(n.Root(), "supervise.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := run(t, "server", "supervise")
	if code != output.Blocked || !strings.Contains(errOut, `supervises project "someone-else", config.toml names "`+p+`"`) {
		t.Errorf("exit %d, stderr %q: want a refusal naming both projects", code, errOut)
	}
	if got := testtool.Calls(t, sc.Postgres[0]); got != nil {
		t.Errorf("postgres ran %q", got)
	}
}
