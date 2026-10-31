package dashboard

import (
	"bytes"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestAssetsPublic(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ name, docs, ctype string }{
		{"logo-light.svg", "../../docs/assets/logo-light.svg", "image/svg+xml"},
		{"logo-dark.svg", "../../docs/assets/logo-dark.svg", "image/svg+xml"},
		{"dashboard.css", "assets/dashboard.css", "text/css; charset=utf-8"},
	} {
		want, err := os.ReadFile(c.docs)
		if err != nil {
			t.Fatal(err)
		}
		r := h.do("GET", assetPath(c.name))
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d, want 200", assetPath(c.name), r.StatusCode)
		}
		if got := body(t, r); got != string(want) {
			t.Errorf("%s: body differs from %s", c.name, c.docs)
		}
		if got := r.Header.Get("Content-Type"); got != c.ctype {
			t.Errorf("%s: Content-Type %q, want %q", c.name, got, c.ctype)
		}
		if r := h.do("HEAD", assetPath(c.name)); r.StatusCode != http.StatusOK || body(t, r) != "" {
			t.Errorf("HEAD %s: %d with a body", c.name, r.StatusCode)
		}
	}
	for _, p := range []string{
		"/assets/logo-light.svg", "/assets/..%2Fserver.go", "/assets/", "/assets/dashboard.000000000000.css",
		assetPath("logo-light.svg") + "x",
		strings.TrimSuffix(assetPath("logo-light.svg"), ".svg") + "%2Esvg", // the exact path only, not a decoded one
	} {
		if r := h.do("GET", p); r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, r.StatusCode)
		}
	}
}

func TestAssetPath(t *testing.T) {
	re := regexp.MustCompile(`^/assets/(logo-light|logo-dark|dashboard)\.[0-9a-f]{12}\.(svg|css)$`)
	seen := map[string]bool{}
	for _, n := range []string{"logo-light.svg", "logo-dark.svg", "dashboard.css"} {
		p := assetPath(n)
		if !re.MatchString(p) {
			t.Errorf("assetPath(%q) = %q", n, p)
		}
		seen[p] = true
	}
	if len(seen) != 3 {
		t.Errorf("assetPath names collide: %v", seen)
	}
	// The hash follows the content: a changed file gets a new name.
	a, b := hashedName("x.css", []byte("a")), hashedName("x.css", []byte("b"))
	if a == b {
		t.Errorf("two contents, one name %q", a)
	}
}

func TestEmbeddedLogosMatchDocs(t *testing.T) {
	for _, n := range []string{"logo-light.svg", "logo-dark.svg"} {
		emb, err := assetFS.ReadFile("assets/" + n)
		if err != nil {
			t.Fatal(err)
		}
		docs, err := os.ReadFile("../../docs/assets/" + n)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(emb, docs) {
			t.Errorf("assets/%s differs from docs/assets/%s: copy it again", n, n)
		}
	}
}
