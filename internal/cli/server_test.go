package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
)

// devInstall writes a dev config and every secret under an isolated home.
func devInstall(t *testing.T, withLLMKey bool) string {
	t.Helper()
	home := isolate(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	_ = os.MkdirAll(filepath.Dir(cfg), 0o700)
	_ = os.WriteFile(cfg, []byte("[server]\nurl = \"http://127.0.0.1:18888\"\nui_port = 19999\n"), 0o600)
	_ = config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error {
		config.GenerateMissing(s)
		if withLLMKey {
			return s.Set("llm_api_key", "fakellmkey")
		}
		return nil
	})
	return home
}

func composeDir(home string) string { return filepath.Join(home, "backsight", "data", "server") }

func TestServerRenderWritesFiles(t *testing.T) {
	home := devInstall(t, true)
	code, out, errOut := run(t, "server", "render", "--json")
	if code != output.OK {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if strings.Contains(out, "fakellmkey") {
		t.Error("a secret reached the output")
	}
	c, err := os.ReadFile(filepath.Join(composeDir(home), "compose.yaml"))
	if err != nil || !strings.Contains(string(c), `"127.0.0.1:18888:8888"`) {
		t.Errorf("%v\n%s", err, c)
	}
	if e, _ := os.ReadFile(filepath.Join(composeDir(home), ".env")); !strings.Contains(string(e), "fakellmkey") {
		t.Error(".env lacks the LLM key")
	}
	_, out, _ = run(t, "server", "render", "--json")
	var env struct {
		Data struct {
			Compose bool `json:"compose_changed"`
			Env     bool `json:"env_changed"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if env.Data.Compose || env.Data.Env {
		t.Errorf("second render changed files: %s", out)
	}
}

func TestServerRenderPlanWritesNothing(t *testing.T) {
	home := devInstall(t, true)
	code, out, _ := run(t, "server", "render", "--plan")
	if code != output.OK || !strings.Contains(out, "compose.yaml: would be written") || strings.Contains(out, "fakellmkey") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(composeDir(home), "compose.yaml")); err == nil {
		t.Error("--plan wrote a file")
	}
}

func TestServerRenderMissingSecretNeedsUser(t *testing.T) {
	devInstall(t, false)
	code, out, _ := run(t, "server", "render", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "llm_api_key") || !strings.Contains(out, "secret set") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestServerRenderWithoutConfig(t *testing.T) {
	isolate(t)
	if code, _, errOut := run(t, "server", "render"); code != output.NeedsUser || !strings.Contains(errOut, "server install") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}
