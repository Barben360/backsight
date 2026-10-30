package cli

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
)

// The hooks and the MCP server run on every prompt of every session: they
// never open the metrics index, nor can the packages they use.
func TestHooksAndMCPNeverOpenTheIndex(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	writeIndexLines(t, d, monthOf(time.Now()), 3, time.Now())
	j := showTurn(t, home, "s1", "p-1", 1, false)
	journalShow(t, j, "p-1", "main", "prompt", "0b4c2a52-0000-4000-8000-000000000001")

	for _, ev := range []struct {
		name string
		in   map[string]any
	}{
		{"session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": home}},
		{"user-prompt-submit", map[string]any{"session_id": "s1", "prompt": "what did we decide about billing?", "cwd": home}},
		{"pre-tool-use", map[string]any{"session_id": "s1", "tool_name": "AskUserQuestion", "tool_input": map[string]any{"questions": []any{}}}},
		{"post-tool-use", map[string]any{"session_id": "s1", "tool_name": "Read", "tool_input": map[string]any{}}},
		{"post-tool-use-failure", map[string]any{"session_id": "s1", "tool_name": "Read", "error": "boom"}},
		{"stop", map[string]any{"session_id": "s1"}},
		{"subagent-start", map[string]any{"session_id": "s1", "agent_id": "a1", "agent_type": "Explore"}},
		{"subagent-stop", map[string]any{"session_id": "s1", "agent_id": "a1", "agent_type": "Explore"}},
		{"session-end", map[string]any{"session_id": "s1", "reason": "other"}},
	} {
		fire(t, ev.name, ev.in)
	}

	cs := mcpConnect(t, home)
	call(t, cs, "memory_search", map[string]any{"queries": []string{"billing"}})
	call(t, cs, "memory_read", map[string]any{"id": "m1"})
	call(t, cs, "memory_store", map[string]any{"kind": "correction", "text": "Billing moved to PostgreSQL 16 on 2026-10-20."})
	call(t, cs, "memory_credit", verdict("outdated", "aaaaaaaa"))

	if n := index.OpensOf(d.State); n != 0 {
		t.Errorf("a hook or an MCP tool opened the index %d time(s)", n)
	}
	if onDisk(d.IndexDir()) {
		t.Errorf("%s exists: a hook or an MCP tool built the index", d.IndexDir())
	}
}

// What the hooks and the MCP server import cannot reach the index or its
// SQLite driver, directly or through another package.
func TestHookPackagesDoNotImportTheIndex(t *testing.T) {
	const mod = "github.com/Barben360/backsight/internal/"
	args := []string{"list", "-deps"}
	for _, p := range []string{"hook", "recall", "session", "telemetry", "feedback", "project", "hindsight"} {
		args = append(args, mod+p)
	}
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 20 {
		t.Fatalf("go list found %d packages: %s", len(deps), out)
	}
	for _, dep := range deps {
		if dep == mod+"index" || strings.HasPrefix(dep, "modernc.org/sqlite") {
			t.Errorf("the hook and MCP packages depend on %s", dep)
		}
	}
}
