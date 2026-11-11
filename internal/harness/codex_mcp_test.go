package harness

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// codex_mcp.go's ledger: block 5, https://github.com/Barben360/backsight/issues/280.
func init() {
	codexOwns("codex_mcp.go", codexPart{over: true, done: []string{"MCPCallSession", "MCPCallDirs"}})
}

// mcpFixture is a file of the recorded Codex 0.167.0 fixtures.
func mcpFixture(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("..", "testdata", "codex", "0.167.0", rel)
}

// codexCallMetas are the _meta of every tools/call a recorded server
// traffic file holds, in order.
func codexCallMetas(t *testing.T, rel string) []map[string]any {
	t.Helper()
	fh, err := os.Open(mcpFixture(t, rel))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	var metas []map[string]any
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Meta map[string]any `json:"_meta"`
			} `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Method == "tools/call" {
			metas = append(metas, msg.Params.Meta)
		}
	}
	if len(metas) == 0 {
		t.Fatalf("%s: no tools/call", rel)
	}
	return metas
}

const codexRootSession = "01a1c72b-9533-7bb0-9002-52dca295a387" // session-root.txt

// Every recorded call names the root session, the subagent's included,
// whose threadId is its own; the server's environment never stands in.
func TestCodexCallSessionIsTheRootSession(t *testing.T) {
	env := func(k string) string {
		return map[string]string{"CODEX_SESSION_ID": "env-session", "CODEX_THREAD_ID": "env-thread", "CLAUDE_CODE_SESSION_ID": "claude-session"}[k]
	}
	for _, rel := range []string{"mcp/root.in.jsonl", "mcp/subagent.in.jsonl"} {
		for _, meta := range codexCallMetas(t, rel) {
			if got := (Codex{}).MCPCallSession(meta, env); got != codexRootSession {
				t.Errorf("%s: call %v: session %q, want the root %q", rel, meta["callId"], got, codexRootSession)
			}
		}
	}
	sub := codexCallMetas(t, "mcp/subagent.in.jsonl")[0]
	if sub["threadId"] == codexRootSession {
		t.Fatal("the subagent fixture's threadId is the root: it proves nothing")
	}
	// The shared process: two threads, one session.
	raw, err := os.ReadFile(mcpFixture(t, "mcp/shared-process-calls.json"))
	if err != nil {
		t.Fatal(err)
	}
	var shared []struct {
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal(raw, &shared); err != nil || len(shared) != 2 {
		t.Fatalf("%v %d", err, len(shared))
	}
	a, b := (Codex{}).MCPCallSession(shared[0].Meta, nil), (Codex{}).MCPCallSession(shared[1].Meta, nil)
	if a == "" || a != b || a != shared[0].Meta["sessionId"] || shared[0].Meta["threadId"] == shared[1].Meta["threadId"] {
		t.Errorf("shared process: %q and %q", a, b)
	}
}

// A call that names no session, or names it in a way that does not hold
// (not a string, not an id, two different ones), has none: the server
// then fails closed where the session matters.
func TestCodexCallWithoutItsSessionHasNone(t *testing.T) {
	env := func(string) string { return codexRootSession }
	for name, meta := range map[string]map[string]any{
		"no _meta":              nil,
		"no sessionId":          {"threadId": codexRootSession, "x-codex-turn-metadata": map[string]any{"session_id": codexRootSession}},
		"empty":                 {"sessionId": ""},
		"not a string":          {"sessionId": 42},
		"not an id":             {"sessionId": "../../etc/passwd"},
		"turn names another":    {"sessionId": codexRootSession, "x-codex-turn-metadata": map[string]any{"session_id": "01a1c72c-5442-7f52-968f-e3e1dbac783c"}},
		"turn metadata garbled": {"sessionId": codexRootSession, "x-codex-turn-metadata": "x"},
	} {
		if got := (Codex{}).MCPCallSession(meta, env); got != "" {
			t.Errorf("%s: session %q", name, got)
		}
	}
	// The turn's metadata may leave the session out: sessionId alone holds.
	if got := (Codex{}).MCPCallSession(map[string]any{"sessionId": codexRootSession, "x-codex-turn-metadata": map[string]any{"turn_id": "t"}}, nil); got != codexRootSession {
		t.Errorf("without the turn's session_id: %q", got)
	}
}

// The directories a call's turn works in are its workspaces, absolute
// paths only.
func TestCodexCallDirs(t *testing.T) {
	got := (Codex{}).MCPCallDirs(codexCallMetas(t, "mcp/root.in.jsonl")[0])
	if want := "/home/tester/work"; filepath.IsAbs(want) && (len(got) != 1 || got[0] != want) {
		t.Errorf("recorded call: %v", got)
	}
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	meta := map[string]any{"x-codex-turn-metadata": map[string]any{"workspaces": map[string]any{
		b: map[string]any{}, a: map[string]any{}, "relative": map[string]any{}, "": map[string]any{},
	}}}
	if got := (Codex{}).MCPCallDirs(meta); len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("sorted, absolute only: %v", got)
	}
	for _, meta := range []map[string]any{nil, {}, {"x-codex-turn-metadata": "x"}, {"x-codex-turn-metadata": map[string]any{"workspaces": []any{a}}}} {
		if got := (Codex{}).MCPCallDirs(meta); len(got) != 0 {
			t.Errorf("%v: %v", meta, got)
		}
	}
}
