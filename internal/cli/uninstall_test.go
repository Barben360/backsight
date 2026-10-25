package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	agelib "filippo.io/age"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// installed is a whole install as setup leaves it: client install's
// shim, binary, marketplace and permission rule, the scheduler entries, a
// raised retention (30 days before), the command on PATH, the server's
// files and one backup archive.
type installed struct {
	home     string
	claude   *fakeClaude
	sys      *fakeSys
	dk       *fakeCompose
	settings string
	backup   string
}

func installMachine(t *testing.T) installed {
	t.Helper()
	home := devInstall(t, true)
	d := pathsOf(home)
	m := installed{home: home, settings: filepath.Join(home, ".claude", "settings.json")}
	m.sys = withSys(t, "linux")
	_ = os.MkdirAll(filepath.Dir(m.settings), 0o700)
	if err := os.WriteFile(m.settings, []byte("{\n  \"cleanupPeriodDays\": 30,\n  \"permissions\": {\n    \"allow\": [\n      \"Read\"\n    ]\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.claude, _ = installedOn(t, home, "0.5.0")
	for _, args := range [][]string{{"jobs", "install"}, {"client", "retention"}} {
		if code, out, errOut := run(t, args...); code != output.OK {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
	_ = os.MkdirAll(d.Bin, 0o755)
	if err := os.Symlink(filepath.Join(d.Data, "bin", "current"), filepath.Join(d.Bin, "backsight")); err != nil {
		t.Fatal(err)
	}
	m.dk = withCompose(t)
	if code, out, errOut := run(t, "server", "render"); code != output.OK {
		t.Fatalf("server render: exit %d\n%s%s", code, out, errOut)
	}
	m.backup = filepath.Join(d.Data, "backups", "backsight-20261025T040000Z.tar.gz.age")
	_ = os.MkdirAll(filepath.Dir(m.backup), 0o700)
	if err := os.WriteFile(m.backup, []byte("age-encrypted archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")); err != nil {
		t.Fatalf("the fixture has no scheduler entry: %v", err)
	}
	m.claude.calls, m.claude.ran, m.sys.calls, m.dk.calls = nil, nil, nil, nil
	return m
}

// snapshot is every file and link under root, with its content or target.
func treeOf(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		switch {
		case de.Type()&fs.ModeSymlink != 0:
			dest, _ := os.Readlink(p)
			out[p] = "-> " + dest
		case de.Type().IsRegular():
			raw, _ := os.ReadFile(p)
			sum := sha256.Sum256(raw)
			out[p] = hex.EncodeToString(sum[:])
		case de.IsDir():
			out[p] = "dir"
		}
		return nil
	})
	return out
}

func sameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	for p, v := range before {
		if after[p] != v {
			t.Errorf("changed: %s (%q → %q)", p, v, after[p])
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("created: %s", p)
		}
	}
}

type uninstallEnvelope struct {
	OK    bool            `json:"ok"`
	Data  uninstallResult `json:"data"`
	Error *struct {
		Code      int              `json:"code"`
		Message   string           `json:"message"`
		Questions []setup.Question `json:"questions"`
		Actions   []output.Action  `json:"actions"`
		Resume    string           `json:"resume"`
	} `json:"error"`
}

func uninstall(t *testing.T, args ...string) (int, uninstallEnvelope, string) {
	t.Helper()
	code, out, errOut := run(t, append([]string{"uninstall", "--json"}, args...)...)
	var env uninstallEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("exit %d, not one envelope: %v\n%s\n%s", code, err, out, errOut)
	}
	return code, env, out
}

func onDisk(p string) bool { _, err := os.Lstat(p); return err == nil }

func stepNamed(r uninstallResult, prefix string) *undoAction {
	for i := range r.Steps {
		if strings.HasPrefix(r.Steps[i].Name, prefix) {
			return &r.Steps[i]
		}
	}
	return nil
}

