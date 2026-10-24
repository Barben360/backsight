package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
)

// fakeClaude records claude CLI calls and answers the list commands.
type fakeClaude struct {
	calls        [][]string
	envs         [][]string
	version      string
	marketplaces string
	plugins      string
	missing      bool
}

func (f *fakeClaude) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	if f.missing {
		return nil, errClaudeMissing
	}
	f.calls = append(f.calls, args)
	f.envs = append(f.envs, env)
	switch strings.Join(args, " ") {
	case "--version":
		return []byte(f.version + " (Claude Code)\n"), nil
	case "plugin marketplace list --json":
		return []byte(f.marketplaces), nil
	case "plugin list --json":
		return []byte(f.plugins), nil
	}
	return []byte("ok\n"), nil
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
	shim := filepath.Join(d.Data, "bin", "current")
	target, err := filepath.EvalSymlinks(shim)
	if err != nil || !strings.HasPrefix(target, filepath.Join(d.Data, "bin")) {
		t.Fatalf("shim %s -> %s %v", shim, target, err)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("shim target not executable: %v", err)
	}
	hooks, _ := os.ReadFile(filepath.Join(mkt, "plugins", "backsight", "hooks", "hooks.json"))
	if !strings.Contains(string(hooks), `"command": "`+shim+`"`) {
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
	if st.PluginVersion == "" || st.ShimTarget != target {
		t.Errorf("state %+v", st)
	}
}

func TestClientInstallAgainUpdatesInsteadOfAdding(t *testing.T) {
	home := devInstall(t, true)
	f := withClaude(t)
	run(t, "client", "install")
	f.calls = nil
	f.marketplaces = `[{"name":"backsight","source":"directory"}]`
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
