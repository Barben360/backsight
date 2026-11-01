package cli

import (
	"bytes"
	"image/png"
	"os"
	"strings"
	"testing"
)

// The README's dashboard section shows the Overview in both themes: each
// image exists, is a 1440 x 900 PNG and stays light enough for a page that
// loads on GitHub.
func TestReadmeShowsTheDashboard(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut("\n"+string(raw), "\n## The dashboard\n")
	if !ok {
		t.Fatal(`README.md has no "## The dashboard" section`)
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	if !strings.Contains(section, "docs/dashboard.md") {
		t.Error("the dashboard section does not link docs/dashboard.md")
	}
	for _, img := range []struct{ file, mode string }{
		{"dashboard-light.png", "gh-light-mode-only"},
		{"dashboard-dark.png", "gh-dark-mode-only"},
	} {
		if !strings.Contains(section, "docs/assets/"+img.file+"#"+img.mode) {
			t.Errorf("the dashboard section does not show docs/assets/%s#%s", img.file, img.mode)
		}
		data, err := os.ReadFile("../../docs/assets/" + img.file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			t.Errorf("%s is not a PNG", img.file)
		}
		if len(data) > 512000 {
			t.Errorf("%s is %d bytes, over 512000", img.file, len(data))
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", img.file, err)
		}
		if cfg.Width != 1440 || cfg.Height != 900 {
			t.Errorf("%s is %d x %d, want 1440 x 900", img.file, cfg.Width, cfg.Height)
		}
	}
}
