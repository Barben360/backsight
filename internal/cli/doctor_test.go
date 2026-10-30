package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/state"
)

// isolatedEnv are the variables of the shell running the tests (a hook,
// verify, a scratch stack, an XDG layout) that would point backsight at
// the real machine; isolate unsets them.
var isolatedEnv = []string{
	"BACKSIGHT_STATE_DIR", "BACKSIGHT_SESSION", "BACKSIGHT_PROJECT", "CLAUDE_ENV_FILE", "DOCKER_HOST", claudePIDEnv,
	updateLockEnv, "BACKSIGHT_HOOK_CHAOS", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
}

// unsetMachineEnv unsets isolatedEnv for the test (t.Setenv restores it).
func unsetMachineEnv(t *testing.T) {
	t.Helper()
	for _, k := range isolatedEnv {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// isolate points backsight and Claude Code at empty temp dirs and fakes the
// probes, so the doctor never touches the real machine.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	unsetMachineEnv(t)
	t.Setenv("BACKSIGHT_HOME", filepath.Join(home, "backsight"))
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	old := probes
	probes = func(e *doctor.Env) {
		e.ManagedDir = filepath.Join(home, "etc")
		e.Docker = func(context.Context) (string, error) { return "28.1.1", nil }
		e.GPUs = func() []string { return nil }
		e.FreeBytes = func(string) (uint64, error) { return 50e9, nil }
		// Never the real /health probe: a reference server on this
		// machine must not answer a test (0067 block 4 review rules).
		e.ReferenceServer = func(context.Context, string) (doctor.RefServer, error) { return doctor.RefServer{}, nil }
	}
	t.Cleanup(func() { probes = old })
	return home
}

func TestDoctorJSONHealthyMachine(t *testing.T) {
	isolate(t)
	code, out, _ := run(t, "doctor", "--json")
	var env struct {
		OK   bool          `json:"ok"`
		Data doctor.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if code != output.OK || !env.OK || len(env.Data.Checks) < 8 {
		t.Errorf("exit %d: %+v", code, env)
	}
}

func TestDoctorErrorsExitBlockedWithTheReport(t *testing.T) {
	home := isolate(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	_ = os.MkdirAll(filepath.Dir(cfg), 0o700)
	_ = os.WriteFile(cfg, []byte("[recall]\nmin_scor = 1\n"), 0o600)
	code, out, _ := run(t, "doctor", "--json")
	var env struct {
		OK    bool          `json:"ok"`
		Data  doctor.Report `json:"data"`
		Error *output.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if code != output.Blocked || env.OK || env.Error == nil || env.Data.Errors != 1 || len(env.Data.Checks) == 0 {
		t.Errorf("exit %d: %s", code, out)
	}
	if strings.Count(out, `"schema_version"`) != 1 {
		t.Errorf("exactly one envelope expected:\n%s", out)
	}
}

func TestDoctorText(t *testing.T) {
	isolate(t)
	code, out, _ := run(t, "doctor")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{"info   config", "WARN   retention", "fix: backsight client retention", "0 error(s), 1 warning(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorTextErrorExitsBlockedWithoutStderr(t *testing.T) {
	home := isolate(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	_ = os.MkdirAll(filepath.Dir(cfg), 0o700)
	_ = os.WriteFile(cfg, []byte("[recall]\nmin_scor = 1\n"), 0o600)
	code, out, errOut := run(t, "doctor")
	if code != output.Blocked || !strings.Contains(out, "ERROR  config") || !strings.Contains(out, "1 error(s)") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if errOut != "" {
		t.Errorf("the report already says it; stderr must stay empty: %q", errOut)
	}
}

func TestDoctorClientCheck(t *testing.T) {
	home := isolate(t)
	_, out, _ := run(t, "doctor")
	if !strings.Contains(out, "client") || !strings.Contains(out, "not wired into Claude Code") {
		t.Errorf("not installed:\n%s", out)
	}
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error { s.PluginVersion = "0.0.0-old"; return nil })
	_, out, _ = run(t, "doctor")
	if !strings.Contains(out, "Claude Code wiring drifted") || !strings.Contains(out, "permission rule") {
		t.Errorf("drift:\n%s", out)
	}
}

// psRunner answers `docker ps` with ps and records the call.
type psRunner struct {
	ps    string
	err   error
	calls []string
}

func (p *psRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	p.calls = append(p.calls, strings.Join(args, " "))
	if p.err != nil {
		return nil, p.err
	}
	return []byte(p.ps), nil
}

func TestReferenceServerProbeFindsTheComposeProjectByItsPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"healthy"}`)) }))
	t.Cleanup(srv.Close)
	ps := &psRunner{ps: "hindsight\thindsight\nhindsight\thindsight-db\n"}
	old := composeRunner
	composeRunner = ps
	t.Cleanup(func() { composeRunner = old })
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	got, err := referenceServer(context.Background(), srv.URL)
	if err != nil || !got.Running || got.Stop != "docker compose -p hindsight stop" || got.Start != "docker compose -p hindsight start" {
		t.Fatalf("%+v %v", got, err)
	}
	if len(ps.calls) != 1 || !strings.HasPrefix(ps.calls[0], "ps --filter publish="+port+" ") {
		t.Errorf("docker calls %q", ps.calls)
	}
	// Nothing listens: stopped, and Docker is not asked.
	ps.calls = nil
	if got, err := referenceServer(context.Background(), "http://127.0.0.1:"+strconv.Itoa(closedPort(t))); err != nil || got.Running || len(ps.calls) != 0 {
		t.Errorf("closed port: %+v %v %q", got, err, ps.calls)
	}
}

// The test home never lets doctor reach a reference server: a real one on
// this machine (port 8888) would answer otherwise (0067 block 4 review
// rules). The reference config points at a server that counts its calls.
func TestIsolatedDoctorNeverProbesTheReferenceServer(t *testing.T) {
	home := devInstall(t, true)
	var hits atomic.Int32
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	t.Cleanup(ref.Close)
	_ = os.MkdirAll(filepath.Join(home, ".hindsight"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".hindsight", "coding-agent.json"), []byte(`{"apiUrl":"`+ref.URL+`","apiToken":"fakevalue","bankId":"coding-agent::memoire"}`), 0o600)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"node /h/.hindsight/coding-agents/dist/claude-stop-hook.js"}]}]}}`), 0o600)
	_, out, _ := run(t, "doctor")
	if !strings.Contains(out, "reference-server") || !strings.Contains(out, "not running") {
		t.Errorf("the reference config was not read:\n%s", out)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("doctor called the reference server %d time(s)", n)
	}
}

// A server that answers with an error still runs; a Docker that fails
// gives no stop command; localhost is this machine.
func TestReferenceServerProbeEdgeCases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(srv.Close)
	ps := &psRunner{ps: "hindsight\thindsight\n"}
	old := composeRunner
	composeRunner = ps
	t.Cleanup(func() { composeRunner = old })
	local := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	got, err := referenceServer(context.Background(), local)
	if err != nil || !got.Running || got.Stop != "docker compose -p hindsight stop" || len(ps.calls) != 1 {
		t.Errorf("500 on localhost: %+v %v, docker calls %q", got, err, ps.calls)
	}
	ps.calls, ps.err = nil, errors.New("Cannot connect to the Docker daemon")
	got, err = referenceServer(context.Background(), srv.URL)
	if err != nil || !got.Running || got.Stop != "" || got.Start != "" || len(ps.calls) != 1 {
		t.Errorf("docker error: %+v %v, docker calls %q", got, err, ps.calls)
	}
}

