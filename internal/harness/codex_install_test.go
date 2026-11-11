package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/state"
)

// codex_install.go's ledger: block 3, https://github.com/Barben360/backsight/issues/278.
func init() {
	codexOwns("codex_install.go", codexPart{over: true,
		done: []string{"MinVersion", "ProfileVar", "IsDefaultProfile", "Locate", "ProfileEnv", "Detect", "Install", "Uninstall",
			"Installed", "Drift", "Untrusted", "NameClash", "MemoriesOn", "HookCommand", "ShellHome", "RecordHome", "RecordedHome"}})
}

const codexFixtures = "../testdata/codex/0.167.0/"

func TestCodexMinVersionAndProfileVar(t *testing.T) {
	c := Codex{}
	if c.MinVersion() != "0.167.0" {
		t.Error(c.MinVersion())
	}
	// The home is recorded, never read from a shell: no variable for the
	// shared lookup to read.
	if c.ProfileVar() != "" {
		t.Errorf("ProfileVar %q", c.ProfileVar())
	}
}

// The handler's command is the quoted shim, then the contract's words, the
// event last; cmd's form is the shim in double quotes.
func TestCodexHookCommand(t *testing.T) {
	c := Codex{}
	got, err := c.HookCommand("/home/o'neil/My Data/bin/current", EventStop, false)
	if err != nil || got != `'/home/o'\''neil/My Data/bin/current' hook --harness codex stop` {
		t.Errorf("sh: %s %v", got, err)
	}
	got, err = c.HookCommand(`C:\Users\Jane Doe\AppData\Local\backsight\bin\current.exe`, EventSessionStart, true)
	if err != nil || got != `"C:\Users\Jane Doe\AppData\Local\backsight\bin\current.exe" hook --harness codex session-start` {
		t.Errorf("cmd: %s %v", got, err)
	}
	if _, err := c.HookCommand(`C:\50%\bin\current.exe`, EventStop, true); !errors.Is(err, ErrUnquotable) {
		t.Errorf("a %% path for cmd: %v", err)
	}
}

// genTree is the generated marketplace for shim, binary and skills.
func genTree(t *testing.T, shim, binary string, skills map[string][]byte) map[string][]byte {
	t.Helper()
	tree, err := codexTree(shim, binary, skills)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

type hooksDoc struct {
	Hooks map[string][]struct {
		Matcher string           `json:"matcher"`
		Hooks   []map[string]any `json:"hooks"`
	} `json:"hooks"`
}

// 0085's generator tests: no args, every command ends with the contract's
// words and runs the shim with them in sh, the PostToolUse matcher matches
// Codex's tool name and not Claude Code's, every handler has its timeout,
// SessionStart has its two matcher rows.
func TestCodexHooksJSON(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "my data", "o'neil $x", "current")
	tree := genTree(t, shim, "1.2.3", nil)
	var doc hooksDoc
	if err := json.Unmarshal(tree["plugins/backsight-hooks/hooks/hooks.json"], &doc); err != nil {
		t.Fatal(err)
	}
	timeouts := map[string]int{}
	for _, e := range (Codex{}).Events() {
		timeouts[e.Name+"|"+e.Matcher] = e.Timeout
	}
	n := 0
	for name, groups := range doc.Hooks {
		for _, g := range groups {
			if len(g.Hooks) != 1 {
				t.Fatalf("%s: %d handlers", name, len(g.Hooks))
			}
			h := g.Hooks[0]
			n++
			if _, ok := h["args"]; ok {
				t.Errorf("%s has args: Codex ignores them", name)
			}
			cmd, _ := h["command"].(string)
			event := cmd[strings.LastIndex(cmd, " ")+1:]
			if !strings.HasSuffix(cmd, " hook --harness codex "+event) {
				t.Errorf("%s: command %q", name, cmd)
			}
			if timeout, ok := h["timeout"].(float64); !ok || int(timeout) != timeouts[name+"|"+g.Matcher] || timeout == 0 {
				t.Errorf("%s %s: timeout %v", name, g.Matcher, h["timeout"])
			}
			if h["type"] != "command" {
				t.Errorf("%s: type %v", name, h["type"])
			}
		}
	}
	if n != len((Codex{}).Events()) || len(doc.Hooks["SessionStart"]) != 2 || doc.Hooks["SessionStart"][0].Matcher != "startup" || doc.Hooks["SessionStart"][1].Matcher != "resume|clear|compact|fork" {
		t.Errorf("%d handlers, SessionStart %+v", n, doc.Hooks["SessionStart"])
	}
	post := regexp.MustCompile("^(?:" + doc.Hooks["PostToolUse"][0].Matcher + ")$")
	if !post.MatchString("mcp__backsight__memory_search") || post.MatchString("mcp__plugin_backsight_memory__memory_search") || post.MatchString("mcp__memory__memory_search") {
		t.Errorf("PostToolUse matcher %q", post)
	}
	// The command runs the shim with the contract's words, as Codex's
	// $SHELL -lc does (sh here: QuoteSh's round trips cover the others).
	if runtime.GOOS != "windows" {
		if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s|' \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := doc.Hooks["Stop"][0].Hooks[0]["command"].(string)
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil || string(out) != "hook|--harness|codex|stop|" {
			t.Errorf("sh -c %s: %q %v", cmd, out, err)
		}
	}
}

