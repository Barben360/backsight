package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

const (
	briefingItems     = 5
	briefingItemChars = 220
	briefingChars     = 1200
	briefingTimeout   = 5 * time.Second
	// The mental-model briefing (decision 0060): its page is cut at a line
	// past this many characters; the server writes it within 600 tokens.
	briefingModelChars  = 1600
	briefingModelTokens = 600
	briefingModelPrefix = "backsight-brief-"
	// briefingModelTimeout bounds the page read, and the page creation,
	// within the briefing's own budget.
	briefingModelTimeout = 1500 * time.Millisecond
	// briefingIdleDays: a project quiet this long loses its page (jobs tick),
	// so the server stops refreshing it.
	briefingIdleDays  = 14
	compactReplay     = 6
	compactStores     = 10
	subagentItems     = 2
	subagentChars     = 600
	subagentItemChars = 180
)

func init() {
	handlers["session-start"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, "")
		if !ok {
			return nil, nil
		}
		if !h.cfg.Components.Read {
			// Write side only: no guide or briefing, but the env file, so
			// that pause can name this session.
			if h.in.Source != "compact" {
				h.writeEnvFile()
			}
			return nil, nil
		}
		return h.sessionStart(), nil
	}
	handlers["subagent-start"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, readSide)
		if !ok {
			return nil, nil
		}
		return h.subagentStart(), nil
	}
}

