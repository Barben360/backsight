package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/testtool"
)

type gateOut struct {
	Specific struct {
		Event    string `json:"hookEventName"`
		Decision string `json:"permissionDecision"`
		Reason   string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func gate(t *testing.T, home string) gateOut {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": filepath.Join(home, "billing"), "tool_name": "AskUserQuestion",
		"tool_input": map[string]any{"questions": []map[string]any{{"question": "Which PostgreSQL version should billing use?"}}}})
	out, err := handleHook(context.Background(), "pre-tool-use", raw)
	if err != nil {
		t.Fatal(err)
	}
	var g gateOut
	if len(out) > 0 {
		_ = json.Unmarshal(out, &g)
	}
	return g
}

func TestGateDeniesOncePerPromptWithHits(t *testing.T) {
	home := hookHome(t)
	recallAnswer = `[{"id":"m1","text":"Billing keeps PostgreSQL 16 for partial indexes.","type":"observation","tags":["project:billing"],"scores":{"reranker":0.8}}]`
	_ = sessions(home).Update("s1", func(st *session.State) error { st.Prompts = 2; return nil })
	g := gate(t, home)
	if g.Specific.Event != "PreToolUse" || g.Specific.Decision != "deny" || !strings.HasPrefix(g.Specific.Reason, instructions.GateReason) ||
		!strings.Contains(g.Specific.Reason, "PostgreSQL 16") {
		t.Fatalf("%+v", g)
	}
	if !strings.Contains(strings.Join(recallQueries, "|"), "Which PostgreSQL version") {
		t.Errorf("the question was not searched: %q", recallQueries)
	}
	if g := gate(t, home); g.Specific.Decision != "" {
		t.Errorf("denied twice in one prompt: %+v", g)
	}
	_ = sessions(home).Update("s1", func(st *session.State) error { st.Prompts = 3; return nil })
	if g := gate(t, home); g.Specific.Decision != "deny" {
		t.Error("a new prompt must gate again")
	}
	if !strings.Contains(telemetryText(t, home), `"event":"gate"`) {
		t.Error("gate not logged")
	}
}

func TestGateAllowsAfterAMemorySearch(t *testing.T) {
	home := hookHome(t)
	_ = sessions(home).Update("s1", func(st *session.State) error { st.Prompts = 2; st.SearchedPromptID = 2; return nil })
	if g := gate(t, home); g.Specific.Decision != "" {
		t.Errorf("%+v", g)
	}
}

func TestStopRecordsTheLastReply(t *testing.T) {
	home := hookHome(t)
	if h := fire(t, "stop", map[string]any{"session_id": "s1", "cwd": home, "last_assistant_message": strings.Repeat("a", 5000) + " Shall I merge?"}); h.Specific.Context != "" || h.SystemMessage != "" {
		t.Errorf("stop must print nothing: %+v", h)
	}
	st, _ := sessions(home).Load("s1")
	if !strings.HasSuffix(st.LastAssistant, "Shall I merge?") || len([]rune(st.LastAssistant)) > 1000 {
		t.Errorf("%d chars: ...%s", len(st.LastAssistant), st.LastAssistant[max(0, len(st.LastAssistant)-20):])
	}
}

// subagentTranscript writes the transcript a real subagent leaves next to
// its session's and returns the session transcript's path.
func subagentTranscript(t *testing.T, sid, agentID string) string {
	t.Helper()
	dir := t.TempDir()
	parent := filepath.Join(dir, sid+".jsonl")
	_ = os.WriteFile(parent, []byte(`{"type":"user","uuid":"u1","sessionId":"`+sid+`","cwd":"/w","entrypoint":"cli","message":{"role":"user","content":"go"}}`+"\n"), 0o600)
	sub := filepath.Join(dir, sid, "subagents")
	_ = os.MkdirAll(sub, 0o700)
	_ = os.WriteFile(filepath.Join(sub, "agent-"+agentID+".jsonl"), []byte(`{"type":"assistant"}`+"\n"), 0o600)
	return parent
}

