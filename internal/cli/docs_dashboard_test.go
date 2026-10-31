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
		"backsight dashboard link", "never put a session secret",
		"a cookie or a login code on a command line", "no `curl` with them",
		"a dashboard is already running")
}

func TestPrivacyDashboardSection(t *testing.T) {
	requireInSection(t, "../../docs/privacy.md", "The dashboard",
		"dashboard.json", "codes/", "`<hash>.claim`", "claim.lock", "127.0.0.1", "extensions", "localhost", "2 hours",
		"**`--open`** puts the one-time link on the browser's command line",
		"can read every page and any memory's text", "Local development servers",
		"Over `ssh -L`", "`backsight report` never includes `dashboard/`", "--purge-local",
		"reads the memory server you configured")
}

func TestDashboardGuideSections(t *testing.T) {
	requireInSection(t, "../../docs/dashboard.md", "Opening the dashboard",
		"within 2 minutes", "`backsight dashboard link`", "**`--open`** starts your default browser",
		"visible to process listings", "**`--port <n>`**", "**`--json`**", "Ctrl+C", "after 2 hours without a signed-in request",
		"at most 8 browsers", "at most every 30 seconds", "start it again")
	requireInSection(t, "../../docs/dashboard.md", "From another PC",
		"same on both ends", "other users of that PC")
	requireInSection(t, "../../docs/dashboard.md", "What the pages show",
		"`backsight stats overview`", "backsight stats <view> --json", "not an interface for scripts")
	requireInSection(t, "../../docs/dashboard.md", "Privacy",
		"load nothing from other sites", "can read every page", "Local development servers", "privacy.md#the-dashboard")
}

func TestTroubleshootingDashboardErrorPages(t *testing.T) {
	requireInSection(t, "../../docs/troubleshooting.md", "The dashboard shows an error page",
		"`misdirected request` (421)", "`this page opens only from its own links or the address bar` (403)",
		"webmail", "\"This dashboard needs a fresh link\" (401)", "opened `localhost:<port>`",
		"`only GET and HEAD` (405)", "`the memory text shows only in a page you open",
		"backsight dashboard link", "already running", "replaced on its own",
		"process id was reused", "delete `dashboard/dashboard.json` in the state directory (`backsight paths` shows it)")
}
