package scripts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func transcriptLine(t *testing.T, content []map[string]any, role string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": role, "message": map[string]any{"role": role, "content": content}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func use(id, name, cmd string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{"command": cmd}}
}

func result(id, text string, isErr bool) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isErr}
}

// The replay counts failures and signatures from transcripts without a
// server, and in live mode counts the signatures that would inject.
func TestReplayFailuresCountsAndSearches(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root, _ := filepath.Abs("..")
	dir := filepath.Join(t.TempDir(), "p", "proj")
	_ = os.MkdirAll(dir, 0o700)
	lines := []string{
		transcriptLine(t, []map[string]any{use("a", "Bash", "npm test"), use("b", "Bash", "npm test"), use("c", "Bash", "ls"), use("d", "mcp__plugin_backsight_memory__memory_search", "x"), use("e", "Read", "f")}, "assistant"),
		transcriptLine(t, []map[string]any{
			result("a", "Exit code 1\nError: Cannot find module 'express' at line 12", true),
			result("b", "Exit code 2\nerror: cannot find module 'express' at line 99", true), // same signature
			result("c", "Exit code 0\nall fine", false),                                      // not a failure
			result("d", "connection refused to the memory server", true),                     // backsight's own tool
			result("e", "File does not exist: /nope/not/here.txt", true),                     // tool not covered
		}, "user"),
	}
	_ = os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	script := filepath.Join(root, "scripts", "replay-failures.py")
	run := func(env ...string) map[string]any {
		cmd := exec.Command("python3", "-I", script, "--projects", filepath.Dir(dir), "--json")
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("replay: %v\n%s", err, out)
		}
		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("not JSON: %s", out)
		}
		return res
	}
	res := run()
	if res["failures"] != 2.0 || res["distinct_signatures"] != 1.0 || res["mode"] == nil || res["would_inject"] != nil {
		t.Fatalf("dry run: %v", res)
	}

	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		query = req.Query
		_, _ = w.Write([]byte(`{"results":[{"id":"m1","text":"t","scores":{"reranker":0.79}},{"id":"m2","text":"t","scores":{"reranker":0.81}}]}`))
	}))
	defer srv.Close()
	res = run("BACKSIGHT_REPLAY_URL="+srv.URL, "BACKSIGHT_REPLAY_BANK=b")
	if res["searched"] != 1.0 || res["would_inject"] != 1.0 || !strings.Contains(query, "npm test") || !strings.Contains(query, "Cannot find module") {
		t.Fatalf("live run: %v, query %q", res, query)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"m1","text":"t","scores":{"reranker":0.79}}]}`))
	}))
	defer srv2.Close()
	if res = run("BACKSIGHT_REPLAY_URL="+srv2.URL, "BACKSIGHT_REPLAY_BANK=b"); res["would_inject"] != 0.0 {
		t.Fatalf("a hit under 0.8 would inject: %v", res)
	}
}
