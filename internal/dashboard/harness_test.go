package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/paths"
)

// clock is a goroutine-safe test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness is a started dashboard serving in a goroutine, under a temp
// BACKSIGHT_HOME, with a client that never follows redirects.
type harness struct {
	t      *testing.T
	s      *Server
	dirs   paths.Dirs
	home   string
	port   int
	clock  *clock
	client *http.Client

	// The data layer's fakes, set by withIndex, withServer and withDoctor.
	ix         *fakeIndex
	srv        *fakeServer
	doctorRuns atomic.Int64

	cancel context.CancelFunc
	done   chan struct{} // closed when Serve returns
	reason StopReason
	err    error
}

// harnessOpt adjusts a harness's Config before Start; h has its t, dirs,
// home and clock.
type harnessOpt func(h *harness, c *Config)

// newHarness starts a dashboard on a free port, without a data layer; opts
// adjust the Config (whose Dirs are set) before Start. The cleanup cancels
// Serve and waits for it, so a package variable a test set is restored
// only after.
func newHarness(t *testing.T, opts ...func(*Config)) *harness {
	t.Helper()
	hopts := make([]harnessOpt, 0, len(opts))
	for _, o := range opts {
		hopts = append(hopts, func(_ *harness, c *Config) { o(c) })
	}
	return newDataHarness(t, hopts...)
}

// newDataHarness is newHarness with options that may install the data
// layer's fakes (withIndex, withServer, withDoctor, withNeedsBuild).
func newDataHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	servedBound(t)
	home := t.TempDir()
	t.Setenv("BACKSIGHT_HOME", home)
	t.Setenv("BACKSIGHT_STATE_DIR", "")
	dirs, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: indexgen.Now}
	h := &harness{
		t: t, dirs: dirs, home: home, clock: c, done: make(chan struct{}),
		client: &http.Client{
			Transport:     &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	cfg := Config{Dirs: dirs, Now: c.now, Version: "test"}
	for _, o := range opts {
		o(h, &cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	h.s, h.port, h.cancel = s, s.Port(), cancel
	go func() {
		h.reason, h.err = s.Serve(ctx)
		close(h.done)
	}()
	t.Cleanup(func() {
		_, _ = h.stop()
		h.client.CloseIdleConnections()
	})
	return h
}

// prodRenderTimeout is renderTimeout as the dashboard ships it, read
// before any test changes it.
var prodRenderTimeout = renderTimeout

// servedRenderTimeout bounds the requests of a test that means "it is
// served", not "it is fast", as dashboard.SetRenderTimeout's other users
// do (internal/cli's parity test). An hour is out of reach: go test's
// -timeout (20 min) ends a hang first, so a busy runner never sees 503.
const servedRenderTimeout = time.Hour

// servedBound raises the render bound to servedRenderTimeout for the
// test, unless it set its own first (the tests of the "too slow" answer,
// at 20 ms): under -race on a busy CI runner a view can take longer than
// the 10 s a user waits, and the page answers 503 (issue 248,
// TestPagesShowNoJargon).
func servedBound(t *testing.T) {
	t.Helper()
	if renderTimeout == prodRenderTimeout {
		t.Cleanup(SetRenderTimeout(servedRenderTimeout))
	}
}

// stop cancels Serve and waits for it to return.
func (h *harness) stop() (StopReason, error) {
	h.cancel()
	<-h.done
	return h.reason, h.err
}

// host is the Host the dashboard accepts on 127.0.0.1.
func (h *harness) host() string { return "127.0.0.1:" + strconv.Itoa(h.port) }

// do sends method path with hdr as name, value pairs (a "Host" pair sets
// the request's Host). The body is read in full and kept in resp.Body.
func (h *harness) do(method, path string, hdr ...string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, "http://"+h.host()+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if len(hdr)%2 != 0 {
		h.t.Fatalf("headers %q: want name, value pairs", hdr)
	}
	for i := 0; i < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Add(hdr[i], hdr[i+1])
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		h.t.Fatalf("%s %s: read the body: %v", method, path, err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp
}

// body is resp's body, as do kept it.
func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return string(b)
}

// raw writes request as bytes on a new connection and returns the one
// response read back, status line, headers and body.
func (h *harness) raw(request string) string {
	h.t.Helper()
	conn, err := net.Dial("tcp", h.host())
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, request); err != nil {
		h.t.Fatalf("write the request: %v", err)
	}
	// The request tells ReadResponse whether a body follows (none for HEAD).
	req, _ := http.ReadRequest(bufio.NewReader(strings.NewReader(request)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		h.t.Fatalf("read the response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := httputil.DumpResponse(resp, true)
	if err != nil {
		h.t.Fatalf("read the response: %v", err)
	}
	return string(out)
}

// rawStatus is the status code of a response raw returned.
func rawStatus(t *testing.T, resp string) int {
	t.Helper()
	f := strings.Fields(resp)
	if len(f) < 2 {
		t.Fatalf("no status line in %q", resp)
	}
	n, err := strconv.Atoi(f[1])
	if err != nil {
		t.Fatalf("status line of %q: %v", resp, err)
	}
	return n
}

// login mints a code and opens it: the session cookie it set.
func (h *harness) login() *http.Cookie {
	h.t.Helper()
	code, err := MintCode(h.dirs, h.clock.now())
	if err != nil {
		h.t.Fatal(err)
	}
	r := h.do("GET", "/?code="+code)
	if r.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("log in: %d, want 303", r.StatusCode)
	}
	for _, c := range r.Cookies() {
		if c.Name == cookieName(h.port) {
			return c
		}
	}
	h.t.Fatalf("log in: no session cookie in %q", r.Header.Values("Set-Cookie"))
	return nil
}

// cookieHdr is the request header that sends c, as a pair for do.
func cookieHdr(c *http.Cookie) []string { return []string{"Cookie", c.Name + "=" + c.Value} }

// A harness bounds its requests at servedRenderTimeout while its test
// runs, unless the test set its own bound first, and the shipped bound
// is back once the test ends.
func TestHarnessBoundsRequestsAsServedNotFast(t *testing.T) {
	if prodRenderTimeout != 10*time.Second || servedRenderTimeout <= prodRenderTimeout {
		t.Fatalf("shipped bound %v, served bound %v: want 10 s and more", prodRenderTimeout, servedRenderTimeout)
	}
	t.Run("the shipped bound", func(t *testing.T) {
		dataHarness(t)
		if renderTimeout != servedRenderTimeout {
			t.Errorf("render bound %v during the test, want %v", renderTimeout, servedRenderTimeout)
		}
	})
	if renderTimeout != prodRenderTimeout {
		t.Errorf("render bound %v after the test, want %v back", renderTimeout, prodRenderTimeout)
	}
	t.Run("a bound of the test's own", func(t *testing.T) {
		t.Cleanup(SetRenderTimeout(20 * time.Millisecond))
		dataHarness(t)
		if renderTimeout != 20*time.Millisecond {
			t.Errorf("render bound %v, want the test's 20ms", renderTimeout)
		}
	})
	if renderTimeout != prodRenderTimeout {
		t.Errorf("render bound %v after the test, want %v back", renderTimeout, prodRenderTimeout)
	}
}
