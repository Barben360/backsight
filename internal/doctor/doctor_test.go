package doctor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

func put(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// fixture is a home with nothing installed, Docker running and no GPU.
type fixture struct {
	t    *testing.T
	home string
	env  Env
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	f := &fixture{t: t, home: home}
	f.env = Env{
		Dirs:       paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), State: filepath.Join(root, "state"), Bin: filepath.Join(root, "bin")},
		Home:       home,
		Getenv:     func(string) string { return "" },
		ManagedDir: filepath.Join(root, "etc"),
		Docker:     func(context.Context) (string, error) { return "28.1.1", nil },
		GPUs:       func() []string { return nil },
		Timeout:    2 * time.Second,
	}
	return f
}

func (f *fixture) run() Report { return Run(context.Background(), f.env) }

func (f *fixture) reference(url string) {
	put(f.t, filepath.Join(f.home, ".hindsight", "coding-agent.json"), `{"apiUrl":"`+url+`","apiToken":"fakevalue","bankId":"coding-agent::memoire"}`, 0o600)
	put(f.t, filepath.Join(f.home, ".claude", "settings.json"), `{"cleanupPeriodDays": 3650, "hooks":{"Stop":[{"hooks":[{"type":"command","command":"node /h/.hindsight/coding-agents/dist/claude-stop-hook.js"}]}]}}`, 0o600)
}

func check(t *testing.T, r Report, id string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", id, r.Checks)
	return Check{}
}

func expect(t *testing.T, c Check, status Status, inSummary string) {
	t.Helper()
	if c.Status != status || !strings.Contains(c.Summary, inSummary) {
		t.Errorf("%s: got %s %q, want %s containing %q", c.ID, c.Status, c.Summary, status, inSummary)
	}
}

