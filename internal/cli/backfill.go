package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// Pacing seams (decision 0051); tests shorten them.
var (
	backfillPoll       = 5 * time.Second
	backfillQueueLimit = 20
	tickBackfillLimit  = 3
)

type backfillFlags struct {
	plan, allowSubscription bool
	since, project, dir     string
	maxCost                 float64
	maxTokens               int64
}

type backfillPlan struct {
	Send      int             `json:"send"`
	Sent      int             `json:"already_sent"`
	OptedOut  int             `json:"opted_out"`
	Notes     int             `json:"notes"`
	Estimate  retain.Estimate `json:"estimate"`
	Currency  string          `json:"currency"`
	MaxCost   float64         `json:"max_cost,omitempty"`
	MaxTokens int64           `json:"max_tokens,omitempty"`
}

type backfillResult struct {
	Plan     backfillPlan    `json:"plan"`
	Done     int             `json:"done"`
	Total    int             `json:"total"`
	Spent    float64         `json:"spent_cost"`
	Tokens   int64           `json:"spent_tokens"`
	Paused   string          `json:"paused,omitempty"`
	Results  []retain.Result `json:"results,omitempty"`
	Finished bool            `json:"finished"`
}

func newRetainBackfillCmd(e *env) *cobra.Command {
	var fl backfillFlags
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Send past sessions (oldest first) and replay their explicit saves, within a spending cap",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, home, err := machine()
			if err != nil {
				return err
			}
			if cfg.LLM.Provider == "claude-code" && !fl.allowSubscription && !fl.plan {
				return &output.Error{Code: output.Usage, Message: "the server extracts with your Claude subscription: backfill would replay months of sessions unattended (decision 0043); rerun with --allow-subscription to accept"}
			}
			f, err := newFlusher(d, home, cfg, sec, false)
			if err != nil {
				return err
			}
			prices := retain.PricesFor(cfg.LLM.Model, cfg.LLM.PriceInput, cfg.LLM.PriceCached, cfg.LLM.PriceOutput, cfg.LLM.Currency)
			files := transcripts(home)
			if fl.dir != "" {
				files, _ = filepath.Glob(filepath.Join(fl.dir, "*", "*.jsonl"))
			}
			cands, plan, err := surveyAll(f, files, fl, prices)
			if err != nil {
				return err
			}
			res := backfillResult{Plan: plan}
			if fl.plan {
				return e.emit("retain backfill", res, func() string { return backfillText(res, true) })
			}
			queue := make([]string, 0, len(cands))
			for _, c := range cands {
				queue = append(queue, c.Path)
			}
			err = state.Update(d, "retain backfill", func(s *state.State) error {
				b := s.Backfill
				if b == nil || len(b.Sessions) == 0 || cmd.Flags().Changed("since") || cmd.Flags().Changed("project") {
					b = &state.Backfill{StartedAt: now().UTC(), Total: len(queue), Sessions: queue, Currency: prices.Currency}
					if t, err := f.Client.LLMTokens(cmd.Context(), cfg.Server.Bank, "30d"); err == nil {
						b.Last = [4]int64{t.Input, t.Cached, t.Output, t.Thoughts}
					}
				}
				b.MaxCost, b.MaxTokens = plan.MaxCost, plan.MaxTokens
				b.Paused = ""
				s.Backfill = b
				return nil
			})
			if err != nil {
				return blocked(err)
			}
			res, failed := runBackfill(cmd.Context(), f, d, cfg, prices, 0)
			res.Plan = plan
			return e.report("retain backfill", res, func() string { return backfillText(res, false) }, failed)
		},
	}
	cmd.Flags().BoolVar(&fl.plan, "plan", false, "show sessions, size, expected cost and duration without sending")
	cmd.Flags().StringVar(&fl.since, "since", "", "only sessions started on or after this date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&fl.project, "project", "", "only sessions of this project")
	cmd.Flags().Float64Var(&fl.maxCost, "max-cost", 0, "spending cap in the provider's currency (default 1.5 × the estimate)")
	cmd.Flags().Int64Var(&fl.maxTokens, "max-tokens", 0, "token cap, for models without a price")
	cmd.Flags().BoolVar(&fl.allowSubscription, "allow-subscription", false, "accept backfill on the Claude subscription lane")
	cmd.Flags().StringVar(&fl.dir, "projects-dir", "", "read transcripts from another Claude Code profile's projects directory")
	return cmd
}

// surveyAll lists the sessions to send, oldest first, and the plan.
func surveyAll(f *retain.Flusher, files []string, fl backfillFlags, prices retain.Prices) ([]retain.Candidate, backfillPlan, error) {
	var since time.Time
	if fl.since != "" {
		t, err := time.Parse("2006-01-02", fl.since)
		if err != nil {
			return nil, backfillPlan{}, &output.Error{Code: output.Usage, Message: "--since wants YYYY-MM-DD"}
		}
		since = t
	}
	plan := backfillPlan{Currency: prices.Currency}
	var send []retain.Candidate
	for _, file := range files {
		c, _ := f.Survey(file)
		if c.Status == "empty" || (!since.IsZero() && c.Start.Before(since)) || (fl.project != "" && c.Project != fl.project) {
			continue
		}
		switch c.Status {
		case "sent":
			plan.Sent++
		case "opted_out":
			plan.OptedOut++
		case "send":
			plan.Send++
			plan.Notes += c.Notes
			plan.Estimate = plan.Estimate.Add(retain.EstimateSession(c.Chars, prices))
			send = append(send, c)
		}
	}
	sort.Slice(send, func(i, j int) bool { return send[i].Start.Before(send[j].Start) })
	switch {
	case fl.maxCost > 0:
		plan.MaxCost = fl.maxCost
	case fl.maxTokens > 0:
		plan.MaxTokens = fl.maxTokens
	case prices.Known():
		plan.MaxCost = 1.5 * plan.Estimate.Cost
	default:
		plan.MaxTokens = (plan.Estimate.InputTokens + plan.Estimate.OutputTokens) * 3 / 2
	}
	return send, plan, nil
}

