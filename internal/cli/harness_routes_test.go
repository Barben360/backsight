package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
)

// fakeToolHook runs event's handler for the fake tool with its own input.
func fakeToolHook(t *testing.T, event string, in map[string]any) (harness.HookOutput, []byte) {
	t.Helper()
	raw, _ := json.Marshal(in)
	out, err := handleHook(harness.WithAdapter(context.Background(), fakeTool{}), event, raw)
	if err != nil {
		t.Fatalf("%s: %v", event, err)
	}
	var fake struct {
		Fake harness.HookOutput `json:"fake"`
	}
	if len(out) > 0 && json.Unmarshal(out, &fake) != nil {
		t.Fatalf("%s: not the fake tool's answer: %s", event, out)
	}
	return fake.Fake, out
}

// The gate's denial and the stop check's block are answered in the
// harness's own shape, never Claude Code's.
func TestDenialsAndBlocksAreTheHarnesssAnswers(t *testing.T) {
	home := hookHome(t)
	recallAnswer = `[{"id":"m1","text":"Billing keeps PostgreSQL 16 for partial indexes.","type":"observation","tags":["project:billing"],"scores":{"reranker":0.8}}]`
	_ = sessions(home).Update("fk1", func(st *session.State) error { st.Prompts = 2; return nil })
	deny, raw := fakeToolHook(t, harness.EventPreToolUse, map[string]any{"conversation": "fk1", "folder": filepath.Join(home, "billing"), "tool": "AskUserQuestion",
		"toolArgs": map[string]any{"questions": []map[string]any{{"question": "Which PostgreSQL version should billing use?"}}}})
	if deny.Event != harness.EventPreToolUse || !strings.HasPrefix(deny.Deny, instructions.GateReason) || !strings.Contains(deny.Deny, "PostgreSQL 16") {
		t.Errorf("gate answered %s", raw)
	}

	j := showTurn(t, home, "fk2", "t-2", 2, false)
	journalShow(t, j, "t-2", "", "prompt", stopIDA)
	block, raw := fakeToolHook(t, harness.EventStop, map[string]any{"conversation": "fk2", "folder": home, "report": "done"})
	if block.Event != harness.EventStop || !strings.HasPrefix(block.Block, instructions.StopCheckHeader) || !strings.Contains(block.Block, stopIDA[:8]) {
		t.Errorf("stop answered %s", raw)
	}
	// The check's journal line names the harness.
	files, _ := filepath.Glob(filepath.Join(pathsOf(home).FeedbackDir(), "*.jsonl"))
	stops := 0
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, `"kind":"stop"`) {
				stops++
				if !strings.Contains(l, `"client":"fake-tool"`) {
					t.Errorf("stop journaled as %s", l)
				}
			}
		}
	}
	if stops == 0 {
		t.Errorf("no stop journaled in %v", files)
	}
}

// A failed call of one of backsight's own tools is told by the harness's
// own tool names: the fake's are skipped, Claude Code's are not its own.
func TestOwnToolFailuresAreTheHarnesssToolNames(t *testing.T) {
	home := hookHome(t)
	failureOn(t, home)
	recallAnswer = hits(0.95)
	in := func(tool string) map[string]any {
		return map[string]any{"conversation": "fk1", "folder": filepath.Join(home, "billing"), "tool": tool, "failure": "connection refused to the server at localhost"}
	}
	fakeToolHook(t, harness.EventPostToolUseFailure, in("fake__backsight__memory_search"))
	if tel := telemetryText(t, home); !strings.Contains(tel, `"reason":"own_tool"`) || len(recallQueries) != 0 {
		t.Fatalf("the fake's own tool was not skipped: %v\n%s", recallQueries, tel)
	}
	fakeToolHook(t, harness.EventPostToolUseFailure, in("mcp__plugin_backsight_memory__memory_search"))
	if tel := telemetryText(t, home); strings.Count(tel, `"reason":"own_tool"`) != 1 {
		t.Errorf("Claude Code's tool name counted as the fake's own:\n%s", tel)
	}
}

// fakeMCP is the memory server started by the fake tool, which names each
// call's session in its _meta.
func fakeMCP(t *testing.T, home string) *mcp.ClientSession {
	t.Helper()
	cfg, sec, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := pathsOf(home)
	eng, err := newEngine(cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	srv := newMCPServer(newMCPDeps(fakeTool{}, func(string) string { return "" }, d, home, cfg, sec, eng, 0))
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callAs calls a memory tool as the session sid (none when "").
func callAs(t *testing.T, cs *mcp.ClientSession, sid, name string, args map[string]any) (string, bool) {
	t.Helper()
	p := &mcp.CallToolParams{Name: name, Arguments: args}
	if sid != "" {
		p.Meta = mcp.Meta{"sessionId": sid}
	}
	res, err := cs.CallTool(t.Context(), p)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// One memory server, two sessions: each call is filed under the session
// its harness names for it, not one fixed when the server started.
func TestMemoryToolsTakeEachCallsSessionFromTheHarness(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	sessionsDir := pathsOf(home).SessionsDir()
	for sid, project := range map[string]string{"fk1": "billing", "fk2": "shop"} {
		dir := filepath.Join(home, project)
		_ = os.MkdirAll(dir, 0o700)
		if err := session.WriteLiveSession(sessionsDir, session.Live{SessionID: sid, Cwd: dir, Project: project, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	cs := fakeMCP(t, home)
	retainStatus.Store(http.StatusServiceUnavailable) // the notes stay queued, where the test reads them
	for _, sid := range []string{"fk1", "fk2"} {
		if out, isErr := callAs(t, cs, sid, "memory_store", map[string]any{"kind": "decision", "text": "Decision (2026-11-10): " + sid + " keeps PostgreSQL 16 for partial indexes."}); isErr {
			t.Fatalf("%s: %s", sid, out)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl"))
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var n retain.Note
		_ = json.Unmarshal([]byte(l), &n)
		got[n.Session] = n.Project
	}
	if got["fk1"] != "billing" || got["fk2"] != "shop" || len(got) != 2 {
		t.Errorf("notes by session %v\n%s", got, raw)
	}
	if out, isErr := callAs(t, cs, "fk1", "memory_credit", map[string]any{"verdict": "none"}); isErr || !strings.HasPrefix(out, creditNonePrefix) {
		t.Errorf("credit with the call's session: %q", out)
	}
	if out, isErr := callAs(t, cs, "", "memory_credit", map[string]any{"verdict": "none"}); !isErr {
		t.Errorf("credit without any session paired: %q", out)
	}
}

// The memory server's telemetry names the harness that started it; a
// harness backsight does not know starts none.
func TestMemoryServerTelemetryNamesItsHarness(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	cs := fakeMCP(t, home)
	callAs(t, cs, "fk1", "memory_search", map[string]any{"queries": []string{"postgres"}})
	var tool struct{ Event, Name, Client string }
	for _, l := range strings.Split(telemetryText(t, home), "\n") {
		if strings.Contains(l, `"name":"memory_search"`) {
			_ = json.Unmarshal([]byte(l), &tool)
		}
	}
	if tool.Name != "memory_search" || tool.Client != "fake-tool" {
		t.Errorf("tool telemetry %+v", tool)
	}
	if code, out, errOut := run(t, "mcp", "--harness", "nope"); code != output.Usage || !strings.Contains(out+errOut, "--harness") {
		t.Errorf("unknown harness: exit %d\n%s%s", code, out, errOut)
	}
}
