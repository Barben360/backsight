package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
)

const (
	recallBudget   = 17 * time.Second // SPEC §6.2
	firstMinChars  = 25               // the first prompt recalls from this length (C12)
	topicShift     = 0.15             // vocabulary overlap below which the topic changed (C12)
	stickyWindow   = 24 * time.Hour
	nudgeTerms     = 4
	approvalMaxLen = 40
)

// machineRE: prompts the harness wrote, not a human (C12, E1).
var machineRE = regexp.MustCompile(`^\s*(?:<(?:agent-message|task-notification|teammate-message|local-command-[\w-]+|system-reminder|bash-std(?:out|err)|command-name|command-message|command-args)\b|Another Claude session sent a message)`)

var fenceRE = regexp.MustCompile("(?s)```.*?(?:```|$)")

// Detector patterns (SPEC §9.3), French and English, whole words.
var (
	ruleRE     = regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(?:retiens|à partir de maintenant|désormais|on a décidé|jamais|toujours|remember|from now on|we decided|actually|never|always)(?:[^\pL\pN]|$)|^\s*non[,.!]?\s+pas\b`)
	approvalRE = regexp.MustCompile(`(?i)^\s*(?:ok(?:ay)?|parfait|valid[ée]|go|looks good|lgtm|yes|oui|d'accord|ça marche|perfect|top)(?:[^\pL\pN]|$)`)
)

// Credit detection on the previous reply (outcome telemetry, C12). The
// credit's 🧠 sets it apart from a plain "from memory"; the v3 wording,
// "From Hindsight memory", still counts.
var (
	creditRE = regexp.MustCompile(`(?i)🧠\s*\**\s*from memory\b|from hindsight memory`)
	emptyRE  = regexp.MustCompile(`(?i)nothing (?:relevant )?in (?:the |your )?memory|rien[^.\n]{0,25}en m[ée]moire`)
	usedRE   = regexp.MustCompile(`(?i)(?:according to|from) (?:the |your )?(?:long-term )?memory\b|memory (?:says|shows|notes|confirms)\b|(?:d['’]après|selon) (?:la|ta|ma) m[ée]moire\b`)
)

var wordRE = regexp.MustCompile(`[\pL\pN_]{4,}`)

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`alors aussi avec dans pour mais donc comme cette entre faire fait sont être avoir plus tout tous sans sous très bien peut ainsi quand dont leur leurs nous vous elle elles notre votre autre autres quel quelle voir veux veut faut fais mettre avant après encore maintenant ensuite depuis chose choses that this with from have what when will your there their about which would could should into than then them also just like make need want does done been were being some other more only very here where please again`) {
		stopwords[w] = true
	}
	handlers["user-prompt-submit"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, readSide)
		if !ok {
			return nil, nil
		}
		return h.userPrompt(), nil
	}
	handlers["post-tool-use"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, "") // stores are recorded with write on, the search flag with read on
		if !ok {
			return nil, nil
		}
		h.postToolUse()
		return nil, nil
	}
}

// maxWordChars: the vocabulary of a long paste is taken from its start.
const maxWordChars = 4000

