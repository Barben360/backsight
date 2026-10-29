package cli

import (
	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
)

// journalRecall records what a memory_search returned, as a recall audit
// event: each hit's id, rank, rank without the boost, server's score and
// boost, and whether the boost changed which hits were returned
// (set_changed: one ranked past them without it) or their order. Only
// for the session this server belongs to (a current record), unpaused,
// under its current turn, when feedback is on; ids and numbers only. A
// failure is logged, never fails the search.
func (d mcpDeps) journalRecall(live session.Live, found string, res recall.Result) {
	if !d.cfg.Feedback.Enabled || found != "record" || len(res.Hits) == 0 {
		return
	}
	if paused, _ := session.Paused(d.dirs.SessionsDir(), live.SessionID); paused {
		return
	}
	e := feedback.Event{Kind: feedback.KindRecall, Path: "search", Items: make([]feedback.Item, 0, len(res.Hits))}
	setChanged, orderChanged := false, false
	for i, h := range res.Hits {
		e.Items = append(e.Items, feedback.Item{ID: h.ID, Rank: i + 1, RankWithoutBoost: h.RankWithoutBoost, Reranker: h.Reranker, Boost: h.Boost})
		if h.RankWithoutBoost != 0 {
			setChanged = setChanged || h.RankWithoutBoost > len(res.Hits)
			orderChanged = orderChanged || h.RankWithoutBoost != i+1
		}
	}
	if setChanged {
		e.Flags = append(e.Flags, feedback.FlagSetChanged)
	}
	if orderChanged {
		e.Flags = append(e.Flags, feedback.FlagOrderChanged)
	}
	dir := d.dirs.FeedbackDir()
	if t, ok := feedback.CurrentTurn(dir, live.SessionID); ok {
		e.Turn = t.ID
	}
	j := feedback.NewJournal(dir, feedback.Common{Session: live.SessionID, Project: live.Project, Binary: buildinfo.Version, V: instructions.Version})
	if err := j.Append(e); err != nil {
		d.tw.Emit(telemetry.Raw{Name: "feedback_error", Fields: map[string]any{"error": err.Error()}})
	}
}
