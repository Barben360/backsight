package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/session"
)

const longPrompt = "Which PostgreSQL version does the billing service run, and why did we pick it last month?"

func hits(scores ...float64) string {
	var items []string
	for i, sc := range scores {
		id := "m" + itoa(i+1)
		items = append(items, `{"id":"`+id+`","text":"memory `+id+` about billing","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":`+strconv.FormatFloat(sc, 'f', -1, 64)+`}}`)
	}
	return "[" + strings.Join(items, ",") + "]"
}

func prompt(t *testing.T, home, sid, text string) hookOut {
	t.Helper()
	return fire(t, "user-prompt-submit", map[string]any{"session_id": sid, "prompt": text, "cwd": filepath.Join(home, "billing")})
}

func telemetryText(t *testing.T, home string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(pathsOf(home).TelemetryDir(), "*.jsonl"))
	var all string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all += string(b)
	}
	return all
}

func TestFirstPromptRecallsAndReminds(t *testing.T) {
	home := hookHome(t)
	recallAnswer = `[{"id":"m1","text":"Billing keeps PostgreSQL 16 for partial indexes.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.8}},
		{"id":"m2","text":"Billing moved to Postgres 16 in October.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.7}},
		{"id":"m3","text":"` + strings.Repeat("long text ", 60) + `","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.6}},
		{"id":"m4","text":"a fourth","type":"observation","tags":["project:billing"],"scores":{"reranker":0.55}},
		{"id":"weak","text":"weak","type":"observation","tags":["project:billing"],"scores":{"reranker":0.46}}]`
	h := prompt(t, home, "s1", longPrompt+"\n```go\nfunc secretCode() {}\n```")
	c := h.Specific.Context
	if h.Specific.Event != "UserPromptSubmit" || !strings.Contains(c, instructions.RecallHeader) || !strings.Contains(c, "PostgreSQL 16 for partial") ||
		strings.Contains(c, "a fourth") || strings.Contains(c, "weak") || !strings.Contains(c, instructions.Reminder) {
		t.Fatalf("context:\n%s", c)
	}
	block, _, _ := strings.Cut(c, instructions.Reminder)
	block = strings.TrimSuffix(strings.TrimSpace(block), instructions.CreditAsk)
	if n := len([]rune(block)); n > 800+len([]rune(instructions.RecallHeader))+10 {
		t.Errorf("recall block %d chars", n)
	}
	for _, q := range recallQueries {
		if strings.Contains(q, "secretCode") || len([]rune(q)) > 300 {
			t.Errorf("query keeps code or is too long: %q", q)
		}
	}
	st, _ := sessions(home).Load("s1")
	if st.Prompts != 1 || st.LastInject != 1 || len(st.Injected) == 0 || st.Injected[0].Rule != "first" || st.CharsInjected == 0 {
		t.Errorf("state %+v", st)
	}
	if tel := telemetryText(t, home); !strings.Contains(tel, `"event":"prompt"`) || !strings.Contains(tel, `"decision":"recall"`) {
		t.Errorf("telemetry:\n%s", tel)
	}
}

func TestMachineMessagesAreSkipped(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	for _, msg := range []string{"<task-notification><task-id>x</task-id></task-notification>", "<system-reminder>auto</system-reminder> hello there",
		"Another Claude session sent a message: hi", "<local-command-stdout>ok</local-command-stdout>"} {
		if h := prompt(t, home, "s1", msg); h.Specific.Context != "" {
			t.Errorf("%q got %q", msg, h.Specific.Context)
		}
	}
	if st, _ := sessions(home).Load("s1"); st.Prompts != 0 {
		t.Errorf("machine messages counted as prompts: %d", st.Prompts)
	}
	if !strings.Contains(telemetryText(t, home), `"reason":"machine"`) {
		t.Error("no machine skip in telemetry")
	}
}

func TestRecallRulesGapTopicShiftSeenAndShortPrompts(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9, 0.85)
	prompt(t, home, "s1", longPrompt) // first: injects m1, m2
	recallAnswer = hits(0.9, 0.85, 0.8)
	if h := prompt(t, home, "s1", longPrompt+" Please also check the billing PostgreSQL indexes."); strings.Contains(h.Specific.Context, instructions.RecallHeader) {
		t.Errorf("same topic within the gap recalled:\n%s", h.Specific.Context)
	}
	if h := prompt(t, home, "s1", "ok"); strings.Contains(h.Specific.Context, instructions.RecallHeader) {
		t.Error("a short prompt recalled")
	}
	h := prompt(t, home, "s1", longPrompt+" One more time, the PostgreSQL version of the billing service?") // 4th: gap reached
	if !strings.Contains(h.Specific.Context, "memory m3") || strings.Contains(h.Specific.Context, "memory m1 ") {
		t.Errorf("gap recall must skip memories already seen:\n%s", h.Specific.Context)
	}
	recallAnswer = `[{"id":"k1","text":"Kubernetes ingress uses Traefik with Let's Encrypt.","type":"observation","tags":["project:billing"],"scores":{"reranker":0.9}}]`
	h = prompt(t, home, "s1", "Completely different subject: how is the kubernetes ingress controller configured with traefik certificates?")
	if !strings.Contains(h.Specific.Context, "Traefik") {
		t.Errorf("a topic shift must recall:\n%s", h.Specific.Context)
	}
	st, _ := sessions(home).Load("s1")
	if st.Injected[len(st.Injected)-1].Rule != "topic_shift" {
		t.Errorf("rule %q", st.Injected[len(st.Injected)-1].Rule)
	}
}

func TestStickyMemoriesAreMutedAcrossSessions(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	for _, sid := range []string{"a", "b", "c"} {
		if h := prompt(t, home, sid, longPrompt); !strings.Contains(h.Specific.Context, "memory m1") {
			t.Fatalf("session %s: not injected", sid)
		}
	}
	if h := prompt(t, home, "d", longPrompt); strings.Contains(h.Specific.Context, "memory m1") {
		t.Error("a 4th injection within 24 h was not muted")
	}
	recallAnswer = hits(0.98)
	if h := prompt(t, home, "e", longPrompt); !strings.Contains(h.Specific.Context, "memory m1") {
		t.Error("a very strong match must bypass the mute")
	}
}

