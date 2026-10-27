package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
