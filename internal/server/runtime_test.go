package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Barben360/backsight/internal/config"
)

// Deployed reads back the image Render wrote for the hindsight service,
// never the database's, and the version a runtime deploys follows
// WithVersion.
func TestDockerDeployedIsTheRenderedHindsightImage(t *testing.T) {
	dir := t.TempDir()
	d := Docker{Compose: Compose{Dir: dir}, Cfg: config.Config{Server: config.Server{URL: "http://127.0.0.1:8888", UIPort: 9999}, Embeddings: config.Embeddings{Preset: "multilingual"}}}
	if v, err := d.Deployed(); err != nil || v != "" {
		t.Fatalf("nothing rendered: %q %v", v, err)
	}
	other := "ghcr.io/vectorize-io/hindsight:1.2.0@sha256:" + string(make64('c'))
	rt := d.WithVersion(other)
	if rt.Version() != other || d.Version() != HindsightImage {
		t.Fatalf("versions %q %q", rt.Version(), d.Version())
	}
	f, err := rt.Render(config.Secrets{APIKey: "k", UIKey: "u", DBPassword: "p", LLMAPIKey: "l"}, "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Write(f); err != nil {
		t.Fatal(err)
	}
	if v, err := d.Deployed(); err != nil || v != other {
		t.Errorf("deployed %q %v, want %q", v, err, other)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if ServiceImage(raw, "db") != PostgresImage {
		t.Errorf("db image %q", ServiceImage(raw, "db"))
	}
}

func make64(c byte) []byte {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return b
}
