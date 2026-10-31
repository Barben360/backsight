package dashboard

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
)

// mint makes a login code on the harness's clock.
func (h *harness) mint() string {
	h.t.Helper()
	code, err := MintCode(h.dirs, h.clock.now())
	if err != nil {
		h.t.Fatal(err)
	}
	return code
}

// codeFile is where code waits until it is spent.
func (h *harness) codeFile(code string) string {
	return filepath.Join(CodesDir(h.dirs), codeName(code))
}

// codeLeft: code's file is still there, unspent.
func (h *harness) codeLeft(code string) bool {
	h.t.Helper()
	_, err := os.Stat(h.codeFile(code))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.t.Fatal(err)
	}
	return err == nil
}

// sessionCookie is the session cookie resp sets, nil when it sets none.
func (h *harness) sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == cookieName(h.port) {
			return c
		}
	}
	return nil
}

// isUnauthorizedPage: resp is the 401 page.
func isUnauthorizedPage(t *testing.T, resp *http.Response) bool {
	t.Helper()
	return resp.StatusCode == http.StatusUnauthorized &&
		resp.Header.Get("Content-Type") == "text/html; charset=utf-8" &&
		strings.Contains(body(t, resp), "run backsight dashboard link in a terminal and open the link it prints")
}

func TestCodeLogin(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ target, want string }{
		{"/quality?days=30&code=", "/quality?days=30"},
		{"/quality?days=30&code=%s&tz=UTC", "/quality?days=30&tz=UTC"},
		{"/?code=", "/"},
		{"/projects/back%20sight?code=", "/projects/back%20sight"},
	} {
		code := h.mint()
		target := c.target + code
		if strings.Contains(c.target, "%s") {
			target = strings.Replace(c.target, "%s", code, 1)
		}
		r := h.do("GET", target, "Sec-Fetch-Site", "none")
		if r.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET %s: %d, want 303", c.target, r.StatusCode)
		}
		if loc := r.Header.Values("Location"); len(loc) != 1 || loc[0] != c.want {
			t.Errorf("GET %s: Location %q, want %q", c.target, loc, c.want)
		}
		if b := body(t, r); strings.Contains(b, code) {
			t.Errorf("GET %s: the body repeats the code: %q", c.target, b)
		}
		raw := r.Header.Values("Set-Cookie")
		if len(raw) != 1 {
			t.Fatalf("GET %s: Set-Cookie %q, want one", c.target, raw)
		}
		ck, err := http.ParseSetCookie(raw[0])
		if err != nil {
			t.Fatalf("Set-Cookie %q: %v", raw[0], err)
		}
		if ck.Name != "backsight_dash_"+strconv.Itoa(h.port) || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode ||
			ck.Path != "/" || ck.Secure || ck.Domain != "" || !lowerHex(ck.Value, 64) {
			t.Errorf("Set-Cookie %q: want backsight_dash_%d=<64 hex>; Path=/; HttpOnly; SameSite=Strict", raw[0], h.port)
		}
		if !ck.Expires.IsZero() || ck.MaxAge != 0 || strings.Contains(strings.ToLower(raw[0]), "max-age") || strings.Contains(strings.ToLower(raw[0]), "expires") {
			t.Errorf("Set-Cookie %q: want a browser-session cookie, no Expires or Max-Age", raw[0])
		}
		if h.codeLeft(code) {
			t.Errorf("GET %s: the code was not spent", c.target)
		}
		if r := h.do("GET", c.want, cookieHdr(ck)...); r.StatusCode != http.StatusOK {
			t.Errorf("GET %s with the new session: %d, want 200", c.want, r.StatusCode)
		}
	}
}

func TestCodeOnce(t *testing.T) {
	h := newHarness(t)
	code := h.mint()
	if r := h.do("GET", "/?code="+code); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("first use: %d, want 303", r.StatusCode)
	}
	r := h.do("GET", "/?code="+code)
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("second use: %d, want 401", r.StatusCode)
	}
	if !strings.Contains(body(t, r), "backsight dashboard link") {
		t.Errorf("second use: body %q, want it to say to run backsight dashboard link", body(t, r))
	}
	if h.sessionCookie(r) != nil {
		t.Error("second use set a session cookie")
	}
}

