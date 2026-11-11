package harness

import (
	"testing"

	"github.com/Barben360/backsight/internal/plugin"
)

// Claude Code's answers to the seams added for Codex are today's: hooks
// pair by Claude Code's pid, the gate stops AskUserQuestion, the plugin's
// PreToolUse matcher, and Codex's hook fields never decode from its events.
func TestClaudeSeamAnswers(t *testing.T) {
	c := Claude{}
	if !c.PairsByPID() {
		t.Error("Claude Code's hooks no longer pair by pid")
	}
	if c.QuestionTool() != "AskUserQuestion" {
		t.Errorf("question tool %q", c.QuestionTool())
	}
	found := false
	for _, h := range plugin.Hooks(plugin.Wiring{}) {
		if h.Arg == EventPreToolUse {
			found = true
		}
	}
	if !found {
		t.Error("the plugin wires no PreToolUse")
	}
	in, err := c.DecodeHook(EventPreToolUse, []byte(`{"session_id":"s","turn_id":"t","tool_use_id":"u","model":"m","permission_mode":"default"}`))
	if err != nil || in.TurnID != "" || in.ToolUseID != "" || in.Model != "" || in.PermissionMode != "" || in.SessionID != "s" {
		t.Errorf("%+v %v", in, err)
	}
}
