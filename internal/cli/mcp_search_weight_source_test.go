package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// sourcedHits answers hits 1 to n of project proj, hit i scoring as in
// weightHits (scores overrides), hits 1 to 6 built from a note's fact,
// the others from a conversation's.
func sourcedHits(n int, scores map[int]float64) {
	var hits []string
	for i := 1; i <= n; i++ {
		score, ok := scores[i]
		if !ok {
			score = 0.99 - float64(i)*0.01
		}
		fact := "fc"
		if i <= 6 {
			fact = "fn"
		}
		hits = append(hits, fmt.Sprintf(`{"id":%q,"text":%q,"type":"observation","tags":["project:proj"],"mentioned_at":"2026-10-20T09:00:00Z","source_fact_ids":[%q],"scores":{"reranker":%.3f}}`,
			weightID(i), weightText(i), fact, score))
	}
	recallAnswer = "[" + strings.Join(hits, ",") + "]"
	recallSourceFacts = `{"fn":{"tags":["source:note"]},"fc":{"tags":["source:chat"]}}`
}

// The lighter answer and credit per show by source together: the hook
// journals every line, shortened past rank 8 and id only for a repeat,
// with its rank; the search's recall line gives each its source; and
// used.by_source counts each memory's first show at its rank (ranks 1 to
// 8 and all ranks) with its credit.
func TestShortenedAndRepeatHitsLandInUsedBySource(t *testing.T) {
	cs, home := creditSession(t)
	if err := feedback.SetTurn(pathsOf(home).FeedbackDir(), "S", feedback.Turn{ID: "p-1", N: 1, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sourcedHits(12, nil)
	first := searchAsHook(t, cs, "S") // 1-8 whole, 9-12 shortened
	// hit 13, new, at rank 9; hits 1-8 shown whole: id only
	sourcedHits(13, map[int]float64{13: 0.905})
	second := searchAsHook(t, cs, "S")
	if !shortened(hitLine(first, 10), 10, 10) || !idOnly(hitLine(second, 1), 1, 1) || !shortened(hitLine(second, 9), 9, 13) {
		t.Fatalf("the answers' shapes:\n%s\n%s", first, second)
	}
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 2 || len(shows[0].Items) != 12 || len(shows[1].Items) != 13 {
		t.Fatalf("both searches journaled whole: %+v", shows)
	}
	for _, c := range []struct{ ev, rank, hit int }{{0, 10, 10}, {0, 12, 12}, {1, 1, 1}, {1, 9, 13}} {
		if it := shows[c.ev].Items[c.rank-1]; it.ID != weightID(c.hit) || it.Rank != c.rank {
			t.Errorf("search %d, rank %d: %+v, want hit %d", c.ev+1, c.rank, it, c.hit)
		}
	}
	recalls := recallEvents(t, home)
	if len(recalls) != 2 || recalls[1].Items[8].ID != weightID(13) || recalls[1].Items[8].Source != "session" || recalls[0].Items[1].Source != "note" {
		t.Fatalf("recall lines carry each hit's source: %+v", recalls)
	}
	for _, ids := range [][]string{{weightID(2)[:8]}, {weightID(10)[:8], weightID(13)[:8]}} {
		out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": ids})
		if isErr {
			t.Fatalf("credit %v: %s", ids, out)
		}
		fire(t, "post-tool-use", map[string]any{"session_id": "S", "tool_name": "mcp__plugin_backsight_memory__memory_credit",
			"tool_input": map[string]any{"ids": ids}, "tool_response": []map[string]string{{"type": "text", "text": out}}})
	}
	now := time.Now().UTC()
	v := runView(t, "stats", "quality", "--dev", "--since", now.Add(-time.Hour).Format(time.RFC3339), "--until", now.Add(time.Hour).Format(time.RFC3339))
	m := at(v.env.Data, "metrics", "used.by_source")
	if at(m, "num") != float64(1) || at(m, "den") != float64(8) {
		t.Errorf("ranks 1-8: %v of %v, want hit 2 of hits 1-8:\n%s", at(m, "num"), at(m, "den"), v.raw)
	}
	cell := func(src, ranks string) string {
		c := at(m, "extra", "by_source", src, "search", ranks)
		return fmt.Sprintf("%v/%v", at(c, "num"), at(c, "den"))
	}
	for _, c := range []struct{ src, ranks, want string }{
		{"note", "all", "1/6"}, {"note", "top8", "1/6"}, // hits 1-6, hit 2 credited
		{"conversation", "all", "2/7"}, {"conversation", "top8", "0/2"}, // hits 7-13, 10 and 13 credited past rank 8
	} {
		if got := cell(c.src, c.ranks); got != c.want {
			t.Errorf("%s %s: %s, want %s", c.src, c.ranks, got, c.want)
		}
	}
	if a := at(m, "extra", "all_ranks"); fmt.Sprintf("%v/%v", at(a, "num"), at(a, "den")) != "3/13" {
		t.Errorf("all ranks %v, want 3/13", a)
	}
}
