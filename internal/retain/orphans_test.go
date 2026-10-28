package retain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A PC leaving its own server: a backfill of its transcripts sends again
// the replay of their saves and nothing else, so a note neither replayed
// from one of them nor stored live in one of their sessions stays in the
// old bank only.
func TestCountOrphanNotes(t *testing.T) {
	dir := t.TempDir()
	s1 := filepath.Join(dir, "s1.jsonl")
	line := `{"type":"assistant","uuid":"s1-a1","sessionId":"s1","cwd":"/home/u/billing","timestamp":"2026-09-20T09:00:00Z","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"t1","name":"mcp__plugin_backsight_memory__memory_store","input":{"kind":"decision","text":"Billing keeps PostgreSQL 16."}}]}}` + "\n"
	if err := os.WriteFile(s1, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	replay := "note:" + uuid5(nsBacksight, "claude-memory\ns1\nt1")
	h := &banksHS{docs: map[string]map[string]json.RawMessage{"claude-memory": {
		replay:            json.RawMessage(`{"source":"note"}`),
		"note:live-s1":    json.RawMessage(`{"source":"note","session":"s1"}`),
		"note:live-s9":    json.RawMessage(`{"source":"note","session":"s9"}`),
		"note:no-session": json.RawMessage(`{"source":"note"}`),
		"conversation:s1": json.RawMessage(`{"session_id":"s1"}`),
		"conversation:s9": json.RawMessage(`{"session_id":"s9"}`),
	}}, configs: map[string]map[string]any{}}
	orphans, total, err := CountOrphanNotes(context.Background(), h.client(t), "claude-memory", []string{s1})
	if err != nil || orphans != 2 || total != 4 {
		t.Errorf("orphans %d of %d: %v", orphans, total, err)
	}
	// Without the transcript, every note is an orphan; a bank that cannot
	// be listed is an error, not a count.
	if orphans, total, err := CountOrphanNotes(context.Background(), h.client(t), "claude-memory", nil); err != nil || orphans != 4 || total != 4 {
		t.Errorf("no transcript: orphans %d of %d: %v", orphans, total, err)
	}
	if _, _, err := CountOrphanNotes(context.Background(), h.client(t), "other-bank", []string{s1}); err == nil {
		t.Error("a missing bank was counted")
	}
}
