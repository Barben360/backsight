package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
)

// syncBuf is a buffer a command writes while a test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var dashLine = regexp.MustCompile(`^Dashboard: http://127\.0\.0\.1:(\d+)/\?code=([0-9a-f]{32})$`)

// startDashboard runs `backsight dashboard args` on viewsFixture in the
// background and returns once its link is printed: the base URL, the
// directories, the first stdout (the link line, or the JSON envelope), the
// stderr so far and stop, which interrupts the command and returns its exit
// code.
func startDashboard(t *testing.T, args ...string) (base string, d paths.Dirs, first string, stderr *syncBuf, stop func() int) {
	t.Helper()
	base, d, first, stderr, _, stop = startDashboardOut(t, args...)
	return
}

// startDashboardOut is startDashboard that also returns the whole stdout.
func startDashboardOut(t *testing.T, args ...string) (base string, d paths.Dirs, first string, stderr, out *syncBuf, stop func() int) {
	t.Helper()
	viewsFixture(t)
	buildIndex(t)
	return serveDashboard(t, args...)
}

// buildIndex builds the metrics index, so that pages answer 200 at once.
func buildIndex(t *testing.T) {
	t.Helper()
	if code, _, errOut := run(t, "stats", "overview"); code != 0 {
		t.Fatalf("stats overview: exit %d: %s", code, errOut)
	}
}

// serveDashboard is startDashboardOut on the machine as it stands: a
// fixture already set up, the index built or not.
func serveDashboard(t *testing.T, args ...string) (base string, d paths.Dirs, first string, stderr, out *syncBuf, stop func() int) {
	t.Helper()
	d, _, err := machine()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr = &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() {
		exit <- ExecuteContext(ctx, append([]string{"dashboard"}, args...), pw, stderr)
		_ = pw.Close()
	}()
	out = &syncBuf{}
	ready := make(chan struct{})
	go func() {
		var once sync.Once
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			_, _ = out.Write(buf[:n])
			if s := out.String(); strings.HasSuffix(s, "\n") && (strings.HasPrefix(s, "Dashboard:") || json.Valid([]byte(s))) {
				once.Do(func() { close(ready) })
			}
			if err != nil {
				once.Do(func() { close(ready) })
				return
			}
		}
	}()
	var stopped bool
	var code int
	stop = func() int {
		if !stopped {
			cancel()
			code, stopped = <-exit, true
			_ = pr.Close()
		}
		return code
	}
	t.Cleanup(func() { stop() })
	<-ready
	first = out.String()
	port := ""
	if m := dashLine.FindStringSubmatch(strings.TrimSpace(strings.SplitN(first, "\n", 2)[0])); m != nil {
		port = m[1]
	} else {
		var env struct{ Data struct{ Port int } }
		if json.Unmarshal([]byte(first), &env) != nil || env.Data.Port == 0 {
			t.Fatalf("no link on stdout: %q (stderr %q)", first, stderr.String())
		}
		port = strconv.Itoa(env.Data.Port)
	}
	return "http://127.0.0.1:" + port, d, first, stderr, out, stop
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// dashLogin signs in with the code of a link and returns the session cookie.
func dashLogin(t *testing.T, base, link string) *http.Cookie {
	t.Helper()
	code := strings.TrimSpace(link[strings.Index(link, "code=")+len("code="):])
	resp, err := noFollow.Get(base + "/?code=" + code)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 {
		t.Fatalf("login: status %d, %d cookies", resp.StatusCode, len(resp.Cookies()))
	}
	return resp.Cookies()[0]
}

func getWith(t *testing.T, url string, c *http.Cookie) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

type linkEnv struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Data    struct {
		URL       string `json:"url"`
		Port      int    `json:"port"`
		ExpiresAt string `json:"expires_at"`
	} `json:"data"`
}

func decodeLink(t *testing.T, s string) linkEnv {
	t.Helper()
	var env linkEnv
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		t.Fatalf("%v: %q", err, s)
	}
	return env
}

