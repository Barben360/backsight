package harness

import "slices"

// Codex's hook decoding and answers: block 4
// (https://github.com/Barben360/backsight/issues/279), 0085 D3. The event
// map below is the contract block 3 generates hooks.json from and block 4
// handles; the methods are stubs until block 4.

// CodexEvent is one Codex hook backsight wires: Codex's event name and
// matcher, the backsight event (Event*) its handler runs, and the
// handler's timeout in seconds (Codex's default, 600 s, is never left).
type CodexEvent struct {
	Name, Matcher, Event string
	Timeout              int
}

// codexEvents is 0085 D3's table. Not wired: PostToolUseFailure (Codex
// has none: a failed MCP call fires nothing), PreCompact, PostCompact,
// PermissionRequest, Interrupt, PreModelRequest, PostModelRequest.
var codexEvents = []CodexEvent{
	{"SessionStart", "startup", EventSessionStart, 10},
	{"SessionStart", "resume|clear|compact|fork", EventSessionStart, 5},
	{"UserPromptSubmit", "", EventUserPromptSubmit, 25},
	{"PreToolUse", "request_user_input", EventPreToolUse, 10},
	{"PostToolUse", codexToolPrefix + ".*", EventPostToolUse, 2},
	{"SubagentStart", "", EventSubagentStart, 5},
	{"SubagentStop", "", EventSubagentStop, 5},
	{"Stop", "", EventStop, 5},
	{"SessionEnd", "", EventSessionEnd, 1}, // Codex clamps it to 1-3 s
}

// Events are the Codex hooks backsight wires, in hooks.json's order.
func (Codex) Events() []CodexEvent { return slices.Clone(codexEvents) }

// DecodeHook reads a Codex hook event: Claude Code's fields plus turn_id,
// model, permission_mode, and tool_response as an object (MCP) or a
// string (Bash, Codex's own tools). Block 4.
func (Codex) DecodeHook(event string, raw []byte) (HookInput, error) {
	return HookInput{}, notYet("hook input")
}

// EncodeHook writes the answer Codex reads: Claude Code's JSON, every
// injected text under 9,000 bytes. Block 4; nothing until then (a hook
// prints nothing).
func (Codex) EncodeHook(out HookOutput) []byte { return nil }

// PairsBySession: Codex's hooks and its memory server pair by session id
// (the server reads it from each call's _meta), never by process. Block
// 4; false until then.
func (Codex) PairsBySession(goos string) bool { return false }
