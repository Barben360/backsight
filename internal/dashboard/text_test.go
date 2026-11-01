package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
)

// textCanary is the memory text the fake server returns: it must show on
// the text page and nowhere else.
const textCanary = "CANARY-TEXT"

// lockedBuffer is a goroutine-safe writer for the dashboard's Stderr.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// textHarness is a data harness whose server returns text, and the first
// memory of the index's list with its text page's path.
func textHarness(t *testing.T, text string, opts ...harnessOpt) (h *harness, mem, path string) {
	t.Helper()
	h = newDataHarness(t, append([]harnessOpt{withIndex(), withServer(), withDoctor()}, opts...)...)
	h.srv.set(func(f *fakeServer) { f.memText = text })
	mem = apiPath(t, h, "/api/v1/memories").Tables.Memories[0].ID
	return h, mem, "/memories/" + url.PathEscape(mem) + "/text"
}

// rawGet is GET path under ck with the extra headers, and its raw body.
func rawGet(t *testing.T, h *harness, ck *http.Cookie, path string, hdr ...string) (*http.Response, string) {
	t.Helper()
	r := h.do("GET", path, append(hdr, cookieHdr(ck)...)...)
	return r, body(t, r)
}

func TestTextPageShowsTheTextEscaped(t *testing.T) {
	h, mem, path := textHarness(t, "<script>alert(1)</script> "+textCanary)
	r, b := rawGet(t, h, h.login(), path)
	if r.StatusCode != 200 {
		t.Fatalf("GET %s: %d %q", path, r.StatusCode, b)
	}
	if !strings.Contains(b, "&lt;script&gt;alert(1)&lt;/script&gt; "+textCanary) {
		t.Errorf("the escaped text is not on the page: %q", b)
	}
	if strings.Contains(b, "<script>alert") {
		t.Errorf("the text is not escaped: %q", b)
	}
	if !strings.Contains(b, "<pre>&lt;script&gt;") {
		t.Errorf("the text is not in <pre>: %q", b)
	}
	if !strings.Contains(b, `href="/memories/`+url.PathEscape(mem)+`"`) {
		t.Errorf("no link back to the memory %s", mem)
	}
	if got := r.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	// The page takes no parameter: no window links, no project filter.
	if strings.Contains(b, "?days=") || strings.Contains(b, `name="project"`) {
		t.Errorf("the text page offers filters it refuses: %q", b)
	}
	if got := h.srv.count("Memory"); got != 1 {
		t.Errorf("Memory called %d times, want 1", got)
	}
}

func TestTextPageRefusesABearer(t *testing.T) {
	h, _, path := textHarness(t, textCanary)
	ck := h.login()
	r := h.do("GET", path, "Authorization", "Bearer "+ck.Value)
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("a bearer: %d, want 401", r.StatusCode)
	}
	if strings.Contains(body(t, r), textCanary) {
		t.Error("a bearer read the text")
	}
	if got := h.srv.count("Memory"); got != 0 {
		t.Errorf("Memory called %d times for a bearer, want 0", got)
	}
}

func TestTextPageRefusesCrossSite(t *testing.T) {
	h, _, path := textHarness(t, textCanary)
	ck := h.login()
	cases := []struct {
		hdr  []string
		want int
	}{
		{[]string{"Sec-Fetch-Site", "cross-site"}, 403},
		{[]string{"Sec-Fetch-Site", "same-site"}, 403},
		{[]string{"Sec-Fetch-Dest", "image"}, 403},
		{[]string{"Sec-Fetch-Dest", "empty"}, 403},
		{[]string{"Sec-Fetch-Dest", "iframe"}, 403},
		{[]string{"Sec-Fetch-Site", "same-origin", "Sec-Fetch-Dest", "document"}, 200},
		{[]string{"Sec-Fetch-Site", "none", "Sec-Fetch-Dest", "document"}, 200}, // the address bar
		{nil, 200},
	}
	oks := 0
	for _, c := range cases {
		r, b := rawGet(t, h, ck, path, c.hdr...)
		if r.StatusCode != c.want {
			t.Errorf("%q: %d, want %d", c.hdr, r.StatusCode, c.want)
		}
		if shown := strings.Contains(b, textCanary); shown != (c.want == 200) {
			t.Errorf("%q: text shown %t", c.hdr, shown)
		}
		if c.want == 200 {
			oks++
		}
		if got := h.srv.count("Memory"); got != oks {
			t.Errorf("after %q: Memory called %d times, want %d (only for the 200s)", c.hdr, got, oks)
		}
	}
}

