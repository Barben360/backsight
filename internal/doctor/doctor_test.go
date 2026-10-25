package doctor

import (
	"context"
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

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/reference"
	"github.com/Barben360/backsight/internal/state"
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
	expect(t, check(t, r, "config"), Info, "not set up")
	expect(t, check(t, r, "server"), Info, "no server")
	expect(t, check(t, r, "reference"), OK, "no reference install")
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
	if !strings.Contains(c.Detail, "unencrypted") {
		t.Errorf("detail: %q", c.Detail)
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

// After the cut-over only leftovers remain: the check is OK, says the
// timer is disabled, and counts nothing as wired.
func TestCheckReferenceLeftovers(t *testing.T) {
	timer := reference.Piece{ID: "python-retro-timer", Where: "systemd memory-retro.timer, disabled", ReplacedIn: "M4"}
	c := checkReference(&reference.Install{Leftovers: []reference.Piece{timer}}, nil)
	if c.Status != OK || strings.Contains(c.Summary, "pieces wired") || !strings.Contains(c.Detail, "left on disk: python-retro-timer (systemd memory-retro.timer, disabled)") {
		t.Errorf("%+v", c)
	}
	hook := reference.Piece{ID: "python-search-hook", Where: "settings.json Stop", ReplacedIn: "M4"}
	c = checkReference(&reference.Install{Pieces: []reference.Piece{hook}, Leftovers: []reference.Piece{timer}}, nil)
	if c.Status != Info || !strings.HasPrefix(c.Summary, "1 reference pieces wired") || !strings.Contains(c.Detail, "python-search-hook (settings.json Stop); left on disk: python-retro-timer") {
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

// With backsight's own config, a bank whose settings lost backsight's
// template warns with the fix; the reference install's bank is not judged.
func TestCheckBankConfigDrift(t *testing.T) {
	f := newFixture(t)
	srv := fakeServerWith(t, map[string]reply{"/v1/default/banks/coding-agent%3A%3Amemoire/config": {200, `{"config":{},"overrides":{"enable_observations":false}}`}})
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	c := check(t, f.run(), "bank-config")
	if c.Status != Warn || !strings.Contains(c.Detail, "enable_observations") || !strings.Contains(c.Fix, "bank apply --force") {
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

// A client behind the bank's restore epoch warns with the fix (its cursors
// may hold as sent what the restore lost); on the same epoch it is ok, and
// an unreachable server skips the check.
func TestCheckRestoreEpoch(t *testing.T) {
	epochPath := "/v1/default/banks/coding-agent%3A%3Amemoire/documents/backsight%3Aepoch"
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

	g := newFixture(t) // no epoch document: epoch 0, which a new client is on
	put(t, g.env.Dirs.ConfigFile(), "[server]\nurl = \""+fakeServer(t).URL+"\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	put(t, g.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	expect(t, check(t, g.run(), "restore-epoch"), OK, "epoch 0")

	put(t, g.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\nbank = \"coding-agent::memoire\"\n", 0o600)
	expect(t, check(t, g.run(), "restore-epoch"), Info, "skipped")
}
