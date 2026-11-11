package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// codex_hooks.go's ledger: block 4, https://github.com/Barben360/backsight/issues/279.
func init() {
	codexOwns("codex_hooks.go", codexPart{over: true,
		done: []string{"Events", "PairsBySession", "PairsByPID", "DecodeHook", "EncodeHook", "QuestionTool", "MachinePrompt"}})
}

// Block 4 is over: no stub of codex_hooks.go is left.
func TestCodexHooksHaveNoStubs(t *testing.T) {
	if p := codexParts["codex_hooks.go"]; !p.over || len(p.stubs) != 0 {
		t.Errorf("codex_hooks.go: over %v, stubs %v", p.over, p.stubs)
	}
}

const (
	cxRootSession = "01a1c72b-9533-7bb0-9002-52dca295a387" // session-root.txt
	cxSubagent    = "01a1c72b-ae08-75b3-a4db-8a06f93c0c83"
)

// Every payload Codex sent decodes, each recorded field into its
// HookInput field (Codex's own included: turn_id, tool_use_id, model,
// permission_mode); inside a subagent the session is still the root's.
func TestCodexDecodesEveryRecordedPayload(t *testing.T) {
	names := fixtureNames(t, "")
	if len(names) < 40 {
		t.Fatalf("%d fixtures", len(names))
	}
	for _, name := range names {
		raw, _ := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "hooks", name+".json"))
		in, err := Codex{}.DecodeHook("", raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		want := codexFixture(t, name)
		got := map[string]any{"session_id": in.SessionID, "turn_id": in.TurnID, "tool_use_id": in.ToolUseID, "model": in.Model,
			"permission_mode": in.PermissionMode, "transcript_path": in.TranscriptPath, "cwd": in.Cwd, "source": in.Source, "prompt": in.Prompt,
			"tool_name": in.ToolName, "agent_id": in.AgentID, "agent_type": in.AgentType, "agent_transcript_path": in.AgentTranscriptPath,
			"last_assistant_message": in.LastAssistantMessage, "reason": in.Reason}
		for k, v := range got {
			if w, ok := want[k]; ok && w != v {
				t.Errorf("%s: %s %q, recorded %q", name, k, v, w)
			} else if !ok && v != "" {
				t.Errorf("%s: %s %q, none recorded", name, k, v)
			}
		}
		if w, _ := want["stop_hook_active"].(bool); w != in.StopHookActive {
			t.Errorf("%s: stop_hook_active %v", name, in.StopHookActive)
		}
		if w, ok := want["tool_input"]; ok {
			var back any
			if json.Unmarshal(in.ToolInput, &back) != nil || !reflect.DeepEqual(back, w) {
				t.Errorf("%s: tool_input %s", name, in.ToolInput)
			}
		}
		// A subagent's event names its thread as the agent, the root
		// session as the session: the main run's subagent, the root.
		if sub := strings.Contains(name, ".subagent"); sub != (in.AgentID != "") || (sub && in.SessionID == in.AgentID) {
			t.Errorf("%s: session %q, agent %q", name, in.SessionID, in.AgentID)
		}
		if strings.Contains(name, ".subagent") && !strings.Contains(name, "probe") && (in.SessionID != cxRootSession || in.AgentID != cxSubagent) {
			t.Errorf("%s: session %q, agent %q: not the root's subagent", name, in.SessionID, in.AgentID)
		}
	}
	if _, err := (Codex{}).DecodeHook("", []byte("not json")); err == nil {
		t.Error("junk decoded")
	}
}

// An MCP tool's response, an object, decodes to its content blocks (the
// shape the handlers read, Claude Code's); a string response (Bash, the
// subagent tools) stays the string.
func TestCodexToolResponseShapes(t *testing.T) {
	decode := func(name string) json.RawMessage {
		raw, _ := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "hooks", name+".json"))
		in, err := Codex{}.DecodeHook(EventPostToolUse, raw)
		if err != nil {
			t.Fatal(err)
		}
		return in.ToolResponse
	}
	var blocks []struct{ Type, Text string }
	if err := json.Unmarshal(decode("PostToolUse.mcp__memory__memory_search"), &blocks); err != nil || len(blocks) != 1 || blocks[0].Text != "nothing in memory\n" {
		t.Errorf("MCP response %v %v", blocks, err)
	}
	var store []struct{ Text string }
	if json.Unmarshal(decode("PostToolUse.mcp__memory__memory_store"), &store) != nil || len(store) == 0 || !strings.Contains(store[0].Text, "note:") {
		t.Errorf("store response %v", store)
	}
	var out string
	if err := json.Unmarshal(decode("PostToolUse.Bash"), &out); err != nil || out != "ok\n" {
		t.Errorf("Bash response %q %v", out, err)
	}
	// An object that is no MCP result is kept whole.
	in, _ := Codex{}.DecodeHook(EventPostToolUse, []byte(`{"tool_response":{"status":"done"}}`))
	if string(in.ToolResponse) != `{"status":"done"}` {
		t.Errorf("other object %s", in.ToolResponse)
	}
}

