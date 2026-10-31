package dashboard

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/paths"
)

// freePort is a port nothing listens on: the system's pick, released.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func noRuntimeFile(t *testing.T, cfg Config) {
	t.Helper()
	if _, err := os.Stat(RuntimeFile(cfg.Dirs)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dashboard.json: want none, stat says %v", err)
	}
}

func TestBindsLoopbackOnly(t *testing.T) {
	h := newHarness(t)
	if a := h.s.ln.Addr().String(); !strings.HasPrefix(a, "127.0.0.1:") {
		t.Errorf("listening on %s, want 127.0.0.1 only", a)
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(h.port))
	if err != nil {
		t.Fatalf("dial 127.0.0.1: %v", err)
	}
	_ = c.Close()
	for _, f := range []string{"Addr", "Host", "Bind", "Listen"} {
		if _, ok := reflect.TypeFor[Config]().FieldByName(f); ok {
			t.Errorf("Config has a field %s: nothing may choose the bind address", f)
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var other net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			other = n.IP.To4()
			break
		}
	}
	if other == nil {
		t.Skip("no non-loopback IPv4 address on this machine")
	}
	// The timeout only bounds a dial that a firewall would drop: a
	// refused dial returns at once, and a timeout is a refusal too.
	d := net.Dialer{Timeout: 5 * time.Second}
	if c, err := d.Dial("tcp", net.JoinHostPort(other.String(), strconv.Itoa(h.port))); err == nil {
		_ = c.Close()
		t.Errorf("a dial to %s:%d succeeded: the dashboard listens beyond loopback", other, h.port)
	}
}

func TestFixedPortTaken(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	cfg := Config{Dirs: testDirs(t), Port: held.Addr().(*net.TCPAddr).Port}
	s, err := Start(context.Background(), cfg)
	if err == nil {
		_ = s.ln.Close()
		t.Fatal("Start on a taken port succeeded")
	}
	if !errors.Is(err, ErrPortTaken) {
		t.Errorf("Start on a taken port: %v, want ErrPortTaken", err)
	}
	noRuntimeFile(t, cfg)
}

func TestPortRange(t *testing.T) {
	for _, p := range []int{-1, 1, 80, 1023, 65536} {
		cfg := Config{Dirs: testDirs(t), Port: p}
		s, err := Start(context.Background(), cfg)
		if err == nil {
			_ = s.ln.Close()
			t.Errorf("Start with port %d succeeded, want refused", p)
			continue
		}
		if !strings.Contains(err.Error(), "1024 to 65535") {
			t.Errorf("port %d: %v, want the allowed range named", p, err)
		}
		noRuntimeFile(t, cfg)
	}
}

func TestStartRefusedWhileOneRuns(t *testing.T) {
	t.Run("free port", func(t *testing.T) {
		d := testDirs(t)
		p := freePort(t)
		raw := plantRuntime(t, d, Runtime{PID: os.Getpid(), Port: p, StartedAt: time.Now()}, time.Now())
		_, err := Start(context.Background(), Config{Dirs: d, Port: p})
		wantRunning(t, err, p)
		if got := fileText(t, RuntimeFile(d)); got != raw {
			t.Errorf("runtime file changed: %q, was %q", got, raw)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			t.Fatalf("port %d left bound: %v", p, err)
		}
		_ = ln.Close()
	})
	// Read before binding: with the port held, a Start that bound first
	// would answer ErrPortTaken.
	t.Run("held port", func(t *testing.T) {
		d := testDirs(t)
		held, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Close() }()
		p := held.Addr().(*net.TCPAddr).Port
		plantRuntime(t, d, Runtime{PID: os.Getpid(), Port: p, StartedAt: time.Now()}, time.Now())
		_, err = Start(context.Background(), Config{Dirs: d, Port: p})
		wantRunning(t, err, p)
	})
}

func TestFreePort(t *testing.T) {
	h := newHarness(t)
	if h.s.Port() <= 0 {
		t.Fatalf("Port() = %d", h.s.Port())
	}
	r := readRuntime(t, h.dirs)
	if r.Port != h.s.Port() {
		t.Errorf("dashboard.json port %d, Port() %d", r.Port, h.s.Port())
	}
	if r.PID != os.Getpid() {
		t.Errorf("dashboard.json pid %d, want %d", r.PID, os.Getpid())
	}
	if !readRunning(t, h) {
		t.Error("the runtime file Start wrote does not read as a running dashboard")
	}
}

