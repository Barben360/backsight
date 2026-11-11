package setup

import (
	"testing"
)

// claudeBacklog is a backfill covering Claude Code alone, as before Codex.
func claudeBacklog(n int, cost string) Backlog {
	return Backlog{Tools: []ToolSessions{{Tool: ToolsClaude, Sessions: n}}, Cost: cost}
}

// The backfill question says the tools it covers: Claude Code's as before
// Codex, else each tool with sessions, named.
func TestBackfillQuestionNamesTheTools(t *testing.T) {
	cases := []struct {
		b    Backlog
		want string
	}{
		{claudeBacklog(3, ""), "Send your 3 past sessions to the memory, in the background?"},
		{claudeBacklog(1, ""), "Send your 1 past session to the memory, in the background?"},
		{Backlog{Tools: []ToolSessions{{ToolsCodex, 2}}}, "Send your 2 past Codex sessions to the memory, in the background?"},
		{Backlog{Tools: []ToolSessions{{ToolsClaude, 3}, {ToolsCodex, 2}}}, "Send your 5 past sessions (3 Claude Code, 2 Codex) to the memory, in the background?"},
		{Backlog{Tools: []ToolSessions{{ToolsClaude, 3}, {ToolsCodex, 0}}}, "Send your 3 past Claude Code sessions to the memory, in the background?"},
		{Backlog{Tools: []ToolSessions{{ToolsClaude, 0}, {ToolsCodex, 1}}}, "Send your 1 past Codex session to the memory, in the background?"},
	}
	for _, c := range cases {
		qs, _, err := Resolve(Inputs{Codex: true, Answers: map[string]string{"tools": ToolsBoth}, Backfill: func(string, bool, bool) Backlog { return c.b }})
		if err != nil {
			t.Fatal(err)
		}
		if q := find(t, qs, "backfill"); q.Prompt != c.want || q.Default != "yes" {
			t.Errorf("%+v: %q default %q, want %q", c.b, q.Prompt, q.Default, c.want)
		}
	}
	qs, _, _ := Resolve(Inputs{Codex: true, Answers: map[string]string{"tools": ToolsBoth}, Backfill: func(string, bool, bool) Backlog {
		return Backlog{Tools: []ToolSessions{{ToolsClaude, 0}, {ToolsCodex, 0}}}
	}})
	if has(qs, "backfill") {
		t.Error("asked with no session to send")
	}
}

// The estimate is asked for the tools this setup wires: Codex alone gets
// the question too (before Codex it was Claude Code's only), with the same
// defaults; a server-only host is asked nothing.
func TestBackfillFollowsTheToolsAnswer(t *testing.T) {
	cases := []struct {
		tools               string
		wantClaude, wantCdx bool
	}{
		{ToolsClaude, true, false},
		{ToolsCodex, false, true},
		{ToolsBoth, true, true},
	}
	for _, c := range cases {
		var claude, codex, asked bool
		qs, _, err := Resolve(Inputs{Codex: true, Answers: map[string]string{"tools": c.tools}, Backfill: func(_ string, cl, cx bool) Backlog {
			claude, codex, asked = cl, cx, true
			return Backlog{Tools: []ToolSessions{{ToolsCodex, 4}}, Cost: "≈ $0.10"}
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !asked || claude != c.wantClaude || codex != c.wantCdx || !has(qs, "backfill") {
			t.Errorf("tools=%s: asked %v for claude %v codex %v, question %v", c.tools, asked, claude, codex, has(qs, "backfill"))
		}
	}
	qs, _, _ := Resolve(Inputs{Codex: true, Answers: map[string]string{"tools": ToolsCodex, "provider": "claude-code"},
		Backfill: func(string, bool, bool) Backlog { return Backlog{Tools: []ToolSessions{{ToolsCodex, 4}}} }})
	if q := find(t, qs, "backfill"); q.Default != "no" || q.Options[0].Caveat == "" {
		t.Errorf("subscription, Codex alone: %+v", q)
	}
	asked := false
	_, _, _ = Resolve(Inputs{Codex: true, Answers: map[string]string{"role": "server"}, Backfill: func(string, bool, bool) Backlog { asked = true; return claudeBacklog(1, "") }})
	if asked {
		t.Error("a server-only host estimated a backfill")
	}
}