func TestContextBudgetStopsInjections(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	_ = sessions(home).Update("s1", func(st *session.State) error { st.CharsInjected = 6000; st.Prompts = 5; st.LastInject = 1; return nil })
	if h := prompt(t, home, "s1", longPrompt); strings.Contains(h.Specific.Context, instructions.RecallHeader) {
		t.Error("injected past the session budget")
	}
	if !strings.Contains(telemetryText(t, home), `"reason":"budget"`) {
		t.Error("budget skip not logged")
	}
}

func TestNudgeNamesTermsThePromptMentions(t *testing.T) {
	home := hookHome(t)
	idx := recall.Index{BuiltAt: time.Now(), Names: map[string][]recall.ProjectCount{
		"PostgreSQL": {{Project: "billing", Count: 4}}, "pgbouncer": {{Project: "infra", Count: 2}}, "Grafana": {{Project: "garage", Count: 1}}}}
	_ = idx.Save(pathsOf(home).EntitiesFile())
	recallAnswer = "[]"
	_ = sessions(home).Update("s1", func(st *session.State) error { st.Prompts, st.LastInject = 3, 1; return nil })
	quiet := "Tune the PostgreSQL pool behind pgbouncer for the nightly batch jobs please" // two known names, >= 40 chars
	h := prompt(t, home, "s1", quiet)                                                      // prompt 4: three quiet prompts before it
	if !strings.Contains(h.Specific.Context, "No memory search for a few prompts") || !strings.Contains(h.Specific.Context, "PostgreSQL") || !strings.Contains(h.Specific.Context, "pgbouncer") {
		t.Fatalf("no nudge:\n%s", h.Specific.Context)
	}
	if h := prompt(t, home, "s1", quiet); strings.Contains(h.Specific.Context, "No memory search") {
		t.Error("nudged again right after a nudge")
	}
	_ = sessions(home).Update("s2", func(st *session.State) error { st.Prompts, st.LastInject = 3, 1; return nil })
	if h := prompt(t, home, "s2", "Only PostgreSQL is mentioned in this long enough prompt here"); strings.Contains(h.Specific.Context, "No memory search") {
		t.Error("nudged with a single known name")
	}
	_ = sessions(home).Update("s3", func(st *session.State) error { st.Prompts, st.LastInject = 3, 1; return nil })
	if h := prompt(t, home, "s3", "PostgreSQL pgbouncer?"); strings.Contains(h.Specific.Context, "No memory search") {
		t.Error("nudged on a short prompt")
	}
	_ = sessions(home).Update("s4", func(st *session.State) error { st.Prompts, st.LastInject, st.SearchedPromptID = 3, 1, 2; return nil })
	if h := prompt(t, home, "s4", quiet); strings.Contains(h.Specific.Context, "No memory search") {
		t.Error("nudged one prompt after a search")
	}
}

func TestPostToolUseMarksTheSearchAndSeenIDs(t *testing.T) {
	home := hookHome(t)
	recallAnswer = "[]"
	prompt(t, home, "s1", longPrompt)
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "tool_name": "mcp__plugin_backsight_memory__memory_search",
		"tool_response": []map[string]string{{"type": "text", "text": "[billing]\n- PostgreSQL 16 (2026-10-20, 4d ago, 18ffc1b1-7481-42b0-be72-f487a392d647, 0.80)"}}})
	st, _ := sessions(home).Load("s1")
	if st.SearchedPromptID != 1 || len(st.Seen) != 1 || st.Seen[0] != "18ffc1b1-7481-42b0-be72-f487a392d647" {
		t.Errorf("%+v", st)
	}
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "tool_name": "mcp__plugin_backsight_memory__memory_store",
		"tool_response": "stored (decision, project billing) note:3d9d9f67-288e-5799-80be-02f3335bb88c"})
	if st, _ := sessions(home).Load("s1"); len(st.Stored) != 1 || st.Stored[0] != "note:3d9d9f67-288e-5799-80be-02f3335bb88c" {
		t.Errorf("stored %v", st.Stored)
	}
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "tool_name": "Bash", "tool_response": "x"})
	if st, _ := sessions(home).Load("s1"); st.SearchedPromptID != 1 {
		t.Error("another tool changed the search flag")
	}
}

func TestDetectorLines(t *testing.T) {
	home := hookHome(t)
	recallAnswer = "[]"
	if h := prompt(t, home, "s1", "From now on we always use tabs in Makefiles, never spaces."); !strings.Contains(h.Specific.Context, instructions.DetectorRule) {
		t.Errorf("rule:\n%s", h.Specific.Context)
	}
	if h := prompt(t, home, "s1", "à partir de maintenant, on garde PostgreSQL 16"); !strings.Contains(h.Specific.Context, instructions.DetectorRule) {
		t.Errorf("French rule:\n%s", h.Specific.Context)
	}
	if h := prompt(t, home, "s1", "ok go"); strings.Contains(h.Specific.Context, instructions.DetectorApproval) {
		t.Error("approval without a question or subagent result before it")
	}
	_ = sessions(home).Update("s1", func(st *session.State) error { st.LastAssistant = "Shall I merge the PR?"; return nil })
	if h := prompt(t, home, "s1", "ok go"); !strings.Contains(h.Specific.Context, instructions.DetectorApproval) {
		t.Errorf("approval after a question:\n%s", h.Specific.Context)
	}
	if !strings.Contains(telemetryText(t, home), `"detector":"approval"`) {
		t.Error("detector not logged")
	}
}

func TestUnavailableMemoryIsShownOncePerSession(t *testing.T) {
	home := hookHome(t)
	recallStatus.Store(500)
	h := prompt(t, home, "s1", longPrompt)
	if !strings.Contains(h.SystemMessage, "memory unavailable") || h.Specific.Context == "" && h.SystemMessage == "" {
		t.Fatalf("%+v", h)
	}
	if h := prompt(t, home, "s1", longPrompt+" again, a different long question about deployment pipelines"); h.SystemMessage != "" {
		t.Errorf("shown twice: %q", h.SystemMessage)
	}
}