func readRunning(t *testing.T, h *harness) bool {
	t.Helper()
	_, alive, err := Running(h.dirs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return alive
}

// Undoing a failed Start: the claim refused after the bind leaves the
// port free and no runtime file.
func TestFailedStartUndoes(t *testing.T) {
	defer func(w time.Duration) { claimLockWait = w }(claimLockWait)
	claimLockWait = 0
	d := testDirs(t)
	if err := os.MkdirAll(dashboardDir(d), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, busy, err := tryLock(filepath.Join(dashboardDir(d), "claim.lock"))
	if err != nil || busy {
		t.Fatalf("hold the claim lock: busy %v, %v", busy, err)
	}
	defer unlock()
	p := freePort(t)
	cfg := Config{Dirs: d, Port: p}
	_, err = Start(context.Background(), cfg)
	wantRunning(t, err, 0)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		t.Fatalf("a failed Start left port %d bound: %v", p, err)
	}
	_ = ln.Close()
	noRuntimeFile(t, cfg)
}

func TestStartTightensModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes on Windows")
	}
	var cfg Config
	newHarness(t, func(c *Config) {
		cfg = *c
		for _, p := range []string{dashboardDir(c.Dirs), CodesDir(c.Dirs)} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		lock := filepath.Join(dashboardDir(c.Dirs), "claim.lock")
		if err := os.WriteFile(lock, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lock, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	for p, want := range map[string]fs.FileMode{
		dashboardDir(cfg.Dirs): 0o700,
		CodesDir(cfg.Dirs):     0o700,
		filepath.Join(dashboardDir(cfg.Dirs), "claim.lock"): 0o600,
	} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", p, got, want)
		}
	}
}

func TestCreatesPrivateDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes on Windows")
	}
	h := newHarness(t)
	for _, p := range []string{dashboardDir(h.dirs), CodesDir(h.dirs)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Errorf("%s: mode %o, want 700", p, got)
		}
	}
}

func TestHostCheck(t *testing.T) {
	h := dataHarness(t) // pages need the data layer
	ck := cookieHdr(h.login())
	p := strconv.Itoa(h.port)
	for _, host := range []string{"localhost:" + p, "127.0.0.1:" + p} {
		if r := h.do("GET", "/", append([]string{"Host", host}, ck...)...); r.StatusCode != http.StatusOK {
			t.Errorf("Host %s: %d, want 200", host, r.StatusCode)
		}
	}
	for _, host := range []string{"127.0.0.1:" + strconv.Itoa(h.port+1), "evil.example:" + p, "127.0.0.1", "LOCALHOST:" + p, "[::1]:" + p, "127.0.0.1:" + p + "."} {
		r := h.do("GET", "/", "Host", host)
		if r.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("Host %s: %d, want 421", host, r.StatusCode)
		}
		if b := body(t, r); b != "misdirected request\n" {
			t.Errorf("Host %s: body %q", host, b)
		}
	}
	if got := rawStatus(t, h.raw("GET / HTTP/1.0\r\n\r\n")); got != http.StatusMisdirectedRequest {
		t.Errorf("no Host: %d, want 421", got)
	}
	// An absolute-form target names the host (RFC 7230 5.4): checked as
	// the Host. A browser never sends one to an origin server.
	for target, want := range map[string]int{
		"http://evil.example:" + p + "/": http.StatusMisdirectedRequest,
		"http://" + h.host() + "/":       http.StatusOK,
	} {
		other := "evil.example:" + p
		if strings.Contains(target, "evil") {
			other = h.host()
		}
		req := "GET " + target + " HTTP/1.1\r\nHost: " + other + "\r\nCookie: " + ck[1] + "\r\nConnection: close\r\n\r\n"
		if got := rawStatus(t, h.raw(req)); got != want {
			t.Errorf("GET %s with Host %s: %d, want %d", target, other, got, want)
		}
	}
}

