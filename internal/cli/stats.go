package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/stats"
)

// extraction is the server's LLM use over the period (Hindsight's traces).
type extraction struct {
	Input    int64   `json:"input_tokens"`
	Cached   int64   `json:"cached_tokens"`
	Output   int64   `json:"output_tokens"`
	Cost     float64 `json:"cost,omitempty"`
	Currency string  `json:"currency,omitempty"`
	Days     int     `json:"days"`            // the server keeps 30 days of daily buckets
	Error    string  `json:"error,omitempty"` // the server could not be read
}

type statsResult struct {
	Days int `json:"days"`
	// Window: set by --since, the report is [from, to) instead of the
	// last Days days.
	Window bool `json:"window,omitempty"`
	stats.Report
	Extraction extraction         `json:"extraction"`
	Parity     []stats.ParityLine `json:"parity,omitempty"`
}

// newStatsCmd is `stats [days]` (SPEC §15): the C12 report from local
// telemetry, and the server's extraction tokens and cost. --since/--until
// fix the window, --parity compares it with C12 (§1.2, decision 0065).
func newStatsCmd(e *env) *cobra.Command {
	var since, until string
	var parity bool
	cmd := &cobra.Command{
		Use:   "stats [days]",
		Short: "Show how memory was used over the last days (default 7): recalls, searches, credits, detector, cost",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			days := 7
			if len(args) == 1 {
				if since != "" || until != "" {
					return &output.Error{Code: output.Usage, Message: "give either [days] or --since/--until"}
				}
				n, err := strconv.Atoi(args[0])
				if err != nil || n < 1 || n > 365 {
					return &output.Error{Code: output.Usage, Message: "days: a whole number from 1 to 365"}
				}
				days = n
			}
			to := now()
			from := to.Add(-time.Duration(days) * 24 * time.Hour)
			if until != "" {
				if since == "" {
					return &output.Error{Code: output.Usage, Message: "--until needs --since"}
				}
				t, err := parseWhen(until)
				if err != nil {
					return &output.Error{Code: output.Usage, Message: "--until: " + err.Error()}
				}
				to = t
			}
			if since != "" {
				t, err := parseWhen(since)
				if err != nil {
					return &output.Error{Code: output.Usage, Message: "--since: " + err.Error()}
				}
				if !t.Before(to) || to.Sub(t) > 366*24*time.Hour {
					return &output.Error{Code: output.Usage, Message: "--since must be before --until (or now), at most 366 days before"}
				}
				from = t
				days = int((to.Sub(from) + 24*time.Hour - 1) / (24 * time.Hour))
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			ev, err := stats.Load(d.TelemetryDir(), from, to)
			if err != nil {
				return blocked(err)
			}
			res := statsResult{Days: days, Window: since != "", Report: stats.Compute(ev, from, to)}
			res.Feedback.SharedMachines = sharedMachines(d)
			res.Extraction = extractionOver(cmd, from, to)
			if parity {
				res.Parity = stats.Parity(res.Report)
			}
			return e.emit("stats", res, func() string { return statsText(res) })
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "start of the window (RFC 3339, YYYY-MM-DDTHH:MMZ or YYYY-MM-DD, UTC) instead of [days]")
	cmd.Flags().StringVar(&until, "until", "", "end of the window (default now), with --since")
	cmd.Flags().BoolVar(&parity, "parity", false, "compare with the baseline measured on the Hindsight integration backsight replaced")
	_ = cmd.Flags().MarkHidden("parity")
	return cmd
}

// sharedMachines is how many other machines' counts recall weighs with
// this one's: those of the shared cache in use now. 0 when feedback or
// share_ledger is off, or the cache cannot be read.
func sharedMachines(d paths.Dirs) int {
	cfg, _, err := loadServerConfig()
	if err != nil || !cfg.Feedback.Enabled || !cfg.Feedback.ShareLedger {
		return 0
	}
	c, err := feedback.LoadSharedCache(d.FeedbackDir())
	if err != nil {
		return 0
	}
	docs, _ := c.Parsed()
	self, _ := retain.MachineID(d.State)
	return feedback.SharedMachines(docs, self, now())
}

// parseWhen reads a window bound; a date alone is midnight UTC.
func parseWhen(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time (RFC 3339, YYYY-MM-DDTHH:MMZ or YYYY-MM-DD)", s)
}

// extractionOver reads Hindsight's LLM token use over the window, from its
// daily buckets: the server answers 1d or 30d only (anything else silently
// gives 7 days), so 30d is asked and the days outside [from, to] dropped.
// A server that cannot be read leaves the rest of the report intact.
func extractionOver(cmd *cobra.Command, from, to time.Time) extraction {
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return extraction{Error: err.Error()}
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 10*time.Second)
	if err != nil {
		return extraction{Error: err.Error()}
	}
	byDay, err := cl.LLMTokensByDay(cmd.Context(), cfg.Server.Bank, "30d")
	if err != nil {
		return extraction{Error: err.Error()}
	}
	// The window's days are [from, to): a to at midnight leaves its day out.
	// The server's 30 buckets count back from today, not from to.
	firstDay := from.UTC().Truncate(24 * time.Hour)
	lastDay := to.UTC().Add(-time.Nanosecond).Truncate(24 * time.Hour)
	if kept := now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -29); firstDay.Before(kept) {
		firstDay = kept
	}
	x := extraction{Days: max(0, int(lastDay.Sub(firstDay)/(24*time.Hour))+1)}
	first, last := firstDay.Format("2006-01-02"), lastDay.Format("2006-01-02")
	var thoughts int64
	for day, t := range byDay {
		if day < first || day > last {
			continue
		}
		x.Input += t.Input
		x.Cached += t.Cached
		x.Output += t.Output
		thoughts += t.Thoughts
	}
	x.Output += thoughts // billed as output
	if p := retain.PricesFor(cfg.LLM.Model, cfg.LLM.PriceInput, cfg.LLM.PriceCached, cfg.LLM.PriceOutput, cfg.LLM.Currency); p.Known() {
		x.Cost, x.Currency = p.Cost(x.Input, x.Cached, x.Output, 0), p.Currency
	}
	return x
}

