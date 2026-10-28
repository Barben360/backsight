package cli

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The hooks' side of the feedback journal (decision 0074): every memory
// shown to the agent, with its path and rank, under the turn it was shown
// in. Best effort: a failed write is logged, never changes a hook's answer.

const shortIDChars = 8

// shortID is the id the agent sees next to a memory: its first 8
// characters.
func shortID(id string) string {
	if len(id) > shortIDChars {
		return id[:shortIDChars]
	}
	return id
}

// memoryTag is a shown memory's "[<project> · <id8>]".
func memoryTag(project, id string) string {
	return "[" + safeProject(project) + " · " + safeProject(shortID(id)) + "]"
}

// feedbackOn: the journal is written for this event.
func (h *hookEnv) feedbackOn() bool {
	return h.cfg.Feedback.Enabled && h.journal != nil
}

// feedbackError logs a journal or turn pointer write that failed.
func (h *hookEnv) feedbackError(err error) {
	if err != nil {
		h.tw.Emit(telemetry.Raw{Name: "feedback_error", Fields: map[string]any{"error": err.Error()}})
	}
}

// turnID is the turn this event journals under: the hook's prompt_id, else
// the one this process started, else the session's turn pointer; "" before
// any prompt (the briefing's turn).
func (h *hookEnv) turnID() string {
	if h.in.PromptID != "" {
		return h.in.PromptID
	}
	if h.turnSet {
		return h.turn
	}
	if t, ok := feedback.CurrentTurn(h.dirs.FeedbackDir(), h.in.SessionID); ok {
		return t.ID
	}
	return ""
}

// setTurn records t as the session's current turn.
func (h *hookEnv) setTurn(t feedback.Turn) {
	h.turn, h.turnSet = t.ID, true
	h.feedbackError(feedback.SetTurn(h.dirs.FeedbackDir(), h.in.SessionID, t))
}

// startSessionTurn is a started, resumed or cleared session's pointer: no
// turn yet, its journal reads starting here, before the briefing.
func (h *hookEnv) startSessionTurn() {
	switch h.in.Source {
	case "startup", "", "resume", "clear":
	default:
		return
	}
	if !h.feedbackOn() {
		return
	}
	file, offset := h.journal.Position()
	h.setTurn(feedback.Turn{At: now(), File: file, Offset: offset})
}

// beginTurn is UserPromptSubmit's pointer and turn event, before any
// recall. A machine message keeps the prompt number of the turn it
// follows; the first turn after a session start keeps its journal
// position, so that its reads include the briefing.
func (h *hookEnv) beginTurn(n int, machine bool) {
	if !h.feedbackOn() {
		return
	}
	prev, had := feedback.CurrentTurn(h.dirs.FeedbackDir(), h.in.SessionID)
	id := h.in.PromptID
	if id == "" {
		id = randomHex(16)
	}
	if machine {
		n = prev.N
	}
	file, offset := h.journal.Position()
	if had && prev.N == 0 && prev.File != "" {
		file, offset = prev.File, prev.Offset
	}
	h.setTurn(feedback.Turn{ID: id, N: n, At: now(), Machine: machine, File: file, Offset: offset})
	h.feedbackError(h.journal.Append(feedback.Event{Kind: "turn", Turn: id, N: n, Machine: machine}))
}

// journalShows records items shown to the agent on path (prompt, gate,
// briefing, search, read).
func (h *hookEnv) journalShows(path string, items []feedback.Item) {
	if !h.feedbackOn() || len(items) == 0 {
		return
	}
	h.feedbackError(h.journal.Append(feedback.Event{Kind: "shows", Turn: h.turnID(), Agent: h.in.AgentID, Path: path, Items: items}))
}

// itemsFromHits ranks hits from 1 in the order shown.
func itemsFromHits(hits []recall.Hit) []feedback.Item {
	items := make([]feedback.Item, 0, len(hits))
	for i, hit := range hits {
		items = append(items, feedback.Item{ID: hit.ID, Rank: i + 1, Reranker: hit.Reranker})
	}
	return items
}

// itemsFromInjections ranks briefing items from 1 in the order shown.
func itemsFromInjections(ins []session.Injection) []feedback.Item {
	items := make([]feedback.Item, 0, len(ins))
	for i, in := range ins {
		items = append(items, feedback.Item{ID: in.ID, Rank: i + 1})
	}
	return items
}

var (
	// searchHitRE: a memory_search hit line, whole: its merged rank first,
	// its id and score last.
	searchHitRE = regexp.MustCompile(`(?m)^- #([0-9]+) .*, ([^\s,()]+), -?[0-9]+\.[0-9]+\)\r?$`)
	// searchRankRE: any line that starts like a hit.
	searchRankRE = regexp.MustCompile(`(?m)^- #[0-9]+ `)
	// showIDRE: what memory_read journals must look like an id, never text.
	showIDRE = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,128}$`)
)

// searchShows reads the ranked hits back from memory_search's text, in
// rank order. Only whole hit lines count, and only when every line that
// starts like a hit is one and their ranks run 1..N: anything else (a
// memory's text posing as a hit) journals nothing rather than a wrong show.
func searchShows(text string) []feedback.Item {
	lines := searchHitRE.FindAllStringSubmatch(text, -1)
	if len(lines) != len(searchRankRE.FindAllStringIndex(text, -1)) {
		return nil
	}
	items := make([]feedback.Item, 0, len(lines))
	for _, l := range lines {
		rank, err := strconv.Atoi(l[1])
		if err != nil {
			return nil
		}
		items = append(items, feedback.Item{ID: l[2], Rank: rank})
	}
	slices.SortFunc(items, func(a, b feedback.Item) int { return a.Rank - b.Rank }) // grouped by project in the text
	for i, it := range items {
		if it.Rank != i+1 {
			return nil
		}
	}
	return items
}

// readShow is memory_read's id as one unranked item; nil when the call
// named nothing that looks like an id.
func readShow(raw json.RawMessage) []feedback.Item {
	var in struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &in)
	if id := strings.TrimSpace(in.ID); showIDRE.MatchString(id) {
		return []feedback.Item{{ID: id}}
	}
	return nil
}