// commandWindows is the shim in double quotes, for cmd /C, a path with
// spaces and & included; one cmd cannot read is refused on Windows, and
// only its commandWindows is left out elsewhere.
func TestCodexCommandWindows(t *testing.T) {
	defer func(g string) { codexGOOS = g }(codexGOOS)
	codexGOOS = "windows"
	shim := `C:\Users\Jane Doe & Co\AppData\Local\backsight\bin\current.exe`
	var doc hooksDoc
	if err := json.Unmarshal(genTree(t, shim, "1.2.3", nil)["plugins/backsight-hooks/hooks/hooks.json"], &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Hooks["SessionEnd"][0].Hooks[0]["commandWindows"]; got != `"C:\Users\Jane Doe & Co\AppData\Local\backsight\bin\current.exe" hook --harness codex session-end` {
		t.Errorf("commandWindows %v", got)
	}
	if _, err := codexTree(`C:\50%\bin\current.exe`, "1.2.3", nil); !errors.Is(err, ErrUnquotable) {
		t.Errorf("a %% shim on Windows: %v", err)
	}
	codexGOOS = "linux"
	if err := json.Unmarshal(genTree(t, "/tmp/50%/current", "1.2.3", nil)["plugins/backsight-hooks/hooks/hooks.json"], &doc); err != nil {
		t.Fatal(err)
	}
	if h := doc.Hooks["Stop"][0].Hooks[0]; h["commandWindows"] != nil || h["command"] != `'/tmp/50%/current' hook --harness codex stop` {
		t.Errorf("elsewhere: %v", h)
	}
}

// The hooks plugin's folder is byte-identical across two generations that
// differ only in the binary's version or a skill's text, and differs when
// a hook does (a moved shim); the main plugin follows the binary.
func TestCodexHooksPluginChangesOnlyWithItsHooks(t *testing.T) {
	hooksOf := func(tree map[string][]byte) map[string][]byte {
		out := map[string][]byte{}
		for p, raw := range tree {
			if strings.HasPrefix(p, "plugins/backsight-hooks/") {
				out[p] = raw
			}
		}
		return out
	}
	skills := map[string][]byte{"skills/pause/SKILL.md": []byte("one")}
	base := genTree(t, "/d/bin/current", "1.0.0", skills)
	if len(hooksOf(base)) != 2 {
		t.Fatalf("hooks plugin %v", hooksOf(base))
	}
	for name, other := range map[string]map[string][]byte{
		"binary": genTree(t, "/d/bin/current", "1.1.0", skills),
		"skill":  genTree(t, "/d/bin/current", "1.0.0", map[string][]byte{"skills/pause/SKILL.md": []byte("two")}),
	} {
		ch := codexChanged(base, other)
		if ch[CodexHooksPlugin] || !ch[CodexMainPlugin] {
			t.Errorf("%s: changed %v", name, ch)
		}
		if !mapsEqual(hooksOf(base), hooksOf(other)) {
			t.Errorf("%s: the hooks plugin's bytes changed", name)
		}
	}
	moved := genTree(t, "/elsewhere/bin/current", "1.0.0", skills)
	if ch := codexChanged(base, moved); !ch[CodexHooksPlugin] || pluginVersion(base, CodexHooksPlugin) == pluginVersion(moved, CodexHooksPlugin) {
		t.Errorf("a moved shim: changed %v, versions %s %s", ch, pluginVersion(base, CodexHooksPlugin), pluginVersion(moved, CodexHooksPlugin))
	}
	if pluginVersion(base, CodexMainPlugin) == pluginVersion(genTree(t, "/d/bin/current", "1.1.0", skills), CodexMainPlugin) {
		t.Error("the main plugin's version does not follow the binary")
	}
	if len(codexChanged(base, genTree(t, "/d/bin/current", "1.0.0", skills))) != 0 {
		t.Error("two identical generations differ")
	}
}

func mapsEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if string(b[k]) != string(v) {
			return false
		}
	}
	return true
}