func TestMethods(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/", "/api/v1/overview", assetPath("logo-light.svg")} {
		for _, m := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
			r := h.do(m, path)
			if r.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d, want 405", m, path, r.StatusCode)
			}
			if a := r.Header.Values("Allow"); len(a) != 1 || a[0] != "GET, HEAD" {
				t.Errorf("%s %s: Allow %q", m, path, a)
			}
			if b := body(t, r); b != "only GET and HEAD\n" {
				t.Errorf("%s %s: body %q", m, path, b)
			}
		}
	}
	for host, want := range map[string]int{h.host(): http.StatusMethodNotAllowed, "evil.example:" + strconv.Itoa(h.port): http.StatusMisdirectedRequest} {
		req := "CONNECT " + host + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n"
		if got := rawStatus(t, h.raw(req)); got != want {
			t.Errorf("CONNECT %s: %d, want %d", host, got, want)
		}
	}
	// net/http answers "OPTIONS *" itself unless told not to.
	resp := h.raw("OPTIONS * HTTP/1.1\r\nHost: " + h.host() + "\r\nConnection: close\r\n\r\n")
	if got := rawStatus(t, resp); got != http.StatusMethodNotAllowed {
		t.Errorf("OPTIONS *: %d, want 405", got)
	}
	if !strings.Contains(resp, "Content-Security-Policy: "+csp) {
		t.Errorf("OPTIONS *: no security headers in %q", resp)
	}
}

func TestSecFetchSite(t *testing.T) {
	h := dataHarness(t) // pages need the data layer
	ck := cookieHdr(h.login())
	if r := h.do("GET", "/", ck...); r.StatusCode != http.StatusOK {
		t.Errorf("no Sec-Fetch-Site: %d, want 200", r.StatusCode)
	}
	for _, v := range []string{"same-origin", "none"} {
		if r := h.do("GET", "/", append([]string{"Sec-Fetch-Site", v}, ck...)...); r.StatusCode != http.StatusOK {
			t.Errorf("Sec-Fetch-Site %s: %d, want 200", v, r.StatusCode)
		}
	}
	refused := [][]string{
		{"Sec-Fetch-Site", "same-site"}, {"Sec-Fetch-Site", "cross-site"}, {"Sec-Fetch-Site", "garbage"},
		{"Sec-Fetch-Site", ""}, {"Sec-Fetch-Site", "same-origin", "Sec-Fetch-Site", "cross-site"},
	}
	for _, hdr := range refused {
		for _, path := range []string{"/", assetPath("logo-light.svg")} {
			r := h.do("GET", path, append(hdr, ck...)...)
			if r.StatusCode != http.StatusForbidden {
				t.Errorf("%q on %s: %d, want 403", hdr, path, r.StatusCode)
			}
			if b := body(t, r); b != "this page opens only from its own links or the address bar\n" {
				t.Errorf("%q on %s: body %q", hdr, path, b)
			}
		}
	}
}

// response is one request of TestHeadersOnEveryResponse and the status it
// must get.
type response struct {
	name, method, path string
	hdr                []string
	status             int // 0: a redirect, whatever its code
	pre                func()
}