// Only a server on this machine is looked up in its Docker.
func TestLocalPort(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:8888":     "8888",
		"http://localhost:8888/":    "8888",
		"http://[::1]:9000":         "9000",
		"http://127.0.0.2:8888":     "8888",
		"http://10.0.0.5:8888":      "",
		"https://mem.example.com:8": "",
		"http://localhost":          "", // no port to filter on
		"://bad":                    "",
	} {
		if got := localPort(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestStopCommands(t *testing.T) {
	for in, want := range map[string][2]string{
		"hindsight\thindsight\n":        {"docker compose -p hindsight stop", "docker compose -p hindsight start"},
		"\tmy-hindsight\n":              {"docker stop my-hindsight", "docker start my-hindsight"},
		"\n\nhindsight\thindsight-db\n": {"docker compose -p hindsight stop", "docker compose -p hindsight start"}, // empty lines first
		"ref_2\tx\r\n":                  {"docker compose -p ref_2 stop", "docker compose -p ref_2 start"},
		"":                              {"", ""},
		// A label or name that is not a plain Compose project or
		// container name never reaches a command the user would paste.
		"hindsight; rm -rf ~\thindsight\n": {"", ""},
		"Hindsight\thindsight\n":           {"", ""},
		"-p\thindsight\n":                  {"", ""},
		"\t$(touch x)\n":                   {"", ""},
		"\t-rm\n":                          {"", ""},
	} {
		if stop, start := stopCommands(in); stop != want[0] || start != want[1] {
			t.Errorf("%q: (%q, %q), want %q", in, stop, start, want)
		}
	}
}

// doctor's ConfigEdits probe plans the edits client config apply makes,
// in the profile asked for.
func TestConfigEditsProbePlansBacksightsOwnEdits(t *testing.T) {
	home := devInstall(t, true)
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error { s.PluginVersion = "0.5.0"; return nil })
	profile := filepath.Join(home, ".claude-work")
	settings := filepath.Join(profile, "settings.json")
	_ = os.MkdirAll(profile, 0o700)
	_ = os.WriteFile(settings, []byte(`{"cleanupPeriodDays": 3650}`), 0o600)
	var e doctor.Env
	e.Dirs, e.Home = pathsOf(home), home
	realProbes(&e)
	e.ManagedDir = filepath.Join(home, "etc")
	planned, err := e.ConfigEdits(profile)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, fr := range planned {
		for _, ch := range fr.Changes {
			paths = append(paths, fr.Target+" "+ch.Path)
		}
	}
	if len(paths) != len(plugin.ToolNames) || !strings.HasPrefix(paths[0], settings+" permissions.allow") {
		t.Errorf("planned %q", paths)
	}
	if b, _ := os.ReadFile(settings); string(b) != `{"cleanupPeriodDays": 3650}` {
		t.Errorf("the probe wrote %s", b)
	}
}

// doctorSys answers the user manager's read-only questions doctor asks on
// native (units' states, linger) and records every call.
type doctorSys struct {
	calls  []string
	states string // systemctl show's answer
	linger string
}

func (s *doctorSys) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, line)
	switch {
	case strings.HasPrefix(line, "systemctl --user show -p ActiveState,SubState "):
		return []byte(s.states), nil
	case strings.HasPrefix(line, "loginctl show-user "):
		return []byte(s.linger + "\n"), nil
	}
	return nil, nil
}

