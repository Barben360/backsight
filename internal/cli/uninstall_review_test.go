package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	agelib "filippo.io/age"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// otherProfile is a Claude Code profile backsight did not wire, holding
// the memory tools' rule as a hand-made install would; the shell points at
// it. Its settings must come out of any uninstall byte for byte.
func otherProfile(t *testing.T, home, name string) (dir, settings, content string) {
	t.Helper()
	dir = filepath.Join(home, name)
	settings = filepath.Join(dir, "settings.json")
	content = "{\n  \"permissions\": {\n    \"allow\": [\n      \"" + plugin.ToolNames[0] + "\"\n    ]\n  }\n}\n"
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(settings, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, settings, content
}

// A shell set to another profile never reaches a claude call meant for
// ~/.claude (block 3 review: it removed that profile's plugin).
func TestUninstallOnTheDefaultProfileIgnoresTheShellProfile(t *testing.T) {
	m := installMachine(t)
	other, settings, content := otherProfile(t, m.home, "work-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", other)
	if code, _, out := uninstall(t); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, cmd := range []string{"plugin list", "plugin uninstall", "plugin marketplace remove"} {
		got := m.claude.profilesOf(cmd)
		if len(got) == 0 {
			t.Errorf("no %s call", cmd)
		}
		for _, p := range got {
			if p != "" {
				t.Errorf("%s ran on CLAUDE_CONFIG_DIR=%s, not ~/.claude", cmd, p)
			}
		}
	}
	if readFile(t, settings) != content {
		t.Errorf("the unwired profile's settings changed:\n%s", readFile(t, settings))
	}
}

// Every profile setup wired is cleaned, each through its own
// CLAUDE_CONFIG_DIR; a third one the shell points at stays as it was.
func TestUninstallCleansEveryWiredProfile(t *testing.T) {
	m := installMachine(t)
	d := pathsOf(m.home)
	wired, wiredSettings, _ := otherProfile(t, m.home, "dev-profile")
	third, thirdSettings, thirdContent := otherProfile(t, m.home, "third-profile")
	rec := &setup.Record{SchemaVersion: setup.RecordVersion}
	rec.Undo.ClientProfiles = []string{filepath.Join(m.home, ".claude"), wired}
	if err := rec.Save(d.SetupFile()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", third)
	if code, _, out := uninstall(t); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := m.claude.profilesOf("plugin uninstall"); !reflect.DeepEqual(got, []string{"", wired}) {
		t.Errorf("plugin uninstall ran on %q, want ~/.claude then %s", got, wired)
	}
	if s := readFile(t, wiredSettings); strings.Contains(s, plugin.ToolNames[0]) {
		t.Errorf("the wired profile keeps the rule:\n%s", s)
	}
	if readFile(t, thirdSettings) != thirdContent {
		t.Errorf("the unwired profile's settings changed:\n%s", readFile(t, thirdSettings))
	}
	if rec, _ := setup.Load(d.SetupFile()); rec == nil || len(rec.Undo.ClientProfiles) != 0 {
		t.Errorf("setup.json still lists profiles: %+v", rec)
	}
}

func TestSetupUndoOnTheDefaultProfileIgnoresTheShellProfile(t *testing.T) {
	m := installMachine(t)
	rec := &setup.Record{SchemaVersion: setup.RecordVersion}
	rec.Undo.ClientProfiles = []string{filepath.Join(m.home, ".claude")}
	if err := rec.Save(pathsOf(m.home).SetupFile()); err != nil {
		t.Fatal(err)
	}
	other, _, _ := otherProfile(t, m.home, "work-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", other)
	if code, out, errOut := run(t, "setup", "--undo", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := m.claude.profilesOf("plugin uninstall"); !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("plugin uninstall ran on %q, want ~/.claude only", got)
	}
}

// client install for ~/.claude named on the command line, from a shell set
// to another profile: claude acts on ~/.claude, without CLAUDE_CONFIG_DIR
// (set to ~/.claude it would move .claude.json into it).
func TestClientInstallOnTheDefaultProfileIgnoresTheShellProfile(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	other, _, _ := otherProfile(t, home, "work-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", other)
	if code, out, errOut := run(t, "client", "install", "--config-dir", filepath.Join(home, ".claude")); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if len(f.ran) == 0 {
		t.Fatal("no claude call")
	}
	for _, c := range f.ran {
		if c.configDir != "" {
			t.Errorf("%s ran on CLAUDE_CONFIG_DIR=%s", c.line, c.configDir)
		}
	}
	// The shell's profile, with no --config-dir, is still the one wired.
	f.ran = nil
	if code, out, errOut := run(t, "client", "install"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := f.profilesOf("plugin install"); !reflect.DeepEqual(got, []string{other}) {
		t.Errorf("plugin install ran on %q, want %s", got, other)
	}
}

// setup's plugin check, for ~/.claude named on the command line.
func TestSetupPluginCheckIgnoresTheShellProfile(t *testing.T) {
	home, _ := setupMachine(t)
	withLLMKey(t, home)
	f := claudeRunner.(*fakeClaude)
	other, _, _ := otherProfile(t, home, "work-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", other)
	code, out, errOut := run(t, "setup", "--config-dir", filepath.Join(home, ".claude"), "--answer", "provider=deepseek", "--answer", "consent=yes",
		"--answer", "backups=off", "--non-interactive", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	got := f.profilesOf("plugin list")
	if len(got) == 0 {
		t.Fatal("setup never asked claude for the plugins")
	}
	for _, p := range got {
		if p != "" {
			t.Errorf("plugin list ran on CLAUDE_CONFIG_DIR=%s, not ~/.claude", p)
		}
	}
}

// A failed step stops before the binaries; the resume keeps every flag,
// and a rerun finishes the job.
func TestUninstallResumeKeepsEveryFlag(t *testing.T) {
	t.Run("rerun finishes", func(t *testing.T) {
		m := installMachine(t)
		d := pathsOf(m.home)
		answers := filepath.Join(m.home, "answers.txt")
		_ = os.WriteFile(answers, []byte("retention=keep\n"), 0o600)
		m.claude.failOn = "plugin uninstall"
		code, env, out := uninstall(t, "--stop", "--answers-file", answers, "--non-interactive")
		if code != output.Partial || env.Error == nil {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if want := "backsight uninstall --stop --non-interactive --answers-file " + answers; env.Error.Resume != want {
			t.Errorf("resume %q, want %q", env.Error.Resume, want)
		}
		if !onDisk(filepath.Join(d.Data, "bin", "current")) || len(m.dk.calls) > 0 {
			t.Errorf("went on after the failure: bin %v, compose %q", onDisk(filepath.Join(d.Data, "bin", "current")), m.dk.subcommands())
		}
		m.claude.failOn, m.claude.calls = "", nil
		code, env, out = uninstall(t, strings.Fields(strings.TrimPrefix(env.Error.Resume, "backsight uninstall"))...)
		if code != output.OK {
			t.Fatalf("rerun: exit %d\n%s", code, out)
		}
		if got := mutating(m.claude); !slices.Contains(got, "plugin uninstall "+plugin.PluginID()+" --scope user") {
			t.Errorf("the rerun did not remove the plugin: %q", got)
		}
		if s := stepNamed(env.Data, "scheduler"); s == nil || s.Status != "skipped" {
			t.Errorf("scheduler on the rerun: %+v", s)
		}
		if onDisk(filepath.Join(d.Data, "bin", "current")) {
			t.Error("the rerun left the binaries")
		}
		if got := m.dk.subcommands(); !reflect.DeepEqual(got, []string{"stop"}) {
			t.Errorf("compose: %q", got)
		}
	})
	t.Run("answers kept", func(t *testing.T) {
		m := installMachine(t)
		m.claude.failOn = "plugin uninstall"
		code, env, out := uninstall(t, "--delete-data", "--answer", "confirm_bank=claude-memory")
		if code != output.Partial || env.Error == nil || env.Error.Resume != "backsight uninstall --delete-data --answer confirm_bank=claude-memory" {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if len(m.dk.calls) > 0 {
			t.Errorf("the server went before the plugin: %q", m.dk.subcommands())
		}
	})
}

// The local files go before the binaries; a failure on the binaries is no
// longer resumable (the state may be gone): exit 20 with what is left.
func TestUninstallBinariesLastAndTheirFailureListsWhatIsLeft(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes read-only directories' entries")
	}
	m := installMachine(t)
	d := pathsOf(m.home)
	bin := filepath.Join(d.Data, "bin")
	if err := os.Chmod(bin, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bin, 0o700) })
	code, env, out := uninstall(t, "--purge-local")
	if code != output.Blocked || env.Error == nil || env.Error.Resume != "" || !strings.Contains(env.Error.Message, bin) ||
		!strings.Contains(env.Error.Message, "by hand") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if onDisk(d.ConfigFile()) || onDisk(d.StateFile()) {
		t.Error("the local files were not removed before the binaries")
	}
	if s := stepNamed(env.Data, "binaries"); s == nil || s.Status != "failed" {
		t.Errorf("binaries step: %+v", s)
	}
}

func clientRetireRun(t *testing.T, m installed) {
	t.Helper()
	d := pathsOf(m.home)
	ts, _, err := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "client retire", Now: now}.Apply([]claudecfg.Edit{{
		Target: m.settings, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
			out, ch, err := claudecfg.Set(doc, []string{"retiredByTest"}, true)
			return out, []claudecfg.Change{ch}, err
		}}})
	if err != nil {
		t.Fatal(err)
	}
	rec := &setup.Record{SchemaVersion: setup.RecordVersion}
	rec.Undo.ConfigRuns = []string{ts}
	if err := rec.Save(d.SetupFile()); err != nil {
		t.Fatal(err)
	}
}

func queueNote(t *testing.T, home string) {
	t.Helper()
	d := pathsOf(home)
	raw, _ := json.Marshal(retain.NewNote("claude-memory", "decision", "p", "a queued note", time.Now()))
	_ = os.MkdirAll(d.OutboxDir(), 0o700)
	if err := os.WriteFile(filepath.Join(d.OutboxDir(), "notes.jsonl"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func questionIDs(env uninstallEnvelope) []string {
	var ids []string
	if env.Error != nil {
		for _, q := range env.Error.Questions {
			ids = append(ids, q.ID)
		}
	}
	return ids
}

// --purge-local asks every question in one go, before any change.
func TestUninstallPurgeLocalAsksFirst(t *testing.T) {
	m := installMachine(t)
	d := pathsOf(m.home)
	id, _ := agelib.GenerateX25519Identity()
	_ = config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("backup_identity", id.String()) })
	// A recipient set since: older archives still open only with the identity.
	configure(t, m.home, "[backup]\nrecipient = "+strconvQuote(id.Recipient().String()))
	clientRetireRun(t, m)
	queueNote(t, m.home)

	before := treeOf(t, m.home)
	code, env, out := uninstall(t, "--purge-local")
	if code != output.NeedsUser || !reflect.DeepEqual(questionIDs(env), []string{"identity_saved", "pre_images_ack", "drop_queued"}) {
		t.Fatalf("exit %d, questions %q\n%s", code, questionIDs(env), out)
	}
	if !strings.Contains(env.Error.Questions[1].Prompt, "setup --undo") || !strings.Contains(env.Error.Questions[2].Prompt, "1 note(s) queued") {
		t.Errorf("prompts: %+v", env.Error.Questions)
	}
	var cmds []string
	for _, a := range env.Error.Actions {
		cmds = append(cmds, a.Cmd)
	}
	if !slices.Contains(cmds, "backsight backup identity show") || !slices.Contains(cmds, "backsight setup --undo") {
		t.Errorf("actions: %q", cmds)
	}
	sameTree(t, before, treeOf(t, m.home))
	if len(mutating(m.claude)) > 0 || len(m.sys.calls) > 0 {
		t.Errorf("acted before the answers: %q %q", mutating(m.claude), m.sys.calls)
	}

	// One answer short: still nothing changed, only that question left.
	code, env, out = uninstall(t, "--purge-local", "--answer", "identity_saved=yes", "--answer", "drop_queued=yes")
	if code != output.NeedsUser || !reflect.DeepEqual(questionIDs(env), []string{"pre_images_ack"}) {
		t.Fatalf("exit %d, questions %q\n%s", code, questionIDs(env), out)
	}
	sameTree(t, before, treeOf(t, m.home))

	if code, _, out := uninstall(t, "--purge-local", "--answer", "identity_saved=yes", "--answer", "drop_queued=yes", "--answer", "pre_images_ack=yes"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, p := range []string{d.SecretsFile(), d.PreImagesDir(), d.OutboxDir()} {
		if onDisk(p) {
			t.Errorf("not purged: %s", p)
		}
	}
}

// Nothing to lose, nothing asked: no identity, no retire, no queue.
func TestUninstallPurgeLocalAsksNothingWhenNothingIsLost(t *testing.T) {
	m := installMachine(t)
	rec := &setup.Record{SchemaVersion: setup.RecordVersion}
	rec.Undo.Timers = []string{"memory-retro.timer"} // stopped timers have no pre-image
	_ = rec.Save(pathsOf(m.home).SetupFile())
	if code, _, out := uninstall(t, "--purge-local"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestUninstallPlanListsTheQueuedWrites(t *testing.T) {
	m := installMachine(t)
	d := pathsOf(m.home)
	queueNote(t, m.home)
	if err := retain.SaveCursor(d.CursorsDir(), "s1", retain.Cursor{Quarantined: true}); err != nil {
		t.Fatal(err)
	}
	code, env, out := uninstall(t, "--plan", "--purge-local")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if w := strings.Join(env.Data.Warnings, "\n"); !strings.Contains(w, "1 note(s) queued") || !strings.Contains(w, "1 session(s)") {
		t.Errorf("the plan does not list the queued writes: %q", env.Data.Warnings)
	}
	// Without --purge-local they are kept: nothing to say.
	if _, env, _ := uninstall(t, "--plan"); strings.Contains(strings.Join(env.Data.Warnings, "\n"), "queued") {
		t.Errorf("warns about writes it keeps: %q", env.Data.Warnings)
	}
}

// State entries backsight did not write stay, and are named.
func TestUninstallPurgeLocalKeepsForeignStateEntries(t *testing.T) {
	m := installMachine(t)
	d := pathsOf(m.home)
	foreign := filepath.Join(d.State, "cutover-backup-20261025", "settings.json")
	_ = os.MkdirAll(filepath.Dir(foreign), 0o700)
	_ = os.WriteFile(foreign, []byte("mine"), 0o600)
	ledger := filepath.Join(d.AttributionDir(), "s1", "main.jsonl")
	_ = os.MkdirAll(filepath.Dir(ledger), 0o700)
	_ = os.WriteFile(ledger, []byte("{}\n"), 0o600)
	for _, n := range []string{"reattribute.jsonl", "reattribute.json", "noise-history.jsonl"} {
		_ = os.WriteFile(filepath.Join(d.State, n), []byte("{}\n"), 0o600)
	}
	_ = os.WriteFile(d.FamiliesFile(), []byte("{}"), 0o600)
	code, env, out := uninstall(t, "--purge-local")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if readFile(t, foreign) != "mine" {
		t.Error("a file backsight did not write was deleted")
	}
	if !strings.Contains(strings.Join(env.Data.Kept, "\n"), filepath.Dir(foreign)) {
		t.Errorf("the kept entry is not named: %q", env.Data.Kept)
	}
	for _, p := range []string{d.StateFile(), d.LocksDir(), d.PreImagesDir(), filepath.Join(d.State, "telemetry"), d.ConfigFile(), d.AttributionDir(), filepath.Join(d.State, "reattribute.jsonl"), filepath.Join(d.State, "reattribute.json"), filepath.Join(d.State, "noise-history.jsonl"), d.FamiliesFile()} {
		if onDisk(p) {
			t.Errorf("not purged: %s", p)
		}
	}
}

// A backup directory reached through a symlink into the state directory
// is kept.
func TestUninstallPurgeLocalFollowsASymlinkedBackupDir(t *testing.T) {
	m := installMachine(t)
	d := pathsOf(m.home)
	real := filepath.Join(d.State, "logs", "archives")
	archive := filepath.Join(real, "backsight-1.tar.gz.age")
	_ = os.MkdirAll(real, 0o700)
	_ = os.WriteFile(archive, []byte("archive"), 0o600)
	link := filepath.Join(m.home, "my-backups")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	configure(t, m.home, "[backup]\ndir = "+strconvQuote(link))
	if code, _, out := uninstall(t, "--purge-local"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !onDisk(archive) {
		t.Error("the backup behind the symlink was deleted")
	}
}

// uninstall waits for a running job, and refuses when it does not finish.
func TestUninstallWaitsForTheJobs(t *testing.T) {
	short := func(t *testing.T, d time.Duration) {
		old := jobLockWait
		jobLockWait = d
		t.Cleanup(func() { jobLockWait = old })
	}
	t.Run("still running", func(t *testing.T) {
		m := installMachine(t)
		short(t, 200*time.Millisecond)
		l, err := state.TryAcquire(filepath.Join(pathsOf(m.home).LocksDir(), "job-maintain.lock"), "jobs maintain")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Release() })
		before := treeOf(t, m.home)
		code, env, out := uninstall(t)
		if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "jobs maintain") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		sameTree(t, before, treeOf(t, m.home))
		if len(mutating(m.claude)) > 0 || len(m.sys.calls) > 0 {
			t.Errorf("acted while a job ran: %q %q", mutating(m.claude), m.sys.calls)
		}
	})
	t.Run("finishes during the wait", func(t *testing.T) {
		m := installMachine(t)
		short(t, 5*time.Second)
		l, err := state.TryAcquire(filepath.Join(pathsOf(m.home).LocksDir(), "job-tick.lock"), "jobs tick")
		if err != nil {
			t.Fatal(err)
		}
		go func() { time.Sleep(300 * time.Millisecond); _ = l.Release() }()
		if code, _, out := uninstall(t); code != output.OK {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
}

// The timers are disabled, then the services stopped, and their files go.
func TestUninstallStopsTheJobServices(t *testing.T) {
	m := installMachine(t)
	unitDir := filepath.Join(m.home, "xdg", "systemd", "user")
	if code, _, out := uninstall(t); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	disable := slices.Index(m.sys.calls, "systemctl --user disable --now backsight-tick.timer backsight-maintain.timer")
	stop := slices.Index(m.sys.calls, "systemctl --user stop backsight-tick.service backsight-maintain.service")
	if disable < 0 || stop < disable {
		t.Errorf("systemctl calls %q: want the timers disabled, then the services stopped", m.sys.calls)
	}
	for _, f := range []string{"backsight-tick.service", "backsight-maintain.service", "backsight-tick.timer", "backsight-maintain.timer"} {
		if onDisk(filepath.Join(unitDir, f)) {
			t.Errorf("%s left", f)
		}
	}
}

func TestUninstallRefusesWhileALockIsHeld(t *testing.T) {
	t.Run("setup", func(t *testing.T) {
		m := installMachine(t)
		l, err := state.TryAcquire(filepath.Join(pathsOf(m.home).LocksDir(), "setup.lock"), "setup")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Release() })
		before := treeOf(t, m.home)
		if code, _, out := uninstall(t); code != output.Blocked || !strings.Contains(out, "another setup is running") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		sameTree(t, before, treeOf(t, m.home))
	})
	for _, args := range [][]string{{"--stop"}, {"--delete-data", "--answer", "confirm_bank=claude-memory"}} {
		t.Run("server "+args[0], func(t *testing.T) {
			m := installMachine(t)
			l, err := state.TryAcquire(filepath.Join(pathsOf(m.home).LocksDir(), "server.lock"), "server upgrade")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Release() })
			before := treeOf(t, m.home)
			if code, _, out := uninstall(t, args...); code != output.Blocked || !strings.Contains(out, "server upgrade") {
				t.Fatalf("exit %d\n%s", code, out)
			}
			sameTree(t, before, treeOf(t, m.home))
			if len(mutating(m.claude)) > 0 || len(m.sys.calls) > 0 || len(m.dk.calls) > 0 {
				t.Errorf("acted: %q %q %q", mutating(m.claude), m.sys.calls, m.dk.subcommands())
			}
		})
	}
}

func TestUninstallRefusesAnUnknownAnswer(t *testing.T) {
	m := installMachine(t)
	before := treeOf(t, m.home)
	code, _, out := uninstall(t, "--answer", "confirm=yes")
	if code != output.Usage || !strings.Contains(out, "drop_queued") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, m.home))
}

