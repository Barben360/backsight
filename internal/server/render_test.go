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
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
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
}

// portLines returns the published-port entries of a rendered compose.
func portLines(compose string) []string {
	var got []string
	for _, l := range strings.Split(compose, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, `- "`) && (strings.HasSuffix(l, `:8888"`) || strings.HasSuffix(l, `:9999"`)) {
			got = append(got, l)
		}
	}
	return got
}

func wantPorts(t *testing.T, cfg config.Config, want ...string) {
	t.Helper()
	f, err := Render(cfg, secrets, "box")
	if err != nil {
		t.Fatal(err)
	}
	if got := portLines(string(f.Compose)); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ports %q, want %q", got, want)
	}
}

// The host's own client keeps server.url on loopback: opening the server to
// other PCs must not take that address away.
func TestPrivateExposurePublishesLoopbackToo(t *testing.T) {
	cfg := devConfig()
	cfg.Network.Exposure, cfg.Network.BindIP = "private", "192.168.1.10"
	wantPorts(t, cfg,
		`- "127.0.0.1:18888:8888"`, `- "127.0.0.1:19999:9999"`,
		`- "192.168.1.10:18888:8888"`, `- "192.168.1.10:19999:9999"`)
}

func TestLoopbackExposurePublishesLoopbackOnly(t *testing.T) {
	wantPorts(t, devConfig(), `- "127.0.0.1:18888:8888"`, `- "127.0.0.1:19999:9999"`)
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

// The server reaches a model served on this machine (Ollama's preset,
// http://host.docker.internal:11434) on Docker Engine for Linux too, where
// the name exists only when the service maps it to the host's gateway
// (Docker Desktop has it built in). Every provider gets the mapping: an
// OpenAI-compatible server on the host needs it as much.
func TestRenderLetsTheServerReachTheHost(t *testing.T) {
	for _, provider := range []string{"ollama", "deepseek"} {
		cfg := devConfig()
		cfg.LLM.Provider = provider
		f, err := Render(cfg, secrets, "box")
		if err != nil {
			t.Fatal(err)
		}
		c := string(f.Compose)
		start, end := strings.Index(c, "\n  hindsight:\n"), strings.Index(c, "\nvolumes:\n")
		if start < 0 || end < start {
			t.Fatalf("no hindsight service in:\n%s", c)
		}
		if !strings.Contains(c[start:end], "\n    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n") {
			t.Errorf("%s: the hindsight service does not map host.docker.internal to the host:\n%s", provider, c[start:end])
		}
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
	wantPorts(t, cfg,
		`- "127.0.0.1:18888:8888"`, `- "127.0.0.1:19999:9999"`,
		`- "[fd7a:115c:a1e0::1]:18888:8888"`, `- "[fd7a:115c:a1e0::1]:19999:9999"`)
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

func TestRenderAndComposeUseTheConfiguredProject(t *testing.T) {
	cfg := devConfig()
	cfg.Server.Project = "bs-fresh"
	f, err := Render(cfg, secrets, "host")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.Compose), "name: bs-fresh\n") {
		t.Errorf("compose name:\n%s", f.Compose)
	}
	c := Compose{Dir: "/x", Project: "bs-fresh"}
	if a := strings.Join(c.args("ps"), " "); !strings.Contains(a, "-p bs-fresh ") || c.DataVolume() != "bs-fresh_pg_data" {
		t.Errorf("args %q, volume %q", a, c.DataVolume())
	}
	if (Compose{}).DataVolume() != "backsight_pg_data" {
		t.Error("the default project changed")
	}
}

// A restore refuses an archive from a newer Hindsight than the one pinned,
// and one it cannot place: a digest alone does not say which is newer.
func TestCompareImages(t *testing.T) {
	const (
		pinned = "ghcr.io/vectorize-io/hindsight:1.1.0@sha256:bbbb"
		older  = "ghcr.io/vectorize-io/hindsight:1.0.2@sha256:aaaa"
		newer  = "ghcr.io/vectorize-io/hindsight:1.10.0@sha256:cccc"
	)
	for _, c := range []struct {
		archive    string
		cmp        int
		comparable bool
	}{
		{pinned, 0, true},
		{"ghcr.io/vectorize-io/hindsight@sha256:bbbb", 0, true}, // a digest set in config, same image
		{older, -1, true},
		{newer, 1, true},
		{"ghcr.io/vectorize-io/hindsight:v1.1.1@sha256:dddd", 1, true},
		{"ghcr.io/vectorize-io/hindsight@sha256:eeee", 0, false},
		{"ghcr.io/vectorize-io/hindsight:1.1.0-rc1@sha256:ffff", 0, false},
		{"ghcr.io/vectorize-io/hindsight:1.1.0@sha256:9999", 0, false}, // same tag, another digest: a rebuild
		{"", 0, false},
	} {
		cmp, ok := CompareImages(c.archive, pinned)
		if cmp != c.cmp || ok != c.comparable {
			t.Errorf("%q against %q: %d %v, want %d %v", c.archive, pinned, cmp, ok, c.cmp, c.comparable)
		}
	}
}

// The server keys come back from a .env Render wrote, without the quotes
// it adds; other variables (the LLM key) are not secrets.toml's to adopt.
func TestKeptSecretsReadsWhatRenderWrote(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "server")
	got, err := KeptSecrets(dir)
	if err != nil || len(got) != 0 {
		t.Fatalf("no .env: %v %v", got, err)
	}
	f, _ := Render(devConfig(), secrets, "box")
	if _, err := Write(dir, f); err != nil {
		t.Fatal(err)
	}
	got, err = KeptSecrets(dir)
	if err != nil || len(got) != 3 || got["db_password"] != secrets.DBPassword || got["api_key"] != secrets.APIKey || got["ui_key"] != secrets.UIKey {
		t.Errorf("%v %v", got, err)
	}
	// Unquoted, double-quoted, CRLF and empty values; comments ignored.
	raw := "# BACKSIGHT_DB_PASSWORD='commented'\r\nBACKSIGHT_DB_PASSWORD=plainpassword1234\r\nHINDSIGHT_API_TENANT_API_KEY=\"dq\"\nHINDSIGHT_CP_ACCESS_KEY=''\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ = KeptSecrets(dir)
	if len(got) != 2 || got["db_password"] != "plainpassword1234" || got["api_key"] != "dq" {
		t.Errorf("%v", got)
	}
}

func TestPinnedVersionIsTheImageTag(t *testing.T) {
	old := HindsightImage
	defer func() { HindsightImage = old }()
	HindsightImage = "ghcr.io/vectorize-io/hindsight:1.1.0@sha256:abc"
	if got := PinnedVersion(); got != "1.1.0" {
		t.Errorf("with digest: %q", got)
	}
	HindsightImage = "localhost:5000/hindsight:2.3.4"
	if got := PinnedVersion(); got != "2.3.4" {
		t.Errorf("registry port: %q", got)
	}
}
