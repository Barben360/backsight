// Package harness is the seam between backsight and the coding tool it is
// wired into (a harness: Claude Code today, Codex later). Everything that
// differs from one tool to the next sits behind an Adapter: where the
// tool keeps its files and which profile it runs with, how to detect it
// and its version, how backsight's integration is installed, removed and
// checked for drift, the hook events it sends and the answers it reads
// back, where its conversations are and how they parse, the skills and
// tool names it sees, the harness: tag backsight's memories carry, and the
// words the user reads about it.
//
// The rest of backsight talks to an Adapter, found by its id in the
// registry (ByID, All), or taken from a context (FromContext) by code that
// runs on behalf of one harness, such as a hook. Claude Code's adapter
// (Claude) is always registered first; another one registers next to it
// (Register) without touching Claude Code's path.
//
// "harness" is a code word only: the user reads the tool's own name
// (Adapter.Name), never "harness". The word "client" stays for a client PC
// of a shared server.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/transcript"
)

// Harness ids: the value of each harness's harness: tag, the key of its
// config and state entries, and the client field of statistics
// (ClaudeCode for records that name none). They are internal/harnessid's,
// the leaf package the state, the statistics writers and retain share:
// none of them can import this package (its adapters import
// instructions, whose tests import their users). Codex has no adapter
// yet.
const (
	ClaudeCode = harnessid.ClaudeCode
	Codex      = harnessid.Codex
)

// Adapter is one harness. Its methods are grouped in small interfaces, so
// code that needs one concern (a hook decoder, a conversation reader)
// asks for that one only. An adapter holds no state of its own: what
// varies per machine and per call comes in an Env.
type Adapter interface {
	Identity
	Home
	Installer
	Hooks
	Conversations
	Tools
}

// Identity names a harness.
type Identity interface {
	// ID is the harness id, e.g. "claude-code": the registry key, the
	// value of the harness: tag and of the statistics' client field.
	ID() string
	// Name is the tool's name as the user reads it, e.g. "Claude Code".
	Name() string
	// Tag is the harness: tag every memory sent for this harness carries,
	// e.g. "harness:claude-code".
	Tag() string
	// Texts are the other user-visible words about the tool.
	Texts() Texts
}

// Texts are the user-visible strings an adapter supplies. None names a
// decision, an internal block or the word "harness".
type Texts struct {
	// ProfileFlag is the help of the flag that picks the tool's directory
	// (--config-dir), naming its default.
	ProfileFlag string
	// InstallCmd installs the tool; InstallWhy says why backsight needs it.
	InstallCmd, InstallWhy string
}

// Env is what the caller hands an adapter for one call: the user's home,
// the profile to act on, and how to run the tool's own command. The
// caller decides the profile (a flag, the shell, or the one recorded at
// install): an adapter never reads it from this process's environment,
// so a shell set to another profile never redirects a call meant for the
// recorded one.
type Env struct {
	// Home is the user's home directory.
	Home string
	// Profile is the tool's directory to act on; "" is its default one
	// (~/.claude for Claude Code).
	Profile string
	// Run runs the tool's command line in this profile; nil when the call
	// runs nothing (a hook, a conversation scan).
	Run Runner
}

// Runner runs the tool's command with extra environment entries env and
// arguments args, returning its standard output. It fails with ErrMissing
// when the command is not installed.
type Runner func(ctx context.Context, env []string, args ...string) ([]byte, error)

// ErrMissing: the tool's command was not found. A Runner returns it, a
// MissingError or an error wrapping either, so that callers tell a
// missing tool from a failing one (errors.Is(err, ErrMissing)).
var ErrMissing = errors.New("the tool's command was not found")

// MissingError is ErrMissing naming the command.
type MissingError struct{ Command string }

func (e MissingError) Error() string        { return "the " + e.Command + " command was not found" }
func (e MissingError) Is(target error) bool { return target == ErrMissing }

