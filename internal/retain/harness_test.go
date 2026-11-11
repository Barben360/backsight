package retain

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

type otherTool struct{}

func (otherTool) ID() string  { return "other-tool" }
func (otherTool) Tag() string { return "harness:other-tool" }

// A note carries the tag its harness gave; one queued before notes had
// one (no field in the outbox line) is Claude Code's.
func TestNotesCarryTheirHarnessTag(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for _, n := range []Note{NewNote("b", "decision", "billing", "Keep 16.", at), NewSubagentNote("b", "s1", "a1", "", "billing", "Found it.", at)} {
		n.HarnessTag = otherTool{}.Tag()
		tags := n.Item("m").Tags
		if !slices.Contains(tags, "harness:other-tool") || slices.Contains(tags, "harness:claude-code") {
			t.Errorf("%s: %v", n.DocumentID, tags)
		}
		raw, _ := json.Marshal(n)
		var back Note
		if json.Unmarshal(raw, &back) != nil || back.HarnessTag != "harness:other-tool" {
			t.Errorf("round trip lost the tag: %s", raw)
		}
	}
	var old Note
	if err := json.Unmarshal([]byte(`{"operation_id":"o","document_id":"note:x","kind":"decision","project":"billing","content":"c","created_at":"2026-11-01T00:00:00Z"}`), &old); err != nil {
		t.Fatal(err)
	}
	if tags := old.Item("m").Tags; !slices.Contains(tags, "harness:claude-code") {
		t.Errorf("an older note: %v", tags)
	}
}

// A segment carries its harness's tag and id; BuildSegment's is Claude
// Code's.
func TestSegmentsCarryTheirHarness(t *testing.T) {
	seg := BuildSegmentOf(otherTool{}, "b", "s1", "conversation:s1", recs(), 1, "billing", "m", "v")
	if !slices.Contains(seg.Item.Tags, "harness:other-tool") || seg.Item.Metadata["harness"] != "other-tool" {
		t.Errorf("%v %v", seg.Item.Tags, seg.Item.Metadata)
	}
	def := BuildSegment("b", "s1", "conversation:s1", recs(), 1, "billing", "m", "v")
	if !slices.Contains(def.Item.Tags, "harness:claude-code") || def.Item.Metadata["harness"] != "claude-code" {
		t.Errorf("%v %v", def.Item.Tags, def.Item.Metadata)
	}
}
