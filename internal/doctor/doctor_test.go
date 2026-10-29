package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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

	"filippo.io/age"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/guide"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/reference"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/target"
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
		FreeBytes:  func(string) (uint64, error) { return 50e9, nil },
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
	return fakeServerWith(t, nil, observations...)
}

// fakeServerWith answers paths in overrides with their status and body first.
func fakeServerWith(t *testing.T, overrides map[string]reply, observations ...string) *httptest.Server {
	srv := httptest.NewServer(fakeHandler(overrides, observations...))
	t.Cleanup(srv.Close)
	return srv
}

func fakeHandler(overrides map[string]reply, observations ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fakevalue" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if o, ok := overrides[r.URL.EscapedPath()]; ok {
			w.WriteHeader(o.status)
			_, _ = w.Write([]byte(o.body))
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
	})
}

type reply struct {
	status int
	body   string
}

const (
	statsPath = "/v1/default/banks/coding-agent%3A%3Amemoire/stats"
	listPath  = "/v1/default/banks/coding-agent%3A%3Amemoire/memories/list"
)

func TestNothingInstalled(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	cfg := check(t, r, "config")
	expect(t, cfg, Info, "not set up")
	if !strings.Contains(cfg.Detail, "backsight setup") || strings.Contains(cfg.Detail, "M4") {
		t.Errorf("config detail must point at setup: %q", cfg.Detail)
	}
	expect(t, check(t, r, "server"), Info, "no server")
	expect(t, check(t, r, "reference"), OK, "no older Hindsight install")
	expect(t, check(t, r, "docker"), OK, "28.1.1")
	ret := check(t, r, "retention")
	expect(t, ret, Warn, "30 days")
	if !strings.Contains(ret.Fix, "backsight client retention") || !strings.Contains(ret.Fix, "3650") {
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
	expect(t, ref, Info, "backsight client retire unwires them")
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

// The control bank holds restore's epoch line, untagged by design: the
// untagged count reads the memory bank alone, so it never warns for it.
func TestUntaggedIgnoresTheControlBank(t *testing.T) {
	f := newFixture(t)
	control := "/v1/default/banks/coding-agent%3A%3Amemoire-control/memories/list"
	f.reference(fakeServerWith(t, map[string]reply{
		"/v1/default/banks": {200, `{"banks":[{"bank_id":"coding-agent::memoire"},{"bank_id":"coding-agent::memoire-control"}]}`},
		control:             {200, `{"items":[{"id":"c1","fact_type":"observation","tags":[]}],"total":1}`},
	}, `["project:a"]`).URL)
	r := f.run()
	expect(t, check(t, r, "untagged"), OK, "every observation has a project tag (1)")
	expect(t, check(t, r, "bank"), OK, "323")
}

func TestServerDown(t *testing.T) {
	f := newFixture(t)
	srv := fakeServer(t)
	url := srv.URL
	srv.Close()
	f.reference(url)
	r := f.run()
	srvCheck := check(t, r, "server")
	expect(t, srvCheck, Error, "unreachable")
	// backsight server start manages backsight's own server, not this one.
	if !strings.Contains(srvCheck.Fix, "older Hindsight server") || strings.Contains(srvCheck.Fix, "backsight server") {
		t.Errorf("fix for a reference server: %q", srvCheck.Fix)
	}
	for _, id := range []string{"bank", "untagged"} {
		expect(t, check(t, r, id), Info, "skipped")
	}
	if r.Errors != 1 {
		t.Errorf("errors = %d", r.Errors)
	}
}

// The server setup configured is down: the fix names the commands that
// manage it.
func TestConfiguredServerDown(t *testing.T) {
	f := newFixture(t)
	srv := fakeServer(t)
	url := srv.URL
	srv.Close()
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+url+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	c := check(t, f.run(), "server")
	expect(t, c, Error, "unreachable")
	for _, want := range []string{"backsight server status", "backsight server start"} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix lacks %q: %q", want, c.Fix)
		}
	}
	if strings.Contains(c.Fix, "docker compose") || strings.Contains(c.Fix, "reference") {
		t.Errorf("fix still names the reference stack: %q", c.Fix)
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
	if !strings.Contains(c.Detail, "Cannot connect") || !strings.Contains(c.Fix, "systemctl start docker") || strings.Contains(c.Fix, "usermod") {
		t.Errorf("daemon down: %+v", c)
	}
	// Without the docker group the fix is the group, not a start.
	f.env.Docker = func(context.Context) (string, error) {
		return "", errors.New("docker info: permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock")
	}
	c = check(t, f.run(), "docker")
	expect(t, c, Warn, "cannot use it without sudo")
	if !strings.Contains(c.Fix, "sudo usermod -aG docker $USER") || !strings.Contains(c.Fix, "log out") {
		t.Errorf("permission: %+v", c)
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
	if !strings.Contains(c.Summary, "not used: backsight runs on the CPU") {
		t.Errorf("must say the GPU is not used: %q", c.Summary)
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

func TestServerFailureBranches(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]reply
		check     string
		status    Status
		inSummary string
	}{
		{"failed operations", map[string]reply{statsPath: {200, `{"total_nodes":3,"failed_operations":2}`}}, "bank", Warn, "bank coding-agent::memoire"},
		{"failed consolidation", map[string]reply{statsPath: {200, `{"total_nodes":3,"failed_consolidation":1}`}}, "bank", Warn, "bank coding-agent::memoire"},
		{"stats error", map[string]reply{statsPath: {500, `boom`}}, "bank", Error, "cannot read bank"},
		{"banks error", map[string]reply{"/v1/default/banks": {500, `boom`}}, "bank", Error, "cannot list banks"},
		{"list error", map[string]reply{listPath: {500, `boom`}}, "untagged", Error, "cannot list observations"},
		{"version fails", map[string]reply{"/version": {500, `boom`}}, "server", Error, "answered with an error"},
		{"health 503", map[string]reply{"/health": {503, `migrating`}}, "server", Error, "answered with an error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.reference(fakeServerWith(t, c.overrides).URL)
			r := f.run()
			got := check(t, r, c.check)
			expect(t, got, c.status, c.inSummary)
			if c.status == Warn && (got.Detail == "" || got.Fix == "") {
				t.Errorf("a warning needs a detail and a fix: %+v", got)
			}
		})
	}
}

func TestInvalidReferenceURL(t *testing.T) {
	f := newFixture(t)
	f.reference("ftp://x")
	r := f.run()
	expect(t, check(t, r, "server"), Error, "unusable server URL")
	expect(t, check(t, r, "bank"), Info, "skipped")
}

func TestReferenceURLCredentialsNeverPrinted(t *testing.T) {
	f := newFixture(t)
	f.reference("http://u:fakepw@127.0.0.1:1")
	r := f.run()
	for _, c := range r.Checks {
		if strings.Contains(c.Summary+c.Detail+c.Fix, "fakepw") {
			t.Errorf("password printed in %+v", c)
		}
	}
}

// The fake server listens on this machine's first non-loopback address, so
// the key would cross a network: no DNS, no outside traffic.
func TestCleartextKeyWarned(t *testing.T) {
	ip := localNonLoopback(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", ip, err)
	}
	srv := httptest.NewUnstartedServer(fakeHandler(nil))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	f := newFixture(t)
	f.reference("http://" + ln.Addr().String())
	c := check(t, f.run(), "server")
	expect(t, c, Warn, "Hindsight 1.1.0")
	if !strings.Contains(c.Detail, "unencrypted") || strings.Contains(c.Fix, "TLS") || !strings.Contains(c.Fix, "backsight server expose --private <tailnet address>") {
		t.Errorf("detail %q, fix %q", c.Detail, c.Fix)
	}
}

// How the key travels, once the server answered: loopback and https say
// nothing; a tailnet address is Info (WireGuard encrypts it); any other
// host warns, with a fix that exists today (no TLS proxy promised).
func TestServerTransport(t *testing.T) {
	for url, want := range map[string]Status{
		"http://127.0.0.1:8888":       OK,
		"https://mem.example:443":     OK,
		"http://100.101.102.103:8888": Info,
		"http://host.tail1234.ts.net": Info,
		"http://192.168.1.10:8888":    Warn,
	} {
		st, detail, fix := transport(target.Target{URL: url})
		if st != want || strings.Contains(fix, "TLS") || (st != OK) != (detail != "") {
			t.Errorf("%s: %s %q %q, want %s", url, st, detail, fix, want)
		}
		if st == Info && !strings.Contains(detail, "WireGuard") {
			t.Errorf("%s: %q", url, detail)
		}
	}
}

func localNonLoopback(t *testing.T) string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return ""
}

