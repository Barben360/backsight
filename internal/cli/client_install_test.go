package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/fsutil"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/testtool"
)

// fakeClaude records claude CLI calls and answers the list commands.
type fakeClaude struct {
	calls        [][]string
	envs         [][]string
	ran          []claudeCall
	version      string
	marketplaces string
	plugins      string
	missing      bool
	failOn       string // a command line that fails
	failOnly     int    // with failOn: fail that many times, then pass (0: always)
	failed       int
	// onRun, when set, sees each command line with the profile it acts on
	// ("" is ~/.claude): a test makes Claude Code's side effects with it.
	onRun func(line, configDir string)
}

func (f *fakeClaude) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	if f.missing {
		return nil, errClaudeMissing
	}
	f.calls = append(f.calls, args)
	f.envs = append(f.envs, env)
	f.ran = append(f.ran, claudeCall{strings.Join(args, " "), configDirIn(claudeEnviron(env))})
	if f.onRun != nil {
		f.onRun(strings.Join(args, " "), configDirIn(claudeEnviron(env)))
	}
	if f.failOn != "" && strings.HasPrefix(strings.Join(args, " "), f.failOn) && (f.failOnly == 0 || f.failed < f.failOnly) {
		f.failed++
		return nil, errors.New("claude failed: " + f.failOn)
	}
	switch strings.Join(args, " ") {
	case "--version":
		return []byte(f.version + " (Claude Code)\n"), nil
	case "plugin marketplace list --json":
		return []byte(f.marketplaces), nil
	case "plugin list --json":
		return []byte(f.plugins), nil
	case "plugin enable " + plugin.PluginID() + " --scope user":
		f.plugins = strings.ReplaceAll(f.plugins, `"enabled":false`, `"enabled":true`)
	}
	return []byte("ok\n"), nil
}

// claudeCall is one claude command line and the CLAUDE_CONFIG_DIR the real
// runner would have given it ("" unset): the profile it acts on.
type claudeCall struct{ line, configDir string }

// profilesOf are the CLAUDE_CONFIG_DIR values the calls starting with
// prefix ran with.
func (f *fakeClaude) profilesOf(prefix string) []string {
	var out []string
	for _, c := range f.ran {
		if strings.HasPrefix(c.line, prefix) {
			out = append(out, c.configDir)
		}
	}
	return out
}

// configDirIn is the CLAUDE_CONFIG_DIR a process with environ sees (the
// last entry wins, as with exec.Cmd).
func configDirIn(environ []string) string {
	dir := ""
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			dir = v
		}
	}
	return dir
}

func withClaude(t *testing.T) *fakeClaude {
	t.Helper()
	f := &fakeClaude{version: "2.1.310", marketplaces: "[]", plugins: "[]"}
	old := claudeRunner
	claudeRunner = f
	t.Cleanup(func() { claudeRunner = old })
	return f
}

func mutating(f *fakeClaude) []string {
	var out []string
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if j != "--version" && !strings.HasSuffix(j, "list --json") {
			out = append(out, j)
		}
	}
	return out
}

