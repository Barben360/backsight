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

// The dashboard guide describes each page's newer cards under their titles,
// the custom window, the density, the shortcuts and the limits a reader hits.
func TestDashboardGuideNewCards(t *testing.T) {
	requireInSection(t, "../../docs/dashboard.md", "What the newer cards show",
		"Which memories helped most?", "Which days did memory help?", "Colour by", "Copy summary", "Summary as text",
		"no memory text, no session or memory id", "a remote server",
		"Did anything change that day?", "How long until a session's memories come back?",
		"Sent, never turned into memories", "Have the health checks passed lately?", "Could memory hold a secret?",
		"Do new memories turn out useful?", "Where did it come from?", "Do two versions differ?", "When did Claude use memory?",
		"never says one is better")
	requireInSection(t, "../../docs/dashboard.md", "Keyboard shortcuts",
		"`g` `o`", "Filter by project", "Refresh this page", "Ctrl, Alt or Meta", "Off by default")
	requireInSection(t, "../../docs/dashboard.md", "What the pages show",
		"Custom window", "Both days are included", "13 months")
	requireInSection(t, "../../docs/dashboard.md", "Preferences", "Density", "Compact", "Keyboard shortcuts")
	requireInSection(t, "../../docs/dashboard.md", "Keyboard and screen readers",
		"over 200 days", "the latest 120 days")
	requireInSection(t, "../../docs/dashboard.md", "Reading the numbers",
		"Memory yield needs four weeks", "Time to recall needs a few days of history", "not complete until")
}

func TestPrivacyCopySummary(t *testing.T) {
	requireInSection(t, "../../docs/privacy.md", "The dashboard",
		"**Copy summary**", "no memory text and no session or memory id", "(`127.0.0.1` or `localhost`)", "a remote server")
}

// The command reference names what the new numbers add to each view.
func TestCommandsNameTheNewNumbers(t *testing.T) {
	raw, err := os.ReadFile("../../docs/commands.md")
	if err != nil {
		t.Fatal(err)
	}
	for view, want := range map[string]string{
		"overview": "helped most", "cost": "extraction tokens per useful memory", "bank": "share of the memory that came up",
		"pipeline": "never turned into memories", "system": "per day", "days": "calendar of the last 13 months",
		"releases": "version switch and settings change", "memories": "where it came from",
	} {
		found := false
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "| `backsight stats "+view) {
				found = true
				if !strings.Contains(line, want) {
					t.Errorf("docs/commands.md, the stats %s row, lacks %q", view, want)
				}
			}
		}
		if !found {
			t.Errorf("docs/commands.md has no stats %s row", view)
		}
	}
}

// The notes of the release that put the new cards on the pages say what is
// new and the limits in a reader's words, and point back to the release that
// began recording the numbers.
func TestReleaseNotesOfThePages(t *testing.T) {
	const notes = "../../docs/releases/v0.1.29-m7.md"
	requireInSection(t, notes, "What changed",
		"Which memories helped most?", "Which days did memory help?", "Copy summary", "Do two versions differ?",
		"never which is better", "Custom window", "Density", "Keyboard shortcuts", "v0.1.28-m7.md")
	requireInSection(t, notes, "Known limits",
		"on a day's page and on Releases only", "13 months", "Time to recall needs a few days of history",
		"Memory yield needs four weeks", "over 200 days", "latest 120 days", "not complete shows no count of new memories")
}