func TestCodeRefused(t *testing.T) {
	h := newHarness(t)
	expired := h.mint()
	old := h.clock.now().Add(-121 * time.Second)
	if err := os.Chtimes(h.codeFile(expired), old, old); err != nil {
		t.Fatal(err)
	}
	for name, code := range map[string]string{
		"never minted": strings.Repeat("ab", 16),
		"malformed":    "xyz",
		"expired":      expired,
		"upper case":   strings.ToUpper(h.mint()),
		"empty":        "",
	} {
		r := h.do("GET", "/?code="+code)
		if !isUnauthorizedPage(t, r) {
			t.Errorf("%s code: %d %q, want the 401 page", name, r.StatusCode, body(t, r))
		}
		if h.sessionCookie(r) != nil {
			t.Errorf("%s code: a session cookie was set", name)
		}
	}
	if h.codeLeft(expired) {
		t.Error("the expired code's file is still there")
	}
	// Two codes in one link: neither is read.
	a, b := h.mint(), h.mint()
	if r := h.do("GET", "/?code="+a+"&code="+b); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("two codes: %d, want 401", r.StatusCode)
	}
	if !h.codeLeft(a) || !h.codeLeft(b) {
		t.Error("two codes: one was spent")
	}
}

func TestHeadDoesNotConsume(t *testing.T) {
	h := newHarness(t)
	code := h.mint()
	if r := h.do("HEAD", "/?code="+code); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("HEAD: %d, want 401", r.StatusCode)
	}
	if !h.codeLeft(code) {
		t.Fatal("HEAD spent the code")
	}
	if r := h.do("GET", "/?code="+code); r.StatusCode != http.StatusSeeOther {
		t.Errorf("GET after HEAD: %d, want 303", r.StatusCode)
	}
}

func TestPrefetchDoesNotConsume(t *testing.T) {
	h := newHarness(t)
	code := h.mint()
	for _, hdr := range [][]string{
		{"Sec-Purpose", "prefetch"},
		{"Sec-Purpose", "prefetch;prerender"},
		{"Sec-Purpose", "prefetch; anonymous-client-ip"},
		{"Purpose", "prefetch"},
		{"X-Moz", "prefetch"},
		{"X-Purpose", "preview"},
		{"X-Purpose", "Preview"},
	} {
		if r := h.do("GET", "/?code="+code, hdr...); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%q: %d, want 401", hdr, r.StatusCode)
		}
		if !h.codeLeft(code) {
			t.Fatalf("%q spent the code", hdr)
		}
	}
	if r := h.do("GET", "/?code="+code); r.StatusCode != http.StatusSeeOther {
		t.Errorf("GET after the prefetches: %d, want 303", r.StatusCode)
	}
}

func TestCrossSiteCodeNotConsumed(t *testing.T) {
	h := newHarness(t)
	code := h.mint()
	r := h.do("GET", "/?code="+code, "Sec-Fetch-Site", "cross-site")
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site: %d, want 403", r.StatusCode)
	}
	if !h.codeLeft(code) {
		t.Error("a cross-site request spent the code")
	}
}

// Login codes are read on page routes only: /api/, /prefs and the text
// page never spend one.
func TestCodeOnlyOnPages(t *testing.T) {
	h := newHarness(t)
	code := h.mint()
	for _, p := range []string{"/api/v1/overview", "/prefs", "/memories/mem-a1b2c3d4/text", "/nope"} {
		if r := h.do("GET", p+"?code="+code); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s with a code: %d, want 401", p, r.StatusCode)
		}
		if !h.codeLeft(code) {
			t.Fatalf("GET %s spent the code", p)
		}
	}
}

func TestReopenedLinkWithSession(t *testing.T) {
	h := newHarness(t)
	ck := h.login()
	spent := h.mint()
	if r := h.do("GET", "/?code="+spent); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("spend the code: %d", r.StatusCode)
	}
	for name, code := range map[string]string{"spent": spent, "malformed": "xyz"} {
		r := h.do("GET", "/quality?code="+code+"&days=7", cookieHdr(ck)...)
		if r.StatusCode != http.StatusSeeOther {
			t.Errorf("%s code with a session: %d, want 303", name, r.StatusCode)
		}
		if loc := r.Header.Get("Location"); loc != "/quality?days=7" {
			t.Errorf("%s code with a session: Location %q, want /quality?days=7", name, loc)
		}
		if sc := r.Header.Values("Set-Cookie"); len(sc) != 0 {
			t.Errorf("%s code with a session: Set-Cookie %q, want none", name, sc)
		}
	}
}

