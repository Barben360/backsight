package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/hook"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/session"
)

// Codex's hooks replay the payloads Codex 0.167.0 sent
// (internal/testdata/codex/0.167.0/hooks/, 0085 D3): each fixture's cwd is
// moved to the test's project, the rest is as recorded.

// The ids of the recorded run (session-root.txt, the hooks' payloads).
const (
	cxRoot      = "01a1c72b-9533-7bb0-9002-52dca295a387" // the root session, session_id of every event
	cxRootTurn  = "01a1c72b-a51a-7ae0-b12f-b52948e012b2" // the first turn's turn_id
	cxAgent     = "01a1c72b-ae08-75b3-a4db-8a06f93c0c83" // the subagent's thread, agent_id
	cxAgentTurn = "01a1c72b-ae14-7df1-8691-7e05982428c2" // the subagent's own turn_id
)

// cxPayload is the recorded payload name, its cwd the test's project, with
// set's fields replaced (a nil value removes the field).
func cxPayload(t *testing.T, home, name string, set map[string]any) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "hooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	in["cwd"] = filepath.Join(home, "billing")
	for k, v := range set {
		if v == nil {
			delete(in, k)
		} else {
			in[k] = v
		}
	}
	return in
}

// cxHook runs event's handler in-process for Codex, as its hook child
// does: the raw answer Codex would read.
func cxHook(t *testing.T, event string, in map[string]any) []byte {
	t.Helper()
	raw, _ := json.Marshal(in)
	out, err := handleHook(harness.WithAdapter(context.Background(), harness.Codex{}), event, raw)
	if err != nil {
		t.Fatalf("%s: %v", event, err)
	}
	return out
}

// cxAnswer decodes a context answer (hookOut), failing on invalid JSON.
func cxAnswer(t *testing.T, raw []byte) hookOut {
	t.Helper()
	var h hookOut
	if len(raw) > 0 && json.Unmarshal(raw, &h) != nil {
		t.Fatalf("invalid JSON: %s", raw)
	}
	return h
}

// cxSearchResponse is a memory_search result as Codex hands it to
// PostToolUse: the MCP result object.
func cxSearchResponse(text string) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}
}

// Every session start Codex sends gets the guide as context, the startup
// the briefing and the banner too; each event records the session under
// its id (where the memory server looks, from each call's
// _meta.sessionId) and never under a process id, whatever pid the hook
// child got; its journal lines and telemetry name Codex.
func TestCodexSessionStartBriefsAndPairsBySession(t *testing.T) {
	home := hookHome(t)
	t.Setenv(claudePIDEnv, "31337")
	recentAnswer["observation"] = "[" + obs("0b4c2a52-0000-4000-8000-000000000001", "Billing keeps PostgreSQL 16 for partial indexes.", time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339)) + "]"
	h := cxAnswer(t, cxHook(t, harness.EventSessionStart, cxPayload(t, home, "SessionStart.startup", nil)))
	if h.Specific.Event != "SessionStart" || !strings.HasPrefix(h.Specific.Context, instructions.Guide) || !strings.Contains(h.Specific.Context, "PostgreSQL 16") ||
		!strings.Contains(h.SystemMessage, "billing") {
		t.Fatalf("startup: %+v", h)
	}
	if l, ok := session.ReadLiveSession(pathsOf(home).SessionsDir(), cxRoot); !ok || l.Project != "billing" {
		t.Errorf("no record by session id: %+v %v", l, ok)
	}
	if _, ok := session.ReadLive(pathsOf(home).SessionsDir(), 31337); ok {
		t.Error("a Codex hook recorded its session under a process id")
	}
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 1 || shows[0].Path != "briefing" || shows[0].Session != cxRoot || shows[0].Client != "codex" {
		t.Errorf("briefing shows %+v", shows)
	}
	if tel := telemetryText(t, home); !strings.Contains(tel, `"client":"codex"`) || strings.Contains(tel, `"client":"claude-code"`) {
		t.Errorf("telemetry clients:\n%s", tel)
	}
	for _, source := range []string{"resume", "clear", "compact", "fork"} {
		in := cxPayload(t, home, "SessionStart."+source, nil)
		h := cxAnswer(t, cxHook(t, harness.EventSessionStart, in))
		if h.Specific.Event != "SessionStart" || !strings.HasPrefix(h.Specific.Context, instructions.Guide) {
			t.Errorf("%s: %+v", source, h)
		}
		if _, ok := session.ReadLiveSession(pathsOf(home).SessionsDir(), in["session_id"].(string)); !ok {
			t.Errorf("%s: no record by session id", source)
		}
	}
}