// The tree is the one recorded working on Codex 0.167 with two plugins
// (checks/3-two-plugins/marketplace-files.txt), with Codex's own skills;
// its memory server takes the contract's args and passes BACKSIGHT_HOME.
func TestCodexTreeLayout(t *testing.T) {
	tree := genTree(t, "/d/bin/current", "1.0.0", Codex{}.Skills("/d/bin/current"))
	var got []string
	for p := range tree {
		got = append(got, p)
	}
	sort.Strings(got)
	raw, err := os.ReadFile(codexFixtures + "checks/3-two-plugins/marketplace-files.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Fields(string(raw))
	if !slices.Equal(got, want) {
		t.Errorf("tree\n%v\nwant\n%v", got, want)
	}
	var mcp struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			EnvVars []string `json:"env_vars"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(tree["plugins/backsight/.mcp.json"], &mcp); err != nil {
		t.Fatal(err)
	}
	m := mcp.Servers["backsight"]
	if len(mcp.Servers) != 1 || m.Command != "/d/bin/current" || !slices.Equal(m.Args, []string{"mcp", "--harness", "codex"}) || !slices.Equal(m.EnvVars, []string{"BACKSIGHT_HOME"}) {
		t.Errorf(".mcp.json %+v", mcp)
	}
	var mk struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name, Source string
		} `json:"plugins"`
	}
	if err := json.Unmarshal(tree[".claude-plugin/marketplace.json"], &mk); err != nil || mk.Name != "backsight" || len(mk.Plugins) != 2 ||
		mk.Plugins[1].Name != "backsight-hooks" || mk.Plugins[1].Source != "./plugins/backsight-hooks" {
		t.Errorf("marketplace %+v %v", mk, err)
	}
}

// The trust keys are the ones Codex wrote once the hooks plugin was
// trusted (checks/3-two-plugins/config-trusted.toml).
func TestCodexTrustKeysAreCodexs(t *testing.T) {
	raw, err := os.ReadFile(codexFixtures + "checks/3-two-plugins/config-trusted.toml")
	if err != nil {
		t.Fatal(err)
	}
	var recorded []string
	for _, m := range regexp.MustCompile(`\[hooks\.state\."(backsight-hooks@backsight:[^"]+)"\]`).FindAllStringSubmatch(string(raw), -1) {
		recorded = append(recorded, m[1])
	}
	got := CodexTrustKeys()
	sort.Strings(recorded)
	sort.Strings(got)
	if !slices.Equal(got, recorded) {
		t.Errorf("keys\n%v\nrecorded\n%v", got, recorded)
	}
}

// fakeCodex is the codex command's plugin and marketplace commands, with
// Codex's own answers (recorded: config/plugin-list.json and the probes of
// the block 3 report), on an in-memory install.
type fakeCodex struct {
	root      string            // marketplace root, "" when not added
	rootType  string            // its sourceType, "" for local
	installed map[string]string // plugin id → version
	disabled  map[string]bool
	ran       [][]string
	failOn    string
	mcp       string // codex mcp list --json
	features  string // codex features list
	env       [][]string
}

func (f *fakeCodex) run(_ context.Context, env []string, args ...string) ([]byte, error) {
	f.env = append(f.env, env)
	cmd := strings.Join(args, " ")
	if f.failOn != "" && strings.HasPrefix(cmd, f.failOn) {
		return nil, errors.New("codex " + cmd + ": failed")
	}
	switch cmd {
	case "--version":
		return os.ReadFile(codexFixtures + "config/codex-version.txt")
	case "plugin marketplace list --json":
		if f.root == "" {
			return []byte(`{"marketplaces": []}`), nil
		}
		return json.Marshal(map[string]any{"marketplaces": []any{map[string]any{"name": "backsight", "root": f.root, "marketplaceSource": map[string]string{"sourceType": cmp.Or(f.rootType, "local"), "source": f.root}}}})
	case "mcp list --json":
		return []byte(f.mcp), nil
	case "features list":
		return []byte(f.features), nil
	case "plugin list --json":
		var list []any
		for id, v := range f.installed {
			name, _, _ := strings.Cut(id, "@")
			list = append(list, map[string]any{"pluginId": id, "version": v, "installed": true, "enabled": !f.disabled[id],
				"source": map[string]string{"source": "local", "path": filepath.Join(f.root, "plugins", name)}})
		}
		return json.Marshal(map[string]any{"installed": list, "available": []any{}})
	}
	f.ran = append(f.ran, args)
	switch {
	case len(args) == 4 && cmd == "plugin marketplace add "+args[3]:
		if f.root != "" && f.root != args[3] {
			return nil, errors.New("marketplace 'backsight' is already added from a different source")
		}
		f.root = args[3]
	case cmd == "plugin marketplace remove backsight":
		if f.root == "" {
			return nil, errors.New("marketplace `backsight` is not configured or installed")
		}
		f.root = ""
	case len(args) == 3 && args[0] == "plugin" && args[1] == "add":
		name, _, _ := strings.Cut(args[2], "@")
		var m struct{ Version string }
		raw, err := os.ReadFile(filepath.Join(f.root, "plugins", name, ".claude-plugin", "plugin.json"))
		if err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &m)
		if f.installed == nil {
			f.installed = map[string]string{}
		}
		f.installed[args[2]] = m.Version
		delete(f.disabled, args[2])
	case len(args) == 3 && args[0] == "plugin" && args[1] == "remove":
		delete(f.installed, args[2])
	default:
		return nil, errors.New("unexpected codex " + cmd)
	}
	return nil, nil
}

