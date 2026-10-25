package release

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub answers the releases list and one asset, recording the
// headers of every request.
type fakeGitHub struct {
	mu      sync.Mutex
	headers map[string]http.Header // path → headers of its last request
	status  int                    // for the list; 0 = 200
	assetTo string                 // redirect the asset there when set
}

func (f *fakeGitHub) seen(path string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers[path]
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if f.headers == nil {
			f.headers = map[string]http.Header{}
		}
		f.headers[r.URL.Path] = r.Header.Clone()
		f.mu.Unlock()
		switch r.URL.Path {
		case "/repos/Barben360/backsight/releases":
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("per_page %q", r.URL.Query().Get("per_page"))
			}
			if f.status != 0 {
				w.WriteHeader(f.status)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = io.WriteString(w, `[
 {"tag_name":"v0.2.0-m1","prerelease":true,"draft":false,"published_at":"2026-10-30T10:00:00Z","body":"## Changelog\n* abc1234 ✨ Add update check",
  "assets":[{"name":"checksums.txt","size":321,"url":"`+srv.URL+`/repos/Barben360/backsight/releases/assets/7"}]},
 {"tag_name":"v0.3.0","prerelease":false,"draft":true,"published_at":null,"body":"draft","assets":[]},
 {"tag_name":"v0.1.0","prerelease":false,"draft":false,"published_at":"2026-10-24T11:53:58Z","body":"first","assets":[]}]`)
		case "/repos/Barben360/backsight/releases/assets/7":
			if r.Header.Get("Accept") != "application/octet-stream" {
				http.Error(w, "json", http.StatusUnsupportedMediaType)
				return
			}
			if f.assetTo != "" {
				http.Redirect(w, r, f.assetTo+"/blob", http.StatusFound)
				return
			}
			_, _ = io.WriteString(w, "sums")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestGitHub(url, token string) *GitHub {
	g := NewGitHub("Barben360", "backsight", token)
	g.BaseURL = url
	return g
}

func TestGitHubListsPublishedReleases(t *testing.T) {
	f := &fakeGitHub{}
	srv := f.serve(t)
	rels, err := newTestGitHub(srv.URL, "").Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 { // the draft is not a release
		t.Fatalf("%+v", rels)
	}
	r := rels[0]
	if r.Version != "0.2.0-m1" || r.Tag != "v0.2.0-m1" || !r.Prerelease || !r.Published.Equal(time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)) ||
		!strings.Contains(r.Notes, "Add update check") || len(r.Assets) != 1 || r.Assets[0].Name != "checksums.txt" || r.Assets[0].Size != 321 {
		t.Errorf("%+v", r)
	}
	h := f.seen("/repos/Barben360/backsight/releases")
	if h.Get("Accept") != "application/vnd.github+json" || h.Get("X-GitHub-Api-Version") == "" || !strings.HasPrefix(h.Get("User-Agent"), "backsight") {
		t.Errorf("headers %v", h)
	}
}

func TestGitHubSendsTheTokenOnlyWhenSet(t *testing.T) {
	f := &fakeGitHub{}
	srv := f.serve(t)
	if _, err := newTestGitHub(srv.URL, "").Releases(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.seen("/repos/Barben360/backsight/releases")["Authorization"]; ok {
		t.Error("an Authorization header without a token")
	}
	if _, err := newTestGitHub(srv.URL, "tok123").Releases(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.seen("/repos/Barben360/backsight/releases").Get("Authorization"); got != "Bearer tok123" {
		t.Errorf("Authorization %q", got)
	}
}

func TestGitHubNotFoundNamesTheToken(t *testing.T) {
	f := &fakeGitHub{status: http.StatusNotFound}
	srv := f.serve(t)
	_, err := newTestGitHub(srv.URL, "").Releases(context.Background())
	if err == nil || !strings.Contains(err.Error(), "GH_TOKEN") || !strings.Contains(err.Error(), "404") {
		t.Errorf("%v", err)
	}
	f.status = http.StatusUnauthorized
	if _, err := newTestGitHub(srv.URL, "bad").Releases(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("%v", err)
	}
}

func TestGitHubUnreachable(t *testing.T) {
	// A listener that hangs up on every connection: unlike a closed port,
	// no other process can take it meanwhile.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if _, err := newTestGitHub("http://"+ln.Addr().String(), "").Releases(context.Background()); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("a server that hangs up: %v", err)
	}
}

// Releases is bounded by its timeout; Open reports an asset the API
// refuses (not a body to verify).
func TestGitHubTimeoutAndRefusedAsset(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases") {
			select {
			case <-stall:
			case <-r.Context().Done():
			}
			return
		}
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()
	defer close(stall)
	g := newTestGitHub(srv.URL, "")
	g.Timeout = 100 * time.Millisecond
	start := time.Now()
	if _, err := g.Releases(context.Background()); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("a stalled list: %v after %s", err, time.Since(start))
	}
	rel := Release{Tag: "v1.0.0", Assets: []Asset{{Name: "checksums.txt", URL: srv.URL + "/assets/1"}}}
	if rc, err := g.Open(context.Background(), rel, "checksums.txt"); err == nil || !strings.Contains(err.Error(), "410") {
		if rc != nil {
			_ = rc.Close()
		}
		t.Errorf("a refused asset: %v", err)
	}
}

func TestGitHubOpensAnAssetThroughTheAPI(t *testing.T) {
	f := &fakeGitHub{}
	srv := f.serve(t)
	g := newTestGitHub(srv.URL, "tok123")
	rels, err := g.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rc, err := g.Open(context.Background(), rels[0], "checksums.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "sums" {
		t.Errorf("body %q", body)
	}
	if got := f.seen("/repos/Barben360/backsight/releases/assets/7").Get("Authorization"); got != "Bearer tok123" {
		t.Errorf("a private asset needs the token: %q", got)
	}
	if _, err := g.Open(context.Background(), rels[0], "nope.tar.gz"); err == nil {
		t.Error("an absent asset opened")
	}
}

// The API redirects asset downloads to a storage host: the token must not
// follow, nor be sent to an asset URL outside the API host.
func TestGitHubKeepsTheTokenOnTheAPIHost(t *testing.T) {
	var mu sync.Mutex
	var storageAuth []string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		storageAuth = append(storageAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.WriteString(w, "from storage")
	}))
	t.Cleanup(storage.Close)
	f := &fakeGitHub{assetTo: storage.URL}
	srv := f.serve(t)
	g := newTestGitHub(srv.URL, "tok123")
	rels, err := g.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rc, err := g.Open(context.Background(), rels[0], "checksums.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	foreign := rels[0]
	foreign.Assets = []Asset{{Name: "x", URL: storage.URL + "/x"}}
	rc2, err := g.Open(context.Background(), foreign, "x")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc2.Close()
	mu.Lock()
	defer mu.Unlock()
	if string(body) != "from storage" || len(storageAuth) != 2 || storageAuth[0] != "" || storageAuth[1] != "" {
		t.Errorf("body %q, storage saw Authorization %q", body, storageAuth)
	}
}

func TestTokenFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := TokenFromEnv(env(map[string]string{"GH_TOKEN": "a", "GITHUB_TOKEN": "b"})); got != "a" {
		t.Errorf("GH_TOKEN first: %q", got)
	}
	if got := TokenFromEnv(env(map[string]string{"GITHUB_TOKEN": "b"})); got != "b" {
		t.Errorf("GITHUB_TOKEN: %q", got)
	}
	if got := TokenFromEnv(env(nil)); got != "" {
		t.Errorf("none: %q", got)
	}
}
