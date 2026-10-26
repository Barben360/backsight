package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

func TestPathsJSON(t *testing.T) {
	unsetMachineEnv(t)
	home := t.TempDir()
	t.Setenv("BACKSIGHT_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "config.toml"), []byte("schema_version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "paths", "--json")
	if code != output.OK {
		t.Fatalf("exit %d: %s", code, out)
	}
	var env struct {
		Data struct {
			Data, Config, State, Bin string
			ConfigFile               string `json:"config_file"`
			ConfigExists             bool   `json:"config_exists"`
			SecretsExists            bool   `json:"secrets_exists"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	d := env.Data
	if d.Data != filepath.Join(home, "data") || d.State != filepath.Join(home, "state") || d.ConfigFile != filepath.Join(home, "config", "config.toml") {
		t.Errorf("%+v", d)
	}
	if !d.ConfigExists || d.SecretsExists {
		t.Errorf("existence flags wrong: %+v", d)
	}
}

func TestPathsRefusesRelativeHome(t *testing.T) {
	t.Setenv("BACKSIGHT_HOME", "relative/dir")
	code, _, errOut := run(t, "paths")
	if code != output.Blocked || !strings.Contains(errOut, "BACKSIGHT_HOME") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestPathsText(t *testing.T) {
	unsetMachineEnv(t)
	home := t.TempDir()
	t.Setenv("BACKSIGHT_HOME", home)
	code, out, _ := run(t, "paths")
	want := []string{"config.toml   " + filepath.Join(home, "config", "config.toml") + " (missing)", "state         " + filepath.Join(home, "state")}
	for _, w := range want {
		if code != output.OK || !strings.Contains(out, w) {
			t.Errorf("exit %d, output lacks %q:\n%s", code, w, out)
		}
	}
}

func TestPathsErrorJSONEnvelope(t *testing.T) {
	t.Setenv("BACKSIGHT_HOME", "relative/dir")
	code, out, errOut := run(t, "paths", "--json")
	var env struct {
		OK    bool          `json:"ok"`
		Error *output.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if code != output.Blocked || env.OK || env.Error == nil || env.Error.Code != output.Blocked || !strings.Contains(env.Error.Message, "BACKSIGHT_HOME") || errOut != "" {
		t.Errorf("exit %d: %s (stderr %q)", code, out, errOut)
	}
}