func TestClientInstallFirstTime(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	_ = os.MkdirAll(filepath.Dir(settings), 0o700)
	_ = os.WriteFile(settings, []byte("{\n  \"permissions\": {\n    \"allow\": [\n      \"Read\"\n    ]\n  }\n}\n"), 0o644)
	code, out, _ := run(t, "client", "install")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	d := pathsOf(home)
	mkt := filepath.Join(d.Data, "marketplace")
	got := mutating(f)
	if len(got) != 2 || got[0] != "plugin marketplace add "+mkt || got[1] != "plugin install "+plugin.PluginID()+" --scope user" {
		t.Errorf("claude calls %q", got)
	}
	shim := filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS))
	target, err := filepath.EvalSymlinks(shim)
	// EvalSymlinks gives long names; a TEMP in 8.3 form (RUNNER~1) spells d.Data short
	if err != nil || !strings.HasPrefix(fsutil.Normal(target), fsutil.Normal(filepath.Join(d.Data, "bin"))) {
		t.Fatalf("shim %s -> %s %v", shim, target, err)
	}
	if !fsutil.Executable(target) {
		t.Errorf("shim target not executable: %s", target)
	}
	hooks, _ := os.ReadFile(filepath.Join(mkt, "plugins", "backsight", "hooks", "hooks.json"))
	if !strings.Contains(string(hooks), `"command": "`+testtool.JSONPath(shim)+`"`) {
		t.Errorf("hooks do not exec the shim:\n%.300s", hooks)
	}
	s, _ := os.ReadFile(settings)
	for _, tool := range plugin.ToolNames {
		if !strings.Contains(string(s), `"`+tool+`"`) {
			t.Errorf("permission rule missing %s:\n%s", tool, s)
		}
	}
	if !strings.Contains(string(s), `"Read",`) {
		t.Errorf("existing rules must stay:\n%s", s)
	}
	st, _ := state.Load(d.StateFile())
	if st.PluginVersion == "" || !shimRuns(target, st.ShimTarget) {
		t.Errorf("state %+v", st)
	}
}

func TestClientInstallAgainUpdatesInsteadOfAdding(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	run(t, "client", "install")
	f.calls = nil
	f.marketplaces = `[{"name":"backsight","source":"directory","path":"` + testtool.JSONPath(filepath.Join(pathsOf(home).Data, "marketplace")) + `"}]`
	f.plugins = `[{"id":"backsight@backsight","version":"x","scope":"user","enabled":true}]`
	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	got := mutating(f)
	if len(got) != 2 || got[0] != "plugin marketplace update backsight" || got[1] != "plugin update "+plugin.PluginID() {
		t.Errorf("second install %q", got)
	}
	settings, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if n := strings.Count(string(settings), plugin.ToolNames[0]); n != 1 {
		t.Errorf("permission rule appended %d times", n)
	}
}