// A valid config.toml without secrets.toml is a server without a key.
func TestConfigWithoutSecretsIsValid(t *testing.T) {
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nbank = \"b\"\n", 0o600)
	expect(t, check(t, f.run(), "config"), OK, "config")
}

func TestRetentionBoundary(t *testing.T) {
	for days, want := range map[int]Status{364: Warn, 365: OK} {
		f := newFixture(t)
		put(t, filepath.Join(f.home, ".claude", "settings.json"), `{"cleanupPeriodDays": `+strconv.Itoa(days)+`}`, 0o600)
		c := check(t, f.run(), "retention")
		if c.Status != want {
			t.Errorf("%d days: %s", days, c.Status)
		}
		if want == Warn && !strings.Contains(c.Fix, filepath.Join(f.home, ".claude", "settings.json")) {
			t.Errorf("fix must name the user's settings file: %q", c.Fix)
		}
	}
}

func TestRetentionCorruptSettings(t *testing.T) {
	f := newFixture(t)
	put(t, filepath.Join(f.home, ".claude", "settings.json"), `{"cleanupPeriodDays": `, 0o600)
	c := check(t, f.run(), "retention")
	expect(t, c, Warn, "cannot read")
	if !strings.Contains(c.Detail, "settings.json") {
		t.Errorf("detail must name the file: %q", c.Detail)
	}
}

func TestMaintenanceLock(t *testing.T) {
	f := newFixture(t)
	expect(t, check(t, f.run(), "maintenance"), OK, "no maintenance")
	if _, err := os.Stat(f.env.Dirs.MaintenanceLock()); err == nil {
		t.Fatal("doctor must not leave a lock behind")
	}

	l, err := state.TryAcquire(f.env.Dirs.MaintenanceLock(), "backup create")
	if err != nil {
		t.Fatal(err)
	}
	expect(t, check(t, f.run(), "maintenance"), Info, "backup create")
	_ = l.Release()

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	put(t, f.env.Dirs.MaintenanceLock(), `{"pid":`+strconv.Itoa(cmd.Process.Pid)+`,"command":"restore","since":"2026-10-24T08:00:00Z"}`, 0o600)
	c := check(t, f.run(), "maintenance")
	expect(t, c, Warn, "stale")
	if _, err := os.Stat(f.env.Dirs.MaintenanceLock()); err != nil {
		t.Error("a stale lock is reported, never removed by doctor")
	}
	if !strings.Contains(c.Fix, f.env.Dirs.MaintenanceLock()) {
		t.Errorf("fix must name the lock: %q", c.Fix)
	}
}

