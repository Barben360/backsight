package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
)

// isolate points backsight and Claude Code at empty temp dirs and fakes the
// probes, so the doctor never touches the real machine.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BACKSIGHT_HOME", filepath.Join(home, "backsight"))
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	old := probes
	probes = func(e *doctor.Env) {
		e.ManagedDir = filepath.Join(home, "etc")
		e.Docker = func(context.Context) (string, error) { return "28.1.1", nil }
		e.GPUs = func() []string { return nil }
		e.FreeBytes = func(string) (uint64, error) { return 50e9, nil }
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
	calls []string
}

func (p *psRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	p.calls = append(p.calls, strings.Join(args, " "))
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

func TestStopCommands(t *testing.T) {
	for in, want := range map[string]string{
		"hindsight\thindsight\n": "docker compose -p hindsight stop",
		"\tmy-hindsight\n":       "docker stop my-hindsight",
		"":                       "",
	} {
		if got, _ := stopCommands(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
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