// head is s cut to n runes.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func words(s string) []string {
	s = head(s, maxWordChars)
	var out []string
	seen := map[string]bool{}
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		if !stopwords[w] && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func overlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 1
	}
	inB := make(map[string]bool, len(b))
	for _, w := range b {
		inB[w] = true
	}
	inter := 0
	for _, w := range a {
		if inB[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// recallQuery: the prompt without fenced code, first QueryChars runes.
func (h *hookEnv) recallQuery(prompt string) string {
	q := strings.TrimSpace(fenceRE.ReplaceAllString(prompt, " "))
	if r := []rune(q); len(r) > h.cfg.Recall.QueryChars {
		q = string(r[:h.cfg.Recall.QueryChars])
	}
	q, _ = redact.Text(q) // a pasted key never leaves as a query
	return q
}

// decide is when to recall (C12): the first prompt, then after MinGap
// prompts or a topic shift, never past the session budget.
func (h *hookEnv) decide(st session.State, n int, prompt string) (bool, string) {
	rc := h.cfg.Recall
	switch {
	case st.CharsInjected >= rc.SessionBudgetChars:
		return false, "budget"
	case st.LastInject == 0:
		if len([]rune(prompt)) >= firstMinChars {
			return true, "first"
		}
		return false, "too_short"
	case len([]rune(prompt)) < rc.RecallMinChars:
		return false, "too_short"
	case n-st.LastInject >= rc.MinGap:
		return true, "gap"
	case overlap(st.LastWords, words(prompt)) < topicShift:
		return true, "topic_shift"
	}
	return false, "same_topic"
}

func detect(st session.State, prompt string) string {
	prompt = head(prompt, maxWordChars) // a rule is stated up front, not deep in a paste
	switch {
	case ruleRE.MatchString(prompt):
		return "rule"
	case len([]rune(prompt)) <= approvalMaxLen && approvalRE.MatchString(prompt) &&
		(strings.HasSuffix(strings.TrimSpace(st.LastAssistant), "?") || st.SubagentDone):
		return "approval"
	}
	return ""
}

func replyKind(text string) *string {
	var k string
	switch {
	case creditRE.MatchString(text):
		k = "exact"
	case emptyRE.MatchString(text):
		k = "empty"
	case usedRE.MatchString(text):
		k = "free"
	default:
		return nil
	}
	return &k
}

// userPrompt (SPEC §6.2, §9.3, §10.2): recall, reminder, nudge, detector
// line, and the visible failure once per session.
func (h *hookEnv) userPrompt() []byte {
	start := time.Now()
	prompt := h.in.Prompt
	promptAt := now()
	ev := telemetry.Prompt{Plen: len([]rune(prompt)), IDs: []string{}, Lang: promptLang(prompt)}
	if machineRE.MatchString(prompt) {
		h.beginTurn(0, true) // not a prompt, but its hooks journal under a turn of their own
		ev.Decision, ev.Reason = "skip", "machine"
		h.tw.Emit(ev)
		return nil
	}
	st, _ := h.store.Load(h.in.SessionID)
	n := st.Prompts + 1
	ev.N = n
	judge := st.LastInject == n-1 && n > 1
	toolCredit := judge && h.creditedSinceTurn() // read before beginTurn moves the pointer
	h.beginTurn(n, false)
	rc := h.cfg.Recall
	detector := detect(st, prompt)
	if detector != "" {
		ev.Detector = &detector
	}

	// What followed the previous prompt's injection.
	if judge {
		var ids []string
		for _, in := range st.Injected {
			if in.Prompt == n-1 {
				ids = append(ids, in.ID)
			}
		}
		credited := replyKind(st.LastAssistant)
		if st.LastCredit != "" {
			credited = &st.LastCredit // computed at Stop over the whole reply
		}
		source := ""
		switch {
		case toolCredit:
			source = "tool"
		case credited != nil && (*credited == "exact" || *credited == "free"):
			source = "citation"
		}
		h.tw.Emit(telemetry.Outcome{N: n - 1, IDs: ids, Searched: st.SearchedPromptID == n-1, Credited: credited, CreditSource: source, Contradicted: detector == "rule"})
	}

	var parts []string
	var injected []session.Injection
	banner := ""
	ok, reason := h.decide(st, n, prompt)
	switch {
	case ok && h.maintenance():
		ok, reason = false, "maintenance"
		if !st.Unavailable {
			banner = instructions.MemoryMaintenance("")
		}
	case ok && h.breakerOpen():
		ok, reason = false, "breaker"
	}
	ev.Decision, ev.Reason = "skip", reason
	memoryChars := 0
	if ok {
		ev.Decision = "recall"
		text, chosen, items, res := h.promptRecall(st, prompt, reason)
		ev.Scopes = map[string]telemetry.ScopeResult{}
		failed := len(res.Scopes) > 0
		for name, m := range res.Scopes {
			ev.Scopes[name] = telemetry.ScopeResult{OK: m.OK, MS: m.MS, Raw: m.Raw}
			failed = failed && !m.OK
		}
		h.breakerRecord(failed)
		ev.Above = len(res.Hits)
		if len(res.Hits) > 0 {
			ev.Top = res.Hits[0].Reranker
		}
		if failed && !st.Unavailable {
			banner = instructions.MemoryUnavailable("", "server unreachable")
		}
		if text != "" {
			parts = append(parts, text+"\n"+h.texts().RecallEnd)
			injected = items
			memoryChars = len([]rune(text))
			h.journalShows("prompt", itemsFromHits(chosen))
		}
	}
	if n == 1 && len([]rune(prompt)) >= rc.ReminderMinChars {
		parts = append(parts, h.texts().Reminder)
		ev.Hinted = true
	}
	nudged := false
	// Quiet-prompt nudge (SPEC §10.2, C12): only when the prompt names at
	// least two things the memory knows about.
	if len(injected) == 0 && len([]rune(prompt)) >= rc.ReminderMinChars && n-1-max(st.SearchedPromptID, st.LastNudge) >= rc.QuietPrompts {
		if idx, err := recall.LoadIndex(h.dirs.EntitiesFile()); err == nil {
			if terms := idx.Mentioned(prompt, nudgeTerms); len(terms) >= 2 {
				parts = append(parts, instructions.Nudge(terms))
				nudged, ev.Nudged = true, true
			}
		}
	}
	switch detector {
	case "rule":
		parts = append(parts, instructions.DetectorRule)
	case "approval":
		parts = append(parts, instructions.DetectorApproval)
	}
	text := strings.Join(parts, "\n\n")

	ids := make([]string, 0, len(injected))
	for _, in := range injected {
		ids = append(ids, in.ID)
	}
	ev.IDs, ev.Injected, ev.Chars = ids, len(ids), len([]rune(text))
	h.updateSession(func(s *session.State) error {
		s.Project, s.Remote, s.ProjectExplicit = h.project, h.remote(), h.resolver.Explicit(h.cwd)
		s.Prompts = max(s.Prompts, n)
		s.LastPromptAt = promptAt
		s.SubagentDone = false
		if nudged {
			s.LastNudge = n
		}
		if banner != "" {
			s.Unavailable = true
		}
		if ev.Decision == "recall" {
			// Every recall decision restarts the gap, hit or not (C12):
			// otherwise each prompt recalls again until something is found.
			red, _ := redact.Text(head(prompt, maxWordChars))
			s.LastInject, s.LastWords = n, words(red)
		}
		for _, in := range injected {
			if !slices.Contains(s.Seen, in.ID) {
				s.Seen = append(s.Seen, in.ID)
			}
		}
		s.Injected = append(s.Injected, injected...)
		s.KeepShownTexts(injectionTexts(injected))
		s.CharsInjected += memoryChars // memory text only, not reminders or nudges
		return nil
	})
	if len(ids) > 0 {
		_ = h.sticky().Record(ids, now(), h.stickyWindow())
	}
	ev.MS = int(time.Since(start).Milliseconds())
	h.tw.Emit(ev)
	return contextOutput("UserPromptSubmit", text, banner)
}

// age renders " (4d ago)", or nothing for an unknown date.
func age(a string) string {
	if a == "" {
		return ""
	}
	return " (" + a + " ago)"
}

// stickyWindow is A.1 recall.sticky_window_h.
func (h *hookEnv) stickyWindow() time.Duration {
	if h.cfg.Recall.StickyWindowH > 0 {
		return time.Duration(h.cfg.Recall.StickyWindowH) * time.Hour
	}
	return stickyWindow
}

func (h *hookEnv) sticky() session.Sticky {
	return session.Sticky{Path: h.dirs.State + "/sticky.json", Locks: h.dirs.LocksDir()}
}

// promptRecall searches the prompt and keeps up to PromptHits unseen,
// unmuted memories above the reranker floor, within PromptChars.
func (h *hookEnv) promptRecall(st session.State, prompt, rule string) (string, []recall.Hit, []session.Injection, recall.Result) {
	_, res := h.searchPrompt(prompt)
	counts := h.sticky().Load(now(), h.stickyWindow())
	rc := h.cfg.Recall
	block, chosen := selectHits(res.Hits, rc, func(hit recall.Hit) bool {
		return slices.Contains(st.Seen, hit.ID) || session.Muted(counts, hit.ID, hit.Reranker, rc.StickyMax, rc.StickyBypass)
	})
	if len(chosen) == 0 {
		return "", nil, nil, res
	}
	at := now()
	items := make([]session.Injection, 0, len(chosen))
	for _, hit := range chosen {
		items = append(items, session.Injection{ID: hit.ID, Text: hit.Text, Project: hit.Project, Score: hit.Score, Scope: hit.Scope, Prompt: st.Prompts + 1, Rule: rule, At: at})
	}
	return block, chosen, items, res
}

// searchPrompt is the prompt path's query and recall (SPEC §10.2), shared
// with `recall-prompt` so the replay gate measures exactly the hook.
func (h *hookEnv) searchPrompt(prompt string) (string, recall.Result) {
	rc := h.cfg.Recall
	q := h.recallQuery(prompt)
	var related []string
	if idx, err := recall.LoadIndex(h.dirs.EntitiesFile()); err == nil {
		related = idx.Projects(q, h.project, h.cfg.Projects.Transverse)
	}
	eng := &recall.Engine{Client: h.client, Bank: h.cfg.Server.Bank, Transverse: h.cfg.Projects.Transverse, Timeout: recallBudget, Now: now, Boost: h.cfg.Recall.ProjectBoost}
	return q, eng.Search(h.ctx, recall.Query{Queries: []string{q}, Current: h.project, Linked: h.linked, Related: related, MinReranker: rc.MinScore, GlobalMinReranker: rc.GlobalMinScore, Limit: 20})
}

var (
	// "on" is English and French alike: in neither list.
	frWords    = wordSet("le la les des du de un une et est sont pour dans avec sur par qui que pas ne au aux ce cette tu je il elle nous vous fait faire si mais c'est ça")
	enWords    = wordSet("the is are was were for with by which that not to of and has have this you we it do make if but")
	langWordRE = regexp.MustCompile(`[a-zàâçéèêëîïôûùüÿœ']+`)
)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// promptLang is "fr" when French function words outnumber English ones in
// the prompt's head, else "en" (E23's heuristic, with "on" dropped and the
// commonest short French words added).
// Telemetry only: E26 found no floor shift worth making (decision 0058),
// and stats and E24 measure French prompts apart.
func promptLang(prompt string) string {
	fr, en := 0, 0
	if len(prompt) > maxWordChars {
		prompt = prompt[:maxWordChars] // a cut rune is no function word
	}
	for _, w := range langWordRE.FindAllString(strings.ToLower(prompt), -1) {
		switch {
		case frWords[w]:
			fr++
		case enWords[w]:
			en++
		}
	}
	if fr > en {
		return "fr"
	}
	return "en"
}

// selectHits keeps up to PromptHits hits that skip does not drop, within
// PromptChars, and renders the injected block.
func selectHits(hits []recall.Hit, rc config.Recall, skip func(recall.Hit) bool) (string, []recall.Hit) {
	var b strings.Builder
	var chosen []recall.Hit
	for _, hit := range hits {
		if len(chosen) == rc.PromptHits {
			break
		}
		if skip(hit) {
			continue
		}
		room := rc.PromptChars - len([]rune(b.String()))
		prefix, suffix := "- "+memoryTag(hit.Project, hit.ID)+" ", age(hit.Age)+"\n"
		textRoom := room - len([]rune(prefix)) - len([]rune(suffix))
		if textRoom < 60 {
			break
		}
		b.WriteString(prefix + clip(hit.Text, textRoom) + suffix)
		chosen = append(chosen, hit)
	}
	if len(chosen) == 0 {
		return "", nil
	}
	return instructions.RecallHeader + "\n" + strings.TrimRight(b.String(), "\n"), chosen
}

var (
	memoryToolRE = regexp.MustCompile(`__memory_(?:search|read|store|credit)$`)
	uuidRE       = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	noteRE       = regexp.MustCompile(`\bnote:[0-9a-f-]{36}\b`)
)

// postToolUse marks that a memory tool ran during this prompt (the gate's
// flag), adds returned ids to the seen set and stores to the session, and
// journals shows and credits.
func (h *hookEnv) postToolUse() {
	if !memoryToolRE.MatchString(h.in.ToolName) {
		return
	}
	text := responseText(h.in.ToolResponse)
	if strings.HasSuffix(h.in.ToolName, "memory_credit") {
		h.journalCredit(text) // naming memories already shown is no search: the gate's flag and the seen set stay
		return
	}
	var ids, notes []string
	if strings.HasSuffix(h.in.ToolName, "memory_store") {
		notes = noteRE.FindAllString(text, -1)
	} else {
		ids = uuidRE.FindAllString(text, -1)
	}
	subagent := h.in.AgentID != "" // a subagent's search is not the main thread's
	n := -1
	// Parallel tool calls run their PostToolUse hooks at once, each with
	// an fsync'd write under the session lock: this hook (deadline 1.5 s)
	// waits up to 1 s rather than the prompt path's 100 ms.
	h.store.Retry = time.Second
	var shownTexts map[string]string
	if strings.HasSuffix(h.in.ToolName, "memory_search") {
		shownTexts = searchTexts(text)
	}
	h.updateSession(func(s *session.State) error {
		n = s.Prompts
		s.KeepShownTexts(shownTexts) // a subagent's too: the end of its turn asks about them
		if !subagent {
			s.SearchedPromptID = s.Prompts
			for _, id := range ids {
				if !slices.Contains(s.Seen, id) {
					s.Seen = append(s.Seen, id)
				}
			}
		}
		s.Stored = append(s.Stored, notes...)
		return nil
	})
	// The MCP server logs the call itself (tool event); this one only ties
	// it to the session, so stats do not count calls twice.
	if n < 0 { // the lock stayed busy: the prompt number, read without it
		st, _ := h.store.Load(h.in.SessionID)
		n = st.Prompts
	}
	// n ties it to the prompt (detector follow-up), memory_ids to the
	// briefing (briefing use).
	h.tw.Emit(telemetry.Raw{Name: "tool_use", Fields: map[string]any{"name": h.in.ToolName, "ids": len(ids), "memory_ids": ids, "n": n, "subagent": subagent}})
	switch {
	case strings.HasSuffix(h.in.ToolName, "memory_search"):
		h.journalShows("search", searchShows(text))
	case strings.HasSuffix(h.in.ToolName, "memory_read"):
		h.journalShows("read", readShow(h.in.ToolInput))
	}
}

// responseText flattens a tool response: a string, or MCP content blocks.
func responseText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			parts = append(parts, b.Text)
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprint(string(raw))
}
