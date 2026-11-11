package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A state.json written before the per-tool entries keeps everything it
// recorded for Claude Code, its profile above all, and saves it back
// under the same key.
func TestOlderClientEntryIsClaudeCodes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	old := `{"schema_version": 1, "applied_migrations": [], "client": {"retention_before": 30, "profile": "/p/dev", "shim_previous": "/bin/0.1.0/backsight"}}`
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cc := s.ClientOf(HarnessClaudeCode)
	if cc == nil || cc.Profile != "/p/dev" || string(cc.RetentionBefore) != "30" || cc.ShimPrevious != "/bin/0.1.0/backsight" || s.ProfileOf(HarnessClaudeCode) != "/p/dev" {
		t.Fatalf("Claude Code's entry %+v", cc)
	}
	if s.ClientOf(HarnessCodex) != nil || s.ProfileOf(HarnessCodex) != "" {
		t.Errorf("Codex got Claude Code's entry: %+v", s.ClientOf(HarnessCodex))
	}
	if err := Save(p, s); err != nil {
		t.Fatal(err)
	}
	var back map[string]json.RawMessage
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["clients"]; ok {
		t.Errorf("an entry was made for another tool: %s", raw)
	}
	var client Client
	if err := json.Unmarshal(back["client"], &client); err != nil || client.Profile != "/p/dev" || string(client.RetentionBefore) != "30" {
		t.Errorf("saved back %s (%v)", back["client"], err)
	}
}

// Each tool records its own profile; Claude Code's stays where an older
// binary reads it, so a downgrade never wires the shell's profile instead.
func TestProfilesAreRecordedPerTool(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	var s State
	s.ClientFor(HarnessClaudeCode).Profile = "/home/u/.claude-work"
	s.ClientFor(HarnessCodex).Profile = "/home/u/.codex-work"
	if s.ClientFor(HarnessCodex) != s.Clients[HarnessCodex] || s.ClientFor(HarnessClaudeCode) != s.Client {
		t.Fatal("ClientFor made a second entry for a tool that has one")
	}
	if err := Save(p, s); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileOf(HarnessClaudeCode) != "/home/u/.claude-work" || got.ProfileOf(HarnessCodex) != "/home/u/.codex-work" {
		t.Errorf("profiles %q %q", got.ProfileOf(HarnessClaudeCode), got.ProfileOf(HarnessCodex))
	}

	// What a binary from before the per-tool entries reads of that file.
	raw, _ := os.ReadFile(p)
	var older struct {
		Client *Client `json:"client"`
	}
	if err := json.Unmarshal(raw, &older); err != nil || older.Client == nil || older.Client.Profile != "/home/u/.claude-work" {
		t.Errorf("an older binary finds no Claude Code profile in %s", raw)
	}
	var codex struct {
		Clients map[string]Client `json:"clients"`
	}
	if err := json.Unmarshal(raw, &codex); err != nil || codex.Clients[HarnessCodex].Profile != "/home/u/.codex-work" || len(codex.Clients) != 1 {
		t.Errorf("clients %s", raw)
	}
}

// clients is a field of State, not a newer binary's key kept in extra:
// saving it twice keeps one entry per tool.
func TestClientsIsAKnownKey(t *testing.T) {
	var s State
	s.ClientFor(HarnessCodex).Profile = "/c"
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ProfileOf(HarnessCodex) != "/c" || back.extra["clients"] != nil {
		t.Errorf("profile %q, extra %s", back.ProfileOf(HarnessCodex), back.extra["clients"])
	}
}
