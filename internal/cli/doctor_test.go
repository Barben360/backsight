package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
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