func TestSubagentStopQueuesItsReport(t *testing.T) {
	home := hookHome(t)
	fire(t, "subagent-stop", map[string]any{"session_id": "s1", "agent_id": "agent-7", "agent_type": "Explore", "cwd": filepath.Join(home, "billing"),
		"transcript_path":        subagentTranscript(t, "s1", "agent-7"),
		"last_assistant_message": "Found the leak in pool.go. Token ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + " was in the log. " + strings.Repeat("x", 5000)})
	raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl"))
	if !strings.Contains(string(raw), `"document_id":"subagent:s1:agent-7"`) || !strings.Contains(string(raw), `"source":"subagent"`) ||
		!strings.Contains(string(raw), `"agent_type":"Explore"`) ||
		strings.Contains(string(raw), "a1B2c3D4e5F6a1B2") || !strings.Contains(string(raw), "pool.go") {
		t.Fatalf("outbox:\n%.600s", raw)
	}
	var n struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal([]byte(strings.SplitN(string(raw), "\n", 2)[0]), &n)
	if len([]rune(n.Content)) > 4000 {
		t.Errorf("report %d chars", len([]rune(n.Content)))
	}
	if st, _ := sessions(home).Load("s1"); !st.SubagentDone {
		t.Error("subagent result not recorded for the approval detector")
	}
}

func TestSubagentStopHonoursPauseAndTheWriteFlag(t *testing.T) {
	home := hookHome(t)
	_ = sessions(home).Update("s1", func(st *session.State) error { st.Paused = true; return nil })
	fire(t, "subagent-stop", map[string]any{"session_id": "s1", "agent_id": "a", "cwd": filepath.Join(home, "billing"), "transcript_path": subagentTranscript(t, "s1", "a"), "last_assistant_message": "report"})
	if raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl")); len(raw) != 0 {
		t.Error("paused session queued a report")
	}
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("[components]\nwrite = false\n")...), 0o600)
	fire(t, "subagent-stop", map[string]any{"session_id": "s2", "agent_id": "a", "cwd": filepath.Join(home, "billing"), "transcript_path": subagentTranscript(t, "s2", "a"), "last_assistant_message": "report"})
	if raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl")); len(raw) != 0 {
		t.Error("write = false queued a report")
	}
}

func TestSessionEndSpawnsADetachedFlush(t *testing.T) {
	home := hookHome(t)
	var got []string
	old := spawnDetached
	spawnDetached = func(args ...string) error { got = args; return nil }
	t.Cleanup(func() { spawnDetached = old })
	fire(t, "session-end", map[string]any{"session_id": "s1", "cwd": home, "reason": "prompt_input_exit"})
	if strings.Join(got, " ") != "retain flush --session s1" {
		t.Errorf("spawned %v", got)
	}
	got = nil
	fire(t, "session-end", map[string]any{"session_id": "../x", "cwd": home})
	if got != nil {
		t.Errorf("an invalid session id was passed on: %v", got)
	}
}

func TestSubagentStopSkipsTheReportOfAHeadlessSession(t *testing.T) {
	home := hookHome(t)
	parent := subagentTranscript(t, "s1", "a") // a real subagent, of a headless session
	_ = os.WriteFile(parent, []byte(`{"type":"user","uuid":"u1","sessionId":"s1","cwd":"/w","entrypoint":"sdk-cli","message":{"role":"user","content":"cron digest"}}`+"\n"), 0o600)
	fire(t, "subagent-stop", map[string]any{"session_id": "s1", "agent_id": "a", "cwd": filepath.Join(home, "billing"),
		"transcript_path": parent, "last_assistant_message": "All 11 workers exited with code 0."})
	if raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl")); len(raw) != 0 {
		t.Fatalf("a headless session's subagent report was queued:\n%s", raw)
	}
	interactive := subagentTranscript(t, "s2", "a")
	fire(t, "subagent-stop", map[string]any{"session_id": "s2", "agent_id": "a", "cwd": filepath.Join(home, "billing"),
		"transcript_path": interactive, "last_assistant_message": "Found the leak in pool.go."})
	if raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl")); !strings.Contains(string(raw), "pool.go") {
		t.Errorf("an interactive session's report was not queued:\n%s", raw)
	}
}

func TestSubagentStopIgnoresInternalHelperAgents(t *testing.T) {
	home := hookHome(t)
	parent := filepath.Join(t.TempDir(), "s1.jsonl") // the session's transcript, no subagents/ file for this agent
	_ = os.WriteFile(parent, []byte(`{"type":"user","uuid":"u1","sessionId":"s1","cwd":"/w","entrypoint":"cli","message":{"role":"user","content":"go"}}`+"\n"), 0o600)
	fire(t, "subagent-stop", map[string]any{"session_id": "s1", "agent_id": "a1fc250e6b154ac35", "cwd": filepath.Join(home, "billing"),
		"transcript_path": parent, "last_assistant_message": "Reviewing checkMention in docs_commands_test.go"})
	if raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl")); len(raw) != 0 {
		t.Fatalf("a helper agent's status line was queued:\n%s", raw)
	}
	if st, _ := sessions(home).Load("s1"); st.SubagentDone {
		t.Error("a helper agent counted as a subagent result for the approval detector")
	}
	// agent_transcript_path, when Claude Code gives it, is the one checked.
	real := subagentTranscript(t, "s3", "b")
	fire(t, "subagent-stop", map[string]any{"session_id": "s1", "agent_id": "x", "cwd": filepath.Join(home, "billing"),
		"agent_transcript_path": filepath.Join(strings.TrimSuffix(real, ".jsonl"), "subagents", "agent-b.jsonl"), "last_assistant_message": "Found the leak in pool.go."})
	if raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl")); !strings.Contains(string(raw), "pool.go") {
		t.Errorf("a subagent with agent_transcript_path was not queued:\n%s", raw)
	}
}

