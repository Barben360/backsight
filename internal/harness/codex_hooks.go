package harness

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Codex's hook decoding and answers: block 4
// (https://github.com/Barben360/backsight/issues/279), 0085 D3. The event
// map below is the contract block 3 generates hooks.json from and block 4
// handles; the pairing answers are decided (0085 D4).

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

// codexHookInput is the event JSON Codex 0.167 sends (C14 §8, the
// recorded hooks/*.json): Claude Code's field names, plus turn_id on every
// turn-scoped event, tool_use_id on a tool's, model and permission_mode.
// Inside a subagent session_id is still the root session, agent_id the
// subagent's thread (C14 §6).
type codexHookInput struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	Cwd                  string          `json:"cwd"`
	Source               string          `json:"source"`
	Prompt               string          `json:"prompt"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	AgentTranscriptPath  string          `json:"agent_transcript_path"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	Reason               string          `json:"reason"`
	StopHookActive       bool            `json:"stop_hook_active"`
	TurnID               string          `json:"turn_id"`
	ToolUseID            string          `json:"tool_use_id"`
	Model                string          `json:"model"`
	PermissionMode       string          `json:"permission_mode"`
}

// DecodeHook reads a Codex hook event. An MCP tool's tool_response is the
// MCP result, an object ({"content":[{"type":"text","text":…}]}); it is
// decoded to its content blocks, the shape the handlers read for Claude
// Code's. A string response (Bash, Codex's own tools) stays as it came.
// Codex sends no prompt_id, error or is_interrupt (no PostToolUseFailure).
func (Codex) DecodeHook(_ string, raw []byte) (HookInput, error) {
	var in codexHookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return HookInput{}, err
	}
	return HookInput{
		SessionID:            in.SessionID,
		TranscriptPath:       in.TranscriptPath,
		Cwd:                  in.Cwd,
		Source:               in.Source,
		Prompt:               in.Prompt,
		ToolName:             in.ToolName,
		ToolInput:            in.ToolInput,
		ToolResponse:         codexToolResponse(in.ToolResponse),
		AgentID:              in.AgentID,
		AgentType:            in.AgentType,
		AgentTranscriptPath:  in.AgentTranscriptPath,
		LastAssistantMessage: in.LastAssistantMessage,
		Reason:               in.Reason,
		StopHookActive:       in.StopHookActive,
		TurnID:               in.TurnID,
		ToolUseID:            in.ToolUseID,
		Model:                in.Model,
		PermissionMode:       in.PermissionMode,
	}, nil
}

// codexToolResponse is an MCP result's content blocks; any other response
// unchanged.
func codexToolResponse(raw json.RawMessage) json.RawMessage {
	var result struct {
		Content json.RawMessage `json:"content"`
	}
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal(raw, &result) != nil || !strings.HasPrefix(strings.TrimSpace(string(result.Content)), "[") {
		return raw
	}
	return result.Content
}

// CodexTextMax is the most bytes of one injected text (context, a stop's
// block, a denial) a Codex answer carries: Codex estimates tokens as
// bytes / 4 and spills a text past 2,500 of them to a file, keeping only
// its head and tail (C14 §10; 0085 D3, where additionalContextLimit is
// not raised).
const CodexTextMax = 9000

// codexCut ends a text cut at CodexTextMax.
const codexCut = "\n…"

// EncodeHook writes the answer Codex reads: Claude Code's JSON, which
// Codex 0.167 accepts unchanged (C14 §8: additionalContext, systemMessage,
// Stop's decision block, PreToolUse's permissionDecision deny), every
// injected text cut to CodexTextMax bytes. The banner keeps its colored
// brand word unless NO_COLOR is set: Codex's TUI shows the hook's message
// without the escape codes (checks/4-approvals-with-hints/tui-screen.txt).
func (Codex) EncodeHook(out HookOutput) []byte {
	out.Context, out.Block, out.Deny = fitCodex(out.Context), fitCodex(out.Block), fitCodex(out.Deny)
	return Claude{}.EncodeHook(out)
}

// fitCodex is s within CodexTextMax bytes: whole as long as it fits, else
// cut at the last line that fits (at a character when that line is the
// first), then codexCut.
func fitCodex(s string) string {
	if len(s) <= CodexTextMax {
		return s
	}
	n := CodexTextMax - len(codexCut)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + codexCut
}

// QuestionTool is Codex's question tool, request_user_input, which the
// question gate stops once per prompt (0085 D3).
func (Codex) QuestionTool() string { return codexQuestionTool }

// codexMachineRE: a prompt Codex wrote itself, not the user: the user-role
// fragments Codex records in a conversation (core/src/context, the
// CONTEXTUAL_USER_FRAGMENT_MATCHERS of 0.167: AGENTS.md, the environment,
// a shell command the user ran, an aborted turn, a subagent's or agent's
// notification, a skill, internal context, a goal) and a stop hook's
// continuation (<hook_prompt>). Codex fires UserPromptSubmit for user input
// only, so none is expected there; the filter keeps one that arrives out
// of a recall.
var codexMachineRE = regexp.MustCompile(`^\s*(?:<(?:environment_context|user_instructions|user_shell_command|turn_aborted|subagent_notification|agent_message_board_notification|skill|hook_prompt|goal_context|codex_internal_context)\b|# AGENTS\.md instructions\b)`)

// MachinePrompt reports a prompt Codex wrote itself (codexMachineRE): the
// prompt hook skips it, as it does Claude Code's own messages.
func (Codex) MachinePrompt(prompt string) bool { return codexMachineRE.MatchString(prompt) }
