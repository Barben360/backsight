package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
)

func atLeast(v, minimum string) bool { return v >= minimum } // enough for these fixed strings

// healthy is a machine where every check passes.
func healthy() Probes {
	return Probes{
		GOOS: "linux", GOARCH: "amd64",
		Docker:    func(context.Context) (string, error) { return "28.1.1", nil },
		Compose:   func(context.Context) (string, error) { return "v2.39.1", nil },
		PortFree:  func(int) bool { return true },
		FreeBytes: func() (uint64, error) { return 50 << 30, nil },
		Claude:    func(context.Context) (string, error) { return "2.1.400", nil },
		MinClaude: "2.1.309", AtLeast: atLeast,
		APIPort: 8888, UIPort: 9999,
	}
}

func byID(cs []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range cs {
		m[c.ID] = c
	}
	return m
}

func TestPreflightPassesOnAHealthyMachine(t *testing.T) {
	cs := Preflight(context.Background(), healthy())
	if class, failed := Verdict(cs); class != "" {
		t.Fatalf("class %q, failed %+v", class, failed)
	}
	for _, id := range []string{"platform", "docker", "compose", "api_port", "ui_port", "disk", "claude", "gpu"} {
		if _, ok := byID(cs)[id]; !ok {
			t.Errorf("no %s check", id)
		}
	}
}

func TestPreflightClassifiesEachFailure(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Probes)
		id     string
		class  string
		action string
	}{
		{"no docker", func(p *Probes) {
			p.Docker = func(context.Context) (string, error) { return "", fmt.Errorf("docker: %w", exec.ErrNotFound) }
		}, "docker", "needs_user", "install"},
		{"docker group", func(p *Probes) {
			p.Docker = func(context.Context) (string, error) {
				return "", errors.New("permission denied while trying to connect to the Docker daemon socket")
			}
		}, "docker", "needs_user", "relogin"},
		{"daemon down", func(p *Probes) {
			p.Docker = func(context.Context) (string, error) { return "", errors.New("Cannot connect to the Docker daemon") }
		}, "docker", "blocked", ""},
		{"compose v1", func(p *Probes) { p.Compose = func(context.Context) (string, error) { return "1.29.2", nil } }, "compose", "needs_user", "install"},
		{"port taken", func(p *Probes) { p.PortFree = func(n int) bool { return n != 8888 } }, "api_port", "blocked", ""},
		{"small disk", func(p *Probes) { p.FreeBytes = func() (uint64, error) { return 2 << 30, nil } }, "disk", "blocked", ""},
		{"no claude", func(p *Probes) {
			p.Claude = func(context.Context) (string, error) { return "", errors.New("not found") }
		}, "claude", "needs_user", "install"},
		{"old claude", func(p *Probes) { p.Claude = func(context.Context) (string, error) { return "2.0.1", nil } }, "claude", "needs_user", "run_as_user"},
		{"no image", func(p *Probes) { p.GOARCH = "riscv64" }, "platform", "blocked", ""},
	}
	for _, c := range cases {
		p := healthy()
		c.mutate(&p)
		cs := Preflight(context.Background(), p)
		got := byID(cs)[c.id]
		if got.Status != Fail || got.Class != c.class || got.Fix == "" {
			t.Errorf("%s: %+v", c.name, got)
			continue
		}
		if (got.Action == nil) != (c.action == "") || (got.Action != nil && got.Action.Kind != c.action) {
			t.Errorf("%s: action %+v, want %q", c.name, got.Action, c.action)
		}
		if class, _ := Verdict(cs); class != c.class {
			t.Errorf("%s: verdict %q", c.name, class)
		}
	}
}

