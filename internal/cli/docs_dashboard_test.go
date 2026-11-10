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

// The local record's memory-docs row says a conversation's document id is
// kept, and that it names the project.
func TestPrivacyLocalRecordSection(t *testing.T) {
	requireInSection(t, "../../docs/privacy.md", "The local record",
		"`memory-docs-`", "`conversation:<session>@<project>#<n>`, which names the session and its project", "never a file's name or path")
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
		"webmail", "\"This link has expired.\" (401)", "opened `localhost:<port>`",
		"`only GET and HEAD` (405)", "`the memory text shows only in a page you open",
		"backsight dashboard link", "already running", "replaced on its own",
		"process id was reused", "delete `dashboard/dashboard.json` in the state directory (`backsight paths` shows it)")
}

// The docs say how to compare two versions and what the comparison is: the
// flags, the groups it matches turns in (the `unknown` path is a turn that no
// main-agent show started), that it only observes, and the new `--has` values.
func TestDocsNameTheComparison(t *testing.T) {
	requireInSection(t, "../../docs/dashboard.md", "What each view answers",
		"backsight stats releases --compare A,B", "--measure", "observational", "unknown",
		"never_extracted", "contradicted")
	raw, err := os.ReadFile("../../docs/commands.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"--compare", "--measure", "used.turn_rate|recall.hit_rate|verdict.wrong_rate", "never_extracted", "contradicted", "`unknown`", "observational"} {
		if !strings.Contains(strings.ReplaceAll(string(raw), `\|`, "|"), w) {
			t.Errorf("docs/commands.md does not name %s", w)
		}
	}
	requireInSection(t, "../../docs/agent-guide.md", "Is memory working?", "stats releases --compare", "observational")
}
