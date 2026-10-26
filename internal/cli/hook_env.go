package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// hookInput is the event JSON Claude Code sends (C06 §2, §4).
type hookInput struct {
	SessionID            string          `json:"session_id"`
	PromptID             string          `json:"prompt_id"`
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
}

// hookEnv is what every handler needs, resolved once per event.
type hookEnv struct {
	ctx     context.Context
	in      hookInput
	dirs    paths.Dirs
	home    string
	cfg     config.Config
	sec     config.Secrets
	project string
	linked  []string // the projects cwd's .backsight.toml relates, then the project's family
	// resolver named project from cwd; remote reads the repository's
	// remote from it, Explicit whether a .backsight.toml set the name
	// (decision 0045).
	resolver *project.Resolver
	cwd      string
	store    session.Store
	tw       *telemetry.Writer
	client   *hindsight.Client
}

// Which component an event belongs to (A.1 components).
const (
	readSide  = "read"
	writeSide = "write"
)

// newHookEnv parses the event; ok false means print nothing: no config yet,
// an unreadable event, or the event's component turned off.
func newHookEnv(ctx context.Context, input []byte, side string) (*hookEnv, bool) {
	h := &hookEnv{ctx: ctx}
	if json.Unmarshal(input, &h.in) != nil {
		return nil, false
	}
	var err error
	if h.dirs, h.home, err = machine(); err != nil {
		return nil, false
	}
	if h.cfg, h.sec, err = loadServerConfig(); err != nil {
		return nil, false
	}
	cwd := h.in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	h.resolver, h.cwd = project.New(h.home, h.cfg.Projects.Transverse), cwd
	h.project = h.resolver.Project(cwd)
	h.linked = linkedProjects(h.dirs, h.cfg, h.resolver, cwd, h.project)
	// The live record is what lets pause and the MCP server find this
	// session: written whatever the components (0056).
	if pid, err := strconv.Atoi(os.Getenv(claudePIDEnv)); err == nil {
		_ = session.WriteLive(h.dirs.SessionsDir(), pid, session.Live{SessionID: h.in.SessionID, Cwd: cwd, Project: h.project, At: now()})
	}
	c := h.cfg.Components
	if (side == readSide && !c.Read) || (side == writeSide && !c.Write) || (side == "" && !c.Read && !c.Write) {
		return nil, false
	}
	// Hooks of one session can run in parallel: a short bounded retry
	// instead of losing their updates (0054 amendment).
	h.store = session.Store{Dir: h.dirs.SessionsDir(), Locks: h.dirs.LocksDir(), Retry: 100 * time.Millisecond}
	h.tw = telemetry.New(h.dirs.State, telemetry.Common{V: instructions.Version, Session: h.in.SessionID, Project: h.project, BinaryVersion: buildinfo.Version})
	// One deadline for every call of a hook: the supervisor's own is the
	// hard stop, this one lets the handler finish with what it has.
	h.client, err = hindsight.New(h.cfg.Server.URL, h.sec.APIKey, 30*time.Second)
	return h, err == nil
}

// remote is the normalised git remote of the session's directory, recorded
// with its project so doctor can tell two repositories sharing a name.
func (h *hookEnv) remote() string { return h.resolver.Remote(h.cwd) }

// updateSession applies fn to this session's state; a busy state is
// skipped, never waited for (SPEC §6.1).
func (h *hookEnv) updateSession(fn func(*session.State) error) {
	if h.in.SessionID == "" {
		return
	}
	if err := h.store.Update(h.in.SessionID, fn); err != nil && !errors.Is(err, session.ErrBusy) {
		h.tw.Emit(telemetry.Raw{Name: "session_state_error", Fields: map[string]any{"error": err.Error()}})
	}
}

// maintenance: a maintenance lock is held; hooks only queue (SPEC §14).
func (h *hookEnv) maintenance() bool {
	return state.MaintenanceHeld(h.dirs.MaintenanceLock())
}

// safeProject renders a project name that came from the server or a tool
// call: one line, no control characters, short (memory text is data and
// must not break out of its framing).
func safeProject(p string) string {
	p = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, p)
	return clip(p, 60)
}

// contextOutput is a hook answer: text for Claude, a banner for the user.
func contextOutput(event, text, banner string) []byte {
	out := map[string]any{}
	if text != "" {
		out["hookSpecificOutput"] = map[string]string{"hookEventName": event, "additionalContext": text}
	}
	if banner != "" {
		out["systemMessage"] = banner
	}
	if len(out) == 0 {
		return nil
	}
	raw, _ := json.Marshal(out)
	return raw
}

// shellQuote quotes s for a POSIX shell (the env file is sourced by bash).
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// clip shortens s to n runes with an ellipsis.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
