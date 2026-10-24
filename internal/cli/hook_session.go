package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

const (
	briefingItems     = 5
	briefingItemChars = 220
	briefingChars     = 1200
	briefingTimeout   = 5 * time.Second
	compactReplay     = 6
	subagentItems     = 2
	subagentChars     = 600
)

func init() {
	handlers["session-start"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, readSide)
		if !ok {
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
		return contextOutput("SessionStart", instructions.Guide+h.compactReplay(), "")
	default: // resume, clear, fork
		return contextOutput("SessionStart", instructions.Guide, "")
	}
	h.writeEnvFile()
	text, items, source, err := h.briefing()
	banner := "backsight · project " + h.project + " · memory on"
	if err != nil {
		banner = "backsight · project " + h.project + " · " + instructions.MemoryUnavailable("server unreachable")
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
		_ = h.sticky().Record(ids, now(), stickyWindow) // briefed ids count towards muting (SPEC §6.2)
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
	b.WriteString(instructions.BriefingHeader(h.project) + "\n")
	if source == "raw" {
		b.WriteString(instructions.BriefingRaw + "\n")
	}
	at := now()
	for _, m := range mems {
		line := fmt.Sprintf("- %s (%s ago)\n", clip(m.Text, briefingItemChars), recall.Age(at.Sub(m.UpdatedAt)))
		if len([]rune(b.String()))+len([]rune(line)) > briefingChars {
			break
		}
		b.WriteString(line)
		items = append(items, session.Injection{ID: m.ID, Text: m.Text, Project: h.project, Scope: "project", Prompt: 0, Rule: "briefing", At: at})
	}
	return strings.TrimRight(b.String(), "\n"), items, source, nil
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

// firstSession records, once, that the notice was shown.
func (h *hookEnv) firstSession() bool {
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
	if err != nil || (len(st.Injected) == 0 && len(st.Stored) == 0) {
		return ""
	}
	var b strings.Builder
	if len(st.Injected) > 0 {
		b.WriteString("\n\nMemories shown earlier in this session:\n")
		for _, in := range st.Injected[max(0, len(st.Injected)-compactReplay):] {
			fmt.Fprintf(&b, "- [%s] %s\n", in.Project, clip(in.Text, briefingItemChars))
		}
	}
	if len(st.Stored) > 0 {
		fmt.Fprintf(&b, "\nStored this session: %s\n", strings.Join(st.Stored, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// subagentStart: a short guide and the top briefing items, no network.
func (h *hookEnv) subagentStart() []byte {
	if slices.Contains(h.cfg.Hooks.SubagentSkip, h.in.AgentType) {
		return nil
	}
	text := instructions.SubagentGuide
	if st, err := h.store.Load(h.in.SessionID); err == nil {
		n := 0
		for _, in := range st.Injected {
			if in.Rule != "briefing" || n == subagentItems {
				continue
			}
			line := "\n- " + clip(in.Text, briefingItemChars)
			if len([]rune(text))+len([]rune(line)) > subagentChars {
				break
			}
			text += line
			n++
		}
	}
	return contextOutput("SubagentStart", text, "")
}