// serverDays notes when the server's 30 days of buckets cut the window.
func serverDays(x extraction, days int) string {
	if x.Days == 0 {
		return " (none counted: the server keeps only the last 30 days)"
	}
	if x.Days < days {
		return fmt.Sprintf(" (last %d days: the server keeps no more)", x.Days)
	}
	return ""
}

func pct(n, of int) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", (100*n+of/2)/of)
}

func counts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func statsText(r statsResult) string {
	var b strings.Builder
	p := r.Prompts
	span := r.From.UTC().Format("2006-01-02 15:04") + " → " + r.To.UTC().Format("2006-01-02 15:04")
	if r.Window {
		fmt.Fprintf(&b, "Memory from %s (UTC)\n\n", span)
	} else {
		fmt.Fprintf(&b, "Memory over the last %d day(s), %s (UTC)\n\n", r.Days, span)
	}
	fmt.Fprintf(&b, "Prompts      %d typed, %d machine messages; recalls %d (%s injected something); skipped: %s\n",
		p.Human, p.Machine, p.Recalls, pct(p.WithHits, p.Recalls), counts(p.Skips))
	fmt.Fprintf(&b, "Recall       p50 %d ms, p95 %d ms; scope calls failed %d / %d\n", p.P50MS, p.P95MS, p.ScopeFailures, p.ScopeCalls)
	if fr, en := p.ByLang["fr"], p.ByLang["en"]; fr[0]+en[0] > 0 {
		fmt.Fprintf(&b, "By language  French %d recalls (%s injected), English %d (%s)\n", fr[0], pct(fr[1], fr[0]), en[0], pct(en[1], en[0]))
	}
	fmt.Fprintf(&b, "Searches     %d of %d prompts ≥ %d chars (%s); replies after a search: %s\n",
		r.Turns.Searched, r.Turns.Long, stats.LongPrompt, pct(r.Turns.Searched, r.Turns.Long), counts(r.Turns.Credit))
	i := r.Injections
	fmt.Fprintf(&b, "Injections   %d: followed by a search %s, credited %s, contradicted next prompt %d\n",
		i.N, pct(i.Searched, i.N), pct(i.Credited, i.N), i.Contradicted)
	fmt.Fprintf(&b, "Briefings    %d (%d mental-model pages, %d empty): %d recent memories, %s returned again by a memory tool later\n",
		r.Briefings.N, r.Briefings.Model, r.Briefings.Empty, r.Briefings.IDs, pct(r.Briefings.Used, r.Briefings.IDs))
	fmt.Fprintf(&b, "Detector     fired %d, followed by a save %s\n", r.Detector.Fired, pct(r.Detector.Saved, r.Detector.Fired))
	fmt.Fprintf(&b, "Context      %d session(s), prompt-hook text per session p95 %d chars\n", r.Budget.Sessions, r.Budget.P95Chars)
	fmt.Fprintf(&b, "Gate         %s\n", counts(r.Gate))
	fmt.Fprintf(&b, "MCP tools    %s; session found: %s\n", counts(r.Tools), counts(r.Live))
	fmt.Fprintf(&b, "Retains      %s\n", counts(r.Retains))
	if a := r.Attribution; a.Main+a.Subagents > 0 {
		fmt.Fprintf(&b, "Attribution  main turns %d, the path rule would file %d elsewhere (%s, not applied); subagent reports %d, %d filed in another repository than their session's\n",
			a.Main, a.Disagree, pct(a.Disagree, a.Main), a.Subagents, a.Moved)
	}
	f := r.Feedback
	fmt.Fprintf(&b, "Credits      turns with memories shown: declared %d, after a block %d, undeclared %d, skipped %d; used %d, none %d, wrong %d, outdated %d, refused %d\n",
		f.Checks["declared"]-f.DeclaredAfterBlock, f.DeclaredAfterBlock, f.Checks["undeclared"], f.Checks["check_skipped"],
		f.Verdicts["used"], f.Verdicts["none"], f.Verdicts["wrong"], f.Verdicts["outdated"], f.Refused)
	fmt.Fprintf(&b, "Retirements  %d raw fact(s) invalidated on the server (%d verdict(s) the server refused halfway), %d observation(s) hidden on this machine\n",
		f.Retired, f.Partly, f.Hidden)
	st := f.Settled
	fmt.Fprintf(&b, "Ledger       settled %d shows: %d credits, %d confident misses (%s of uncredited), %d unknown, %d late; honesty flags: %s",
		st.Shows, st.Credits, st.Misses, pct(st.Misses, st.Shows-st.Credits), st.Unknown, f.Late, counts(f.Flags))
	switch {
	case f.SharedMachines == 1:
		b.WriteString("; shared with 1 other machine")
	case f.SharedMachines > 1:
		fmt.Fprintf(&b, "; shared with %d other machines", f.SharedMachines)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Hook failures %s; unknown transcript records: %s\n", counts(r.HookFailures), counts(r.Unknown))
	x := r.Extraction
	switch {
	case x.Error != "":
		fmt.Fprintf(&b, "Extraction   unavailable: %s\n", x.Error)
	case x.Currency != "":
		fmt.Fprintf(&b, "Extraction   %d input (%d cached), %d output tokens: %.2f %s%s\n", x.Input, x.Cached, x.Output, x.Cost, x.Currency, serverDays(x, r.Days))
	default:
		fmt.Fprintf(&b, "Extraction   %d input (%d cached), %d output tokens (no price known: set llm.price_* for a cost)%s\n", x.Input, x.Cached, x.Output, serverDays(x, r.Days))
	}
	if len(r.Parity) > 0 {
		fmt.Fprintf(&b, "\nParity against the baseline of the Hindsight integration backsight replaced (a verdict needs %d events)\n", stats.MinParity)
		for _, l := range r.Parity {
			got := pct(l.N, l.Of) + fmt.Sprintf(" (%d/%d)", l.N, l.Of)
			if l.Latency {
				got = fmt.Sprintf("%d ms (%d recalls)", l.N, l.Of)
			}
			fmt.Fprintf(&b, "  %-44s %s %-16s now %-22s %s\n", l.Metric, l.Rule, l.Baseline, got, l.Verdict)
		}
		fmt.Fprintf(&b, "Parity       %s\n", stats.ParityOverall(r.Parity))
	}
	return b.String()
}
