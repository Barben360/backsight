package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/stats"
)

// retain resend-failed (issue 149): the sessions a failed retain or a
// given-up part names are sent again whole. The server accepts a part
// before its extraction LLM reads it: with a key the provider rejected,
// the part failed there and its turns are missing from memory, though
// still in the transcript.

const (
	// resendFailedSince: how far back a run looks by default, as doctor's
	// replace-unconfirmed check.
	resendFailedSince = 30 * 24 * time.Hour
	// failedPages: the failed operations read, 100 a page (the newest
	// first); a longer list is cut and says so.
	failedPages = 100
)

type resendFailedFlags struct {
	plan              bool
	since             string
	allowSubscription bool
	answers           []string
}

func (f resendFailedFlags) resume() string {
	cmd := "backsight retain resend-failed"
	if f.since != "" {
		cmd += " --since " + f.since
	}
	if f.allowSubscription {
		cmd += " --allow-subscription"
	}
	for _, a := range f.answers {
		cmd += " --answer " + resumeWord(a)
	}
	return cmd
}

type resendFailedResult struct {
	Bank  string `json:"bank"`
	Since string `json:"since"`
	// Action: planned | nothing | done | resends_pending | resends_waiting | partial | refused (nothing read)
	Action string `json:"action"`
	// FailedRetains: the failed retains read since Since; Cut: the list
	// was longer than read (the oldest left out). GivenUp: the parts this
	// PC gave up unconfirmed since Since.
	FailedRetains int               `json:"failed_retains"`
	Cut           bool              `json:"cut,omitempty"`
	GivenUp       int               `json:"given_up"`
	Plan          retain.FailedPlan `json:"plan"`
	Backup        string            `json:"backup,omitempty"`
	Queued        []string          `json:"queued"`
	Waiting       []string          `json:"waiting,omitempty"`
	Failed        []string          `json:"failed,omitempty"`
	Note          string            `json:"note,omitempty"`
}

