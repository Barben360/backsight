package server

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
)

var update = flag.Bool("update", false, "rewrite golden files")

func devConfig() config.Config {
	cfg := config.Default(paths.Dirs{Data: "/d"}, "/home/u")
	cfg.Server.URL, cfg.Server.UIPort = "http://127.0.0.1:18888", 19999
	return cfg
}

// Low-entropy fakes: secret scanners must not flag the golden files.
var secrets = config.Secrets{APIKey: "fakeapikey", UIKey: "fakeuikey", DBPassword: "fakedbpassword", LLMAPIKey: "fakellmkey"}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s drifted (run go test ./internal/server -update after checking):\n%s", name, got)
	}
}

func TestRenderDevInstall(t *testing.T) {
	f, err := Render(devConfig(), secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "compose.dev.yaml", f.Compose)
	golden(t, "env.dev", f.Env)
	// compose.yaml is world-readable: no secret may land in it.
	for _, v := range []string{secrets.APIKey, secrets.UIKey, secrets.DBPassword, secrets.LLMAPIKey} {
		if strings.Contains(string(f.Compose), v) {
			t.Errorf("secret %q in compose.yaml", v)
		}
	}
}

func TestRenderPrivateExposureAndOverrides(t *testing.T) {
	cfg := devConfig()
	cfg.Network.Exposure, cfg.Network.BindIP = "private", "100.101.102.103"
	cfg.Server.Digest = "sha256:" + strings.Repeat("b", 64)
	cfg.LLM.Provider, cfg.LLM.Model, cfg.LLM.BaseURL = "openai", "gpt-5-mini", "https://llm.example/v1"
	f, err := Render(cfg, secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	c, e := string(f.Compose), string(f.Env)
	for _, want := range []string{`"100.101.102.103:18888:8888"`, `"100.101.102.103:19999:9999"`, "ghcr.io/vectorize-io/hindsight@sha256:" + strings.Repeat("b", 64)} {
		if !strings.Contains(c, want) {
			t.Errorf("compose lacks %s", want)
		}
	}
	for _, want := range []string{"HINDSIGHT_API_LLM_PROVIDER=openai\n", "HINDSIGHT_API_LLM_MODEL=gpt-5-mini\n", "HINDSIGHT_API_LLM_BASE_URL='https://llm.example/v1'\n"} {
		if !strings.Contains(e, want) {
			t.Errorf(".env lacks %q", want)
		}
	}
	if strings.Contains(c, "127.0.0.1") {
		t.Error("private exposure must not also publish on loopback")
	}
}

func TestRenderListsMissingSecrets(t *testing.T) {
	_, err := Render(devConfig(), config.Secrets{UIKey: "x"}, "box")
	var missing *MissingSecretsError
	if !errors.As(err, &missing) || strings.Join(missing.Names, ",") != "api_key,db_password,llm_api_key" {
		t.Errorf("got %v", err)
	}
	_, err = Render(devConfig(), config.Secrets{APIKey: "a", DBPassword: "b", LLMAPIKey: "c"}, "box")
	if !errors.As(err, &missing) || strings.Join(missing.Names, ",") != "ui_key" || !strings.Contains(err.Error(), "backsight secret set ui_key") {
		t.Errorf("ui_key: %v", err)
	}
}

// Ollama runs without a key.
func TestRenderOllamaNeedsNoLLMKey(t *testing.T) {
	cfg := devConfig()
	cfg.LLM.Provider, cfg.LLM.Model, cfg.LLM.BaseURL = "ollama", "qwen3:8b", "http://host.docker.internal:11434"
	s := secrets
	s.LLMAPIKey = ""
	f, err := Render(cfg, s, "box")
	if err != nil || strings.Contains(string(f.Env), "HINDSIGHT_API_LLM_API_KEY") {
		t.Errorf("%v\n%s", err, f.Env)
	}
}

func TestRenderRefusals(t *testing.T) {
	cfg := devConfig()
	cfg.Embeddings.Preset = "english"
	if _, err := Render(cfg, secrets, "box"); err == nil || !strings.Contains(err.Error(), "not available yet") {
		t.Errorf("english preset: %v", err)
	}
	s := secrets
	s.LLMAPIKey = "it's"
	if _, err := Render(devConfig(), s, "box"); err == nil || strings.Contains(err.Error(), "it's") {
		t.Errorf("a quote in a value must be refused without echoing it: %v", err)
	}
	s.LLMAPIKey = "a\nb"
	if _, err := Render(devConfig(), s, "box"); err == nil {
		t.Error("a newline in a value must be refused")
	}
}

func TestWorkerIDIsSanitised(t *testing.T) {
	f, _ := Render(devConfig(), secrets, "My Box.local")
	if !strings.Contains(string(f.Env), "HINDSIGHT_API_WORKER_ID=backsight-my-box-local\n") {
		t.Errorf("%s", f.Env)
	}
}

func TestWriteFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "server")
	f, _ := Render(devConfig(), secrets, "box")
	changed, err := Write(dir, f)
	if err != nil || !changed.Compose || !changed.Env {
		t.Fatalf("%+v %v", changed, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, ".env")); fi.Mode().Perm() != 0o600 {
			t.Errorf(".env mode %o", fi.Mode().Perm())
		}
	}
	changed, err = Write(dir, f)
	if err != nil || changed.Compose || changed.Env {
		t.Errorf("identical content must not count as a change: %+v %v", changed, err)
	}
	plan, err := Diff(dir, f)
	if err != nil || plan.Compose || plan.Env {
		t.Errorf("plan: %+v %v", plan, err)
	}
	f2, _ := Render(devConfig(), config.Secrets{APIKey: "other", UIKey: "fakeuikey", DBPassword: "fakedbpassword", LLMAPIKey: "fakellmkey"}, "box")
	if plan, _ := Diff(dir, f2); plan.Compose || !plan.Env {
		t.Errorf("a secret change touches .env only: %+v", plan)
	}
}

