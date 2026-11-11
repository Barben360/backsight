package harness

import "slices"

// Codex's hook decoding and answers: block 4
// (https://github.com/Barben360/backsight/issues/279), 0085 D3. The event
// map below is the contract block 3 generates hooks.json from and block 4
// handles; the pairing answers are decided (0085 D4); the other methods
// are stubs until block 4.

// CodexEvent is one Codex hook backsight wires: Codex's event name and
// matcher, the backsight event (Event*) its handler runs, and the
// handler's timeout in seconds (Codex's default, 600 s, is never left).
type CodexEvent struct {
	Name, Matcher, Event string
	Timeout              int
}

// codexEvents is 0085 D3's table, in hooks.json's order; the generator
// groups the rows by Name. Not wired: PostToolUseFailure (Codex has none:
// a failed MCP call fires nothing), PreCompact, PostCompact,
// PermissionRequest, Interrupt, PreModelRequest, PostModelRequest.
var codexEvents = []CodexEvent{
	{Name: "SessionStart", Matcher: "startup", Event: EventSessionStart, Timeout: 10},
	{Name: "SessionStart", Matcher: "resume|clear|compact|fork", Event: EventSessionStart, Timeout: 5},
	{Name: "UserPromptSubmit", Event: EventUserPromptSubmit, Timeout: 25},
	{Name: "PreToolUse", Matcher: codexQuestionTool, Event: EventPreToolUse, Timeout: 10},
	{Name: "PostToolUse", Matcher: codexToolPrefix + ".*", Event: EventPostToolUse, Timeout: 2},
	{Name: "SubagentStart", Event: EventSubagentStart, Timeout: 5},
	{Name: "SubagentStop", Event: EventSubagentStop, Timeout: 5},
	{Name: "Stop", Event: EventStop, Timeout: 5},
	{Name: "SessionEnd", Event: EventSessionEnd, Timeout: 1}, // Codex clamps it to 1-3 s
}

// codexQuestionTool is Codex's question tool, which the gate's PreToolUse
// matches (0085 D3).
const codexQuestionTool = "request_user_input"

// Events are the Codex hooks backsight wires, in hooks.json's order.
func (Codex) Events() []CodexEvent { return slices.Clone(codexEvents) }

// PairsBySession: Codex's hooks record their session under its id, where
// the memory server finds it from each call's _meta.sessionId (0085 D4).
func (Codex) PairsBySession(goos string) bool { return true }

// PairsByPID: never. Each thread starts its own memory server process, and
// under Codex's sharing flags one process serves several: a process id
// names no session (0085 D4).
func (Codex) PairsByPID() bool { return false }

// DecodeHook reads a Codex hook event: Claude Code's fields plus turn_id,
// tool_use_id, model, permission_mode, and tool_response as an object
// (MCP) or a string (Bash, Codex's own tools). Block 4.
func (Codex) DecodeHook(event string, raw []byte) (HookInput, error) {
	return HookInput{}, notYet("hook input")
}

// EncodeHook writes the answer Codex reads: Claude Code's JSON, every
// injected text under 9,000 bytes. Block 4; nothing until then (a hook
// prints nothing).
func (Codex) EncodeHook(out HookOutput) []byte { return nil }

// QuestionTool is codexQuestionTool, for the gate. Block 4 (with the
// gate's Codex behaviour); "" until then, so the gate never stops a Codex
// question.
func (Codex) QuestionTool() string { return "" }