// Beyond the destinations the spec names: any destination but a document,
// a repeated header, a prefetch and a HEAD read nothing.
func TestTextPageRefusesWhatIsNotAnOpenedPage(t *testing.T) {
	h, _, path := textHarness(t, textCanary)
	ck := h.login()
	cases := map[string][]string{
		"object":           {"Sec-Fetch-Dest", "object"},
		"embed":            {"Sec-Fetch-Dest", "embed"},
		"frame":            {"Sec-Fetch-Dest", "frame"},
		"script":           {"Sec-Fetch-Dest", "script"},
		"empty value":      {"Sec-Fetch-Dest", ""},
		"two values":       {"Sec-Fetch-Dest", "document", "Sec-Fetch-Dest", "image"},
		"upper case":       {"Sec-Fetch-Dest", "Document"},
		"prefetch":         {"Sec-Fetch-Dest", "document", "Sec-Purpose", "prefetch"},
		"prerender":        {"Sec-Fetch-Dest", "document", "Sec-Purpose", "prefetch;prerender"},
		"legacy prefetch":  {"Purpose", "prefetch"},
		"firefox prefetch": {"X-Moz", "prefetch"},
		"safari preview":   {"X-Purpose", "preview"},
		"cors mode":        {"Sec-Fetch-Mode", "cors"},
		"no-cors mode":     {"Sec-Fetch-Mode", "no-cors"},
		"same-origin mode": {"Sec-Fetch-Mode", "same-origin"},
		"two modes":        {"Sec-Fetch-Mode", "navigate", "Sec-Fetch-Mode", "cors"},
	}
	for name, hdr := range cases {
		r, b := rawGet(t, h, ck, path, hdr...)
		if r.StatusCode != http.StatusForbidden || strings.Contains(b, textCanary) {
			t.Errorf("%s: %d %q, want 403 without the text", name, r.StatusCode, b)
		}
	}
	if got := h.srv.count("Memory"); got != 0 {
		t.Errorf("Memory called %d times, want 0", got)
	}
	// A navigation without a destination (an older browser) is the page.
	if r, b := rawGet(t, h, ck, path, "Sec-Fetch-Mode", "navigate"); r.StatusCode != 200 || !strings.Contains(b, textCanary) {
		t.Errorf("Sec-Fetch-Mode navigate alone: %d, want 200 with the text", r.StatusCode)
	}
	r := h.do("HEAD", path, cookieHdr(ck)...)
	if r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != "GET" {
		t.Errorf("HEAD: %d, Allow %q; want 405, GET", r.StatusCode, r.Header.Get("Allow"))
	}
	if got := h.srv.count("Memory"); got != 1 {
		t.Errorf("Memory called %d times, want 1 (the navigation only)", got)
	}
}

