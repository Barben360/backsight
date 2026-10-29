package cli

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/paths"
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

// texts are the instruction texts for feedback.enabled: memory_credit is
// named only where it is offered.
func (h *hookEnv) texts() instructions.Texts {
	return instructions.For(h.cfg.Feedback.Enabled)
}

// feedbackEnabled: feedback is on for this event, whether or not its
// session is paused; the turn pointer follows it.
func (h *hookEnv) feedbackEnabled() bool {
	return h.cfg.Feedback.Enabled && h.journal != nil
}

// feedbackOn: the journal is written for this event. A paused session
// journals nothing (an unreadable session state counts as paused).
func (h *hookEnv) feedbackOn() bool {
	if !h.feedbackEnabled() {
		return false
	}
	paused, _ := session.Paused(h.dirs.SessionsDir(), h.in.SessionID)
	return !paused
}

// feedbackError logs a journal or turn pointer write that failed.
func (h *hookEnv) feedbackError(err error) {
	if err != nil {
		h.tw.Emit(telemetry.Raw{Name: "feedback_error", Fields: map[string]any{"error": err.Error()}})
	}
}

// turnPointer is the session's turn pointer, which the prompt hook moves.
func (h *hookEnv) turnPointer() (feedback.Turn, bool) {
	return feedback.CurrentTurn(h.dirs.FeedbackDir(), h.in.SessionID)
}

// turnID is the turn this event journals under: the one this process
// started, else the session's turn pointer (the end-of-turn check reads
// that turn), else the hook's prompt_id when no pointer exists; "" before
// any prompt (the briefing's turn).
func (h *hookEnv) turnID() string {
	if h.turnSet {
		return h.turn
	}
	if t, ok := h.turnPointer(); ok {
		return t.ID
	}
	return h.in.PromptID
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
	if !h.feedbackEnabled() {
		return
	}
	file, offset := h.journal.Position()
	h.setTurn(feedback.Turn{At: now(), File: file, Offset: offset})
}

// beginTurn is UserPromptSubmit's pointer and turn event, before any
// recall; a paused session moves the pointer only. A machine message keeps the prompt number and the journal
// position of the turn it follows, so that the next prompt still reads
// that turn's credits (a machine turn is never checked at Stop); the first
// turn after a session start keeps its journal position, so that its reads
// include the briefing.
func (h *hookEnv) beginTurn(n int, machine bool) {
	if !h.feedbackEnabled() {
		return
	}
	prev, had := h.turnPointer()
	id := h.in.PromptID
	if id == "" {
		id = randomHex(16)
	}
	if machine {
		n = prev.N
	}
	file, offset := h.journal.Position()
	if had && (prev.N == 0 || machine) && prev.File != "" {
		file, offset = prev.File, prev.Offset
	}
	h.setTurn(feedback.Turn{ID: id, N: n, At: now(), Machine: machine, File: file, Offset: offset})
	if h.feedbackOn() {
		h.feedbackError(h.journal.Append(feedback.Event{Kind: "turn", Turn: id, N: n, Machine: machine}))
	}
}

// creditedSinceTurn: the main agent of this session made a memory_credit
// call since its current turn began, read before the next turn moves the
// pointer. False when the journal is off or cannot be read.
func (h *hookEnv) creditedSinceTurn() bool {
	if !h.feedbackOn() {
		return false
	}
	t, ok := h.turnPointer()
	if !ok {
		return false
	}
	events, _, err := feedback.ReadSince(t.File, t.Offset)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(events, func(e feedback.Event) bool {
		return e.Kind == "credit" && e.Session == h.in.SessionID && e.Agent == ""
	})
}

// journalShows records items shown to the agent on path (prompt, gate,
// briefing, search, read).
func (h *hookEnv) journalShows(path string, items []feedback.Item) {
	if !h.feedbackOn() || len(items) == 0 {
		return
	}
	h.feedbackError(h.journal.Append(feedback.Event{Kind: "shows", Turn: h.turnID(), Agent: h.in.AgentID, Path: path, Items: items}))
}