func TestAdoptedServerOwnsItsPortsAndDisk(t *testing.T) {
	p := healthy()
	p.PortFree = func(int) bool { return false }
	p.FreeBytes = func() (uint64, error) { return 3 << 30, nil }
	p.OwnPorts, p.ServerInstalled = []int{8888, 9999}, true
	cs := Preflight(context.Background(), p)
	if class, failed := Verdict(cs); class != "" {
		t.Fatalf("adoption blocked: %+v", failed)
	}
	if byID(cs)["disk"].Status != Warn {
		t.Errorf("low disk on an installed server must warn: %+v", byID(cs)["disk"])
	}
	// A port backsight's server does not publish stays another program's.
	p.UIPort = 9998
	cs = Preflight(context.Background(), p)
	if c := byID(cs)["ui_port"]; c.Status != Fail || !strings.Contains(c.Fix, "--answer ui_port=") {
		t.Errorf("a taken port backsight does not hold passed: %+v", c)
	}
}

func TestRemoteServerSkipsTheDockerChecks(t *testing.T) {
	p := healthy()
	p.Remote = true
	p.Docker = func(context.Context) (string, error) { t.Fatal("docker probed for a remote server"); return "", nil }
	cs := Preflight(context.Background(), p)
	for _, id := range []string{"api_port", "ui_port", "compose", "disk"} {
		if _, ok := byID(cs)[id]; ok {
			t.Errorf("%s checked for a remote server", id)
		}
	}
	if byID(cs)["docker"].Status != Info {
		t.Errorf("%+v", byID(cs)["docker"])
	}
}

func TestNeedsUserWinsTheVerdict(t *testing.T) {
	p := healthy()
	p.Docker = func(context.Context) (string, error) { return "", errors.New("Cannot connect") }
	p.Claude = func(context.Context) (string, error) { return "", errors.New("missing") }
	class, failed := Verdict(Preflight(context.Background(), p))
	if class != "needs_user" || len(failed) != 2 {
		t.Errorf("class %q failed %d", class, len(failed))
	}
}

func TestRootlessAndGPUAreReported(t *testing.T) {
	p := healthy()
	p.Docker = func(context.Context) (string, error) { return "28.1.1 [name=rootless]", nil }
	p.GPUs = []string{"AMD Radeon"}
	cs := byID(Preflight(context.Background(), p))
	if !strings.Contains(cs["docker"].Summary, "rootless") || !strings.Contains(cs["gpu"].Summary, "AMD Radeon") {
		t.Errorf("%+v %+v", cs["docker"], cs["gpu"])
	}
}

func TestNeedsUserWinsWhateverTheOrder(t *testing.T) {
	p := healthy()
	p.Docker = func(context.Context) (string, error) { return "", fmt.Errorf("x: %w", exec.ErrNotFound) } // needs_user, first
	p.FreeBytes = func() (uint64, error) { return 1 << 30, nil }                                          // blocked, later
	if class, failed := Verdict(Preflight(context.Background(), p)); class != "needs_user" || len(failed) != 2 {
		t.Errorf("class %q, %d failed", class, len(failed))
	}
}

// A machine without a systemd user session (a container, an su shell)
// cannot load the timers: the check fails as the user's choice (exit 10)
// naming scheduler=off and what stops without it; scheduler=off skips it.
func TestNoUserSessionIsTheUsersChoice(t *testing.T) {
	p := healthy()
	p.UserScheduler = func(context.Context) error { return errors.New("Failed to connect to bus: No medium found") }
	cs := Preflight(context.Background(), p)
	c := byID(cs)["scheduler"]
	if c.Status != Fail || c.Class != "needs_user" || !strings.Contains(c.Summary, "No medium found") {
		t.Fatalf("%+v", c)
	}
	for _, s := range []string{"scheduler=off", "jobs tick", "jobs maintain", "backup create", "login session"} {
		if !strings.Contains(c.Fix, s) {
			t.Errorf("fix lacks %q: %s", s, c.Fix)
		}
	}
	if c.Action == nil || !strings.Contains(c.Action.Cmd, "--answer scheduler=off") {
		t.Errorf("action %+v", c.Action)
	}
	if class, _ := Verdict(cs); class != "needs_user" {
		t.Errorf("verdict %q", class)
	}
	p.SchedulerOff = true
	if _, ok := byID(Preflight(context.Background(), p))["scheduler"]; ok {
		t.Error("scheduler=off still checks the user session")
	}
	p.SchedulerOff, p.UserScheduler = false, func(context.Context) error { return nil }
	if c := byID(Preflight(context.Background(), p))["scheduler"]; c.Status != OK {
		t.Errorf("reachable: %+v", c)
	}
}

