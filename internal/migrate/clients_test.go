package migrate

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
)

func writeConfig(t *testing.T, d paths.Dirs, content string) {
	t.Helper()
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.ConfigFile(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, d paths.Dirs) string {
	t.Helper()
	raw, err := os.ReadFile(d.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func clientsMigration(t *testing.T) Migration {
	t.Helper()
	i := slices.IndexFunc(All(), func(m Migration) bool { return m.ID == ClientsTableID })
	if i < 0 {
		t.Fatal("the clients table migration is not registered")
	}
	return All()[i]
}

// An older config.toml's [client] becomes [clients.claude_code] with the
// same value, comments and layout; the change is a config editor run (a
// backup copy, a journal line) that client config restore undoes; a
// second run changes nothing.
func TestClientsTableMigrationIsJournaledAndUndoable(t *testing.T) {
	d := dirs(t)
	old := "schema_version = 1\n# my retention\n[client]\nretention_days = 3650 # keep\n\n[server]\nbank = \"backsight\"\n"
	writeConfig(t, d, old)
	ran, err := Apply(context.Background(), Env{Dirs: d}, []Migration{clientsMigration(t)})
	if err != nil || !slices.Equal(ran, []string{ClientsTableID}) || !slices.Contains(applied(t, d), ClientsTableID) {
		t.Fatalf("ran %v, %v; applied %v", ran, err, applied(t, d))
	}
	want := "schema_version = 1\n# my retention\n[clients.claude_code]\nretention_days = 3650 # keep\n\n[server]\nbank = \"backsight\"\n"
	if got := readConfig(t, d); got != want {
		t.Fatalf("config:\n%s", got)
	}
	cfg, err := config.Load(d.ConfigFile(), d, "/home/u")
	if err != nil || cfg.Clients.ClaudeCode.RetentionDays != 3650 {
		t.Fatalf("migrated config reads %d, %v", cfg.Clients.ClaudeCode.RetentionDays, err)
	}

	runs, _ := claudecfg.Runs(d.PreImagesDir())
	if len(runs) != 1 {
		t.Fatalf("runs %v", runs)
	}
	m, err := claudecfg.LoadManifest(d.PreImagesDir(), runs[0])
	if err != nil || m.Command != "update migrate" || len(m.Files) != 1 || m.Files[0].Target != d.ConfigFile() || m.Files[0].PreImage == nil {
		t.Fatalf("manifest %+v %v", m, err)
	}
	journal, _ := claudecfg.ReadJournal(d.JournalFile())
	if !slices.ContainsFunc(journal, func(l claudecfg.JournalLine) bool { return l.Run == runs[0] && l.Target == d.ConfigFile() }) {
		t.Errorf("no journal line for the run: %+v", journal)
	}

	// Idempotent: the migrated file needs nothing, no second run.
	if err := clientsTable(context.Background(), Env{Dirs: d}); err != nil {
		t.Fatal(err)
	}
	if again, _ := claudecfg.Runs(d.PreImagesDir()); len(again) != 1 || readConfig(t, d) != want {
		t.Errorf("a second run changed things: runs %v\n%s", again, readConfig(t, d))
	}

	// Undo: the file as it was, byte for byte, and still read the same.
	runner := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "client config restore"}
	if _, _, err := runner.Restore(runs[0], false); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t, d); got != old {
		t.Errorf("restored:\n%s", got)
	}
	if cfg, err := config.Load(d.ConfigFile(), d, "/home/u"); err != nil || cfg.Clients.ClaudeCode.RetentionDays != 3650 {
		t.Errorf("restored config reads %d, %v", cfg.Clients.ClaudeCode.RetentionDays, err)
	}
}

// Nothing to move, or nothing a line edit can move safely: the file stays
// as it is, no run is made, and the migration passes (an update never
// stops on a file the new binary reads anyway).
func TestClientsTableMigrationLeavesOtherFilesAlone(t *testing.T) {
	d := dirs(t)
	if err := clientsTable(context.Background(), Env{Dirs: d}); err != nil {
		t.Fatalf("no config.toml: %v", err)
	}
	if _, err := os.Stat(d.ConfigFile()); !os.IsNotExist(err) {
		t.Error("a config.toml was made")
	}
	for name, content := range map[string]string{
		"fresh install": "schema_version = 1\n[server]\nbank = \"backsight\"\n",
		"migrated":      "[clients.claude_code]\nretention_days = 90\n",
		"dotted key":    "client.retention_days = 90\n",
		"inline table":  "client = { retention_days = 90 }\n",
		"disagreeing":   "[client]\nretention_days = 30\n[clients.claude_code]\nretention_days = 90\n",
		"not a config":  "[client]\nretention_days = 30\n[server\n",
		// Renaming [client] would define clients.claude_code twice: the
		// edited file would not parse, so it is not written.
		"inline new table": "[clients]\nclaude_code = { retention_days = 30 }\n[client]\nretention_days = 30\n",
	} {
		d := dirs(t)
		writeConfig(t, d, content)
		if err := clientsTable(context.Background(), Env{Dirs: d}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := readConfig(t, d); got != content {
			t.Errorf("%s: changed to\n%s", name, got)
		}
		if _, err := os.Stat(d.PreImagesDir()); !os.IsNotExist(err) {
			t.Errorf("%s: a config editor run was made", name)
		}
	}
}
