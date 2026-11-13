package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
)

// recallBudget is a prompt recall's budget (SPEC §6.2). probeBudget is a
// recall's once the breaker's pause is over, until one answers: above the
// slowest recalls measured on a CPU-only server, so a server still hanging
// costs a prompt that much at most, not the whole budget (decision 0091).
// Variables for the tests.
var (
	recallBudget = 17 * time.Second
	probeBudget  = 8 * time.Second
)

const (
	firstMinChars  = 25   // the first prompt recalls from this length (C12)
	topicShift     = 0.15 // vocabulary overlap below which the topic changed (C12)
	stickyWindow   = 24 * time.Hour
	nudgeTerms     = 4
	approvalMaxLen = 40
)

// searchBudget gives the hooks' recalls their budget: nil, the engine's
// own deadline (tests end it on cue).
var searchBudget func(context.Context, time.Duration) (context.Context, context.CancelFunc)

// machinePrompter is a harness that tells the prompts it writes itself
// apart (Codex), beside the ones machineRE knows.
type machinePrompter interface{ MachinePrompt(prompt string) bool }

// harnessMachinePrompt: a's own message, by its rule; false for a harness
// that has none.
func harnessMachinePrompt(a harness.Adapter, prompt string) bool {
	m, ok := a.(machinePrompter)
	return ok && m.MachinePrompt(prompt)
}

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

// refRE: a link, or an issue or pull request reference (#12, owner/repo#12).
var refRE = regexp.MustCompile(`https?://[^\s<>()\[\]"'\x60]+|(?:[\w.-]+/[\w.-]+)?#\d+\b`)

// pointerVerbs: the words that say what to do with a reference, not what
// it is about.
var pointerVerbs = map[string]bool{}

