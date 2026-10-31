//go:build windows

package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/supervise"
	"github.com/Barben360/backsight/internal/testtool"
)

// TestMain lets the test binary be a testtool fake (postgres, the API)
// under TestTaskStartStopForReal's supervisor. No test asks a real
// supervisor's pipe by default (a developer's own server would answer,
// and stop): TestTaskStartStopForReal sets Native.Ask for its project.
func TestMain(m *testing.M) {
	testtool.Main()
	superviseAsk = func(_ context.Context, project, op string) (supervise.Status, error) {
		return supervise.Status{}, errors.New("test reached the supervisor of " + project + " (" + op + ") without a fake")
	}
	os.Exit(m.Run())
}

// commandLineToArgv is how a Windows program splits its command line.
func commandLineToArgv(t *testing.T, line string) []string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(line)
	if err != nil {
		t.Fatal(err)
	}
	var n int32
	argv, err := windows.CommandLineToArgv(p, &n)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(argv)))) }()
	out := make([]string, n)
	for i := range out {
		out[i] = windows.UTF16PtrToString(&argv[i][0])
	}
	return out
}

// A profile path with a space, & and non-ASCII: task.xml is odd-path.xml,
// and conhost's command line, read back from the XML and split as Windows
// does, gives the exact shim and BACKSIGHT_HOME.
func TestTaskXMLCarriesAnyPathExactly(t *testing.T) {
	withSID(t)
	windowsUser(t, `C:\Users\Jé & Co`)
	home := `C:\Users\Jé & Co\AppData\Local\backsight home`
	n := windowsNative(t, home, home+`\data`, home+`\state`)
	f, err := taskHost{n: n}.Files(8890, n.Version())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "task-golden/odd-path.xml", f["task.xml"])
	m := regexp.MustCompile(`<Arguments>(.*)</Arguments>`).FindStringSubmatch(decodeUTF16(t, f["task.xml"]))
	if m == nil {
		t.Fatal("task.xml has no arguments")
	}
	args := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(m[1])
	got := commandLineToArgv(t, `conhost.exe `+args)
	want := []string{"conhost.exe", "--headless", home + `\data\bin\current.exe`, "server", "supervise", "--backsight-home", home}
	if !slices.Equal(got, want) {
		t.Errorf("conhost's argv\n%q\nwant\n%q", got, want)
	}
}

// execSys runs the real schtasks.
type execSys struct{}

func (execSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// noTaskSys stands for a Task Scheduler that never runs the task (no
// console logon): every change succeeds and nothing is registered. A
// really registered task would not do: StartWhenAvailable runs its
// missed 5-minute start as soon as it is registered (seen on CI's runner).
type noTaskSys struct{}

func (noTaskSys) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if slices.Contains(args, "/Query") {
		return nil, errors.New("exit status 1")
	}
	return nil, nil
}

// buildBacksight builds the backsight binary as the shim, testhooks on:
// its supervisor's guard skips the stray scan (Task 10's).
func buildBacksight(t *testing.T, out string) {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		gobin = filepath.Join(os.Getenv("GOROOT"), "bin", "go.exe")
	}
	start := time.Now()
	cmd := exec.Command(gobin, "build", "-tags", "testhooks", "-o", out, "github.com/Barben360/backsight/cmd/backsight")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building backsight: %v\n%s", err, raw)
	}
	t.Logf("built backsight in %s", time.Since(start).Round(time.Second))
}

// alive: pid runs.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