func TestUninstallPlanWithTheRetentionBack(t *testing.T) {
	m := installMachine(t)
	before := treeOf(t, m.home)
	code, env, out := uninstall(t, "--plan", "--answer", "retention=restore")
	if code != output.OK || env.Data.Retention == nil || env.Data.Retention.Action != "restore" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if s := stepNamed(env.Data, "claude settings"); s == nil || s.Status != "planned" || !strings.Contains(s.Detail, "cleanupPeriodDays") {
		t.Errorf("settings step: %+v", s)
	}
	sameTree(t, before, treeOf(t, m.home))
}

func TestUninstallRemoteServerRefusesStop(t *testing.T) {
	m := installMachine(t)
	raw := readFile(t, pathsOf(m.home).ConfigFile())
	_ = os.WriteFile(pathsOf(m.home).ConfigFile(), []byte(strings.Replace(raw, "[server]\n", "[server]\nmode = \"remote\"\n", 1)), 0o600)
	before := treeOf(t, m.home)
	for _, args := range [][]string{{"--stop"}, {"--delete-data"}} {
		if code, _, out := uninstall(t, args...); code != output.Blocked || !strings.Contains(out, "on that server's host") {
			t.Errorf("%s on a remote server: exit %d\n%s", args[0], code, out)
		}
	}
	sameTree(t, before, treeOf(t, m.home))
}