// A user session that does not answer in time is retried from a login
// session (blocked), scheduler=off only the last resort.
func TestSlowUserSessionIsRetried(t *testing.T) {
	p := healthy()
	p.UserScheduler = func(context.Context) error { return fmt.Errorf("probe: %w", ErrSessionSlow) }
	c := byID(Preflight(context.Background(), p))["scheduler"]
	if c.Status != Fail || c.Class != "blocked" || c.Action != nil || !strings.HasPrefix(c.Fix, "retry, from a login session") || !strings.Contains(c.Fix, "jobs tick") {
		t.Errorf("%+v", c)
	}
}

// A server-only host has no Claude Code: its checks and lines are not
// setup's business there, even when claude is missing.
func TestPreflightServerOnlySkipsClaudeCode(t *testing.T) {
	p := healthy()
	p.ServerOnly = true
	p.Claude = func(context.Context) (string, error) { return "", fmt.Errorf("claude: %w", exec.ErrNotFound) }
	p.Retention, p.Transcript, p.Reference = "Claude Code keeps transcripts 30 days", "3 transcripts", 2
	cs := Preflight(context.Background(), p)
	if class, failed := Verdict(cs); class != "" {
		t.Fatalf("class %q, failed %+v", class, failed)
	}
	got := byID(cs)
	for _, id := range []string{"claude", "retention", "transcripts", "reference"} {
		if _, ok := got[id]; ok {
			t.Errorf("server-only host has a %s check: %+v", id, got[id])
		}
	}
	for _, id := range []string{"platform", "docker", "compose", "api_port", "ui_port", "disk", "gpu"} {
		if _, ok := got[id]; !ok {
			t.Errorf("no %s check", id)
		}
	}
}

// nativeHealthy is a machine where the native runtime's checks pass.
func nativeHealthy() Probes {
	p := healthy()
	p.Runtime = "native"
	p.Linger = func() (string, error) { return "yes", nil }
	p.UserManager = func(context.Context) error { return nil }
	return p
}

// The native runtime checks its own needs: Linux x86_64, the user
// manager and lingering, the API port and 3.5 GB; Docker and the UI port
// are not its.
func TestPreflightOnNative(t *testing.T) {
	p := nativeHealthy()
	p.Docker = func(context.Context) (string, error) { return "", fmt.Errorf("docker: %w", exec.ErrNotFound) }
	p.Compose = func(context.Context) (string, error) { return "", errors.New("no compose") }
	p.PortFree = func(port int) bool { return port != p.UIPort }
	p.FreeBytes = func() (uint64, error) { return NativeDiskMB << 20, nil }
	cs := Preflight(context.Background(), p)
	if class, failed := Verdict(cs); class != "" {
		t.Fatalf("class %q, failed %+v", class, failed)
	}
	m := byID(cs)
	for _, id := range []string{"docker", "compose"} {
		if m[id].Status != Info || m[id].Summary != "not used by the native runtime" {
			t.Errorf("%s: %+v", id, m[id])
		}
	}
	if _, ok := m["ui_port"]; ok {
		t.Error("the UI port is checked on native")
	}
	if _, ok := m["scheduler"]; ok {
		t.Error("the scheduler check runs beside the systemd one")
	}
	for _, id := range []string{"platform", "systemd", "linger", "api_port", "disk"} {
		if m[id].Status != OK {
			t.Errorf("%s: %+v", id, m[id])
		}
	}
	// One MB under the floor fails, as on Docker's 6 GB.
	p.FreeBytes = func() (uint64, error) { return NativeDiskMB<<20 - 1<<20, nil }
	if d := byID(Preflight(context.Background(), p))["disk"]; d.Status != Fail || d.Class != "blocked" || !strings.Contains(d.Summary, "3.5 GB") {
		t.Errorf("disk %+v", d)
	}
}

func TestPreflightOnNativeRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*Probes)
		id     string
		class  string
		want   string // in the summary or fix
		action string // the action's command
	}{
		{"arm64", func(p *Probes) { p.GOARCH = "arm64" }, "platform", "blocked", config.NativeRefusal, ""},
		{"darwin", func(p *Probes) { p.GOOS = "darwin" }, "platform", "blocked", config.NativeRefusal, ""},
		{"linger off", func(p *Probes) { p.Linger = func() (string, error) { return "no", nil } }, "linger", "needs_user", "loginctl enable-linger", "loginctl enable-linger"},
		{"linger unknown", func(p *Probes) { p.Linger = func() (string, error) { return "", errors.New("loginctl: not found") } }, "linger", "needs_user", "loginctl enable-linger", "loginctl enable-linger"},
		{"no user manager", func(p *Probes) {
			p.UserManager = func(context.Context) error { return errors.New("Failed to connect to bus") }
		}, "systemd", "needs_user", NoUserBus, ""},
		{"slow user manager", func(p *Probes) { p.UserManager = func(context.Context) error { return ErrSessionSlow } }, "systemd", "blocked", "retry", ""},
		{"api port taken", func(p *Probes) { p.PortFree = func(int) bool { return false } }, "api_port", "blocked", "8888", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := nativeHealthy()
			c.mutate(&p)
			got := byID(Preflight(context.Background(), p))[c.id]
			if got.Status != Fail || got.Class != c.class || !strings.Contains(got.Summary+" "+got.Fix, c.want) {
				t.Errorf("%+v", got)
			}
			if c.action != "" && (got.Action == nil || got.Action.Cmd != c.action || got.Action.Kind != "run_as_user") {
				t.Errorf("action %+v", got.Action)
			}
		})
	}
}

// A Docker machine's pre-flight is the one it always had: no systemd or
// linger check, even with the native probes set.
func TestPreflightOnDockerIgnoresTheNativeProbes(t *testing.T) {
	p := healthy()
	p.Linger = func() (string, error) { return "no", nil }
	p.UserManager = func(context.Context) error { return errors.New("no bus") }
	m := byID(Preflight(context.Background(), p))
	if _, ok := m["linger"]; ok {
		t.Error("linger checked on Docker")
	}
	if _, ok := m["systemd"]; ok {
		t.Error("systemd checked on Docker")
	}
}

// windowsHealthy is a Windows x86_64 machine where the native runtime's
// checks pass (decision 0080).
func windowsHealthy() Probes {
	p := healthy()
	p.GOOS, p.Runtime = "windows", "native"
	p.VenvRoot = `C:\Users\jo\AppData\Local\backsight\data\server\native\venv\1.1.0-14d79a0a7914`
	p.NativePaths = []string{`C:\Users\jo\AppData\Local\backsight\data`, `C:\Users\jo\AppData\Local\backsight\data\server\native`,
		`C:\Users\jo\AppData\Local\backsight\data\bin\current.exe`}
	p.Conhost = func(context.Context) error { return nil }
	p.Task = func(context.Context) error { return nil }
	p.Docker = func(context.Context) (string, error) { return "", fmt.Errorf("docker: %w", exec.ErrNotFound) }
	p.Compose = func(context.Context) (string, error) { return "", errors.New("no compose") }
	return p
}