func TestUninstallPlanChangesNothing(t *testing.T) {
	m := installMachine(t)
	before := treeOf(t, m.home)
	code, env, out := uninstall(t, "--plan", "--delete-data", "--purge-local")
	if code != output.OK || !env.Data.Plan {
		t.Fatalf("exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, m.home))
	if got := mutating(m.claude); len(got) > 0 {
		t.Errorf("claude changed: %q", got)
	}
	if len(m.sys.calls) > 0 || len(m.dk.calls) > 0 {
		t.Errorf("ran %q and %q", m.sys.calls, m.dk.subcommands())
	}
	for _, name := range []string{"scheduler", "client", "claude settings", "server", "binaries", "local files"} {
		if s := stepNamed(env.Data, name); s == nil || s.Status != "planned" {
			t.Errorf("%s not planned:\n%s", name, out)
		}
	}
	// The plan names the edited paths, never a whole file.
	if s := stepNamed(env.Data, "claude settings"); s == nil || !strings.Contains(s.Detail, "permissions.allow") {
		t.Errorf("settings plan: %+v", s)
	}
}

func TestUninstallRemovesEachPieceAndKeepsTheRest(t *testing.T) {
	m := installMachine(t)
	d := pathsOf(m.home)
	code, env, out := uninstall(t)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got, want := mutating(m.claude), []string{"plugin uninstall " + plugin.PluginID() + " --scope user", "plugin marketplace remove " + plugin.Marketplace}; !reflect.DeepEqual(got, want) {
		t.Errorf("claude: %q", got)
	}
	for _, p := range []string{
		filepath.Join(m.home, "xdg", "systemd", "user", "backsight-tick.timer"),
		filepath.Join(m.home, "xdg", "systemd", "user", "backsight-maintain.timer"),
		filepath.Join(d.Data, "bin"), filepath.Join(d.Data, "marketplace"), filepath.Join(d.Bin, "backsight"),
	} {
		if onDisk(p) {
			t.Errorf("still there: %s", p)
		}
	}
	settings := readFile(t, m.settings)
	for _, tool := range plugin.ToolNames {
		if strings.Contains(settings, tool) {
			t.Errorf("permission rule %s left:\n%s", tool, settings)
		}
	}
	// The user's own rule and the longer retention stay (the default).
	if !strings.Contains(settings, `"Read"`) || !strings.Contains(settings, `"cleanupPeriodDays": 3650`) {
		t.Errorf("settings:\n%s", settings)
	}
	if r := env.Data.Retention; r == nil || r.Before != "30" || r.Action != "kept" {
		t.Errorf("the old retention is not offered back: %+v", r)
	}
	// Config, secrets, state, the server and its data, and backups stay.
	for _, p := range []string{d.ConfigFile(), d.SecretsFile(), d.StateFile(), filepath.Join(composeDir(m.home), "compose.yaml"), filepath.Join(composeDir(m.home), ".env")} {
		if !onDisk(p) {
			t.Errorf("removed: %s", p)
		}
	}
	if readFile(t, m.backup) != "age-encrypted archive" {
		t.Error("the backup changed")
	}
	if len(m.dk.subcommands()) > 0 {
		t.Errorf("the kept server was touched: %q", m.dk.subcommands())
	}
	// Through the config editor: the run's pre-image is kept.
	runs, _ := claudecfg.Runs(d.PreImagesDir())
	if mf, err := claudecfg.LoadManifest(d.PreImagesDir(), runs[len(runs)-1]); err != nil || mf.Command != "uninstall" {
		t.Errorf("no uninstall pre-image: %+v %v", mf, err)
	}
	if st, _ := state.Load(d.StateFile()); st.PluginVersion != "" || st.Scheduler != nil || st.ShimTarget != "" {
		t.Errorf("state still says installed: %+v", st)
	}
	// A rerun finds nothing left to do.
	m.claude.calls = nil
	m.claude.plugins, m.claude.marketplaces = "[]", "[]"
	if code, _, out := uninstall(t); code != output.OK || len(mutating(m.claude)) > 0 {
		t.Errorf("rerun: exit %d, %q\n%s", code, mutating(m.claude), out)
	}
}

func TestUninstallOffersTheRetentionBack(t *testing.T) {
	t.Run("restored", func(t *testing.T) {
		m := installMachine(t)
		code, env, out := uninstall(t, "--answer", "retention=restore")
		if code != output.OK || !strings.Contains(readFile(t, m.settings), `"cleanupPeriodDays": 30`) || env.Data.Retention.Action != "restored" {
			t.Fatalf("exit %d\n%s\n%s", code, readFile(t, m.settings), out)
		}
	})
	t.Run("absent before", func(t *testing.T) {
		m := installMachine(t)
		_ = state.Update(pathsOf(m.home), "test", func(s *state.State) error { s.Client.RetentionBefore = json.RawMessage("null"); return nil })
		if code, _, out := uninstall(t, "--answer", "retention=restore"); code != output.OK || strings.Contains(readFile(t, m.settings), "cleanupPeriodDays") {
			t.Fatalf("exit %d\n%s\n%s", code, readFile(t, m.settings), out)
		}
	})
	t.Run("changed by hand since", func(t *testing.T) {
		m := installMachine(t)
		_ = os.WriteFile(m.settings, []byte(strings.Replace(readFile(t, m.settings), "3650", "7000", 1)), 0o644)
		code, env, out := uninstall(t, "--answer", "retention=restore")
		if code != output.OK || !strings.Contains(readFile(t, m.settings), `"cleanupPeriodDays": 7000`) || env.Data.Retention.Action != "changed_since" {
			t.Fatalf("exit %d\n%s\n%s", code, readFile(t, m.settings), out)
		}
	})
	t.Run("invalid answer", func(t *testing.T) {
		m := installMachine(t)
		before := treeOf(t, m.home)
		if code, _, out := uninstall(t, "--answer", "retention=maybe"); code != output.Usage {
			t.Fatalf("exit %d\n%s", code, out)
		}
		sameTree(t, before, treeOf(t, m.home))
	})
}

func TestUninstallStopKeepsTheData(t *testing.T) {
	m := installMachine(t)
	if code, env, out := uninstall(t, "--stop"); code != output.OK || stepNamed(env.Data, "server").Status != "done" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := m.dk.subcommands(); !reflect.DeepEqual(got, []string{"stop"}) {
		t.Errorf("compose: %q", got)
	}
	if !onDisk(filepath.Join(composeDir(m.home), ".env")) || readFile(t, m.backup) != "age-encrypted archive" {
		t.Error("stop removed the server's files or the backup")
	}
}

func TestUninstallDeletesDataOnlyWithTheBankID(t *testing.T) {
	m := installMachine(t)
	before := treeOf(t, m.home)
	code, env, out := uninstall(t, "--delete-data")
	if code != output.NeedsUser || env.Error == nil || len(env.Error.Questions) != 1 || env.Error.Questions[0].ID != "confirm_bank" {
		t.Fatalf("no id: exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, m.home))
	if code, _, out := uninstall(t, "--delete-data", "--answer", "confirm_bank=another-bank"); code != output.Usage {
		t.Fatalf("wrong id: exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, m.home))
	if len(mutating(m.claude)) > 0 || len(m.dk.calls) > 0 || len(m.sys.calls) > 0 {
		t.Fatalf("acted without the id: %q %q %q", mutating(m.claude), m.dk.subcommands(), m.sys.calls)
	}

	code, _, out = uninstall(t, "--delete-data", "--answer", "confirm_bank=claude-memory")
	if code != output.OK {
		t.Fatalf("right id: exit %d\n%s", code, out)
	}
	if got := m.dk.subcommands(); !reflect.DeepEqual(got, []string{"down --remove-orphans"}) {
		t.Errorf("compose: %q", got)
	}
	for _, f := range []string{"compose.yaml", ".env"} {
		if onDisk(filepath.Join(composeDir(m.home), f)) {
			t.Errorf("%s left", f)
		}
	}
	if readFile(t, m.backup) != "age-encrypted archive" {
		t.Error("a backup was touched")
	}
}

func atTerminal(t *testing.T, input string) {
	t.Helper()
	withStdin(t, input)
	stdinIsTerminal = func() bool { return true }
	old := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdoutIsTerminal = old })
}

func TestUninstallAsksForTheBankIDAtATerminal(t *testing.T) {
	m := installMachine(t)
	before := treeOf(t, m.home)
	atTerminal(t, "claude\n")
	code, out, errOut := run(t, "uninstall", "--delete-data")
	if code != output.Usage || !strings.Contains(errOut+out, "is not this server's") {
		t.Fatalf("mistyped: exit %d\n%s%s", code, out, errOut)
	}
	sameTree(t, before, treeOf(t, m.home))
	if len(m.dk.calls) > 0 || len(mutating(m.claude)) > 0 {
		t.Fatalf("acted on a mistyped id: %q %q", m.dk.subcommands(), mutating(m.claude))
	}
	// The id, then Enter for the retention question (keep).
	atTerminal(t, "claude-memory\n\n")
	if code, out, errOut := run(t, "uninstall", "--delete-data"); code != output.OK || !reflect.DeepEqual(m.dk.subcommands(), []string{"down --remove-orphans"}) {
		t.Fatalf("typed: exit %d %q\n%s%s", code, m.dk.subcommands(), out, errOut)
	}
	if !strings.Contains(readFile(t, m.settings), "3650") {
		t.Error("Enter did not keep the retention")
	}
}

func TestUninstallRefusesWhileBusy(t *testing.T) {
	for name, mark := range map[string]func(t *testing.T, home string){
		"maintenance held": func(t *testing.T, home string) {
			l, _, err := state.Acquire(pathsOf(home).MaintenanceLock(), "backup create")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Release() })
		},
		"restore unfinished": func(t *testing.T, home string) {
			if err := state.WriteRestoreMarker(pathsOf(home), state.RestoreMarker{Command: "restore", StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		},
		"update unfinished": func(t *testing.T, home string) {
			if err := state.WriteUpdateMarker(pathsOf(home), state.UpdateMarker{From: "0.4.0", To: "0.5.0", StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := installMachine(t)
			mark(t, m.home)
			before := treeOf(t, m.home)
			if code, _, out := uninstall(t); code != output.Blocked {
				t.Fatalf("exit %d\n%s", code, out)
			}
			sameTree(t, before, treeOf(t, m.home))
			if len(mutating(m.claude)) > 0 || len(m.sys.calls) > 0 {
				t.Errorf("acted: %q %q", mutating(m.claude), m.sys.calls)
			}
		})
	}
}

func TestUninstallPurgeLocalKeepsBackupsAndTheirKey(t *testing.T) {
	m := installMachine(t)
	d := pathsOf(m.home)
	id, _ := agelib.GenerateX25519Identity()
	_ = config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { return s.Set("backup_identity", id.String()) })
	// A backup directory chosen inside the state directory survives too.
	inState := filepath.Join(d.State, "my-backups", "backsight-1.tar.gz.age")
	_ = os.MkdirAll(filepath.Dir(inState), 0o700)
	_ = os.WriteFile(inState, []byte("archive"), 0o600)
	configure(t, m.home, "[backup]\ndir = "+strconvQuote(filepath.Dir(inState)))

	before := treeOf(t, m.home)
	code, env, out := uninstall(t, "--purge-local")
	if code != output.NeedsUser || env.Error == nil || len(env.Error.Questions) != 1 || env.Error.Questions[0].ID != "identity_saved" {
		t.Fatalf("the backups' only key would go: exit %d\n%s", code, out)
	}
	if len(env.Error.Actions) == 0 || !strings.Contains(env.Error.Actions[0].Cmd, "backup identity show") {
		t.Errorf("no way to save the key: %+v", env.Error.Actions)
	}
	sameTree(t, before, treeOf(t, m.home))

	if code, _, out := uninstall(t, "--purge-local", "--answer", "identity_saved=yes"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, p := range []string{d.ConfigFile(), d.SecretsFile(), d.StateFile(), d.PreImagesDir()} {
		if onDisk(p) {
			t.Errorf("not purged: %s", p)
		}
	}
	if readFile(t, inState) != "archive" || readFile(t, m.backup) != "age-encrypted archive" {
		t.Error("a backup was removed")
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestUninstallLeavesAPathEntryThatIsNotOurs(t *testing.T) {
	m := installMachine(t)
	link := filepath.Join(pathsOf(m.home).Bin, "backsight")
	_ = os.Remove(link)
	dev := filepath.Join(m.home, "src", "backsight")
	if err := os.Symlink(dev, link); err != nil {
		t.Fatal(err)
	}
	if code, _, out := uninstall(t); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if dest, err := os.Readlink(link); err != nil || dest != dev {
		t.Errorf("a developer's link was touched: %q %v", dest, err)
	}
}

// A failing step stops before the binaries go, so a rerun can finish.
func TestUninstallFailureIsResumable(t *testing.T) {
	m := installMachine(t)
	m.claude.failOn = "plugin uninstall"
	code, env, out := uninstall(t)
	if code != output.Partial || env.Error == nil || !strings.Contains(env.Error.Resume, "backsight uninstall") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !onDisk(filepath.Join(pathsOf(m.home).Data, "bin", "current")) {
		t.Error("the binaries went before the plugin was removed")
	}
	m.claude.failOn = ""
	if code, _, out := uninstall(t); code != output.OK {
		t.Fatalf("rerun: exit %d\n%s", code, out)
	}
}

func TestUninstallFlagsAreChecked(t *testing.T) {
	m := installMachine(t)
	before := treeOf(t, m.home)
	if code, _, out := uninstall(t, "--stop", "--delete-data"); code != output.Usage {
		t.Errorf("--stop with --delete-data: exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, m.home))
	raw := readFile(t, pathsOf(m.home).ConfigFile())
	_ = os.WriteFile(pathsOf(m.home).ConfigFile(), []byte(strings.Replace(raw, "[server]\n", "[server]\nmode = \"remote\"\n", 1)), 0o600)
	before = treeOf(t, m.home)
	if code, _, out := uninstall(t, "--stop"); code != output.Blocked || !strings.Contains(out, "remote") {
		t.Errorf("--stop on a remote server: exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, m.home))
}

// setup retired reference pieces: uninstall says it does not bring them
// back, and how to.
func TestUninstallNamesWhatSetupRetired(t *testing.T) {
	m := installMachine(t)
	rec := &setup.Record{SchemaVersion: setup.RecordVersion}
	rec.Undo.Timers = []string{"memory-retro.timer"}
	if err := rec.Save(pathsOf(m.home).SetupFile()); err != nil {
		t.Fatal(err)
	}
	code, env, out := uninstall(t, "--plan")
	if code != output.OK || !strings.Contains(strings.Join(env.Data.Warnings, " "), "setup --undo") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// Nothing installed: every step is skipped, and --delete-data asks for no
// bank id since there is no server to delete.
func TestUninstallOnAnEmptyMachine(t *testing.T) {
	home := isolate(t)
	withSys(t, "linux")
	withClaude(t)
	_ = os.Remove(filepath.Join(pathsOf(home).Data, "bin", "current")) // withSys's placeholder shim
	code, env, out := uninstall(t, "--delete-data", "--non-interactive")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, s := range env.Data.Steps {
		if s.Status != "skipped" && s.Name != "binaries" {
			t.Errorf("%s: %s %s", s.Name, s.Status, s.Detail)
		}
	}
	if strings.Contains(strings.Join(env.Data.Kept, " "), "server's data") {
		t.Errorf("claims to keep a server that is not there: %q", env.Data.Kept)
	}
}