func TestOutcomeOfThePreviousInjection(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	prompt(t, home, "s1", longPrompt)
	_ = sessions(home).Update("s1", func(st *session.State) error {
		st.LastAssistant = "> 🧠 **From memory (billing)** — PostgreSQL 16."
		return nil
	})
	prompt(t, home, "s1", "no, actually we never use PostgreSQL 15 anywhere")
	tel := telemetryText(t, home)
	if !strings.Contains(tel, `"event":"outcome"`) || !strings.Contains(tel, `"credited":"exact"`) || !strings.Contains(tel, `"contradicted":true`) {
		t.Errorf("telemetry:\n%s", tel)
	}
}

// v7: the prompt-recall block ends with the ask to declare the memories
// used, on the first prompt (before the reminder) as on later ones.
func TestRecallHeaderEndsWithTheCreditLine(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	c := prompt(t, home, "s1", longPrompt).Specific.Context
	block, _, _ := strings.Cut(c, instructions.Reminder)
	if !strings.HasPrefix(block, instructions.RecallHeader+"\n- [billing · m1] memory m1 about billing") || !strings.HasSuffix(strings.TrimSpace(block), "\n"+instructions.CreditAsk) {
		t.Errorf("first prompt's block:\n%s", c)
	}
	recallAnswer = hits(0, 0.9)
	_ = sessions(home).Update("s1", func(st *session.State) error { st.LastInject = -10; return nil }) // the gap has passed
	c = prompt(t, home, "s1", "Which deployment pipeline does the billing service use for its nightly releases?").Specific.Context
	if !strings.HasPrefix(c, instructions.RecallHeader+"\n- [billing · m2] memory m2") || !strings.HasSuffix(c, "\n"+instructions.CreditAsk) {
		t.Errorf("later block:\n%s", c)
	}
}

// feedback.enabled off: memory_credit is not offered, and no text the
// agent reads names it (session guide, MCP instructions, reminder, recall
// block, search result); on, each carries the v7 ask.
func TestCreditAskFollowsFeedback(t *testing.T) {
	for _, feedback := range []bool{true, false} {
		home := hookHome(t)
		extra := ""
		if !feedback {
			extra = "\n[feedback]\nenabled = false\n"
			appendConfig(t, home, extra)
		}
		want := instructions.For(feedback)
		recallAnswer = hits(0.9)
		guide := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")}).Specific.Context
		first := prompt(t, home, "s1", longPrompt).Specific.Context
		cs := mcpSessionWith(t, extra) // a home of its own, after the hooks
		recallAnswer = hits(0.9)
		search, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres version"}})
		texts := map[string]string{"guide": guide, "first prompt": first, "mcp instructions": cs.InitializeResult().Instructions, "search": search}
		for name, text := range texts {
			if strings.Contains(text, "memory_credit") != feedback {
				t.Errorf("feedback %v, %s:\n%s", feedback, name, text)
			}
		}
		block, _, _ := strings.Cut(first, want.Reminder)
		if !strings.HasPrefix(guide, want.Guide) || !strings.Contains(first, want.Reminder) || !strings.HasSuffix(strings.TrimSpace(block), "\n"+want.RecallEnd) ||
			cs.InitializeResult().Instructions != want.ShortGuide || !strings.HasSuffix(search, "\n"+want.SearchEnd+"\n") {
			t.Errorf("feedback %v: guide %q\nfirst prompt %q\nsearch %q", feedback, guide, first, search)
		}
	}
}

// outcomes is the outcome events of session sid, in order.
func outcomes(t *testing.T, home, sid string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(telemetryText(t, home), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(l), &e) == nil && e["event"] == "outcome" && e["session"] == sid {
			out = append(out, e)
		}
	}
	return out
}

// An outcome says where its credit came from: a memory_credit call
// journaled in the judged turn (tool), else a citation found in the reply;
// a credit of another session or of a subagent counts for none.
func TestOutcomeCreditSource(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	credit := func(sid, agent string) {
		in := map[string]any{"session_id": sid, "tool_name": "mcp__plugin_backsight_memory__memory_credit",
			"tool_input": map[string]any{"verdict": "none"}, "tool_response": []map[string]string{{"type": "text", "text": "Noted: no memory used."}}}
		if agent != "" {
			in["agent_id"] = agent
		}
		fire(t, "post-tool-use", in)
	}
	cite := func(sid string) {
		_ = sessions(home).Update(sid, func(st *session.State) error {
			st.LastAssistant = "> 🧠 **From memory (billing)** — PostgreSQL 16."
			return nil
		})
	}
	next := "And which replica does the billing service read from at night?"

	prompt(t, home, "tool", longPrompt)
	credit("tool", "")
	cite("tool") // the call wins over the citation
	prompt(t, home, "tool", next)

	prompt(t, home, "cited", longPrompt)
	credit("other", "")   // another session's call, journaled during this turn
	credit("cited", "a1") // a subagent's call: its own shows, not the injection
	cite("cited")
	prompt(t, home, "cited", next)

	prompt(t, home, "silent", longPrompt)
	prompt(t, home, "silent", next)

	// A message Claude Code sent between the credit and the next prompt
	// keeps the turn's journal position: the credit is still seen.
	prompt(t, home, "notified", longPrompt)
	credit("notified", "")
	prompt(t, home, "notified", "<task-notification><task-id>x</task-id></task-notification>")
	prompt(t, home, "notified", next)

	for sid, want := range map[string]any{"tool": "tool", "cited": "citation", "silent": nil, "notified": "tool"} {
		o := outcomes(t, home, sid)
		if len(o) != 1 || o[0]["credit_source"] != want {
			t.Errorf("%s: outcomes %v, want credit_source %v", sid, o, want)
		}
	}
}

// recall-prompt (E23) shows exactly what the hook would inject for a first
// prompt, without session state.
func TestRecallPromptMatchesTheHookSelection(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9, 0.85, 0.8, 0.75)
	code, out, _ := run(t, "recall-prompt", "--cwd", filepath.Join(home, "billing"), "--json", longPrompt)
	var env struct {
		Data recallPromptResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != 0 || env.Data.Project != "billing" || len(env.Data.Ranked) != 4 || strings.Join(env.Data.Injected, ",") != "m1,m2,m3" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if ents, _ := os.ReadDir(pathsOf(home).SessionsDir()); len(ents) != 0 {
		t.Errorf("recall-prompt wrote session state: %v", ents)
	}
	h := prompt(t, home, "s1", longPrompt)
	if !strings.Contains(h.Specific.Context, env.Data.Block) {
		t.Errorf("the hook injected something else:\nhook:\n%s\nrecall-prompt:\n%s", h.Specific.Context, env.Data.Block)
	}
}