// Native on Windows checks the disk, the path bound, the % rule, the API
// port, conhost --headless and Task Scheduler, in that order; Docker,
// systemd and linger are not its.
func TestPreflightOnWindowsNative(t *testing.T) {
	p := windowsHealthy()
	cs := Preflight(context.Background(), p)
	if class, failed := Verdict(cs); class != "" {
		t.Fatalf("class %q, failed %+v", class, failed)
	}
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	want := "platform docker compose disk path-length path-chars api_port conhost task claude gpu"
	if strings.Join(ids, " ") != want {
		t.Errorf("checks %v\nwant %s", ids, want)
	}
	m := byID(cs)
	for _, id := range []string{"platform", "disk", "path-length", "path-chars", "api_port", "conhost", "task"} {
		if m[id].Status != OK {
			t.Errorf("%s: %+v", id, m[id])
		}
	}
	if m["platform"].Summary != "windows/amd64" {
		t.Errorf("platform %+v", m["platform"])
	}
	if StartOnSession(cs) {
		t.Error("an accepted task sets start_on_session")
	}
	// A refused probe fails its check.
	p.Conhost = func(context.Context) error { return errors.New("exit status 1") }
	if c := byID(Preflight(context.Background(), p))["conhost"]; c.Status != Fail || c.Class != "blocked" || !strings.Contains(c.Summary, "exit status 1") {
		t.Errorf("conhost refused: %+v", c)
	}
	p.Conhost = nil // never probed: not taken for a pass
	if c := byID(Preflight(context.Background(), p))["conhost"]; c.Status != Fail {
		t.Errorf("conhost unprobed: %+v", c)
	}
	// windows/arm64 stays refused.
	p = windowsHealthy()
	p.GOARCH = "arm64"
	if c := byID(Preflight(context.Background(), p))["platform"]; c.Status != Fail || !strings.Contains(c.Summary, config.NativeRefusal) {
		t.Errorf("windows/arm64: %+v", c)
	}
}

