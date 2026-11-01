package cli

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/metrics"
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
	const alt = `alt="The dashboard's Overview: the verdict, the summary line and six tiles" src="docs/assets/`
	var shots [][]byte
	for _, img := range []struct{ file, mode string }{
		{"dashboard/overview-healthy-light.png", "gh-light-mode-only"},
		{"dashboard/overview-healthy-dark.png", "gh-dark-mode-only"},
	} {
		if !strings.Contains(section, "docs/assets/"+img.file+"#"+img.mode) {
			t.Errorf("the dashboard section does not show docs/assets/%s#%s", img.file, img.mode)
		}
		if !strings.Contains(section, alt+img.file) {
			t.Errorf("%s has no alt text of the Overview", img.file)
		}
		data, err := os.ReadFile("../../docs/assets/" + img.file)
		if err != nil {
			t.Fatal(err)
		}
		shots = append(shots, data)
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
	if len(shots) == 2 && bytes.Equal(shots[0], shots[1]) {
		t.Error("the light and the dark screenshots are the same file")
	}
}

// Every command and drill the metrics catalog suggests is one the command
// tree has, flags included: users read them in Definitions, in `stats defs`
// and on the pages' CLI lines.
func TestCatalogCommandsExist(t *testing.T) {
	root := docsRoot()
	check := func(text string) error {
		ms := mentions(text)
		if len(ms) != 1 {
			return fmt.Errorf("%q does not name one backsight command", text)
		}
		return checkMention(root, ms[0], nil)
	}
	// The check bites: the value this test replaced is refused.
	if err := check("backsight config set recall.session_budget_chars <n>"); err == nil {
		t.Fatal("checkMention accepts a command the tree does not have")
	}
	named := 0
	for _, d := range metrics.Catalog {
		for field, text := range map[string]string{"Command": d.Command, "Drill": d.Drill} {
			if text == "" {
				continue
			}
			named++
			if err := check(text); err != nil {
				t.Errorf("%s %s: %v", d.ID, field, err)
			}
		}
	}
	if named < 30 {
		t.Errorf("only %d catalog commands checked", named)
	}
}