// The root thread's prompt recalls, and its turn is Codex's turn_id: the
// turn pointer the memory server reads under the session, and the turn of
// every journal line, a subagent's included. A subagent's task, which
// Codex also sends to the prompt hook (with agent_id), gets nothing: no
// recall, no reminder, no new prompt, and the turn stays its parent's.
func TestCodexPromptRecallsAndASubagentTaskDoesNot(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	h := cxAnswer(t, cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit", nil)))
	if h.Specific.Event != "UserPromptSubmit" || !strings.Contains(h.Specific.Context, "memory m1 about billing") || !strings.Contains(h.Specific.Context, instructions.For(true).Reminder) {
		t.Fatalf("root prompt: %+v", h)
	}
	if cur, ok := feedback.CurrentTurn(pathsOf(home).FeedbackDir(), cxRoot); !ok || cur.ID != cxRootTurn || cur.N != 1 {
		t.Fatalf("pointer %+v %v", cur, ok)
	}
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 1 || shows[0].Path != "prompt" || shows[0].Turn != cxRootTurn || shows[0].Session != cxRoot || shows[0].Client != "codex" {
		t.Fatalf("prompt shows %+v", shows)
	}
	queries := len(recallQueries)

	if out := cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit.subagent", nil)); out != nil {
		t.Errorf("a subagent's task answered %s", out)
	}
	if len(recallQueries) != queries {
		t.Errorf("a subagent's task recalled: %v", recallQueries[queries:])
	}
	if st, _ := sessions(home).Load(cxRoot); st.Prompts != 1 {
		t.Errorf("a subagent's task counted as prompt %d", st.Prompts)
	}
	if cur, _ := feedback.CurrentTurn(pathsOf(home).FeedbackDir(), cxRoot); cur.ID != cxRootTurn {
		t.Errorf("a subagent's task moved the turn to %q", cur.ID)
	}
	if n := len(eventsOf(journalEvents(t, home), "turn")); n != 1 {
		t.Errorf("%d turn lines: a subagent's task began a turn", n)
	}
	if !strings.Contains(telemetryText(t, home), `"reason":"subagent"`) {
		t.Error("the skipped task is not in telemetry")
	}
}

// A prompt Codex writes itself (a subagent's notification, AGENTS.md, a
// stop hook's continuation) is skipped like Claude Code's own messages:
// no recall, its turn a machine one.
func TestCodexOwnMessagesAreSkipped(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	for _, p := range []string{
		"<subagent_notification>\n{\"agent_path\":\"" + cxAgent + "\",\"status\":{\"completed\":\"SUB-DONE\"}}\n</subagent_notification>",
		"# AGENTS.md instructions for /home/tester/work\n\n<INSTRUCTIONS>\nUse PostgreSQL 16 for billing everywhere.\n</INSTRUCTIONS>",
		`<hook_prompt hook_run_id="stop:1">Memories were shown to you this turn, credit them before you stop.</hook_prompt>`,
	} {
		if out := cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit", map[string]any{"prompt": p})); out != nil {
			t.Errorf("%.30q answered %s", p, out)
		}
	}
	if len(recallQueries) != 0 {
		t.Errorf("Codex's own messages recalled: %v", recallQueries)
	}
	if cur, _ := feedback.CurrentTurn(pathsOf(home).FeedbackDir(), cxRoot); !cur.Machine {
		t.Errorf("pointer %+v: not a machine turn", cur)
	}
}