// The auto-refresh preference does not reach the text page, nor its state
// pages; the menu still shows it as set, and other pages keep it.
func TestTextPageNeverRefreshes(t *testing.T) {
	h, _, path := textHarness(t, textCanary)
	hdr := withPrefs(h.login(), "v1.system.0.1")
	for _, p := range []string{path, "/memories/zzzzzzzz/text", path + "?days=7"} {
		r := h.do("GET", p, hdr...)
		b := body(t, r)
		if strings.Contains(b, `http-equiv="refresh"`) {
			t.Errorf("%s (%d) refreshes on its own: %q", p, r.StatusCode, b)
		}
		if doc := parseHTML(t, b); doc.first("div.page-head h1") == nil || doc.first("div.page-head span") != nil || doc.first("nav.seg") != nil {
			t.Errorf("%s: the text page's head or window links: %q", p, b)
		}
		if !strings.Contains(b, `<a href="/prefs?refresh=1&amp;back=`+url.QueryEscape(p)+`" aria-current="true"><span class="check">✓</span>Every minute</a>`) {
			t.Errorf("%s: the menu does not show auto-refresh as on: %q", p, b)
		}
	}
	if b := body(t, h.do("GET", "/memories", hdr...)); !strings.Contains(b, `<meta http-equiv="refresh" content="60">`) {
		t.Errorf("the preference no longer refreshes other pages: %q", b)
	}
}

func TestTextPageNeverCached(t *testing.T) {
	h, _, path := textHarness(t, "A-"+textCanary)
	ck := h.login()
	cached := func() (results int, server int) {
		c := h.s.data.results
		c.mu.Lock()
		results = len(c.entries)
		for k, e := range c.entries {
			raw, err := json.Marshal(e.res)
			if err != nil || bytes.Contains(raw, []byte(textCanary)) {
				t.Errorf("result cache entry %s holds the text (or fails to marshal: %v)", k, err)
			}
		}
		c.mu.Unlock()
		srv := h.s.data.src.Server.(*cachedServer)
		srv.mu.Lock()
		server = len(srv.entries)
		for k, e := range srv.entries {
			raw, err := json.Marshal(e.v)
			if err != nil || bytes.Contains(raw, []byte(textCanary)) {
				t.Errorf("server cache entry %s holds the text (or fails to marshal: %v)", k, err)
			}
		}
		srv.mu.Unlock()
		return results, server
	}
	results, server := cached()
	if _, b := rawGet(t, h, ck, path); !strings.Contains(b, "A-"+textCanary) {
		t.Fatalf("first text not shown: %q", b)
	}
	h.srv.set(func(f *fakeServer) { f.memText = "B-" + textCanary })
	r, b := rawGet(t, h, ck, path)
	if !strings.Contains(b, "B-"+textCanary) || strings.Contains(b, "A-"+textCanary) {
		t.Errorf("second GET: %q, want the new text only", b)
	}
	if got := r.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	if got := h.srv.count("Memory"); got != 2 {
		t.Errorf("Memory called %d times, want 2", got)
	}
	if r2, s2 := cached(); r2 != results || s2 != server {
		t.Errorf("caches went from %d results, %d server reads to %d, %d", results, server, r2, s2)
	}
}

func TestTextPageParams(t *testing.T) {
	h, _, path := textHarness(t, textCanary)
	ck := h.login()
	for _, q := range []string{"?days=7", "?text=1", "?code=" + strings.Repeat("ab", 16), "?x", "?%zz"} {
		r, b := rawGet(t, h, ck, path+q)
		if r.StatusCode != http.StatusBadRequest || strings.Contains(b, textCanary) {
			t.Errorf("%s: %d %q, want 400 without the text", q, r.StatusCode, b)
		}
	}
	if got := h.srv.count("Memory"); got != 0 {
		t.Errorf("Memory called %d times, want 0", got)
	}
}

func TestTextPageUnknownMemory(t *testing.T) {
	h, _, _ := textHarness(t, textCanary)
	r, b := pageGet(t, h, h.login(), "/memories/zzzzzzzz/text")
	if r.StatusCode != http.StatusNotFound || !strings.Contains(b, `No memory "zzzzzzzz" in the metrics index.`) {
		t.Errorf("%d %q, want 404 naming the memory", r.StatusCode, b)
	}
	if strings.Contains(b, "?days=") {
		t.Errorf("the not-found text page offers window links it refuses: %q", b)
	}
	if got := h.srv.count("Memory"); got != 0 {
		t.Errorf("Memory called %d times for an unknown id, want 0", got)
	}
}

