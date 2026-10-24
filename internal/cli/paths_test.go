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
	home := t.TempDir()
	t.Setenv("BACKSIGHT_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "config.toml"), []byte("language = \"fr\"\n"), 0o600); err != nil {
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