// Home locates a harness's files.
type Home interface {
	// ProfileVar is the environment variable that moves the tool's
	// directory (CLAUDE_CONFIG_DIR), "" when there is none. A caller reads
	// it from the shell when the user asked for the shell's profile.
	ProfileVar() string
	// IsDefaultProfile reports that profile ("" included) is the tool's
	// default directory, also when named another way (a symlink).
	IsDefaultProfile(home, profile string) bool
	// Locate is where the tool keeps its files for env's profile.
	Locate(env Env) Location
}

// Location is where a harness keeps the files backsight reads or edits.
type Location struct {
	// Dir is the tool's directory (the profile).
	Dir string
	// Settings is the user settings file backsight's edits go to.
	Settings string
	// Conversations is the directory the tool stores conversations in.
	Conversations string
}

// Installer detects the tool and installs, removes and checks backsight's
// integration in it (hooks, the memory tools, skills).
type Installer interface {
	// Detect runs the tool for its version; ErrMissing when it is not
	// installed.
	Detect(ctx context.Context, env Env) (version string, err error)
	// MinVersion is the oldest version backsight was verified on.
	MinVersion() string
	// Install generates backsight's integration for in and registers it
	// with the tool in env's profile; with plan, it only says what it
	// would do (running the tool's read-only commands only). The steps
	// are reported in order, also on failure; the first error stops it
	// (an *InstallError says how far it went).
	Install(ctx context.Context, env Env, in Integration, plan bool) (InstallResult, error)
	// Uninstall removes backsight's integration from env's profile, when
	// it is there.
	Uninstall(ctx context.Context, env Env) error
	// Installed reports whether any of backsight's integration is
	// registered in env's profile (read only).
	Installed(ctx context.Context, env Env) (bool, error)
	// Drift lists what no longer matches the integration recorded at
	// install, in env's profile; cfg nil (unreadable) accepts the
	// config-dependent wiring installed. Cheap: a session start runs it.
	Drift(env Env, rec Recorded, cfg *config.Config) []string
}

// Integration is what an install generates from: backsight's data
// directory, the stable shim every generated file runs, the binary's
// version, and the config (whose switches wire optional hooks).
type Integration struct {
	Data, Shim, BinaryVersion string
	Config                    config.Config
}

// Recorded is what the last install recorded: the integration's version
// and the binary the shim pointed to, for this data directory and binary.
type Recorded struct {
	Version, ShimTarget string
	Data, BinaryVersion string
}

// InstallResult is what an install did: its steps, and the version of
// the integration it generated (recorded as Recorded.Version).
type InstallResult struct {
	Version string
	Steps   []Step
}

// Step is one thing an install did or would do. Changed is false for a
// step that found everything in place.
type Step struct {
	Name, Detail string
	Changed      bool
}

// InstallError is an Install failure and how far the install went.
type InstallError struct {
	Stage Stage
	Err   error
}

func (e *InstallError) Error() string { return e.Err.Error() }
func (e *InstallError) Unwrap() error { return e.Err }

// Stage is how far a failed Install went.
type Stage int

const (
	// Prepared: nothing of the tool's was written.
	Prepared Stage = iota
	// Generated: backsight's own files were written, not yet registered.
	Generated
	// Registering: some registration commands ran.
	Registering
	// Conflict: the profile is wired to another backsight install; Err's
	// text says what to do (user-visible).
	Conflict
)