func pidsIn(path string) []int {
	raw, _ := os.ReadFile(path)
	var out []int
	for _, f := range strings.Fields(string(raw)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// The real thing, as bstest on the VM and on CI: the task registered by
// schtasks runs conhost --headless backsight server supervise (or, with
// no console logon, the same command detached), whose supervisor runs
// testtool fakes as postgres and the API; Stop leaves no process and the
// task disabled; Remove leaves no task.
func TestTaskStartStopForReal(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "current.exe")
	buildBacksight(t, shim)
	for _, path := range []string{"task", "detached"} {
		t.Run(path, func(t *testing.T) { taskForReal(t, shim, path == "detached") })
	}
}

func taskForReal(t *testing.T, built string, forceDetached bool) {
	ctx := context.Background()
	home := t.TempDir()
	if strings.Contains(home, "%") {
		t.Skipf("the temporary directory %s holds a %%: a task cannot carry it", home)
	}
	project := "bs-test-" + strconv.Itoa(os.Getpid()) + map[bool]string{false: "-task", true: "-detached"}[forceDetached]
	d := paths.Dirs{Data: filepath.Join(home, "data"), Config: filepath.Join(home, "config"), State: filepath.Join(home, "state"), Bin: filepath.Join(home, "bin")}
	for _, dir := range []string{d.Config, filepath.Join(d.Data, "bin")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(d.ConfigFile(), []byte("[server]\nproject = \""+project+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(d, home)
	cfg.Server.Project = project
	var notes []string
	n := Native{Cfg: cfg, Dirs: d, Env: map[string]string{"BACKSIGHT_HOME": home}, Sys: execSys{}, GOOS: "windows",
		Ask: supervise.Ask, Note: func(s string) { notes = append(notes, s) }}
	if forceDetached {
		n.Sys = noTaskSys{}
	}
	h := taskHost{n: n}
	if err := copyFileForTest(built, n.shim()); err != nil {
		t.Fatal(err)
	}

	fakes := filepath.Join(home, "fakes")
	file := func(name string) string { return filepath.Join(fakes, name) }
	pg := testtool.Write(t, fakes, "postgres", testtool.Fake{Script: "pid " + file("pg.pids") + "\nwrite " + file("ready") + " up\nwaitfile " + file("pg-stop")})
	isReady := testtool.Write(t, fakes, "pg_isready", testtool.Fake{Script: "nofile " + file("ready") + " 2"})
	ctl := testtool.Write(t, fakes, "pg_ctl", testtool.Fake{Script: "write " + file("pg-stop") + " stop"})
	api := testtool.Write(t, fakes, "hindsight-api", testtool.Fake{Script: "pid " + file("api.pids") + "\nwaitfile " + file("never")})

	files, err := h.Files(5432, "1.1.0-abcdefabcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(n.Root(), 0o700); err != nil {
		t.Fatal(err)
	}
	sc := supervise.Config{Project: project, DataDir: file("db"), Postgres: []string{pg, "-D", file("db")}, PGIsReady: []string{isReady},
		PGCtlStop: []string{ctl, "stop"}, API: []string{api}, Env: n.windowsEnv(fakes), EnvFile: n.EnvFile(), LogDir: n.LogDir(), Venv: "1.1.0-abcdefabcdef"}
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"task.xml": files["task.xml"], "supervise.json": raw, "server.env": []byte("K='v'\n")} {
		if err := os.WriteFile(filepath.Join(n.Root(), name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var supPID int
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_ = h.stopSupervisor(cctx)
		_, _ = execSys{}.Run(cctx, "schtasks", "/Delete", "/F", "/TN", n.TaskName())
		for _, pid := range append(append(pidsIn(file("pg.pids")), pidsIn(file("api.pids"))...), supPID) {
			if pid != 0 && alive(pid) {
				t.Errorf("pid %d still runs at the end", pid)
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	})

	if err := h.StartDB(ctx, false); err != nil {
		t.Fatalf("StartDB: %v\nsupervisor log:\n%s", err, supervisorLog(h))
	}
	how := "the task"
	if slices.Contains(notes, NoLogonNote) {
		how = "the detached path"
	}
	t.Logf("the supervisor came up through %s", how)
	switch {
	case forceDetached && how != "the detached path":
		t.Errorf("with Task Scheduler running nothing, notes %q: the detached path did not run", notes)
	case !forceDetached && os.Getenv("BACKSIGHT_TEST_CONSOLE_LOGON") == "1" && how != "the task":
		t.Errorf("with a console logon (BACKSIGHT_TEST_CONSOLE_LOGON=1) the task did not bring the supervisor up: notes %q", notes)
	case !forceDetached && os.Getenv("BACKSIGHT_TEST_CONSOLE_LOGON") != "1":
		t.Log("BACKSIGHT_TEST_CONSOLE_LOGON is not 1: which path came up is not checked")
	}
	if err := h.StartAPI(ctx, false); err != nil {
		t.Fatal(err)
	}
	var st supervise.Status
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, err = supervise.Ask(ctx, project, "status")
		if err == nil && st.DB == "running" && st.API == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v %v, want both running\nsupervisor log:\n%s", st, err, supervisorLog(h))
		}
		time.Sleep(200 * time.Millisecond)
	}
	supPID = st.PID
	if states, err := h.States(ctx); err != nil || states["db"] != "active" || states["hindsight"] != "active" {
		t.Errorf("States = %v %v", states, err)
	}
	// "running" is the start's: a fake writes its pid once it runs.
	var children []int
	for end := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if children = append(pidsIn(file("pg.pids")), pidsIn(file("api.pids"))...); len(children) == 2 || time.Now().After(end) {
			break
		}
	}
	if len(children) != 2 {
		t.Fatalf("children %v, want postgres' and the API's", children)
	}
	if !h.Owns(children[0]) || h.Owns(os.Getpid()) {
		t.Errorf("Owns: postgres %v, this test %v: want the job's processes only", h.Owns(children[0]), h.Owns(os.Getpid()))
	}
	if e, err := h.Engine(ctx); !forceDetached && (err != nil || e.Version != "Task Scheduler") {
		t.Errorf("Engine = %+v %v", e, err)
	}

	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v\nsupervisor log:\n%s", err, supervisorLog(h))
	}
	for _, pid := range append(children, supPID) {
		for end := time.Now().Add(5 * time.Second); alive(pid) && time.Now().Before(end); {
			time.Sleep(100 * time.Millisecond)
		}
		if alive(pid) {
			t.Errorf("pid %d still runs after Stop", pid)
		}
	}
	if _, err := supervise.Ask(ctx, project, "status"); !errors.Is(err, supervise.ErrNotRunning) {
		t.Errorf("the pipe after Stop: %v, want it gone", err)
	}
	sys := execSys{}
	if !forceDetached {
		q, err := sys.Run(ctx, "schtasks", "/Query", "/TN", n.TaskName(), "/XML")
		if err != nil || !regexp.MustCompile(`(?s)<Settings>.*<Enabled>false</Enabled>.*</Settings>`).Match(q) {
			t.Errorf("the task after Stop is not disabled (%v):\n%s", err, q)
		}
	}
	log := supervisorLog(h)
	if !strings.Contains(log, "api stopped") || strings.Contains(log, "no console to interrupt it") {
		t.Errorf("the API was not stopped through its console's CTRL_BREAK:\n%s", log)
	}

	if err := h.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.Run(ctx, "schtasks", "/Query", "/TN", n.TaskName()); err == nil {
		t.Errorf("the task %s stays after Remove", n.TaskName())
	}
}

func supervisorLog(h taskHost) string {
	raw, _ := os.ReadFile(h.logFile("supervisor"))
	return string(raw)
}

func copyFileForTest(from, to string) error {
	raw, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	return os.WriteFile(to, raw, 0o755)
}