// The venv root plus the 140 characters below it must stay within 250
// (E34 ruling 9): a root making 251 fails, naming BACKSIGHT_HOME; 250
// passes. The length counts UTF-16 units, as Windows' limit does.
func TestPreflightPathLength(t *testing.T) {
	base := `C:\Users\jé\venv\` // é: one unit, two bytes
	for _, c := range []struct {
		total int
		ok    bool
	}{{250, true}, {251, false}, {200, true}, {300, false}} {
		p := windowsHealthy()
		p.VenvRoot = base + strings.Repeat("x", c.total-VenvDepth-len([]rune(base)))
		if got := VenvPathLength(p.VenvRoot) + VenvDepth; got != c.total {
			t.Fatalf("fixture: %d, want %d", got, c.total)
		}
		got := byID(Preflight(context.Background(), p))["path-length"]
		if c.ok {
			if got.Status != OK {
				t.Errorf("%d: %+v", c.total, got)
			}
			continue
		}
		if got.Status != Fail || got.Class != "blocked" || !strings.Contains(got.Fix, "BACKSIGHT_HOME") || !strings.Contains(got.Summary, p.VenvRoot) {
			t.Errorf("%d: %+v", c.total, got)
		}
	}
	if VenvPathLength(`C:\é`) != 4 || VenvPathLength("C:\\\U0001F600") != 5 { // a surrogate pair counts 2
		t.Errorf("UTF-16 length: %d %d", VenvPathLength(`C:\é`), VenvPathLength("C:\\\U0001F600"))
	}
}

// A % in the data root, the native root or the shim's path is refused:
// Task Scheduler expands %VAR% in the task's command line (E34 ruling
// 5). &, spaces and non-ASCII are fine.
func TestPreflightRefusesAPercentInThePath(t *testing.T) {
	for i := range 3 {
		p := windowsHealthy()
		p.NativePaths[i] = strings.Replace(p.NativePaths[i], `\jo\`, `\a%USERNAME%b\`, 1)
		got := byID(Preflight(context.Background(), p))["path-chars"]
		if got.Status != Fail || got.Class != "blocked" || !strings.Contains(got.Summary, `a%USERNAME%b`) || !strings.Contains(got.Fix, "BACKSIGHT_HOME") {
			t.Errorf("path %d: %+v", i, got)
		}
	}
	p := windowsHealthy()
	for i := range p.NativePaths {
		p.NativePaths[i] = strings.Replace(p.NativePaths[i], `\jo\`, `\Jé & Co\`, 1)
	}
	if got := byID(Preflight(context.Background(), p))["path-chars"]; got.Status != OK {
		t.Errorf("spaces, & and é: %+v", got)
	}
}

// Task Scheduler refusing the probe task is a warning, not a failure:
// setup goes on with the fallback (server start by hand, and the
// SessionStart hook starting the server), and sets start_on_session.
func TestPreflightTaskRefusedSetsStartOnSession(t *testing.T) {
	p := windowsHealthy()
	p.Task = func(context.Context) error { return errors.New("ERROR: Access is denied.") }
	cs := Preflight(context.Background(), p)
	got := byID(cs)["task"]
	if got.Status != Warn || !strings.Contains(got.Summary, "Access is denied") || got.Fix != TaskFallback {
		t.Errorf("task: %+v", got)
	}
	for _, s := range []string{"backsight server start", "start_on_session"} {
		if !strings.Contains(TaskFallback, s) {
			t.Errorf("the fallback does not name %q: %s", s, TaskFallback)
		}
	}
	if class, failed := Verdict(cs); class != "" {
		t.Errorf("a refused task stops setup: %q %+v", class, failed)
	}
	if !StartOnSession(cs) {
		t.Error("a refused task does not set start_on_session")
	}
	// Linux never sets it, whatever its checks.
	if StartOnSession(Preflight(context.Background(), nativeHealthy())) {
		t.Error("linux sets start_on_session")
	}
}

// The probe task is created from the rendered XML and always deleted:
// after a refused /Create too (a half-created task must not stay), and
// its XML file with it.
func TestProbeTaskAlwaysDeletes(t *testing.T) {
	dir := t.TempDir()
	for _, refuse := range []bool{false, true} {
		var calls [][]string
		var xml []byte
		run := func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			if args[0] == "/Create" {
				xml, _ = os.ReadFile(args[slices.Index(args, "/XML")+1])
				if refuse {
					return []byte("ERROR: Access is denied."), errors.New("exit status 1")
				}
			}
			return nil, nil
		}
		err := ProbeTask(context.Background(), run, "S-1-5-21-1-2-3-1001", "bs-test-probe", `C:\Jé & Co\bin\current.exe`, dir)
		if refuse != (err != nil) {
			t.Fatalf("refuse %v: %v", refuse, err)
		}
		if refuse && !strings.Contains(err.Error(), "Access is denied") {
			t.Errorf("the refusal does not say why: %v", err)
		}
		want := [][]string{{"schtasks", "/Create", "/XML", "", "/TN", `\bs-test-probe\probe`}, {"schtasks", "/Delete", "/F", "/TN", `\bs-test-probe\probe`}}
		if len(calls) != 2 || len(calls[0]) != 6 {
			t.Fatalf("calls %q", calls)
		}
		calls[0][3] = ""
		if fmt.Sprint(calls) != fmt.Sprint(want) {
			t.Errorf("refuse %v: calls %q\nwant %q", refuse, calls, want)
		}
		if len(xml) < 2 || xml[0] != 0xFF || xml[1] != 0xFE {
			t.Errorf("the probe's XML is not the rendered UTF-16 XML: % x", xml[:min(len(xml), 8)])
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Errorf("left in the temporary directory: %v", left)
		}
	}
	// A path Task Scheduler would expand never reaches schtasks.
	called := false
	err := ProbeTask(context.Background(), func(context.Context, string, ...string) ([]byte, error) { called = true; return nil, nil },
		"S-1-5-21-1-2-3-1001", "bs-test-probe", `C:\a%USERNAME%b\current.exe`, dir)
	if err == nil || called {
		t.Errorf("a %% path: %v, schtasks called %v", err, called)
	}
}
