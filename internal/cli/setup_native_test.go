package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/testtool"
)

// nativeFresh is a machine with nothing of a server yet, where a native
// install will run: every server key already in secrets.toml (so the fake
// database knows its password), uv, Python, the venv and the models
// already fetched, a fake user manager and fake tools, and a fake
// Hindsight that answers only while the server unit runs. n is the
// runtime the install will build.
func nativeFresh(t *testing.T, home string) (n server.Native, sys *nativeSysFake, port int) {
	t.Helper()
	d := pathsOf(home)
	withLLMKey(t, home)
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { config.GenerateMissing(s); return nil }); err != nil {
		t.Fatal(err)
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil {
		t.Fatal(err)
	}
	port = fakeHindsight(t)
	cfg := config.Default(d, home)
	cfg.Server.Runtime, cfg.Server.URL = "native", "http://127.0.0.1:"+strconv.Itoa(port)
	n = server.Native{Cfg: cfg, Dirs: d}
	for path, content := range map[string]string{
		n.UV():        "uv",
		n.PythonBin(): "python",
		filepath.Join(n.VenvDir(n.Version()), ".backsight-complete"): "",
		filepath.Join(n.HF(), ".backsight-models"):                   nativelock.Hash12() + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tools := &nativeDBTools{n: n, pw: sec.DBPassword, rows: -1, nextID: "333"}
	apiDown := &atomic.Bool{}
	apiDown.Store(true) // nothing runs yet
	sys = &nativeSysFake{db: tools, apiDown: apiDown}
	oldSys, oldTools, oldProc, oldPoll := nativeSys, nativeTools, nativeProc, healthPoll
	nativeSys, nativeTools, nativeProc, healthPoll = sys, tools, t.TempDir(), time.Millisecond
	t.Cleanup(func() { nativeSys, nativeTools, nativeProc, healthPoll = oldSys, oldTools, oldProc, oldPoll })
	fakeHook = func(*http.Request) {
		if apiDown.Load() {
			panic(http.ErrAbortHandler) // the connection drops: nothing listens
		}
	}
	return n, sys, port
}

// unitPath is where the fake user manager keeps a unit of the native
// runtime (withSys's XDG_CONFIG_HOME).
func unitPath(home, unit string) string { return filepath.Join(home, "xdg", "systemd", "user", unit) }

func hasCall(calls []string, want string) bool { return slices.Contains(calls, want) }

// setup --runtime native installs a fresh memory on the native runtime:
// the answer goes to config.toml and setup's saved answers, and the server
// step builds the native runtime's files, creates its database (ruling
// R35) and starts both units.
func TestSetupRuntimeNativeWritesTheConfig(t *testing.T) {
	testtool.SkipWindows(t, "Task 10: native runtime")
	home, f := setupMachine(t)
	f.real["server install"] = true
	n, sys, port := nativeFresh(t, home)
	d := pathsOf(home)

	code, out, errOut := run(t, "setup", "--runtime", "native", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "backups=off",
		"--answer", "api_port="+strconv.Itoa(port), "--answer", "ui_port=19999", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(d.ConfigFile())
	if !strings.Contains(string(raw), "[server]\nruntime = \"native\"\n") {
		t.Errorf("config.toml lacks the runtime:\n%s", raw)
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil || cfg.RuntimeKind() != "native" {
		t.Fatalf("config %v: runtime %q", err, cfg.Server.Runtime)
	}
	if !hasStep(env.Data.Steps, "server", "done", "no web UI on the native runtime") {
		t.Errorf("server step:\n%s", out)
	}
	if env.Data.Answers["runtime"] != "native" {
		t.Errorf("answers %v", env.Data.Answers)
	}
	if rec, err := setup.Load(d.SetupFile()); err != nil || rec == nil || rec.Answers["runtime"] != "native" {
		t.Errorf("saved answers lack the runtime: %+v %v", rec, err)
	}
	if fi, err := os.Stat(n.EnvFile()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("server.env: %v", err)
	}
	for _, u := range []string{n.DBUnit(), n.ServerUnit()} {
		if !onDisk(unitPath(home, u)) {
			t.Errorf("%s not written", u)
		}
		if !hasCall(sys.calls, "systemctl --user enable --now "+u) {
			t.Errorf("%s not started: %q", u, sys.calls)
		}
	}
	if id, err := os.ReadFile(n.IdentityFile()); err != nil || strings.TrimSpace(string(id)) != "333" {
		t.Errorf("the install created no database: identity %q %v", id, err)
	}
	if onDisk(filepath.Join(d.Data, "server", "compose.yaml")) {
		t.Error("a native install wrote compose.yaml")
	}
}

// Only an install creates the native database (ruling R35): server start
// on a machine without one refuses and enables nothing; server install
// creates it and starts both units.
func TestNativeServerInstallCreatesWhatServerStartRefuses(t *testing.T) {
	testtool.SkipWindows(t, "Task 10: native runtime")
	home := isolate(t)
	withSys(t, "linux")
	n, sys, port := nativeFresh(t, home)
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	if err := os.WriteFile(d.ConfigFile(), []byte("[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\nruntime = \"native\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "server", "start")
	if code != output.Blocked || !strings.Contains(errOut, n.InstanceDir()) {
		t.Errorf("server start on no database: exit %d\n%s%s", code, out, errOut)
	}
	if onDisk(n.InstanceDir()) || onDisk(n.IdentityFile()) || slices.ContainsFunc(sys.calls, func(c string) bool { return strings.Contains(c, "enable") }) {
		t.Fatalf("server start created or enabled something: %q", sys.calls)
	}

	code, out, errOut = run(t, "server", "install")
	if code != output.OK {
		t.Fatalf("server install: exit %d\n%s%s", code, out, errOut)
	}
	if id, err := os.ReadFile(n.IdentityFile()); err != nil || strings.TrimSpace(string(id)) != "333" {
		t.Errorf("identity %q %v", id, err)
	}
	if !hasCall(sys.calls, "systemctl --user enable --now "+n.ServerUnit()) {
		t.Errorf("calls %q", sys.calls)
	}
	if !strings.Contains(out, "no web UI on the native runtime") || strings.Contains(out, "(UI ") {
		t.Errorf("summary:\n%s", out)
	}
}

// The db unit's ExecStartPre is the shim (server native-check): a fresh
// setup runs server install before the client step installs the shim, so
// server install puts it in place when it is missing (the native chain's
// first real run: 203/EXEC). A shim already there is the client's to
// point, and is left alone; a dangling one is installed again.
func TestNativeServerInstallInstallsTheShimItsDBUnitRuns(t *testing.T) {
	testtool.SkipWindows(t, "Task 10: native runtime")
	for _, c := range []struct {
		name               string
		existing, dangling bool
	}{{"no shim", false, false}, {"a shim already there", true, false}, {"a dangling shim", false, true}} {
		existing := c.existing
		t.Run(c.name, func(t *testing.T) {
			home := isolate(t)
			withSys(t, "linux")
			_, sys, port := nativeFresh(t, home)
			sys.needShim = true
			d := pathsOf(home)
			exe := filepath.Join(t.TempDir(), "backsight")
			if err := os.WriteFile(exe, []byte("#!/bin/sh\n# this build\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			old := selfExe
			selfExe = func() (string, error) { return exe, nil }
			t.Cleanup(func() { selfExe = old })
			shim := filepath.Join(d.Data, "bin", "current")
			other := filepath.Join(d.Data, "bin", "0.0.1-aaaaaaaaaaaa", "backsight")
			if err := os.Remove(shim); err != nil && !errors.Is(err, os.ErrNotExist) { // isolate's stand-in
				t.Fatal(err)
			}
			if existing {
				if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(other, []byte("#!/bin/sh\n# the client's build\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, shim); err != nil {
					t.Fatal(err)
				}
			}
			if c.dangling { // its binary removed by hand: systemd's 203/EXEC again
				if err := os.Symlink(filepath.Join(d.Data, "bin", "0.0.1-gone", "backsight"), shim); err != nil {
					t.Fatal(err)
				}
			}
			_ = os.MkdirAll(d.Config, 0o700)
			if err := os.WriteFile(d.ConfigFile(), []byte("[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\nruntime = \"native\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := run(t, "server", "install")
			if code != output.OK {
				t.Fatalf("server install: exit %d\n%s%s", code, out, errOut)
			}
			got, err := filepath.EvalSymlinks(shim)
			if err != nil || !executable(shim) {
				t.Fatalf("no executable shim at %s after server install: %v", shim, err)
			}
			want := other
			if !existing {
				raw, _ := os.ReadFile(got)
				if string(raw) != "#!/bin/sh\n# this build\n" {
					t.Errorf("the shim runs %s (%q), not this build", got, raw)
				}
				want = got
			}
			if got != want {
				t.Errorf("the shim points at %s, want %s", got, want)
			}
		})
	}
}

// Ruling R44: the database listens on loopback TCP only, so a long data
// directory (here a native root of 200+ characters) installs and starts:
// with a Unix socket under it, postgres would refuse (sun_path's 107
// bytes), as the native chain's first real run showed.
func TestNativeServerInstallStartsUnderALongDataDirectory(t *testing.T) {
	testtool.SkipWindows(t, "Task 10: native runtime")
	isolate(t)
	home := filepath.Join(t.TempDir(), strings.Repeat("h", 150))
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	testtool.SetHome(t, home)
	t.Setenv("BACKSIGHT_HOME", filepath.Join(home, "backsight"))
	withSys(t, "linux")
	n, sys, port := nativeFresh(t, home)
	sys.socketLimit = true
	if len(n.Root()) < 200 {
		t.Fatalf("native root %q is %d characters, want 200 or more", n.Root(), len(n.Root()))
	}
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	if err := os.WriteFile(d.ConfigFile(), []byte("[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\nruntime = \"native\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "server", "install")
	if code != output.OK {
		t.Fatalf("server install under a %d-character native root: exit %d\n%s%s", len(n.Root()), code, out, errOut)
	}
	unit, err := os.ReadFile(testUnitFile(n.DBUnit()))
	if err != nil {
		t.Fatalf("no db unit where the fake systemd reads it: %v", err)
	}
	if !strings.Contains(string(unit), "-c unix_socket_directories= ") || !strings.Contains(string(unit), "pg_isready -q -h 127.0.0.1 ") {
		t.Errorf("the db unit does not listen on TCP only:\n%s", unit)
	}
}

// The db unit's HOME is pg0's home/, so its native-check runs with the
// user's HOME, which server install takes from this process: on an
// install with no BACKSIGHT_HOME, native-check found no config.toml
// under home/ and the live switch to native failed at its target step.
func TestNativeServerInstallRunsNativeCheckWithTheUsersHome(t *testing.T) {
	testtool.SkipWindows(t, "Task 10: native runtime")
	home := isolate(t)
	withSys(t, "linux")
	n, _, port := nativeFresh(t, home)
	d := pathsOf(home)
	_ = os.MkdirAll(d.Config, 0o700)
	if err := os.WriteFile(d.ConfigFile(), []byte("[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\nruntime = \"native\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run(t, "server", "install"); code != output.OK {
		t.Fatalf("server install: exit %d\n%s%s", code, out, errOut)
	}
	unit, err := os.ReadFile(testUnitFile(n.DBUnit()))
	if err != nil {
		t.Fatalf("no db unit where the fake systemd reads it: %v", err)
	}
	want := "\nExecStartPre=/usr/bin/env HOME=" + home + " " + filepath.Join(d.Data, "bin", "current") + " server native-check\n"
	if !strings.Contains(string(unit), want) {
		t.Errorf("the db unit's native-check does not run with the user's HOME %s:\n%s", home, unit)
	}
}

// Without a UI, the summary says so instead of naming one.
func TestNativeInstallSummaryHasNoUI(t *testing.T) {
	docker := installSummary(installResult{URL: "http://127.0.0.1:8888", UI: "http://127.0.0.1:9999", Version: "1.1.0"})
	if !strings.Contains(docker, "(UI http://127.0.0.1:9999)") {
		t.Errorf("docker summary %q", docker)
	}
	native := installSummary(installResult{URL: "http://127.0.0.1:8888", Version: "1.1.0", native: true})
	if want := "Hindsight 1.1.0 healthy at http://127.0.0.1:8888 (no web UI on the native runtime)\n"; !strings.HasSuffix(native, want) || strings.Contains(native, "(UI ") {
		t.Errorf("native summary %q, want %q", native, want)
	}
}

// nativePreflight is a setup machine whose pre-flight sees lingering on,
// changed by probe.
func nativePreflight(t *testing.T, probe func(p *setup.Probes)) (string, *fakeSteps) {
	t.Helper()
	home, f := setupMachine(t)
	withLLMKey(t, home)
	inner := preflightProbes
	preflightProbes = func(ctx context.Context, s setupEnv, x map[string]string) setup.Probes {
		p := inner(ctx, s, x)
		if probe != nil {
			probe(&p)
		}
		return p
	}
	return home, f
}

var nativeAnswers = []string{"setup", "--runtime", "native", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json"}

// The native runtime's pre-flight: Linux x86_64 only, a reachable user
// manager, lingering on; Docker is not needed, the UI port not used, and
// 3.5 GB of disk is enough. Every refusal changes nothing.
func TestSetupRuntimeNativePreflight(t *testing.T) {
	for _, c := range []struct {
		name  string
		probe func(p *setup.Probes)
		noBus bool
		args  []string
		code  int
		want  []string // in the message
		not   string   // never in the message
		cmd   string   // the action's command
	}{
		{name: "arm64", probe: func(p *setup.Probes) { p.GOARCH = "arm64" }, code: output.Blocked, want: []string{"linux-amd64"}},
		{name: "linger off", probe: func(p *setup.Probes) { p.Linger = func() (string, error) { return "no", nil } },
			code: output.NeedsUser, want: []string{"loginctl enable-linger"}, cmd: "loginctl enable-linger"},
		{name: "no user manager", noBus: true, code: output.NeedsUser,
			want: []string{"no systemd user session is reachable from this shell", "the native runtime"}, not: "scheduler=off"},
		{name: "server-only host", args: []string{"--role", "server"}, code: output.Usage,
			want: []string{"a server-only host serves other PCs, which the native runtime cannot yet (loopback only): use the docker runtime there"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, f := nativePreflight(t, c.probe)
			if c.noBus {
				withSys(t, "linux").noBus = true
			}
			code, out, errOut := run(t, append(slices.Clone(nativeAnswers), c.args...)...)
			env := parseSetup(t, out)
			if code != c.code || env.Error == nil {
				t.Fatalf("exit %d, want %d\n%s%s", code, c.code, out, errOut)
			}
			for _, w := range c.want {
				if !strings.Contains(env.Error.Message, w) {
					t.Errorf("message lacks %q: %s", w, env.Error.Message)
				}
			}
			if c.not != "" && strings.Contains(out, c.not) {
				t.Errorf("the advice names %q:\n%s", c.not, out)
			}
			if c.cmd != "" && !slices.ContainsFunc(env.Error.Actions, func(a output.Action) bool { return a.Cmd == c.cmd }) {
				t.Errorf("actions %+v lack %q", env.Error.Actions, c.cmd)
			}
			if onDisk(pathsOf(home).ConfigFile()) || len(f.calls) > 0 {
				t.Errorf("a refused setup changed something: %v", f.commands())
			}
		})
	}
}

// What the native pre-flight checks, and what it leaves out: no Docker,
// no UI port, a 3.5 GB disk floor.
func TestSetupRuntimeNativePreflightChecks(t *testing.T) {
	nativePreflight(t, func(p *setup.Probes) {
		p.Docker = func(context.Context) (string, error) {
			return "", errors.New("exec: \"docker\": executable file not found in $PATH")
		}
		p.Compose = func(context.Context) (string, error) { return "", errors.New("no compose") }
		p.PortFree = func(port int) bool { return port != 9999 } // the UI port is taken
		p.FreeBytes = func() (uint64, error) { return 4 << 30, nil }
	})
	code, out, _ := run(t, "setup", "--runtime", "native", "--plan", "--answer", "provider=deepseek", "--json")
	env := parseSetup(t, out)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	got := map[string]setup.Check{}
	for _, c := range env.Data.Preflight {
		got[c.ID] = c
		if c.Status == setup.Fail {
			t.Errorf("%s failed: %s", c.ID, c.Summary)
		}
	}
	for _, id := range []string{"docker", "compose"} {
		if got[id].Status != setup.Info || !strings.Contains(got[id].Summary, "not used by the native runtime") {
			t.Errorf("%s: %+v", id, got[id])
		}
	}
	if _, ok := got["ui_port"]; ok {
		t.Errorf("the UI port is checked: %+v", got["ui_port"])
	}
	for _, id := range []string{"platform", "systemd", "linger", "api_port", "disk"} {
		if got[id].Status != setup.OK {
			t.Errorf("%s: %+v", id, got[id])
		}
	}
	if !hasStep(env.Data.Steps, "server", "planned", "no web UI") {
		t.Errorf("plan: %+v", env.Data.Steps)
	}
}

// The runtime answer is setup's like role: --runtime and the answer must
// agree, and an install on Docker is not moved by an answer.
func TestSetupRuntimeAnswerRules(t *testing.T) {
	home, _ := nativePreflight(t, nil)
	if code, _, errOut := run(t, "setup", "--runtime", "native", "--answer", "runtime=docker", "--non-interactive"); code != output.Usage || !strings.Contains(errOut, "--runtime native and the answer runtime=docker differ") {
		t.Errorf("disagreeing: exit %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "setup", "--runtime", "podman", "--non-interactive"); code != output.Usage || !strings.Contains(errOut, "runtime=podman") {
		t.Errorf("unknown runtime: exit %d %s", code, errOut)
	}
	_ = os.WriteFile(pathsOf(home).ConfigFile(), []byte("schema_version = 1\n[server]\nurl = \"http://127.0.0.1:18888\"\nui_port = 19999\n"), 0o600)
	if code, _, errOut := run(t, "setup", "--runtime", "native", "--answer", "consent=yes", "--non-interactive"); code != output.Usage || !strings.Contains(errOut, "backsight server runtime switch native") {
		t.Errorf("native over a Docker config: exit %d %s", code, errOut)
	}
}