func (f *fakeCodex) commands() []string {
	var out []string
	for _, a := range f.ran {
		out = append(out, strings.Join(a, " "))
	}
	f.ran = nil
	return out
}

func codexIntegration(t *testing.T, data, binary string) Integration {
	t.Helper()
	return Integration{Data: data, Shim: filepath.Join(data, "bin", "current"), BinaryVersion: binary}
}

func stepNames(r InstallResult) []string {
	var out []string
	for _, s := range r.Steps {
		if s.Changed {
			out = append(out, s.Name+": "+s.Detail)
		}
	}
	return out
}

// Install: the marketplace and both plugins the first time, the trust
// step with them; nothing the second; the main plugin alone for a new
// binary (the hooks stay trusted); both again for a moved data directory.
func TestCodexInstall(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	f := &fakeCodex{}
	env := Env{Home: home, Run: f.run}
	c := Codex{}
	mkt := CodexMarketplaceDir(data)

	res, err := c.Install(context.Background(), env, codexIntegration(t, data, "1.0.0"), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.commands(); !slices.Equal(got, []string{"plugin marketplace add " + mkt, "plugin add backsight@backsight", "plugin add backsight-hooks@backsight"}) {
		t.Errorf("first install ran %v", got)
	}
	if res.Version != CodexVersion("1.0.0", filepath.Join(data, "bin", "current")) || !hasStep(res, "trust") || !hasStep(res, "plugin") {
		t.Errorf("first install: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(mkt, "plugins", "backsight-hooks", "hooks", "hooks.json")); err != nil {
		t.Errorf("the tree is not written: %v", err)
	}

	res, err = c.Install(context.Background(), env, codexIntegration(t, data, "1.0.0"), false)
	if err != nil || len(f.commands()) != 0 || len(stepNames(res)) != 0 {
		t.Errorf("second install: %v %v", stepNames(res), err)
	}

	res, err = c.Install(context.Background(), env, codexIntegration(t, data, "1.1.0"), false)
	if got := f.commands(); err != nil || !slices.Equal(got, []string{"plugin add backsight@backsight"}) || hasStep(res, "trust") {
		t.Errorf("new binary: ran %v, steps %v, %v", got, stepNames(res), err)
	}

	// Disabled in Codex: added again, which enables it.
	f.disabled = map[string]bool{"backsight-hooks@backsight": true}
	if _, err := c.Install(context.Background(), env, codexIntegration(t, data, "1.1.0"), false); err != nil || !slices.Equal(f.commands(), []string{"plugin add backsight-hooks@backsight"}) {
		t.Errorf("disabled plugin: %v", err)
	}

	// The data directory moved: the old marketplace root is gone.
	moved := filepath.Join(home, "moved")
	if err := os.Rename(data, moved); err != nil {
		t.Fatal(err)
	}
	res, err = c.Install(context.Background(), env, codexIntegration(t, moved, "1.1.0"), false)
	want := []string{"plugin marketplace remove backsight", "plugin marketplace add " + CodexMarketplaceDir(moved), "plugin add backsight@backsight", "plugin add backsight-hooks@backsight"}
	if got := f.commands(); err != nil || !slices.Equal(got, want) || !hasStep(res, "trust") {
		t.Errorf("moved: ran %v, %v", got, err)
	}
	// The default home: no CODEX_HOME given to codex.
	for _, e := range f.env {
		if len(e) != 0 {
			t.Errorf("env %v for ~/.codex", e)
		}
	}
}

func hasStep(r InstallResult, name string) bool {
	for _, s := range r.Steps {
		if s.Name == name && s.Changed {
			return true
		}
	}
	return false
}

// A plan writes nothing and runs only Codex's list commands; a Codex home
// wired to another install is a conflict; a failed add says how far it went.
func TestCodexInstallPlanConflictAndFailure(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	f := &fakeCodex{}
	env := Env{Home: home, Profile: filepath.Join(home, "codexhome"), Run: f.run}
	res, err := Codex{}.Install(context.Background(), env, codexIntegration(t, data, "1.0.0"), true)
	if err != nil || len(f.ran) != 0 {
		t.Fatalf("plan ran %v: %v", f.ran, err)
	}
	if _, err := os.Stat(CodexMarketplaceDir(data)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a plan wrote the tree")
	}
	if len(stepNames(res)) != 5 || !hasStep(res, "trust") {
		t.Errorf("plan steps %v", stepNames(res))
	}
	for _, e := range f.env {
		if !slices.Equal(e, []string{"CODEX_HOME=" + env.Profile}) {
			t.Errorf("env %v, want the recorded home", e)
		}
	}

	other := filepath.Join(home, "other", "codex-marketplace")
	if err := os.MkdirAll(filepath.Join(other, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.root = other
	_, err = Codex{}.Install(context.Background(), env, codexIntegration(t, data, "1.0.0"), false)
	var ie *InstallError
	if !errors.As(err, &ie) || ie.Stage != Conflict || !strings.Contains(err.Error(), other) || len(f.ran) != 0 {
		t.Errorf("conflict: %v, ran %v", err, f.ran)
	}
	// This install's own Claude Code marketplace, added by hand: replaced.
	claudeMkt := filepath.Join(data, "marketplace")
	if err := os.MkdirAll(filepath.Join(claudeMkt, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.root = claudeMkt
	if _, err := (Codex{}).Install(context.Background(), env, codexIntegration(t, data, "1.0.0"), false); err != nil || f.commands()[0] != "plugin marketplace remove backsight" {
		t.Errorf("own Claude Code marketplace: %v", err)
	}

	f2 := &fakeCodex{failOn: "plugin add backsight-hooks"}
	_, err = Codex{}.Install(context.Background(), Env{Home: home, Run: f2.run}, codexIntegration(t, filepath.Join(home, "d2"), "1.0.0"), false)
	if !errors.As(err, &ie) || ie.Stage != Registering {
		t.Errorf("failed add: %v", err)
	}
	if _, err := (Codex{}).Install(context.Background(), Env{Home: home}, codexIntegration(t, data, "1.0.0"), false); !errors.Is(err, ErrMissing) {
		t.Errorf("no runner: %v", err)
	}
}

// Uninstall removes what is installed and the marketplace, then the empty
// cache folder Codex leaves; a cache still holding a file stays.
func TestCodexUninstallAndInstalled(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	f := &fakeCodex{}
	env := Env{Home: home, Run: f.run}
	c := Codex{}
	if there, err := c.Installed(context.Background(), env); err != nil || there {
		t.Fatalf("installed before: %v %v", there, err)
	}
	if _, err := c.Install(context.Background(), env, codexIntegration(t, data, "1.0.0"), false); err != nil {
		t.Fatal(err)
	}
	f.commands()
	if there, err := c.Installed(context.Background(), env); err != nil || !there {
		t.Fatalf("installed: %v %v", there, err)
	}
	cache := filepath.Join(home, ".codex", "plugins", "cache", "backsight")
	if err := os.MkdirAll(filepath.Join(cache, "backsight-hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Uninstall(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got := f.commands(); !slices.Equal(got, []string{"plugin remove backsight@backsight", "plugin remove backsight-hooks@backsight", "plugin marketplace remove backsight"}) {
		t.Errorf("uninstall ran %v", got)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the empty cache folder stays: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "plugins", "cache")); err != nil {
		t.Errorf("Codex's own cache folder went: %v", err)
	}
	if there, _ := c.Installed(context.Background(), env); there {
		t.Error("still installed")
	}
	// Nothing left: nothing run; a cache holding a file stays.
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(cache, "kept"), nil, 0o600)
	if err := c.Uninstall(context.Background(), env); err != nil || len(f.commands()) != 0 {
		t.Errorf("second uninstall: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Error("a cache folder holding a file was removed")
	}
}

func TestCodexDetect(t *testing.T) {
	f := &fakeCodex{}
	if v, err := (Codex{}).Detect(context.Background(), Env{Run: f.run}); err != nil || v != "0.167.0" {
		t.Errorf("%q %v", v, err)
	}
	odd := func(context.Context, []string, ...string) ([]byte, error) { return []byte("something else\n"), nil }
	if _, err := (Codex{}).Detect(context.Background(), Env{Run: odd}); err == nil {
		t.Error("an unexpected answer read as a version")
	}
	missing := func(context.Context, []string, ...string) ([]byte, error) { return nil, MissingError{Command: "codex"} }
	if _, err := (Codex{}).Detect(context.Background(), Env{Run: missing}); !errors.Is(err, ErrMissing) {
		t.Errorf("missing: %v", err)
	}
}

// The home: the flag, else $CODEX_HOME when set and non-empty, else
// ~/.codex; a named one must exist; recorded "" for the default.
func TestCodexHome(t *testing.T) {
	c := Codex{}
	home := t.TempDir()
	flag, shell := filepath.Join(home, "flag"), filepath.Join(home, "shell")
	_ = os.MkdirAll(flag, 0o700)
	_ = os.MkdirAll(shell, 0o700)
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "CODEX_HOME" {
				return v
			}
			return "x"
		}
	}
	for _, tc := range []struct {
		flag, shell, want string
	}{
		{flag, shell, flag},
		{"", shell, shell},
		{"", "", filepath.Join(home, ".codex")},
	} {
		if got, err := c.ShellHome(home, tc.flag, env(tc.shell)); err != nil || got != tc.want {
			t.Errorf("flag %q shell %q: %q %v", tc.flag, tc.shell, got, err)
		}
	}
	if _, err := c.ShellHome(home, filepath.Join(home, "nope"), env("")); err == nil || !strings.Contains(err.Error(), "--codex-home") {
		t.Errorf("a missing flag home: %v", err)
	}
	if _, err := c.ShellHome(home, "", env(filepath.Join(home, "nope"))); err == nil || !strings.Contains(err.Error(), "CODEX_HOME") {
		t.Errorf("a missing CODEX_HOME: %v", err)
	}
	var st state.State
	if err := c.RecordHome(&st, flag); err != nil || st.ClientOf(harnessid.Codex).Profile != flag || st.ClientOf(harnessid.ClaudeCode) != nil {
		t.Errorf("recorded %+v %v", st, err)
	}
	if got, err := c.RecordedHome(st); err != nil || got != flag {
		t.Errorf("read back %q %v", got, err)
	}
	if err := c.RecordHome(&st, "relative/dir"); err == nil {
		t.Error("a relative home recorded")
	}
	if got, _ := c.RecordedHome(state.State{}); got != "" {
		t.Errorf("nothing recorded: %q", got)
	}
	if !c.IsDefaultProfile(home, "") || !c.IsDefaultProfile(home, filepath.Join(home, ".codex")) || c.IsDefaultProfile(home, flag) {
		t.Error("IsDefaultProfile")
	}
	if l := c.Locate(Env{Home: home}); l.Settings != filepath.Join(home, ".codex", "config.toml") || l.Conversations != filepath.Join(home, ".codex", "sessions") {
		t.Errorf("Locate %+v", l)
	}
	// The recorded home's sessions/ are the conversations (block 6 reads them).
	if l := c.Locate(Env{Home: home, Profile: flag}); l.Dir != flag || l.Settings != filepath.Join(flag, "config.toml") || l.Conversations != filepath.Join(flag, "sessions") {
		t.Errorf("Locate %+v", l)
	}
	if c.ProfileEnv(home, "") != nil || !slices.Equal(c.ProfileEnv(home, flag), []string{"CODEX_HOME=" + flag}) {
		t.Error("ProfileEnv")
	}
}

// Drift: none for an install as recorded with every hook trusted; the
// version for another binary, the files edited, a missing shim, the hooks
// Codex has not trusted.
func TestCodexDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a symlink here")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	f := &fakeCodex{}
	env := Env{Home: home, Run: f.run}
	in := codexIntegration(t, data, "1.0.0")
	res, err := Codex{}.Install(context.Background(), env, in, false)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(data, "bin", "1.0.0-abc", "backsight")
	_ = os.MkdirAll(filepath.Dir(target), 0o755)
	_ = os.WriteFile(target, nil, 0o755)
	if err := os.Symlink(target, in.Shim); err != nil {
		t.Fatal(err)
	}
	trusted, err := os.ReadFile(codexFixtures + "checks/3-two-plugins/config-trusted.toml")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	settings := filepath.Join(home, ".codex", "config.toml")
	if err := os.WriteFile(settings, trusted, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := Recorded{Version: res.Version, ShimTarget: target, Data: data, BinaryVersion: "1.0.0"}
	if d := (Codex{}).Drift(env, rec, nil); d != nil {
		t.Errorf("drift on a clean install: %v", d)
	}
	if d := (Codex{}).Drift(env, Recorded{}, nil); d != nil {
		t.Errorf("never installed: %v", d)
	}
	newer := rec
	newer.BinaryVersion = "1.1.0"
	if d := (Codex{}).Drift(env, newer, nil); len(d) != 1 || !strings.Contains(d[0], "not this binary's") {
		t.Errorf("another binary: %v", d)
	}
	hooks := filepath.Join(CodexMarketplaceDir(data), "plugins", "backsight-hooks", "hooks", "hooks.json")
	_ = os.WriteFile(hooks, []byte("{}"), 0o644)
	if d := (Codex{}).Drift(env, rec, nil); len(d) != 1 || !strings.Contains(d[0], "files in") {
		t.Errorf("edited files: %v", d)
	}
	_ = os.Remove(target)
	start, _ := os.ReadFile(codexFixtures + "config/config-start.toml")
	_ = os.WriteFile(settings, start, 0o600)
	d := (Codex{}).Drift(env, rec, nil)
	if len(d) != 3 || !strings.Contains(d[1], "missing") || !strings.Contains(d[2], "9 of backsight's hooks") {
		t.Errorf("missing shim, untrusted: %v", d)
	}
	if got := (Codex{}).Untrusted(env); len(got) != 9 {
		t.Errorf("untrusted %v", got)
	}
}

// A server of the user's named memory is a clash, named, whether config.toml
// or `codex mcp list` shows it; backsight's own server is not.
func TestCodexNameClash(t *testing.T) {
	home := t.TempDir()
	shim := filepath.Join(home, "data", "bin", "current")
	f := &fakeCodex{mcp: "[]"}
	env := Env{Home: home, Run: f.run}
	c := Codex{}
	if got, err := c.NameClash(context.Background(), env, shim); err != nil || got != "" {
		t.Errorf("no server: %q %v", got, err)
	}
	quoted, _ := json.Marshal(shim)
	ours := `[{"name":"backsight","transport":{"type":"stdio","command":` + string(quoted) + `,"args":["mcp","--harness","codex"]}}]`
	f.mcp = ours
	if got, err := c.NameClash(context.Background(), env, shim); err != nil || got != "" {
		t.Errorf("backsight's own: %q %v", got, err)
	}
	f.mcp = `[{"name":"probe","transport":{"type":"stdio","command":"/x"}},{"name":"backsight","transport":{"type":"stdio","command":"/usr/bin/mem","args":["serve"]}}]`
	if got, err := c.NameClash(context.Background(), env, shim); err != nil || !strings.Contains(got, "/usr/bin/mem") {
		t.Errorf("the user's: %q %v", got, err)
	}
	f.mcp = `[{"name":"backsight","transport":{"type":"streamable_http","url":"https://mem.example/mcp"}}]`
	if got, _ := c.NameClash(context.Background(), env, shim); !strings.Contains(got, "https://mem.example/mcp") {
		t.Errorf("an HTTP one: %q", got)
	}
	// A server of the user's named memory, Claude Code's server name, is
	// no clash under Codex: backsight's is named backsight there.
	f.mcp = `[{"name":"memory","transport":{"type":"stdio","command":"/usr/bin/mem"}}]`
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("[mcp_servers.memory]\ncommand = \"/usr/bin/mem\"\n"), 0o600)
	if got, err := c.NameClash(context.Background(), env, shim); err != nil || got != "" {
		t.Errorf("a user server named memory: %q %v", got, err)
	}
	f.mcp = ours
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("[mcp_servers.backsight]\ncommand = \"/usr/bin/mem\"\n"), 0o600)
	if got, err := c.NameClash(context.Background(), env, shim); err != nil || !strings.Contains(got, "[mcp_servers.backsight]") {
		t.Errorf("config.toml: %q %v", got, err)
	}
	f.mcp = "not json"
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), nil, 0o600)
	if _, err := c.NameClash(context.Background(), env, shim); err == nil {
		t.Error("an unreadable list passed")
	}
}

