package cli

import (
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
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
func journalForget(ids []string) {
	cfg, _, err := loadServerConfig()
	if err != nil || !cfg.Feedback.Enabled {
		return
	}
	d, _, err := machine()
	if err != nil {
		return
	}
	_ = feedback.NewJournal(d.FeedbackDir(), feedback.Common{Binary: buildinfo.Version}).Append(feedback.Event{Kind: "forget", IDs: ids})
}
