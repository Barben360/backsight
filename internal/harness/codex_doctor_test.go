package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// codex_doctor.go's ledger: block 8, https://github.com/Barben360/backsight/issues/283.
func init() {
	codexOwns("codex_doctor.go", codexPart{over: true,
		done: []string{"ConversationStart", "PluginStates", "UntrustedHooks", "LatestRoot", "Features"}})
}

// The recorded plugin list holds the main plugin alone, installed and
// enabled: the hooks plugin reads as not installed.
func TestCodexPluginStatesFromTheRecordedList(t *testing.T) {
	raw, err := os.ReadFile(codexFixtures + "config/plugin-list.json")
	if err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") != "plugin list --json" {
			t.Fatalf("ran codex %v", args)
		}
		return raw, nil
	}
	got, err := Codex{}.PluginStates(context.Background(), Env{Run: run})
	if err != nil {
		t.Fatal(err)
	}
	if m := got["backsight@backsight"]; !m.Installed || !m.Enabled || m.Version != "0.0.0-dev+7cabb9088085.codex" {
		t.Errorf("main plugin %+v", m)
	}
	if h, ok := got["backsight-hooks@backsight"]; !ok || h.Installed || h.Enabled {
		t.Errorf("hooks plugin %+v (listed %v)", h, ok)
	}
	bad := func(context.Context, []string, ...string) ([]byte, error) { return []byte("not json"), nil }
	if _, err := (Codex{}).PluginStates(context.Background(), Env{Run: bad}); err == nil {
		t.Error("unexpected output read as a list")
	}
}

// Every wired handler needs its trusted_hash; a config.toml that is not
// there trusts none; one that does not parse is an error, not "trusted".
func TestCodexUntrustedHooks(t *testing.T) {
	home := t.TempDir()
	env := Env{Home: home, Profile: home}
	keys := CodexTrustKeys()
	got, err := Codex{}.UntrustedHooks(env)
	if err != nil || !slices.Equal(got, keys) {
		t.Fatalf("no config.toml: %v %v, want every key", got, err)
	}
	var b strings.Builder
	for _, k := range keys[1:] {
		b.WriteString("[hooks.state.\"" + k + "\"]\ntrusted_hash = \"sha256:00\"\n\n")
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := (Codex{}).UntrustedHooks(env); err != nil || !slices.Equal(got, keys[:1]) {
		t.Errorf("one missing: %v %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[hooks\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := (Codex{}).UntrustedHooks(env); err == nil {
		t.Errorf("invalid TOML: %v, no error", got)
	}
}

// The newest root thread started by a time: subagent threads (no
// SessionStart of their own) are skipped, a later thread is not taken.
func TestCodexLatestRoot(t *testing.T) {
	home, paths := codexHome(t, "root.jsonl", "subagent.jsonl", "fork.jsonl", "clear-new.jsonl")
	env := Env{Home: home, Profile: home}
	m, p, ok := Codex{}.LatestRoot(env, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if !ok || m.ID != clearID || p != paths["clear-new.jsonl"] || !m.Started.Equal(time.Date(2026, 11, 10, 20, 35, 2, 765e6, time.UTC)) {
		t.Errorf("latest: %+v %s %v", m, p, ok)
	}
	// The subagent (20:34:51) is newer than the root (20:34:44): skipped.
	m, _, ok = Codex{}.LatestRoot(env, time.Date(2026, 11, 10, 20, 34, 55, 0, time.UTC))
	if !ok || m.ID != rootID {
		t.Errorf("by 20:34:55: %+v %v, want the root thread", m, ok)
	}
	if _, _, ok := (Codex{}).LatestRoot(env, time.Date(2026, 11, 10, 20, 34, 0, 0, time.UTC)); ok {
		t.Error("a thread before any started")
	}
}

// The recorded features list: memories off, hooks on; output that is no
// list is an error.
func TestCodexFeaturesFromTheRecordedList(t *testing.T) {
	raw, err := os.ReadFile(codexFixtures + "config/features.txt")
	if err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, _ []string, args ...string) ([]byte, error) { return raw, nil }
	got, err := Codex{}.Features(context.Background(), Env{Run: run})
	if err != nil {
		t.Fatal(err)
	}
	if on, ok := got["memories"]; !ok || on {
		t.Errorf("memories %v %v", on, ok)
	}
	if on := got["hooks"]; !on {
		t.Error("hooks off")
	}
	odd := func(context.Context, []string, ...string) ([]byte, error) { return []byte("error: unknown\n"), nil }
	if _, err := (Codex{}).Features(context.Background(), Env{Run: odd}); err == nil {
		t.Error("no rows read as a list")
	}
}

// A rollout's start, from its head: the front end that started it and its
// directory.
func TestCodexConversationStart(t *testing.T) {
	_, paths := codexHome(t, "root.jsonl")
	if ep, cwd := (Codex{}).ConversationStart(paths["root.jsonl"]); ep != "codex-tui" || cwd != "/home/tester/work" {
		t.Errorf("%q %q", ep, cwd)
	}
	if ep, cwd := (Codex{}).ConversationStart(filepath.Join(t.TempDir(), "gone.jsonl")); ep != "" || cwd != "" {
		t.Errorf("gone: %q %q", ep, cwd)
	}
}
