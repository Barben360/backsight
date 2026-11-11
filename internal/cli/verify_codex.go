package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/state"
)

// verify --codex (the Codex parity gaps, epic
// https://github.com/Barben360/backsight/issues/276): Codex's hooks run as
// Codex runs them (`hook --harness codex <event>`, Codex-shaped events, as
// recorded in internal/testdata/codex/0.167.0/hooks/), the memory server as
// Codex starts it (`mcp --harness codex`), its calls naming their session
// in _meta (decision 0085 D4), then the same scratch-bank round trip.

// codexOnRecord: client install --codex recorded a Codex home.
func codexOnRecord(home string, st state.State) bool {
	_, ok := codexWired(home, st)
	return ok
}

// codexSyntheticInput is a synthetic Codex event for e, as Codex 0.167
// sends it: Claude Code's fields plus turn_id, model and permission_mode,
// tool_use_id on a tool's, the MCP result as an object. As for Claude
// Code's: a session of its own in an empty directory, a prompt too short
// to recall, a rollout that does not exist, so no hook sends or stores
// anything.
func codexSyntheticInput(e harness.CodexEvent, sid, turn, dir string) []byte {
	in := map[string]any{"session_id": sid, "cwd": dir, "hook_event_name": e.Name, "model": "backsight-verify", "permission_mode": "default",
		"transcript_path": filepath.Join(dir, "rollout-verify-"+sid+".jsonl")}
	if e.Name != "SessionStart" && e.Name != "SessionEnd" {
		in["turn_id"] = turn
	}
	switch e.Name {
	case "SessionStart":
		in["source"] = "startup"
	case "UserPromptSubmit":
		in["prompt"] = "backsight verify"
	case "PreToolUse":
		in["tool_name"], in["tool_input"], in["tool_use_id"] = codex.QuestionTool(), map[string]any{"questions": []any{}}, "backsight-verify-call-1"
	case "PostToolUse":
		in["tool_name"], in["tool_input"], in["tool_use_id"] = codex.ToolNames()[0], map[string]any{"queries": []string{"verify"}}, "backsight-verify-call-2"
		in["tool_response"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "nothing in memory\n"}}}
	case "SubagentStart", "SubagentStop":
		in["agent_id"], in["agent_type"] = "backsight-verify-agent", "default"
		if e.Name == "SubagentStop" {
			in["agent_transcript_path"], in["last_assistant_message"], in["stop_hook_active"] = filepath.Join(dir, "rollout-verify-agent.jsonl"), "done", false
		}
	case "Stop":
		in["last_assistant_message"], in["stop_hook_active"] = "done", false
	case "SessionEnd":
		in["reason"] = "other"
	}
	b, _ := json.Marshal(in)
	return b
}

// verifyCodexMCP starts the memory server as Codex does and expects every
// memory tool (memory_credit only with feedback on); then, with feedback
// on, two memory_credit calls with verdict none check that the server
// takes each call's session from its _meta (0085 D4), as Codex sends it:
// the call naming the session is noted, the one naming none is refused.
// Neither reaches the memory server: a none verdict is answered locally.
func verifyCodexMCP(ctx context.Context, shim string, env []string, dir string, feedbackOn bool, sid, turn string) verifyCheck {
	c := verifyCheck{ID: "mcp"}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shim, codex.MCPArgs()...)
	cmd.Env, cmd.Dir = env, dir
	start := time.Now()
	client := mcp.NewClient(&mcp.Implementation{Name: "backsight-verify", Version: buildinfo.Version}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		c.Status, c.Summary = "fail", "no MCP handshake: "+err.Error()
		return c
	}
	defer func() { _ = sess.Close() }()
	list, err := sess.ListTools(ctx, nil)
	if err != nil {
		c.Status, c.Summary = "fail", "tools/list: "+err.Error()
		return c
	}
	var got []string
	for _, t := range list.Tools {
		got = append(got, t.Name)
	}
	for _, want := range codex.ToolNames() {
		short := want[strings.LastIndex(want, "__")+2:]
		if !slices.Contains(got, short) && (feedbackOn || short != "memory_credit") {
			c.Status, c.Summary = "fail", fmt.Sprintf("tool %s missing (server lists %v)", short, got)
			return c
		}
	}
	identity := "the per-call session not checked (feedback is off: no memory_credit)"
	if feedbackOn {
		meta := mcp.Meta{"sessionId": sid, "threadId": sid, "callId": "backsight-verify-call-3",
			"x-codex-turn-metadata": map[string]any{"session_id": sid, "thread_id": sid, "turn_id": turn, "workspaces": map[string]any{dir: map[string]any{"has_changes": false}}}}
		text, isErr, err := creditNone(ctx, sess, meta)
		if err != nil || isErr || !strings.HasPrefix(text, creditNonePrefix) {
			c.Status, c.Summary = "fail", "a call naming its session in _meta was not taken as that session's: "+firstOf(err, text)
			return c
		}
		text, isErr, err = creditNone(ctx, sess, nil)
		if err != nil || !isErr || text != noCallSession {
			c.Status, c.Summary = "fail", "a call naming no session was not refused as Codex's server refuses it: "+firstOf(err, text)
			return c
		}
		identity = "each call's session read from its _meta"
	}
	c.MS = int(time.Since(start).Milliseconds())
	c.Status, c.Summary = "ok", fmt.Sprintf("handshake and %d tools, %s, in %d ms", len(got), identity, c.MS)
	return c
}

// creditNone calls memory_credit with verdict none and meta as _meta: the
// answer's text and whether it is an error result.
func creditNone(ctx context.Context, sess *mcp.ClientSession, meta mcp.Meta) (string, bool, error) {
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Meta: meta, Name: "memory_credit", Arguments: map[string]any{"verdict": "none"}})
	if err != nil {
		return "", false, err
	}
	var text string
	for _, ct := range res.Content {
		if t, ok := ct.(*mcp.TextContent); ok {
			text += t.Text
		}
	}
	return text, res.IsError, nil
}

func firstOf(err error, text string) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("%q", text)
}