// lockedVenv fills v's site-packages with the lock's distributions, as
// uv leaves them, but fastmcp at fastmcp.
func lockedVenv(t *testing.T, n server.Native, v, fastmcp string) {
	t.Helper()
	sp := filepath.Join(n.VenvDir(v), "lib", "python3.11", "site-packages")
	_ = os.RemoveAll(sp)
	for name, ver := range nativelock.Versions() {
		if name == "fastmcp" {
			ver = fastmcp
		}
		if err := os.MkdirAll(filepath.Join(sp, strings.ReplaceAll(name, "-", "_")+"-"+ver+".dist-info"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func doctorChecks(t *testing.T) (int, map[string]doctor.Check) {
	t.Helper()
	code, out, _ := run(t, "doctor", "--json")
	var env struct {
		Data doctor.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	byID := map[string]doctor.Check{}
	for _, c := range env.Data.Checks {
		if _, dup := byID[c.ID]; !dup {
			byID[c.ID] = c
		}
	}
	return code, byID
}

// doctor on a native install reads its guards through the runtime, and
// only reads: no unit started or stopped, no stray stopped, nothing
// written under the native directory.
func TestDoctorOnNativeReadsTheGuards(t *testing.T) {
	_, n, tools, _, _ := nativeHost(t)
	sys := &doctorSys{states: "ActiveState=active\nSubState=running\n\nActiveState=active\nSubState=running\n", linger: "yes"}
	nativeSys = sys
	lockedVenv(t, n, n.Version(), "3.2.0")
	if err := os.Chmod(n.Home(), 0o700); err != nil {
		t.Fatal(err)
	}
	tools.calls = nil
	_, c := doctorChecks(t)
	for _, id := range []string{"native-unit", "native-fsync", "native-identity", "native-stray", "native-linger", "native-home", "native-venv"} {
		if c[id].Status != doctor.OK {
			t.Errorf("%s: %+v, want ok", id, c[id])
		}
	}
	if c["docker"].Status != doctor.Info || c["web-ui"].Status != doctor.Info {
		t.Errorf("docker %+v, web-ui %+v", c["docker"], c["web-ui"])
	}
	all := strings.Join(append(append([]string{}, sys.calls...), tools.calls...), "\n")
	for _, want := range []string{"systemctl --user show -p ActiveState,SubState", "loginctl show-user", "SHOW fsync", "pg_controldata -D"} {
		if !strings.Contains(all, want) {
			t.Errorf("doctor never asked %q:\n%s", want, all)
		}
	}
	for _, call := range append(append([]string{}, sys.calls...), tools.calls...) {
		if !strings.Contains(call, " show") && !strings.HasSuffix(call, "SHOW fsync") && !strings.Contains(call, "pg_controldata -D") &&
			!strings.HasPrefix(call, "systemctl --user show-environment") {
			t.Errorf("doctor ran %q", call)
		}
	}

	// Each guard broken: its check fails with its fix.
	sys.states = "ActiveState=active\nSubState=running\n\nActiveState=failed\nSubState=failed\n"
	sys.linger = "no"
	lockedVenv(t, n, n.Version(), "4.1.4")
	_ = os.Chmod(n.Home(), 0o755)
	if err := os.WriteFile(n.IdentityFile(), []byte("999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := n.InstanceDir() + ".refused-1761700000"
	if err := os.MkdirAll(refused, 0o700); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(nativeProc, "4242")
	_ = os.MkdirAll(stray, 0o700)
	_ = os.WriteFile(filepath.Join(stray, "cmdline"), []byte("/x/bin/postgres\x00-D\x00"+n.DataDir()+"\x00"), 0o600)
	_ = os.WriteFile(filepath.Join(stray, "cgroup"), []byte("0::/user.slice/user-1000.slice/session-2.scope\n"), 0o600)
	code, c := doctorChecks(t)
	for id, want := range map[string]string{
		"native-unit":     n.ServerUnit() + " is failed",
		"native-linger":   "linger is off",
		"native-venv":     "fastmcp==3.2.0",
		"native-home":     "has mode 0755, not 0700",
		"native-identity": "identifier 111, recorded 999",
		"native-stray":    "pid 4242",
	} {
		if c[id].Status != doctor.Error || !strings.Contains(c[id].Summary, want) {
			t.Errorf("%s: %+v, want error with %q", id, c[id], want)
		}
	}
	if c["native-refused"].Status != doctor.Info || c["native-refused"].Fix != "rm -rf "+refused {
		t.Errorf("native-refused: %+v", c["native-refused"])
	}
	if code != output.Blocked {
		t.Errorf("exit %d, want %d", code, output.Blocked)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("doctor stopped the stray: %v", err)
	}
	if fi, _ := os.Stat(n.Home()); fi.Mode().Perm() != 0o755 {
		t.Errorf("doctor changed home's mode: %v", fi.Mode())
	}
}

// After a switch, doctor shows what server runtime clean would delete and
// its size, as clean names them (ruling R43): the whole native directory,
// not only the database.
func TestDoctorShowsTheKeptRuntimeWithCleansSize(t *testing.T) {
	home := devInstall(t, true)
	withSys(t, "linux")
	d := pathsOf(home)
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatal(err)
	}
	n := server.Native{Cfg: cfg, Dirs: d}
	for p, size := range map[string]int{filepath.Join(n.DataDir(), "base"): 300, filepath.Join(n.VenvDir("v"), "lib"): 2000, filepath.Join(n.HF(), "m"): 1000} {
		put0(t, p, size)
	}
	if err := state.Update(d, "test", func(s *state.State) error {
		s.RuntimeSwitch = &state.RuntimeSwitch{From: "native", To: "docker", Step: "done", At: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, c := doctorChecks(t)
	want := "the native server in " + n.Root() + ": the database, the Python environments and the models (3.2 kB) kept from before the switch: backsight server runtime clean deletes it"
	if rs := c["runtime-switch"]; rs.Status != doctor.Info || !strings.Contains(rs.Summary, "switched from native to docker") || rs.Detail != want {
		t.Errorf("runtime-switch %+v\nwant detail %q", rs, want)
	}
}

func put0(t *testing.T, p string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}
