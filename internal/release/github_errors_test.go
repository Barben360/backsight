package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An asset the API refuses is an error naming the asset and the status,
// never a body read as the asset.
func TestGitHubOpenRefusesAnAssetTheAPIRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	t.Cleanup(srv.Close)
	g := newTestGitHub(srv.URL, "")
	rel := Release{Tag: "v0.2.0", Assets: []Asset{{Name: "checksums.txt", URL: srv.URL + "/repos/Barben360/backsight/releases/assets/7"}}}
	rc, err := g.Open(context.Background(), rel, "checksums.txt")
	if err == nil {
		_ = rc.Close()
		t.Fatal("a 403 opened as the asset")
	}
	for _, s := range []string{"checksums.txt", "403", "rate limit exceeded", "GH_TOKEN"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error lacks %q: %v", s, err)
		}
	}
}

// A releases list that never comes stops at the source's timeout.
func TestGitHubReleasesTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	g := newTestGitHub(srv.URL, "")
	g.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := g.Releases(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unreachable") || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("a hanging server: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %s, the timeout is %s", d, g.Timeout)
	}
}
