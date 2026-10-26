package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/setup"
)

type reattributeFlags struct {
	plan    bool
	answers []string
}

func (f reattributeFlags) resume() string {
	cmd := "backsight retain reattribute"
	for _, a := range f.answers {
		cmd += " --answer " + shellWord(a)
	}
	return cmd
}

// reattributeOptions are jobs maintain's: it backs up first itself, and
// never sends anything again (that costs extraction).
type reattributeOptions struct {
	backedUp bool // a backup was just taken: none here
	freeOnly bool // re-tags only, the re-sends left pending
}

type reattributeResult struct {
	Bank string `json:"bank"`
	// Action: planned | nothing | done | resends_pending
	Action   string                   `json:"action"`
	Plan     retain.ReattributionPlan `json:"plan"`
	Backup   string                   `json:"backup,omitempty"` // the verified archive taken first
	Retagged []string                 `json:"retagged"`         // documents moved in place
	Queued   []string                 `json:"queued"`           // sessions to be sent again by the next flush
	Failed   []string                 `json:"failed,omitempty"` // "<document or session>: <why>"
	Note     string                   `json:"note,omitempty"`
}

func newRetainReattributeCmd(e *env) *cobra.Command {
	var f reattributeFlags
	cmd := &cobra.Command{
		Use:   "reattribute",
		Short: "File the memory already sent under the projects today's rules give",
		Long: `Judges every document sent from this machine against today's project rules.
A document whose whole content belongs to another project is re-tagged in
place, with no extraction (the server rebuilds its observations). A session whose documents mix projects is sent again per
project, which costs extraction: it waits for --answer resend=yes.
A verified backup comes first. Running it again does nothing more.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runReattribute(cmd.Context(), f, reattributeOptions{})
			return e.report("retain reattribute", res, func() string { return reattributeText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&f.plan, "plan", false, "list what would move and what a re-send would cost, change nothing")
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "answer a question: resend=yes|no, backup=done (a remote server, backed up on its host)")
	return cmd
}

// runReattribute is retain reattribute: plan, then (with writes to make) a
// verified backup, the maintenance lock, the plan again under it, the free
// re-tags, and the re-sends once answered.
func runReattribute(ctx context.Context, f reattributeFlags, opt reattributeOptions) (reattributeResult, error) {
	res := reattributeResult{Action: "planned", Retagged: []string{}, Queued: []string{}}
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
			return res, &output.Error{Code: output.Usage, Message: "unknown answer " + k + ": retain reattribute asks resend and backup"}
		}
	}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return res, err
	}
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	fl, err := newFlusher(d, home, cfg, sec, false)
	if err != nil {
		return res, err
	}
	res.Bank = cfg.Server.Bank
	prices := retain.PricesFor(cfg.LLM.Model, cfg.LLM.PriceInput, cfg.LLM.PriceCached, cfg.LLM.PriceOutput, cfg.LLM.Currency)
	plan := func() (retain.ReattributionPlan, map[string][]string, error) {
		docs, err := fl.Client.ListDocuments(ctx, cfg.Server.Bank)
		if err != nil {
			return retain.ReattributionPlan{}, nil, blocked(err)
		}
		tags := map[string][]string{}
		for _, doc := range docs {
			tags[doc.ID] = doc.Tags
		}
		return fl.PlanReattribution(docs, transcripts(home), prices), tags, nil
	}
	p, _, err := plan()
	res.Plan = p
	if err != nil || f.plan {
		return res, err
	}
	resend := answers["resend"] == "yes" && !opt.freeOnly
	if len(p.Retags) == 0 && (!resend || len(p.Resends) == 0) {
		fl.KeepDecisions(p)
		return finishReattribute(res, f, opt, answers)
	}
	if !opt.backedUp {
		b, err := createBackup(ctx, nil)
		if err != nil {
			return res, err
		}
		if b.Skipped != "" && answers["backup"] != "done" {
			return res, &output.Error{Code: output.NeedsUser, Resume: f.resume() + " --answer backup=done",
				Questions: []setup.Question{{ID: "backup", Required: true, Options: []setup.Option{{Value: "done", Label: "the server was backed up on its host"}},
					Prompt: "the server is remote, so no backup was taken here: back it up on its host, then answer backup=done"}},
				Message: "retain reattribute needs a backup first: " + b.Skipped + "; nothing was changed"}
		}
		if b.Skipped == "" {
			// The first backup made the identity: read the secrets again.
			if sec, err = config.LoadSecrets(d.SecretsFile()); err != nil {
				return res, blocked(err)
			}
			_, _, ids, err := backupKeys(d, cfg, sec, false)
			if err != nil {
				return res, err
			}
			v, err := verifyArchive(ctx, d, cfg, ids, b.Path)
			if err != nil || !v.OK {
				if err == nil {
					err = fmt.Errorf("the backup's counts differ from its manifest")
				}
				return res, &output.Error{Code: output.Blocked, Message: "the backup " + b.Path + " could not be verified, nothing was changed: " + err.Error()}
			}
			res.Backup = b.Path
		}
	}
	l, note, err := acquireMaintenance(d, "retain reattribute")
	if err != nil {
		return res, err
	}
	defer func() { _ = l.Release() }()
	res.Note = note
	// Under the lock: no flush runs, so the plan applied is the server's now.
	p, tags, err := plan()
	res.Plan = p
	if err != nil {
		return res, err
	}
	for _, r := range p.Retags {
		if err := fl.ApplyRetag(ctx, r, tags[r.Document]); err != nil {
			res.Failed = append(res.Failed, r.Document+": "+err.Error())
			continue
		}
		res.Retagged = append(res.Retagged, r.Document)
	}
	fl.KeepDecisions(p)
	if resend {
		for _, r := range p.Resends {
			if err := fl.ApplyResend(ctx, r); err != nil {
				res.Failed = append(res.Failed, r.Session+": "+err.Error())
				continue
			}
			res.Queued = append(res.Queued, r.Session)
		}
	}
	return finishReattribute(res, f, opt, answers)
}

// finishReattribute gives the run's outcome its exit code: 40 when a write
// failed (the same command finishes it), 10 when re-sends wait for an
// answer, else 0.
func finishReattribute(res reattributeResult, f reattributeFlags, opt reattributeOptions, answers map[string]string) (reattributeResult, error) {
	pending := len(res.Plan.Resends) > 0 && len(res.Queued) == 0
	switch {
	case len(res.Failed) > 0:
		res.Action = "partial"
		return res, &output.Error{Code: output.Partial, Resume: f.resume(),
			Message: fmt.Sprintf("%d write(s) failed (%s): run the same command again to finish", len(res.Failed), strings.Join(res.Failed, "; "))}
	case pending && answers["resend"] == "" && !opt.freeOnly:
		res.Action = "resends_pending"
		c := res.Plan.Cost
		return res, &output.Error{Code: output.NeedsUser, Resume: f.resume() + " --answer resend=yes",
			Questions: []setup.Question{{ID: "resend", Required: true, Options: []setup.Option{
				{Value: "yes", Label: "send them again per project", Cost: estimateText(c)},
				{Value: "no", Label: "leave them as they are"}},
				Prompt: fmt.Sprintf("%d session(s) hold documents that mix projects: send them again per project (their documents are deleted first)?", len(res.Plan.Resends))}},
			Message: fmt.Sprintf("%d session(s) wait for --answer resend=yes (%s); the moves in place are done", len(res.Plan.Resends), estimateText(c))}
	case pending:
		res.Action = "resends_pending"
	case len(res.Retagged)+len(res.Queued) == 0:
		res.Action = "nothing"
	default:
		res.Action = "done"
	}
	return res, nil
}

// estimateText: an estimate's tokens and, when prices are known, cost.
func estimateText(e retain.Estimate) string {
	s := fmt.Sprintf("about %d input and %d output tokens", e.InputTokens, e.OutputTokens)
	if e.Cost > 0 {
		s += fmt.Sprintf(", about %.2f", e.Cost)
	}
	return s
}

func reattributeText(r reattributeResult) string {
	var b strings.Builder
	p := r.Plan
	fmt.Fprintf(&b, "%d document(s) read, %d judged against this machine's transcripts, %d unchanged\n", p.Documents, p.Judged, p.Unchanged)
	verb := map[bool]string{true: "moved", false: "would move"}[r.Action != "planned"]
	for _, m := range p.Retags {
		fmt.Fprintf(&b, "%s %s: %s → %s (in place, no extraction)\n", verb, m.Document, safeProject(m.From), safeProject(m.To))
	}
	for _, s := range p.Resends {
		names := make([]string, len(s.Projects))
		for i, n := range s.Projects {
			names[i] = safeProject(n)
		}
		fmt.Fprintf(&b, "re-send session %s (%s): %d record(s) to %s, %s", s.Session, s.Reason, s.Records, strings.Join(names, ", "), estimateText(s.Estimate))
		if s.KeptOut > 0 {
			fmt.Fprintf(&b, "; %d record(s) now kept out are not sent again", s.KeptOut)
		}
		b.WriteString("\n")
	}
	if len(p.Resends) > 0 {
		fmt.Fprintf(&b, "re-sends in all: %s\n", estimateText(p.Cost))
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
	if len(p.Retags)+len(p.Resends) == 0 {
		b.WriteString("nothing to move\n")
	}
	return b.String()
}