// PostToolUse journals what the memory tools showed from the MCP result
// Codex sends (tool_response.content[].text), with the session, the turn
// pointer's turn, the agent and the call's id (tool_use_id, the memory
// call's _meta.callId); a subagent's search goes under its parent's turn
// and stays out of the main thread's seen set; an accepted credit is
// journaled the same way. The recorded empty search journals no show but
// still counts as this prompt's search.
func TestCodexPostToolUseJournalsShowsAndCredits(t *testing.T) {
	home := hookHome(t)
	cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit", nil))
	cxHook(t, harness.EventPostToolUse, cxPayload(t, home, "PostToolUse.mcp__backsight__memory_search", nil))
	if st, _ := sessions(home).Load(cxRoot); st.SearchedPromptID != 1 {
		t.Errorf("the recorded search is not this prompt's: %+v", st)
	}
	if n := len(eventsOf(journalEvents(t, home), "shows")); n != 0 {
		t.Fatalf("an empty search journaled %d shows", n)
	}
	root := recall.Result{Hits: []recall.Hit{{ID: stopIDA, Text: "Billing keeps PostgreSQL 16.", Project: "billing", Age: "2d", Score: 0.9}}}
	sub := recall.Result{Hits: []recall.Hit{{ID: stopIDB, Text: "Billing moved in October.", Project: "billing", Age: "3d", Score: 0.8}}}
	cxHook(t, harness.EventPostToolUse, cxPayload(t, home, "PostToolUse.mcp__backsight__memory_search", map[string]any{"tool_response": cxSearchResponse(mcpSearchText(root, true))}))
	cxHook(t, harness.EventPostToolUse, cxPayload(t, home, "PostToolUse.subagent.mcp__backsight__memory_search", map[string]any{"tool_response": cxSearchResponse(mcpSearchText(sub, true))}))
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 2 {
		t.Fatalf("shows %+v", shows)
	}
	if s := shows[0]; s.Path != "search" || s.Session != cxRoot || s.Turn != cxRootTurn || s.Agent != "" || s.Call != "call_fake_6_0" || s.Client != "codex" || itemsText(s.Items) != stopIDA+":1:0" {
		t.Errorf("root search %+v", s)
	}
	if s := shows[1]; s.Path != "search" || s.Session != cxRoot || s.Turn != cxRootTurn || s.Agent != cxAgent || s.Call != "call_fake_5_0" || itemsText(s.Items) != stopIDB+":1:0" {
		t.Errorf("subagent search %+v (its own turn is %s, not the journal's)", s, cxAgentTurn)
	}
	st, _ := sessions(home).Load(cxRoot)
	if len(st.Seen) != 1 || st.Seen[0] != stopIDA {
		t.Errorf("seen %v: the root search only", st.Seen)
	}
	cxHook(t, harness.EventPostToolUse, cxPayload(t, home, "PostToolUse.mcp__backsight__memory_search", map[string]any{
		"tool_name": "mcp__backsight__memory_credit", "tool_use_id": "call_fake_7_0", "tool_input": map[string]any{"ids": []string{stopIDA[:8]}},
		"tool_response": cxSearchResponse("Credited. Print this line in your final message:\n> 🧠 **From memory (billing · 0b4c2a52)** — x")}))
	credits := eventsOf(journalEvents(t, home), "credit")
	if len(credits) != 1 || credits[0].Verdict != "used" || strings.Join(credits[0].IDs, ",") != stopIDA || credits[0].Turn != cxRootTurn ||
		credits[0].Call != "call_fake_7_0" || credits[0].Session != cxRoot {
		t.Errorf("credits %+v", credits)
	}
}

// Stop asks for the credit once, in the answer Codex reads (decision
// block, which continues the turn), and nothing when Codex says the stop
// already goes on from a hook's block (stop_hook_active); a turn whose
// first stop is already active is never blocked.
func TestCodexStopBlocksOnceAndHonoursStopHookActive(t *testing.T) {
	home := hookHome(t)
	cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit", nil))
	res := recall.Result{Hits: []recall.Hit{{ID: stopIDA, Text: "Billing keeps PostgreSQL 16.", Project: "billing", Age: "2d", Score: 0.9}}}
	cxHook(t, harness.EventPostToolUse, cxPayload(t, home, "PostToolUse.mcp__backsight__memory_search", map[string]any{"tool_response": cxSearchResponse(mcpSearchText(res, true))}))
	out := cxHook(t, harness.EventStop, cxPayload(t, home, "Stop", nil))
	var block map[string]string
	if json.Unmarshal(out, &block) != nil || len(block) != 2 || block["decision"] != "block" || !strings.Contains(block["reason"], "0b4c2a52") ||
		!strings.HasPrefix(block["reason"], instructions.StopCheckHeader) {
		t.Fatalf("stop answered %s", out)
	}
	if out := cxHook(t, harness.EventStop, cxPayload(t, home, "Stop", map[string]any{"stop_hook_active": true})); out != nil {
		t.Errorf("stop_hook_active answered %s", out)
	}
	if got := stopOutcomes(stopsOf(t, home, cxRoot)); got != ":blocked,:undeclared" {
		t.Errorf("stops %s", got)
	}

	// A fresh turn whose stop is active from the start.
	cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit", map[string]any{"turn_id": "01a1c72b-b000-7000-8000-000000000002", "prompt": longPrompt}))
	cxHook(t, harness.EventPostToolUse, cxPayload(t, home, "PostToolUse.mcp__backsight__memory_search", map[string]any{"turn_id": "01a1c72b-b000-7000-8000-000000000002", "tool_response": cxSearchResponse(mcpSearchText(res, true))}))
	if out := cxHook(t, harness.EventStop, cxPayload(t, home, "Stop", map[string]any{"turn_id": "01a1c72b-b000-7000-8000-000000000002", "stop_hook_active": true})); out != nil {
		t.Errorf("an active first stop answered %s", out)
	}
}