func TestDiskThresholds(t *testing.T) {
	for free, want := range map[uint64]Status{50e9: OK, 1e9: Warn, 100e6: Error} {
		f := newFixture(t)
		f.env.FreeBytes = func(string) (uint64, error) { return free, nil }
		if c := check(t, f.run(), "disk"); c.Status != want {
			t.Errorf("%d bytes free: %s %q", free, c.Status, c.Summary)
		}
	}
	f := newFixture(t)
	f.env.FreeBytes = func(string) (uint64, error) { return 0, errors.New("statfs: not supported") }
	expect(t, check(t, f.run(), "disk"), Info, "unknown")
}

func TestExistingParent(t *testing.T) {
	dir := t.TempDir()
	if got := existingParent(filepath.Join(dir, "a", "b")); got != dir {
		t.Errorf("got %s, want %s", got, dir)
	}
}

func TestFreeBytesOnRealDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not implemented on Windows")
	}
	if n, err := FreeBytes(t.TempDir()); err != nil || n == 0 {
		t.Errorf("%d %v", n, err)
	}
}

func TestDetectGPUsOtherCases(t *testing.T) {
	dev := t.TempDir()
	if got := detectGPUs("linux", "amd64", dev); len(got) != 0 {
		t.Errorf("empty /dev: %v", got)
	}
	put(t, filepath.Join(dev, "nvidiactl"), "", 0o600)
	if got := detectGPUs("linux", "amd64", dev); len(got) != 1 || !strings.Contains(got[0], "NVIDIA") {
		t.Errorf("nvidia: %v", got)
	}
	for _, osArch := range [][2]string{{"windows", "amd64"}, {"darwin", "amd64"}} {
		if got := detectGPUs(osArch[0], osArch[1], dev); got != nil {
			t.Errorf("%v: %v", osArch, got)
		}
	}
}

func TestWritesCheck(t *testing.T) {
	f := newFixture(t)
	expect(t, check(t, f.run(), "writes"), OK, "0 note(s) queued")
	d := f.env.Dirs
	put(t, filepath.Join(d.OutboxDir(), "notes.jsonl"), `{"operation_id":"a","content":"x"}`+"\n", 0o600)
	put(t, filepath.Join(d.CursorsDir(), "s1.json"), `{"quarantined":true}`, 0o600)
	c := check(t, f.run(), "writes")
	expect(t, c, Warn, "1 note(s) queued, 1 session(s)")
	if !strings.Contains(c.Fix, "retry-quarantine") {
		t.Errorf("fix: %q", c.Fix)
	}
}

// hooks.outbox_warn_mb and hooks.outbox_max_mb set the thresholds.
func TestWritesCheckUsesConfiguredLimits(t *testing.T) {
	f := newFixture(t)
	d := f.env.Dirs
	put(t, filepath.Join(d.OutboxDir(), "notes.jsonl"), strings.Repeat("x", 1<<20+5), 0o600)
	expect(t, check(t, f.run(), "writes"), OK, "queued") // under the default 50 MB
	put(t, d.ConfigFile(), "[hooks]\noutbox_warn_mb = 1\noutbox_max_mb = 5\n", 0o600)
	c := check(t, f.run(), "writes")
	expect(t, c, Warn, "queued")
	if !strings.Contains(c.Fix, "server status") {
		t.Errorf("fix: %q", c.Fix)
	}
	put(t, d.ConfigFile(), "[hooks]\noutbox_warn_mb = 1\noutbox_max_mb = 1\n", 0o600)
	c = check(t, f.run(), "writes")
	expect(t, c, Error, "queued")
	if !strings.Contains(c.Fix, "outbox is full") {
		t.Errorf("fix: %q", c.Fix)
	}
}

// After the cut-over only leftovers remain: the check is OK, says the
// timer is disabled, and counts nothing as wired.
func TestCheckReferenceLeftovers(t *testing.T) {
	timer := reference.Piece{ID: "python-retro-timer", Where: "systemd memory-retro.timer, disabled", ReplacedIn: "M4"}
	c := checkReference(&reference.Install{Leftovers: []reference.Piece{timer}}, nil)
	if c.Status != OK || strings.Contains(c.Summary, "piece(s)") || !strings.Contains(c.Detail, "left on disk: python-retro-timer (systemd memory-retro.timer, disabled)") {
		t.Errorf("%+v", c)
	}
	hook := reference.Piece{ID: "python-search-hook", Where: "settings.json Stop", ReplacedIn: "M4"}
	c = checkReference(&reference.Install{Pieces: []reference.Piece{hook}, Leftovers: []reference.Piece{timer}}, nil)
	if c.Status != Info || !strings.HasPrefix(c.Summary, "1 piece(s) of an older Hindsight install wired") || !strings.Contains(c.Detail, "python-search-hook (settings.json Stop); left on disk: python-retro-timer") {
		t.Errorf("%+v", c)
	}
}

