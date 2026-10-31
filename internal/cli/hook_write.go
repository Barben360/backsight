package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/hook"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
	"github.com/Barben360/backsight/internal/transcript"
)

const (
	gateRecallBudget = 6 * time.Second // SPEC §6.2: hits only when they arrive within 6 s
	// subagentAttributionBudget bounds the path rule in SubagentStop (3 s
	// for the whole hook): past it the report keeps its session's project.
	subagentAttributionBudget = 1500 * time.Millisecond
	// subagentAppendMargin is what SubagentStop keeps of its child's
	// deadline once the wait for the outbox lock ends: the append itself
	// (a write and its fsync, up to about 100 ms on a slow disk), the
	// telemetry and the process's own start before hookChildStart.
	subagentAppendMargin = 500 * time.Millisecond
	gateHits             = 2
	lastReplyChars       = 1000 // the end of the last reply, for the detector and outcomes
)

// spawnDetached starts `backsight <args>` outside the hook's process group,
// so it outlives SessionEnd's 1.5 s budget; tests replace it.
var spawnDetached = func(args ...string) error { return startDetachedTo(nil, args...) }

func init() {
	handlers["pre-tool-use"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, readSide)
		if !ok {
			return nil, nil
		}
		return h.gate(), nil
	}
	handlers["stop"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, "") // read (detector, outcomes) or write: either side needs it
		if !ok {
			return nil, nil
		}
		return h.stop(), nil
	}
	handlers["subagent-stop"] = func(ctx context.Context, event string, input []byte) ([]byte, error) {
		start := hookChildStart
		if start.IsZero() {
			start = time.Now()
		}
		h, ok := newHookEnv(ctx, input, "")
		if !ok {
			return nil, nil
		}
		return h.subagentStop(start.Add(hook.ChildDeadline(event, input) - subagentAppendMargin)), nil
	}
	handlers["session-end"] = sessionEnd
}

// sessionEnd spawns the session's flush, doing as little as possible first:
// the whole event has 1.5 s (C06). No project resolution, no client.
func sessionEnd(_ context.Context, _ string, input []byte) ([]byte, error) {
	var in hookInput
	if json.Unmarshal(input, &in) != nil || !session.ValidID(in.SessionID) {
		return nil, nil
	}
	cfg, _, err := loadServerConfig()
	if err != nil || !cfg.Components.Write {
		return nil, nil
	}
	if err := spawnDetached("retain", "flush", "--session", in.SessionID); err != nil {
		if d, derr := paths.Default(); derr == nil {
			telemetry.New(d.State, telemetry.Common{V: instructions.Version, Session: in.SessionID, BinaryVersion: buildinfo.Version}).
				Emit(telemetry.Raw{Name: "spawn_failed", Fields: map[string]any{"error": err.Error()}})
		}
	}
	return nil, nil
}

