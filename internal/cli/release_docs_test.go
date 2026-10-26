package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
)

// The release documents state what the binary pins and trusts (decision
// 0067 block 3): docs/compatibility.md's current section and SECURITY.md's
// key table must say what this binary reports, so a pin bump or a key
// rotation that forgets them fails here.

const (
	compatibilityDoc = "../../docs/compatibility.md"
	securityDoc      = "../../SECURITY.md"
)

var codeSpan = regexp.MustCompile("`([^`]+)`")

// docValue returns the first `code` span of the one table row whose first
// cell is label, in file's "## <heading>" section (up to the next "## "
// heading, so a release history elsewhere is never read).
func docValue(file, heading, label string) (string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	_, section, ok := strings.Cut("\n"+string(raw), "\n## "+heading+"\n")
	if !ok {
		return "", fmt.Errorf("%s has no %q section", file, "## "+heading)
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	var found []string
	for _, line := range strings.Split(section, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 3 || strings.TrimSpace(cells[1]) != label {
			continue
		}
		m := codeSpan.FindStringSubmatch(strings.Join(cells[2:], "|"))
		if m == nil {
			return "", fmt.Errorf("%s: the %q row has no `value`: %s", file, label, line)
		}
		found = append(found, m[1])
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%s: want one %q row under %q, found %d %q", file, label, heading, len(found), found)
	}
	return found[0], nil
}

func mustDocValue(t *testing.T, file, heading, label string) string {
	t.Helper()
	v, err := docValue(file, heading, label)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func versionReport(t *testing.T) versionInfo {
	t.Helper()
	code, out, _ := run(t, "version", "--json")
	var v struct {
		Data versionInfo `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != output.OK {
		t.Fatalf("version --json: exit %d, %v\n%s", code, err, out)
	}
	return v.Data
}

func TestCompatibilityNamesTheBinarysHindsightPin(t *testing.T) {
	pin := versionReport(t).HindsightImage
	if !regexp.MustCompile(`^[^@]+:[^@/]+@sha256:[0-9a-f]{64}$`).MatchString(pin) {
		t.Fatalf("version --json pins %q: want a tag and a digest", pin)
	}
	if got := mustDocValue(t, compatibilityDoc, "Current", "Hindsight server"); got != pin {
		t.Errorf("docs/compatibility.md pins %q, the binary pins %q (version --json)", got, pin)
	}
}

func TestCompatibilityNamesThePostgresPin(t *testing.T) {
	if got := mustDocValue(t, compatibilityDoc, "Current", "PostgreSQL"); got != server.PostgresImage {
		t.Errorf("docs/compatibility.md pins %q, the binary renders %q", got, server.PostgresImage)
	}
}

func TestCompatibilityNamesTheMinimumClaudeCode(t *testing.T) {
	if got := mustDocValue(t, compatibilityDoc, "Current", "Claude Code"); got != minClaude {
		t.Errorf("docs/compatibility.md says Claude Code %q, client install and setup refuse below %q", got, minClaude)
	}
}

func TestSecurityListsTheTrustedReleaseKeys(t *testing.T) {
	want := map[string]string{}
	for _, k := range versionReport(t).SigningKeys {
		if k.Role != "test" {
			want[k.Role] = k.ID
		}
	}
	if len(want) != 2 || want["current"] == "" || want["next"] == "" {
		t.Fatalf("version --json lists %v: want a current and a next key", want)
	}
	for role, id := range want {
		if got := mustDocValue(t, securityDoc, "Release signing", role); got != id {
			t.Errorf("SECURITY.md lists %q as the %s key, the binary trusts %q", got, role, id)
		}
	}
}

// The reader takes the current section's row only, and refuses a document
// it cannot read one value from.
func TestDocValueReadsOneRowOfItsSection(t *testing.T) {
	const pin, old = "img:1.1@sha256:new", "img:1.0@sha256:old"
	cases := []struct {
		name, doc, want, err string
	}{
		{"current row, history ignored", "# C\n\n## Current\n\n| Hindsight server | `" + pin + "` | x |\n\n## Releases\n\n| Hindsight server | `" + old + "` |\n", pin, ""},
		{"first code span of the row", "## Current\n| Hindsight server | `" + pin + "` or `" + old + "` |\n", pin, ""},
		{"no section", "# C\n\n| Hindsight server | `" + pin + "` |\n", "", `no "## Current" section`},
		{"only in another section", "## Current\n\n## Releases\n| Hindsight server | `" + old + "` |\n", "", "found 0"},
		{"subheading stays in the section", "## Current\n### Detail\n| Hindsight server | `" + pin + "` |\n", pin, ""},
		{"two rows", "## Current\n| Hindsight server | `" + pin + "` |\n| Hindsight server | `" + old + "` |\n", "", "found 2"},
		{"row without a value", "## Current\n| Hindsight server | 1.1.0 |\n", "", "has no `value`"},
		{"label only in prose", "## Current\nHindsight server `" + pin + "`\n", "", "found 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "doc.md")
			if err := os.WriteFile(f, []byte(c.doc), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := docValue(f, "Current", "Hindsight server")
			switch {
			case c.err == "" && (err != nil || got != c.want):
				t.Errorf("got %q, %v; want %q", got, err, c.want)
			case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
				t.Errorf("got %q, %v; want an error containing %q", got, err, c.err)
			}
		})
	}
	if _, err := docValue(filepath.Join(t.TempDir(), "missing.md"), "Current", "Hindsight server"); err == nil {
		t.Error("a missing document read as a value")
	}
}

// releaseRows lists the first cell of each row of docs/compatibility.md's
// "## Releases" table.
func releaseRows(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(compatibilityDoc)
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(raw), "\n## Releases\n")
	if !ok {
		t.Fatal("docs/compatibility.md has no ## Releases section")
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	var rows []string
	for _, line := range strings.Split(section, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 3 || !strings.HasPrefix(line, "|") {
			continue
		}
		if c := strings.TrimSpace(cells[1]); c != "Release" && !strings.HasPrefix(c, "---") {
			rows = append(rows, c)
		}
	}
	return rows
}

// Each release has its hand-written notes (docs/releases/<tag>.md, what
// the release workflow publishes) and its row in docs/compatibility.md,
// and nothing else is listed: no row for a version never released here.
func TestEveryReleaseHasItsNotesAndItsRow(t *testing.T) {
	tag := regexp.MustCompile(`^v\d+\.\d+\.\d+(?:-[0-9a-z.]+)?$`)
	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "releases", "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no release notes in docs/releases: %v", err)
	}
	notes := map[string]bool{}
	for _, f := range files {
		name, isMD := strings.CutSuffix(filepath.Base(f), ".md")
		if !isMD || !tag.MatchString(name) {
			t.Errorf("docs/releases/%s: want <tag>.md, a tag like v1.2.3 or v1.2.3-rc.1", filepath.Base(f))
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil || len(strings.TrimSpace(string(raw))) < 200 {
			t.Errorf("docs/releases/%s is empty or a stub (%v)", filepath.Base(f), err)
		}
		notes[name] = true
	}
	rows := releaseRows(t)
	if len(rows) == 0 {
		t.Fatal("docs/compatibility.md lists no release")
	}
	for _, r := range rows {
		if !notes[r] {
			t.Errorf("docs/compatibility.md lists %s, which has no docs/releases/%s.md", r, r)
		}
		delete(notes, r)
	}
	for n := range notes {
		t.Errorf("docs/releases/%s.md has no row in docs/compatibility.md's Releases table", n)
	}
}