// pointerMaxWords: below this many content words besides its references,
// a prompt only points at something.
const pointerMaxWords = 3

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`alors aussi avec dans pour mais donc comme cette entre faire fait sont être avoir plus tout tous sans sous très bien peut ainsi quand dont leur leurs nous vous elle elles notre votre autre autres quel quelle voir veux veut faut fais mettre avant après encore maintenant ensuite depuis chose choses that this with from have what when will your there their about which would could should into than then them also just like make need want does done been were being some other more only very here where please again`) {
		stopwords[w] = true
	}
	for _, w := range strings.Fields(`issue issues ticket pull request review check look read implement handle solve close address investigate continue thanks merci regarde corrige traite implémente vérifie relis résous ferme`) {
		pointerVerbs[w] = true
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

// redactMargin is how far past a cut the text is redacted with what is
// kept: a secret straddling the cut is seen whole, and one longer than
// that is still past its pattern's minimum length where it is cut.
// Redacting a whole long paste would cost a hook too much.
const redactMargin = 4096

// redactMaxWindow caps the window redactHead and redactTail redact: the
// rules run at about half a megabyte a second, and a hook calls them
// twice per prompt.
const redactMaxWindow = 1 << 16

// redactText is redact.Text; tests watch what it is given.
var redactText = redact.Text

// redactHead is s's first n runes, redacted before the cut so that a key
// straddling it never stays as its prefix (decision 0088). The window
// grows when redaction shrank it to near n runes (long keys replaced), so
// that the cut never lands within redactMargin/2 of its end, up to
// redactMaxWindow. Still short there, it fails closed: the window's last
// redactMargin runes, where a key cut at the cap would sit, are dropped.
func redactHead(s string, n int) string {
	maxW := max(redactMaxWindow, n+redactMargin)
	for w := n + redactMargin; ; w = min(2*w, maxW) {
		cut := head(s, w)
		red, _ := redactText(cut)
		rn := utf8.RuneCountInString(red)
		switch {
		case len(cut) == len(s) || rn >= n+redactMargin/2:
			return head(red, n)
		case w >= maxW:
			return head(red, min(n, max(0, rn-redactMargin)))
		}
	}
}

// redactTail is s's last n runes, redacted before the cut, as redactHead,
// the window's first runes dropped when it fails closed. A window that
// starts inside a private key block starts at its header instead, when
// that is within redactMaxWindow; else it starts after the block's END
// line, or is empty: a key's body without its header is not recognised
// as one.
func redactTail(s string, n int) string {
	maxW := max(redactMaxWindow, n+redactMargin)
	for w := n + redactMargin; ; w = min(2*w, maxW) {
		from := tailStart(s, w)
		if b := redact.OpenKeyBefore(s, from); b >= 0 {
			if utf8.RuneCountInString(s[b:from]) <= redactMaxWindow {
				from = b
			} else {
				cut := s[from:]
				if e := redact.KeyEnd(cut); e >= 0 {
					red, _ := redactText(cut[e:])
					return runeTail(red, n)
				}
				return ""
			}
		}
		red, _ := redactText(s[from:])
		rn := utf8.RuneCountInString(red)
		switch {
		case from == 0 || rn >= n+redactMargin/2:
			return runeTail(red, n)
		case w >= maxW:
			return runeTail(red, min(n, max(0, rn-redactMargin)))
		}
	}
}

// tailStart is where s's last n runes start.
func tailStart(s string, n int) int {
	i := len(s)
	for ; n > 0 && i > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return i
}

// runeTail is s's last n runes.
func runeTail(s string, n int) string { return s[tailStart(s, n):] }

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

// pointerRefs is the references of a prompt that only points at
// something ("Fix https://…/issues/144", "regarde #12"): searching its
// text finds little, the content behind the link says what it is about.
// nil when the prompt has no reference or says enough by itself.
func pointerRefs(prompt string) []string {
	prompt = head(fenceRE.ReplaceAllString(prompt, " "), maxWordChars)
	var refs []string
	for _, r := range refRE.FindAllString(prompt, -1) {
		if r = strings.TrimRight(r, ".,;:!?"); !slices.Contains(refs, r) {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	n := 0
	for _, w := range words(refRE.ReplaceAllString(prompt, " ")) {
		if !pointerVerbs[w] {
			n++
		}
	}
	if n >= pointerMaxWords {
		return nil
	}
	return refs
}

// recallQuery: the prompt without fenced code, first QueryChars runes,
// redacted before the cut: a pasted key never leaves as a query, nor as
// its prefix.
func (h *hookEnv) recallQuery(prompt string) string {
	return redactHead(strings.TrimSpace(fenceRE.ReplaceAllString(prompt, " ")), h.cfg.Recall.QueryChars)
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
	if h.in.AgentID != "" && h.adapter.ID() == harnessid.Codex {
		// A subagent's task (Codex fires the prompt hook for it, 0085 D3):
		// SubagentStart gave it the short guide; it is no prompt of the
		// session, and the turn stays its parent's, whose end is checked.
		// Codex's only: Claude Code's prompt path stays as it was.
		ev.Decision, ev.Reason = "skip", "subagent"
		h.tw.Emit(ev)
		return nil
	}
	if machineRE.MatchString(prompt) || harnessMachinePrompt(h.adapter, prompt) {
		h.beginTurn(0, true) // not a prompt, but its hooks journal under a turn of their own
		// the next human prompt does not answer the last injection
		h.updateSession(func(s *session.State) error { s.MachineAfter = s.Prompts; return nil })
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
		// A correction judges the injection only when nothing but the
		// reply came between: after a machine message the prompt answers
		// that, not the memory (issue 317).
		contradicted := detector == "rule" && st.MachineAfter != n-1
		h.tw.Emit(telemetry.Outcome{N: n - 1, IDs: ids, Searched: st.SearchedPromptID == n-1, Credited: credited, CreditSource: source, Contradicted: contradicted})
	}

	var parts []string
	var injected []session.Injection
	banner := ""
	ok, reason := h.decide(st, n, prompt)
	var refs []string // only where a recall was due: a follow-up's "merge #152" is no new topic
	if ok {
		refs = pointerRefs(prompt)
	}
	switch {
	case refs != nil:
		// Not searched, and the gap does not restart: the prompt after
		// the reading recalls as if this one had not been sent.
		ok, reason = false, "pointer"
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
		budget := h.budget(recallBudget)
		sel, flags, items, res, empty := h.promptRecall(st, prompt, reason, budget)
		text, chosen := sel.Block, sel.Chosen
		ev.Empty = empty
		ev.CutCount, ev.CutSize, ev.Shortened = sel.CutCount, sel.CutSize, sel.Shortened
		ev.Scopes = map[string]telemetry.ScopeResult{}
		failed := len(res.Scopes) > 0
		for name, m := range res.Scopes {
			ev.Scopes[name] = telemetry.ScopeResult{OK: m.OK, MS: m.MS, Raw: m.Raw}
			failed = failed && !m.OK
		}
		h.breakerRecord(failed, res.TimedOut && budget >= probeBudget)
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
			h.journalShows("prompt", itemsFromHits(chosen), flags...)
		}
	}
	switch {
	case refs != nil:
		parts = append(parts, instructions.Pointer(refs))
		ev.Hinted = true
	case n == 1 && len([]rune(prompt)) >= rc.ReminderMinChars:
		parts = append(parts, h.texts().Reminder)
		ev.Hinted = true
	}
	nudged := false
	// Quiet-prompt nudge (SPEC §10.2, C12): only when the prompt names at
	// least two things the memory knows about.
	if refs == nil && len(injected) == 0 && len([]rune(prompt)) >= rc.ReminderMinChars && n-1-max(st.SearchedPromptID, st.LastNudge) >= rc.QuietPrompts {
		if idx, err := recall.LoadIndex(h.dirs.EntitiesFile()); err == nil {
			if terms := idx.Mentioned(prompt, nudgeTerms); len(terms) >= 2 {
				parts = append(parts, instructions.Nudge(terms))
				nudged, ev.Nudged = true, true
			}
		}
	}
	// Other projects the prompt names (#271): each once per session; the
	// recall above searched them already (searchPrompt).
	var named []string
	if idx, err := recall.LoadIndex(h.dirs.EntitiesFile()); err == nil {
		named = idx.Named(prompt, append(h.namedSkip(), st.NamedProjects...))
	}
	if len(named) > 0 {
		safe := make([]string, len(named))
		for i, p := range named {
			safe[i] = safeProject(p)
		}
		parts = append(parts, instructions.ProjectNamed(safe))
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
	if ev.Injected > 0 {
		ev.Empty = ""
	}
	h.updateSession(func(s *session.State) error {
		s.Project, s.Remote, s.ProjectExplicit = h.project, h.remote(), h.resolver.Explicit(h.cwd)
		s.Prompts = max(s.Prompts, n)
		s.LastPromptAt = promptAt
		s.SubagentDone = false
		if nudged {
			s.LastNudge = n
		}
		for _, p := range named {
			if !slices.Contains(s.NamedProjects, p) {
				s.NamedProjects = append(s.NamedProjects, p)
			}
		}
		if banner != "" {
			s.Unavailable = true
		}
		if ev.Decision == "recall" {
			// Every recall decision restarts the gap, hit or not (C12):
			// otherwise each prompt recalls again until something is found.
			s.LastInject, s.LastWords = n, words(redactHead(prompt, maxWordChars))
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
	return h.answer(harness.EventUserPromptSubmit, text, banner)
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
// unmuted memories above the reranker floor, within PromptChars; flags
// holds set_changed when the boost changed which ones (boostChangedSet).
// empty says why nothing was chosen (the prompt event's empty field):
// error (the search failed, whatever the bank holds), no_candidates,
// below_floor, all_seen or capped; "" when something was.
func (h *hookEnv) promptRecall(st session.State, prompt, rule string, budget time.Duration) (sel selection, flags []string, items []session.Injection, res recall.Result, empty string) {
	_, res = h.searchPrompt(prompt, budget)
	counts := h.sticky().Load(now(), h.stickyWindow())
	rc := h.cfg.Recall
	// Within what the session budget has left, like the failure recall.
	rc.PromptChars = min(rc.PromptChars, rc.SessionBudgetChars-st.CharsInjected)
	skip := func(hit recall.Hit) bool {
		return slices.Contains(st.Seen, hit.ID) || session.Muted(counts, hit.ID, hit.Reranker, rc.StickyMax, rc.StickyBypass)
	}
	sel = selectHits(res.Hits, rc, skip)
	chosen := sel.Chosen
	if len(chosen) == 0 {
		switch {
		case len(res.Hits) == 0 && searchFailed(res):
			empty = "error"
		case len(res.Hits) == 0 && rawHits(res) == 0:
			empty = "no_candidates"
		case len(res.Hits) == 0:
			empty = "below_floor"
		case !slices.ContainsFunc(res.Hits, func(hit recall.Hit) bool { return !skip(hit) }):
			empty = "all_seen"
		default:
			empty = "capped"
		}
		return sel, nil, nil, res, empty
	}
	if boostChangedSet(res.Hits, chosen, rc, skip) {
		flags = []string{feedback.FlagSetChanged}
	}
	at := now()
	items = make([]session.Injection, 0, len(chosen))
	for _, hit := range chosen {
		items = append(items, session.Injection{ID: hit.ID, Text: hit.Text, Project: hit.Project, Source: hit.Source, Score: hit.Score, Scope: hit.Scope, Prompt: st.Prompts + 1, Rule: rule, At: at})
	}
	return sel, flags, items, res, ""
}

// searchFailed: every scope failed (server error, timeout, refused key), so
// the empty result says nothing about the bank. One scope answering is
// enough for the hits to speak. An open breaker never gets here: it skips the
// recall ("breaker").
func searchFailed(res recall.Result) bool {
	for _, m := range res.Scopes {
		if m.OK {
			return false
		}
	}
	return len(res.Scopes) > 0
}

// rawHits is how many hits the scopes returned before the reranker floor.
func rawHits(res recall.Result) int {
	n := 0
	for _, m := range res.Scopes {
		n += m.Raw
	}
	return n
}

// boostChangedSet: the boost changed which hits a prompt injects. The
// order without it is already known (each hit's RankWithoutBoost, from
// the search), so the same selection runs again on it: another set, or a
// hit only the boost let in, changes it; so does a rank missing before
// the last one picked (a hit the boost pushed out of the list). Without
// a ledger, no hit has a rank without the boost: nothing changed.
func boostChangedSet(hits, chosen []recall.Hit, rc config.Recall, skip func(recall.Hit) bool) bool {
	var plain []recall.Hit
	for _, h := range hits {
		switch {
		case h.RankWithoutBoost != 0:
			plain = append(plain, h)
		case h.Boost != 0 && slices.ContainsFunc(chosen, func(c recall.Hit) bool { return c.ID == h.ID }):
			return true // only the boost let it in
		}
	}
	if len(plain) == 0 {
		return false
	}
	slices.SortFunc(plain, func(a, b recall.Hit) int { return cmp.Compare(a.RankWithoutBoost, b.RankWithoutBoost) })
	picked := selectHits(plain, rc, skip).Chosen
	if len(picked) != len(chosen) {
		return true
	}
	if len(picked) > 0 {
		last := picked[len(picked)-1]
		if i := slices.IndexFunc(plain, func(p recall.Hit) bool { return p.ID == last.ID }); plain[i].RankWithoutBoost != i+1 {
			return true // a rank missing before the last pick
		}
	}
	for _, h := range picked {
		if !slices.ContainsFunc(chosen, func(c recall.Hit) bool { return c.ID == h.ID }) {
			return true
		}
	}
	return false
}

// namedSkip: the projects a prompt naming them brings nothing new for:
// this one, the transverse pool and the linked projects, all searched
// anyway.
func (h *hookEnv) namedSkip() []string {
	return append(append([]string{h.project}, h.cfg.Projects.Transverse...), h.linked...)
}

// searchPrompt is the prompt path's query and recall (SPEC §10.2), shared
// with `recall-prompt` so the replay gate measures exactly the hook.
func (h *hookEnv) searchPrompt(prompt string, budget time.Duration) (string, recall.Result) {
	rc := h.cfg.Recall
	q := h.recallQuery(prompt)
	var related, named []string
	if idx, err := recall.LoadIndex(h.dirs.EntitiesFile()); err == nil {
		related = idx.Projects(q, h.project, h.cfg.Projects.Transverse)
		named = idx.Named(q, h.namedSkip()) // searched as if asked for: no floor for other projects
	}
	eng := &recall.Engine{Client: h.client, Bank: h.cfg.Server.Bank, Transverse: h.cfg.Projects.Transverse, Timeout: budget, Budget: searchBudget, Now: now, Boost: h.cfg.Recall.ProjectBoost,
		Adjust: ledgerAdjust(h.dirs, h.cfg, now(), h.tw)}
	return q, eng.Search(h.ctx, recall.Query{Queries: []string{q}, Current: h.project, Linked: h.linked, Projects: named, Related: related, MinReranker: rc.MinScore, GlobalMinReranker: rc.GlobalMinScore, Limit: 20, Provenance: true})
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

// headlineChars is the text of a memory shown shortened: about a
// sentence, enough to tell what it is about, its id kept for memory_read.
// minTextChars is the least text a line is worth.
const (
	headlineChars = 160
	minTextChars  = 60
)

// selection is what selectHits kept and what the limits left out (issue
// 328): the prompt event's cut_count, cut_size and shortened.
type selection struct {
	Block     string
	Chosen    []recall.Hit
	CutCount  int // above the floor, left out by the count (checked first: such a hit may not have fitted either)
	CutSize   int // left out because not even a headline fitted
	Shortened int // shown as a headline
}

// selectHits keeps, in rank order, the hits that skip does not drop: up to
// PromptHits, and up to PromptHitsMax when each extra one's reranker score
// is at least PromptConfidentScore, within PromptChars. A hit that does
// not fit whole is shown as a headline ending with "…" (memory_read has
// the rest); when not even that fits, the next one is still tried.
func selectHits(hits []recall.Hit, rc config.Recall, skip func(recall.Hit) bool) selection {
	var s selection
	var b strings.Builder
	used := 0
	for _, hit := range hits {
		if skip(hit) {
			continue
		}
		if n := len(s.Chosen); n >= rc.PromptHits && (n >= rc.PromptHitsMax || hit.Reranker < rc.PromptConfidentScore) {
			s.CutCount++
			continue
		}
		prefix, suffix := "- "+memoryTag(hit.Project, hit.Source, hit.ID)+" ", age(hit.Age)+"\n"
		textRoom := rc.PromptChars - used - len([]rune(prefix)) - len([]rune(suffix))
		text := strings.Join(strings.Fields(hit.Text), " ")
		switch {
		case len([]rune(text)) <= textRoom:
		case textRoom >= minTextChars:
			text = clip(text, min(textRoom, headlineChars))
			s.Shortened++
		default:
			s.CutSize++
			continue
		}
		line := prefix + text + suffix
		b.WriteString(line)
		used += len([]rune(line))
		s.Chosen = append(s.Chosen, hit)
	}
	if len(s.Chosen) == 0 {
		return selection{CutCount: s.CutCount, CutSize: s.CutSize}
	}
	s.Block = instructions.RecallHeader + "\n" + strings.TrimRight(b.String(), "\n")
	return s
}

var (
	memoryToolRE = regexp.MustCompile(`__memory_(?:search|read|store|credit)$`)
	uuidRE       = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	noteRE       = regexp.MustCompile(`\bnote:[0-9a-f-]{36}\b`)
)

// postToolUseLockWait is how long PostToolUse waits for the session lock.
// Parallel tool calls run their PostToolUse hooks at once, each with an
// fsync'd write under that lock: this hook (deadline 1.5 s) waits up to
// 1 s rather than the prompt path's 100 ms. A variable so a test can bound
// the wait by itself rather than by its disk's speed.
var postToolUseLockWait = time.Second

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
	var stored []feedback.Item
	if strings.HasSuffix(h.in.ToolName, "memory_store") {
		// the note's id is on the answer's first lines, never in the
		// similar memory's text below them
		notes = noteRE.FindAllString(storeHead(text), -1)
		stored = storeShows(text)
		for _, it := range stored {
			ids = append(ids, it.ID)
		}
	} else {
		ids = uuidRE.FindAllString(text, -1)
	}
	subagent := h.in.AgentID != "" // a subagent's search is not the main thread's
	n := -1
	h.store.Retry = postToolUseLockWait
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
		h.journalShows("read", h.readShow(h.in.ToolInput))
	case strings.HasSuffix(h.in.ToolName, "memory_store"):
		h.journalShows(feedback.PathStore, stored)
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