// A backfill paused on its budget is a warning with the way out; one in
// progress is information; a finished or absent one is OK.
func TestCheckBackfill(t *testing.T) {
	d := paths.Dirs{State: t.TempDir()}
	if c := checkBackfill(d); c.Status != OK || c.Summary != "no backfill pending" {
		t.Errorf("none: %+v", c)
	}
	set := func(b *state.Backfill) {
		if err := state.Update(d, "test", func(s *state.State) error { s.Backfill = b; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	set(&state.Backfill{Done: 0, Total: 2, Paused: "budget", SpentCost: 0.19, Currency: "USD", Sessions: []string{"a", "b"}})
	if c := checkBackfill(d); c.Status != Warn || !strings.Contains(c.Summary, "paused at 0/2: budget reached") || !strings.Contains(c.Fix, "--max-cost") {
		t.Errorf("budget: %+v", c)
	}
	set(&state.Backfill{Done: 1, Total: 2, Paused: "server", Sessions: []string{"b"}})
	if c := checkBackfill(d); c.Status != Warn || !strings.Contains(c.Summary, "paused at 1/2: server") {
		t.Errorf("server: %+v", c)
	}
	set(&state.Backfill{Done: 1, Total: 2, Sessions: []string{"b"}})
	if c := checkBackfill(d); c.Status != Info || !strings.Contains(c.Summary, "1/2") {
		t.Errorf("running: %+v", c)
	}
	set(&state.Backfill{Done: 2, Total: 2})
	if c := checkBackfill(d); c.Status != OK || !strings.Contains(c.Summary, "finished (2/2)") {
		t.Errorf("finished: %+v", c)
	}
}

// A session continued on two PCs is information, never a warning: nothing
// was lost, its shared part may be stored twice (decision 0073). Doctor
// names the sessions.
func TestSplitSessionIsInfo(t *testing.T) {
	f := newFixture(t)
	if c := check(t, f.run(), "split-session"); c.Status != OK || c.Summary != "no session continued on two PCs" {
		t.Errorf("none: %+v", c)
	}
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for _, s := range [][2]string{{"s1", "conversation:s1"}, {"s1", "conversation:s1@beta"}, {"s2", "conversation:s2"}} {
		if err := retain.RecordSplit(f.env.Dirs.State, s[0], s[1], at); err != nil {
			t.Fatal(err)
		}
	}
	c := check(t, f.run(), "split-session")
	if c.Status != Info || c.Summary != "2 session(s) continued on two PCs: their shared part may be stored twice" ||
		c.Detail != "s1, s2" || c.Fix != "" {
		t.Errorf("two: %+v", c)
	}
}

// unconfirmedEvent is a replace_unconfirmed telemetry line, as the flush
// writes it, at "at".
func unconfirmedEvent(at time.Time, session, document string) string {
	return `{"ts":"` + at.UTC().Format("2006-01-02T15:04:05.000Z") + `","event":"replace_unconfirmed","session":"` + session +
		`","document":"` + document + `","reason":"never readable","binary_version":"dev"}` + "\n"
}

// A session part given up unconfirmed in the last 30 days warns, naming it
// (its session's first 8 characters and its document): some of this PC's
// turns may be missing on the server. An older one is no longer reported.
func TestDoctorReplaceUnconfirmedWarns(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := newFixture(t)
	f.env.Now = func() time.Time { return now }
	dir := f.env.Dirs.TelemetryDir()
	old := now.Add(-40 * 24 * time.Hour) // September's file
	put(t, filepath.Join(dir, "events-"+old.Format("2006-01")+".jsonl"), unconfirmedEvent(old, "oldsession99", "conversation:oldsession99"), 0o600)
	if c := check(t, f.run(), "replace-unconfirmed"); c.Status != OK || c.Detail != "" {
		t.Errorf("only an old event: %+v", c)
	}
	recent := now.Add(-3 * 24 * time.Hour)
	put(t, filepath.Join(dir, "events-"+recent.Format("2006-01")+".jsonl"),
		`{"ts":"2026-10-25T11:00:00.000Z","event":"prompt","session":"s1"}`+"\n"+unconfirmedEvent(recent, "0123456789abcdef", "conversation:0123456789abcdef@beta"), 0o600)
	c := check(t, f.run(), "replace-unconfirmed")
	const ref = "troubleshooting#doctor-a-session-part-was-never-confirmed"
	if c.Status != Warn || c.Detail != "1 session part(s) never confirmed on the server since 2026-10-25: 01234567/conversation:0123456789abcdef@beta" ||
		c.Fix != "backsight guide "+ref {
		t.Errorf("recent: %+v", c)
	}
	corpus, err := guide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tp := range corpus.Topics {
		for _, s := range tp.Sections {
			found = found || s.Ref() == ref
		}
	}
	if !found {
		t.Errorf("the docs have no section %s", ref)
	}
}

// At most 5 parts are named, the newest first; the count is all of them.
func TestDoctorReplaceUnconfirmedNamesFive(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := newFixture(t)
	f.env.Now = func() time.Time { return now }
	var b strings.Builder
	for i := 1; i <= 6; i++ { // one a day: the oldest, 6 days ago, dates "since"
		b.WriteString(unconfirmedEvent(now.Add(-time.Duration(i)*24*time.Hour), "session"+strconv.Itoa(i)+"xyz", "conversation:s"+strconv.Itoa(i)))
	}
	put(t, filepath.Join(f.env.Dirs.TelemetryDir(), "events-2026-10.jsonl"), b.String(), 0o600)
	c := check(t, f.run(), "replace-unconfirmed")
	want := "6 session part(s) never confirmed on the server since 2026-10-22: session1/conversation:s1, session2/conversation:s2, session3/conversation:s3, session4/conversation:s4, session5/conversation:s5, …"
	if c.Status != Warn || c.Detail != want {
		t.Errorf("got %q\nwant %q", c.Detail, want)
	}
}

// The window is the last 30 days: a part given up 29 days ago warns, one
// given up 31 days ago does not.
func TestDoctorReplaceUnconfirmedWindowEdge(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := newFixture(t)
	f.env.Now = func() time.Time { return now }
	put31 := func(age time.Duration, session string) {
		at := now.Add(-age)
		p := filepath.Join(f.env.Dirs.TelemetryDir(), "events-"+at.Format("2006-01")+".jsonl")
		old, _ := os.ReadFile(p)
		put(t, p, string(old)+unconfirmedEvent(at, session, "conversation:"+session), 0o600)
	}
	put31(31*24*time.Hour, "day31")
	if c := check(t, f.run(), "replace-unconfirmed"); c.Status != OK {
		t.Errorf("31 days: %+v", c)
	}
	put31(29*24*time.Hour, "day29")
	c := check(t, f.run(), "replace-unconfirmed")
	if c.Status != Warn || c.Detail != "1 session part(s) never confirmed on the server since 2026-09-29: day29/conversation:day29" {
		t.Errorf("29 days: %+v", c)
	}
}

// Telemetry that cannot be read warns, saying so: the check cannot tell
// whether a part was given up.
func TestDoctorReplaceUnconfirmedUnreadableTelemetry(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := newFixture(t)
	f.env.Now = func() time.Time { return now }
	if err := os.MkdirAll(filepath.Join(f.env.Dirs.TelemetryDir(), "events-2026-10.jsonl"), 0o700); err != nil { // a directory: unreadable
		t.Fatal(err)
	}
	if c := check(t, f.run(), "replace-unconfirmed"); c.Status != Warn || c.Summary != "telemetry unreadable" || c.Detail == "" {
		t.Errorf("%+v", c)
	}
}

// A server-only host runs no flush: the check is not a client's there.
func TestDoctorReplaceUnconfirmedNotAClientOnServerOnlyHost(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := newFixture(t)
	f.env.Now = func() time.Time { return now }
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\nbank = \"coding-agent::memoire\"\nrole = \"server\"\n", 0o600)
	put(t, filepath.Join(f.env.Dirs.TelemetryDir(), "events-2026-10.jsonl"), unconfirmedEvent(now.Add(-time.Hour), "s1", "conversation:s1"), 0o600)
	r := f.run()
	n := 0
	for _, c := range r.Checks {
		if c.ID == "replace-unconfirmed" {
			n++
		}
	}
	if c := check(t, r, "replace-unconfirmed"); n != 1 || c.Status != Info || c.Summary != "not a client: server-only host" {
		t.Errorf("%d check(s), %+v", n, c)
	}
}

// Only a dead owner's lock is reported; a held one is not.
func TestCheckLocks(t *testing.T) {
	d := paths.Dirs{State: t.TempDir()}
	if c := checkLocks(d); c.Status != OK {
		t.Errorf("no locks dir: %+v", c)
	}
	l, err := state.TryAcquire(filepath.Join(d.LocksDir(), "job-tick.lock"), "jobs tick")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if c := checkLocks(d); c.Status != OK {
		t.Errorf("a live lock: %+v", c)
	}
	put(t, filepath.Join(d.LocksDir(), "job-backfill.lock"), `{"pid":999999,"command":"retain backfill","since":"2026-10-25T03:00:00Z"}`, 0o600)
	if c := checkLocks(d); c.Status != Warn || !strings.Contains(c.Detail, "job-backfill.lock (retain backfill, pid 999999)") || strings.Contains(c.Detail, "job-tick") {
		t.Errorf("a stale lock: %+v", c)
	}
}

// The newest backup's age: none or over 2 days warns (jobs maintain logs
// its run even when the backup in it fails); backups off is information.
func TestCheckBackupAge(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if c := checkBackup(f.env, now); c.Status != Info || !strings.Contains(c.Summary, "no backsight config") {
		t.Errorf("no config: %+v", c)
	}
	dir := filepath.Join(f.home, "bk")
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\n[backup]\ndir = \""+dir+"\"\n", 0o600)
	if c := checkBackup(f.env, now); c.Status != Warn || !strings.Contains(c.Summary, "no backup in") {
		t.Errorf("none: %+v", c)
	}
	put(t, filepath.Join(dir, backup.Name(now.Add(-72*time.Hour))), "x", 0o600)
	if c := checkBackup(f.env, now); c.Status != Warn || !strings.Contains(c.Summary, "last backup 3 days ago") || !strings.Contains(c.Fix, "backup create") {
		t.Errorf("3 days: %+v", c)
	}
	put(t, filepath.Join(dir, backup.Name(now.Add(-47*time.Hour))), "x", 0o600)
	if c := checkBackup(f.env, now); c.Status != OK || !strings.Contains(c.Summary, "2026-10-23 13:00 UTC, 2 kept") {
		t.Errorf("47 h: %+v", c)
	}
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\n[backup]\nenabled = false\n", 0o600)
	if c := checkBackup(f.env, now); c.Status != Info || !strings.Contains(c.Summary, "backups are off") {
		t.Errorf("off: %+v", c)
	}
}

// A client's memory is on the server's host, which backs it up: no
// archive here, or old ones from before connect, is information, never a
// warning whose fix (backup create) a client refuses.
func TestCheckBackupOnAClient(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(f.home, "bk")
	put(t, f.env.Dirs.ConfigFile(), "[server]\nmode = \"remote\"\nurl = \"http://192.168.1.20:8888\"\n[backup]\ndir = \""+dir+"\"\n", 0o600)
	want := "backups are made on the server's host (backsight backup create there)"
	if c := checkBackup(f.env, now); c.Status != Info || c.Summary != want || c.Fix != "" {
		t.Errorf("none: %+v", c)
	}
	put(t, filepath.Join(dir, backup.Name(now.Add(-30*24*time.Hour))), "x", 0o600)
	if c := checkBackup(f.env, now); c.Status != Info || c.Summary != want || c.Fix != "" {
		t.Errorf("an old archive of its own server: %+v", c)
	}
}

// archive writes a backup and its sidecar, encrypted for recipients.
func archive(t *testing.T, dir string, at time.Time, recipients ...string) {
	t.Helper()
	p := filepath.Join(dir, backup.Name(at))
	put(t, p, "x", 0o600)
	raw, _ := json.Marshal(backup.Manifest{Format: 1, CreatedAt: at, Bank: "claude-memory", Recipients: recipients})
	put(t, p+".json", string(raw), 0o600)
}

// After a reinstall, the archives of the previous install were made for a
// backup key this machine no longer has: they are not this install's
// backups, and doctor says so instead of ok.
func TestCheckBackupCountsOnlyArchivesThisMachineCanRead(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(f.home, "bk")
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\n[backup]\ndir = \""+dir+"\"\n", 0o600)
	old, _ := age.GenerateX25519Identity()
	mine, _ := age.GenerateX25519Identity()
	archive(t, dir, now.Add(-2*time.Hour), old.Recipient().String())

	// No identity at all (purged with the previous install).
	c := checkBackup(f.env, now)
	if c.Status != Warn || !strings.Contains(c.Summary, "no backup of this install") || !strings.Contains(c.Detail, "1 archive(s)") || !strings.Contains(c.Fix, "backup create") {
		t.Errorf("no identity: %+v", c)
	}
	// A new identity: the old archive is still not readable here.
	put(t, f.env.Dirs.SecretsFile(), "backup_identity = \""+mine.String()+"\"\n", 0o600)
	if c := checkBackup(f.env, now); c.Status != Warn || !strings.Contains(c.Summary, "no backup of this install") {
		t.Errorf("another identity: %+v", c)
	}
	// This machine's first backup counts; the old one is named apart.
	archive(t, dir, now.Add(-1*time.Hour), mine.Recipient().String())
	c = checkBackup(f.env, now)
	if c.Status != OK || !strings.Contains(c.Summary, "2026-10-25 11:00 UTC, 1 kept") || !strings.Contains(c.Detail, "1 archive(s)") {
		t.Errorf("one of ours: %+v", c)
	}
	// A configured recipient (the identity kept elsewhere) is this install's.
	other, _ := age.GenerateX25519Identity()
	archive(t, dir, now.Add(-30*time.Minute), other.Recipient().String())
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\n[backup]\ndir = \""+dir+"\"\nrecipient = \""+other.Recipient().String()+"\"\n", 0o600)
	if c := checkBackup(f.env, now); c.Status != OK || !strings.Contains(c.Summary, "11:30 UTC, 2 kept") {
		t.Errorf("configured recipient: %+v", c)
	}
}