// sessionStart (SPEC §6.2): guide always; at startup also the briefing, the
// env file, the banner and the one-time notice; after a compaction the
// replay of recent injections and stores.
func (h *hookEnv) sessionStart() []byte {
	switch h.in.Source {
	case "startup", "":
	case "compact":
		replay := h.compactReplay()
		// After a compaction only what was replayed is still in context:
		// the rest of the seen set may be recalled again.
		h.updateSession(func(st *session.State) error {
			var keep []string
			for _, in := range st.Injected[max(0, len(st.Injected)-compactReplay):] {
				keep = append(keep, in.ID)
			}
			st.Seen = keep
			return nil
		})
		return contextOutput("SessionStart", instructions.Guide+replay, "")
	default: // resume, clear, fork: clear and fork start a new session id
		h.writeEnvFile()
		return contextOutput("SessionStart", instructions.Guide, "")
	}
	h.writeEnvFile()
	project := safeProject(h.project)
	var text, source string
	var items []session.Injection
	var err error
	banner := "backsight · project " + project + " · memory on"
	unavailable := false
	switch {
	case h.maintenance():
		banner, unavailable = "backsight · project "+project+" · "+instructions.MemoryUnavailable("maintenance in progress"), true
	case h.breakerOpen(): // the server failed again and again: no calls for a while
		banner, unavailable = "backsight · project "+project+" · "+instructions.MemoryUnavailable("server failing, retried in a few minutes"), true
	default:
		text, items, source, err = h.briefing()
		if err != nil {
			banner, unavailable = "backsight · project "+project+" · "+instructions.MemoryUnavailable("server unreachable"), true
		}
	}
	if st, err := state.Load(h.dirs.StateFile()); err == nil {
		settings := claudecode.Locate(h.home, os.Getenv).Settings
		if drift := plugin.Drift(h.dirs.Data, settings, plugin.Installed{Version: st.PluginVersion, ShimTarget: st.ShimTarget}, buildinfo.Version); len(drift) > 0 {
			banner += "\nbacksight: Claude Code wiring drifted (" + drift[0] + "): run `backsight client install`"
		}
	}
	if held, _ := retain.QuarantinedSessions(h.dirs.CursorsDir()); len(held) > 0 {
		banner += fmt.Sprintf("\nbacksight: %d session(s) quarantined: run `backsight doctor`", len(held))
	}
	if h.firstSession() {
		banner = instructions.FirstSessionNotice(h.cfg.Server.URL, h.cfg.LLM.Provider) + "\n" + banner
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	h.updateSession(func(st *session.State) error {
		st.Project = h.project
		st.Unavailable = st.Unavailable || unavailable
		if source == "model" {
			st.BriefingPage = text
		}
		for _, it := range items {
			if !slices.Contains(st.Seen, it.ID) {
				st.Seen = append(st.Seen, it.ID)
			}
			st.Injected = append(st.Injected, it)
		}
		return nil
	})
	if err == nil {
		h.tw.Emit(telemetry.Briefing{IDs: ids, Chars: len([]rune(text)), Source: source})
		// Briefed ids join this session's seen set only: counting them in the
		// cross-session sticky muted them in sessions that never got the
		// briefing (resume, clear, fork).
	}
	context := instructions.Guide
	if text != "" {
		context += "\n\n" + text
	}
	return contextOutput("SessionStart", context, banner)
}

// briefing: the project's most recently updated observations, else its
// latest raw facts (backfill), else the empty-memory line.
func (h *hookEnv) briefing() (text string, items []session.Injection, source string, err error) {
	ctx, cancel := context.WithTimeout(h.ctx, briefingTimeout)
	defer cancel()
	// An excluded project gets no page: creating one would send its name to
	// the server and have the server's LLM summarise it.
	optedOut, _ := project.New(h.home, h.cfg.Projects.Transverse).OptedOut(h.in.Cwd, h.cfg.Privacy.ExcludePaths)
	missing := false
	if h.cfg.Recall.Briefing == "model" && !optedOut {
		// The page read has its own short budget: a slow server must leave
		// the fallback briefing its time.
		mctx, mcancel := context.WithTimeout(ctx, briefingModelTimeout)
		text, ok, absent := h.modelBriefing(mctx)
		mcancel()
		if ok {
			return text, nil, "model", nil
		}
		missing = absent
	}
	text, items, source, err = h.recentBriefing(ctx)
	// A page is created only for a project with memories: an empty or new
	// one would be summarised from nothing, then pruned, in a loop.
	if missing && err == nil && source != "empty" {
		pctx, pcancel := context.WithTimeout(ctx, briefingModelTimeout)
		if cerr := h.client.CreateMentalModel(pctx, h.cfg.Server.Bank, briefingModelID(h.project), "backsight briefing: "+h.project, instructions.BriefingQuery,
			[]string{"project:" + h.project}, briefingModelTokens); cerr != nil {
			h.tw.Emit(telemetry.Raw{Name: "briefing_page_error", Fields: map[string]any{"error": cerr.Error()}})
		}
		pcancel()
	}
	return text, items, source, err
}

// recentBriefing is the E8 briefing: the project's most recent observations
// by event time (raw facts while none is consolidated yet).
func (h *hookEnv) recentBriefing(ctx context.Context) (text string, items []session.Injection, source string, err error) {
	tag := "project:" + h.project
	mems, err := h.client.Recent(ctx, h.cfg.Server.Bank, tag, "observation", briefingItems)
	if err != nil {
		return "", nil, "", err
	}
	source = "observations"
	if len(mems) == 0 {
		source = "raw"
		if mems, err = h.client.Recent(ctx, h.cfg.Server.Bank, tag, "", briefingItems); err != nil {
			return "", nil, "", err
		}
	}
	if len(mems) == 0 {
		return instructions.BriefingEmpty, nil, "empty", nil
	}
	var b strings.Builder
	b.WriteString(instructions.BriefingHeader(safeProject(h.project)) + "\n")
	if source == "raw" {
		b.WriteString(instructions.BriefingRaw + "\n")
	}
	at := now()
	for _, m := range mems {
		ago := ""
		if w := m.When(); !w.IsZero() {
			ago = recall.Age(at.Sub(w))
		}
		line := "- " + clip(m.Text, briefingItemChars) + age(ago) + "\n"
		if len([]rune(b.String()))+len([]rune(line)) > briefingChars {
			break
		}
		b.WriteString(line)
		items = append(items, session.Injection{ID: m.ID, Text: m.Text, Project: h.project, Scope: "project", Prompt: 0, Rule: "briefing", At: at})
	}
	return strings.TrimRight(b.String(), "\n"), items, source, nil
}

// briefingModelID is the project's mental-model id: a readable slug and a
// hash, so that two project names never share a page.
func briefingModelID(project string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(project) {
		if b.Len() >= 40 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	sum := sha256.Sum256([]byte(project))
	return briefingModelPrefix + b.String() + "-" + hex.EncodeToString(sum[:4])
}

// modelBriefing is the project's mental model, cut at a line boundary;
// absent reports a project without one (the caller creates it: the server
// writes it in the background, then refreshes it after each consolidation
// of the project's memories). Any failure falls back to E8.
func (h *hookEnv) modelBriefing(ctx context.Context) (text string, ok, absent bool) {
	m, err := h.client.GetMentalModel(ctx, h.cfg.Server.Bank, briefingModelID(h.project))
	var apiErr *hindsight.APIError
	if errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound {
		return "", false, true
	}
	content := strings.TrimSpace(m.Content)
	if err != nil || content == "" {
		return "", false, false
	}
	age := ""
	if t, err := time.Parse(time.RFC3339Nano, m.LastRefreshedAt); err == nil {
		age = recall.Age(now().Sub(t))
	}
	var b strings.Builder
	b.WriteString(instructions.BriefingModelHeader(safeProject(h.project), age) + "\n")
	// Server-written text: every line quoted, headings flattened, and a
	// closing line, so that the page reads as notes, not as instructions.
	header := b.Len()
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "# "))
		if line == "" {
			continue
		}
		if room := briefingModelChars - len([]rune(b.String())); len([]rune(line)) > room {
			if b.Len() == header && room > 60 {
				b.WriteString("> " + clip(line, room) + "\n") // a page written as one long line
			} else {
				b.WriteString("> …\n")
			}
			break
		}
		b.WriteString("> " + line + "\n")
	}
	b.WriteString(instructions.BriefingModelEnd)
	return b.String(), true, false
}

