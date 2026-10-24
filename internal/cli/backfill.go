package cli

import (
	"context"
	"fmt"
	"io"
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
	backfillQueueLimit = 2                // wait until the server queue is this short
	tickQueueWait      = 60 * time.Second // a tick does not wait longer on the queue
	finalQueueWait     = 15 * time.Minute // the closing measurement waits at most this long
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
	NotesOnly int             `json:"notes_only"`
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
				return e.emit("retain backfill", res, func() string { return backfillText(res) })
			}
			if cmd.Flags().Changed("max-cost") && !prices.Known() {
				return &output.Error{Code: output.Usage, Message: "no price is known for model " + cfg.LLM.Model + ": cap with --max-tokens, or set [llm] price_input and price_output"}
			}
			queue := make([]string, 0, len(cands))
			for _, c := range cands {
				queue = append(queue, c.Path)
			}
			filters := fl.since + "|" + fl.project + "|" + fl.dir
			capGiven := cmd.Flags().Changed("max-cost") || cmd.Flags().Changed("max-tokens")
			// The lock comes first: a running backfill would write its old
			// cap and queue back over this invocation's changes.
			release, err := acquireBackfill(d)
			if err != nil {
				return err
			}
			defer release()
			var startErr error
			err = state.Update(d, "retain backfill", func(s *state.State) error {
				b := s.Backfill
				active := b != nil && len(b.Sessions) > 0
				switch {
				case !active:
					// A new backfill: baseline of today's spend, which must be readable.
					day := now().UTC().Format("2006-01-02")
					days, err := f.Client.LLMTokensByDay(cmd.Context(), cfg.Server.Bank, "30d")
					if err != nil {
						startErr = fmt.Errorf("cannot read the server's LLM usage, so the spending cap could not work: %w", err)
						return nil
					}
					t := days[day]
					b = &state.Backfill{StartedAt: now().UTC(), StartDay: day, StartBase: [4]int64{t.Input, t.Cached, t.Output, t.Thoughts},
						Currency: prices.Currency, MaxCost: plan.MaxCost, MaxTokens: plan.MaxTokens}
					b.Sessions, b.Total, b.Filters = queue, len(queue), filters
				case b.Filters != filters:
					// Other filters: a new queue, the spend so far still counts.
					b.Sessions, b.Total, b.Done, b.Filters = queue, len(queue), 0, filters
				}
				if capGiven {
					b.MaxCost, b.MaxTokens = plan.MaxCost, plan.MaxTokens
				}
				if b.MaxCost <= 0 && b.MaxTokens <= 0 {
					b.MaxCost, b.MaxTokens = plan.MaxCost, plan.MaxTokens
				}
				if b.Paused == "budget" && !capGiven {
					startErr = &output.Error{Code: output.Partial, Message: fmt.Sprintf("backfill paused: budget reached (%.4f %s spent); rerun with a higher --max-cost or --max-tokens", b.SpentCost, b.Currency)}
					return nil
				}
				b.Paused = ""
				b.AllowSubscription = fl.allowSubscription
				s.Backfill = b
				return nil
			})
			if err != nil {
				return blocked(err)
			}
			if startErr != nil {
				return blocked(startErr)
			}
			if !e.json { // the plan before a long run (decision 0051)
				_, _ = io.WriteString(e.stdout, backfillText(backfillResult{Plan: plan}))
			}
			res, failed := runBackfill(cmd.Context(), f, d, cfg, prices, false)
			res.Plan = plan
			return e.report("retain backfill", res, func() string { return backfillRunText(res) }, failed)
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
		case "notes":
			plan.NotesOnly++
			plan.Notes += c.Notes
			send = append(send, c)
		}
		if c.Status == "send" || c.Status == "notes" {
			for _, n := range c.NoteEst { // each save is its own document and extraction
				plan.Estimate = plan.Estimate.Add(retain.EstimateSession(n, prices))
			}
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

// errBackfillBusy: another backfill holds the lock.
var errBackfillBusy = &output.Error{Code: output.Blocked, Message: "another backfill is running"}

// acquireBackfill takes the backfill lock or reports another backfill.
func acquireBackfill(d paths.Dirs) (func(), error) {
	l, err := state.TryAcquire(filepath.Join(d.LocksDir(), "job-backfill.lock"), "retain backfill")
	if err != nil {
		return nil, errBackfillBusy
	}
	return func() { _ = l.Release() }, nil
}

// runBackfill sends the queued sessions, one at a time, measuring the
// spend before each (decision 0051). inTick: at most tickBackfillLimit
// sessions and a bounded wait on the server's queue.
// The caller holds the backfill lock (acquireBackfill).
func runBackfill(ctx context.Context, f *retain.Flusher, d paths.Dirs, cfg config.Config, prices retain.Prices, inTick bool) (backfillResult, error) {
	var res backfillResult
	ob := notesOutbox(d)
	pause := func(reason string, msg string) (backfillResult, error) {
		if err := state.Update(d, "retain backfill", func(s *state.State) error {
			if s.Backfill != nil {
				s.Backfill.Paused = reason
			}
			return nil
		}); err != nil {
			return res, blocked(err)
		}
		res.Paused = reason
		return res, &output.Error{Code: output.Partial, Message: "backfill paused: " + msg}
	}
	sent := 0
	for {
		st, err := state.Load(d.StateFile())
		if err != nil {
			return res, blocked(err)
		}
		if st.Backfill == nil {
			return res, blocked(fmt.Errorf("no backfill in progress"))
		}
		b := *st.Backfill
		res.Done, res.Total, res.Spent, res.Tokens = b.Done, b.Total, b.SpentCost, b.SpentTokens
		if len(b.Sessions) == 0 {
			if !inTick { // the last sessions' extraction is spend too
				_ = waitQueue(ctx, f.Client, cfg.Server.Bank, 0, finalQueueWait)
				if err := measure(ctx, f.Client, cfg.Server.Bank, &b, prices); err == nil {
					_ = state.Update(d, "retain backfill", func(s *state.State) error { s.Backfill = &b; return nil })
					res.Spent, res.Tokens = b.SpentCost, b.SpentTokens
				}
			}
			res.Finished = true
			return res, nil
		}
		if inTick && sent >= tickBackfillLimit {
			return res, nil
		}
		if ctx.Err() != nil {
			return pause("interrupted", "interrupted; rerun backsight retain backfill to continue")
		}
		wait := time.Duration(0)
		if inTick {
			wait = tickQueueWait
		}
		if err := waitQueue(ctx, f.Client, cfg.Server.Bank, backfillQueueLimit, wait); err != nil {
			if ctx.Err() != nil {
				return pause("interrupted", "interrupted; rerun backsight retain backfill to continue")
			}
			if inTick {
				return res, nil // the server is busy: the next tick tries again
			}
			return pause("server", "cannot read the server's queue: "+err.Error())
		}
		if err := measure(ctx, f.Client, cfg.Server.Bank, &b, prices); err != nil {
			return pause("measure", "cannot measure the spend, so the cap cannot be enforced: "+err.Error())
		}
		over := (b.MaxCost > 0 && b.SpentCost >= b.MaxCost) || (b.MaxTokens > 0 && b.SpentTokens >= b.MaxTokens)
		if err := state.Update(d, "retain backfill", func(s *state.State) error { s.Backfill = &b; return nil }); err != nil {
			return res, blocked(err)
		}
		res.Spent, res.Tokens = b.SpentCost, b.SpentTokens
		if over {
			return pause("budget", fmt.Sprintf("budget reached (%.4f %s, %d tokens); rerun with a higher --max-cost or --max-tokens", b.SpentCost, b.Currency, b.SpentTokens))
		}
		path := b.Sessions[0]
		r := f.FlushFile(ctx, path, retain.Options{Force: true, IgnoreSince: true})
		res.Results = append(res.Results, r)
		switch r.Status {
		case retain.Sent, retain.Nothing, retain.OptedOut, retain.Quarantined, retain.Held:
		default:
			if r.Stop {
				return pause("server", r.Detail+"; rerun once the server is back")
			}
			// Locked, maintenance or a transient read error: keep the session.
			if inTick {
				return res, nil
			}
			return res, &output.Error{Code: output.Partial, Message: "backfill stopped at " + r.Session + " (" + r.Status + ": " + r.Detail + "); rerun to continue"}
		}
		if _, recs := f.Survey(path); len(recs) > 0 {
			sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			for _, n := range f.Notes(sid, recs) {
				if _, err := ob.Append(n); err != nil {
					return res, blocked(err)
				}
			}
			_, _ = ob.Flush(ctx, f.Client, cfg.Server.Bank, f.MachineID) // kept notes go out with the next tick
			_ = retain.MarkNotesReplayed(d.CursorsDir(), sid)            // they are in the outbox: never queued again
		}
		if err := state.Update(d, "retain backfill", func(s *state.State) error {
			if s.Backfill != nil && len(s.Backfill.Sessions) > 0 && s.Backfill.Sessions[0] == path {
				s.Backfill.Sessions = s.Backfill.Sessions[1:]
				s.Backfill.Done++
			}
			return nil
		}); err != nil {
			return res, blocked(err)
		}
		sent++
	}
}

// measure sets the spend from the bank's per-day token sums: the start
// day's increase over its baseline plus every later day. Other writers to
// the bank count too (conservative).
func measure(ctx context.Context, c *hindsight.Client, bank string, b *state.Backfill, prices retain.Prices) error {
	days, err := c.LLMTokensByDay(ctx, bank, "30d")
	if err != nil {
		return err
	}
	var sum [4]int64
	for day, t := range days {
		v := [4]int64{t.Input, t.Cached, t.Output, t.Thoughts}
		switch {
		case day > b.StartDay:
		case day == b.StartDay:
			for i := range v {
				v[i] = max(v[i]-b.StartBase[i], 0)
			}
		default:
			continue
		}
		for i := range v {
			sum[i] += v[i]
		}
	}
	b.SpentTokens = sum[0] + sum[2] + sum[3]
	b.SpentCost = prices.Cost(sum[0], sum[1], sum[2], sum[3])
	return nil
}

// waitQueue waits until the bank has at most limit queued or running
// operations; maxWait > 0 bounds the wait (errQueueBusy).
func waitQueue(ctx context.Context, c *hindsight.Client, bank string, limit int, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for {
		n, err := c.QueuedOperations(ctx, bank)
		if err != nil {
			return err
		}
		if n <= limit {
			return nil
		}
		if maxWait > 0 && time.Now().After(deadline) {
			return errQueueBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backfillPoll):
		}
	}
}

var errQueueBusy = fmt.Errorf("the server's queue stays busy")

func backfillText(r backfillResult) string {
	var b strings.Builder
	p := r.Plan
	capText := fmt.Sprintf("%.2f %s", p.MaxCost, p.Currency)
	if p.MaxCost == 0 {
		capText = fmt.Sprintf("%d tokens", p.MaxTokens)
	}
	fmt.Fprintf(&b, "%d session(s) to send (%d already sent, %d opted out), %d note(s) to replay (%d from sessions already sent)\n", p.Send, p.Sent, p.OptedOut, p.Notes, p.NotesOnly)
	fmt.Fprintf(&b, "estimate: %.1fk characters, %dk input and %dk output tokens, %.2f %s, about %s; cap %s\n",
		float64(p.Estimate.Chars)/1000, p.Estimate.InputTokens/1000, p.Estimate.OutputTokens/1000, p.Estimate.Cost, p.Currency,
		p.Estimate.Duration.Round(time.Minute), capText)
	return b.String()
}

// backfillRunText is the outcome of a run, printed after its plan.
func backfillRunText(r backfillResult) string {
	var b strings.Builder
	p := r.Plan
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