// everyResponse is the set TestHeadersOnEveryResponse checks: each route's
// sample by GET and HEAD under a session, each refusal the guard writes,
// a login and the 401 answers.
func everyResponse(h *harness) []response {
	ck := cookieHdr(h.login())
	var set []response
	for _, r := range routes {
		want := http.StatusOK
		switch {
		case r.Kind == "api" && strings.Contains(r.Sample, "/sessions/"),
			r.Kind == "page" && (r.Page == "project" || r.Page == "session" || r.Page == "memory"),
			r.Kind == "text":
			want = http.StatusNotFound // the sample names no project, session or memory of the generated index
		case r.Kind == "prefs":
			want = http.StatusBadRequest // the sample sets nothing and names no page to go back to
		}
		for _, m := range []string{"GET", "HEAD"} {
			code := want
			if r.Kind == "text" && m == "HEAD" {
				code = http.StatusMethodNotAllowed // the text page is GET only
			}
			set = append(set, response{r.Pattern, m, r.Sample, ck, code, nil})
		}
	}
	return append(set,
		response{"wrong host", "GET", "/", append([]string{"Host", "evil.example:" + strconv.Itoa(h.port)}, ck...), http.StatusMisdirectedRequest, nil},
		response{"post", "POST", "/", ck, http.StatusMethodNotAllowed, nil},
		response{"cross-site", "GET", "/", append([]string{"Sec-Fetch-Site", "cross-site"}, ck...), http.StatusForbidden, nil},
		response{"unknown asset", "GET", "/assets/nope.css", ck, http.StatusNotFound, nil},
		response{"unhashed asset", "GET", "/assets/logo-light.svg", ck, http.StatusNotFound, nil},
		response{"unknown route", "GET", "/nope", ck, http.StatusNotFound, nil},
		response{"cleaned path", "GET", "/cost/../quality", ck, 0, nil}, // net/http's own redirect
		response{"login", "GET", "/quality?code=" + h.mint(), nil, http.StatusSeeOther, nil},
		response{"401 page", "GET", "/quality", nil, http.StatusUnauthorized, nil},
		response{"401 page, HEAD", "HEAD", "/quality", nil, http.StatusUnauthorized, nil},
		response{"401 envelope", "GET", "/api/v1/overview", nil, http.StatusUnauthorized, nil},
		response{"page 400", "GET", "/quality?limit=0", ck, http.StatusBadRequest, nil},
		response{"page 404", "GET", "/sessions/zzzzzzzz", ck, http.StatusNotFound, nil},
		response{"api 400", "GET", "/api/v1/overview?limit=0", ck, http.StatusBadRequest, nil},
		response{"api 404", "GET", "/api/v1/nope", ck, http.StatusNotFound, nil},
		response{"api root", "GET", "/api/v1/", ck, http.StatusNotFound, nil},
		// Last: the index is marked as changed by a newer backsight, for good.
		response{"api 503", "GET", "/api/v1/overview?days=3", ck, http.StatusServiceUnavailable, func() { h.ix.set(func(f *fakeIndex) { f.side = true }) }},
		response{"page 503", "GET", "/quality?days=4", ck, http.StatusServiceUnavailable, nil},
	)
}

func TestHeadersOnEveryResponse(t *testing.T) {
	h := dataHarness(t)
	five := map[string]string{
		"Content-Security-Policy":      "default-src 'none'; style-src 'self'; img-src 'self' data:; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for _, c := range everyResponse(h) {
		if c.pre != nil {
			c.pre()
		}
		r := h.do(c.method, c.path, append([]string{"Origin", "http://evil.example"}, c.hdr...)...)
		at := c.method + " " + c.path + " (" + c.name + ")"
		if c.status == 0 && (r.StatusCode < 300 || r.StatusCode > 399) || c.status != 0 && r.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", at, r.StatusCode, c.status)
		}
		for k, v := range five {
			if got := r.Header.Values(k); len(got) != 1 || got[0] != v {
				t.Errorf("%s: %s %q, want %q", at, k, got, v)
			}
		}
		cache := "no-store"
		if strings.HasPrefix(c.path, "/assets/") && r.StatusCode == http.StatusOK {
			cache = "public, max-age=31536000, immutable"
		}
		if got := r.Header.Values("Cache-Control"); len(got) != 1 || got[0] != cache {
			t.Errorf("%s: Cache-Control %q, want %q", at, got, cache)
		}
		for k := range r.Header {
			if strings.HasPrefix(k, "Access-Control-") {
				t.Errorf("%s: CORS header %s", at, k)
			}
		}
	}
}

func TestServerLimits(t *testing.T) {
	h := newHarness(t)
	srv := h.s.srv
	for _, c := range []struct {
		name      string
		got, want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 5 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 15 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 60 * time.Second},
	} {
		if c.got != c.want {
			t.Errorf("%s %v, want %v", c.name, c.got, c.want)
		}
	}
	if srv.MaxHeaderBytes != 16384 {
		t.Errorf("MaxHeaderBytes %d, want 16384", srv.MaxHeaderBytes)
	}
	if srv.ErrorLog == nil || srv.ErrorLog.Writer() != io.Discard {
		t.Errorf("ErrorLog %v: want a logger that discards", srv.ErrorLog)
	}
	big := "GET / HTTP/1.1\r\nHost: " + h.host() + "\r\nX-Big: " + strings.Repeat("a", 32<<10) + "\r\n\r\n"
	if got := rawStatus(t, h.raw(big)); got != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("a 32 KiB header: %d, want 431", got)
	}
}