// SubagentStop queues the subagent's report, read from Codex's
// agent_transcript_path (its own rollout) and tagged harness:codex, and
// asks the subagent for its credit in Codex's block answer.
func TestCodexSubagentStopReportsAndChecks(t *testing.T) {
	home := hookHome(t)
	cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit", nil))
	sub := recall.Result{Hits: []recall.Hit{{ID: stopIDB, Text: "Billing moved in October.", Project: "billing", Age: "3d", Score: 0.8}}}
	cxHook(t, harness.EventPostToolUse, cxPayload(t, home, "PostToolUse.subagent.mcp__backsight__memory_search", map[string]any{"tool_response": cxSearchResponse(mcpSearchText(sub, true))}))
	rollout := filepath.Join(t.TempDir(), "rollout-2026-11-10T20-34-51-"+cxAgent+".jsonl")
	_ = os.WriteFile(rollout, []byte(`{"type":"session_meta","payload":{"id":"`+cxAgent+`"}}`+"\n"), 0o600)
	out := cxHook(t, harness.EventSubagentStop, cxPayload(t, home, "SubagentStop.subagent", map[string]any{
		"agent_transcript_path": rollout, "last_assistant_message": padTo("Found the leak in pool.go.", 450)}))
	var block map[string]string
	if json.Unmarshal(out, &block) != nil || block["decision"] != "block" || !strings.Contains(block["reason"], "1c4c2a52") {
		t.Fatalf("subagent stop answered %s", out)
	}
	notes, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl"))
	if !strings.Contains(string(notes), `"document_id":"subagent:`+cxRoot+`:`+cxAgent+`"`) || !strings.Contains(string(notes), `"harness_tag":"harness:codex"`) {
		t.Errorf("outbox:\n%.600s", notes)
	}
	if got := stopOutcomes(stopsOf(t, home, cxRoot)); got != cxAgent+":blocked" {
		t.Errorf("stops %s", got)
	}
	// Without its rollout on disk, nothing: no report, no check.
	if out := cxHook(t, harness.EventSubagentStop, cxPayload(t, home, "SubagentStop.subagent", nil)); out != nil {
		t.Errorf("a subagent with no rollout answered %s", out)
	}
}

// SubagentStart gives the short guide, before the subagent's task.
func TestCodexSubagentStartGivesTheShortGuide(t *testing.T) {
	home := hookHome(t)
	h := cxAnswer(t, cxHook(t, harness.EventSubagentStart, cxPayload(t, home, "SubagentStart.subagent", nil)))
	if h.Specific.Event != "SubagentStart" || !strings.HasPrefix(h.Specific.Context, instructions.SubagentGuide) {
		t.Errorf("%+v", h)
	}
}

