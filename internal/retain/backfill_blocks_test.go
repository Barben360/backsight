package retain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/transcript"
)

func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestNotesFromRecords(t *testing.T) {
	ts := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	recs := []transcript.Record{{UUID: "a1", Timestamp: ts, Tools: []transcript.ToolUse{
		{ID: "t1", Name: "mcp__hindsight__hindsight_ingest_document", Input: map[string]json.RawMessage{
			"title": raw("Décision : sauvegarde de ~/.claude"), "content": raw("Le repo claude-config est privé."), "tags": raw([]string{"project:dotfiles"})}},
		{ID: "t2", Name: "mcp__plugin_backsight_memory__memory_store", Input: map[string]json.RawMessage{
			"kind": raw("pitfall"), "text": raw("Use key " + "sk-" + "ant-" + strings.Repeat("Ab3dE5gH9k", 3))}},
		{ID: "t3", Name: "Bash", Input: map[string]json.RawMessage{"command": raw("ls")}},
		{ID: "t4", Name: "mcp__user-memory__memory_store", Input: map[string]json.RawMessage{"kind": raw("idea"), "text": raw("Unknown kind falls back.")}},
	}}}
	all := func(string) (string, bool) { return "workspace", true }
	notes := NotesFromRecords("b", "s1", recs, all)
	if len(notes) != 3 {
		t.Fatalf("%d notes: %+v", len(notes), notes)
	}
	a, b, c := notes[0], notes[1], notes[2]
	if a.Kind != "decision" || a.Project != "dotfiles" || !strings.HasPrefix(a.Content, "Décision : sauvegarde") || !a.CreatedAt.Equal(ts) {
		t.Errorf("ingest note %+v", a)
	}
	if b.Kind != "pitfall" || b.Project != "workspace" || !strings.Contains(b.Content, "[redacted:anthropic_key]") {
		t.Errorf("memory_store note %+v", b)
	}
	if c.Kind != "lesson" {
		t.Errorf("unknown kind: %+v", c)
	}
	again := NotesFromRecords("b", "s1", recs, all)
	if again[0].DocumentID != a.DocumentID || again[0].OperationID != a.OperationID {
		t.Error("ids must be deterministic so a rerun collapses")
	}
	if other := NotesFromRecords("b", "s2", recs, all); other[0].DocumentID == a.DocumentID {
		t.Error("the session must be part of the id")
	}
	if none := NotesFromRecords("b", "s1", recs, func(string) (string, bool) { return "", false }); len(none) != 0 {
		t.Errorf("records in an opted-out directory must give no note: %+v", none)
	}
}

func TestKindFromTitle(t *testing.T) {
	for title, want := range map[string]string{"Décision : x": "decision", "Correction : y": "correction", "État : z": "state",
		"Piège: w": "pitfall", "Convention: v": "convention", "decision: lowercase": "decision", "Pitfall: en": "pitfall",
		"State of things": "state", "Random title": "lesson"} {
		if got := kindFromTitle(title); got != want {
			t.Errorf("%q: %s", title, got)
		}
	}
}

func TestPricesAndCost(t *testing.T) {
	p := PricesFor("deepseek-v4-flash", 0, 0, 0, "USD")
	if !p.Known() || p.Input != 0.14 || p.Output != 0.28 {
		t.Fatalf("%+v", p)
	}
	// 1M fresh input + 1M cached + 0.5M output + 0.5M thoughts.
	if got := p.Cost(2_000_000, 1_000_000, 500_000, 500_000); got < 0.4227 || got > 0.4229 {
		t.Errorf("cost %v", got)
	}
	if q := PricesFor("deepseek-v4-flash", 1, 0, 2, "EUR"); q.Input != 1 || q.Cached != 1 || q.Currency != "EUR" {
		t.Errorf("config must override the table, cached falling back to input: %+v", q)
	}
	if got := p.Cost(100, 500, 0, 0); got != 500*p.Cached/1e6 {
		t.Errorf("cached above input must not go negative: %v", got)
	}
	if PricesFor("some-local-model", 0, 0, 0, "USD").Known() {
		t.Error("an unknown model has no price")
	}
}

func TestEstimateSession(t *testing.T) {
	p := PricesFor("deepseek-v4-flash", 0, 0, 0, "USD")
	e := EstimateSession(60000, p)
	// 2 chunks × 3300 + 60000/3.5 + 100 facts × 1600 = 6600 + 17142 + 160000
	if e.InputTokens != 183742 || e.OutputTokens != 27561 || e.Duration != 2*time.Minute+50*time.Second ||
		e.Cost < 0.0334 || e.Cost > 0.0335 {
		t.Errorf("%+v", e)
	}
	if small := EstimateSession(0, p); small.InputTokens != 3300 {
		t.Errorf("an empty session still costs one prompt: %+v", small)
	}
	if sum := e.Add(e); sum.InputTokens != 2*e.InputTokens || sum.Chars != 120000 {
		t.Errorf("%+v", sum)
	}
}

func TestEstimateChunkBoundary(t *testing.T) {
	p := Prices{}
	if a, b := EstimateSession(30000, p), EstimateSession(30001, p); b.InputTokens-a.InputTokens < 3300 {
		t.Errorf("30001 characters is a second chunk: %d vs %d", a.InputTokens, b.InputTokens)
	}
}
