package retain

import (
	"encoding/json"
	"slices"
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
	all := func(transcript.Record) (string, bool) { return "workspace", true }
	notes := NotesFromRecords("b", "s1", recs, nil, all)
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
	again := NotesFromRecords("b", "s1", recs, nil, all)
	if again[0].DocumentID != a.DocumentID || again[0].OperationID != a.OperationID {
		t.Error("ids must be deterministic so a rerun collapses")
	}
	if other := NotesFromRecords("b", "s2", recs, nil, all); other[0].DocumentID == a.DocumentID {
		t.Error("the session must be part of the id")
	}
	if none := NotesFromRecords("b", "s1", recs, nil, func(transcript.Record) (string, bool) { return "", false }); len(none) != 0 {
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
	p := PricesFor("deepseek-flash", 0, 0, 0, "USD")
	// DeepSeek's peak prices (the conservative ones): the cap must never under-count.
	if !p.Known() || p.Input != 0.30 || p.Cached != 0.006 || p.Output != 1.20 {
		t.Fatalf("%+v", p)
	}
	// The retired name is still accepted by DeepSeek and billed the same.
	if old := PricesFor("DeepSeek-V4-Flash", 0, 0, 0, "USD"); old != p {
		t.Errorf("legacy name: %+v", old)
	}
	// 1M fresh input + 1M cached + 0.5M output + 0.5M thoughts.
	if got := p.Cost(2_000_000, 1_000_000, 500_000, 500_000); got < 1.5059 || got > 1.5061 {
		t.Errorf("cost %v", got)
	}
	if q := PricesFor("deepseek-v4-flash", 1, 0, 2, "EUR"); q.Input != 1 || q.Cached != 1 || q.Currency != "EUR" {
		t.Errorf("config must override the table, cached falling back to input: %+v", q)
	}
	if got := p.Cost(100, 500, 0, 0); got != 500*p.Cached/1e6 {
		t.Errorf("cached above input must not go negative: %v", got)
	}
	if l := PricesFor("gpt-6-luna", 0, 0, 0, "USD"); l.Input != 0.10 || l.Cached != 0.01 || l.Output != 0.50 {
		t.Errorf("setup's openai default needs its price: %+v", l)
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
		e.Cost < 0.0881 || e.Cost > 0.0883 {
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

// A memory_store project input wins over the session's project; an ingest
// note without a project tag takes the session's (M1 review).
func TestNoteProjectPrecedence(t *testing.T) {
	ts := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	recs := []transcript.Record{{UUID: "a1", Timestamp: ts, Tools: []transcript.ToolUse{
		{ID: "t1", Name: "mcp__plugin_backsight_memory__memory_store", Input: map[string]json.RawMessage{
			"kind": raw("convention"), "text": raw("Tabs in Makefiles."), "project": raw("infra")}},
		{ID: "t2", Name: "mcp__hindsight__hindsight_ingest_document", Input: map[string]json.RawMessage{
			"title": raw("Leçon : x"), "content": raw("y"), "tags": raw([]string{"kind:lesson"})}},
	}}}
	notes := NotesFromRecords("b", "s1", recs, nil, func(transcript.Record) (string, bool) { return "billing", true })
	if len(notes) != 2 || notes[0].Project != "infra" || notes[1].Project != "billing" {
		t.Errorf("%+v", notes)
	}
}

// A replayed save naming the transverse pool goes to the canonical alias, as
// a live one does; a real project is untouched; a save without project in a
// pool directory keeps its record's project, folded to the canonical alias
// (issue 33).
func TestReplayedTransverseNotesGetTheAlias(t *testing.T) {
	ts := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	store := func(id string, in map[string]any) transcript.ToolUse {
		m := map[string]json.RawMessage{"kind": raw("convention"), "text": raw("Rule " + id)}
		for k, v := range in {
			m[k] = raw(v)
		}
		return transcript.ToolUse{ID: id, Name: "mcp__plugin_backsight_memory__memory_store", Input: m}
	}
	recs := []transcript.Record{{UUID: "a1", Timestamp: ts, Tools: []transcript.ToolUse{
		store("keyword", map[string]any{"project": "transverse"}),
		store("alias", map[string]any{"project": "home"}),
		store("real", map[string]any{"project": "billing"}),
		store("none", nil),
		{ID: "ingest", Name: "mcp__hindsight__hindsight_ingest_document", Input: map[string]json.RawMessage{
			"title": raw("Leçon : x"), "content": raw("y"), "tags": raw([]string{"project:transverse"})}},
	}}}
	aliases := []string{".claude", "home"}
	notes := NotesFromRecords("b", "s1", recs, aliases, func(transcript.Record) (string, bool) { return "home", true })
	got := []string{}
	for _, n := range notes {
		got = append(got, n.Project)
	}
	if want := []string{".claude", ".claude", "billing", ".claude", ".claude"}; !slices.Equal(got, want) {
		t.Errorf("projects %v, want %v", got, want)
	}
	if tags := notes[0].Item("m").Tags; !slices.Contains(tags, "project:.claude") || slices.Contains(tags, "project:transverse") {
		t.Errorf("tags %v", tags)
	}
	// Outside a pool directory, a save without project takes its record's.
	notes = NotesFromRecords("b", "s1", recs, aliases, func(transcript.Record) (string, bool) { return "billing", true })
	if notes[3].Project != "billing" {
		t.Errorf("%+v", notes[3])
	}
}