// The question gate stops Codex's question tool (request_user_input) once
// per prompt that searched nothing, with a denial Codex reads, and the
// question's hits; Claude Code's question tool under Codex is not gated.
func TestCodexQuestionGate(t *testing.T) {
	home := hookHome(t)
	cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit", nil))
	recallAnswer = `[{"id":"` + stopIDA + `","text":"Billing keeps PostgreSQL 16.","type":"observation","tags":["project:billing"],"scores":{"reranker":0.8}}]`
	if out := cxHook(t, harness.EventPreToolUse, cxPayload(t, home, "PreToolUse.request_user_input", map[string]any{"tool_name": "AskUserQuestion"})); out != nil {
		t.Errorf("AskUserQuestion under Codex answered %s", out)
	}
	out := cxHook(t, harness.EventPreToolUse, cxPayload(t, home, "PreToolUse.request_user_input", nil))
	var g gateOut
	if json.Unmarshal(out, &g) != nil || g.Specific.Decision != "deny" || !strings.HasPrefix(g.Specific.Reason, instructions.GateReason) ||
		!strings.Contains(g.Specific.Reason, "[billing · 0b4c2a52] Billing keeps PostgreSQL 16.") || !strings.Contains(string(out), `"hookEventName":"PreToolUse"`) {
		t.Fatalf("gate answered %s", out)
	}
	if q := recallQueries[len(recallQueries)-1]; q != "Which one?" {
		t.Errorf("the gate searched %q, not the question", q)
	}
	if shows := eventsOf(journalEvents(t, home), "shows"); len(shows) == 0 || shows[len(shows)-1].Path != "gate" || shows[len(shows)-1].Call != "call_fake_12_3" {
		t.Errorf("gate shows %+v", shows)
	}
	if out := cxHook(t, harness.EventPreToolUse, cxPayload(t, home, "PreToolUse.request_user_input", nil)); out != nil {
		t.Errorf("the second question of the prompt answered %s", out)
	}
}

// SessionEnd spawns the session's flush, detached.
func TestCodexSessionEndSpawnsTheFlush(t *testing.T) {
	home := hookHome(t)
	var got []string
	old := spawnDetached
	spawnDetached = func(args ...string) error { got = args; return nil }
	t.Cleanup(func() { spawnDetached = old })
	if out := cxHook(t, harness.EventSessionEnd, cxPayload(t, home, "SessionEnd.other", nil)); out != nil || strings.Join(got, " ") != "retain flush --session "+cxRoot {
		t.Errorf("printed %s, spawned %v", out, got)
	}
}

// Every text a Codex answer injects stays within what Codex keeps whole
// (CodexTextMax bytes), however much the config lets a recall inject.
func TestCodexAnswersFitBeforeCodexSpillsThem(t *testing.T) {
	home := hookHome(t)
	appendConfig(t, home, "[recall]\nprompt_hits = 10\nprompt_chars = 40000")
	var items []string
	for i := range 10 {
		items = append(items, `{"id":"m`+itoa(i+1)+`","text":"`+strings.Repeat("é billing memory ", 200)+`","type":"observation","tags":["project:billing"],"scores":{"reranker":0.9}}`)
	}
	recallAnswer = "[" + strings.Join(items, ",") + "]"
	raw := cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit", nil))
	h := cxAnswer(t, raw)
	if h.Specific.Event != "UserPromptSubmit" || len(h.Specific.Context) > harness.CodexTextMax || len(h.Specific.Context) < harness.CodexTextMax/2 || !utf8.ValidString(h.Specific.Context) {
		t.Errorf("context of %d bytes (at most %d), valid UTF-8 %v", len(h.Specific.Context), harness.CodexTextMax, utf8.ValidString(h.Specific.Context))
	}
}

// The child's deadline of each wired event is below the timeout Codex
// is given for its handler: the supervisor's answer always arrives.
func TestCodexTimeoutsLeaveTheChildItsDeadline(t *testing.T) {
	for _, e := range (harness.Codex{}).Events() {
		input := []byte(`{"source":"` + strings.Split(e.Matcher, "|")[0] + `"}`)
		if d := hook.ChildDeadline(e.Event, input); d >= time.Duration(e.Timeout)*time.Second {
			t.Errorf("%s %s: child deadline %s, Codex's timeout %ds", e.Name, e.Matcher, d, e.Timeout)
		}
	}
}

