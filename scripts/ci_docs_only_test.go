package scripts

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// docsOnly runs ci-docs-only.sh on the paths given, one per line.
func docsOnly(t *testing.T, input string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script")
	}
	cmd := exec.Command("bash", "ci-docs-only.sh")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ci-docs-only.sh: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// A pull request of documentation alone skips the jobs the docs cannot
// reach (ci.yml changes); anything else, or nothing listed, runs them.
func TestDocsOnlyTellsDocumentationFromCode(t *testing.T) {
	for _, c := range []struct {
		name, paths, want string
	}{
		{"a docs page", "docs/install.md\n", "code=false"},
		{"release notes and the root's Markdown", "docs/releases/v0.1.25-m7.md\nREADME.md\nCONTRIBUTING.md", "code=false"},
		{"CRLF lines", "docs/install.md\r\nREADME.md\r\n", "code=false"},
		{"Go beside docs", "docs/install.md\ninternal/cli/root.go\n", "code=true"},
		{"docs' own Go file", "docs/docs.go\n", "code=true"},
		{"a docs asset", "docs/assets/screen.png\n", "code=true"},
		{"Markdown outside docs", "plugin/skills/doctor/SKILL.md\n", "code=true"},
		{"the workflow", "docs/install.md\n.github/workflows/ci.yml\n", "code=true"},
		{"a root file that is not Markdown", "Makefile\n", "code=true"},
		{"code renamed into docs (both names listed)", "docs/notes.md\ninternal/x/notes.md\n", "code=true"},
		{"nothing listed", "", "code=true"},
		{"blank lines only", "\n\n", "code=true"},
	} {
		if got := docsOnly(t, c.paths); got != c.want {
			t.Errorf("%s: %q gives %q, want %q", c.name, c.paths, got, c.want)
		}
	}
}