// fakeServer fakes the endpoints doctor reads; observations carries tags lists.
func fakeServer(t *testing.T, observations ...string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fakevalue" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.EscapedPath() {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"healthy","database":"connected"}`))
		case "/version":
			_, _ = w.Write([]byte(`{"api_version":"1.1.0"}`))
		case "/v1/default/banks":
			_, _ = w.Write([]byte(`{"banks":[{"bank_id":"coding-agent::memoire"}]}`))
		case "/v1/default/banks/coding-agent%3A%3Amemoire/stats":
			_, _ = w.Write([]byte(`{"total_nodes":323,"total_documents":11,"failed_operations":0,"pending_consolidation":2}`))
		case "/v1/default/banks/coding-agent%3A%3Amemoire/memories/list":
			items := []string{}
			for i, tags := range observations {
				items = append(items, `{"id":"o`+string(rune('a'+i))+`","fact_type":"observation","tags":`+tags+`}`)
			}
			_, _ = w.Write([]byte(`{"items":[` + strings.Join(items, ",") + `],"total":` + strconv.Itoa(len(items)) + `}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNothingInstalled(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	expect(t, check(t, r, "config"), Info, "not set up")
	expect(t, check(t, r, "server"), Info, "no server")
	expect(t, check(t, r, "reference"), OK, "no reference install")
	expect(t, check(t, r, "docker"), OK, "28.1.1")
	ret := check(t, r, "retention")
	expect(t, ret, Warn, "30 days")
	if !strings.Contains(ret.Fix, "cleanupPeriodDays") || !strings.Contains(ret.Fix, "3650") {
		t.Errorf("retention fix: %q", ret.Fix)
	}
	expect(t, check(t, r, "gpu"), Info, "no GPU detected")
	if r.Errors != 0 {
		t.Errorf("a fresh machine has nothing broken: %+v", r)
	}
}

func TestReferenceInstallHealthy(t *testing.T) {
	f := newFixture(t)
	srv := fakeServer(t, `["project:a"]`, `[]`, `["source:chat"]`)
	f.reference(srv.URL)
	r := f.run()
	expect(t, check(t, r, "server"), OK, "1.1.0")
	if s := check(t, r, "server"); !strings.Contains(s.Summary, "reference") {
		t.Errorf("server summary must name its source: %q", s.Summary)
	}
	expect(t, check(t, r, "bank"), OK, "323")
	expect(t, check(t, r, "untagged"), Warn, "2 of 3")
	ref := check(t, r, "reference")
	expect(t, ref, Info, "M4")
	if !strings.Contains(ref.Detail, "npm-plugin-hook") {
		t.Errorf("pieces missing from detail: %q", ref.Detail)
	}
	expect(t, check(t, r, "retention"), OK, "3650 days")
	if r.Errors != 0 || r.Server == nil || r.Server.Source != "reference" {
		t.Errorf("%+v", r)
	}
}

func TestAllObservationsTagged(t *testing.T) {
	f := newFixture(t)
	f.reference(fakeServer(t, `["project:a"]`, `["project:b","source:note"]`).URL)
	expect(t, check(t, f.run(), "untagged"), OK, "every")
}

func TestServerDown(t *testing.T) {
	f := newFixture(t)
	srv := fakeServer(t)
	url := srv.URL
	srv.Close()
	f.reference(url)
	r := f.run()
	expect(t, check(t, r, "server"), Error, "unreachable")
	for _, id := range []string{"bank", "untagged"} {
		expect(t, check(t, r, id), Info, "skipped")
	}
	if r.Errors != 1 {
		t.Errorf("errors = %d", r.Errors)
	}
}

func TestServerRejectsKey(t *testing.T) {
	f := newFixture(t)
	f.reference(fakeServer(t).URL)
	put(t, filepath.Join(f.home, ".hindsight", "coding-agent.json"), `{"apiUrl":"`+fakeServer(t).URL+`","apiToken":"wrong","bankId":"coding-agent::memoire"}`, 0o600)
	c := check(t, f.run(), "server")
	expect(t, c, Error, "API key")
	if strings.Contains(c.Summary+c.Detail+c.Fix, "wrong") {
		t.Errorf("token printed: %+v", c)
	}
}

func TestMissingBank(t *testing.T) {
	f := newFixture(t)
	srv := fakeServer(t)
	f.reference(srv.URL)
	put(t, filepath.Join(f.home, ".hindsight", "coding-agent.json"), `{"apiUrl":"`+srv.URL+`","apiToken":"fakevalue","bankId":"other"}`, 0o600)
	r := f.run()
	expect(t, check(t, r, "bank"), Error, "other")
	expect(t, check(t, r, "untagged"), Info, "skipped")
}

func TestBackSightConfigWinsOverReference(t *testing.T) {
	f := newFixture(t)
	srv := fakeServer(t)
	f.reference("http://127.0.0.1:1")
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	r := f.run()
	expect(t, check(t, r, "config"), OK, f.env.Dirs.ConfigFile())
	expect(t, check(t, r, "server"), OK, "config")
	if r.Server.URL != srv.URL {
		t.Errorf("target %+v", r.Server)
	}
}

func TestInvalidConfig(t *testing.T) {
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[recall]\nmin_scor = 1\n", 0o600)
	r := f.run()
	c := check(t, r, "config")
	expect(t, c, Error, "invalid")
	if !strings.Contains(c.Detail, "recall.min_scor") {
		t.Errorf("detail must name the key: %q", c.Detail)
	}
}

func TestSecretsReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o644)
	c := check(t, f.run(), "config")
	expect(t, c, Error, "secrets")
	if !strings.Contains(c.Fix, "chmod 600") || strings.Contains(c.Summary+c.Detail+c.Fix, "fakevalue") {
		t.Errorf("%+v", c)
	}
}

func TestDockerStates(t *testing.T) {
	f := newFixture(t)
	f.env.Docker = func(context.Context) (string, error) { return "", exec.ErrNotFound }
	expect(t, check(t, f.run(), "docker"), Warn, "not installed")
	f.env.Docker = func(context.Context) (string, error) { return "", errors.New("Cannot connect to the Docker daemon") }
	c := check(t, f.run(), "docker")
	expect(t, c, Warn, "not running")
	if !strings.Contains(c.Detail, "Cannot connect") {
		t.Errorf("detail: %q", c.Detail)
	}
}

func TestRetentionManagedByPolicy(t *testing.T) {
	f := newFixture(t)
	put(t, filepath.Join(f.env.ManagedDir, "managed-settings.json"), `{"cleanupPeriodDays": 14}`, 0o644)
	c := check(t, f.run(), "retention")
	expect(t, c, Warn, "14 days")
	if !strings.Contains(c.Fix, "administrator") {
		t.Errorf("a managed value is not the user's to change: %q", c.Fix)
	}
}

func TestRetentionReportsTranscripts(t *testing.T) {
	f := newFixture(t)
	p := filepath.Join(f.home, ".claude", "projects", "-home-u-a", "s1.jsonl")
	put(t, p, strings.Repeat("x", 2048), 0o600)
	old := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	_ = os.Chtimes(p, old, old)
	c := check(t, f.run(), "retention")
	if !strings.Contains(c.Detail, "1 transcript") || !strings.Contains(c.Detail, "2026-09-18") {
		t.Errorf("detail: %q", c.Detail)
	}
}

func TestGPUReported(t *testing.T) {
	f := newFixture(t)
	f.env.GPUs = func() []string { return []string{"AMD ROCm (/dev/kfd)"} }
	c := check(t, f.run(), "gpu")
	expect(t, c, Info, "AMD ROCm")
	if !strings.Contains(c.Summary, "v2") {
		t.Errorf("must say GPU use comes in v2: %q", c.Summary)
	}
}

func TestDetectGPUsLinux(t *testing.T) {
	dev := t.TempDir()
	put(t, filepath.Join(dev, "kfd"), "", 0o600)
	put(t, filepath.Join(dev, "dri", "renderD128"), "", 0o600)
	got := strings.Join(detectGPUs("linux", "amd64", dev), "|")
	if !strings.Contains(got, "ROCm") || !strings.Contains(got, "renderD128") || strings.Contains(got, "NVIDIA") {
		t.Errorf("got %q", got)
	}
	if got := detectGPUs("darwin", "arm64", dev); len(got) != 1 || !strings.Contains(got[0], "Metal") {
		t.Errorf("darwin arm64: %v", got)
	}
}