// itemsFromHits ranks hits from 1 in the order shown, each with its boost,
// its score and its rank among them without it.
func itemsFromHits(hits []recall.Hit) []feedback.Item {
	items := make([]feedback.Item, 0, len(hits))
	for i, hit := range hits {
		items = append(items, feedback.Item{ID: hit.ID, Rank: i + 1, Reranker: hit.Reranker, Project: hit.Project,
			Boost: hit.Boost, RankWithoutBoost: rankWithoutBoost(hits, hit), ScoreWithoutBoost: scoreWithoutBoost(hit)})
	}
	return items
}

// scoreWithoutBoost is hit's score before the ledger's boost, to 4
// decimals as recall rounds it.
func scoreWithoutBoost(hit recall.Hit) *float64 {
	score := math.Round((hit.Score-hit.Boost)*10000) / 10000
	return &score
}

// rankWithoutBoost is hit's place among the hits shown had recall not
// boosted them, from 1; 0 when recall had no ledger.
func rankWithoutBoost(hits []recall.Hit, hit recall.Hit) int {
	if hit.RankWithoutBoost == 0 {
		return 0
	}
	n := 1
	for _, o := range hits {
		if o.RankWithoutBoost != 0 && o.RankWithoutBoost < hit.RankWithoutBoost {
			n++
		}
	}
	return n
}

// hitTexts is the hits' texts by id, for the session's ShownTexts.
func hitTexts(hits []recall.Hit) map[string]string {
	texts := make(map[string]string, len(hits))
	for _, hit := range hits {
		texts[hit.ID] = hit.Text
	}
	return texts
}

// injectionTexts is the injected memories' texts by id, for the session's
// ShownTexts.
func injectionTexts(ins []session.Injection) map[string]string {
	texts := make(map[string]string, len(ins))
	for _, in := range ins {
		texts[in.ID] = in.Text
	}
	return texts
}

// itemsFromInjections ranks briefing items from 1 in the order shown.
func itemsFromInjections(ins []session.Injection) []feedback.Item {
	items := make([]feedback.Item, 0, len(ins))
	for i, in := range ins {
		items = append(items, feedback.Item{ID: in.ID, Rank: i + 1, Project: in.Project})
	}
	return items
}