// gate (SPEC §6.2): before AskUserQuestion, if no memory tool ran during
// this prompt, deny once with the top hits for the question.
func (h *hookEnv) gate() []byte {
	if h.in.ToolName != "AskUserQuestion" {
		return nil
	}
	if h.maintenance() || h.breakerOpen() {
		return nil // memory unreachable: never block a question on it
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
		eng := &recall.Engine{Client: h.client, Bank: h.cfg.Server.Bank, Transverse: h.cfg.Projects.Transverse, Timeout: gateRecallBudget, Now: now,
			Adjust: ledgerAdjust(h.dirs, h.cfg, now())}
		res := eng.Search(h.ctx, recall.Query{Queries: []string{h.recallQuery(q)}, Current: h.project, MinReranker: h.cfg.Recall.MinScore,
			GlobalMinReranker: h.cfg.Recall.GlobalMinScore, Limit: gateHits})
		if len(res.Hits) > 0 {
			reason += "\nPossibly relevant memories " + instructions.HintsLine + ":"
			for _, hit := range res.Hits {
				reason += fmt.Sprintf("\n- %s %s%s", memoryTag(hit.Project, hit.ID), clip(hit.Text, briefingItemChars), age(hit.Age))
			}
			h.journalShows("gate", itemsFromHits(res.Hits))
			h.updateSession(func(s *session.State) error { s.KeepShownTexts(hitTexts(res.Hits)); return nil })
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
// the detector's approval context and the next outcome's credit; then the
// end-of-turn check, whose block is its answer. No network.
func (h *hookEnv) stop() []byte {
	if paused, _ := session.Paused(h.dirs.SessionsDir(), h.in.SessionID); paused {
		return nil
	}
	full := strings.TrimSpace(h.in.LastAssistantMessage)
	credit := ""
	if k := replyKind(full); k != nil {
		credit = *k // over the whole reply: a citation may sit early in a long one
	}
	last := []rune(full)
	if len(last) > lastReplyChars {
		last = last[len(last)-lastReplyChars:]
	}
	tail, _ := redact.Text(string(last))
	last = []rune(tail)
	ev := telemetry.Reply{Credit: credit}
	h.updateSession(func(s *session.State) error {
		s.LastAssistant, s.LastCredit = string(last), credit
		ev.N, ev.Searched = s.Prompts, s.Prompts > 0 && s.SearchedPromptID == s.Prompts
		return nil
	})
	if ev.N > 0 {
		h.tw.Emit(ev)
	}
	return h.creditCheck("")
}

// subagentStop queues the subagent's final report (waiting for the outbox
// lock until lockDeadline at most), then runs the end-of-turn check for
// the subagent, whose block is its answer.
func (h *hookEnv) subagentStop(lockDeadline time.Time) []byte {
	if !subagentRan(h.in) {
		return nil // an internal helper agent (a tool-call summary): no transcript, no result, no report
	}
	h.updateSession(func(s *session.State) error { s.SubagentDone = true; return nil }) // the approval detector (read side)
	h.queueSubagentReport(lockDeadline)
	if paused, _ := session.Paused(h.dirs.SessionsDir(), h.in.SessionID); paused || h.in.AgentID == "" {
		return nil
	}
	return h.creditCheck(h.in.AgentID) // a subagent runs inside its parent's turn
}

// queueSubagentReport queues the subagent's final report as a document
// (SPEC §6.2), redacted and capped, honouring the session pause and
// opt-outs.
func (h *hookEnv) queueSubagentReport(lockDeadline time.Time) {
	text := strings.TrimSpace(h.in.LastAssistantMessage)
	if !h.cfg.Components.Write || text == "" || h.in.AgentID == "" {
		return
	}
	if paused, _ := session.Paused(h.dirs.SessionsDir(), h.in.SessionID); paused {
		return
	}
	if out, _ := project.New(h.home, h.cfg.Projects.Transverse).OptedOut(h.in.Cwd, h.cfg.Privacy.ExcludePaths); out {
		return
	}
	if h.cfg.Privacy.SkipsEntrypoint(parentEntrypoint(h.in.TranscriptPath)) {
		return // a headless session's subagent (decision 0071 C1)
	}
	text = retain.SubagentReport(text) // redacted and capped, as a reconcile rebuilds it
	proj := canonicalProject(h.cfg, h.project)
	// The report goes to the own repository the subagent's work is in
	// (the path rule, decision 0072), else to its session's project.
	source := ""
	if p := agentTranscriptPath(h.in); p != "" {
		a := attributor(h.dirs, h.cfg, h.resolver)
		a.Deadline = time.Now().Add(subagentAttributionBudget)
		e, err := a.Subagent(h.in.SessionID, h.in.AgentID, h.in.TranscriptPath, p, proj, h.in.LastAssistantMessage, now())
		if err != nil {
			h.tw.Emit(telemetry.Raw{Name: "attribution_error", Fields: map[string]any{"error": err.Error()}})
		}
		proj, source = a.ReportProject(e), e.Source
		h.tw.Emit(attributionEvent(e))
	}
	n := retain.NewSubagentNote(h.cfg.Server.Bank, h.in.SessionID, h.in.AgentID, h.in.AgentType, proj, text, now())
	n.Attribution = source
	ob := notesOutboxFor(h.dirs, h.cfg)
	ob.LockDeadline = lockDeadline // a lock held past it refuses the note before the kill
	if _, err := ob.Append(n); err != nil {
		h.tw.Emit(telemetry.Retain{Document: n.DocumentID, Items: 1, Status: "failed"})
		return
	}
	h.tw.Emit(telemetry.Retain{Document: n.DocumentID, Items: 1, Status: "queued"})
}

// parentEntrypoint is how the session that ran a subagent was started, read
// from the head of its transcript; "" when unknown.
func parentEntrypoint(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	return transcript.Entrypoint(f)
}

// subagentRan: the subagent wrote its own transcript. Claude Code's
// internal helper agents (UI and Remote Control summaries of tool calls)
// also fire SubagentStop, with a one-line status and no transcript: over
// 1 000 such documents in a day on the author's machine (decision 0071).
func subagentRan(in hookInput) bool {
	return agentTranscriptPath(in) != ""
}

// agentTranscriptPath is the subagent's own transcript, "" when it has
// none.
func agentTranscriptPath(in hookInput) string {
	p := in.AgentTranscriptPath
	if p == "" && in.TranscriptPath != "" && session.ValidID(in.SessionID) && session.ValidID(in.AgentID) {
		p = filepath.Join(strings.TrimSuffix(in.TranscriptPath, ".jsonl"), "subagents", "agent-"+in.AgentID+".jsonl")
	}
	if p == "" {
		return ""
	}
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}