// Stop records how the reply used memory; PostToolUse ties a memory tool
// call to its prompt and the ids it returned (stats: search rate, detector
// follow-up, briefing use).
func TestReplyAndToolUseEventsCarryTheTurn(t *testing.T) {
	home := hookHome(t)
	recallAnswer = "[]"
	prompt(t, home, "s1", longPrompt)
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "tool_name": "mcp__plugin_backsight_memory__memory_search",
		"tool_response": "[billing] fact (2d ago, 0b4c2a52-0000-4000-8000-000000000001)"})
	fire(t, "stop", map[string]any{"session_id": "s1", "last_assistant_message": "> 🧠 **From memory (billing)** — PostgreSQL 16."})
	tel := telemetryText(t, home)
	if !strings.Contains(tel, `"event":"tool_use"`) || !strings.Contains(tel, `"memory_ids":["0b4c2a52-0000-4000-8000-000000000001"]`) || !strings.Contains(tel, `"n":1`) {
		t.Errorf("tool_use:\n%s", tel)
	}
	if !strings.Contains(tel, `"credit":"exact","event":"reply","n":1`) || !strings.Contains(tel, `"searched":true`) {
		t.Errorf("reply:\n%s", tel)
	}
}

// The prompt event records the prompt's language (decision 0058).
func TestPromptEventsCarryTheLanguage(t *testing.T) {
	home := hookHome(t)
	recallAnswer = "[]"
	prompt(t, home, "s1", "Quelle version de PostgreSQL est-ce que le service de facturation utilise, et pourquoi on l'a choisie ?")
	prompt(t, home, "s2", longPrompt)
	if tel := telemetryText(t, home); !strings.Contains(tel, `"lang":"fr"`) || !strings.Contains(tel, `"lang":"en"`) {
		t.Errorf("telemetry:\n%s", tel)
	}
}

func TestPromptLang(t *testing.T) {
	for text, want := range map[string]string{
		"Peux-tu vérifier que le backup tourne bien sur la machine ?": "fr",
		"Can you check that the backup runs on the machine?":          "en",
		"kubectl get pods -n hindsight":                               "en",
		"Look on GitHub":                                              "en",
		"Vérifie si l'install marche et si c'est bon":                 "fr",
		"On a un souci de build":                                      "fr",
	} {
		if got := promptLang(text); got != want {
			t.Errorf("%q: %s, want %s", text, got, want)
		}
	}
}

// The credit is told apart by its 🧠: a plain "from memory" is a free use,
// and the v3 wording still counts as exact.
func TestReplyKindTellsTheCreditApart(t *testing.T) {
	for text, want := range map[string]string{
		"> 🧠 **From memory (billing)** — PostgreSQL 16.":           "exact",
		"🧠 From memory (billing) — PostgreSQL 16.":                 "exact",
		"> 🧠 **From Hindsight memory (billing)** — PostgreSQL 16.": "exact",
		"From memory, billing runs PostgreSQL 16.":                 "free",
		"Nothing in memory about it.":                              "empty",
	} {
		if got := replyKind(text); got == nil || *got != want {
			t.Errorf("%q: got %s, want %s", text, kindOf(got), want)
		}
	}
	if got := replyKind("PostgreSQL 16, as the docs say."); got != nil {
		t.Errorf("no memory use: got %q", *got)
	}
}

func kindOf(k *string) string {
	if k == nil {
		return "none"
	}
	return *k
}

// journalEvents is everything the feedback journal holds.
func journalEvents(t *testing.T, home string) []feedback.Event {
	t.Helper()
	evs, _, err := feedback.Read(pathsOf(home).FeedbackDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func eventsOf(evs []feedback.Event, kind string) []feedback.Event {
	var out []feedback.Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func itemProjects(items []feedback.Item) string {
	var parts []string
	for _, it := range items {
		parts = append(parts, it.Project)
	}
	return strings.Join(parts, ",")
}

func itemsText(items []feedback.Item) string {
	var parts []string
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s:%d:%g", it.ID, it.Rank, it.Reranker))
	}
	return strings.Join(parts, ",")
}

// The prompt hook journals the turn, then what it injected, ranked in the
// order shown: a memory skipped as already seen takes no rank.
func TestPromptRecallJournalsShowsWithRanks(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9, 0.8, 0.7)
	_ = sessions(home).Update("s1", func(st *session.State) error { st.Seen = []string{"m2"}; return nil })
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "prompt_id": "p-1", "prompt": longPrompt, "cwd": filepath.Join(home, "billing")})
	evs := journalEvents(t, home)
	if len(evs) != 2 || evs[0].Kind != "turn" || evs[1].Kind != "shows" {
		t.Fatalf("events %+v", evs)
	}
	if turn := evs[0]; turn.Turn != "p-1" || turn.N != 1 || turn.Machine || turn.Session != "s1" {
		t.Errorf("turn %+v", turn)
	}
	shows := evs[1]
	if shows.Path != "prompt" || shows.Turn != "p-1" || shows.Session != "s1" || shows.Agent != "" || itemsText(shows.Items) != "m1:1:0.9,m3:2:0.7" || itemProjects(shows.Items) != "billing,billing" {
		t.Errorf("shows %+v items %s", shows, itemsText(shows.Items))
	}
	if cur, ok := feedback.CurrentTurn(pathsOf(home).FeedbackDir(), "s1"); !ok || cur.ID != "p-1" || cur.N != 1 || cur.Machine {
		t.Errorf("pointer %+v %v", cur, ok)
	}
}

func TestInjectedLinesCarryShortIDs(t *testing.T) {
	home := hookHome(t)
	recallAnswer = `[{"id":"0b4c2a52-0000-4000-8000-000000000001","text":"Billing keeps PostgreSQL 16.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.8}}]`
	h := prompt(t, home, "s1", longPrompt)
	if !strings.Contains(h.Specific.Context, "\n- [billing · 0b4c2a52] Billing keeps PostgreSQL 16. (") {
		t.Errorf("context:\n%s", h.Specific.Context)
	}
	if shortID("m1") != "m1" || shortID("0b4c2a52-0000") != "0b4c2a52" {
		t.Errorf("shortID: %q %q", shortID("m1"), shortID("0b4c2a52-0000"))
	}
}

