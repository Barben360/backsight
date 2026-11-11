package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
)

// pidlessTool is the fake tool, keeping no by-pid pairing (as Codex).
type pidlessTool struct{ fakeTool }

func (pidlessTool) PairsByPID() bool { return false }

// askTool is the fake tool with its own question tool; mutedTool has none.
type (
	askTool   struct{ fakeTool }
	mutedTool struct{ fakeTool }
)

func (askTool) QuestionTool() string   { return "ask_human" }
func (mutedTool) QuestionTool() string { return "" }

// A harness that pairs by pid gets the record under the pid; one that does
// not gets none, only the record by session id.
func TestHooksKeepAByPIDRecordOnlyForAHarnessThatPairsSo(t *testing.T) {
	home := hookHome(t)
	t.Setenv(claudePIDEnv, "31337")
	dir := pathsOf(home).SessionsDir()
	raw, _ := json.Marshal(map[string]string{"conversation": "fk1", "folder": filepath.Join(home, "billing")})
	if _, err := handleHook(harness.WithAdapter(context.Background(), pidlessTool{}), harness.EventSessionStart, raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := session.ReadLive(dir, 31337); ok {
		t.Error("a harness that does not pair by pid got a by-pid record")
	}
	if _, ok := session.ReadLiveSession(dir, "fk1"); !ok {
		t.Error("no record by session id")
	}
	if _, err := handleHook(harness.WithAdapter(context.Background(), fakeTool{}), harness.EventSessionStart, raw); err != nil {
		t.Fatal(err)
	}
	if l, ok := session.ReadLive(dir, 31337); !ok || l.SessionID != "fk1" {
		t.Errorf("a harness that pairs by pid: %+v %v", l, ok)
	}
}

// A memory server whose harness keeps no by-pid pairing never reads the
// record under its parent's pid: its call's session wins.
func TestMemoryServerReadsNoPIDRecordForAHarnessThatPairsOtherwise(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dir := pathsOf(home).SessionsDir()
	_ = os.MkdirAll(filepath.Join(home, "billing"), 0o700)
	_ = os.MkdirAll(filepath.Join(home, "shop"), 0o700)
	if err := session.WriteLive(dir, 4242, session.Live{SessionID: "other", Cwd: filepath.Join(home, "shop"), Project: "shop", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteLiveSession(dir, session.Live{SessionID: "fk1", Cwd: filepath.Join(home, "billing"), Project: "billing", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := newEngine(cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	deps := newMCPDeps(pidlessTool{}, func(string) string { return "" }, pathsOf(home), home, cfg, sec, eng, 0)
	deps.parentPID = 4242
	srv := newMCPServer(deps)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	retainStatus.Store(http.StatusServiceUnavailable) // the note stays queued, where the test reads it
	if out, isErr := callAs(t, cs, "fk1", "memory_store", map[string]any{"kind": "decision", "text": "Decision (2026-11-11): billing keeps PostgreSQL 16 for partial indexes."}); isErr {
		t.Fatal(out)
	}
	raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl"))
	var n retain.Note
	_ = json.Unmarshal(raw, &n)
	if n.Session != "fk1" || n.Project != "billing" {
		t.Errorf("note filed under %s/%s, want fk1/billing", n.Session, n.Project)
	}
}

// The question gate stops the harness's own question tool, not Claude
// Code's under another harness.
func TestTheGateStopsTheHarnesssQuestionTool(t *testing.T) {
	home := hookHome(t)
	recallAnswer = `[{"id":"m1","text":"Billing keeps PostgreSQL 16 for partial indexes.","type":"observation","tags":["project:billing"],"scores":{"reranker":0.8}}]`
	gateOf := func(a harness.Adapter, sid, tool string) []byte {
		_ = sessions(home).Update(sid, func(st *session.State) error { st.Prompts = 2; return nil })
		raw, _ := json.Marshal(map[string]any{"conversation": sid, "folder": filepath.Join(home, "billing"), "tool": tool,
			"toolArgs": map[string]any{"questions": []map[string]any{{"question": "Which PostgreSQL version?"}}}})
		out, err := handleHook(harness.WithAdapter(context.Background(), a), harness.EventPreToolUse, raw)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := gateOf(askTool{}, "fk1", "AskUserQuestion"); out != nil {
		t.Errorf("Claude Code's question tool stopped: %s", out)
	}
	if out := gateOf(askTool{}, "fk2", "ask_human"); out == nil {
		t.Error("the harness's question tool was not stopped")
	}
	// A harness with no question tool: nothing is a question, not even a
	// call that names no tool.
	if out := gateOf(mutedTool{}, "fk3", ""); out != nil {
		t.Errorf("a harness without a question tool stopped a call: %s", out)
	}
}

// The codex command is reached through Codex's runner (codex_cli_test.go
// runs it for real); Claude Code's runner is unchanged.
func TestToolRunnersPerHarness(t *testing.T) {
	run := toolRunner(harnessid.Codex)
	if run == nil {
		t.Fatal("no runner for Codex")
	}
	t.Setenv("PATH", t.TempDir()) // no codex there
	if _, err := run(t.Context(), nil, "--version"); !errors.Is(err, harness.ErrMissing) {
		t.Errorf("codex runner: %v", err)
	}
	f := withClaude(t)
	if out, err := toolRunner(harness.ClaudeCode)(t.Context(), nil, "--version"); err != nil || string(out) != f.version+" (Claude Code)\n" {
		t.Errorf("claude runner: %q %v", out, err)
	}
	if toolRunner("nope") != nil {
		t.Error("a runner for an unknown harness")
	}
}