func TestDSNEncodesThePassword(t *testing.T) {
	s := secrets
	s.DBPassword = "p@ss/w:rd?#%"
	f, err := Render(devConfig(), s, "box")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.Env), "@db:5432/hindsight'") || !strings.Contains(string(f.Env), "hindsight:p%40ss%2Fw%3Ard%3F%23%25@db") {
		t.Errorf("%s", f.Env)
	}
}

func TestIPv6BindIsBracketed(t *testing.T) {
	cfg := devConfig()
	cfg.Network.Exposure, cfg.Network.BindIP = "private", "fd7a:115c:a1e0::1"
	f, err := Render(cfg, secrets, "box")
	if err != nil || !strings.Contains(string(f.Compose), `"[fd7a:115c:a1e0::1]:18888:8888"`) {
		t.Errorf("%v\n%s", err, f.Compose)
	}
}

func TestRedact(t *testing.T) {
	got := Redact("auth failed for fakellmkey; db fakedbpassword; short x", secrets)
	if strings.Contains(got, "fakellmkey") || strings.Contains(got, "fakedbpassword") || !strings.Contains(got, "[redacted]") {
		t.Errorf("%q", got)
	}
}

// A .env someone loosened goes back to 0600 even when its content is
// unchanged: it holds the database password and keys (M1 review).
func TestWriteTightensALooseEnvFile(t *testing.T) {
	dir := t.TempDir()
	f := Files{Compose: []byte("services: {}\n"), Env: []byte("KEY=v\n")}
	if _, err := Write(dir, f); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(dir, ".env"), 0o644)
	if _, err := Write(dir, f); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, ".env")); fi.Mode().Perm() != 0o600 {
		t.Errorf(".env mode %v", fi.Mode().Perm())
	}
}
