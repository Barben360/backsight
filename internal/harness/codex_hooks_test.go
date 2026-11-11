package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// codex_hooks.go's ledger: block 4, https://github.com/Barben360/backsight/issues/279.
func init() {
	codexOwns("codex_hooks.go", codexPart{
		done: []string{"Events", "PairsBySession", "PairsByPID"},
		stubs: map[string]func() bool{
			"DecodeHook": func() bool {
				_, err := (Codex{}).DecodeHook(EventStop, []byte(`{"session_id":"s"}`))
				return notImplemented(err)
			},
			// A real implementation answers a stop's block.
			"EncodeHook":   func() bool { return (Codex{}).EncodeHook(HookOutput{Event: EventStop, Block: "b"}) == nil },
			"QuestionTool": func() bool { return (Codex{}).QuestionTool() == "" },
		},
	})
}

// codexFixture is the hook payload Codex 0.167.0 sent, by fixture name.
func codexFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "hooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return in
}

// fixtureNames are the recorded payloads of event, without ".json".
func fixtureNames(t *testing.T, event string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join("..", "testdata", "codex", "0.167.0", "hooks", event+"*.json"))
	var out []string
	for _, f := range files {
		out = append(out, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	return out
}

// The event map is 0085 D3's, tied to what Codex sent: each event was
// recorded, each runs one of backsight's events with its own timeout, the
// SessionStart matchers are the recorded sources, the gate's matcher is the
// recorded question tool, the memory tools' matcher takes the recorded
// memory calls and nothing else, and no failure hook is wired.
func TestCodexEventMap(t *testing.T) {
	ids := []string{EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventPostToolUse,
		EventSubagentStart, EventSubagentStop, EventStop, EventSessionEnd}
	events := Codex{}.Events()
	var sources []string
	for _, e := range events {
		if !slices.Contains(ids, e.Event) {
			t.Errorf("%s runs %q", e.Name, e.Event)
		}
		if e.Timeout <= 0 {
			t.Errorf("%s has no timeout", e.Name)
		}
		if len(fixtureNames(t, e.Name)) == 0 {
			t.Errorf("Codex sent no %s in the recorded run", e.Name)
		}
		m := regexp.MustCompile("^(?:" + e.Matcher + ")$")
		switch e.Name {
		case "SessionStart":
			sources = append(sources, strings.Split(e.Matcher, "|")...)
		case "PreToolUse":
			if tool := codexFixture(t, "PreToolUse.request_user_input")["tool_name"]; tool != e.Matcher {
				t.Errorf("gate matcher %q, Codex's question tool %v", e.Matcher, tool)
			}
		case "PostToolUse":
			for _, name := range fixtureNames(t, "PostToolUse") {
				tool, _ := codexFixture(t, name)["tool_name"].(string)
				if m.MatchString(tool) != strings.HasPrefix(tool, "mcp__memory__") {
					t.Errorf("memory matcher %q on %s's %q", e.Matcher, name, tool)
				}
			}
			if m.MatchString("mcp__plugin_backsight_memory__memory_search") {
				t.Errorf("memory matcher %q takes Claude Code's name", e.Matcher)
			}
		}
	}
	var recorded []string
	for _, name := range fixtureNames(t, "SessionStart") {
		recorded = append(recorded, strings.TrimPrefix(name, "SessionStart."))
	}
	slices.Sort(sources)
	slices.Sort(recorded)
	if !slices.Equal(sources, recorded) {
		t.Errorf("SessionStart sources %v, recorded %v", sources, recorded)
	}
	for _, e := range events {
		if e.Name == "PostToolUseFailure" {
			t.Error("a failure hook is wired: Codex has none")
		}
	}
	events[0].Name = "changed"
	if (Codex{}).Events()[0].Name == "changed" {
		t.Error("Events hands out the table itself")
	}
}

// Codex's hooks and memory server pair by session id, never by process.
func TestCodexPairsBySessionOnly(t *testing.T) {
	c := Codex{}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if !c.PairsBySession(goos) {
			t.Errorf("%s: not by session", goos)
		}
	}
	if c.PairsByPID() {
		t.Error("by process id")
	}
}
