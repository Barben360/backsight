package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/recall"
)

// cxRollout writes the recorded root rollout where Codex keeps it, its
// originator (backsight's entrypoint) replaced by originator; it returns
// its path.
func cxRollout(t *testing.T, originator string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "rollouts", "root.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"originator":"codex-tui"`), []byte(`"originator":"`+originator+`"`), 1)
	p := filepath.Join(t.TempDir(), "sessions", "2026", "11", "10", "rollout-2026-11-10T20-34-44-"+cxRoot+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A session's entrypoint is read by the harness whose conversation it is:
// a Codex rollout's originator (the recorded compressed thread was run by
// codex exec), a Claude Code transcript's entrypoint as before.
func TestParentEntrypointReadsEachHarnesssConversation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions", "2026", "11", "10")
	_ = os.MkdirAll(dir, 0o700)
	packed := filepath.Join(dir, "rollout-2026-11-10T20-35-38-01a1c72c-6968-7223-8b71-37becf81a899.jsonl.zst")
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "rollouts", "compressed.jsonl.zst"))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(packed, raw, 0o600)
	claude := filepath.Join(t.TempDir(), "s1.jsonl")
	_ = os.WriteFile(claude, []byte(`{"type":"user","uuid":"u1","sessionId":"s1","entrypoint":"sdk-cli","message":{"role":"user","content":"hi"}}`+"\n"), 0o600)
	for path, want := range map[string]string{packed: "codex_exec", cxRollout(t, "codex-tui"): "codex-tui", claude: "sdk-cli", "": "", filepath.Join(dir, "missing.jsonl"): ""} {
		if got := parentEntrypoint(path); got != want {
			t.Errorf("%s: %q, want %q", filepath.Base(path), got, want)
		}
	}
}

// A subagent of a headless Codex session (codex exec, skipped by default)
// gets no report and no end-of-turn check, as a Claude Code Agent SDK
// session's does; the same subagent of an interactive session does.
func TestCodexHeadlessSessionsSubagentIsLeftOut(t *testing.T) {
	for originator, kept := range map[string]bool{"codex_exec": false, "codex-tui": true} {
		t.Run(originator, func(t *testing.T) {
			home := hookHome(t)
			parent := cxRollout(t, originator)
			cxHook(t, harness.EventUserPromptSubmit, cxPayload(t, home, "UserPromptSubmit", map[string]any{"transcript_path": parent}))
			sub := recall.Result{Hits: []recall.Hit{{ID: stopIDB, Text: "Billing moved in October.", Project: "billing", Age: "3d", Score: 0.8}}}
			cxHook(t, harness.EventPostToolUse, cxPayload(t, home, "PostToolUse.subagent.mcp__memory__memory_search", map[string]any{"transcript_path": parent, "tool_response": cxSearchResponse(mcpSearchText(sub, true))}))
			rollout := filepath.Join(filepath.Dir(parent), "rollout-2026-11-10T20-34-51-"+cxAgent+".jsonl")
			_ = os.WriteFile(rollout, []byte(`{"type":"session_meta","payload":{"id":"`+cxAgent+`","parent_thread_id":"`+cxRoot+`"}}`+"\n"), 0o600)
			out := cxHook(t, harness.EventSubagentStop, cxPayload(t, home, "SubagentStop.subagent", map[string]any{
				"transcript_path": parent, "agent_transcript_path": rollout, "last_assistant_message": padTo("Found the leak in pool.go.", 450)}))
			notes, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl"))
			reported := strings.Contains(string(notes), `"document_id":"subagent:`+cxRoot+`:`+cxAgent+`"`)
			if reported != kept || (out != nil) != kept {
				t.Errorf("%s: report queued %v, answered %s; want both %v", originator, reported, out, kept)
			}
		})
	}
}
