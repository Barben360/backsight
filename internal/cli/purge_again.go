package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
)

// A purge clears the observations drawn from the purged facts, and the
// server then consolidates their other sources again: LLM work. Who pays
// for it decides whether a look purges unattended.
const (
	// subscriptionHold: the claude-code provider runs it on the user's
	// subscription.
	subscriptionHold = "the purge makes the server consolidate again, which the claude-code provider runs on your Claude subscription: run backsight retain purge-again --allow-subscription"
	// remoteHold: a server this machine does not run consolidates with its
	// owner's provider, which this machine cannot see.
	remoteHold = "the purge makes the server consolidate again, with the provider of the server's owner (this machine does not run it): run backsight retain purge-again --allow-subscription once its owner agrees"
)

// guardHold is the hold of an unattended look with this configuration: a
// remote server, whatever this machine's provider, or the claude-code
// provider; "" when the look may purge.
func guardHold(cfg config.Config) string {
	switch {
	case cfg.Server.Mode == "remote":
		return remoteHold
	case cfg.LLM.Provider == "claude-code":
		return subscriptionHold
	}
	return ""
}

// guardTickBudget bounds jobs tick's look: it runs after the sends, and
// the next tick picks up what it did not finish. A variable for tests.
var guardTickBudget = 20 * time.Second

// guardTick is jobs tick's look for recreated purged documents (backsight
// issue 5), after the sends and notes: only the documents created since
// the last look, within guardTickBudget (jobs maintain looks at every
// tombstone). It returns a line for the report, "" when nothing was found.
func guardTick(ctx context.Context, f *retain.Flusher, cfg config.Config) string {
	ctx, cancel := context.WithTimeout(ctx, guardTickBudget)
	defer cancel()
	g, err := f.GuardPurges(ctx, retain.GuardOptions{Hold: guardHold(cfg)})
	if err != nil {
		return "not looked at: " + err.Error()
	}
	return guardLine(g)
}

// guardMaintain is jobs maintain's daily full look: every tombstone,
// after making this machine's and the control bank's the same.
func guardMaintain(ctx context.Context, d paths.Dirs, home string) string {
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return "not looked at: " + err.Error()
	}
	if !cfg.Components.Write {
		return ""
	}
	f, err := newFlusher(d, home, cfg, sec, false)
	if err != nil {
		return "not looked at: " + err.Error()
	}
	_ = f.SyncTombstones(ctx) // unreadable: this machine's own still count
	g, err := f.GuardPurges(ctx, retain.GuardOptions{Full: true, Hold: guardHold(cfg)})
	if err != nil {
		return "not looked at: " + err.Error()
	}
	return guardLine(g)
}

func guardLine(g retain.Guarded) string {
	var parts []string
	if n := len(g.Purged.Documents); n > 0 {
		parts = append(parts, fmt.Sprintf("%d recreated purged document(s) purged again (%s)", n, strings.Join(g.Purged.Documents, ", ")))
	}
	if n := len(g.Waiting); n > 0 {
		parts = append(parts, fmt.Sprintf("%d recreated purged document(s) waiting (%s: %s)", n, g.Waiting[0].Document, g.Waiting[0].Why))
	}
	return strings.Join(parts, "; ")
}

type purgeAgainResult struct {
	Bank string `json:"bank"`
	// Action: nothing | planned | needs_consent | purged | waiting
	Action string `json:"action"`
	retain.Guarded
}

func newRetainPurgeAgainCmd(e *env) *cobra.Command {
	var plan, allowSubscription bool
	cmd := &cobra.Command{
		Use:   "purge-again",
		Short: "Purge again the purged documents an older client wrote again",
		Long: `Looks for every purged document (its tombstone) that the server holds again:
an older backsight, or any client unaware of purges, appended to its id,
and Hindsight recreated it. Each is purged again as forget --purge does:
the observations of its facts cleared, its facts retired, the document
deleted, then the briefing pages of its projects. The turns that client
sent after the purge go again to the session's current document at the
next send. jobs tick and jobs maintain do it unattended, except with the
claude-code provider or a server this machine does not run: then this
command does it, with --allow-subscription (the server consolidates again
after a purge). A document an append is queued for waits for it. A second
run does nothing.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runPurgeAgain(cmd.Context(), plan, allowSubscription)
			return e.report("retain purge-again", res, func() string { return purgeAgainText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "list the purged documents the server holds again, change nothing")
	cmd.Flags().BoolVar(&allowSubscription, "allow-subscription", false, "with the claude-code provider or a server this machine does not run, let the server's consolidation after the purge run")
	return cmd
}

func runPurgeAgain(ctx context.Context, plan, allowSubscription bool) (purgeAgainResult, error) {
	res := purgeAgainResult{Action: "nothing"}
	if err := maintenanceRefusal(); err != nil {
		return res, err
	}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return res, err
	}
	res.Bank = cfg.Server.Bank
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	f, err := newFlusher(d, home, cfg, sec, false)
	if err != nil {
		return res, err
	}
	opt := retain.GuardOptions{Full: true, Manual: true}
	// Another machine's purges count too: they are in the control bank. A
	// plan reads them without writing them anywhere.
	if plan {
		remote, err := retain.ReadTombstones(ctx, f.Client, cfg.Server.Bank)
		if err != nil {
			return res, blocked(err)
		}
		opt.Plan, opt.Remote = true, remote
	} else if err := f.SyncTombstones(ctx); err != nil {
		return res, blocked(err)
	}
	hold := guardHold(cfg)
	gated := hold != "" && !allowSubscription
	if gated && !plan {
		opt.Hold = hold
	}
	res.Guarded, err = f.GuardPurges(ctx, opt)
	if err != nil {
		return res, &output.Error{Code: output.Blocked, Message: "cannot look for recreated purged documents now: " + err.Error()}
	}
	switch {
	case len(res.Waiting) == 0 && res.Purged.Empty():
		return res, nil
	case plan:
		res.Action = "planned"
		return res, nil
	case gated:
		res.Action = "needs_consent"
		return res, &output.Error{Code: output.NeedsUser, Resume: "backsight retain purge-again --allow-subscription",
			Message: strings.Replace(hold, "run backsight retain purge-again --allow-subscription", "add --allow-subscription to go on", 1) + "; nothing was changed"}
	case len(res.Waiting) > 0:
		res.Action = "waiting"
		return res, &output.Error{Code: output.Partial, Resume: "backsight retain purge-again",
			Message: fmt.Sprintf("%d recreated purged document(s) wait (%s: %s): run it again later; jobs tick retries too", len(res.Waiting), res.Waiting[0].Document, res.Waiting[0].Why)}
	}
	res.Action = "purged"
	return res, nil
}

func purgeAgainText(r purgeAgainResult) string {
	var b strings.Builder
	switch r.Action {
	case "nothing":
		fmt.Fprintf(&b, "no purged document of bank %s is on the server again (%d checked)\n", r.Bank, r.Checked)
		return b.String()
	case "planned", "needs_consent":
		fmt.Fprintf(&b, "purged documents of bank %s on the server again, to purge again:\n", r.Bank)
	default:
		if !r.Purged.Empty() {
			fmt.Fprintf(&b, "purged again: %s\n", r.Purged.String())
		}
	}
	for _, w := range r.Waiting {
		fmt.Fprintf(&b, "- %s (session %s)", w.Document, w.Session)
		if r.Action == "waiting" {
			fmt.Fprintf(&b, ": %s", w.Why)
		}
		b.WriteString("\n")
	}
	return b.String()
}
