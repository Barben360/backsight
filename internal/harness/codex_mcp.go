package harness

import (
	"path/filepath"
	"slices"

	"github.com/Barben360/backsight/internal/session"
)

// Codex's memory-call identity: block 5
// (https://github.com/Barben360/backsight/issues/280), 0085 D4 (and D6's
// tool hints, which the server declares for every harness). The server's
// command line is codex.go's contract (MCPArgs, MCPEnvVars).
//
// Codex gives its memory server no session in the environment, and one
// server process may serve several threads (a subagent's, or, under
// Codex's MCP sharing flags, several chats'): the session is read from each
// call's _meta, never kept per process. The server (cli/mcp.go) knows a
// harness of this kind by PairsByPID false.
//
// What _meta carries and what is used (C14 §6-§7, mcp/*.in.jsonl):
//   - sessionId: the root session, equal to the hooks' session_id, in a
//     subagent too, after /clear, resume and fork: the call's session;
//   - threadId: the thread, a subagent's own inside one: never the session;
//   - x-codex-turn-metadata.session_id: the same root session, a
//     cross-check (a call whose two ids differ has none);
//   - x-codex-turn-metadata.workspaces: the turn's directories (MCPCallDirs);
//   - x-codex-turn-metadata.turn_id and callId: Codex's turn and the hooks'
//     tool_use_id. Unused by the server: it journals only the recall line,
//     under backsight's own turn pointer that the hooks keep (SPEC §7);
//     shows and credits, which pair a call with its hook, stay the
//     PostToolUse hook's (block 4).
//
// supports_parallel_tool_calls (a Codex MCP server setting, off unless
// set): left off, so the integration writes nothing for it. The server
// itself is safe under concurrent calls (a shared process serves several
// threads at once, tested), but in one thread a memory_credit must follow
// the search whose shows it names, and a wrong or outdated verdict counts
// the session's earlier retirements (feedback.max_retirements) from the
// journal the PostToolUse hook writes after each call: run side by side,
// the credit is refused and two verdicts can pass the cap together. Codex
// running one thread's memory calls one after the other keeps both. The
// gain would be small: one memory_search takes up to four queries.

// CallDirs is a harness whose memory calls name the directories their turn
// works in (Codex): the code that needs them asserts an adapter to it.
type CallDirs interface {
	MCPCallDirs(meta map[string]any) []string
}

var _ CallDirs = Codex{}

// codexTurnMeta is the key of Codex's turn metadata in a call's _meta.
const codexTurnMeta = "x-codex-turn-metadata"

// MCPCallSession is the call's _meta.sessionId, when it is a session id
// and the turn's metadata names no other; "" otherwise. getenv is never
// read: a server's environment is per process, and a process may serve
// several sessions.
func (Codex) MCPCallSession(meta map[string]any, _ func(string) string) string {
	s, _ := meta["sessionId"].(string)
	if !session.ValidID(s) {
		return ""
	}
	if raw, ok := meta[codexTurnMeta]; ok {
		turn, ok := raw.(map[string]any)
		if !ok {
			return ""
		}
		if other, ok := turn["session_id"]; ok && other != s {
			return ""
		}
	}
	return s
}

// MCPCallDirs are the absolute directories the call's turn works in (its
// workspaces), sorted; none when its _meta does not say. The server checks
// them against the privacy settings when no hook record gives the call's
// session directory.
func (Codex) MCPCallDirs(meta map[string]any) []string {
	turn, _ := meta[codexTurnMeta].(map[string]any)
	ws, _ := turn["workspaces"].(map[string]any)
	var dirs []string
	for dir := range ws {
		if filepath.IsAbs(dir) {
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	return dirs
}
