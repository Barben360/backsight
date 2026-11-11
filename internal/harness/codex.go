package harness

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/plugin"
)

// Codex is Codex's adapter (decision 0085, D1). This file is its shape:
// the type, its identity and tool names, and the contracts the blocks
// that fill it agree on. Each other part lives in one file per owning
// block, every method there a stub until its block implements it:
//
//   - codex_install.go: home and profile, detection and version, install,
//     uninstall and drift (block 3, https://github.com/Barben360/backsight/issues/278;
//     0085 D2, D5, D8, D9);
//   - codex_hooks.go: hook decoding and answers, the event map (block 4,
//     https://github.com/Barben360/backsight/issues/279; 0085 D3);
//   - codex_mcp.go: each memory call's session (block 5,
//     https://github.com/Barben360/backsight/issues/280; 0085 D4);
//   - codex_rollout.go: the conversation store, rollouts (block 6,
//     https://github.com/Barben360/backsight/issues/281; 0085 D7);
//   - codex_texts.go: skills and user-facing strings (block 7,
//     https://github.com/Barben360/backsight/issues/282; 0085 D1 texts).
//
// A block touches this file only to add a field, never to restructure it.
//
// # Off until its blocks land
//
// Codex is known to ByID but not on: it is not in All (which the flush
// loop and every per-harness pass iterate), it is never Default, and no
// command detects or wires it on a user's machine. A command line that
// names it (`backsight hook --harness codex …`, `backsight mcp --harness
// codex`) reaches its stubs, which answer nothing: a hook prints nothing,
// a memory call has no session. It is turned on by moving it from known to
// adapters in registry.go, once blocks 3 (install records a Codex home)
// and 6 (rollouts read) have landed; setup and doctor offer it from block
// 3 on, through their own code.
//
// # Contracts
//
// Hook command line (0085 D2). Codex runs a hook handler as one shell
// string and ignores the exec form's args. The handler for backsight's
// event E (an Event* id) is the shim, quoted for the platform's shell,
// followed by HookArgs(E):
//
//	"<shim>" hook --harness codex <E>
//
// The parent `backsight hook` already reads `--harness <id>` before the
// event (2A) and hands the child the harness. The generator rule, as
// built (it amends 0085's "every command ends with `hook <event>`"): the
// hooks.json of Codex has no args field, and every handler's command ends
// with `hook --harness codex <event>`, the event its last word.
//
// Quoting (0085 D2). HookCommand (block 3) writes "command" for sh and
// "commandWindows" for cmd, with QuoteSh and QuoteCmd (codex_quote.go):
//   - sh: POSIX single quotes, a single quote written '\” and a
//     backslash '\\' (both outside the quotes), which every shell Codex
//     may run the command with ($SHELL -lc: sh, bash, zsh, fish) reads
//     back as the same path, $, %, & and spaces included;
//   - cmd: the path in double quotes, for cmd /C "<command>"; a path
//     holding a double quote, a %, or a control character is refused, as
//     cmd cannot be made to read it back reliably.
//
// Block 3 tests both on paths with spaces, $, ', %, & (cmd on Windows: its
// run comes from Codex's source only, unverified).
//
// Memory server (0085 D2, D4). Codex's .mcp.json uses the exec form, whose
// args Codex honours there: command the shim, args MCPArgs (`mcp --harness
// codex`, the server's hidden flag), env_vars MCPEnvVars (BACKSIGHT_HOME,
// so a non-default home reaches the server, which gets almost no other
// environment). The server then asks MCPCallSession for each call's
// session (block 5), and keeps no by-pid pairing (PairsByPID false).
//
// Hook events (0085 D3). The Codex events backsight wires, their matchers
// and timeouts, and the backsight event each one runs, are the table in
// codex_hooks.go (Events). The hooks.json generator groups the rows by
// Name, in order (SessionStart has two rows, two matchers). Codex sends
// Claude Code's field names plus a few, in HookInput already (TurnID,
// ToolUseID, Model, PermissionMode; block 4 decodes them).
//
// Home and profile (0085 D1). The Codex home is $CODEX_HOME when set and
// non-empty, else ~/.codex, overridden by --codex-home; it is resolved
// once, at setup or client install (ShellHome), and recorded in the
// state's per-harness entry, state.ClientFor(harnessid.Codex).Profile
// ("" for ~/.codex) (RecordHome); every later command reads the recorded
// one (RecordedHome), never the shell's. ProfileVar is therefore "", so
// the shared wiredProfile never reads CODEX_HOME from a shell. These are
// the HomeRecorder and HookCommander interfaces (harness.go): the code
// that needs them asserts an adapter to them, so block 3 fills only
// codex_install.go (and the codex command runner, cli/codex_cli.go).
type Codex struct{}

var (
	_ Adapter       = Codex{}
	_ HookCommander = Codex{}
	_ HomeRecorder  = Codex{}
)

// ErrNotImplemented: a Codex method its block has not implemented yet.
var ErrNotImplemented = errors.New("not available for Codex yet")

// notYet is ErrNotImplemented for what, on one line.
func notYet(what string) error { return fmt.Errorf("codex %s: %w", what, ErrNotImplemented) }

// codexToolPrefix starts every memory tool's name under Codex: its MCP
// tools are mcp__<server>__<tool>, the server being "memory" (0085 D1).
const codexToolPrefix = "mcp__" + plugin.Server + "__"

func (Codex) ID() string         { return harnessid.Codex }
func (Codex) Name() string       { return "Codex" }
func (Codex) Tag() string        { return "harness:" + harnessid.Codex }
func (Codex) ToolPrefix() string { return codexToolPrefix }

// ToolNames are Claude Code's four memory tools under Codex's prefix.
func (Codex) ToolNames() []string {
	out := make([]string, 0, len(plugin.ToolNames))
	for _, n := range plugin.ToolNames {
		out = append(out, codexToolPrefix+strings.TrimPrefix(n, Claude{}.ToolPrefix()))
	}
	return out
}

// HookArgs are the words after the shim in the hook handler for
// backsight's event (the hook command line contract above).
func (Codex) HookArgs(event string) []string {
	return []string{"hook", "--harness", harnessid.Codex, event}
}

// MCPArgs are the memory server's args in Codex's .mcp.json.
func (Codex) MCPArgs() []string { return []string{"mcp", "--harness", harnessid.Codex} }

// MCPEnvVars are the variables Codex passes the memory server from its
// own environment (.mcp.json env_vars).
func (Codex) MCPEnvVars() []string { return slices.Clone(codexMCPEnvVars) }

var codexMCPEnvVars = []string{"BACKSIGHT_HOME"}