// With backsight's own config, a bank whose settings lost backsight's
// template warns with the fix; the reference install's bank is not judged.
func TestCheckBankConfigDrift(t *testing.T) {
	f := newFixture(t)
	srv := fakeServerWith(t, map[string]reply{"/v1/default/banks/coding-agent%3A%3Amemoire/config": {200, `{"config":{},"overrides":{"enable_observations":false}}`}})
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	c := check(t, f.run(), "bank-config")
	if c.Status != Warn || !strings.Contains(c.Detail, "enable_observations") || !strings.Contains(c.Fix, "backsight bank apply") {
		t.Errorf("%+v", c)
	}
	g := newFixture(t)
	g.reference(srv.URL)
	for _, c := range g.run().Checks {
		if c.ID == "bank-config" {
			t.Errorf("reference bank judged: %+v", c)
		}
	}
}

// overridesOf is the overrides answer of a bank holding config.
func overridesOf(t *testing.T, config map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return `{"config":{},"overrides":` + string(raw) + `}`
}

// The bank-config check tells an older backsight template (the bank holds
// the one recorded as applied: bank apply) from a change made on the
// server (bank apply --force, which overwrites those edits), and says
// when it cannot tell (M5 review rules).
func TestCheckBankConfigTellsAnOlderTemplateFromAServerChange(t *testing.T) {
	current := server.Current()
	older := server.Current()
	strategies := older.Config["retain_strategies"].(map[string]any)
	delete(strategies, "subagent") // the template before 0071 C5
	edited := server.Current()
	edited.Config["enable_observations"] = false
	olderEdited := server.Current()
	delete(olderEdited.Config["retain_strategies"].(map[string]any), "subagent")
	olderEdited.Config["retain_mission"] = "the user's own"
	for _, tc := range []struct {
		name     string
		bank     map[string]any // what the server holds
		recorded string         // state.json's hash
		record   *server.Template
		status   Status
		summary  string
		fix      string
		notFix   string
	}{
		{"current, recorded", current.Config, current.Hash(), &current, OK, "match", "", ""},
		{"current, not recorded", current.Config, "", nil, Warn, "not recorded as applied", "backsight bank apply (records it)", "--force"},
		{"older template", older.Config, older.Hash(), &older, Warn, "an older backsight template", "backsight bank apply (sends", "--force"},
		{"changed on the server", edited.Config, current.Hash(), &current, Warn, "changed on the server", "bank apply --force (puts backsight's values back: it overwrites the edits", ""},
		{"older template changed on the server", olderEdited.Config, older.Hash(), &older, Warn, "changed on the server", "bank apply --force", ""},
		{"no record", older.Config, older.Hash(), nil, Warn, "an older backsight template, or a change made on the server", "it also overwrites any edit", "--force"},
		{"another template's record", older.Config, older.Hash(), &current, Warn, "or a change made on the server", "backsight bank apply", "--force"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			srv := fakeServerWith(t, map[string]reply{"/v1/default/banks/coding-agent%3A%3Amemoire/config": {200, overridesOf(t, tc.bank)}})
			put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
			put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
			if err := state.Save(f.env.Dirs.StateFile(), state.State{SchemaVersion: state.SchemaVersion, BankConfigHash: tc.recorded}); err != nil {
				t.Fatal(err)
			}
			if tc.record != nil {
				raw, _ := json.Marshal(tc.record)
				put(t, f.env.Dirs.BankTemplateFile(), string(raw), 0o600)
			}
			c := check(t, f.run(), "bank-config")
			if c.Status != tc.status || !strings.Contains(c.Summary, tc.summary) || !strings.Contains(c.Fix, tc.fix) || (tc.notFix != "" && strings.Contains(c.Fix, tc.notFix)) {
				t.Errorf("%+v", c)
			}
		})
	}
}