func TestDashboardPrintsTheLink(t *testing.T) {
	base, _, first, stderr, _ := startDashboard(t)
	line := strings.TrimSuffix(first, "\n")
	m := dashLine.FindStringSubmatch(line)
	if m == nil || strings.Count(first, "\n") != 1 {
		t.Fatalf("stdout %q", first)
	}
	if !strings.HasPrefix(line, "Dashboard: "+base+"/") {
		t.Errorf("%q is not on %s", line, base)
	}
	c := dashLogin(t, base, line)
	if got := getWith(t, base+"/", c); got != http.StatusOK {
		t.Errorf("/ with the session: %d", got)
	}
	if got := getWith(t, base+"/?code="+m[2], nil); got != http.StatusUnauthorized {
		t.Errorf("the code again: %d, want 401", got)
	}
	if !strings.Contains(stderr.String(), "Ctrl+C stops the dashboard") {
		t.Errorf("stderr %q", stderr.String())
	}
	if strings.Contains(stderr.String(), m[2]) {
		t.Errorf("stderr holds the code: %q", stderr.String())
	}
}

func TestDashboardJSON(t *testing.T) {
	_, _, first, _, _ := startDashboard(t, "--json")
	env := decodeLink(t, first)
	m := dashLine.FindStringSubmatch("Dashboard: " + env.Data.URL)
	if env.Command != "dashboard" || !env.OK || m == nil {
		t.Fatalf("%+v", env)
	}
	if strconv.Itoa(env.Data.Port) != m[1] {
		t.Errorf("port %d, url %s", env.Data.Port, env.Data.URL)
	}
	if env.Data.ExpiresAt != "2026-10-08T12:02:00Z" {
		t.Errorf("expires_at %q", env.Data.ExpiresAt)
	}
}