func TestClientInstallPlanTouchesNothing(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	code, out, _ := run(t, "client", "install", "--plan", "--json")
	if code != output.OK || len(mutating(f)) != 0 {
		t.Fatalf("exit %d, calls %q\n%s", code, mutating(f), out)
	}
	if _, err := os.Stat(filepath.Join(pathsOf(home).Data, "marketplace")); !os.IsNotExist(err) {
		t.Error("plan generated the marketplace")
	}
	var env struct {
		Data clientInstallResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if len(env.Data.Steps) < 4 {
		t.Errorf("plan steps %+v", env.Data.Steps)
	}
}

func TestClientInstallNeedsTheClaudeCLI(t *testing.T) {
	devInstall(t, true)
	f := withClaude(t)
	f.missing = true
	if code, out, _ := run(t, "client", "install", "--json"); code != output.NeedsUser || !strings.Contains(out, "claude") {
		t.Errorf("missing CLI: exit %d\n%s", code, out)
	}
	f.missing, f.version = false, "2.0.1"
	if code, out, errOut := run(t, "client", "install"); code != output.NeedsUser || !strings.Contains(out+errOut, "2.0.1") {
		t.Errorf("old CLI: exit %d\n%s%s", code, out, errOut)
	}
}

func TestClientInstallIntoAnotherProfile(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	profile := filepath.Join(home, "claude-dev")
	if code, out, _ := run(t, "client", "install", "--config-dir", profile); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for i, c := range f.calls {
		if strings.Join(c, " ") == "--version" {
			continue
		}
		if !contains(f.envs[i], "CLAUDE_CONFIG_DIR="+profile) {
			t.Errorf("%v ran without the profile: %v", c, f.envs[i])
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(profile, "settings.json")); !strings.Contains(string(raw), plugin.ToolNames[0]) {
		t.Error("permission rule not in the profile's settings")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Error("the default profile was touched")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestClientInstallLeavesNoDriftAndRecordsTheProfile(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	profile := filepath.Join(home, "claude-dev")
	if code, out, _ := run(t, "client", "install", "--config-dir", profile); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	d := pathsOf(home)
	st, _ := state.Load(d.StateFile())
	if st.Client == nil || st.Client.Profile != profile {
		t.Fatalf("profile not recorded: %+v", st.Client)
	}
	if drift := plugin.Drift(d.Data, filepath.Join(profile, "settings.json"), plugin.Installed{Version: st.PluginVersion, ShimTarget: st.ShimTarget}, "dev"); len(drift) != 0 {
		t.Errorf("drift right after install: %v", drift)
	}
	// doctor checks the recorded profile, not the shell's default one
	_, out, _ := run(t, "doctor")
	if !strings.Contains(out, "permission rule in place in "+profile) {
		t.Errorf("doctor:\n%s", out)
	}
}

// A profile given by the shell's CLAUDE_CONFIG_DIR is recorded too: the
// scheduled jobs have no such variable and read their transcripts from it.
func TestClientInstallRecordsTheProfileFromTheEnvironment(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	profile := filepath.Join(home, "claude-dev")
	t.Setenv("CLAUDE_CONFIG_DIR", profile)
	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.Client == nil || st.Client.Profile != profile {
		t.Errorf("profile from CLAUDE_CONFIG_DIR not recorded: %+v", st.Client)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.Client == nil || st.Client.Profile != "" {
		t.Errorf("~/.claude is recorded as the default, \"\": %+v", st.Client)
	}
}

func TestClientInstallShimTargetIsAVerifiedCopy(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	run(t, "client", "install")
	d := pathsOf(home)
	st, _ := state.Load(d.StateFile())
	exe, _ := os.Executable()
	want, _ := fsutil.HashFile(exe)
	if !strings.HasPrefix(st.ShimTarget, filepath.Join(d.Data, "bin", "dev-"+want[:12])+string(filepath.Separator)) {
		t.Errorf("target %s", st.ShimTarget)
	}
	// a damaged copy (crash, full disk) is replaced on the next install
	_ = os.WriteFile(st.ShimTarget, nil, 0o755)
	run(t, "client", "install")
	if got, _ := fsutil.HashFile(st.ShimTarget); got != want {
		t.Error("a damaged binary was kept")
	}
}

// shimRuns: the shim (resolved) is the target, or on Windows a copy of it.
func shimRuns(shim, target string) bool {
	if runtime.GOOS != "windows" {
		return shim == target
	}
	a, err1 := os.ReadFile(shim)
	b, err2 := os.ReadFile(target)
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}

func TestClientInstallOnANewVersionSwitchesTheShim(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	setBuild(t, "1.0.0", "a")
	run(t, "client", "install")
	d := pathsOf(home)
	first, _ := state.Load(d.StateFile())
	setBuild(t, "1.1.0", "b")
	run(t, "client", "install")
	st, _ := state.Load(d.StateFile())
	got, _ := filepath.EvalSymlinks(filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS)))
	want, _ := filepath.EvalSymlinks(st.ShimTarget)
	if st.ShimTarget == first.ShimTarget || !shimRuns(got, want) || !strings.HasPrefix(st.PluginVersion, "1.1.0+") ||
		st.Client == nil || st.Client.ShimPrevious != first.ShimTarget {
		t.Errorf("state %+v (client %+v), shim -> %s", st, st.Client, got)
	}
	if _, err := os.Stat(first.ShimTarget); err != nil {
		t.Error("the previous binary was removed: no rollback")
	}
}

func TestClientInstallMovedDataDirReaddsTheMarketplace(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	f.marketplaces = `[{"name":"backsight","source":"directory","path":"/old/place/marketplace"}]`
	run(t, "client", "install")
	got := mutating(f)
	mkt := filepath.Join(pathsOf(home).Data, "marketplace")
	if len(got) < 2 || got[0] != "plugin marketplace remove backsight" || got[1] != "plugin marketplace add "+mkt {
		t.Errorf("calls %q", got)
	}
}

func TestClientInstallMarketplaceKnownPluginNot(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	f.marketplaces = `[{"name":"backsight","source":"directory","path":"` + testtool.JSONPath(filepath.Join(pathsOf(home).Data, "marketplace")) + `"}]`
	run(t, "client", "install")
	if got := mutating(f); len(got) != 2 || got[0] != "plugin marketplace update backsight" || got[1] != "plugin install backsight@backsight --scope user" {
		t.Errorf("calls %q", got)
	}
}

func TestClientInstallHalfwayFailureThenRerun(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	f.failOn = "plugin install"
	if code, _, _ := run(t, "client", "install"); code != output.Partial {
		t.Fatalf("exit %d, want 40", code)
	}
	f.failOn, f.calls = "", nil
	f.marketplaces = `[{"name":"backsight","source":"directory","path":"` + testtool.JSONPath(filepath.Join(pathsOf(home).Data, "marketplace")) + `"}]`
	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatalf("rerun: exit %d\n%s", code, out)
	}
	if got := mutating(f); got[len(got)-1] != "plugin install backsight@backsight --scope user" {
		t.Errorf("rerun calls %q", got)
	}
}

func TestClientInstallPlanWritesNothingAtAll(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	code, out, _ := run(t, "client", "install", "--plan", "--json")
	var env struct {
		Data clientInstallResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d %v\n%s", code, err, out)
	}
	d := pathsOf(home)
	for _, p := range []string{filepath.Join(d.Data, "bin"), filepath.Join(home, ".claude", "settings.json"), d.StateFile()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("plan wrote %s", p)
		}
	}
	names := map[string]string{}
	for _, s := range env.Data.Steps {
		names[s.Name] = s.Status
	}
	if names["shim"] != "planned" || names["plugin"] != "planned" || names["permission rule"] != "planned" {
		t.Errorf("steps %+v", env.Data.Steps)
	}
}

func TestClientInstallVersionBoundaryAndActions(t *testing.T) {
	devInstall(t, true)
	f := withClaude(t)
	f.version = "2.1.309"
	if code, _, _ := run(t, "client", "install"); code != output.OK {
		t.Errorf("2.1.309 (the minimum): exit %d", code)
	}
	f.version = "2.1.308"
	code, out, _ := run(t, "client", "install", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "npm install -g @anthropic-ai/claude-code") || !strings.Contains(out, `"resume"`) {
		t.Errorf("2.1.308: exit %d\n%s", code, out)
	}
}

func TestClientInstallRefusesUnreadableClaudeOutputAndConcurrency(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	f.plugins = "Warning: not json"
	// the shim and plugin are already rewritten when claude's lists are read: partial
	if code, out, _ := run(t, "client", "install", "--json"); code != output.Partial || !strings.Contains(out, "unexpected output") {
		t.Errorf("exit %d\n%s", code, out)
	}
	f.plugins = "[]"
	d := pathsOf(home)
	l, err := state.TryAcquire(filepath.Join(d.LocksDir(), "client-install.lock"), "other")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if code, out, _ := run(t, "client", "install", "--json"); code != output.Blocked || !strings.Contains(out, "another client install") {
		t.Errorf("concurrent: exit %d\n%s", code, out)
	}
}

// The real runner parses stdout only: a warning on stderr must not break
// the JSON (block-3 review, proven).
func TestExecClaudeIgnoresStderrWarnings(t *testing.T) {
	bin := t.TempDir()
	testtool.Write(t, bin, "claude", testtool.Fake{Script: "stderr Warning: something\n1=--version stdout 2.1.310 (Claude Code)\n1!=--version stdout []"})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := execClaude{}.Run(context.Background(), nil, "plugin", "list", "--json")
	if err != nil || strings.TrimSpace(string(out)) != "[]" {
		t.Errorf("%q %v", out, err)
	}
	v, _ := execClaude{}.Run(context.Background(), nil, "--version")
	if !strings.HasPrefix(string(v), "2.1.310") {
		t.Errorf("version %q", v)
	}
}

// The real runner never passes on the shell's CLAUDE_CONFIG_DIR: the
// caller names the profile, and none means ~/.claude (block 3 review).
func TestExecClaudeTakesTheProfileFromTheCallerOnly(t *testing.T) {
	bin := t.TempDir()
	testtool.Write(t, bin, "claude", testtool.Fake{Script: "env CLAUDE_CONFIG_DIR unset"})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/u/.claude-work")
	if out, err := (execClaude{}).Run(context.Background(), nil, "plugin", "list"); err != nil || strings.TrimSpace(string(out)) != "unset" {
		t.Errorf("no profile given: CLAUDE_CONFIG_DIR=%q %v", out, err)
	}
	if out, err := (execClaude{}).Run(context.Background(), []string{"CLAUDE_CONFIG_DIR=/home/u/.claude-dev"}, "plugin", "list"); err != nil || strings.TrimSpace(string(out)) != "/home/u/.claude-dev" {
		t.Errorf("profile given: CLAUDE_CONFIG_DIR=%q %v", out, err)
	}
}

func TestDoctorAsksClaudeCodeWhetherThePluginIsEnabled(t *testing.T) {
	devInstall(t, true)
	withClaude(t)
	run(t, "client", "install")
	old := probes
	probes = func(e *doctor.Env) {
		old(e)
		e.ClaudePlugins = func(string) ([]byte, error) {
			return []byte(`[{"id":"backsight@backsight","enabled":false}]`), nil
		}
	}
	t.Cleanup(func() { probes = old })
	if _, out, _ := run(t, "doctor"); !strings.Contains(out, "disabled in Claude Code") {
		t.Errorf("doctor:\n%s", out)
	}
	probes = func(e *doctor.Env) {
		old(e)
		e.ClaudePlugins = func(string) ([]byte, error) { return []byte(`[]`), nil }
	}
	if _, out, _ := run(t, "doctor"); !strings.Contains(out, "not installed in Claude Code") {
		t.Errorf("doctor:\n%s", out)
	}
}

// Doctor's client check sends a disabled plugin to client install: the
// install enables it again (in its profile), says so, and doctor's check
// is then ok. An enabled plugin is left alone.
func TestClientInstallEnablesADisabledPlugin(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	run(t, "client", "install")
	f.marketplaces = `[{"name":"backsight","source":"directory","path":"` + testtool.JSONPath(filepath.Join(pathsOf(home).Data, "marketplace")) + `"}]`
	f.plugins = `[{"id":"backsight@backsight","version":"x","scope":"user","enabled":false}]`
	old := probes
	probes = func(e *doctor.Env) { // doctor asks the fake claude, as the real one
		old(e)
		e.ClaudePlugins = func(string) ([]byte, error) {
			return claudeRunner.Run(context.Background(), nil, "plugin", "list", "--json")
		}
	}
	t.Cleanup(func() { probes = old })
	doctorClient := func() doctor.Check {
		_, out, _ := run(t, "doctor", "--json")
		var env struct {
			Data struct {
				Checks []doctor.Check `json:"checks"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &env)
		for _, c := range env.Data.Checks {
			if c.ID == "client" {
				return c
			}
		}
		t.Fatalf("no client check:\n%s", out)
		return doctor.Check{}
	}
	if c := doctorClient(); c.Status != doctor.Warn || !strings.Contains(c.Detail, "disabled in Claude Code") {
		t.Fatalf("before: %+v", c)
	}
	f.calls = nil
	code, out, _ := run(t, "client", "install", "--json")
	enable := "plugin enable " + plugin.PluginID() + " --scope user"
	if code != output.OK || !slices.Contains(mutating(f), enable) || !strings.Contains(out, enable) {
		t.Fatalf("exit %d, claude %q\n%s", code, mutating(f), out)
	}
	if c := doctorClient(); c.Status != doctor.OK {
		t.Errorf("after: %+v", c)
	}
	f.calls = nil
	run(t, "client", "install")
	if slices.Contains(mutating(f), enable) {
		t.Errorf("an enabled plugin enabled again: %q", mutating(f))
	}
}

// Two installs cannot share a profile: a marketplace that still exists
// elsewhere is another install's, a missing one is this install's, moved.
func TestClientInstallRefusesAProfileWiredToAnotherInstall(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	other := filepath.Join(home, "other-install", "marketplace")
	_ = os.MkdirAll(filepath.Join(other, ".claude-plugin"), 0o700)
	f.marketplaces = `[{"name":"backsight","source":"directory","path":"` + testtool.JSONPath(other) + `"}]`
	code, out, _ := run(t, "client", "install", "--json")
	if code != output.Blocked || !strings.Contains(out, "another backsight install") || len(mutating(f)) > 0 {
		t.Fatalf("exit %d, claude %v\n%s", code, mutating(f), out)
	}
	_ = os.RemoveAll(filepath.Join(home, "other-install")) // moved away: ours, re-added
	f.calls = nil
	if code, out, _ := run(t, "client", "install"); code != output.OK || !strings.Contains(strings.Join(mutating(f), ","), "plugin marketplace remove backsight") {
		t.Errorf("a moved data directory must be re-added: exit %d %v\n%s", code, mutating(f), out)
	}
}

func schedulerStepOf(t *testing.T, out string) installStep {
	t.Helper()
	var env struct {
		Data clientInstallResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, s := range env.Data.Steps {
		if s.Name == "scheduler" {
			return s
		}
	}
	return installStep{}
}

// client install (so the new binary's, during an update apply) brings the
// scheduler entries to its templates (SPEC §7 step 7, issue 222); --plan
// only says so; a refresh that fails does not fail the install.
func TestClientInstallRefreshesTheSchedulerEntries(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	sys := withSys(t, "linux")
	if code, out, _ := run(t, "client", "install", "--json"); code != output.OK || schedulerStepOf(t, out).Name != "" {
		t.Fatalf("no entries yet, no scheduler step: exit %d\n%s", code, out)
	}
	run(t, "jobs", "install")
	svc := filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.service")
	oldTemplate(t, svc, "TimeoutStopSec=45min\n")

	code, out, _ := run(t, "client", "install", "--plan", "--json")
	if s := schedulerStepOf(t, out); code != output.OK || s.Status != "planned" || !strings.Contains(s.Detail, svc) || strings.Contains(readFile(t, svc), "TimeoutStopSec") {
		t.Fatalf("plan: exit %d %+v", code, s)
	}
	sys.calls = nil
	code, out, _ = run(t, "client", "install", "--json")
	if s := schedulerStepOf(t, out); code != output.OK || s.Status != "done" || !strings.Contains(readFile(t, svc), "TimeoutStopSec=45min\n") {
		t.Fatalf("install: exit %d %+v\n%s", code, s, readFile(t, svc))
	}
	if !slices.Contains(sys.calls, "systemctl --user daemon-reload") {
		t.Errorf("calls %q", sys.calls)
	}
	if _, out, _ := run(t, "client", "install", "--json"); schedulerStepOf(t, out).Status != "unchanged" {
		t.Errorf("rerun:\n%s", out)
	}

	sys.xdg = filepath.Join(home, "elsewhere") // entries this binary cannot refresh in place
	code, out, _ = run(t, "client", "install", "--json")
	if s := schedulerStepOf(t, out); code != output.OK || s.Status != "failed" || !strings.Contains(s.Detail, "jobs install") {
		t.Errorf("failed refresh: exit %d %+v", code, s)
	}
}
