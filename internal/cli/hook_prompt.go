package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

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

// Credit detection on the previous reply (outcome telemetry, C12).
var (
	creditRE = regexp.MustCompile(`(?i)from hindsight memory`)
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
		h, ok := newHookEnv(ctx, input, readSide)
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
	ev := telemetry.Prompt{Plen: len([]rune(prompt)), IDs: []string{}}
	if machineRE.MatchString(prompt) {
		ev.Decision, ev.Reason = "skip", "machine"
		h.tw.Emit(ev)
		return nil
	}
	st, _ := h.store.Load(h.in.SessionID)
	n := st.Prompts + 1
	ev.N = n
	rc := h.cfg.Recall
	detector := detect(st, prompt)
	if detector != "" {
		ev.Detector = &detector
	}

	// What followed the previous prompt's injection.
	if st.LastInject == n-1 && n > 1 {
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
		h.tw.Emit(telemetry.Outcome{N: n - 1, IDs: ids, Searched: st.SearchedPromptID == n-1, Credited: credited, Contradicted: detector == "rule"})
	}

	var parts []string
	var injected []session.Injection
	banner := ""
	ok, reason := h.decide(st, n, prompt)
	switch {
	case ok && h.maintenance():
		ok, reason = false, "maintenance"
		if !st.Unavailable {
			banner = instructions.MemoryUnavailable("maintenance in progress")
		}
	case ok && h.breakerOpen():
		ok, reason = false, "breaker"
	}
	ev.Decision, ev.Reason = "skip", reason
	memoryChars := 0
	if ok {
		ev.Decision = "recall"
		text, items, res := h.promptRecall(st, prompt, reason)
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
			banner = instructions.MemoryUnavailable("server unreachable")
		}
		if text != "" {
			parts = append(parts, text)
			injected = items
			memoryChars = len([]rune(text))
		}
	}
	if n == 1 && len([]rune(prompt)) >= rc.ReminderMinChars {
		parts = append(parts, instructions.Reminder)
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
		s.Project = h.project
		s.Prompts = max(s.Prompts, n)
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
		s.CharsInjected += memoryChars // memory text only, not reminders or nudges
		return nil
	})
	if len(ids) > 0 {
		_ = h.sticky().Record(ids, now(), stickyWindow)
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

func (h *hookEnv) sticky() session.Sticky {
	return session.Sticky{Path: h.dirs.State + "/sticky.json", Locks: h.dirs.LocksDir()}
}

// promptRecall searches the prompt and keeps up to PromptHits unseen,
// unmuted memories above the reranker floor, within PromptChars.
func (h *hookEnv) promptRecall(st session.State, prompt, rule string) (string, []session.Injection, recall.Result) {
	rc := h.cfg.Recall
	q := h.recallQuery(prompt)
	var related []string
	if idx, err := recall.LoadIndex(h.dirs.EntitiesFile()); err == nil {
		related = idx.Projects(q, h.project, h.cfg.Projects.Transverse)
	}
	eng := &recall.Engine{Client: h.client, Bank: h.cfg.Server.Bank, Transverse: h.cfg.Projects.Transverse, Timeout: recallBudget, Now: now}
	res := eng.Search(h.ctx, recall.Query{Queries: []string{q}, Current: h.project, Related: related, MinReranker: rc.MinScore, GlobalMinReranker: rc.GlobalMinScore, Limit: 20})
	counts := h.sticky().Load(now(), stickyWindow)
	var b strings.Builder
	var items []session.Injection
	at := now()
	for _, hit := range res.Hits {
		if len(items) == rc.PromptHits {
			break
		}
		if slices.Contains(st.Seen, hit.ID) || session.Muted(counts, hit.ID, hit.Reranker, rc.StickyMax, rc.StickyBypass) {
			continue
		}
		room := rc.PromptChars - len([]rune(b.String()))
		prefix, suffix := "- ["+safeProject(hit.Project)+"] ", age(hit.Age)+"\n"
		textRoom := room - len([]rune(prefix)) - len([]rune(suffix))
		if textRoom < 60 {
			break
		}
		b.WriteString(prefix + clip(hit.Text, textRoom) + suffix)
		items = append(items, session.Injection{ID: hit.ID, Text: hit.Text, Project: hit.Project, Score: hit.Score, Scope: hit.Scope, Prompt: st.Prompts + 1, Rule: rule, At: at})
	}
	if len(items) == 0 {
		return "", nil, res
	}
	return instructions.RecallHeader + "\n" + strings.TrimRight(b.String(), "\n"), items, res
}

var (
	memoryToolRE = regexp.MustCompile(`__memory_(?:search|read|store)$`)
	uuidRE       = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	noteRE       = regexp.MustCompile(`\bnote:[0-9a-f-]{36}\b`)
)

// postToolUse marks that a memory tool ran during this prompt (the gate's
// flag), adds returned ids to the seen set and stores to the session.
func (h *hookEnv) postToolUse() {
	if !memoryToolRE.MatchString(h.in.ToolName) {
		return
	}
	text := responseText(h.in.ToolResponse)
	var ids, notes []string
	if strings.HasSuffix(h.in.ToolName, "memory_store") {
		notes = noteRE.FindAllString(text, -1)
	} else {
		ids = uuidRE.FindAllString(text, -1)
	}
	h.updateSession(func(s *session.State) error {
		s.SearchedPromptID = s.Prompts
		for _, id := range ids {
			if !slices.Contains(s.Seen, id) {
				s.Seen = append(s.Seen, id)
			}
		}
		s.Stored = append(s.Stored, notes...)
		return nil
	})
	// The MCP server logs the call itself (tool event); this one only ties
	// it to the session, so stats do not count calls twice.
	h.tw.Emit(telemetry.Raw{Name: "tool_use", Fields: map[string]any{"name": h.in.ToolName, "ids": len(ids)}})
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