// gitRepo makes dir a git repository with origin remote.
func gitRepo(t *testing.T, dir, remote string) {
	t.Helper()
	testtool.RequireGit(t)
	_ = os.MkdirAll(dir, 0o700)
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// A subagent's report goes to the own repository its work was in, and
// the hook records the decision; a third-party clone's work stays with
// the session's project.
func TestSubagentStopFilesTheReportWhereItsWorkIs(t *testing.T) {
	testtool.SkipWindows(t, "Task 3b: attribution with git on Windows (git's forward slashes, C:\\ transcript paths, 8.3 short names, drive-letter case)")
	home := hookHome(t)
	billing, shop, vendor := filepath.Join(home, "billing"), filepath.Join(home, "src", "shop"), filepath.Join(home, "build", "vendor-tool")
	gitRepo(t, billing, "git@github.com:me/billing.git")
	gitRepo(t, shop, "git@github.com:me/shop.git")
	gitRepo(t, vendor, "https://github.com/someone/vendor-tool")
	// s3's transcript lags: its final reply is not written yet, so its
	// evidence may be partial and the report keeps its session's project.
	for _, c := range []struct{ sid, edits, want string }{{"s1", shop, "shop"}, {"s2", vendor, "billing"}, {"s3", shop, "billing"}} {
		parent := subagentTranscript(t, c.sid, "ag1")
		_ = os.WriteFile(parent, []byte(`{"type":"user","uuid":"u1","sessionId":"`+c.sid+`","cwd":"`+testtool.JSONPath(billing)+`","entrypoint":"cli","message":{"role":"user","content":"go"}}`+"\n"), 0o600)
		agent := filepath.Join(strings.TrimSuffix(parent, ".jsonl"), "subagents", "agent-ag1.jsonl")
		line := func(uuid, file string) string {
			return `{"type":"assistant","isSidechain":true,"uuid":"` + uuid + `","cwd":"` + testtool.JSONPath(billing) + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"` + uuid + `","name":"Edit","input":{"file_path":"` + filepath.Join(file, "x.go") + `"}}]}}` + "\n"
		}
		reply := `{"type":"assistant","isSidechain":true,"uuid":"r","cwd":"` + testtool.JSONPath(billing) + `","message":{"role":"assistant","content":[{"type":"text","text":"Fixed in ` + c.sid + `"}]}}` + "\n"
		if c.sid == "s3" {
			reply = ""
		}
		_ = os.WriteFile(agent, []byte(`{"type":"user","isSidechain":true,"uuid":"p","cwd":"`+testtool.JSONPath(billing)+`","message":{"role":"user","content":"Fix it."}}`+"\n"+line("e1", c.edits)+line("e2", c.edits)+reply), 0o600)
		fire(t, "subagent-stop", map[string]any{"session_id": c.sid, "agent_id": "ag1", "agent_type": "general-purpose", "cwd": billing,
			"transcript_path": parent, "last_assistant_message": "Fixed in " + c.sid})
	}
	raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl"))
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var n struct {
			DocumentID string `json:"document_id"`
			Project    string `json:"project"`
		}
		_ = json.Unmarshal([]byte(l), &n)
		got[n.DocumentID] = n.Project
	}
	if got["subagent:s1:ag1"] != "shop" || got["subagent:s2:ag1"] != "billing" || got["subagent:s3:ag1"] != "billing" {
		t.Errorf("report projects %v", got)
	}
	// the decision travels with the report, into its document's metadata
	if !strings.Contains(string(raw), `"attribution":"evidence"`) || !strings.Contains(string(raw), `"attribution":"incomplete"`) {
		t.Errorf("outbox without the decisions:\n%s", raw)
	}
	tel := telemetryText(t, home)
	if !strings.Contains(tel, `"event":"attribution"`) || !strings.Contains(tel, `"path_rule":"shop"`) || !strings.Contains(tel, `"base":"billing"`) {
		t.Errorf("the decision was not recorded:\n%s", tel)
	}
}
