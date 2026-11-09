package cli

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

// ledgerMigrateBudget bounds `feedback migrate`, which `update apply`
// runs after its commit point: a lock or a disk that hangs never holds
// the update.
var ledgerMigrateBudget = 60 * time.Second

// ledgerMigrate is ledgerdb.Migrate; tests swap it.
var ledgerMigrate = ledgerdb.Migrate

// The fixed sentences of a failed feedback migrate: the only text update
// apply reads of it (never a path nor an error's text).
const (
	ledgerUpdatingSentence   = "an update is being applied: the feedback ledger is migrated once it is done"
	ledgerBusySentence       = "the feedback ledger is in use: try again in a moment"
	ledgerUnreadableSentence = "the feedback ledger cannot be read: backsight feedback rebuild recomputes it from the journal"
	ledgerFailedSentence     = "the feedback ledger could not be migrated: the next jobs tick retries"
)

// feedbackOff is why feedback migrate built nothing: feedback.enabled is
// false and there was no file to migrate.
const feedbackOff = "feedback is off"

// feedbackMigrateResult is feedback migrate's JSON: ledgerdb's result,
// and why nothing was done when it was skipped.
type feedbackMigrateResult struct {
	ledgerdb.MigrateResult
	Skipped string `json:"skipped,omitempty"`
}

// newFeedbackMigrateCmd is the hidden `feedback migrate`, which `update
// apply` runs with the new binary after its commit point: it applies the
// pending migrations of the ledger database and builds a missing one from
// the journal, so that the first tick after the update only folds.
func newFeedbackMigrateCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:    "migrate",
		Short:  "Apply the pending migrations of the feedback ledger, building it from the journal when missing",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), ledgerMigrateBudget)
			defer cancel()
			_, cfg, err := feedbackConfig()
			if err != nil {
				return err
			}
			o := ledgerOptions(d, home)
			o.Now = now
			o.ForceSetAside = true // run on purpose (update apply, or by hand), never by the tick
			var res feedbackMigrateResult
			if !cfg.Feedback.Enabled {
				// Off: an existing file still migrates (it is at the head
				// when feedback is turned on again); a missing one is never
				// built.
				pr, err := ledgerMigrate(ctx, o, true)
				if err != nil {
					return ledgerError(err)
				}
				if !pr.Existed {
					res = feedbackMigrateResult{MigrateResult: pr, Skipped: feedbackOff}
					return e.emit("feedback migrate", res, func() string { return ledgerMigrateText(res, plan) })
				}
			}
			if res.MigrateResult, err = ledgerMigrate(ctx, o, plan); err != nil {
				return ledgerError(err)
			}
			return e.emit("feedback migrate", res, func() string { return ledgerMigrateText(res, plan) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "list the pending migrations and apply none")
	return cmd
}

// ledgerError is a ledger database error as feedback migrate reports it:
// one of the fixed sentences.
func ledgerError(err error) error {
	msg := ledgerFailedSentence
	switch {
	case errors.Is(err, ledgerdb.ErrUpdateInProgress):
		msg = ledgerUpdatingSentence
	case errors.Is(err, ledgerdb.ErrBusy) || sqlitedb.Code(err) == "busy":
		msg = ledgerBusySentence
	case errors.Is(err, ledgerdb.ErrUnreadable) || errors.Is(err, ledgerdb.ErrSchema):
		msg = ledgerUnreadableSentence
	}
	return &output.Error{Code: output.Blocked, Message: msg}
}

func ledgerMigrateText(r feedbackMigrateResult, plan bool) string {
	ids, what := r.Applied, "applied "
	if plan {
		ids, what = r.Pending, "would apply "
	}
	var s string
	switch {
	case r.Skipped != "":
		s = "feedback ledger: not built, " + r.Skipped
	case r.SetAside:
		s = "feedback ledger: damaged, set aside as " + r.File + ".corrupt and built again from the journal"
	case len(ids) == 0:
		s = "feedback ledger: up to date"
	case !r.Existed && plan:
		s = "feedback ledger: would be built from the journal"
	case !r.Existed:
		s = "feedback ledger: built from the journal"
	default:
		s = "feedback ledger: " + what + strings.Join(ids, ", ")
	}
	if r.Newer {
		s += " (" + r.File + ": the ledger.db beside it is a newer backsight's)"
	}
	return s + "\n"
}
