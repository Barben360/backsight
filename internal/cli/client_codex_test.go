package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// codexStart is a user's config.toml before backsight (Codex's own memories
// on), the one a whole round trip must give back.
const codexStart = "model = \"gpt-6.1-sol\"\n\n[features]\nmemories = true\n\n[mcp_servers.probe]\ncommand = \"/opt/probe\"\n"

func codexHomeWith(t *testing.T, home, cfg string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex")
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func installCodex(t *testing.T, args ...string) clientInstallResult {
	t.Helper()
	code, out, errOut := run(t, append([]string{"client", "install", "--codex", "--json"}, args...)...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var res clientInstallResult
	jsonData(t, out, &res)
	return res
}

// client install --codex: the shim, the two plugins in their own
// marketplace, added with codex, the Codex home recorded, the trust to
// give; nothing written to config.toml by backsight; Claude Code untouched.
func TestClientInstallCodexFirstTime(t *testing.T) {
	home := devInstall(t, true)
	claude := withClaude(t)
	f := withCodex(t, home)
	dir := codexHomeWith(t, home, codexStart)
	res := installCodex(t)
	d := pathsOf(home)
	mkt := filepath.Join(d.Data, "codex-marketplace")
	if got := f.mutating(); !slices.Equal(got, []string{"plugin marketplace add " + mkt, "plugin add backsight@backsight", "plugin add backsight-hooks@backsight"}) {
		t.Errorf("codex calls %q", got)
	}
	if len(claude.calls) != 0 {
		t.Errorf("claude ran: %v", claude.calls)
	}
	if res.Client != "codex" || res.Profile != dir || res.Trust == "" || !strings.Contains(res.Trust, "/hooks") {
		t.Errorf("result %+v", res)
	}
	st, _ := state.Load(d.StateFile())
	c := st.ClientOf(harnessid.Codex)
	if c == nil || c.Profile != "" || c.PluginVersion == "" || st.ShimTarget == "" || st.PluginVersion != "" {
		t.Errorf("state %+v %+v", st, c)
	}
	if _, err := os.Stat(filepath.Join(mkt, "plugins", "backsight-hooks", "hooks", "hooks.json")); err != nil {
		t.Error(err)
	}
	// Codex wrote its tables; backsight's own edit is none.
	if cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml")); !strings.HasPrefix(string(cfg), codexStart) {
		t.Errorf("config.toml:\n%s", cfg)
	}
	if runs, _ := claudecfg.Runs(d.PreImagesDir()); len(runs) != 0 {
		t.Errorf("config editor runs %v", runs)
	}
	// Again: nothing to add, no trust to give.
	f.calls = nil
	res = installCodex(t)
	if got := f.mutating(); len(got) != 0 || res.Trust != "" {
		t.Errorf("second install: %v, trust %q", got, res.Trust)
	}
	if text := installText(res); !strings.Contains(text, "restart Codex") || strings.Contains(text, "Claude") {
		t.Errorf("text %q", text)
	}
}

// The Codex home: --codex-home, else the shell's, recorded once; a later
// install (update apply's, a job's) acts on the recorded one whatever the
// shell says, and codex runs with it.
func TestClientInstallCodexRecordsItsHome(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	f := withCodex(t, home)
	shellHome := filepath.Join(home, "codex-shell")
	_ = os.MkdirAll(shellHome, 0o700)
	t.Setenv("CODEX_HOME", shellHome)
	res := installCodex(t)
	if res.Profile != shellHome {
		t.Fatalf("profile %s", res.Profile)
	}
	other := filepath.Join(home, "codex-other")
	_ = os.MkdirAll(other, 0o700)
	t.Setenv("CODEX_HOME", other)
	f.calls = nil
	if res := installCodex(t); res.Profile != shellHome {
		t.Errorf("the shell's moved the install: %s", res.Profile)
	}
	for _, c := range f.calls {
		if c.home != shellHome {
			t.Errorf("%s ran in %s", c.line, c.home)
		}
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.ProfileOf(harnessid.Codex) != shellHome {
		t.Errorf("recorded %q", st.ProfileOf(harnessid.Codex))
	}
	// --codex-home moves it, explicitly.
	if res := installCodex(t, "--codex-home", other); res.Profile != other {
		t.Errorf("--codex-home: %s", res.Profile)
	}
	if code, _, _ := run(t, "client", "install", "--codex", "--codex-home", filepath.Join(home, "nope")); code != output.Usage {
		t.Errorf("a missing --codex-home: exit %d", code)
	}
}

// Refusals: no codex, an older one, a user server named memory (named,
// nothing written), the flags of the other tool.
func TestClientInstallCodexRefusals(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	f := withCodex(t, home)
	f.missing = true
	if code, out, _ := run(t, "client", "install", "--codex", "--json"); code != output.NeedsUser || !strings.Contains(out, "npm install -g @openai/codex") {
		t.Errorf("missing: exit %d %s", code, out)
	}
	f.missing, f.version = false, "0.166.9"
	if code, out, _ := run(t, "client", "install", "--codex", "--json"); code != output.NeedsUser || !strings.Contains(out, "older than 0.167.0") {
		t.Errorf("older: exit %d %s", code, out)
	}
	f.version = "0.167.0"
	codexHomeWith(t, home, "[mcp_servers.memory]\ncommand = \"/usr/bin/mem\"\n")
	code, out, _ := run(t, "client", "install", "--codex", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "[mcp_servers.memory]") || !strings.Contains(out, "codex mcp remove memory") {
		t.Errorf("clash in config.toml: exit %d %s", code, out)
	}
	codexHomeWith(t, home, "")
	f.mcp = `[{"name":"memory","transport":{"type":"stdio","command":"/usr/bin/mem"}}]`
	if code, out, _ := run(t, "client", "install", "--codex", "--json"); code != output.NeedsUser || !strings.Contains(out, "/usr/bin/mem") {
		t.Errorf("clash in codex mcp list: exit %d %s", code, out)
	}
	if len(f.mutating()) != 0 {
		t.Errorf("a refusal ran %v", f.mutating())
	}
	if _, err := os.Stat(filepath.Join(pathsOf(home).Data, "codex-marketplace")); !os.IsNotExist(err) {
		t.Error("a refusal wrote the plugins")
	}
	if code, _, _ := run(t, "client", "install", "--codex-home", home); code != output.Usage {
		t.Errorf("--codex-home alone: exit %d", code)
	}
	if code, _, _ := run(t, "client", "--config-dir", home, "install", "--codex"); code != output.Usage {
		t.Errorf("--config-dir with --codex: exit %d", code)
	}
}

// A plan writes nothing and runs codex's list commands only.
func TestClientInstallCodexPlan(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	f := withCodex(t, home)
	code, out, _ := run(t, "client", "install", "--codex", "--plan", "--json")
	if code != output.OK {
		t.Fatalf("exit %d %s", code, out)
	}
	var res clientInstallResult
	jsonData(t, out, &res)
	if len(f.mutating()) != 0 || !res.Plan || !strings.HasPrefix(res.Trust, "Codex will ask") {
		t.Errorf("plan ran %v: %+v", f.mutating(), res)
	}
	if _, err := os.Stat(pathsOf(home).StateFile()); err == nil {
		if st, _ := state.Load(pathsOf(home).StateFile()); st.ClientOf(harnessid.Codex) != nil {
			t.Error("a plan recorded Codex")
		}
	}
}

// client codex-memories off: memories = true removed, a copy kept, the
// state says backsight did it; on puts it back.
func TestClientCodexMemories(t *testing.T) {
	home := devInstall(t, true)
	f := withCodex(t, home)
	dir := codexHomeWith(t, home, codexStart)
	f.memories = true
	code, out, _ := run(t, "client", "codex-memories", "off", "--plan", "--json")
	if code != output.OK || !strings.Contains(out, "features.memories") {
		t.Fatalf("plan: exit %d %s", code, out)
	}
	if cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml")); string(cfg) != codexStart {
		t.Fatal("a plan wrote")
	}
	f.memories = false // what Codex says once the key is gone
	code, out, _ = run(t, "client", "codex-memories", "off", "--json")
	if code != output.OK {
		t.Fatalf("off: exit %d %s", code, out)
	}
	var res clientConfigResult
	jsonData(t, out, &res)
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if strings.Contains(string(cfg), "memories") || !strings.Contains(string(cfg), "[features]\n") || res.Run == "" || len(res.Notes) != 0 {
		t.Errorf("off:\n%s\n%+v", cfg, res)
	}
	m, err := claudecfg.LoadManifest(pathsOf(home).PreImagesDir(), res.Run)
	if err != nil || m.Command != "client codex-memories" || m.Files[0].Target != filepath.Join(dir, "config.toml") {
		t.Errorf("manifest %+v %v", m, err)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if c := st.ClientOf(harnessid.Codex); c == nil || !c.MemoriesOff {
		t.Errorf("state %+v", c)
	}
	// Still on elsewhere: said.
	f.memories = true
	_, out, _ = run(t, "client", "codex-memories", "off", "--json")
	if !strings.Contains(out, "still on") {
		t.Errorf("no note: %s", out)
	}
	code, _, _ = run(t, "client", "codex-memories", "on")
	if cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml")); code != output.OK || !strings.Contains(string(cfg), "[features]\nmemories = true\n") {
		t.Errorf("on: exit %d\n%s", code, cfg)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.ClientOf(harnessid.Codex).MemoriesOff {
		t.Error("still recorded off")
	}
	if code, _, _ := run(t, "client", "codex-memories", "maybe"); code != output.Usage {
		t.Errorf("maybe: exit %d", code)
	}
}

// The round trip of the issue on a throwaway Codex home: install, the user
// trusts the hooks, memories off, an update, then uninstall: config.toml
// is as it started, but for the [hooks.state] table Codex wrote itself,
// and the plugin cache Codex leaves is gone.
func TestCodexInstallThenUninstallGivesConfigBack(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	f := withCodex(t, home)
	dir := codexHomeWith(t, home, codexStart)
	installCodex(t)
	trustAll(t, dir)
	f.memories = true
	if code, out, _ := run(t, "client", "codex-memories", "off"); code != output.OK {
		t.Fatalf("off: %d %s", code, out)
	}
	setBuild(t, "9.9.9", "abc")
	f.calls = nil
	res := installCodex(t) // update apply's client install
	if got := f.mutating(); !slices.Equal(got, []string{"plugin add backsight@backsight"}) || res.Trust != "" {
		t.Errorf("update re-added %v, trust %q", got, res.Trust)
	}
	code, out, errOut := run(t, "uninstall", "--json")
	if code != output.OK {
		t.Fatalf("uninstall: exit %d %s %s", code, out, errOut)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if string(cfg) != codexStart+"\n[hooks.state]\n" {
		t.Errorf("config.toml after uninstall:\n%q\nwant\n%q", cfg, codexStart+"\n[hooks.state]\n")
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "cache", "backsight")); !os.IsNotExist(err) {
		t.Errorf("the plugin cache is left: %v", err)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.ClientOf(harnessid.Codex) != nil {
		t.Errorf("state keeps Codex: %+v", st.ClientOf(harnessid.Codex))
	}
}

// uninstall --plan says what goes, changing nothing; with codex missing,
// uninstall says what to remove by hand.
func TestUninstallCodexPlanAndMissing(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	f := withCodex(t, home)
	dir := codexHomeWith(t, home, codexStart)
	installCodex(t)
	before, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	f.calls = nil
	code, out, _ := run(t, "uninstall", "--plan", "--json")
	if code != output.OK || !strings.Contains(out, `"codex `+jsonPathOf(dir)) || len(f.mutating()) != 0 {
		t.Errorf("plan: exit %d %s, ran %v", code, out, f.mutating())
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "config.toml")); string(after) != string(before) {
		t.Error("a plan changed config.toml")
	}
	f.missing = true
	code, out, _ = run(t, "uninstall", "--json")
	if code != output.OK || !strings.Contains(out, "codex plugin remove backsight-hooks@backsight") {
		t.Errorf("missing codex: exit %d %s", code, out)
	}
}

// setup --undo removes the plugins where setup added them and puts Codex's
// memories back at their place, though Codex wrote config.toml since.
func TestSetupUndoRemovesCodex(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	f := withCodex(t, home)
	dir := codexHomeWith(t, home, codexStart)
	installCodex(t)
	f.memories = true
	code, out, _ := run(t, "client", "codex-memories", "off", "--json")
	if code != output.OK {
		t.Fatal(out)
	}
	var mr clientConfigResult
	jsonData(t, out, &mr)
	trustAll(t, dir) // Codex writes config.toml after the run
	d := pathsOf(home)
	rec := &setup.Record{SchemaVersion: setup.RecordVersion, Undo: setup.Undo{CodexHomes: []string{dir}, ConfigRuns: []string{mr.Run}}}
	if err := rec.Save(d.SetupFile()); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "setup", "--undo", "--json")
	if code != output.OK {
		t.Fatalf("undo: exit %d %s %s", code, out, errOut)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if string(cfg) != codexStart+"\n[hooks.state]\n" {
		t.Errorf("config.toml after undo:\n%s", cfg)
	}
	st, _ := state.Load(d.StateFile())
	if st.ClientOf(harnessid.Codex) != nil {
		t.Errorf("state keeps Codex: %+v", st.ClientOf(harnessid.Codex))
	}
	if r, _ := setup.Load(d.SetupFile()); !r.Undo.Empty() {
		t.Errorf("undo record %+v", r.Undo)
	}
}

// The drift a session compares: Claude Code's from the shell's profile and
// state's plugin_version; Codex's from its own entry, never the shell's.
func TestWiredRecordPerTool(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/shell/claude")
	t.Setenv("CODEX_HOME", "/shell/codex")
	st := state.State{PluginVersion: "c1", ShimTarget: "/t", Clients: map[string]*state.Client{harnessid.Codex: {Profile: "/rec/codex", PluginVersion: "x1"}}}
	env, rec := wiredRecord(claudeCode, "/home/u", "/data", st)
	if env.Profile != "/shell/claude" || rec.Version != "c1" || rec.ShimTarget != "/t" || rec.Data != "/data" {
		t.Errorf("Claude Code: %+v %+v", env, rec)
	}
	env, rec = wiredRecord(codex, "/home/u", "/data", st)
	if env.Profile != "/rec/codex" || rec.Version != "x1" || rec.ShimTarget != "/t" {
		t.Errorf("Codex: %+v %+v", env, rec)
	}
	if _, rec := wiredRecord(codex, "/home/u", "/data", state.State{PluginVersion: "c1"}); rec.Version != "" {
		t.Errorf("Codex not wired: %+v", rec)
	}
}

// The real runner: codex from PATH, the shell's CODEX_HOME dropped and the
// one asked for given.
func TestCodexRunnerDropsTheShellsHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a sh script stands for codex")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s|%s' \"$CODEX_HOME\" \"$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", "/shell/codex")
	out, err := codexCLI{}.Run(t.Context(), nil, "plugin", "list", "--json")
	if err != nil || string(out) != "|plugin list --json" {
		t.Errorf("default home: %q %v", out, err)
	}
	out, err = codexCLI{}.Run(t.Context(), harness.Codex{}.ProfileEnv("/home/u", "/rec/codex"), "--version")
	if err != nil || string(out) != "/rec/codex|--version" {
		t.Errorf("recorded home: %q %v", out, err)
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\necho oops >&2\nexit 3\n"), 0o755)
	if _, err := (codexCLI{}).Run(t.Context(), nil, "x"); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("a failing codex: %v", err)
	}
	var _ = json.Valid
}