var (
	// searchHitRE: a memory_search hit line, whole: its merged rank first,
	// its id and score last.
	searchHitRE = regexp.MustCompile(`(?m)^- #([0-9]+) .*, ([^\s,()]+), -?[0-9]+\.[0-9]+\)\r?$`)
	// searchRankRE: any line that starts like a hit.
	searchRankRE = regexp.MustCompile(`(?m)^- #[0-9]+ `)
	// searchTextRE: a hit line's text, before its last parenthesis.
	searchTextRE = regexp.MustCompile(`^- #[0-9]+ (.*) \([^()]*, ([^\s,()]+), -?[0-9]+\.[0-9]+\)\r?$`)
	// searchGroupRE: a project's group header, "[<project>]".
	searchGroupRE = regexp.MustCompile(`^\[(.+)\]\r?$`)
	// showIDRE: what memory_read journals must look like an id, never text.
	showIDRE = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,128}$`)
)

// searchShows reads the ranked hits back from memory_search's text, in
// rank order, each with the project of the group header above it. Only
// whole hit lines count, and only when every line that starts like a hit
// is one and their ranks run 1..N: anything else (a memory's text posing
// as a hit) journals nothing rather than a wrong show. A memory's text is
// on one line, so it cannot pose as a header.
func searchShows(text string) []feedback.Item {
	if len(searchHitRE.FindAllStringIndex(text, -1)) != len(searchRankRE.FindAllStringIndex(text, -1)) {
		return nil
	}
	var items []feedback.Item
	project := ""
	for _, line := range strings.Split(text, "\n") {
		if g := searchGroupRE.FindStringSubmatch(line); g != nil {
			project = g[1]
			continue
		}
		l := searchHitRE.FindStringSubmatch(line)
		if l == nil {
			continue
		}
		rank, err := strconv.Atoi(l[1])
		if err != nil {
			return nil
		}
		items = append(items, feedback.Item{ID: l[2], Rank: rank, Project: project})
	}
	slices.SortFunc(items, func(a, b feedback.Item) int { return a.Rank - b.Rank }) // grouped by project in the text
	for i, it := range items {
		if it.Rank != i+1 {
			return nil
		}
	}
	return items
}

// searchTexts is the texts of memory_search's hit lines by id, for the
// session's ShownTexts; nothing when searchShows finds no whole list.
func searchTexts(text string) map[string]string {
	if searchShows(text) == nil {
		return nil
	}
	texts := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if l := searchTextRE.FindStringSubmatch(line); l != nil {
			texts[l[2]] = l[1]
		}
	}
	return texts
}

// readShow is memory_read's id as one unranked item, a short id resolved
// to the id shown in the session it names, as the server does; nil when
// the call named nothing that looks like an id, or a short id of several.
func (h *hookEnv) readShow(raw json.RawMessage) []feedback.Item {
	if !h.feedbackOn() {
		return nil
	}
	var in struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &in)
	id := strings.TrimSpace(in.ID)
	if !showIDRE.MatchString(id) {
		return nil
	}
	if !fullUUIDRE.MatchString(id) {
		full, err := feedback.Resolve(id, shownInSession(h.dirs, h.in.SessionID))
		switch {
		case err == nil:
			id = full
		case errors.Is(err, feedback.ErrAmbiguous):
			return nil
		}
	}
	return []feedback.Item{{ID: id}}
}

// shownInSession is what session sess was shown: the journal's shows of
// the last 62 days and, when the journal cannot be read, the session
// state's seen ids too.
func shownInSession(d paths.Dirs, sess string) []feedback.Show {
	events, _, err := feedback.Read(d.FeedbackDir(), now().AddDate(0, 0, -62))
	shown := feedback.Shows(events, sess)
	if err != nil {
		st, _ := session.Store{Dir: d.SessionsDir()}.Load(sess)
		for _, id := range st.Seen {
			shown = append(shown, feedback.Show{ID: id})
		}
	}
	return shown
}

// journalCredit records a memory_credit call the server accepted (its
// answer starts with one of the credit prefixes; a refusal records
// nothing), prefixes resolved to the full ids shown. A Sources answer is
// no credit: the facts it lists are journaled as shown, for the second
// call to name them.
func (h *hookEnv) journalCredit(text string) {
	if !h.feedbackOn() {
		return
	}
	var in struct {
		IDs     []string `json:"ids"`
		Verdict string   `json:"verdict"`
	}
	_ = json.Unmarshal(h.in.ToolInput, &in)
	var verdict string
	switch {
	case strings.HasPrefix(text, creditUsedPrefix):
		verdict = "used"
	case strings.HasPrefix(text, creditNonePrefix):
		verdict = "none"
	case strings.HasPrefix(text, creditRetiredPrefix), strings.HasPrefix(text, creditPartlyPrefix), strings.HasPrefix(text, creditHiddenPrefix):
		verdict = in.Verdict // only a wrong or outdated call answers these
	case strings.HasPrefix(text, creditSourcesPrefix):
		h.journalShows(feedback.PathSources, sourcesShows(text))
		return
	default:
		return
	}
	var ids []string
	if verdict != "none" {
		shown := shownInSession(h.dirs, h.in.SessionID)
		for _, ref := range in.IDs {
			if id, err := feedback.Resolve(strings.TrimSpace(ref), shown); err == nil && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return // a credit naming nothing shown credits nothing
		}
	}
	h.feedbackError(h.journal.Append(feedback.Event{Kind: "credit", Turn: h.turnID(), Agent: h.in.AgentID, IDs: ids, Verdict: verdict}))
}

// sourceLineRE: a fact line of a Sources answer, whole: its project and
// short id in the tag, its full id last.
var sourceLineRE = regexp.MustCompile(`^- \[(.+?) · ([^\s\]]+)\] .* \(([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\)\r?$`)

// sourcesShows reads the facts a Sources answer lists, in order, each
// once, unranked, with its project. A fact's text is on its line (the
// server makes it one), so only whole fact lines whose short id starts
// their full id count; any other line is skipped rather than journaled
// as a wrong show.
func sourcesShows(text string) []feedback.Item {
	var items []feedback.Item
	for _, line := range strings.Split(text, "\n") {
		l := sourceLineRE.FindStringSubmatch(line)
		if l == nil || !strings.HasPrefix(l[3], l[2]) {
			continue
		}
		if !slices.ContainsFunc(items, func(it feedback.Item) bool { return it.ID == l[3] }) {
			items = append(items, feedback.Item{ID: l[3], Project: l[1]})
		}
	}
	return items
}
