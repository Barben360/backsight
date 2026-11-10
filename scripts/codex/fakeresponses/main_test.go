package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tools is the shape Codex 0.167 sends: plain functions, a free-form tool,
// and an MCP server's tools inside a namespace.
var codexTools = []map[string]any{
	{"type": "function", "name": "exec_command"},
	{"type": "custom", "name": "apply_patch"},
	{"type": "namespace", "name": "mcp__memory", "tools": []any{
		map[string]any{"type": "function", "name": "memory_search"},
		map[string]any{"type": "function", "name": "memory_store"},
	}},
	{"type": "namespace", "name": "multi_agent_v1", "tools": []any{
		map[string]any{"type": "function", "name": "spawn_agent"},
		map[string]any{"type": "function", "name": "wait_agent"},
	}},
}

func user(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func developer(text string) map[string]any {
	return map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func output(id, text string) map[string]any {
	return map[string]any{"type": "function_call_output", "call_id": id, "output": text}
}

func body(t *testing.T, input ...map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"model": "fake-model", "input": input, "tools": codexTools})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// item is the output item of a reply, which must be one created, one
// output_item.done and one completed event.
func item(t *testing.T, raw []byte, compacting bool) map[string]any {
	t.Helper()
	events, err := reply(raw, compacting)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0]["type"] != "response.created" || events[1]["type"] != "response.output_item.done" || events[2]["type"] != "response.completed" {
		t.Fatalf("events: %v", events)
	}
	return events[1]["item"].(map[string]any)
}

func text(it map[string]any) string {
	if it["type"] != "message" {
		return ""
	}
	return it["content"].([]map[string]any)[0]["text"].(string)
}

func TestScriptPlaysOneStepPerAnsweredCall(t *testing.T) {
	prompt := user(`do it <<fake [{"call":"memory_search","args":{"queries":["x"]}},{"say":"all done"}]>>`)
	first := item(t, body(t, developer("instructions"), prompt), false)
	if first["type"] != "function_call" || first["name"] != "memory_search" || first["namespace"] != "mcp__memory" || first["arguments"] != `{"queries":["x"]}` {
		t.Fatalf("first step: %v", first)
	}
	// Hook context after the prompt (a developer message) does not hide it.
	second := item(t, body(t, prompt, developer("Memory reminder"), first, output(first["call_id"].(string), "nothing in memory")), false)
	if text(second) != "all done" {
		t.Fatalf("second step: %v", second)
	}
	third := item(t, body(t, prompt, first, output("c", "x"), second, output("d", "y")), false)
	if text(third) != "done" {
		t.Fatalf("past the last step: %v", third)
	}
}

// A subagent's task carried in a step is not this conversation's script.
func TestNestedScriptIsNotTaken(t *testing.T) {
	sub := `sub <<fake [{"call":"memory_search","args":{"queries":["y"]}}]>>`
	// As a user types it: the inner block's markers stay literal.
	parent := `parent <<fake [{"call":"spawn_agent","args":{"message":"sub <<fake [{\"call\":\"memory_search\",\"args\":{\"queries\":[\"y\"]}}]>>"}}]>>`
	it := item(t, body(t, user(parent)), false)
	if it["name"] != "spawn_agent" || it["namespace"] != "multi_agent_v1" || !strings.Contains(it["arguments"].(string), "memory_search") {
		t.Fatalf("parent step: %v", it)
	}
	// The subagent's own request has only its task: it plays that one.
	if it := item(t, body(t, user(sub)), false); it["name"] != "memory_search" {
		t.Fatalf("subagent step: %v", it)
	}
}

func TestNoScriptAndCompactionAnswerPlainly(t *testing.T) {
	if got := text(item(t, body(t, user("hello")), false)); got != "ok" {
		t.Errorf("no script: %q", got)
	}
	scripted := body(t, user(`<<fake [{"call":"memory_search"}]>>`))
	if got := text(item(t, scripted, true)); !strings.HasPrefix(got, "summary") {
		t.Errorf("compaction: %q", got)
	}
	if !compaction(`{"request_kind":"compaction","turn_id":"t"}`) || compaction(`{"request_kind":"turn"}`) || compaction("") {
		t.Error("compaction header misread")
	}
}

func TestToolResolution(t *testing.T) {
	cases := []struct {
		call, wantType, wantName, wantText string
	}{
		{"exec_command", "function_call", "exec_command", ""},
		{"apply_patch", "custom_tool_call", "apply_patch", ""},
		{"memory", "message", "", "fake model: no tool named memory in the request"},
		{"agent", "message", "", ""}, // spawn_agent and wait_agent both end with _agent: refused
	}
	for _, c := range cases {
		args := `{}`
		if c.wantType == "custom_tool_call" {
			args = `"*** Begin Patch\n*** End Patch\n"`
		}
		it := item(t, body(t, user(`<<fake [{"call":"`+c.call+`","args":`+args+`}]>>`)), false)
		if it["type"] != c.wantType {
			t.Errorf("%s: %v", c.call, it)
			continue
		}
		if c.wantName != "" && it["name"] != c.wantName {
			t.Errorf("%s: name %v", c.call, it["name"])
		}
		if c.wantText != "" && text(it) != c.wantText {
			t.Errorf("%s: %q", c.call, text(it))
		}
		if c.call == "agent" && !strings.Contains(text(it), "2 tools match agent") {
			t.Errorf("ambiguous suffix: %q", text(it))
		}
		if c.wantType == "custom_tool_call" && it["input"] != "*** Begin Patch\n*** End Patch\n" {
			t.Errorf("custom input: %v", it["input"])
		}
	}
}