// writeEnvFile gives the CLI fallback its project and session (E15).
func (h *hookEnv) writeEnvFile() {
	path := os.Getenv("CLAUDE_ENV_FILE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "export BACKSIGHT_PROJECT=%s\nexport BACKSIGHT_SESSION=%s\n", shellQuote(h.project), shellQuote(h.in.SessionID))
	_ = f.Close()
}

// firstSession records, once, that the notice was shown; later starts only
// read the state (jobs write it too).
func (h *hookEnv) firstSession() bool {
	if st, err := state.Load(h.dirs.StateFile()); err != nil || st.FirstSessionNoticeShown {
		return false
	}
	first := false
	_ = state.Update(h.dirs, "hook session-start", func(s *state.State) error {
		if !s.FirstSessionNoticeShown {
			s.FirstSessionNoticeShown, first = true, true
		}
		return nil
	})
	return first
}

// compactReplay: the last injected memories and what was stored this
// session, so a compaction does not lose them.
func (h *hookEnv) compactReplay() string {
	st, err := h.store.Load(h.in.SessionID)
	if err != nil || (len(st.Injected) == 0 && len(st.Stored) == 0 && st.BriefingPage == "") {
		return ""
	}
	var b strings.Builder
	if st.BriefingPage != "" {
		b.WriteString("\n\n" + st.BriefingPage + "\n")
	}
	if len(st.Injected) > 0 {
		b.WriteString("\n\nMemories shown earlier in this session " + instructions.HintsLine + ":\n")
		for _, in := range st.Injected[max(0, len(st.Injected)-compactReplay):] {
			fmt.Fprintf(&b, "- [%s] %s\n", safeProject(in.Project), clip(in.Text, briefingItemChars))
		}
	}
	if len(st.Stored) > 0 {
		stored := st.Stored[max(0, len(st.Stored)-compactStores):]
		fmt.Fprintf(&b, "\nStored this session: %s\n", strings.Join(stored, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// subagentStart: a short guide and the top briefing items, no network.
func (h *hookEnv) subagentStart() []byte {
	if slices.Contains(h.cfg.Hooks.SubagentSkip, h.in.AgentType) {
		return nil
	}
	text := instructions.SubagentGuide + " " + instructions.HintsLine
	if st, err := h.store.Load(h.in.SessionID); err == nil {
		if st.BriefingPage != "" { // the page's first lines, within the subagent budget
			page := strings.SplitN(st.BriefingPage, "\n", 2)
			if len(page) == 2 {
				text += "\n" + clip(page[1], subagentChars-len([]rune(text)))
			}
		}
		n := 0
		for _, in := range st.Injected {
			if in.Rule != "briefing" || n == subagentItems {
				continue
			}
			line := "\n- " + clip(in.Text, subagentItemChars)
			if len([]rune(text))+len([]rune(line)) > subagentChars {
				break
			}
			text += line
			n++
		}
	}
	return contextOutput("SubagentStart", text, "")
}