// A message Claude Code sent is no prompt, but it starts a turn: its
// hooks journal under a fresh id, with the prompt number kept.
func TestMachineMessageStillMovesTheTurn(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "prompt_id": "p-1", "prompt": longPrompt, "cwd": filepath.Join(home, "billing")})
	before := len(eventsOf(journalEvents(t, home), "shows"))
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "prompt": "<task-notification><task-id>x</task-id></task-notification>", "cwd": filepath.Join(home, "billing")})
	cur, ok := feedback.CurrentTurn(pathsOf(home).FeedbackDir(), "s1")
	if !ok || !cur.Machine || cur.N != 1 || cur.ID == "" || cur.ID == "p-1" {
		t.Errorf("pointer %+v %v", cur, ok)
	}
	evs := journalEvents(t, home)
	turns := eventsOf(evs, "turn")
	if len(turns) != 2 || !turns[1].Machine || turns[1].Turn != cur.ID || turns[1].N != 1 {
		t.Errorf("turns %+v", turns)
	}
	if after := len(eventsOf(evs, "shows")); after != before || before != 1 {
		t.Errorf("shows %d before, %d after the machine message", before, after)
	}
	if st, _ := sessions(home).Load("s1"); st.Prompts != 1 {
		t.Errorf("a machine message counted as a prompt: %d", st.Prompts)
	}
	// Its prompt_id, when Claude Code sends one, is the turn's id.
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "prompt_id": "p-2", "prompt": "<system-reminder>auto</system-reminder>", "cwd": filepath.Join(home, "billing")})
	if cur, _ := feedback.CurrentTurn(pathsOf(home).FeedbackDir(), "s1"); cur.ID != "p-2" || cur.N != 1 || !cur.Machine {
		t.Errorf("pointer %+v", cur)
	}
}

// memory_search and memory_read shows are journaled by PostToolUse, which
// knows the turn and the agent: search ranks come from the #n markers.
func TestPostToolUseJournalsSearchAndReadShows(t *testing.T) {
	home := hookHome(t)
	res := recall.Result{Hits: []recall.Hit{
		{ID: "0b4c2a52-0000-4000-8000-000000000001", Text: "trap, b, 0.9) not an id", Project: "billing", Age: "2d", Score: 0.9},
		{ID: "1c4c2a52-0000-4000-8000-000000000002", Text: "second", Project: ".claude", Age: "3d", Score: 0.8},
		{ID: "2d4c2a52-0000-4000-8000-000000000003", Text: "third", Project: "billing", Age: "4d", Score: 0.7},
	}}
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "prompt_id": "p-1", "agent_id": "a7", "tool_name": "mcp__plugin_backsight_memory__memory_search",
		"tool_response": []map[string]string{{"type": "text", "text": mcpSearchText(res, true)}}})
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "prompt_id": "p-1", "tool_name": "mcp__plugin_backsight_memory__memory_read",
		"tool_input": map[string]string{"id": "1c4c2a52-0000-4000-8000-000000000002"}, "tool_response": "second"})
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 2 {
		t.Fatalf("shows %+v", shows)
	}
	if s := shows[0]; s.Path != "search" || s.Turn != "p-1" || s.Agent != "a7" ||
		itemsText(s.Items) != "0b4c2a52-0000-4000-8000-000000000001:1:0,1c4c2a52-0000-4000-8000-000000000002:2:0,2d4c2a52-0000-4000-8000-000000000003:3:0" {
		t.Errorf("search %+v items %s", s, itemsText(s.Items))
	}
	if s := shows[1]; s.Path != "read" || s.Turn != "p-1" || s.Agent != "" || itemsText(s.Items) != "1c4c2a52-0000-4000-8000-000000000002:0:0" {
		t.Errorf("read %+v items %s", s, itemsText(s.Items))
	}
	// each search item carries the project of the group its line sits in
	if p := itemProjects(shows[0].Items); p != "billing,.claude,billing" {
		t.Errorf("search item projects %q", p)
	}
	// A search that found nothing shows nothing.
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "tool_name": "mcp__plugin_backsight_memory__memory_search", "tool_response": "nothing in memory\n"})
	if n := len(eventsOf(journalEvents(t, home), "shows")); n != 2 {
		t.Errorf("an empty search journaled a show: %d", n)
	}
}

// A memory_credit the server accepted is journaled by PostToolUse, which
// knows the turn and the agent: prefixes become the full ids shown.
func TestPostToolUseJournalsTheCredit(t *testing.T) {
	home := hookHome(t)
	full := "0b4c2a52-0000-4000-8000-000000000001"
	res := recall.Result{Hits: []recall.Hit{{ID: full, Text: "first", Project: "billing", Age: "2d", Score: 0.9}}}
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "prompt_id": "p-1", "agent_id": "a7", "tool_name": "mcp__plugin_backsight_memory__memory_search",
		"tool_response": []map[string]string{{"type": "text", "text": mcpSearchText(res, true)}}})
	credit := func(prompt string, input map[string]any, response string) {
		fire(t, "post-tool-use", map[string]any{"session_id": "s1", "prompt_id": prompt, "agent_id": "a7", "tool_name": "mcp__plugin_backsight_memory__memory_credit",
			"tool_input": input, "tool_response": []map[string]string{{"type": "text", "text": response}}})
	}
	credit("p-2", map[string]any{"ids": []string{"0b4c2a52"}}, "Credited. Print this line in your final message:\n> 🧠 **From memory (billing · 0b4c2a52)** — <the facts you used, in your words>")
	credit("p-2", map[string]any{"verdict": "none"}, "Noted: no memory used.")
	// a refused call is no credit, whatever it was asked
	credit("p-2", map[string]any{"ids": []string{"cccccccc"}}, "Credited. Print this line in your final message:\n> 🧠 **From memory (billing · cccccccc)** — x") // used, but no id resolves: no empty credit
	credit("p-2", map[string]any{"ids": []string{"0b4c2a52"}}, "0b4c2a52: not shown in this session, or its search result has not arrived yet: call again after the result")
	credits := eventsOf(journalEvents(t, home), "credit")
	if len(credits) != 2 {
		t.Fatalf("credits %+v", credits)
	}
	if c := credits[0]; c.Verdict != "used" || strings.Join(c.IDs, ",") != full || c.Turn != "p-2" || c.Agent != "a7" || c.Session != "s1" {
		t.Errorf("used %+v", c)
	}
	if c := credits[1]; c.Verdict != "none" || len(c.IDs) != 0 || c.Turn != "p-2" {
		t.Errorf("none %+v", c)
	}
	// feedback off: nothing journaled
	appendConfig(t, home, "[feedback]\nenabled = false")
	credit("p-3", map[string]any{"verdict": "none"}, "Noted: no memory used.")
	if n := len(eventsOf(journalEvents(t, home), "credit")); n != 2 {
		t.Errorf("feedback off journaled a credit: %d", n)
	}
}

