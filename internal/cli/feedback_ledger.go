package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/telemetry"
)

// feedbackParams is the [feedback] table as the ledger's parameters. It
// lives here, not in config: feedback imports state, which imports config.
func feedbackParams(cfg config.Config) feedback.Params {
	const day = 24 * time.Hour
	return feedback.Params{
		ConfidentRank:  cfg.Feedback.ConfidentRank,
		MissHalfLife:   time.Duration(cfg.Feedback.MissHalfLifeDays) * day,
		CreditHalfLife: time.Duration(cfg.Feedback.CreditHalfLifeDays) * day,
		TurnTimeout:    feedback.TurnTimeout,
	}
}

// settleFeedback is the tick's settle: local files only, so a server down
// never delays it. A failure is logged and never fails the tick.
func settleFeedback(d paths.Dirs, cfg config.Config, tw *telemetry.Writer) feedback.SettleResult {
	if !cfg.Feedback.Enabled {
		return feedback.SettleResult{}
	}
	res, err := feedback.Settle(d.FeedbackDir(), feedbackParams(cfg), now())
	if err != nil {
		tw.Emit(telemetry.Raw{Name: "feedback_error", Fields: map[string]any{"error": err.Error()}})
		return feedback.SettleResult{}
	}
	if res != (feedback.SettleResult{}) {
		tw.Emit(telemetry.FeedbackSettle{Shows: res.Shows, Credits: res.Credits, Misses: res.Misses, Unknown: res.Unknown, Late: res.Late})
	}
	return res
}

// journalForget records memories the user retired, for the ledger to drop
// them. Best effort: a failed write never fails the forget.
func journalForget(ids []string) { journalUser(feedback.Event{Kind: "forget", IDs: ids}) }

// journalUnhide records memories the user restored, for the ledger to
// clear their verdicts and hiding. Best effort, like journalForget.
func journalUnhide(ids []string) {
	journalUser(feedback.Event{Kind: feedback.KindUnhide, IDs: ids, Outcome: "restored"})
}

// journalUser journals an event of a user command, when feedback is on.
func journalUser(e feedback.Event) {
	cfg, _, err := loadServerConfig()
	if err != nil || !cfg.Feedback.Enabled {
		return
	}
	d, _, err := machine()
	if err != nil {
		return
	}
	_ = feedback.NewJournal(d.FeedbackDir(), feedback.Common{Binary: buildinfo.Version}).Append(e)
}

// hiddenHere: id is hidden on this machine, by the ledger with what was
// journaled since its last settle. False when feedback is off or the
// ledger cannot be read.
func hiddenHere(id string) bool {
	cfg, _, err := loadServerConfig()
	if err != nil || !cfg.Feedback.Enabled {
		return false
	}
	d, _, err := machine()
	if err != nil {
		return false
	}
	l, err := feedback.Current(d.FeedbackDir())
	return err == nil && l.Memories[id].Hidden != nil
}

// A tick reads at most hiddenChecks histories, within hiddenCheckTimeout.
const (
	hiddenChecks       = 20
	hiddenCheckTimeout = 10 * time.Second
)

// checkHidden lifts the hidings the server made moot: an observation with
// a change in its history after it was hidden is journaled unhide
// rewritten, one the server no longer holds unhide gone. The longest
// hidden are read first; any other answer leaves the hiding as it is.
// Best effort: it returns how many it lifted.
func checkHidden(ctx context.Context, cl *hindsight.Client, bank, dir string, now time.Time) (lifted int) {
	l, err := feedback.Current(dir)
	if err != nil {
		return 0
	}
	type hiding struct {
		id    string
		since time.Time
	}
	var hidden []hiding
	for id, m := range l.Memories {
		if m.Hidden != nil {
			hidden = append(hidden, hiding{id, m.Hidden.Since})
		}
	}
	slices.SortFunc(hidden, func(a, b hiding) int {
		if c := a.since.Compare(b.since); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	ctx, cancel := context.WithTimeout(ctx, hiddenCheckTimeout)
	defer cancel()
	j := feedback.NewJournal(dir, feedback.Common{Binary: buildinfo.Version})
	for _, h := range hidden[:min(len(hidden), hiddenChecks)] {
		if ctx.Err() != nil {
			break
		}
		changes, err := cl.History(ctx, bank, h.id)
		var apiErr *hindsight.APIError
		outcome := ""
		switch {
		case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
			outcome = "gone"
		case err != nil:
			continue
		case slices.ContainsFunc(changes, func(c hindsight.HistoryEntry) bool { return c.ChangedAt.After(h.since) }):
			outcome = "rewritten"
		default:
			continue
		}
		if j.Append(feedback.Event{Kind: feedback.KindUnhide, TS: now.UTC(), IDs: []string{h.id}, Outcome: outcome}) == nil {
			lifted++
		}
	}
	return lifted
}
