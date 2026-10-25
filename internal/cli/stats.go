package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
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
	stats.Report
	Extraction extraction `json:"extraction"`
}

// newStatsCmd is `stats [days]` (SPEC §15): the C12 report from local
// telemetry, and the server's extraction tokens and cost.
func newStatsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "stats [days]",
		Short: "Show how memory was used over the last days (default 7): recalls, searches, credits, detector, cost",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			days := 7
			if len(args) == 1 {
				n, err := strconv.Atoi(args[0])
				if err != nil || n < 1 || n > 365 {
					return &output.Error{Code: output.Usage, Message: "days: a whole number from 1 to 365"}
				}
				days = n
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			to := now()
			from := to.Add(-time.Duration(days) * 24 * time.Hour)
			ev, err := stats.Load(d.TelemetryDir(), from, to)
			if err != nil {
				return blocked(err)
			}
			res := statsResult{Days: days, Report: stats.Compute(ev, from, to)}
			res.Extraction = extractionOver(cmd, from, to, days)
			return e.emit("stats", res, func() string { return statsText(res) })
		},
	}
}

// extractionOver reads Hindsight's LLM token use over the window, from its
// daily buckets: the server answers 1d or 30d only (anything else silently
// gives 7 days), so 30d is asked and the days outside [from, to] dropped.
// A server that cannot be read leaves the rest of the report intact.
func extractionOver(cmd *cobra.Command, from, to time.Time, days int) extraction {
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
	x := extraction{Days: min(days, 30)}
	first := from.UTC().Format("2006-01-02")
	if days > 30 {
		first = to.UTC().AddDate(0, 0, -29).Format("2006-01-02")
	}
	last := to.UTC().Format("2006-01-02")
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
	fmt.Fprintf(&b, "Memory over the last %d day(s), %s → %s (UTC)\n\n", r.Days, r.From.UTC().Format("2006-01-02 15:04"), r.To.UTC().Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "Prompts      %d typed, %d machine messages; recalls %d (%s injected something); skipped: %s\n",
		p.Human, p.Machine, p.Recalls, pct(p.WithHits, p.Recalls), counts(p.Skips))
	fmt.Fprintf(&b, "Recall       p50 %d ms, p95 %d ms; scope calls failed %d / %d\n", p.P50MS, p.P95MS, p.ScopeFailures, p.ScopeCalls)
	fmt.Fprintf(&b, "Searches     %d of %d prompts ≥ %d chars (%s); replies after a search: %s\n",
		r.Turns.Searched, r.Turns.Long, stats.LongPrompt, pct(r.Turns.Searched, r.Turns.Long), counts(r.Turns.Credit))
	i := r.Injections
	fmt.Fprintf(&b, "Injections   %d: followed by a search %s, credited %s, contradicted next prompt %d\n",
		i.N, pct(i.Searched, i.N), pct(i.Credited, i.N), i.Contradicted)
	fmt.Fprintf(&b, "Briefings    %d (%d empty): %d memories, %s returned again by a memory tool later\n",
		r.Briefings.N, r.Briefings.Empty, r.Briefings.IDs, pct(r.Briefings.Used, r.Briefings.IDs))
	fmt.Fprintf(&b, "Detector     fired %d, followed by a save %s\n", r.Detector.Fired, pct(r.Detector.Saved, r.Detector.Fired))
	fmt.Fprintf(&b, "Context      %d session(s), prompt-hook text per session p95 %d chars\n", r.Budget.Sessions, r.Budget.P95Chars)
	fmt.Fprintf(&b, "Gate         %s\n", counts(r.Gate))
	fmt.Fprintf(&b, "MCP tools    %s; session found: %s\n", counts(r.Tools), counts(r.Live))
	fmt.Fprintf(&b, "Retains      %s\n", counts(r.Retains))
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
	return b.String()
}