// A wrong or outdated verdict the server carried out is a credit of the
// turn, with its verdict and the full ids; a Sources answer or a refusal
// is none.
func TestVerdictCreditIsJournaled(t *testing.T) {
	home := hookHome(t)
	full := "0b4c2a52-0000-4000-8000-000000000001"
	res := recall.Result{Hits: []recall.Hit{{ID: full, Text: "first", Project: "billing", Age: "2d", Score: 0.9}}}
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "prompt_id": "p-1", "tool_name": "mcp__plugin_backsight_memory__memory_search",
		"tool_response": []map[string]string{{"type": "text", "text": mcpSearchText(res, true)}}})
	credit := func(verdict, response string) {
		fire(t, "post-tool-use", map[string]any{"session_id": "s1", "prompt_id": "p-2", "tool_name": "mcp__plugin_backsight_memory__memory_credit",
			"tool_input":    map[string]any{"ids": []string{"0b4c2a52"}, "verdict": verdict, "correction": "Billing runs PostgreSQL 16.", "evidence": "psql printed 16.4"},
			"tool_response": []map[string]string{{"type": "text", "text": response}}})
	}
	tail := "\nTell the user which memory you retired, why, and that `backsight forget --restore <id>` undoes it."
	credit("outdated", "Sources: these observations are drawn from the facts below; only facts are retired.\nCall memory_credit again with the verdict, naming the source facts that are wrong (or all of them).")
	credit("wrong", "retirements: at most 3 per session (feedback.max_retirements), 3 already: nothing stored")
	if n := len(eventsOf(journalEvents(t, home), "credit")); n != 0 {
		t.Fatalf("no credit expected yet: %d", n)
	}
	credit("outdated", "Retired. Correction stored: note:11111111-0000-4000-8000-000000000001.\nInvalidated on the server: "+full+tail)
	credit("wrong", "Partly retired. The server refused a retirement: x. Correction stored: note:11111111-0000-4000-8000-000000000002."+tail)
	credit("wrong", "Hidden. Correction stored: note:11111111-0000-4000-8000-000000000003."+tail)
	credits := eventsOf(journalEvents(t, home), "credit")
	if len(credits) != 3 {
		t.Fatalf("credits %+v", credits)
	}
	for i, v := range []string{"outdated", "wrong", "wrong"} {
		if c := credits[i]; c.Verdict != v || strings.Join(c.IDs, ",") != full || c.Turn != "p-2" || c.Session != "s1" {
			t.Errorf("credit %d: %+v", i, c)
		}
	}
	if strings.Contains(fmt.Sprint(journalEvents(t, home)), "psql") {
		t.Error("the evidence reached the journal")
	}
}

// The hook journals the facts of a Sources answer line by line: a line
// whose short id does not start its full id, or that is not a whole fact
// line, is skipped; the others are journaled, in order.
func TestSourcesShowsSkipBadLines(t *testing.T) {
	home := hookHome(t)
	const (
		one = "61616161-0000-4000-8000-000000000061"
		two = "62626262-0000-4000-8000-000000000062"
		odd = "63636363-0000-4000-8000-000000000063"
	)
	answer := "Sources: only raw facts are retired, and an observation is drawn from them; nothing was retired or stored.\n" +
		"0f0f0f0f is drawn from:\n" +
		"- [billing · 61616161] Billing ran PostgreSQL 14. (" + one + ")\n" +
		"- [billing · 99999999] its short id is another's. (" + odd + ")\n" +
		"- [billing · 63636363] no full id at the end\n" +
		"- [atlas-spec · 62626262] The replicas run 14. (" + two + ")\n" +
		"Call memory_credit again with the verdict, naming the source facts that are wrong (or all of them)."
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "prompt_id": "p-2", "tool_name": "mcp__plugin_backsight_memory__memory_credit",
		"tool_input":    map[string]any{"ids": []string{"0f0f0f0f"}, "verdict": "outdated", "correction": "Billing runs PostgreSQL 16.", "evidence": "psql printed 16.4"},
		"tool_response": []map[string]string{{"type": "text", "text": answer}}})
	shows := eventsOf(journalEvents(t, home), "shows")
	want := []feedback.Item{{ID: one, Project: "billing"}, {ID: two, Project: "atlas-spec"}}
	if len(shows) != 1 || shows[0].Path != "sources" || !slices.Equal(shows[0].Items, want) {
		t.Errorf("shows %+v", shows)
	}
	if n := len(eventsOf(journalEvents(t, home), "credit")); n != 0 {
		t.Errorf("a Sources answer journaled %d credits", n)
	}
}

// With feedback off nothing reaches the journal or the turn pointers; the
// lines the agent reads keep their short ids.
func TestFeedbackOffJournalsNothing(t *testing.T) {
	home := hookHome(t)
	appendConfig(t, home, "[feedback]\nenabled = false")
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	recallAnswer = `[{"id":"0b4c2a52-0000-4000-8000-000000000001","text":"Billing keeps PostgreSQL 16.","type":"observation","tags":["project:billing"],"scores":{"reranker":0.8}}]`
	fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	fire(t, "subagent-start", map[string]any{"session_id": "s1", "agent_id": "a1", "agent_type": "general-purpose", "cwd": filepath.Join(home, "billing")})
	h := fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "prompt_id": "p-1", "prompt": longPrompt, "cwd": filepath.Join(home, "billing")})
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "prompt": "<task-notification>x</task-notification>", "cwd": filepath.Join(home, "billing")})
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "tool_name": "mcp__plugin_backsight_memory__memory_read", "tool_input": map[string]string{"id": "0b4c2a52-0000-4000-8000-000000000001"}})
	if !strings.Contains(h.Specific.Context, "[billing · 0b4c2a52] Billing keeps PostgreSQL 16.") {
		t.Errorf("context:\n%s", h.Specific.Context)
	}
	if _, err := os.Stat(pathsOf(home).FeedbackDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("feedback dir written with feedback off: %v", err)
	}
}

