package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// closedPort is a loopback port nothing listens on: tests must never reach
// a real server (the author's dev install listens on 18888).
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// devInstall writes a dev config pointing at a closed port and every secret
// under an isolated home; tests needing a server call pointAt.
func devInstall(t *testing.T, withLLMKey bool) string {
	t.Helper()
	home := isolate(t)
	_ = os.MkdirAll(filepath.Join(home, "backsight", "config"), 0o700)
	pointAt(t, home, closedPort(t))
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
	if err != nil || !strings.Contains(string(c), `"127.0.0.1:`) || !strings.Contains(string(c), `:8888"`) {
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

type fakeCompose struct {
	calls  [][]string
	ps     string
	failOn string // subcommand that fails like a port clash
}

func (f *fakeCompose) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	for _, a := range args {
		if f.failOn != "" && a == f.failOn {
			return []byte("Error: port is already allocated"), errors.New("exit status 1")
		}
	}
	for _, a := range args {
		if a == "ps" {
			return []byte(f.ps), nil
		}
	}
	return nil, nil
}

func (f *fakeCompose) subcommands() []string {
	var out []string
	for _, c := range f.calls {
		for i, a := range c {
			if a == "compose.yaml" || strings.HasSuffix(a, "compose.yaml") {
				out = append(out, strings.Join(c[i+1:], " "))
				break
			}
		}
	}
	return out
}

// fakeHindsight answers like a started server: health, version, banks and
// bank config (overrides kept in memory).
func fakeHindsight(t *testing.T) (port int) {
	t.Helper()
	var mu sync.Mutex
	banks := map[string]bool{}
	overrides := map[string]json.RawMessage{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{"status":"healthy","database":"connected"}`))
		case r.URL.Path == "/version":
			_, _ = w.Write([]byte(`{"api_version":"1.1.0"}`))
		case r.URL.Path == "/v1/default/banks":
			var items []string
			for b := range banks {
				items = append(items, `{"bank_id":"`+b+`"}`)
			}
			_, _ = w.Write([]byte(`{"banks":[` + strings.Join(items, ",") + `]}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/default/banks/"):
			banks[strings.TrimPrefix(r.URL.Path, "/v1/default/banks/")] = true
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/config"):
			var body struct {
				Updates map[string]json.RawMessage `json:"updates"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body.Updates {
				overrides[k] = v
			}
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/memories/dry-run-extract"):
			_, _ = w.Write([]byte(`{"facts":[{"text":"The billing service keeps PostgreSQL 16 for partial indexes.","fact_type":"world"}],"usage":{"input_tokens":900,"output_tokens":40}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/config"):
			ov, _ := json.Marshal(overrides)
			_, _ = w.Write([]byte(`{"config":{},"overrides":` + string(ov) + `}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ = strconv.Atoi(u.Port())
	return port
}

// pointAt rewrites the dev config to use the fake server's port.
func pointAt(t *testing.T, home string, port int) {
	t.Helper()
	cfgPath := filepath.Join(home, "backsight", "config", "config.toml")
	_ = os.WriteFile(cfgPath, []byte("[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\n"), 0o600)
}

func withCompose(t *testing.T) *fakeCompose {
	t.Helper()
	f := &fakeCompose{ps: `{"Service":"db","State":"running","Health":"healthy"}` + "\n" + `{"Service":"hindsight","State":"running","Health":""}`}
	old, oldPoll := composeRunner, healthPoll
	composeRunner, healthPoll = f, 10*time.Millisecond
	t.Cleanup(func() { composeRunner, healthPoll = old, oldPoll })
	return f
}

func TestServerInstallFromScratch(t *testing.T) {
	home := isolate(t)
	f := withCompose(t)
	port := fakeHindsight(t)
	t.Setenv("LLM", "fakellmkey")
	if code, _, errOut := run(t, "secret", "set", "llm_api_key", "--from-env", "LLM"); code != output.OK {
		t.Fatal(errOut)
	}
	code, out, errOut := run(t, "server", "install", "--api-port", strconv.Itoa(port), "--ui-port", "19999")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(f.subcommands(), " | "); got != "pull | up -d --wait" {
		t.Errorf("compose calls: %s", got)
	}
	cfg, err := config.Load(filepath.Join(home, "backsight", "config", "config.toml"), pathsOf(home), home)
	if err != nil || cfg.Server.UIPort != 19999 || !strings.HasSuffix(cfg.Server.URL, ":"+strconv.Itoa(port)) {
		t.Errorf("%+v %v", cfg.Server, err)
	}
	s, _ := config.LoadSecrets(secretsFile(home))
	if s.APIKey == "" || s.UIKey == "" || s.DBPassword == "" || s.LLMAPIKey != "fakellmkey" {
		t.Errorf("secrets: %v", s.Names())
	}
	if !strings.Contains(out, "bank") || !strings.Contains(out, "provider") || !strings.Contains(out, "healthy") || strings.Contains(out+errOut, s.APIKey) {
		t.Errorf("output:\n%s", out)
	}
	// Idempotent: a second run keeps config and secrets.
	before := s
	if code, _, errOut := run(t, "server", "install"); code != output.OK {
		t.Fatalf("rerun: %s", errOut)
	}
	if s2, _ := config.LoadSecrets(secretsFile(home)); s2 != before {
		t.Error("a rerun regenerated secrets")
	}
}

func TestServerInstallStopsForTheLLMKey(t *testing.T) {
	home := isolate(t)
	f := withCompose(t)
	code, out, _ := run(t, "server", "install", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "secret set llm_api_key") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing may start without the key: %v", f.calls)
	}
	// Config and generated secrets are kept, so the rerun resumes.
	if s, _ := config.LoadSecrets(secretsFile(home)); s.APIKey == "" {
		t.Error("generated secrets were not kept")
	}
}

func TestServerInstallRefusesConflictingFlags(t *testing.T) {
	devInstall(t, true)
	withCompose(t)
	if code, _, errOut := run(t, "server", "install", "--api-port", "28888"); code != output.Usage || !strings.Contains(errOut, "server.url") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestServerInstallPlanDoesNothing(t *testing.T) {
	home := isolate(t)
	f := withCompose(t)
	code, out, _ := run(t, "server", "install", "--plan")
	if code != output.OK || !strings.Contains(out, "pull") || len(f.calls) != 0 {
		t.Errorf("exit %d calls %v\n%s", code, f.calls, out)
	}
	if _, err := os.Stat(filepath.Join(home, "backsight", "config", "config.toml")); err == nil {
		t.Error("--plan wrote the config")
	}
}

func TestServerInstallComposeFailure(t *testing.T) {
	isolate(t)
	f := withCompose(t)
	f.failOn = "up"
	t.Setenv("LLM", "fakellmkey")
	run(t, "secret", "set", "llm_api_key", "--from-env", "LLM")
	code, _, errOut := run(t, "server", "install", "--api-port", strconv.Itoa(closedPort(t)), "--ui-port", "19999")
	if code != output.Blocked || !strings.Contains(errOut, "port is already allocated") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestServerInstallHealthTimeout(t *testing.T) {
	isolate(t)
	withCompose(t)
	old := healthTimeout
	healthTimeout = 50 * time.Millisecond
	t.Cleanup(func() { healthTimeout = old })
	t.Setenv("LLM", "fakellmkey")
	run(t, "secret", "set", "llm_api_key", "--from-env", "LLM")
	code, _, errOut := run(t, "server", "install", "--api-port", strconv.Itoa(closedPort(t)), "--ui-port", "19999")
	if code != output.Blocked || !strings.Contains(errOut, "health") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestServerStatus(t *testing.T) {
	home := devInstall(t, true)
	withCompose(t)
	pointAt(t, home, fakeHindsight(t))
	code, out, _ := run(t, "server", "status", "--json")
	var env struct {
		Data struct {
			Containers []server.Container `json:"containers"`
			API        struct {
				Healthy bool   `json:"healthy"`
				Version string `json:"version"`
			} `json:"api"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d %v\n%s", code, err, out)
	}
	if len(env.Data.Containers) != 2 || !env.Data.API.Healthy || env.Data.API.Version != "1.1.0" {
		t.Errorf("%s", out)
	}
}

func TestServerStatusAPIDown(t *testing.T) {
	devInstall(t, true) // a closed port
	f := withCompose(t)
	f.ps = ""
	code, out, _ := run(t, "server", "status")
	if code != output.Blocked || !strings.Contains(out, "not running") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestServerStartStop(t *testing.T) {
	devInstall(t, true)
	f := withCompose(t)
	if code, _, errOut := run(t, "server", "start"); code != output.OK {
		t.Fatal(errOut)
	}
	if code, _, errOut := run(t, "server", "stop"); code != output.OK {
		t.Fatal(errOut)
	}
	if got := strings.Join(f.subcommands(), " | "); got != "up -d --wait | stop" {
		t.Errorf("%s", got)
	}
}

func pathsOf(home string) paths.Dirs {
	d, _ := paths.Resolve("linux", home, func(k string) string {
		if k == "BACKSIGHT_HOME" {
			return filepath.Join(home, "backsight")
		}
		return ""
	})
	return d
}

func TestErrorEnvelopeNamesTheFullCommand(t *testing.T) {
	isolate(t)
	_, out, _ := run(t, "server", "render", "--json")
	if !strings.Contains(out, `"command": "server render"`) {
		t.Errorf("%s", out)
	}
}

func TestBankApply(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	code, out, errOut := run(t, "bank", "apply", "--json")
	if code != output.OK || !strings.Contains(out, `"created": true`) || !strings.Contains(out, `"sent": true`) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	st, _ := state.Load(filepath.Join(home, "backsight", "state", "state.json"))
	if st.BankConfigHash != server.TemplateHash() {
		t.Errorf("hash not recorded: %q", st.BankConfigHash)
	}
	_, out, _ = run(t, "bank", "apply", "--json")
	if !strings.Contains(out, `"sent": false`) {
		t.Errorf("an unchanged template was re-sent:\n%s", out)
	}
	_, out, _ = run(t, "bank", "apply", "--plan")
	if !strings.Contains(out, "unchanged") {
		t.Errorf("plan: %s", out)
	}
}

func TestBankApplyServerDown(t *testing.T) {
	devInstall(t, true)
	if code, _, errOut := run(t, "bank", "apply"); code != output.Blocked || !strings.Contains(errOut, "unreachable") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestServerLLMCheck(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	code, out, errOut := run(t, "server", "llm", "check")
	if code != output.OK || !strings.Contains(out, "1 fact") || !strings.Contains(out, "PostgreSQL 16") {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestServerLLMCheckServerDown(t *testing.T) {
	devInstall(t, true)
	if code, _, errOut := run(t, "server", "llm", "check"); code != output.Blocked || !strings.Contains(errOut, "LLM") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

// A state hash with no bank on the server (volume wiped) must recreate and
// reconfigure the bank.
func TestBankApplyRecreatesAWipedBank(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	_ = state.Update(d, "test", func(s *state.State) error { s.BankConfigHash = server.TemplateHash(); return nil })
	_, out, _ := run(t, "bank", "apply", "--json")
	if !strings.Contains(out, `"created": true`) || !strings.Contains(out, `"sent": true`) {
		t.Errorf("%s", out)
	}
}
