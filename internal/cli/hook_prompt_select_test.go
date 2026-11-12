package cli

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/recall"
)

// selectRecall is the per-prompt limits of issue 328: 3 memories, up to 5
// when confidently relevant, within the given characters.
func selectRecall(chars int) config.Recall {
	return config.Recall{PromptHits: 3, PromptHitsMax: 5, PromptConfidentScore: 0.8, PromptChars: chars}
}

func scored(id string, score float64, text string) recall.Hit {
	return recall.Hit{ID: id, Project: "billing", Text: text, Score: score, Reranker: score}
}

func chosenIDs(hits []recall.Hit) string {
	var out []string
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return strings.Join(out, ",")
}

func keepAll(recall.Hit) bool { return false }

// A long first memory no longer takes the whole budget: it is shortened to
// a headline that keeps its id, and the next memories still come whole.
func TestSelectHitsShortensALongMemoryAndKeepsTheNextOnes(t *testing.T) {
	long := strings.Repeat("the billing service keeps PostgreSQL 16 ", 20) // 800 chars
	got := selectHits([]recall.Hit{
		scored("aaaaaaaa-1", 0.7, long),
		scored("bbbbbbbb-2", 0.6, "Billing deploys on Fridays are frozen."),
		scored("cccccccc-3", 0.55, "The billing cron runs at 02:00 UTC."),
	}, selectRecall(500), keepAll)
	if chosenIDs(got.Chosen) != "aaaaaaaa-1,bbbbbbbb-2,cccccccc-3" {
		t.Fatalf("chosen %s, block:\n%s", chosenIDs(got.Chosen), got.Block)
	}
	if got.Shortened != 1 || got.CutSize != 0 || got.CutCount != 0 {
		t.Errorf("shortened %d, cut by size %d, by count %d", got.Shortened, got.CutSize, got.CutCount)
	}
	first := strings.Split(got.Block, "\n")[1]
	if !strings.HasPrefix(first, "- [billing · aaaaaaaa]") || !strings.HasSuffix(first, "…") || len([]rune(first)) > 220 {
		t.Errorf("the long memory is not a short headline with its id: %q", first)
	}
	for _, whole := range []string{"Billing deploys on Fridays are frozen.", "The billing cron runs at 02:00 UTC."} {
		if !strings.Contains(got.Block, whole) {
			t.Errorf("%q is not shown whole:\n%s", whole, got.Block)
		}
	}
	if n := len([]rune(strings.TrimPrefix(got.Block, instructions.RecallHeader+"\n"))); n > 500 {
		t.Errorf("memory lines take %d chars, over the 500 budget", n)
	}
}

// A memory that fits whole is keepAll shortened, whatever its rank.
func TestSelectHitsShowsWholeWhatFits(t *testing.T) {
	text := strings.Repeat("x", 600)
	got := selectHits([]recall.Hit{scored("m1", 0.9, text)}, selectRecall(1500), keepAll)
	if got.Shortened != 0 || !strings.Contains(got.Block, text) {
		t.Fatalf("shortened %d, block:\n%s", got.Shortened, got.Block)
	}
}

// When even a headline no longer fits, the memory is counted as cut by
// size and the next one is still tried.
func TestSelectHitsCountsWhatTheSizeLeavesOut(t *testing.T) {
	got := selectHits([]recall.Hit{
		scored("m1", 0.9, strings.Repeat("y", 300)),
		scored("m2", 0.85, strings.Repeat("z", 300)),
		scored("m3", 0.8, "short"),
	}, selectRecall(360), keepAll)
	if chosenIDs(got.Chosen) != "m1,m3" || got.CutSize != 1 {
		t.Fatalf("chosen %s, cut by size %d, block:\n%s", chosenIDs(got.Chosen), got.CutSize, got.Block)
	}
}

// Beyond 3, a memory comes only when confidently relevant, up to 5; the
// others are counted as cut by the count.
func TestSelectHitsAddsConfidentMemoriesUpToTheMax(t *testing.T) {
	got := selectHits([]recall.Hit{
		scored("m1", 0.95, "one"), scored("m2", 0.93, "two"), scored("m3", 0.9, "three"),
		scored("m4", 0.85, "four"), scored("m5", 0.82, "five"), scored("m6", 0.81, "six"),
	}, selectRecall(1500), keepAll)
	if chosenIDs(got.Chosen) != "m1,m2,m3,m4,m5" || got.CutCount != 1 {
		t.Fatalf("chosen %s, cut by count %d", chosenIDs(got.Chosen), got.CutCount)
	}
	got = selectHits([]recall.Hit{
		scored("m1", 0.95, "one"), scored("m2", 0.7, "two"), scored("m3", 0.6, "three"),
		scored("m4", 0.79, "four"), scored("m5", 0.55, "five"),
	}, selectRecall(1500), keepAll)
	if chosenIDs(got.Chosen) != "m1,m2,m3" || got.CutCount != 2 {
		t.Fatalf("not confident: chosen %s, cut by count %d", chosenIDs(got.Chosen), got.CutCount)
	}
}

// Skipped hits (seen, muted) are neither chosen nor counted as cut.
func TestSelectHitsDoesNotCountSkippedHitsAsCut(t *testing.T) {
	got := selectHits([]recall.Hit{scored("m1", 0.9, "one"), scored("seen", 0.9, "two"), scored("m3", 0.9, "three")},
		selectRecall(1500), func(h recall.Hit) bool { return h.ID == "seen" })
	if chosenIDs(got.Chosen) != "m1,m3" || got.CutCount != 0 || got.CutSize != 0 {
		t.Fatalf("chosen %s, cut %d/%d", chosenIDs(got.Chosen), got.CutCount, got.CutSize)
	}
}

// The header tells the agent how to read a shortened memory in full.
func TestRecallHeaderSaysHowToReadAShortenedMemory(t *testing.T) {
	if !strings.Contains(instructions.RecallHeader, "…") || !strings.Contains(instructions.RecallHeader, "memory_read") {
		t.Fatalf("header: %q", instructions.RecallHeader)
	}
}

// A config that raised prompt_hits above prompt_hits_max keeps its count:
// the max only ever adds.
func TestSelectHitsKeepsARaisedPromptHits(t *testing.T) {
	rc := selectRecall(1500)
	rc.PromptHits, rc.PromptHitsMax = 4, 3
	got := selectHits([]recall.Hit{scored("m1", 0.6, "one"), scored("m2", 0.6, "two"), scored("m3", 0.6, "three"), scored("m4", 0.6, "four"), scored("m5", 0.6, "five")}, rc, keepAll)
	if chosenIDs(got.Chosen) != "m1,m2,m3,m4" || got.CutCount != 1 {
		t.Fatalf("chosen %s, cut by count %d", chosenIDs(got.Chosen), got.CutCount)
	}
}