func TestStartSweepsOldCodes(t *testing.T) {
	var cfg Config
	h := newHarness(t, func(c *Config) {
		cfg = *c
		dir := CodesDir(c.Dirs)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{strings.Repeat("a", 64), strings.Repeat("0", 64), "notes.txt"} {
			if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	// Left: notes.txt and the first link's code, minted after the sweep.
	first := codeName(strings.TrimPrefix(h.s.FirstLink().URL, "http://"+h.host()+"/?code="))
	if got := codeDirNames(t, cfg); !slices.Equal(got, []string{first, "notes.txt"}) {
		t.Errorf("codes/ after Start: %q, want %s and notes.txt", got, first)
	}
}

func codeDirNames(t *testing.T, cfg Config) []string {
	t.Helper()
	entries, err := os.ReadDir(CodesDir(cfg.Dirs))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestServeStopsOnCancel(t *testing.T) {
	h := newHarness(t)
	if _, err := MintCode(h.dirs, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	reason, err := h.stop()
	if reason != "interrupted" || err != nil {
		t.Errorf("Serve: %q, %v; want interrupted, nil", reason, err)
	}
	noRuntimeFile(t, Config{Dirs: h.dirs})
	for _, n := range codeDirNames(t, Config{Dirs: h.dirs}) {
		if lowerHex(n, 64) {
			t.Errorf("code file %s left after Serve", n)
		}
	}
	if _, err := net.Dial("tcp", h.host()); err == nil {
		t.Error("the port still accepts connections after Serve returned")
	}
}

// The server reads the clock it was given: the harness's.
func TestHarnessClock(t *testing.T) {
	h := newHarness(t)
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	h.clock.set(at)
	h.clock.add(90 * time.Second)
	if got, want := h.s.now(), at.Add(90*time.Second); !got.Equal(want) {
		t.Errorf("server clock %v, want %v", got, want)
	}
}

// net/http's path-cleaning redirect stays on this origin: a target
// starting "//" is a path, never a host.
func TestCleanedPathRedirectStaysLocal(t *testing.T) {
	h := newHarness(t)
	ck := cookieHdr(h.login())
	for _, target := range []string{"//evil.example/x", "/.//evil.example/x", "/a/..//evil.example/x"} {
		resp := h.raw("GET " + target + " HTTP/1.1\r\nHost: " + h.host() + "\r\nCookie: " + ck[1] + "\r\nConnection: close\r\n\r\n")
		if got := rawStatus(t, resp); got < 300 || got > 399 {
			t.Errorf("GET %s: %d, want a redirect", target, got)
			continue
		}
		loc := ""
		for _, l := range strings.Split(resp, "\r\n") {
			if v, ok := strings.CutPrefix(l, "Location: "); ok {
				loc = v
			}
		}
		if !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, "/\\") {
			t.Errorf("GET %s: Location %q, want a local path", target, loc)
		}
	}
}

// A Start failing after its claim (the old codes cannot be swept, the
// first code cannot be minted) releases the claim and the port, and
// returns an error.
func TestFailedStartAfterClaimUndoes(t *testing.T) {
	// waitOpen is an index open waiting for its context's end; openEnded
	// receives how it ended.
	openEnded := make(chan error, 1)
	waitOpen := func(ctx context.Context) error {
		<-ctx.Done()
		openEnded <- ctx.Err()
		return ctx.Err()
	}
	failRand := func(t *testing.T) {
		t.Cleanup(func() { randRead = rand.Read })
		randRead = func([]byte) (int, error) { return 0, errors.New("no randomness") }
	}
	for _, c := range []struct {
		name  string
		spoil func(t *testing.T, d paths.Dirs)
		cfg   func(*Config)
		check func(t *testing.T)
	}{
		{"sweep", func(t *testing.T, d paths.Dirs) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("needs a directory its owner cannot read")
			}
			codes := CodesDir(d)
			if err := os.MkdirAll(codes, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(codes, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(codes, 0o700) })
		}, nil, nil},
		{"mint", func(t *testing.T, _ paths.Dirs) { failRand(t) }, nil, nil},
		{"needs build", func(*testing.T, paths.Dirs) {}, func(c *Config) {
			c.OpenIndex = func(ctx context.Context) (Index, error) { return nil, waitOpen(ctx) }
			c.NeedsBuild = func(context.Context) (bool, error) { return false, errors.New("cannot inspect") }
		}, nil},
		{"open", func(*testing.T, paths.Dirs) {}, func(c *Config) {
			c.OpenIndex = func(context.Context) (Index, error) { return nil, index.ErrUnreadable }
		}, nil},
		{"mint during the first build", func(t *testing.T, _ paths.Dirs) { failRand(t) }, func(c *Config) {
			c.OpenIndex = func(ctx context.Context) (Index, error) { return nil, waitOpen(ctx) }
			c.NeedsBuild = func(context.Context) (bool, error) { return true, nil }
		}, func(t *testing.T) {
			select {
			case err := <-openEnded:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("the build's open ended with %v, want context.Canceled", err)
				}
			default:
				t.Error("Start returned with the first build still running")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := testDirs(t)
			c.spoil(t, d)
			p := freePort(t)
			cfg := Config{Dirs: d, Port: p}
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			s, err := Start(context.Background(), cfg)
			if err == nil {
				_ = s.ln.Close()
				t.Fatal("Start succeeded")
			}
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
			if err != nil {
				t.Fatalf("a failed Start left port %d bound: %v", p, err)
			}
			_ = ln.Close()
			noRuntimeFile(t, cfg)
			if c.check != nil {
				c.check(t)
			}
		})
	}
}

// blockRoute makes the first request that passes the guard wait in
// beforeRoute until release is closed or its context ends; entered is
// closed once it waits, cancelled receives whether its context had ended.
func blockRoute(t *testing.T) (entered, release chan struct{}, cancelled chan bool) {
	t.Helper()
	entered, release, cancelled = make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	var once sync.Once
	beforeRoute = func(r *http.Request) {
		first := false
		once.Do(func() { first = true })
		if !first {
			return
		}
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		cancelled <- r.Context().Err() != nil
	}
	t.Cleanup(func() { beforeRoute = func(*http.Request) {} })
	return entered, release, cancelled
}

// get sends GET / with session cookie c in the background: its status,
// or -1 when the request got no response.
func (h *harness) get(c *http.Cookie) chan int {
	got := make(chan int, 1)
	go func() {
		req, err := http.NewRequest("GET", "http://"+h.host()+"/", nil)
		if err != nil {
			got <- -1
			return
		}
		req.AddCookie(c)
		resp, err := h.client.Do(req)
		if err != nil {
			got <- -1
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		got <- resp.StatusCode
	}()
	return got
}

// A request running when Serve's context ends keeps its own context and
// finishes within the grace.
func TestStopLetsARunningRequestFinish(t *testing.T) {
	entered, release, cancelled := blockRoute(t)
	t.Cleanup(func() { beforeShutdown = func() {} })
	h := dataHarness(t) // pages need the data layer
	beforeShutdown = func() { close(release) }
	got := h.get(h.login())
	<-entered
	reason, err := h.stop()
	if reason != "interrupted" || err != nil {
		t.Errorf("Serve: %q, %v; want interrupted, nil", reason, err)
	}
	if <-cancelled {
		t.Error("the running request's context ended with Serve's: it got no grace")
	}
	if status := <-got; status != http.StatusOK {
		t.Errorf("the running request got %d, want 200", status)
	}
}

// A request still running after the grace is cut: its connection is
// closed, its context ends, and Serve returns.
func TestStopCutsARequestAfterTheGrace(t *testing.T) {
	defer func(g time.Duration) { shutdownGrace = g }(shutdownGrace)
	shutdownGrace = 0
	entered, _, cancelled := blockRoute(t)
	h := newHarness(t)
	got := h.get(h.login())
	<-entered
	reason, err := h.stop()
	if reason != "interrupted" || err != nil {
		t.Errorf("Serve: %q, %v; want interrupted, nil", reason, err)
	}
	if !<-cancelled {
		t.Error("the request left running was not cancelled")
	}
	if status := <-got; status != -1 {
		t.Errorf("the request cut at the stop got a response %d", status)
	}
	noRuntimeFile(t, Config{Dirs: h.dirs})
}
