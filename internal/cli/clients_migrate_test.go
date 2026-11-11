package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/migrate"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

// An existing user's config.toml with [client] (issue 277): the client
// commands read it as before; update migrate (which update apply runs)
// renames the table, recorded once; client config restore puts it back,
// and the commands read that file the same.
func TestOlderClientTableMigratesAndRestores(t *testing.T) {
	home, settings := clientHome(t, userSettings, 3650)
	old := readFile(t, cfgFile(home))
	if code, out, _ := run(t, "client", "config", "plan"); code != output.OK || !strings.Contains(out, "cleanupPeriodDays: 30 → 3650") {
		t.Fatalf("the older table is not read: exit %d\n%s", code, out)
	}

	if code, out, _ := run(t, "update", "migrate", "--json"); code != output.OK || !strings.Contains(out, migrate.ClientsTableID) {
		t.Fatalf("migrate: exit %d\n%s", code, out)
	}
	if got := readFile(t, cfgFile(home)); got != "[clients.claude_code]\nretention_days = 3650\n" {
		t.Fatalf("config after the migration:\n%s", got)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); !slices.Contains(st.AppliedMigrations, migrate.ClientsTableID) {
		t.Errorf("not recorded: %v", st.AppliedMigrations)
	}
	if code, out, _ := run(t, "client", "config", "plan"); code != output.OK || !strings.Contains(out, "cleanupPeriodDays: 30 → 3650") {
		t.Fatalf("the migrated table is not read: exit %d\n%s", code, out)
	}

	// The migration is the last config editor run: restore undoes it.
	if code, out, _ := run(t, "client", "config", "restore"); code != output.OK {
		t.Fatalf("restore: exit %d\n%s", code, out)
	}
	if got := readFile(t, cfgFile(home)); got != old {
		t.Errorf("restored config:\n%s", got)
	}
	if readFile(t, settings) != userSettings {
		t.Error("Claude Code's settings were touched")
	}
	if code, out, _ := run(t, "client", "config", "plan"); code != output.OK || !strings.Contains(out, "cleanupPeriodDays: 30 → 3650") {
		t.Errorf("the restored table is not read: exit %d\n%s", code, out)
	}
}

// Invalid values are refused under the new name as under the old one,
// each naming the key as the file writes it.
func TestClientTableRefusalsNameTheirKey(t *testing.T) {
	for body, key := range map[string]string{
		"[clients.claude_code]\nretention_days = -1\n": "clients.claude_code.retention_days",
		"[client]\nretention_days = -1\n":              "client.retention_days",
	} {
		home, settings := clientHome(t, userSettings, 0)
		if err := os.WriteFile(cfgFile(home), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, errOut := run(t, "client", "config", "apply")
		if code != output.Usage || !strings.Contains(errOut, key+": must be ≥ 0") || readFile(t, settings) != userSettings {
			t.Errorf("%q: exit %d\n%s", body, code, errOut)
		}
	}
}