// The effective state of Codex's memories, from `codex features list`.
func TestCodexMemoriesOn(t *testing.T) {
	raw, err := os.ReadFile(codexFixtures + "config/features.txt")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCodex{features: string(raw)}
	if on, err := (Codex{}).MemoriesOn(context.Background(), Env{Run: f.run}); err != nil || on {
		t.Errorf("recorded (off): %v %v", on, err)
	}
	f.features = regexp.MustCompile(`(?m)^(memories\s+stable\s+)false$`).ReplaceAllString(string(raw), "${1}true")
	if on, err := (Codex{}).MemoriesOn(context.Background(), Env{Run: f.run}); err != nil || !on {
		t.Errorf("on: %v %v", on, err)
	}
	f.features = "hooks stable true\n"
	if _, err := (Codex{}).MemoriesOn(context.Background(), Env{Run: f.run}); err == nil {
		t.Error("no row read as off")
	}
}

// A marketplace of the user's named backsight (a git one, or a local folder
// backsight did not generate, even one gone) is never removed: install
// refuses, naming it; uninstall and Installed leave it and its plugins.
func TestCodexForeignMarketplace(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	for name, f := range map[string]*fakeCodex{
		"git":        {root: "https://github.com/someone/backsight", rootType: "git", installed: map[string]string{"backsight@backsight": "2.0"}},
		"local gone": {root: filepath.Join(home, "my-plugins"), installed: map[string]string{"backsight@backsight": "2.0"}},
	} {
		t.Run(name, func(t *testing.T) {
			env := Env{Home: home, Run: f.run}
			_, err := Codex{}.Install(context.Background(), env, codexIntegration(t, data, "1.0.0"), false)
			var ie *InstallError
			if !errors.As(err, &ie) || ie.Stage != Conflict || !strings.Contains(err.Error(), f.root) || !strings.Contains(err.Error(), "did not make") || len(f.ran) != 0 {
				t.Errorf("install: %v, ran %v", err, f.ran)
			}
			if there, err := (Codex{}).Installed(context.Background(), env); err != nil || there {
				t.Errorf("installed: %v %v", there, err)
			}
			if err := (Codex{}).Uninstall(context.Background(), env); err != nil || len(f.ran) != 0 {
				t.Errorf("uninstall ran %v: %v", f.ran, err)
			}
		})
	}
}

