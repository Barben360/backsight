package migrate

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
)

func headlessMigration(t *testing.T) Migration {
	t.Helper()
	i := slices.IndexFunc(All(), func(m Migration) bool { return m.ID == HeadlessSkipID })
	if i < 0 {
		t.Fatal("the headless skip migration is not registered")
	}
	if i == 0 || All()[i-1].ID != ClientsTableID {
		t.Fatalf("the headless skip migration must follow %s", ClientsTableID)
	}
	return All()[i]
}

// An install whose config.toml carries the former default, as setup
// wrote it (the whole Appendix A.1 file), gets today's: one line changed,
// as a config editor run (a backup copy, a journal line) that client
// config restore undoes byte for byte; a second run changes nothing.
func TestHeadlessSkipMigrationIsJournaledAndUndoable(t *testing.T) {
	d := dirs(t)
	golden, err := os.ReadFile("../config/testdata/appendix-a1.toml")
	if err != nil {
		t.Fatal(err)
	}
	today := `skip_entrypoints = ["sdk-*", "codex_exec", "codex_sdk_*", "codex-app-server-sdk", "codex_mcp_server"]`
	if !strings.Contains(string(golden), today) {
		t.Fatal("the golden no longer holds today's default: update this test")
	}
	// The golden's two paths, as this platform writes them (setup writes
	// absolute ones; /d/server is not one on Windows).
	quote := func(p string) string { return strconv.Quote(p) }
	golden = []byte(strings.NewReplacer(`"/d/server"`, quote(filepath.Join(d.Data, "server")), `"/d/backups"`, quote(filepath.Join(d.Data, "backups"))).Replace(string(golden)))
	old := strings.Replace(string(golden), today, `skip_entrypoints = ["sdk-*"] # as setup wrote it`, 1)
	writeConfig(t, d, old)
	ran, err := Apply(context.Background(), Env{Dirs: d}, []Migration{headlessMigration(t)})
	if err != nil || !slices.Equal(ran, []string{HeadlessSkipID}) || !slices.Contains(applied(t, d), HeadlessSkipID) {
		t.Fatalf("ran %v, %v; applied %v", ran, err, applied(t, d))
	}
	want := strings.Replace(old, `skip_entrypoints = ["sdk-*"] # as setup wrote it`, today+" # as setup wrote it", 1)
	if got := readConfig(t, d); got != want {
		t.Fatalf("config:\n%s", got)
	}
	// read as config.Load does (the golden's POSIX paths aside)
	var cfg config.Config
	if err := toml.Unmarshal([]byte(readConfig(t, d)), &cfg); err != nil || !cfg.Privacy.SkipsEntrypoint("codex_exec") || !cfg.Privacy.SkipsEntrypoint("sdk-cli") || cfg.Privacy.SkipsEntrypoint("codex-tui") {
		t.Fatalf("migrated config skips %v, %v", cfg.Privacy.SkipEntrypoints, err)
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

	// Idempotent: today's value needs nothing, no second run.
	if err := headlessSkip(context.Background(), Env{Dirs: d}); err != nil {
		t.Fatal(err)
	}
	if again, _ := claudecfg.Runs(d.PreImagesDir()); len(again) != 1 || readConfig(t, d) != want {
		t.Errorf("a second run changed things: runs %v\n%s", again, readConfig(t, d))
	}

	// Undo: the file as it was, byte for byte.
	runner := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "client config restore"}
	if _, _, err := runner.Restore(runs[0], false); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t, d); got != old {
		t.Errorf("restored:\n%s", got)
	}
}

// Any value but the former default is the user's own, and a file that
// does not set the key reads today's default already: the file stays as
// it is, no run is made, and the migration passes.
func TestHeadlessSkipMigrationLeavesOtherFilesAlone(t *testing.T) {
	d := dirs(t)
	if err := headlessSkip(context.Background(), Env{Dirs: d}); err != nil {
		t.Fatalf("no config.toml: %v", err)
	}
	if _, err := os.Stat(d.ConfigFile()); !os.IsNotExist(err) {
		t.Error("a config.toml was made")
	}
	for name, content := range map[string]string{
		"customised":   "[privacy]\nskip_entrypoints = [\"sdk-cli\", \"sdk-ts\"]\n",
		"sends all":    "[privacy]\nskip_entrypoints = []\n",
		"already":      "[privacy]\nskip_entrypoints = [\"sdk-*\", \"codex_exec\", \"codex_sdk_*\", \"codex-app-server-sdk\", \"codex_mcp_server\"]\n",
		"absent key":   "[privacy]\nexclude_paths = [\"/w/secret\"]\n",
		"no privacy":   "schema_version = 1\n[server]\nbank = \"backsight\"\n",
		"dotted key":   "privacy.skip_entrypoints = [\"sdk-*\"]\n",
		"not a config": "[privacy]\nskip_entrypoints = [\"sdk-*\"]\n[server\n",
	} {
		d := dirs(t)
		writeConfig(t, d, content)
		if err := headlessSkip(context.Background(), Env{Dirs: d}); err != nil {
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