// runBackfill sends the queued sessions (limit > 0: at most that many),
// checking the measured spend and the server queue before each.
func runBackfill(ctx context.Context, f *retain.Flusher, d paths.Dirs, cfg config.Config, prices retain.Prices, limit int) (backfillResult, error) {
	var res backfillResult
	ob := notesOutbox(d)
	sent := 0
	for {
		st, err := state.Load(d.StateFile())
		if err != nil || st.Backfill == nil {
			return res, blocked(fmt.Errorf("no backfill in progress"))
		}
		b := *st.Backfill
		res.Done, res.Total, res.Spent, res.Tokens = b.Done, b.Total, b.SpentCost, b.SpentTokens
		if len(b.Sessions) == 0 {
			res.Finished = true
			return res, nil
		}
		if limit > 0 && sent >= limit {
			return res, nil
		}
		// Measured spend since the last check (decision 0051).
		if t, err := f.Client.LLMTokens(ctx, cfg.Server.Bank, "30d"); err == nil {
			now := [4]int64{t.Input, t.Cached, t.Output, t.Thoughts}
			var delta [4]int64
			for i := range now {
				if delta[i] = now[i] - b.Last[i]; delta[i] < 0 {
					delta[i] = 0 // an old day left the 30-day window
				}
			}
			b.Last = now
			b.SpentTokens += delta[0] + delta[2] + delta[3]
			b.SpentCost += prices.Cost(delta[0], delta[1], delta[2], delta[3])
		}
		over := (b.MaxCost > 0 && b.SpentCost >= b.MaxCost) || (b.MaxTokens > 0 && b.SpentTokens >= b.MaxTokens)
		if over {
			b.Paused = "budget"
		}
		if err := state.Update(d, "retain backfill", func(s *state.State) error { s.Backfill = &b; return nil }); err != nil {
			return res, blocked(err)
		}
		res.Spent, res.Tokens = b.SpentCost, b.SpentTokens
		if over {
			res.Paused = "budget"
			return res, &output.Error{Code: output.Partial, Message: "backfill paused: budget reached; rerun with a higher --max-cost (or --max-tokens) to continue"}
		}
		if err := waitQueue(ctx, f.Client, cfg.Server.Bank); err != nil {
			return res, blocked(err)
		}
		path := b.Sessions[0]
		r := f.FlushFile(ctx, path, retain.Options{Force: true, IgnoreSince: true})
		res.Results = append(res.Results, r)
		if r.Stop {
			_ = state.Update(d, "retain backfill", func(s *state.State) error { s.Backfill.Paused = "server"; return nil })
			res.Paused = "server"
			return res, &output.Error{Code: output.Partial, Message: "backfill paused: " + r.Detail + "; rerun once the server is back"}
		}
		if r.Status == retain.Sent || r.Status == retain.Nothing || r.Status == retain.OptedOut {
			if _, recs := f.Survey(path); len(recs) > 0 && r.Status != retain.OptedOut {
				sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
				for _, n := range retain.NotesFromRecords(cfg.Server.Bank, sid, f.Resolver.Project(recs[0].CWD), recs) {
					if _, err := ob.Append(n); err != nil {
						return res, blocked(err)
					}
				}
				_, _ = ob.Flush(ctx, f.Client, cfg.Server.Bank, f.MachineID)
			}
		}
		_ = state.Update(d, "retain backfill", func(s *state.State) error {
			s.Backfill.Sessions = s.Backfill.Sessions[1:]
			s.Backfill.Done++
			return nil
		})
		sent++
	}
}

// waitQueue waits while the bank has more than backfillQueueLimit queued
// or running operations, so spend is measured as it happens.
func waitQueue(ctx context.Context, c *hindsight.Client, bank string) error {
	for {
		n, err := c.QueuedOperations(ctx, bank)
		if err != nil {
			return err
		}
		if n <= backfillQueueLimit {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backfillPoll):
		}
	}
}

func backfillText(r backfillResult, plan bool) string {
	var b strings.Builder
	p := r.Plan
	capText := fmt.Sprintf("%.2f %s", p.MaxCost, p.Currency)
	if p.MaxCost == 0 {
		capText = fmt.Sprintf("%d tokens", p.MaxTokens)
	}
	fmt.Fprintf(&b, "%d session(s) to send (%d already sent, %d opted out), %d note(s) to replay\n", p.Send, p.Sent, p.OptedOut, p.Notes)
	fmt.Fprintf(&b, "estimate: %.1fk characters, %dk input and %dk output tokens, %.2f %s, about %s; cap %s\n",
		float64(p.Estimate.Chars)/1000, p.Estimate.InputTokens/1000, p.Estimate.OutputTokens/1000, p.Estimate.Cost, p.Currency,
		p.Estimate.Duration.Round(time.Minute), capText)
	if plan {
		return b.String()
	}
	fmt.Fprintf(&b, "done %d/%d, spent %.4f %s (%d tokens)", r.Done, r.Total, r.Spent, p.Currency, r.Tokens)
	if r.Paused != "" {
		fmt.Fprintf(&b, ", paused: %s", r.Paused)
	}
	if r.Finished {
		b.WriteString(", finished")
	}
	b.WriteString("\n")
	return b.String()
}
