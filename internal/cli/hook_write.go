package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
)

const (
	gateRecallBudget = 6 * time.Second // SPEC §6.2: hits only when they arrive within 6 s
	gateHits         = 2
	reportChars      = 4000 // subagent report kept (SPEC §6.2)
	lastReplyChars   = 1000 // the end of the last reply, for the detector and outcomes
)

// spawnDetached starts `backsight <args>` outside the hook's process group,
// so it outlives SessionEnd's 1.5 s budget; tests replace it.
var spawnDetached = startDetached

func init() {
	handlers["pre-tool-use"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, readSide)
		if !ok {
			return nil, nil
		}
		return h.gate(), nil
	}
	handlers["stop"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, "")
		if !ok {
			return nil, nil
		}
		h.stop()
		return nil, nil
	}
	handlers["subagent-stop"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, writeSide)
		if !ok {
			return nil, nil
		}
		h.subagentStop()
		return nil, nil
	}
	handlers["session-end"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, writeSide)
		if !ok {
			return nil, nil
		}
		if session.ValidID(h.in.SessionID) {
			_ = spawnDetached("retain", "flush", "--session", h.in.SessionID)
		}
		return nil, nil
	}
}

// gate (SPEC §6.2): before AskUserQuestion, if no memory tool ran during
// this prompt, deny once with the top hits for the question.
func (h *hookEnv) gate() []byte {
	if h.in.ToolName != "AskUserQuestion" {
		return nil
	}
	st, _ := h.store.Load(h.in.SessionID)
	n := st.Prompts
	ev := telemetry.Gate{N: n, Decision: "allow"}
	switch {
	case st.SearchedPromptID == n && n > 0:
		ev.Reason = "searched"
	case st.GatedPrompt == n:
		ev.Reason = "already_gated"
	default:
		ev.Decision, ev.Reason = "deny", "no_search"
	}
	h.tw.Emit(ev)
	if ev.Decision == "allow" {
		return nil
	}
	h.updateSession(func(s *session.State) error { s.GatedPrompt = n; return nil })
	reason := instructions.GateReason
	if q := questionText(h.in.ToolInput); q != "" {
		eng := &recall.Engine{Client: h.client, Bank: h.cfg.Server.Bank, Transverse: h.cfg.Projects.Transverse, Timeout: gateRecallBudget, Now: now}
		res := eng.Search(h.ctx, recall.Query{Queries: []string{h.recallQuery(q)}, Current: h.project, MinReranker: h.cfg.Recall.MinScore, Limit: gateHits})
		if len(res.Hits) > 0 {
			reason += "\nPossibly relevant memories:"
			for _, hit := range res.Hits {
				reason += fmt.Sprintf("\n- [%s] %s", hit.Project, clip(hit.Text, briefingItemChars))
			}
		}
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}})
	return out
}

func questionText(raw json.RawMessage) string {
	var in struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(raw, &in)
	var qs []string
	for _, q := range in.Questions {
		qs = append(qs, q.Question)
	}
	return strings.Join(qs, " ")
}

// stop records the end of the last reply (the transcript can lag, C06):
// the detector's approval context and the next outcome's credit. No network.
func (h *hookEnv) stop() {
	last := []rune(strings.TrimSpace(h.in.LastAssistantMessage))
	if len(last) > lastReplyChars {
		last = last[len(last)-lastReplyChars:]
	}
	h.updateSession(func(s *session.State) error { s.LastAssistant = string(last); return nil })
}

// subagentStop queues the subagent's final report as a document (SPEC §6.2),
// redacted and capped, honouring the session pause and opt-outs.
func (h *hookEnv) subagentStop() {
	h.updateSession(func(s *session.State) error { s.SubagentDone = true; return nil })
	text := strings.TrimSpace(h.in.LastAssistantMessage)
	if text == "" || h.in.AgentID == "" {
		return
	}
	if paused, _ := session.Paused(h.dirs.SessionsDir(), h.in.SessionID); paused {
		return
	}
	if out, _ := project.New(h.home, h.cfg.Projects.Transverse).OptedOut(h.in.Cwd, h.cfg.Privacy.ExcludePaths); out {
		return
	}
	text, _ = redact.Text(text)
	if r := []rune(text); len(r) > reportChars {
		text = string(r[:reportChars])
	}
	n := retain.NewSubagentNote(h.cfg.Server.Bank, h.in.SessionID, h.in.AgentID, noteProject(h.home, h.cfg, h.project), text, now())
	if _, err := notesOutbox(h.dirs).Append(n); err != nil {
		h.tw.Emit(telemetry.Retain{Document: n.DocumentID, Items: 1, Status: "failed"})
		return
	}
	h.tw.Emit(telemetry.Retain{Document: n.DocumentID, Items: 1, Status: "queued"})
}