// Ids that try the path or the markup are only ever not found, and shown
// escaped.
func TestTextPageOddIDs(t *testing.T) {
	h, _, _ := textHarness(t, textCanary)
	ck := h.login()
	for _, p := range []string{"/memories/%3Cb%3Ex%3C%2Fb%3E1234/text", "/memories/a%2F..%2F..%2Fetc/text", "/memories/%22%27%3E%3Cscript%3E/text", "/memories/%20/text"} {
		r, b := rawGet(t, h, ck, p)
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, r.StatusCode)
		}
		if strings.Contains(b, "<b>x") || strings.Contains(b, "<script>") || strings.Contains(b, textCanary) {
			t.Errorf("%s: %q", p, b)
		}
	}
	// A path with dot segments is cleaned by net/http and redirected, never served.
	if r := h.do("GET", "/memories/x/../../text", cookieHdr(ck)...); r.StatusCode == 200 {
		t.Errorf("a dot-segment path was served: %d", r.StatusCode)
	}
	if got := h.srv.count("Memory"); got != 0 {
		t.Errorf("Memory called %d times, want 0", got)
	}
}

// The index's states apply: each is its page, and the server is not read.
func TestTextPageStates(t *testing.T) {
	cases := map[string]struct {
		mod  func(*fakeIndex)
		want string
	}{
		"updated":  {func(f *fakeIndex) { f.side = true }, updatedPage},
		"updating": {func(f *fakeIndex) { f.openErrs = []error{index.ErrUpdateInProgress, index.ErrUpdateInProgress} }, "An update is being applied"},
		"broken":   {func(f *fakeIndex) { f.openErrs = []error{index.ErrUpdateInProgress, errors.New("boom /secret/path")} }, "The metrics index could not be read"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := dataHarness(t, c.mod)
			h.srv.set(func(f *fakeServer) { f.memText = textCanary })
			r, b := pageGet(t, h, h.login(), "/memories/mem-a1b2c3d4/text")
			if r.StatusCode != 503 || !strings.Contains(b, c.want) || strings.Contains(b, textCanary) || strings.Contains(b, "secret") {
				t.Errorf("%d %q, want 503 with %q", r.StatusCode, b, c.want)
			}
			if got := h.srv.count("Memory"); got != 0 {
				t.Errorf("Memory called %d times, want 0", got)
			}
		})
	}
}

func TestTextServerDown(t *testing.T) {
	old := metrics.ServerTimeout
	metrics.ServerTimeout = 20 * time.Millisecond
	t.Cleanup(func() { metrics.ServerTimeout = old })
	h, _, path := textHarness(t, textCanary)
	h.srv.set(func(f *fakeServer) { f.down = true })
	r, b := pageGet(t, h, h.login(), path)
	if r.StatusCode != 200 || !strings.Contains(b, "The text could not be read: memory server not answering.") {
		t.Errorf("%d %q, want 200 saying the server is not answering", r.StatusCode, b)
	}
	if strings.Contains(b, textCanary) || strings.Contains(b, "<pre>") {
		t.Errorf("text shown with the server down: %q", b)
	}
}

// A server error's own words are never shown: only the reason's.
func TestTextServerErrorBodyNeverShown(t *testing.T) {
	h, _, path := textHarness(t, textCanary)
	h.srv.set(func(f *fakeServer) { f.fail = errSecret })
	r, b := pageGet(t, h, h.login(), path)
	if r.StatusCode != 200 || strings.Contains(b, "CANARY-SERVER-ERROR") || strings.Contains(b, textCanary) {
		t.Errorf("%d %q", r.StatusCode, b)
	}
	if !strings.Contains(b, "The text could not be read") {
		t.Errorf("no reason given: %q", b)
	}
}

// errSecret is a server failure whose text must never reach a page.
var errSecret = &secretError{}

type secretError struct{}