// The name the generator gives the server is the one a real Codex 0.167.0
// listed, offered the model, and named the tools after in its PostToolUse
// payloads, recorded with the generated plugins (server-name/).
func TestCodexServerNameIsTheRecordedOne(t *testing.T) {
	read := func(name string) []byte {
		raw, err := os.ReadFile(codexFixtures + "server-name/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var listed []struct{ Name string }
	if err := json.Unmarshal(read("mcp-list.json"), &listed); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(listed, func(s struct{ Name string }) bool { return s.Name == CodexServer }) {
		t.Errorf("codex mcp list: %+v", listed)
	}
	var offered []struct {
		Name  string
		Tools []string
	}
	if err := json.Unmarshal(read("model-namespaces.json"), &offered); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ns := range offered {
		if ns.Name+"__" == (Codex{}).ToolPrefix() {
			found = true
			var full []string
			for _, tool := range ns.Tools {
				full = append(full, ns.Name+"__"+tool)
			}
			sort.Strings(full)
			want := Codex{}.ToolNames()
			sort.Strings(want)
			if !slices.Equal(full, want) {
				t.Errorf("tools offered %v, want %v", full, want)
			}
		}
	}
	if !found {
		t.Errorf("no namespace %s offered: %+v", Codex{}.ToolPrefix(), offered)
	}
	var matcher string
	for _, e := range (Codex{}).Events() {
		if e.Name == "PostToolUse" {
			matcher = e.Matcher
		}
	}
	post := regexp.MustCompile("^(?:" + matcher + ")$")
	for _, line := range strings.Split(strings.TrimSpace(string(read("post-tool-use-names.txt"))), "\n") {
		if f := strings.Fields(line); len(f) != 2 || !post.MatchString(f[1]) || !slices.Contains(Codex{}.ToolNames(), f[1]) {
			t.Errorf("recorded PostToolUse %q", line)
		}
	}
}

// An install generated with the server named memory (before 2026-11-11):
// the next install (an update's) adds both plugins again, the hooks
// plugin's matcher now mcp__backsight__.*, and says Codex asks to review
// the hooks once.
func TestCodexUpdateFromTheMemoryServerName(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	f := &fakeCodex{}
	env := Env{Home: home, Run: f.run}
	in := codexIntegration(t, data, "1.0.0")
	if _, err := (Codex{}).Install(context.Background(), env, in, false); err != nil {
		t.Fatal(err)
	}
	// The tree as the earlier generator wrote it: the server key and the
	// matcher of the old name, the hooks plugin's version of those bytes.
	mkt := CodexMarketplaceDir(data)
	rewrite := func(rel, from, to string) {
		p := filepath.Join(mkt, filepath.FromSlash(rel))
		raw, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(raw), from) {
			t.Fatalf("%s: %q not found (%v)", rel, from, err)
		}
		_ = os.WriteFile(p, []byte(strings.ReplaceAll(string(raw), from, to)), 0o644)
	}
	rewrite("plugins/backsight/.mcp.json", `"backsight": {`, `"memory": {`)
	rewrite("plugins/backsight-hooks/hooks/hooks.json", "mcp__backsight__.*", "mcp__memory__.*")
	old, _ := os.ReadFile(filepath.Join(mkt, "plugins", "backsight-hooks", "hooks", "hooks.json"))
	newVersion := pluginVersion(genTree(t, in.Shim, "1.0.0", Codex{}.Skills(in.Shim)), CodexHooksPlugin)
	rewrite("plugins/backsight-hooks/.claude-plugin/plugin.json", newVersion, codexHooksVersion(old))
	f.installed["backsight-hooks@backsight"] = codexHooksVersion(old)
	f.commands()

	res, err := (Codex{}).Install(context.Background(), env, in, false)
	if got := f.commands(); err != nil || !slices.Equal(got, []string{"plugin add backsight@backsight", "plugin add backsight-hooks@backsight"}) {
		t.Fatalf("update ran %v: %v", got, err)
	}
	if !hasStep(res, "trust") {
		t.Errorf("no trust review said: %v", stepNames(res))
	}
	raw, _ := os.ReadFile(filepath.Join(mkt, "plugins", "backsight-hooks", "hooks", "hooks.json"))
	if !strings.Contains(string(raw), `"matcher": "mcp__backsight__.*"`) || strings.Contains(string(raw), "mcp__memory__") {
		t.Errorf("hooks.json after the update:\n%s", raw)
	}
}
