package cli

import (
	"os"
	"strings"
	"testing"
)

// requireInSection fails for each of wants that the "## <heading>" section
// of a docs file (up to the next "## " heading) does not contain.
func requireInSection(t *testing.T, file, heading string, wants ...string) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut("\n"+string(raw), "\n## "+heading+"\n")
	if !ok {
		t.Fatalf("%s has no %q section", file, "## "+heading)
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	for _, w := range wants {
		if !strings.Contains(section, w) {
			t.Errorf("%s, section %q, lacks %q", file, heading, w)
		}
	}
}

func TestAgentGuideDashboardRules(t *testing.T) {
	requireInSection(t, "../../docs/agent-guide.md", "The dashboard",
		"never start the dashboard on your own", "backsight dashboard --open",
		"backsight dashboard link", "never put a session secret")
}

func TestPrivacyDashboardSection(t *testing.T) {
	requireInSection(t, "../../docs/privacy.md", "The dashboard",
		"dashboard.json", "codes/", "127.0.0.1", "extensions", "localhost", "2 hours")
}

func TestTroubleshootingDashboardErrorPages(t *testing.T) {
	requireInSection(t, "../../docs/troubleshooting.md", "The dashboard shows an error page",
		"421", "403", "401", "backsight dashboard link", "already running")
}