// location is the Location header of a response raw returned.
func location(resp string) []string {
	var locs []string
	head, _, _ := strings.Cut(resp, "\r\n\r\n")
	for _, l := range strings.Split(head, "\r\n") {
		if v, ok := strings.CutPrefix(l, "Location: "); ok {
			locs = append(locs, v)
		}
	}
	return locs
}

func TestLoginRedirectStaysLocal(t *testing.T) {
	h := newHarness(t)
	host := "\r\nHost: " + h.host() + "\r\nConnection: close\r\n\r\n"
	for _, target := range []string{"//evil.example/?code=", "//evil.example/../?code=", "///evil.example/..?code="} {
		resp := h.raw("GET " + target + h.mint() + " HTTP/1.1" + host)
		for _, loc := range location(resp) {
			if strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, "/\\") || strings.Contains(loc, "evil.example") {
				t.Errorf("GET %s: Location %q, want a local path", target, loc)
			}
		}
	}
	// A path that cleans to a page: logged in, sent home.
	resp := h.raw("GET //evil.example/../?code=" + h.mint() + " HTTP/1.1" + host)
	if got := rawStatus(t, resp); got != http.StatusSeeOther {
		t.Errorf("GET //evil.example/../ with a code: %d, want 303", got)
	}
	if loc := location(resp); len(loc) != 1 || loc[0] != "/" {
		t.Errorf("GET //evil.example/../ with a code: Location %q, want /", loc)
	}
	for _, target := range []string{"/%0d%0a?code=", "/projects/%0d%0a?code=", "/projects/a?x=%0d%0a&code="} {
		resp := h.raw("GET " + target + h.mint() + " HTTP/1.1" + host)
		for _, loc := range location(resp) {
			if strings.ContainsAny(loc, "\r\n") {
				t.Errorf("GET %s: Location %q holds CR or LF", target, loc)
			}
		}
	}
}

func TestLocalBack(t *testing.T) {
	for in, want := range map[string]string{
		"/":                      "/",
		"/quality?days=30":       "/quality?days=30",
		"/quality?days=30#top":   "/quality?days=30",
		"/projects/back%20sight": "/projects/back%20sight",
		"/memories?q=a%2F%2Fb":   "/memories?q=a%2F%2Fb",
		"/cost/../quality":       "/cost/../quality",
	} {
		if got, ok := localBack(in); !ok || got != want {
			t.Errorf("localBack(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"", "quality", "//evil.example/", "///evil.example", "/\\evil.example", "/a\\b", "/a//b", "/a?x=//b",
		"http://evil.example/", "https:/evil.example", "/a\r\nX-Injected: 1", "/a\tb", "/a\x7fb", "/a\x00",
		"javascript:alert(1)", "#/x",
	} {
		if got, ok := localBack(in); ok {
			t.Errorf("localBack(%q) = %q, true; want refused", in, got)
		}
	}
}

func TestSessionRequired(t *testing.T) {
	h := newHarness(t)
	ck := h.login()
	other := "backsight_dash_" + strconv.Itoa(h.port+1)
	cases := map[string][]string{
		"no cookie":          nil,
		"never issued":       {"Cookie", cookieName(h.port) + "=" + strings.Repeat("ab", 32)},
		"another port's":     {"Cookie", other + "=" + ck.Value},
		"a bearer":           {"Authorization", "Bearer " + ck.Value},
		"upper-case secret":  {"Cookie", cookieName(h.port) + "=" + strings.ToUpper(ck.Value)},
		"secret as its name": {"Cookie", ck.Value + "=" + ck.Value},
	}
	for name, hdr := range cases {
		for _, p := range []string{"/", "/quality", "/prefs", "/memories/mem-a1b2c3d4/text", "/nope"} {
			if r := h.do("GET", p, hdr...); !isUnauthorizedPage(t, r) {
				t.Errorf("%s on %s: %d %q, want the 401 page", name, p, r.StatusCode, body(t, r))
			}
		}
		r := h.do("GET", "/api/v1/overview", hdr...)
		if r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s on the API: %d, want 401", name, r.StatusCode)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("%s on the API: Content-Type %q", name, ct)
		}
		var got, want any
		if err := json.Unmarshal([]byte(body(t, r)), &got); err != nil {
			t.Fatalf("%s on the API: %v in %q", name, err, body(t, r))
		}
		_ = json.Unmarshal([]byte(`{"schema_version":1,"command":"dashboard","ok":false,"error":{"code":10,"message":"no valid session: run backsight dashboard link and open the link it prints"}}`), &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s on the API: body %v, want %v", name, got, want)
		}
	}
	// Every cookie of the name is tried, wherever the valid one sits.
	for _, hdr := range [][]string{
		{"Cookie", cookieName(h.port) + "=" + strings.Repeat("ab", 32) + "; " + cookieName(h.port) + "=" + ck.Value},
		{"Cookie", other + "=x; " + cookieName(h.port) + "=" + ck.Value},
		{"Cookie", cookieName(h.port) + "=junk", "Cookie", cookieName(h.port) + "=" + ck.Value},
	} {
		if r := h.do("GET", "/", hdr...); r.StatusCode != http.StatusOK {
			t.Errorf("%q: %d, want 200", hdr, r.StatusCode)
		}
	}
}

func TestSessionsEndToEnd(t *testing.T) {
	h := newHarness(t)
	var cookies []*http.Cookie
	for range 9 {
		cookies = append(cookies, h.login())
	}
	if r := h.do("GET", "/", cookieHdr(cookies[0])...); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("the first of nine sessions: %d, want 401 (dropped)", r.StatusCode)
	}
	for i, c := range cookies[1:] {
		if r := h.do("GET", "/", cookieHdr(c)...); r.StatusCode != http.StatusOK {
			t.Errorf("session %d of nine: %d, want 200", i+2, r.StatusCode)
		}
	}
	h.clock.add(2*time.Hour + time.Second)
	for i, c := range cookies[1:] {
		if r := h.do("GET", "/", cookieHdr(c)...); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("session %d after 2 h 1 s idle: %d, want 401", i+2, r.StatusCode)
		}
	}
}