// On a client of a shared server the bank's settings are its host's: the
// check is info, says how they compare with this backsight's template,
// and its fix runs on the host. Nothing local (the recorded hash) counts.
func TestBankConfigOnAClientIsInfo(t *testing.T) {
	current, edited := server.Current(), server.Current()
	edited.Config["enable_observations"] = false
	for _, tc := range []struct {
		name   string
		bank   map[string]any
		detail string
		fix    string
	}{
		{"same template", current.Config, "bank settings match backsight's template", ""},
		{"another template", edited.Config, "enable_observations", "on the server's host: backsight bank apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			srv := fakeServerWith(t, map[string]reply{"/v1/default/banks/coding-agent%3A%3Amemoire/config": {200, overridesOf(t, tc.bank)}})
			put(t, f.env.Dirs.ConfigFile(), "[server]\nmode = \"remote\"\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
			put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
			c := check(t, f.run(), "bank-config")
			if c.Status != Info || !strings.Contains(c.Summary, "the server's host") || !strings.Contains(c.Detail, tc.detail) || c.Fix != tc.fix {
				t.Errorf("%+v", c)
			}
		})
	}
}

// A client behind the bank's restore epoch warns with the fix (its cursors
// may hold as sent what the restore lost); on the same epoch it is ok, and
// an unreachable server skips the check.
func TestCheckRestoreEpoch(t *testing.T) {
	epochPath := "/v1/default/banks/coding-agent%3A%3Amemoire-control/documents/backsight%3Aepoch"
	srv := fakeServerWith(t, map[string]reply{epochPath: {200, `{"id":"backsight:epoch","document_metadata":{"epoch":"2","restored_at":"2026-10-25T10:00:00Z","backup_created_at":"2026-10-25T03:00:00Z"}}`}})
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	c := check(t, f.run(), "restore-epoch")
	if c.Status != Warn || !strings.Contains(c.Summary, "client on an old restore epoch") || !strings.Contains(c.Summary, "epoch 2") || c.Fix != "backsight jobs tick" {
		t.Errorf("behind: %+v", c)
	}
	if err := state.Save(f.env.Dirs.StateFile(), state.State{SchemaVersion: state.SchemaVersion,
		RestoreEpoch: &state.RestoreEpoch{Bank: "coding-agent::memoire", Epoch: 2}}); err != nil {
		t.Fatal(err)
	}
	expect(t, check(t, f.run(), "restore-epoch"), OK, "epoch 2")
	// The same number from another restore (a host restored elsewhere
	// from an older archive): not reconciled yet.
	if err := state.Save(f.env.Dirs.StateFile(), state.State{SchemaVersion: state.SchemaVersion,
		RestoreEpoch: &state.RestoreEpoch{Bank: "coding-agent::memoire", Epoch: 2, RestoredAt: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)}}); err != nil {
		t.Fatal(err)
	}
	if c := check(t, f.run(), "restore-epoch"); c.Status != Warn || !strings.Contains(c.Summary, "restored again") || c.Fix != "backsight jobs tick" {
		t.Errorf("another restore: %+v", c)
	}

	g := newFixture(t) // no epoch document: epoch 0, which a new client is on
	put(t, g.env.Dirs.ConfigFile(), "[server]\nurl = \""+fakeServer(t).URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, g.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	expect(t, check(t, g.run(), "restore-epoch"), OK, "epoch 0")

	put(t, g.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	expect(t, check(t, g.run(), "restore-epoch"), Info, "skipped")
}

// A server-only host runs no Claude Code: the client checks say so, the
// server's own checks run as on any host.
func TestServerOnlyHostSkipsTheClientChecks(t *testing.T) {
	f := newFixture(t)
	srv := fakeServer(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\nrole = \"server\"\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	f.env.ClaudePlugins = func(string) ([]byte, error) {
		t.Error("doctor asked Claude Code on a server-only host")
		return nil, errors.New("no claude")
	}
	f.env.Scheduler = func() (SchedulerEntries, error) { return SchedulerEntries{Installed: true, Kind: "systemd"}, nil }
	r := f.run()
	seen := map[string]int{}
	for _, c := range r.Checks {
		seen[c.ID]++
	}
	for _, id := range append([]string{"client", "retention", "reference", "usage"}, ClientChecks...) {
		c := check(t, r, id)
		expect(t, c, Info, "not a client: server-only host")
		if seen[id] != 1 {
			t.Errorf("%s reported %d times", id, seen[id])
		}
	}
	expect(t, check(t, r, "server"), OK, "config")
	expect(t, check(t, r, "scheduler"), OK, "systemd entries match")
	for _, id := range []string{"backup", "pending-restore", "bank"} {
		if c := check(t, r, id); strings.Contains(c.Summary, "not a client") {
			t.Errorf("%s is a server check, reported %+v", id, c)
		}
	}
}

const serverIDPath = "/v1/default/banks/coding-agent%3A%3Amemoire-control/documents/backsight%3Aserver"

// boundClient is a client of srv that joined server id.
func boundClient(t *testing.T, srv *httptest.Server, mode, id string) *fixture {
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nmode = \""+mode+"\"\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	if id != "" {
		if err := state.Save(f.env.Dirs.StateFile(), state.State{SchemaVersion: state.SchemaVersion,
			RemoteServer: &state.RemoteServer{ID: id, URL: srv.URL}}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// A client whose address answers with another server's id, or a server
// with none, is in error: it sends nothing until connect runs again.
func TestServerIDMismatchIsAnError(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{serverIDPath: {200, `{"id":"backsight:server","document_metadata":{"server_id":"b"}}`}})
	c := check(t, boundClient(t, srv, "remote", "a").run(), "server-id")
	if c.Status != Error || !strings.Contains(c.Summary, "server b") || !strings.Contains(c.Summary, "joined a") ||
		c.Fix != "backsight connect --token-file FILE with a token from the server now at "+srv.URL+" (backsight server token there)" {
		t.Errorf("another id: %+v", c)
	}
	expect(t, check(t, boundClient(t, srv, "remote", "b").run(), "server-id"), OK, "server b")
	expect(t, check(t, boundClient(t, fakeServer(t), "remote", "a").run(), "server-id"), Error, "server none")
}

// Only a client bound to a server checks its id: an install before
// connect has no binding, and a machine running its own server is not
// bound to one.
func TestServerIDCheckOnlyOnABoundClient(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{serverIDPath: {200, `{"id":"backsight:server","document_metadata":{"server_id":"b"}}`}})
	for name, f := range map[string]*fixture{
		"remote, no binding": boundClient(t, srv, "remote", ""),
		"local, bound":       boundClient(t, srv, "local", "a"),
	} {
		for _, c := range f.run().Checks {
			if c.ID == "server-id" {
				t.Errorf("%s: %+v", name, c)
			}
		}
	}
}

// reachURL: a server that does not answer.
const reachURL = "http://127.0.0.1:1"

// reachingClient is a client of reachURL whose server last answered at
// lastOK and has failed since failing (zero: it answers), with one queued
// note and two sessions kept for the next tick.
func reachingClient(t *testing.T, now, lastOK, failing time.Time) *fixture {
	f := newFixture(t)
	f.env.Now = func() time.Time { return now }
	put(t, f.env.Dirs.ConfigFile(), "[server]\nmode = \"remote\"\nurl = \""+reachURL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	put(t, filepath.Join(f.env.Dirs.OutboxDir(), "notes.jsonl"), `{"operation_id":"a","content":"x"}`+"\n", 0o600)
	state.RecordReach(f.env.Dirs, true, lastOK)
	if !failing.IsZero() {
		state.RecordReach(f.env.Dirs, false, failing)
	}
	state.RecordWaiting(f.env.Dirs, 2)
	return f
}

// server-reach says when the server last answered and what waits on this
// PC; a silence over an hour warns, with where to look. Never an error:
// the server check carries a live failure.
func TestServerReachWarnsAfterAnHour(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	c := check(t, reachingClient(t, now, now.Add(-3*time.Hour), now.Add(-59*time.Minute)).run(), "server-reach")
	want := "last answer 2026-10-28 09:00 UTC; breaker closed; 1 note(s) queued; 2 session(s) waiting"
	if c.Status != Info || c.Summary != want || c.Fix != "" {
		t.Errorf("59 min: %+v", c)
	}
	f := reachingClient(t, now, now.Add(-3*time.Hour), now.Add(-61*time.Minute))
	put(t, filepath.Join(f.env.Dirs.State, "breaker.json"), `{"open_until":"2026-10-28T12:05:00Z"}`, 0o600)
	c = check(t, f.run(), "server-reach")
	want = "last answer 2026-10-28 09:00 UTC; breaker open until 2026-10-28 12:05 UTC; 1 note(s) queued; 2 session(s) waiting"
	if c.Status != Warn || c.Summary != want ||
		c.Fix != "the server at "+reachURL+" has not answered since 2026-10-28 10:59 UTC: check it on its host (backsight server status there) or the network; nothing is lost" {
		t.Errorf("61 min: %+v", c)
	}
	c = check(t, reachingClient(t, now, now.Add(-5*time.Minute), time.Time{}).run(), "server-reach")
	if c.Status != Info || !strings.HasPrefix(c.Summary, "last answer 2026-10-28 11:55 UTC; breaker closed;") {
		t.Errorf("answering: %+v", c)
	}
}

// A server that answers doctor now is not reported silent, whatever the
// record says until the next recall or tick catches up.
func TestServerReachAnswersNowAfterASilence(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := reachingClient(t, now, now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+fakeServer(t).URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	r := f.run()
	expect(t, check(t, r, "server"), OK, "Hindsight")
	c := check(t, r, "server-reach")
	if c.Status != Info || c.Fix != "" ||
		c.Summary != "last answer now; it was silent from 2026-10-28 10:00 UTC; breaker closed; 1 note(s) queued; 2 session(s) waiting" {
		t.Errorf("answers now: %+v", c)
	}
}

// A server-only host runs no hooks: its reach is not a client's.
func TestServerReachIsNotAClientCheckOnAServerOnlyHost(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := reachingClient(t, now, now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\nbank = \"coding-agent::memoire\"\nrole = \"server\"\n", 0o600)
	r := f.run()
	n := 0
	for _, c := range r.Checks {
		if c.ID == "server-reach" {
			n++
		}
	}
	if c := check(t, r, "server-reach"); n != 1 || c.Status != Info || c.Summary != "not a client: server-only host" {
		t.Errorf("%d server-reach check(s), %+v", n, c)
	}
}

// A client compares its server's Hindsight version with its pin and warns
// on a skew, naming the fix; a PC running its own server has no skew.
func TestVersionSkewOnlyOnAClient(t *testing.T) {
	srv := fakeServerWith(t, map[string]reply{"/version": {200, `{"api_version":"1.2.0"}`}})
	c := check(t, boundClient(t, srv, "remote", "").run(), "version-skew")
	if c.Status != Warn || c.Summary != "server runs Hindsight 1.2.0, this backsight pins "+server.PinnedVersion() ||
		!strings.Contains(c.Fix, "update apply") {
		t.Errorf("remote: %+v", c)
	}
	for _, ch := range boundClient(t, srv, "local", "").run().Checks {
		if ch.ID == "version-skew" {
			t.Errorf("local: %+v", ch)
		}
	}
}