func TestUninstallWarnsAboutAPathEntryThatIsNotOurs(t *testing.T) {
	m := installMachine(t)
	link := filepath.Join(pathsOf(m.home).Bin, "backsight")
	_ = os.Remove(link)
	if err := os.WriteFile(link, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, env, out := uninstall(t)
	if code != output.OK || readFile(t, link) != "#!/bin/sh\n" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(strings.Join(env.Data.Warnings, "\n"), link) {
		t.Errorf("no warning names %s: %q", link, env.Data.Warnings)
	}
}

func TestUninstallRemovesARelativeLinkOfOurs(t *testing.T) {
	m := installMachine(t)
	d := pathsOf(m.home)
	link := filepath.Join(d.Bin, "backsight")
	_ = os.Remove(link)
	rel, err := filepath.Rel(d.Bin, filepath.Join(d.Data, "bin", "current"))
	if err != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("fixture: %q %v", rel, err)
	}
	if err := os.Symlink(rel, link); err != nil {
		t.Fatal(err)
	}
	code, env, out := uninstall(t)
	if code != output.OK || onDisk(link) {
		t.Fatalf("exit %d, link left %v\n%s", code, onDisk(link), out)
	}
	if strings.Contains(strings.Join(env.Data.Warnings, "\n"), link) {
		t.Errorf("warns about its own link: %q", env.Data.Warnings)
	}
}