func TestUnauthorizedPageAssets(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/")
	if !isUnauthorizedPage(t, r) {
		t.Fatalf("GET / without a session: %d %q", r.StatusCode, body(t, r))
	}
	b := body(t, r)
	for _, want := range []string{
		`<link rel="stylesheet" href="` + assetPath("dashboard.css") + `">`,
		`src="` + assetPath("logo-light.svg") + `"`,
		`width="64" height="64"`,
		"This dashboard needs a fresh link: run backsight dashboard link in a terminal and open the link it prints.",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("the 401 page lacks %q:\n%s", want, b)
		}
	}
	for _, name := range []string{"dashboard.css", "logo-light.svg"} {
		if r := h.do("GET", assetPath(name)); r.StatusCode != http.StatusOK {
			t.Errorf("GET %s without a session: %d, want 200", assetPath(name), r.StatusCode)
		}
	}
}

func TestTemplatesFS(t *testing.T) {
	b, err := fs.ReadFile(Templates(), "unauthorized.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "{{/* views: none */}}") {
		t.Errorf("unauthorized.html does not start with its views header: %q", b[:min(len(b), 40)])
	}
}

func TestFirstLink(t *testing.T) {
	h := newHarness(t)
	l := h.s.FirstLink()
	if !regexp.MustCompile(`^http://127\.0\.0\.1:` + strconv.Itoa(h.port) + `/\?code=[0-9a-f]{32}$`).MatchString(l.URL) {
		t.Errorf("FirstLink().URL %q", l.URL)
	}
	if l.Port != h.port {
		t.Errorf("FirstLink().Port %d, want %d", l.Port, h.port)
	}
	if want := indexgen.Now.Add(120 * time.Second); !l.ExpiresAt.Equal(want) {
		t.Errorf("FirstLink().ExpiresAt %v, want %v (the clock at mint + 120 s)", l.ExpiresAt, want)
	}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || keys["url"] != l.URL || keys["port"] != float64(h.port) || keys["expires_at"] == nil {
		t.Errorf("FirstLink as JSON: %s, want url, port and expires_at", raw)
	}
	path := strings.TrimPrefix(l.URL, "http://"+h.host())
	if r := h.do("GET", path); r.StatusCode != http.StatusSeeOther || h.sessionCookie(r) == nil {
		t.Errorf("the first link: %d, want 303 with a session", r.StatusCode)
	}
	if r := h.do("GET", path); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("the first link again: %d, want 401", r.StatusCode)
	}
}