// Hooks decodes the hook events a harness sends and encodes the answers
// it reads back, and pairs a session's hooks with the memory server it
// starts.
type Hooks interface {
	// DecodeHook parses event's input (event is backsight's id, e.g.
	// EventSessionStart).
	DecodeHook(event string, raw []byte) (HookInput, error)
	// EncodeHook is the answer the tool reads; nil prints nothing.
	EncodeHook(out HookOutput) []byte
	// MCPCallSession is the session a memory tool call belongs to, from
	// the call's _meta (meta, nil when the call has none) and the memory
	// server's environment (getenv, nil when none); "" when neither names
	// one. It is asked on every call, never once per server: a harness may
	// give each call its session (and share one server between several),
	// another hands its server one at start. The session is the one its
	// hooks name (HookInput.SessionID), the key of the record they keep.
	MCPCallSession(meta map[string]any, getenv func(string) string) string
	// PairsBySession says hooks also record their session under its id,
	// because the tool's process tree may not pair the memory server with
	// the hooks by process id on goos.
	PairsBySession(goos string) bool
}

// backsight's hook events: the argument of `backsight hook <event>`,
// whatever the harness calls them.
const (
	EventSessionStart       = "session-start"
	EventUserPromptSubmit   = "user-prompt-submit"
	EventPreToolUse         = "pre-tool-use"
	EventPostToolUse        = "post-tool-use"
	EventPostToolUseFailure = "post-tool-use-failure"
	EventSubagentStart      = "subagent-start"
	EventSubagentStop       = "subagent-stop"
	EventStop               = "stop"
	EventSessionEnd         = "session-end"
)

// HookInput is a hook event, decoded: the same fields for every harness;
// one that sends no such field leaves it zero.
type HookInput struct {
	SessionID            string
	PromptID             string
	TranscriptPath       string
	Cwd                  string
	Source               string // session start: startup, resume, clear, compact, fork
	Prompt               string
	ToolName             string
	ToolInput            json.RawMessage
	ToolResponse         json.RawMessage
	AgentID              string
	AgentType            string
	AgentTranscriptPath  string
	LastAssistantMessage string
	Reason               string
	Error                string // a failed tool call
	IsInterrupt          bool   // a failed tool call: the user interrupted it
	StopHookActive       bool   // a stop: the turn goes on from a stop hook's block
}

// HookOutput is a hook's answer before encoding. At most one of Block and
// Deny is set; either one wins over Context and Banner.
type HookOutput struct {
	// Event is backsight's event id the answer is for.
	Event string
	// Context is text added to the agent's context.
	Context string
	// Banner is a message shown to the user.
	Banner string
	// Block keeps a stopping turn going, with this reason for the agent.
	Block string
	// Deny refuses the tool call, with this reason for the agent.
	Deny string
}

// Conversations finds and parses the conversations a harness stores.
//
// A conversation is a file, and its content stream is what the
// conversation reads as: the file's bytes, or what they decompress to
// when the harness stores it compressed (an archived conversation). Every
// offset backsight keeps (a cursor, a record's Start and End, what a read
// consumed) is an offset into that stream, never into the file; the
// adapter does any decompression in Open and Size. Cursors, locks and the
// quarantine are keyed by SessionOf, never by the file name.
type Conversations interface {
	// Conversations lists the conversation files of env's profile, sorted.
	Conversations(env Env) []string
	// SessionOf is the session id of the conversation at path, "" when the
	// path is not one of this harness's conversations.
	SessionOf(path string) string
	// Size is the length of path's content stream.
	Size(path string) (int64, error)
	// Open is path's content stream from offset from on; the caller closes
	// it.
	Open(path string, from int64) (io.ReadCloser, error)
	// ReadConversation normalises the complete records of r, which starts
	// at offset from of a conversation's content stream (transcript.Read's
	// contract: records, what was consumed, the unknown lines).
	ReadConversation(r io.Reader, from int64) ([]transcript.Record, transcript.Stats, error)
}

// Tools are the names the harness gives backsight's memory tools.
type Tools interface {
	// ToolPrefix starts the name of every backsight tool, e.g.
	// "mcp__plugin_backsight_memory__".
	ToolPrefix() string
	// ToolNames are the memory tools' full names.
	ToolNames() []string
	// Skills are backsight's skill files for shim, by path relative to
	// the integration's root.
	Skills(shim string) map[string][]byte
}