// The prompt recall follows the ledger, and its shows say how: m2, used
// often, is injected over m1, 0.02 above it, each item with its boost,
// its score and its rank without it (the score with the current-project
// boost, for feedback replay to order the hits again).
func TestPromptShowsJournalTheBoost(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.82, 0.80)
	at := time.Now().UTC().Format(time.RFC3339Nano)
	writeLedger(t, home, `{"schema":1,"watermark":"`+at+`","memories":{"m2":{"shown":10,"credits":10,"credits_at":"`+at+`"}}}`)
	h := prompt(t, home, "s1", longPrompt)
	if i, j := strings.Index(h.Specific.Context, "memory m2"), strings.Index(h.Specific.Context, "memory m1"); i < 0 || j < 0 || i > j {
		t.Fatalf("m2 not injected first:\n%s", h.Specific.Context)
	}
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 1 || len(shows[0].Items) != 2 {
		t.Fatalf("shows %+v", shows)
	}
	m2, m1 := shows[0].Items[0], shows[0].Items[1]
	if m2.ID != "m2" || m2.Rank != 1 || m2.RankWithoutBoost != 2 || m2.Boost < 0.034 || m2.Boost > 0.035 || m2.Reranker != 0.8 || m2.ScoreWithoutBoost == nil || *m2.ScoreWithoutBoost != 0.85 {
		t.Errorf("m2 %+v", m2)
	}
	if m1.ID != "m1" || m1.Rank != 2 || m1.RankWithoutBoost != 1 || m1.Boost != 0 || m1.ScoreWithoutBoost == nil || *m1.ScoreWithoutBoost != 0.87 {
		t.Errorf("m1 %+v", m1)
	}
}

// A memory hidden since the last settle is skipped by the next recall:
// the journal's hide counts before the tick folds it.
func TestAFreshHidingSkipsTheNextRecall(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.82, 0.80)
	writeLedger(t, home, `{"schema":1,"watermark":"2026-10-01T00:00:00Z","memories":{}}`)
	j := feedback.NewJournal(pathsOf(home).FeedbackDir(), feedback.Common{Session: "s0"})
	if err := j.AppendFolded(feedback.Event{Kind: feedback.KindHide, IDs: []string{"m1"}, Verdict: "wrong"}); err != nil {
		t.Fatal(err)
	}
	h := prompt(t, home, "s1", longPrompt)
	if strings.Contains(h.Specific.Context, "memory m1") || !strings.Contains(h.Specific.Context, "memory m2") {
		t.Errorf("context:\n%s", h.Specific.Context)
	}
}

// Two memories with ids a shared document can name: uA 0.02 above uB.
const (
	uA = "a1111111-0000-4000-8000-000000000001"
	uB = "b2222222-0000-4000-8000-000000000002"
)

func uuidHits() string {
	return `[{"id":"` + uA + `","text":"memory A about billing","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.82}},` +
		`{"id":"` + uB + `","text":"memory B about billing","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.80}}]`
}

// writeSharedCache writes docs as the tick's shared.json, each through
// MarshalMeta as the other machines upload it, and combines them now as
// the tick does into shared-combined.json, which recall reads.
func writeSharedCache(t *testing.T, home string, docs ...feedback.SharedDoc) {
	t.Helper()
	c := feedback.SharedCache{FetchedAt: time.Now().UTC()}
	for _, d := range docs {
		meta, err := d.MarshalMeta()
		if err != nil {
			t.Fatal(err)
		}
		c.Docs = append(c.Docs, json.RawMessage(meta))
	}
	dir := pathsOf(home).FeedbackDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := feedback.SaveSharedCache(dir, c); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := combineShared(dir, c, "", time.Now(), feedbackParams(cfg)); err != nil {
		t.Fatal(err)
	}
}

// otherMachine is a document of another machine at now, with entries.
func otherMachine(machine string, entries map[string]feedback.SharedEntry) feedback.SharedDoc {
	if entries == nil {
		entries = map[string]feedback.SharedEntry{}
	}
	return feedback.SharedDoc{V: feedback.SharedVersion, Machine: machine, At: time.Now().Truncate(time.Second), Entries: entries}
}

// injectedFirst: A comes before B in the context (false when either is
// missing).
func injectedFirst(ctx, a, b string) bool {
	i, j := strings.Index(ctx, a), strings.Index(ctx, b)
	return i >= 0 && j >= 0 && i < j
}

// A memory this machine never credited, used often on another machine,
// climbs over its neighbour here, this machine's ledger empty; with
// share_ledger off the shared counts are not read.
func TestRecallUsesOtherMachinesCredits(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	credited := map[string]feedback.SharedEntry{uB: {Credits: 10, CreditsAt: now}}
	home := hookHome(t)
	recallAnswer = uuidHits()
	writeSharedCache(t, home, otherMachine("pc-b", credited))
	h := prompt(t, home, "s1", longPrompt)
	if !injectedFirst(h.Specific.Context, "memory B", "memory A") {
		t.Fatalf("B, credited on another machine, not injected first:\n%s", h.Specific.Context)
	}
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 1 || len(shows[0].Items) != 2 || shows[0].Items[0].ID != uB || shows[0].Items[0].Boost < 0.034 || shows[0].Items[0].RankWithoutBoost != 2 {
		t.Errorf("shows %+v", shows)
	}

	off := hookHome(t)
	recallAnswer = uuidHits()
	appendConfig(t, off, "[feedback]\nshare_ledger = false")
	writeSharedCache(t, off, otherMachine("pc-b", credited))
	if h := prompt(t, off, "s1", longPrompt); !injectedFirst(h.Specific.Context, "memory A", "memory B") {
		t.Errorf("share_ledger off still read the shared counts:\n%s", h.Specific.Context)
	}
}

