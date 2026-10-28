package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

	for sid, want := range map[string]any{"tool": "tool", "cited": "citation", "silent": nil} {
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
		"tool_response": []map[string]string{{"type": "text", "text": mcpSearchText(res)}}})
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
		"tool_response": []map[string]string{{"type": "text", "text": mcpSearchText(res)}}})
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