// A backsight command run from Codex's shell belongs to the session Codex
// names there (CODEX_SESSION_ID, the root session); Claude Code's
// BACKSIGHT_SESSION comes first; a subagent's thread id never names one.
func TestShellSessionFromCodexsShell(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"CODEX_SESSION_ID": cxRoot, "CODEX_THREAD_ID": cxAgent}, cxRoot},
		{map[string]string{"BACKSIGHT_SESSION": "s1", "CODEX_SESSION_ID": cxRoot}, "s1"},
		// Claude Code started from a Codex shell: Claude Code's session.
		{map[string]string{"CLAUDE_CODE_SESSION_ID": "s-claude", "CODEX_SESSION_ID": cxRoot}, ""},
		{map[string]string{"BACKSIGHT_SESSION": "s1", "CLAUDE_CODE_SESSION_ID": "s-claude", "CODEX_SESSION_ID": cxRoot}, "s1"},
		{map[string]string{"CODEX_THREAD_ID": cxAgent}, ""},
		{map[string]string{"CODEX_SESSION_ID": "../x"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := shellSession(env(c.env)); got != c.want {
			t.Errorf("%v: %q, want %q", c.env, got, c.want)
		}
	}
}

// The commands that take the shell's session (store, pause, why, feedback
// log): from Codex's shell, the root session Codex names; from a Claude
// Code session started in a Codex shell (CLAUDE_CODE_SESSION_ID set,
// CODEX_SESSION_ID inherited), what each took before Codex: store
// Claude Code's session, why the latest session, pause and feedback log
// none.
func TestShellCommandsTakeCodexsSessionOnlyOutsideClaudeCode(t *testing.T) {
	home := hookHome(t)
	t.Chdir(filepath.Join(home, "billing"))
	_ = sessions(home).Update(cxRoot, func(st *session.State) error {
		st.Injected = []session.Injection{{ID: "c1", Text: "codex memory", Rule: "first"}}
		return nil
	})
	time.Sleep(10 * time.Millisecond)
	_ = sessions(home).Update("latest", func(st *session.State) error {
		st.Injected = []session.Injection{{ID: "l1", Text: "latest memory", Rule: "first"}}
		return nil
	})
	t.Setenv("BACKSIGHT_SESSION", "")
	t.Setenv("CODEX_SESSION_ID", cxRoot)

	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-claude")
	if code, _, out := storeJSON(t, "--kind", "decision", "Billing keeps PostgreSQL 16."); code != 0 {
		t.Fatal(out)
	}
	if got := lastNoteSession(t); got != "s-claude" {
		t.Errorf("under Claude Code: note names session %q, want s-claude", got)
	}
	if code, _, _ := run(t, "pause"); code != output.Usage {
		t.Errorf("under Claude Code: pause exit %d, want a usage error (no session)", code)
	}
	if paused, _ := session.Paused(pathsOf(home).SessionsDir(), cxRoot); paused {
		t.Error("under Claude Code: the inherited Codex session was paused")
	}
	if _, out, _ := run(t, "why"); !strings.Contains(out, "latest memory") || strings.Contains(out, "codex memory") {
		t.Errorf("under Claude Code: why shows\n%s", out)
	}
	if code, _, _ := run(t, "feedback", "log"); code != output.Usage {
		t.Errorf("under Claude Code: feedback log exit %d, want a usage error (no session)", code)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if code, _, out := storeJSON(t, "--kind", "decision", "Billing keeps its cron."); code != 0 {
		t.Fatal(out)
	}
	if got := lastNoteSession(t); got != cxRoot {
		t.Errorf("from Codex: note names session %q", got)
	}
	if _, out, _ := run(t, "why"); !strings.Contains(out, "codex memory") {
		t.Errorf("from Codex: why shows\n%s", out)
	}
	if code, out, errOut := run(t, "feedback", "log", "--json"); code != output.OK || !strings.Contains(out, cxRoot) {
		t.Errorf("from Codex: feedback log exit %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut := run(t, "pause"); code != 0 {
		t.Fatalf("from Codex: pause %d %s %s", code, out, errOut)
	}
	if paused, _ := session.Paused(pathsOf(home).SessionsDir(), cxRoot); !paused {
		t.Error("from Codex: the Codex session is not paused")
	}
}

// Claude Code's prompt path is Claude Code's whatever its payload holds:
// one carrying an agent_id still recalls (the subagent-task skip is
// Codex's).
func TestClaudePromptWithAnAgentIDStillRecalls(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.9)
	h := fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "agent_id": "a7", "prompt": longPrompt, "cwd": filepath.Join(home, "billing")})
	if !strings.Contains(h.Specific.Context, "memory m1 about billing") {
		t.Errorf("context:\n%s", h.Specific.Context)
	}
	if st, _ := sessions(home).Load("s1"); st.Prompts != 1 {
		t.Errorf("prompt %d", st.Prompts)
	}
}