func TestSecondDashboardRefused(t *testing.T) {
	base, _, _, _, _ := startDashboard(t)
	port := base[strings.LastIndex(base, ":")+1:]
	want := "a dashboard is already running at " + base + "/: run backsight dashboard link for a new link"
	for _, args := range [][]string{{"dashboard"}, {"dashboard", "--port", port}} {
		code, out, errOut := run(t, args...)
		if code != output.Blocked || out != "" || !strings.Contains(errOut, ": "+want+"\n") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}

func TestDashboardLinkWorks(t *testing.T) {
	base, _, _, _, stop := startDashboard(t)
	code, out, errOut := run(t, "dashboard", "link")
	line := strings.TrimSuffix(out, "\n")
	m := dashLine.FindStringSubmatch(line)
	if code != 0 || m == nil || "http://127.0.0.1:"+m[1] != base {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	c := dashLogin(t, base, line)
	if got := getWith(t, base+"/", c); got != http.StatusOK {
		t.Errorf("/ after the link's login: %d", got)
	}
	code, out, _ = run(t, "dashboard", "link", "--json")
	env := decodeLink(t, out)
	if code != 0 || env.Command != "dashboard link" || "http://127.0.0.1:"+strconv.Itoa(env.Data.Port) != base || env.Data.ExpiresAt != "2026-10-08T12:02:00Z" {
		t.Errorf("exit %d, %+v", code, env)
	}
	if stop() != 0 {
		t.Fatal("the dashboard did not stop cleanly")
	}
	code, out, errOut = run(t, "dashboard", "link")
	if code != output.Blocked || out != "" || !strings.Contains(errOut, "no dashboard is running: start one with backsight dashboard\n") {
		t.Errorf("after stop: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestDashboardPortChecks(t *testing.T) {
	viewsFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	taken := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	code, out, errOut := run(t, "dashboard", "--port", taken)
	if code != output.Usage || out != "" || !strings.Contains(errOut, "port "+taken+" is taken: choose another --port or leave it out for a free one\n") {
		t.Errorf("taken: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	for _, p := range []string{"80", "70000", "-1"} {
		code, _, errOut := run(t, "dashboard", "--port", p)
		if code != output.Usage || !strings.Contains(errOut, "--port: 0 for a free port, or a whole number from 1024 to 65535\n") {
			t.Errorf("--port %s: exit %d, stderr %q", p, code, errOut)
		}
	}
}

func TestDashboardStopsCleanly(t *testing.T) {
	_, d, _, _, stop := startDashboard(t)
	if _, err := os.Stat(dashboard.RuntimeFile(d)); err != nil {
		t.Fatalf("no runtime file while running: %v", err)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(dashboard.RuntimeFile(d)); !os.IsNotExist(err) {
		t.Errorf("runtime file left: %v", err)
	}
	left, _ := os.ReadDir(dashboard.CodesDir(d))
	for _, f := range left {
		if len(f.Name()) == 64 {
			t.Errorf("code file left: %s", f.Name())
		}
	}
}

func TestDashboardTelemetry(t *testing.T) {
	base, d, first, _, stop := startDashboard(t)
	c := dashLogin(t, base, first)
	for range 2 {
		if got := getWith(t, base+"/", c); got != http.StatusOK {
			t.Fatalf("page: %d", got)
		}
	}
	if stop() != 0 {
		t.Fatal("not a clean stop")
	}
	files, _ := filepath.Glob(filepath.Join(d.TelemetryDir(), "events-*.jsonl"))
	var evs []map[string]any
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		for _, l := range strings.Split(string(raw), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) == nil && m["event"] == "dashboard" {
				evs = append(evs, m)
			}
		}
	}
	if len(evs) < 2 {
		t.Fatalf("dashboard events: %v", evs)
	}
	start, end := evs[len(evs)-2], evs[len(evs)-1]
	if start["action"] != "start" || start["pages"] != float64(0) || end["action"] != "stop" || end["pages"] != float64(2) {
		t.Errorf("start %v, stop %v", start, end)
	}
}

func TestOpenFlag(t *testing.T) {
	var got []string
	old := openBrowser
	openBrowser = func(u string) error { got = append(got, u); return nil }
	t.Cleanup(func() { openBrowser = old })
	_, _, first, _, stop := startDashboard(t, "--open")
	stop()
	want := strings.TrimPrefix(strings.TrimSpace(first), "Dashboard: ")
	if !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("openBrowser called with %v, want [%s]", got, want)
	}
}

func TestOpenFlagFailureIsOneLine(t *testing.T) {
	old := openBrowser
	openBrowser = func(string) error { return os.ErrNotExist }
	t.Cleanup(func() { openBrowser = old })
	_, _, first, stderr, stop := startDashboard(t, "--open")
	code := stop()
	code0 := strings.TrimPrefix(strings.TrimSpace(first), "Dashboard: ")
	if code != 0 || strings.Count(stderr.String(), "could not open a browser: open the link above\n") != 1 || strings.Contains(stderr.String(), code0) {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestBrowserCommand(t *testing.T) {
	const u = "http://127.0.0.1:1/?code=x"
	for goos, want := range map[string][]string{
		"linux":   {"xdg-open", u},
		"darwin":  {"open", u},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", u},
		"freebsd": {"xdg-open", u},
	} {
		if got := browserCommand(goos, u); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", goos, got, want)
		}
	}
}

func TestDashboardReadsTheServerReadOnly(t *testing.T) {
	viewsFixture(t)
	d, home, _ := machine()
	cfg, err := dashboardConfig(context.Background(), d, home, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Source.Server.(*hindsight.ReadOnly); !ok || !cfg.Source.Configured || !strings.HasPrefix(cfg.Source.ServerHost, "127.0.0.1:") {
		t.Errorf("server %T, configured %v, host %q", cfg.Source.Server, cfg.Source.Configured, cfg.Source.ServerHost)
	}
}

// countingIndex counts Close.
type countingIndex struct {
	dashboard.Index
	closes *int
}

func (c countingIndex) Close() error { *c.closes++; return c.Index.Close() }

func TestStopDuringFirstBuild(t *testing.T) {
	viewsFixture(t)
	d, home, _ := machine()
	big := bytes.Repeat([]byte(strings.Repeat("x", 1023)+"\n"), 2048)
	if err := os.WriteFile(filepath.Join(d.TelemetryDir(), "events-2026-09.jsonl"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := dashboardConfig(context.Background(), d, home, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	real := cfg.OpenIndex
	entered, release := make(chan struct{}), make(chan struct{})
	closes := 0
	cfg.OpenIndex = func(ctx context.Context) (dashboard.Index, error) {
		ix, err := real(ctx)
		if err != nil {
			return nil, err
		}
		close(entered)
		<-release
		return countingIndex{ix, &closes}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := dashboard.Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Serve(ctx); done <- err }()
	<-entered
	cancel()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return")
	}
	if _, err := os.Stat(dashboard.RuntimeFile(d)); !os.IsNotExist(err) {
		t.Errorf("runtime file left: %v", err)
	}
	left, _ := os.ReadDir(dashboard.CodesDir(d))
	if len(left) != 0 {
		t.Errorf("code files left: %v", left)
	}
	if closes != 1 {
		t.Errorf("Close counted %d times, want 1", closes)
	}
}

func TestNoDashboardConfigKey(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	cfgPath := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfgPath)
	if err := os.WriteFile(cfgPath, append(raw, "\n[dashboard]\nport = 8080\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := run(t, "stats", "overview")
	if code != output.Blocked || !strings.Contains(errOut, "unknown keys: dashboard") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

// A runtime file being written (no port yet) is a dashboard starting, judged
// on the real clock even when the commands' clock is frozen.
func TestDashboardLinkWhileStarting(t *testing.T) {
	viewsFixture(t)
	d, _, _ := machine()
	if err := os.MkdirAll(filepath.Dir(dashboard.RuntimeFile(d)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dashboard.RuntimeFile(d), []byte(`{"pid":`+strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "dashboard", "link")
	if code != output.Blocked || out != "" || !strings.Contains(errOut, "a dashboard is starting: run backsight dashboard link in a few seconds\n") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// readOnlyDir makes dir unwritable for the test, or skips where modes do
// not bind (root, Windows).
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory modes do not bind here")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// A failure to clean up at the stop is a sentence on stderr and exit 20,
// never the error's text (a path), and never a second JSON document.
func TestDashboardCleanupFailureIsASentence(t *testing.T) {
	_, d, _, stderr, out, stop := startDashboardOut(t, "--json")
	readOnlyDir(t, filepath.Dir(dashboard.RuntimeFile(d)))
	if code := stop(); code != output.Blocked {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "the dashboard stopped but could not clean up its files in the state directory (permission denied)") || strings.Contains(stderr.String(), d.State) {
		t.Errorf("stderr %q", stderr.String())
	}
	dec := json.NewDecoder(strings.NewReader(out.String()))
	var env linkEnv
	if err := dec.Decode(&env); err != nil || !env.OK || dec.More() {
		t.Errorf("stdout is not one success envelope: %q (%v)", out.String(), err)
	}
}

// A start that cannot write its files is not an index problem and names no
// path.
func TestDashboardStartFailureIsNotAnIndexProblem(t *testing.T) {
	viewsFixture(t)
	d, _, _ := machine()
	dir := filepath.Dir(dashboard.RuntimeFile(d))
	if err := os.MkdirAll(dashboard.CodesDir(d), 0o700); err != nil {
		t.Fatal(err)
	}
	readOnlyDir(t, dir)
	code, out, errOut := run(t, "dashboard")
	if code != output.Blocked || out != "" || !strings.Contains(errOut, "could not write the dashboard's runtime file") ||
		strings.Contains(errOut, "metrics index") || strings.Contains(errOut, d.State) {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// Each kind of start failure has its own sentence, and none shows a path.
// The kinds come from the dashboard's real failure paths where a test can
// provoke them; a listen failure other than a taken port cannot be.
func TestDashboardStartErrorClasses(t *testing.T) {
	viewsFixture(t)
	d, home, _ := machine()
	cfg, err := dashboardConfig(context.Background(), d, home, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NeedsBuild = func(context.Context) (bool, error) {
		return false, &fs.PathError{Op: "open", Path: "/secret/index", Err: fs.ErrPermission}
	}
	_, indexErr := dashboard.Start(context.Background(), cfg)
	denied := &fs.PathError{Op: "open", Path: "/secret/state/x", Err: fs.ErrPermission}
	for _, c := range []struct {
		name string
		err  error
		code int
		want string
	}{
		{"index", indexErr, output.Blocked, "metrics index"},
		{"listen", &dashboard.StartError{Kind: dashboard.StartListen, Err: denied}, output.Blocked, "could not listen on 127.0.0.1:4242 (permission denied)"},
		{"login code", &dashboard.StartError{Kind: dashboard.StartLoginCode, Err: denied}, output.Blocked, "could not create a login code (permission denied)"},
		{"runtime file", &dashboard.StartError{Kind: dashboard.StartRuntimeFile, Err: denied}, output.Blocked, "could not write the dashboard's runtime file"},
		{"port taken", fmt.Errorf("port 4242: %w", dashboard.ErrPortTaken), output.Usage, "port 4242 is taken: choose another --port or leave it out for a free one"},
	} {
		err := dashboardStartError(4242, c.err)
		if output.CodeOf(err) != c.code || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "/secret") {
			t.Errorf("%s: exit %d, %q; want %d, %q", c.name, output.CodeOf(err), err, c.code, c.want)
		}
	}
	var se *dashboard.StartError
	if !errors.As(indexErr, &se) || se.Kind != dashboard.StartIndex {
		t.Errorf("Start's index failure: %v", indexErr)
	}
}

// A login code that cannot be written is its own failure, not an index
// problem, and names no path.
func TestDashboardLoginCodeFailure(t *testing.T) {
	viewsFixture(t)
	d, _, _ := machine()
	if err := os.MkdirAll(dashboard.CodesDir(d), 0o700); err != nil {
		t.Fatal(err)
	}
	readOnlyDir(t, dashboard.CodesDir(d))
	code, out, errOut := run(t, "dashboard")
	if code != output.Blocked || out != "" || !strings.Contains(errOut, "could not create a login code (permission denied)") || strings.Contains(errOut, d.State) {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// A stop that interrupts Start (before Serve) exits 0 with a plain line,
// not an index message.
func TestDashboardStoppedBeforeServing(t *testing.T) {
	viewsFixture(t)
	old := startServer
	startServer = func(ctx context.Context, _ dashboard.Config) (*dashboard.Server, error) {
		return nil, fmt.Errorf("open the metrics index: %w", &dashboard.StartError{Kind: dashboard.StartIndex, Err: ctx.Err()})
	}
	t.Cleanup(func() { startServer = old })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errb bytes.Buffer
	code := ExecuteContext(ctx, []string{"dashboard"}, &out, &errb)
	if code != 0 || out.Len() != 0 || strings.Contains(errb.String(), "index") || !strings.Contains(errb.String(), "stopped before it started") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

// A context already done still ends cleanly, with nothing left behind.
func TestDashboardCancelledContext(t *testing.T) {
	viewsFixture(t)
	d, _, _ := machine()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errb bytes.Buffer
	if code := ExecuteContext(ctx, []string{"dashboard"}, &out, &errb); code != 0 {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	if _, err := os.Stat(dashboard.RuntimeFile(d)); !os.IsNotExist(err) {
		t.Errorf("runtime file left: %v", err)
	}
}

// A runtime file whose process is gone is no running dashboard.
func TestDashboardLinkWithStaleRuntimeFile(t *testing.T) {
	viewsFixture(t)
	d, _, _ := machine()
	if err := os.MkdirAll(filepath.Dir(dashboard.RuntimeFile(d)), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"pid":2147483646,"port":43210,"started_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(dashboard.RuntimeFile(d), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "dashboard", "link")
	if code != output.Blocked || out != "" || !strings.Contains(errOut, "no dashboard is running: start one with backsight dashboard\n") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}
