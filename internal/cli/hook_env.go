package cli

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// hookEnv is what every handler needs, resolved once per event.
type hookEnv struct {
	ctx context.Context
	// adapter is the harness the event came from (the context's, Claude
	// Code's by default): it decoded in and encodes the answer.
	adapter harness.Adapter
	in      harness.HookInput
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
	// generation is the sends generation read before cfg: the briefing
	// page is written only while it still holds (sendsGate).
	generation int
	store      session.Store
	tw         *telemetry.Writer
	client     *hindsight.Client
	// journal records the memories shown to the agent (feedback); turn is
	// the turn this process started, when turnSet.
	journal *feedback.Journal
	turn    string
	turnSet bool
}

// Which component an event belongs to (A.1 components).
const (
	readSide  = "read"
	writeSide = "write"
)

// newHookEnv parses the event; ok false means print nothing: no config yet,
// an unreadable event, or the event's component turned off.
func newHookEnv(ctx context.Context, input []byte, side string) (*hookEnv, bool) {
	h := &hookEnv{ctx: ctx, adapter: harness.FromContext(ctx)}
	var err error
	if h.in, err = h.adapter.DecodeHook(hookEvent(ctx), input); err != nil {
		return nil, false
	}
	if h.dirs, h.home, err = machine(); err != nil {
		return nil, false
	}
	h.generation = sendsGeneration(h.dirs)
	if h.cfg, h.sec, err = loadServerConfig(); err != nil {
		return nil, false
	}
	sendsConfigLoaded()
	cwd := h.in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	h.resolver, h.cwd = project.New(h.home, h.cfg.Projects.Transverse), cwd
	h.project = h.resolver.Project(cwd)
	h.linked = linkedProjects(h.dirs, h.cfg, h.resolver, cwd, h.project)
	// The live record is what lets pause and the MCP server find this
	// session: written whatever the components (0056).
	live := session.Live{SessionID: h.in.SessionID, Cwd: cwd, Project: h.project, At: now()}
	pid, err := strconv.Atoi(os.Getenv(claudePIDEnv))
	if err == nil && h.adapter.PairsByPID() {
		_ = session.WriteLive(h.dirs.SessionsDir(), pid, live)
	}
	// Under the session id too where the pid may not pair (0080): where
	// the harness's process tree may differ (Claude Code on Windows), or
	// without the harness's pid. Elsewhere the pid record is enough.
	if h.adapter.PairsBySession(runtime.GOOS) || err != nil {
		_ = session.WriteLiveSession(h.dirs.SessionsDir(), live)
	}
	c := h.cfg.Components
	if (side == readSide && !c.Read) || (side == writeSide && !c.Write) || (side == "" && !c.Read && !c.Write) {
		return nil, false
	}
	// Hooks of one session can run in parallel: a short bounded retry
	// instead of losing their updates (0054 amendment).
	h.store = session.Store{Dir: h.dirs.SessionsDir(), Locks: h.dirs.LocksDir(), Retry: 100 * time.Millisecond}
	h.tw = telemetry.New(h.dirs.State, telemetry.Common{V: instructions.Version, Session: h.in.SessionID, Project: h.project, BinaryVersion: buildinfo.Version, Client: h.adapter.ID()})
	h.journal = feedback.NewJournal(h.dirs.FeedbackDir(), feedback.Common{Session: h.in.SessionID, Project: h.project, Binary: buildinfo.Version, V: instructions.Version, Client: h.adapter.ID()})
	// One deadline for every call of a hook: the supervisor's own is the
	// hard stop, this one lets the handler finish with what it has.
	h.client, err = hindsight.New(h.cfg.Server.URL, h.sec.APIKey, 30*time.Second)
	return h, err == nil
}

// shellSession is the session a backsight command run from the agent's
// shell belongs to: BACKSIGHT_SESSION, which Claude Code's shells get from
// the SessionStart hook's env file, else CODEX_SESSION_ID, which Codex
// gives every shell command it runs: the root session, also in a
// subagent's shell, the session_id its hooks key their records on (0085
// D4; never CODEX_THREAD_ID, a subagent's own thread). "" when neither is
// set (an invalid CODEX_SESSION_ID counts as unset), and "" too when
// CLAUDE_CODE_SESSION_ID is set: the command runs under Claude Code (one
// started from a Codex shell inherits its CODEX_SESSION_ID), which keeps
// the session each command takes without this variable (store's
// CLAUDE_CODE_SESSION_ID, why's latest session, none for pause).
func shellSession(getenv func(string) string) string {
	if s := getenv("BACKSIGHT_SESSION"); s != "" {
		return s
	}
	if getenv("CLAUDE_CODE_SESSION_ID") != "" {
		return ""
	}
	if s := getenv("CODEX_SESSION_ID"); session.ValidID(s) {
		return s
	}
	return ""
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

// answer is a hook answer to backsight's event, as the harness reads it:
// text for the agent, a banner for the user.
func (h *hookEnv) answer(event, text, banner string) []byte {
	return h.adapter.EncodeHook(harness.HookOutput{Event: event, Context: text, Banner: banner})
}

// hookEventKey carries backsight's event id to the handler's decoder.
type hookEventKey struct{}

// withHookEvent is ctx for a handler of event.
func withHookEvent(ctx context.Context, event string) context.Context {
	return context.WithValue(ctx, hookEventKey{}, event)
}

// hookEvent is the event withHookEvent set, "" when none (a command that
// builds its own input, such as recall-prompt).
func hookEvent(ctx context.Context) string {
	ev, _ := ctx.Value(hookEventKey{}).(string)
	return ev
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