func newRetainResendFailedCmd(e *env) *cobra.Command {
	var f resendFailedFlags
	cmd := &cobra.Command{
		Use:   "resend-failed",
		Short: "Send again whole the sessions whose parts failed on the server (a rejected LLM key)",
		Long: `Finds the sessions of this PC that the server failed to extract (its failed
retains, and the parts this PC gave up unconfirmed) and sends each again
whole, from its transcript: its documents are deleted, then every turn
goes out once more, which costs extraction for the whole session. A
verified backup comes first; it waits for --answer resend=yes, and refuses
while the extraction LLM still fails. Running it again does nothing more.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runResendFailed(cmd.Context(), f)
			return e.report("retain resend-failed", res, func() string { return resendFailedText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&f.plan, "plan", false, "list the sessions and what sending them again would cost, change nothing")
	cmd.Flags().StringVar(&f.since, "since", "", "look back to this day (YYYY-MM-DD; default: 30 days ago)")
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "answer a question: resend=yes|no, backup=done (a remote server, backed up on its host)")
	cmd.Flags().BoolVar(&f.allowSubscription, "allow-subscription", false, "with the claude-code provider, let the extraction run on your Claude subscription")
	return cmd
}

func runResendFailed(ctx context.Context, f resendFailedFlags) (resendFailedResult, error) {
	res := resendFailedResult{Action: "planned", Queued: []string{}}
	answers, err := parseAnswers(f.answers, "")
	if err != nil {
		return res, err
	}
	for k, v := range answers {
		switch {
		case k == "resend" && (v == "yes" || v == "no"), k == "backup" && v == "done":
		case k == "resend" || k == "backup":
			return res, &output.Error{Code: output.Usage, Message: fmt.Sprintf("%s=%s: resend takes yes or no, backup takes done", k, v)}
		default:
			return res, &output.Error{Code: output.Usage, Message: "unknown answer " + k + ": retain resend-failed asks resend and backup"}
		}
	}
	since := now().Add(-resendFailedSince).UTC().Truncate(24 * time.Hour)
	if f.since != "" {
		if since, err = time.Parse("2006-01-02", f.since); err != nil {
			return res, &output.Error{Code: output.Usage, Message: "--since takes a day as YYYY-MM-DD: " + f.since}
		}
	}
	res.Since = since.Format("2006-01-02")
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	generation := sendsGeneration(d) // before the config it goes with
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return res, err
	}
	sendsConfigLoaded()
	fl, err := newFlusher(d, home, cfg, sec, generation, false)
	if err != nil {
		return res, err
	}
	res.Bank = cfg.Server.Bank
	// As a re-attribution: never on a half-restored or half-updated
	// server, before the restore epoch is reconciled, or while retains
	// are queued (a late append to a deleted document comes back alone).
	if !f.plan {
		reconcile(ctx, fl)
	}
	if err := fl.ReattributionGuard(ctx); err != nil {
		res.Action = "refused"
		return res, &output.Error{Code: output.Blocked, Message: "nothing was changed: " + err.Error()}
	}
	// Sent again while extraction still fails, each session would fail
	// once more.
	failing := extractionFailing(ctx, fl, d, cfg.Server.Bank)
	if failing != "" && !f.plan {
		res.Action = "refused"
		return res, &output.Error{Code: output.Blocked, Message: failing + "; nothing was changed: fix it first (backsight doctor)"}
	}
	prices := retain.PricesFor(cfg.LLM.Model, cfg.LLM.PriceInput, cfg.LLM.PriceCached, cfg.LLM.PriceOutput, cfg.LLM.Currency)
	plan := func() (retain.FailedPlan, error) {
		named, err := failedSessions(ctx, fl, d.TelemetryDir(), since, &res)
		if err != nil {
			return retain.FailedPlan{}, err
		}
		resent, err := retain.FailedResends(d.State)
		if err != nil {
			return retain.FailedPlan{}, blocked(fmt.Errorf("the record of the sessions sent again cannot be read: %w", err))
		}
		docs, err := fl.Client.ListDocuments(ctx, cfg.Server.Bank)
		if err != nil {
			return retain.FailedPlan{}, blocked(err)
		}
		return fl.PlanFailedResends(named, resent, docs, transcripts(home), prices), nil
	}
	p, err := plan()
	res.Plan = p
	if err != nil {
		return res, err
	}
	if f.plan {
		res.Note = failing
		return res, nil
	}
	if len(p.Resends) == 0 {
		res.Action = "nothing"
		return res, nil
	}
	if answers["resend"] != "yes" {
		res.Action = "resends_pending"
		if answers["resend"] == "no" {
			return res, nil
		}
		return res, &output.Error{Code: output.NeedsUser, Resume: f.resume() + " --answer resend=yes",
			Questions: []setup.Question{{ID: "resend", Required: true, Options: []setup.Option{
				{Value: "yes", Label: "send them again whole", Cost: estimateText(p.Cost)},
				{Value: "no", Label: "leave them as they are"}},
				Prompt: fmt.Sprintf("%d session(s) of this PC had parts fail on the server: send them again whole? Their documents are deleted first, then every turn goes out once more", len(p.Resends))}},
			Message: fmt.Sprintf("%d session(s) wait for --answer resend=yes (%s); nothing was changed", len(p.Resends), estimateText(p.Cost))}
	}
	if cfg.LLM.Provider == "claude-code" && !f.allowSubscription {
		return res, &output.Error{Code: output.NeedsUser, Resume: f.resume() + " --allow-subscription",
			Message: "the claude-code provider extracts on your Claude subscription: add --allow-subscription to send these sessions again on it; nothing was changed"}
	}
	answered := map[string]bool{} // the sessions the cost was shown for
	for _, r := range p.Resends {
		answered[r.Session] = true
	}
	if res.Backup, err = verifiedBackup(ctx, d, cfg, "retain resend-failed", f.resume(), answers["backup"] == "done"); err != nil {
		return res, err
	}
	l, note, err := acquireMaintenance(d, "retain resend-failed")
	if err != nil {
		return res, err
	}
	defer func() { _ = l.Release() }()
	res.Note = note
	// Under the lock: no flush runs, and nothing is queued on the server.
	if err := fl.ReattributionGuard(ctx); err != nil {
		return res, &output.Error{Code: output.Blocked, Message: "nothing was changed: " + err.Error()}
	}
	if p, err = plan(); err != nil {
		return res, err
	}
	res.Plan = p
	for _, r := range p.Resends {
		if !answered[r.Session] {
			continue // planned after the answer: its cost was not shown
		}
		err := fl.ApplyResend(ctx, r)
		switch {
		case errors.Is(err, retain.ErrQueued):
			res.Waiting = append(res.Waiting, r.Session)
		case err != nil:
			res.Failed = append(res.Failed, r.Session+": "+err.Error())
		default:
			res.Queued = append(res.Queued, r.Session)
			if err := retain.RecordFailedResend(d.State, r.Session, now(), 2*resendFailedSince); err != nil {
				res.Failed = append(res.Failed, r.Session+": sent again, but not recorded as such (a later run would send it again): "+err.Error())
			}
		}
	}
	switch {
	case len(res.Failed) > 0:
		res.Action = "partial"
		return res, &output.Error{Code: output.Partial, Resume: f.resume(),
			Message: fmt.Sprintf("%d session(s) failed (%s): run the same command again to finish", len(res.Failed), strings.Join(res.Failed, "; "))}
	case len(res.Waiting) > 0:
		res.Action = "resends_waiting"
		return res, &output.Error{Code: output.Blocked, Resume: f.resume(),
			Message: fmt.Sprintf("%d session(s) wait for the server's queue to empty, nothing was changed for them: run the same command again later", len(res.Waiting))}
	}
	res.Action = "done"
	return res, nil
}

// extractionFailing says why sending again now would fail once more: the
// hold jobs tick recorded, or the bank's latest finished retain failed
// with none completed since; "" otherwise (or when it cannot be read).
func extractionFailing(ctx context.Context, fl *retain.Flusher, d paths.Dirs, bank string) string {
	if hold := extractionHold(d, bank); hold != "" {
		return hold
	}
	failed, anyFailed, err := fl.Client.LatestOperation(ctx, bank, "failed", "retain")
	if err != nil || !anyFailed {
		return ""
	}
	completed, anyCompleted, err := fl.Client.LatestOperation(ctx, bank, "completed", "retain")
	if err != nil || anyCompleted && !failed.FinishedAt().After(completed.FinishedAt()) {
		return ""
	}
	return "the bank's latest extraction failed and none succeeded since"
}

// failedSessions names the sessions to send again, each with its latest
// failure: the conversation documents of the bank's failed retains
// updated since since, and those of the parts this PC gave up unconfirmed
// since then (telemetry). It counts them in res.
func failedSessions(ctx context.Context, fl *retain.Flusher, telemetryDir string, since time.Time, res *resendFailedResult) (map[string]time.Time, error) {
	named := map[string]time.Time{}
	note := func(doc string, at time.Time) {
		if sid, ok := retain.SessionOfDocument(doc); ok && at.After(named[sid]) {
			named[sid] = at
		}
	}
	ops, complete, err := fl.Client.QueuedOperationsOf(ctx, fl.Bank, "failed", failedPages)
	if err != nil {
		return nil, blocked(fmt.Errorf("the server's failed operations cannot be read: %w", err))
	}
	res.FailedRetains, res.Cut, res.GivenUp = 0, !complete, 0
	for _, op := range ops {
		if !strings.Contains(op.Type, "retain") || op.FinishedAt().Before(since) {
			continue
		}
		res.FailedRetains++
		note(op.DocumentID, op.FinishedAt())
	}
	ev, err := stats.Load(telemetryDir, since, now())
	if err != nil {
		return nil, blocked(fmt.Errorf("this PC's telemetry cannot be read: %w", err))
	}
	for _, e := range ev {
		if e.Event == "replace_unconfirmed" && !e.TS.Before(since) {
			res.GivenUp++
			note(e.Document, e.TS)
		}
	}
	return named, nil
}

func resendFailedText(r resendFailedResult) string {
	if r.Action == "refused" {
		return "" // the error says why
	}
	var b strings.Builder
	p := r.Plan
	fmt.Fprintf(&b, "since %s: %d failed retain(s) on the server, %d part(s) given up on this PC; %d session(s) named, %d not on this PC, %d already sent again\n",
		r.Since, r.FailedRetains, r.GivenUp, p.Named, p.NotHere, p.Done)
	if r.Cut {
		fmt.Fprintf(&b, "the server holds more failed operations than read (%d): the oldest were left out\n", failedPages*100)
	}
	for _, s := range p.Resends {
		names := make([]string, len(s.Projects))
		for i, n := range s.Projects {
			names[i] = safeProject(n)
		}
		verb := map[bool]string{true: "sent again", false: "send again"}[slices.Contains(r.Queued, s.Session)]
		fmt.Fprintf(&b, "%s session %s: %d record(s) to %s, %s", verb, s.Session, s.Records, strings.Join(names, ", "), estimateText(s.Estimate))
		if s.KeptOut > 0 {
			fmt.Fprintf(&b, "; %d record(s) kept out of memory are not sent", s.KeptOut)
		}
		b.WriteString("\n")
	}
	if len(p.Resends) > 0 {
		fmt.Fprintf(&b, "in all: %s\n", estimateText(p.Cost))
	}
	for _, h := range p.Held {
		fmt.Fprintf(&b, "left alone: session %s (%s)\n", h.Session, h.Reason)
	}
	if r.Backup != "" {
		fmt.Fprintf(&b, "verified backup %s\n", r.Backup)
	}
	if len(r.Queued) > 0 {
		fmt.Fprintf(&b, "%d session(s) queued: the next jobs tick sends them again\n", len(r.Queued))
	}
	for _, f := range r.Failed {
		fmt.Fprintf(&b, "failed %s\n", f)
	}
	if r.Note != "" {
		b.WriteString(r.Note + "\n")
	}
	if len(p.Resends) == 0 {
		b.WriteString("nothing to send again\n")
	}
	return b.String()
}
