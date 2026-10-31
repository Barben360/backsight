package cli

import (
	"encoding/json"
	"slices"
	"strconv"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The end-of-turn check (decision 0074): a turn that showed memories ends
// with a declaration, a memory_credit call naming the ones used or none.
// Stop asks the main agent, SubagentStop each subagent, at most once per
// turn. It never blocks on a failure: the journal is best effort.

const stopReasonChars = 60 // a memory's text in the reason

// creditCheck is the answer that ends agent's turn ("" is the main agent):
// a block asking for the declaration, or nothing. It journals the outcome
// of every turn that showed memories.
func (h *hookEnv) creditCheck(agent string) []byte {
	if !h.feedbackOn() {
		return nil
	}
	t, ok := h.turnPointer() // the turn every writer of the turn filed under (turnID)
	if !ok {
		return nil // no turn began under the journal: nothing it showed is known
	}
	mode := h.cfg.Feedback.StopCheck
	events, _, err := feedback.ReadSince(t.File, t.Offset)
	if err != nil {
		h.feedbackError(telemetry.FeedbackReasonJournalRead, err)
		h.journalStop(t.ID, agent, "check_skipped", mode, 0, nil, false)
		return nil
	}
	// The briefing is shown before the first prompt: the first turn's.
	c := feedback.CheckTurn(events, h.in.SessionID, t.ID, agent, agent == "" && t.N == 1 && !t.Machine)
	ids := shownIDs(c.Shown)
	if len(ids) == 0 {
		return nil
	}
	reply := h.in.LastAssistantMessage
	texts := h.shownTexts()
	var outcome string
	switch {
	case mode == "off" || t.Machine || h.cfg.Privacy.SkipsEntrypoint(parentEntrypoint(h.in.TranscriptPath)):
		outcome = "check_skipped" // a machine message's turn, or a headless or SDK session: no one to ask
	case c.Declared:
		outcome = "declared"
	case h.in.StopHookActive || c.Blocked:
		outcome = "undeclared" // asked once already
	case mode == "cited" && !creditRE.MatchString(reply) && !slices.ContainsFunc(c.Shown, func(s feedback.Show) bool {
		return s.Path == "search" || s.Path == "read"
	}):
		outcome = "undeclared" // memories only injected, and the reply cites none
	default:
		outcome = "blocked"
	}
	afterBlock := c.Blocked && (outcome == "declared" || outcome == "undeclared")
	h.journalStop(t.ID, agent, outcome, mode, len(ids), c.Flags(reply, texts), afterBlock)
	if outcome != "blocked" {
		return nil
	}
	reason := instructions.StopCheckHeader
	for _, id := range ids {
		text := "(text not kept)"
		if s := texts[id]; s != "" {
			text = strconv.Quote(clip(s, stopReasonChars))
		}
		reason += "\n- " + safeProject(shortID(id)) + " " + text
	}
	out, _ := json.Marshal(map[string]string{"decision": "block", "reason": reason + "\n" + instructions.StopCheckAsk})
	return out
}

// journalStop records the check's outcome, best effort, and its telemetry.
func (h *hookEnv) journalStop(turn, agent, outcome, mode string, shown int, flags []string, afterBlock bool) {
	h.feedbackError(telemetry.FeedbackReasonJournalWrite, h.journal.Append(feedback.Event{Kind: "stop", Turn: turn, Agent: agent, Outcome: outcome, StopCheck: mode, Flags: flags}))
	h.tw.Emit(telemetry.CreditCheck{Outcome: outcome, Shown: shown, Flags: flags, AfterBlock: afterBlock})
}

// shownIDs is each memory shown once, in the order first shown.
func shownIDs(shown []feedback.Show) []string {
	var ids []string
	for _, s := range shown {
		if !slices.Contains(ids, s.ID) {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// shownTexts is the session's memory texts by id: those kept as shown,
// and the injected ones the cap dropped.
func (h *hookEnv) shownTexts() map[string]string {
	st, _ := h.store.Load(h.in.SessionID)
	texts := map[string]string{}
	for _, in := range st.Injected {
		texts[in.ID] = in.Text
	}
	for id, text := range st.ShownTexts {
		texts[id] = text
	}
	return texts
}