func (*secretError) Error() string { return "status 500: CANARY-SERVER-ERROR /home/secret" }

func TestTextPageCountsAsAPage(t *testing.T) {
	h, _, path := textHarness(t, textCanary)
	ck := h.login()
	before := h.s.Pages()
	if r, _ := rawGet(t, h, ck, path); r.StatusCode != 200 {
		t.Fatalf("text page: %d", r.StatusCode)
	}
	if got := h.s.Pages(); got != before+1 {
		t.Errorf("Pages() = %d after one text page, want %d", got, before+1)
	}
	rawGet(t, h, ck, path, "Sec-Fetch-Dest", "image")
	rawGet(t, h, ck, path+"?days=7")
	if got := h.s.Pages(); got != before+1 {
		t.Errorf("Pages() = %d after two refused text pages, want %d", got, before+1)
	}
}

func TestAPINeverReturnsText(t *testing.T) {
	stderr := &lockedBuffer{}
	h, mem, path := textHarness(t, textCanary, func(_ *harness, c *Config) { c.Stderr = stderr })
	ck := h.login()
	esc := url.PathEscape(mem)
	for _, p := range []string{"/api/v1/memories/" + esc, "/memories/" + esc} {
		r, b := rawGet(t, h, ck, p)
		if r.StatusCode != 200 || strings.Contains(b, textCanary) {
			t.Errorf("%s: %d, text shown %t", p, r.StatusCode, strings.Contains(b, textCanary))
		}
	}
	r, m := apiGet(t, h, ck, "/api/v1/memories/"+esc+"?text=1")
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("?text=1: %d %v, want 400", r.StatusCode, m)
	}
	if raw, _ := json.Marshal(m); bytes.Contains(raw, []byte(textCanary)) {
		t.Errorf("?text=1 returned the text: %s", raw)
	}
	// The text page shows it (the canary is reachable) ...
	if _, b := rawGet(t, h, ck, path); !strings.Contains(b, textCanary) {
		t.Fatalf("the text page lacks the text: %q", b)
	}
	// ... and no other page, no view of the API, nor stderr.
	paths := browsePaths(t, h)
	for _, rt := range routes {
		if rt.Kind == "page" && !strings.Contains(rt.Pattern, "{") {
			paths = append(paths, rt.Sample)
		}
	}
	for _, v := range metrics.Views {
		paths = append(paths, "/api/v1/"+v, "/api/v1/"+v+"?dev=1")
	}
	paths = append(paths, "/api/v1/memories/"+esc+"?dev=1", "/memories/"+esc+"?dev=1")
	for _, p := range paths {
		if r, b := rawGet(t, h, ck, p); strings.Contains(b, textCanary) || strings.Contains(fmt.Sprint(r.Header), textCanary) {
			t.Errorf("%s shows the memory text", p)
		}
	}
	if strings.Contains(stderr.String(), textCanary) {
		t.Errorf("the text reached stderr: %q", stderr.String())
	}
}

// While the first build runs, a page asks again by itself, but the text
// page does not: each reading of the text is a click.
func TestBuildingPageRefreshOnlyOffTheTextPage(t *testing.T) {
	h := newDataHarness(t, withIndex(func(f *fakeIndex) { f.openGate = make(chan struct{}) }), withServer(), withNeedsBuild())
	ck := h.login()
	const refresh = `<meta http-equiv="refresh" content="2">`
	for path, want := range map[string]bool{"/memories": true, "/memories/zzzzzzzz/text": false} {
		r, b := rawGet(t, h, ck, path, "Sec-Fetch-Dest", "document")
		if r.StatusCode != 503 || !strings.Contains(b, "Building the metrics index") {
			t.Fatalf("GET %s: %d %q, want the building page", path, r.StatusCode, b)
		}
		if got := strings.Contains(b, refresh); got != want {
			t.Errorf("GET %s while building: refresh %v, want %v", path, got, want)
		}
	}
}