func TestPlaceholderTakesTheLastOutput(t *testing.T) {
	prompt := user(`<<fake [{"call":"spawn_agent"},{"call":"wait_agent","args":{"targets":["{{last:agent_id}}"]}}]>>`)
	it := item(t, body(t, prompt, output("a", `{"agent_id":"019-abc","nickname":"Turing"}`)), false)
	if it["name"] != "wait_agent" || it["arguments"] != `{"targets":["019-abc"]}` {
		t.Fatalf("wait step: %v", it)
	}
}

// Codex's memory extraction asks for structured output: the answer is a JSON
// object with every required property, whatever script the rollout holds.
func TestStructuredRequestGetsItsSchemaFilled(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"input": []map[string]any{user(`rollout text <<fake [{"call":"memory_search"}]>>`)},
		"text": map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{
			"type":     "object",
			"required": []string{"rollout_summary", "rollout_slug", "raw_memory"},
			"properties": map[string]any{
				"rollout_summary": map[string]any{"type": "string"},
				"rollout_slug":    map[string]any{"type": []string{"string", "null"}},
				"raw_memory":      map[string]any{"type": "string"},
			},
		}}},
	})
	var got map[string]string
	if err := json.Unmarshal([]byte(text(item(t, raw, false))), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["rollout_summary"] == "" || got["raw_memory"] == "" || got["rollout_slug"] != "fake-slug" {
		t.Fatalf("structured answer: %v", got)
	}
}

// The consolidation agent writes its files through code mode's exec tool,
// found among the additional_tools; v1 also writes MEMORY.md.
func TestConsolidationAgentWritesTheMemoryFiles(t *testing.T) {
	codeMode := map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{
		map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "exec"}}},
	}}
	for _, c := range []struct {
		prompt, root string
		handbook     bool
	}{
		{"## Memory Writing Agent: Phase 2 (Consolidation)\n\nFolder structure (under /h/.codex/memories/):\n", "/h/.codex/memories", true},
		{"Consolidate the supplied rollout summaries into `memory_summary.md` so another\nagent...\nRead `phase2_workspace_diff.md` in `/h/.codex/memories_v2/` first.", "/h/.codex/memories_v2", false},
	} {
		raw, _ := json.Marshal(map[string]any{"input": []map[string]any{codeMode, user(c.prompt)}})
		it := item(t, raw, false)
		in, _ := it["input"].(string)
		if it["type"] != "custom_tool_call" || it["name"] != "exec" || it["namespace"] != "functions" || !strings.Contains(in, "tools.exec_command") {
			t.Fatalf("%s: %v", c.root, it)
		}
		if !strings.Contains(in, `cd \"`+c.root+`\"`) || !strings.Contains(in, "memory_summary.md") || strings.Contains(in, "MEMORY.md") != c.handbook {
			t.Errorf("%s: %s", c.root, in)
		}
		raw, _ = json.Marshal(map[string]any{"input": []map[string]any{codeMode, user(c.prompt), it, output("x", "written")}})
		if got := text(item(t, raw, false)); got != "consolidated" {
			t.Errorf("%s: after the write: %q", c.root, got)
		}
	}
}

func TestBadScriptsAreRefused(t *testing.T) {
	for _, s := range []string{`<<fake [{"call":}]>>`, `<<fake [{"say":"x"}] no close`, `<<fake [{}]>>`} {
		if _, err := reply(body(t, user(s)), false); err == nil {
			t.Errorf("%q: accepted", s)
		}
	}
}

func TestServerLogsEachRequestWithCredentialsRedacted(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(&server{dir: dir})
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(string(body(t, user("hi <b>")))))
	req.Header.Set("Authorization", "Bearer sk-not-a-real-key")
	req.Header.Set("Session-Id", "s-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	_ = resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type %q", ct)
	}
	if !strings.Contains(b.String(), "event: response.completed\ndata: ") || !strings.Contains(b.String(), `"text":"ok"`) {
		t.Errorf("stream: %s", b.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"hi <b>"`) {
		t.Errorf("the log escapes the text: %s", raw)
	}
	if strings.Contains(string(raw), "sk-not-a-real-key") {
		t.Error("the key reached the log")
	}
	var entry struct {
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    map[string]any    `json:"body"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Path != "/v1/responses" || entry.Headers["Authorization"] != "[redacted]" || entry.Headers["Session-Id"] != "s-1" || entry.Body["model"] != "fake-model" {
		t.Errorf("log entry: %+v", entry)
	}
	resp, err = http.Get(ts.URL + "/v1/other")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "0002.json")); err != nil {
		t.Error("an unknown path is not logged")
	}
}