// The retire warning names setup --undo for a reference retire, and is
// silent when setup retired nothing.
func TestUninstallRetireWarning(t *testing.T) {
	t.Run("client retire", func(t *testing.T) {
		m := installMachine(t)
		clientRetireRun(t, m)
		if _, env, out := uninstall(t, "--plan"); !strings.Contains(strings.Join(env.Data.Warnings, " "), "setup --undo") {
			t.Errorf("no warning:\n%s", out)
		}
	})
	t.Run("nothing retired", func(t *testing.T) {
		m := installMachine(t)
		if err := (&setup.Record{SchemaVersion: setup.RecordVersion}).Save(pathsOf(m.home).SetupFile()); err != nil {
			t.Fatal(err)
		}
		if _, env, _ := uninstall(t, "--plan"); strings.Contains(strings.Join(env.Data.Warnings, " "), "setup --undo") {
			t.Errorf("warns with nothing retired: %q", env.Data.Warnings)
		}
	})
}

func TestUninstallOnAnEmptyMachineLeavesNoState(t *testing.T) {
	home := isolate(t)
	withSys(t, "linux")
	withClaude(t)
	d := pathsOf(home)
	_ = os.Remove(filepath.Join(d.Data, "bin", "current"))
	if code, _, out := uninstall(t, "--non-interactive"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, p := range []string{d.LocksDir(), d.State} {
		if onDisk(p) {
			t.Errorf("left behind: %s %v", p, treeOf(t, p))
		}
	}
}