// A memory another machine hid is skipped here, until this machine
// restores it after that hiding.
func TestRemoteHidingHidesHere(t *testing.T) {
	home := hookHome(t)
	recallAnswer = uuidHits()
	writeSharedCache(t, home, otherMachine("pc-b", map[string]feedback.SharedEntry{uA: {HiddenSince: time.Now().Add(-time.Hour).Truncate(time.Second)}}))
	h := prompt(t, home, "s1", longPrompt)
	if strings.Contains(h.Specific.Context, "memory A") || !strings.Contains(h.Specific.Context, "memory B") {
		t.Fatalf("hidden on another machine, still recalled here:\n%s", h.Specific.Context)
	}
	j := feedback.NewJournal(pathsOf(home).FeedbackDir(), feedback.Common{})
	if err := j.AppendFolded(feedback.Event{Kind: feedback.KindUnhide, IDs: []string{uA}, Outcome: "restored"}); err != nil {
		t.Fatal(err)
	}
	if h := prompt(t, home, "s2", longPrompt); !strings.Contains(h.Specific.Context, "memory A") {
		t.Errorf("restored here after the hiding, still skipped:\n%s", h.Specific.Context)
	}
}

// Shared counts recall cannot use leave it on this machine's ledger: the
// same context, byte for byte, and the same shows as with none at all.
// So do a combined view that is garbage, of an unknown version or 30
// days old, documents of an unknown version (the tick combines nothing
// of them), and documents never combined: recall reads the combined view
// alone, never a document.
func TestRecallIgnoresABrokenSharedCache(t *testing.T) {
	at := time.Now().UTC().Format(time.RFC3339Nano)
	local := `{"schema":1,"watermark":"` + at + `","memories":{"` + uA + `":{"shown":10,"credits":10,"credits_at":"` + at + `"}}}`
	run := func(cache func(home string)) (string, []feedback.Item) {
		t.Helper()
		home := hookHome(t)
		recallAnswer = uuidHits()
		writeLedger(t, home, local)
		cache(home)
		h := prompt(t, home, "s1", longPrompt)
		shows := eventsOf(journalEvents(t, home), "shows")
		if len(shows) != 1 {
			t.Fatalf("shows %+v", shows)
		}
		return h.Specific.Context, shows[0].Items
	}
	file := func(name func(string) string, content string) func(string) {
		return func(home string) {
			dir := pathsOf(home).FeedbackDir()
			if err := os.WriteFile(name(dir), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// What a usable view would do: credit B far above A, and hide A.
	now := time.Now().Unix()
	view := func(header string) string {
		return fmt.Sprintf("%s\n%s\t0\t0\t0\t0\t%d\n%s\t50\t%d\t0\t0\t0\n", header, uA, now, uB, now)
	}
	entries := fmt.Sprintf(`{"%s":[50,%d,0,0,0],"%s":[0,0,0,0,%d]}`, uB, now, uA, now)
	wantCtx, wantItems := run(func(string) {})
	if !injectedFirst(wantCtx, "memory A", "memory B") || wantItems[0].Boost <= 0 {
		t.Fatalf("baseline without the local boost:\n%s\n%+v", wantCtx, wantItems)
	}
	if ctx, _ := run(file(feedback.SharedCombinedFile, view(fmt.Sprintf("v1 %d", now)))); strings.Contains(ctx, "memory A") || !strings.Contains(ctx, "memory B") {
		t.Fatalf("the usable view is not used, the cases below prove nothing:\n%s", ctx)
	}
	for name, cache := range map[string]func(string){
		"garbage":         file(feedback.SharedCombinedFile, "{not json"),
		"unknown version": file(feedback.SharedCombinedFile, view(fmt.Sprintf("v2 %d", now))),
		"31 days old":     file(feedback.SharedCombinedFile, view(fmt.Sprintf("v1 %d", now-31*24*3600))),
		"documents of an unknown version": func(home string) {
			c := feedback.SharedCache{FetchedAt: time.Now(), Docs: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"v":2,"machine":"pc-b","at":%d,"entries":%s}`, now, entries))}}
			if err := feedback.SaveSharedCache(pathsOf(home).FeedbackDir(), c); err != nil {
				t.Fatal(err)
			}
			if err := combineShared(pathsOf(home).FeedbackDir(), c, "", time.Now(), feedback.Params{}); err != nil {
				t.Fatal(err)
			}
		},
		"documents never combined": func(home string) {
			writeSharedCache(t, home, otherMachine("pc-b", map[string]feedback.SharedEntry{uB: {Credits: 50, CreditsAt: time.Now().Truncate(time.Second)}}))
			if err := os.Remove(feedback.SharedCombinedFile(pathsOf(home).FeedbackDir())); err != nil {
				t.Fatal(err)
			}
		},
	} {
		ctx, items := run(cache)
		if ctx != wantCtx {
			t.Errorf("%s: context differs:\n%s\nwant:\n%s", name, ctx, wantCtx)
		}
		if !slices.EqualFunc(items, wantItems, func(a, b feedback.Item) bool {
			return a.ID == b.ID && a.Rank == b.Rank && a.Boost == b.Boost && a.RankWithoutBoost == b.RankWithoutBoost
		}) {
			t.Errorf("%s: shows %+v, want %+v", name, items, wantItems)
		}
	}
}

// Recall reads the combined view, never the documents nor the machine
// id: with shared.json unreadable it still weighs the other machines'
// counts, and it creates no machine id.
func TestRecallReadsOnlyTheCombinedCounts(t *testing.T) {
	home := hookHome(t)
	recallAnswer = uuidHits()
	writeSharedCache(t, home, otherMachine("pc-b", map[string]feedback.SharedEntry{uB: {Credits: 10, CreditsAt: time.Now().Truncate(time.Second)}}))
	dir := pathsOf(home).FeedbackDir()
	if err := os.WriteFile(feedback.SharedCacheFile(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(pathsOf(home).State, "machine_id"))
	if h := prompt(t, home, "s1", longPrompt); !injectedFirst(h.Specific.Context, "memory B", "memory A") {
		t.Errorf("the combined counts not used:\n%s", h.Specific.Context)
	}
	if _, err := os.Stat(filepath.Join(pathsOf(home).State, "machine_id")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("recall created the machine id: %v", err)
	}
}