// Codex reads the answers backsight gave it in the recorded run
// (backsight/*.out.json, accepted by Codex 0.167): the encoder writes the
// same JSON for the same answer, its brand word colored unless NO_COLOR.
func TestCodexAnswersAreTheOnesCodexAccepted(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	recorded := func(name string) map[string]any {
		raw, err := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "backsight", name+".out.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	decode := func(raw []byte) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		return m
	}
	stop := recorded("stop")
	if got := decode(Codex{}.EncodeHook(HookOutput{Event: EventStop, Block: stop["reason"].(string)})); !reflect.DeepEqual(got, stop) {
		t.Errorf("stop %v, recorded %v", got, stop)
	}
	for name, event := range map[string]string{"session-start": EventSessionStart, "user-prompt-submit": EventUserPromptSubmit, "subagent-start": EventSubagentStart} {
		want := recorded(name)
		ctx := want["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
		banner, _ := want["systemMessage"].(string)
		plain := strings.ReplaceAll(strings.ReplaceAll(banner, "\x1b[1;38;2;201;80;122m", ""), "\x1b[0m", "")
		if got := decode(Codex{}.EncodeHook(HookOutput{Event: event, Context: ctx, Banner: plain})); !reflect.DeepEqual(got, want) {
			t.Errorf("%s %v, recorded %v", name, got, want)
		}
	}
	// The gate's denial, which Codex's PreToolUse reads (permissionDecision).
	deny := decode(Codex{}.EncodeHook(HookOutput{Event: EventPreToolUse, Deny: "ask memory first"}))
	if s, _ := deny["hookSpecificOutput"].(map[string]any); s["hookEventName"] != "PreToolUse" || s["permissionDecision"] != "deny" || s["permissionDecisionReason"] != "ask memory first" {
		t.Errorf("deny %v", deny)
	}
	if out := (Codex{}).EncodeHook(HookOutput{Event: EventPostToolUse}); out != nil {
		t.Errorf("nothing to say printed %s", out)
	}
}

// Each text Codex injects stays within CodexTextMax bytes, past which
// Codex spills it to a file: a longer one is cut at its last whole line
// that fits, never inside a character, and says it was cut; a text that
// fits, up to the last byte, is whole.
func TestCodexTextsFitBeforeTheSpill(t *testing.T) {
	line := strings.Repeat("é", 49) + "\n" // 99 bytes
	long := strings.Repeat(line, 200)
	for _, o := range []HookOutput{{Event: EventSessionStart, Context: long}, {Event: EventStop, Block: long}, {Event: EventPreToolUse, Deny: long}} {
		var m map[string]any
		_ = json.Unmarshal(Codex{}.EncodeHook(o), &m)
		text, _ := m["reason"].(string)
		if s, ok := m["hookSpecificOutput"].(map[string]any); ok {
			text, _ = s["additionalContext"].(string)
			if text == "" {
				text, _ = s["permissionDecisionReason"].(string)
			}
		}
		if len(text) > CodexTextMax || len(text) < CodexTextMax-len(line)-len(codexCut) || !utf8.ValidString(text) ||
			!strings.HasSuffix(text, "\n…") || !strings.HasPrefix(long, strings.TrimSuffix(text, "…")) {
			t.Errorf("%s: %d bytes, ends %q", o.Event, len(text), text[max(0, len(text)-10):])
		}
	}
	fits := strings.Repeat("a", CodexTextMax)
	if got := fitCodex(fits); got != fits {
		t.Errorf("a text of exactly %d bytes was cut to %d", CodexTextMax, len(got))
	}
	// One long line is cut inside it, at a character (the "a" puts the
	// byte limit inside one).
	one := "a" + strings.Repeat("é", CodexTextMax)
	if got := fitCodex(one); len(got) > CodexTextMax || !utf8.ValidString(got) || !strings.HasSuffix(got, codexCut) {
		t.Errorf("one line: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

// Codex's question tool is the gate's, the one Codex sent PreToolUse for.
func TestCodexQuestionToolIsTheRecordedOne(t *testing.T) {
	if q := (Codex{}).QuestionTool(); q != codexFixture(t, "PreToolUse.request_user_input")["tool_name"] {
		t.Errorf("question tool %q", q)
	}
}

// The prompts Codex writes itself are told from the user's: the recorded
// prompts are the user's; Codex's own fragments, as its rollouts carry
// them, are not.
func TestCodexMachinePrompts(t *testing.T) {
	for _, name := range fixtureNames(t, "UserPromptSubmit") {
		if p := codexFixture(t, name)["prompt"].(string); (Codex{}).MachinePrompt(p) {
			t.Errorf("%s: the user's prompt %.40q taken for Codex's", name, p)
		}
	}
	for _, p := range []string{
		"<subagent_notification>\n{\"agent_path\":\"x\"}\n</subagent_notification>",
		"<environment_context>\n  <cwd>/home/tester/work</cwd>\n</environment_context>",
		"# AGENTS.md instructions for /home/tester/work\n\n<INSTRUCTIONS>\nx\n</INSTRUCTIONS>",
		"<user_shell_command>\nls\n</user_shell_command>",
		"<turn_aborted>\nx\n</turn_aborted>",
		`<hook_prompt hook_run_id="stop:1">x</hook_prompt>`,
		"  <skill>\n<name>x</name>\n</skill>",
	} {
		if !(Codex{}).MachinePrompt(p) {
			t.Errorf("%.40q taken for the user's", p)
		}
	}
	for _, p := range []string{"what does <environment_context> hold?", "<environment_contextual>", "AGENTS.md instructions"} {
		if (Codex{}).MachinePrompt(p) {
			t.Errorf("%q taken for Codex's", p)
		}
	}
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
