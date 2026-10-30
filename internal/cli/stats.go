package cli

import (
	"cmp"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
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

// serverReach is what this PC recorded of its server's answers
// (state.Reach), the breaker and the notes queue: read from local files,
// never from the network, so stats reports a server that is down.
type serverReach struct {
	Host             string    `json:"host,omitempty"` // a client's server.url host
	LastOK           time.Time `json:"last_ok,omitzero"`
	FailingSince     time.Time `json:"failing_since,omitzero"`
	BreakerOpenUntil time.Time `json:"breaker_open_until,omitzero"` // set while recall is paused
	NotesQueued      int       `json:"notes_queued"`
	SessionsWaiting  int       `json:"sessions_waiting"`
	// OtherServer: the id of another server that answers at a client's
	// joined address (state.RemoteServer.Mismatch), since HeldSince:
	// every write is held, whatever it answers.
	OtherServer string    `json:"other_server,omitempty"`
	JoinedID    string    `json:"-"`
	HeldSince   time.Time `json:"held_since,omitzero"`
}

type statsResult struct {
	Days int `json:"days"`
	// Window: set by --since, the report is [from, to) instead of the
	// last Days days.
	Window bool `json:"window,omitempty"`
	stats.Report
	Server serverReach `json:"server"`
	// Runtime is a local server's runtime (decision 0078), "" on a client;
	// RuntimeSwitch its last switch.
	Runtime       string             `json:"runtime,omitempty"`
	RuntimeSwitch *statsSwitch       `json:"runtime_switch,omitempty"`
	Extraction    extraction         `json:"extraction"`
	Parity        []stats.ParityLine `json:"parity,omitempty"`
	now           time.Time
}

// statsSwitch is the last server runtime switch, from state.json.
type statsSwitch struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	At      time.Time `json:"at"`
	Overlap float64   `json:"overlap"` // raw
	Step    string    `json:"step"`    // running | done | rolled-back
	// OverlapBeyondTies: the one shown (issue #123); absent from a switch
	// recorded before it.
	OverlapBeyondTies float64 `json:"overlap_beyond_near_ties,omitempty"`
}

// runtimeOf is the local server's runtime and its last switch; "" and
// nil on a client.
func runtimeOf(d paths.Dirs, home string) (string, *statsSwitch) {
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil || cfg.Server.Mode != "local" {
		return "", nil
	}
	st, err := state.Load(d.StateFile())
	if err != nil || st.RuntimeSwitch == nil {
		return cfg.RuntimeKind(), nil
	}
	rs := st.RuntimeSwitch
	return cfg.RuntimeKind(), &statsSwitch{From: rs.From, To: rs.To, At: rs.At.UTC(), Overlap: rs.Overlap, Step: rs.Step, OverlapBeyondTies: rs.OverlapBeyondTies}
}

// runtimeText is the Runtime line's text after its label.
func runtimeText(kind string, s *statsSwitch) string {
	if s == nil {
		return kind
	}
	day := s.At.Format("2006-01-02")
	switch s.Step {
	case "done":
		if s.OverlapBeyondTies > 0 {
			return fmt.Sprintf("%s (switched from %s on %s, recall overlap@10 %.2f beyond near-ties)", kind, s.From, day, s.OverlapBeyondTies)
		}
		return fmt.Sprintf("%s (switched from %s on %s, recall overlap@10 %.2f)", kind, s.From, day, s.Overlap)
	case "rolled-back":
		return fmt.Sprintf("%s (the switch to %s on %s was rolled back)", kind, s.To, day)
	}
	return fmt.Sprintf("%s (a switch to %s runs or was cut off on %s: see backsight doctor)", kind, s.To, day)
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
			d, home, err := machine()
			if err != nil {
				return err
			}
			ev, err := stats.Load(d.TelemetryDir(), from, to)
			if err != nil {
				return blocked(err)
			}
			res := statsResult{Days: days, Window: since != "", Report: stats.Compute(ev, from, to), now: now()}
			res.Server = reachOf(d, home, res.now)
			res.Runtime, res.RuntimeSwitch = runtimeOf(d, home)
			res.Feedback.SharedMachines = sharedMachines(d)
			journalCounts(d, &res.Report)
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

// reachOf reads what this PC recorded of its server at now. On a client
// (server.mode remote) it names the server's host, never the whole URL.
func reachOf(d paths.Dirs, home string, now time.Time) serverReach {
	r := state.LoadReach(d)
	s := serverReach{LastOK: r.LastOK.UTC(), FailingSince: r.FailingSince.UTC(), SessionsWaiting: r.SessionsWaiting}
	if b := state.LoadBreaker(d); now.Before(b.OpenUntil) {
		s.BreakerOpenUntil = b.OpenUntil.UTC()
	}
	s.NotesQueued, _ = retain.Outbox{Path: filepath.Join(d.OutboxDir(), "notes.jsonl")}.Pending()
	if cfg, err := config.Load(d.ConfigFile(), d, home); err == nil && cfg.Server.Mode == "remote" {
		if u, err := url.Parse(cfg.Server.URL); err == nil {
			s.Host = u.Hostname()
		}
		if st, err := state.Load(d.StateFile()); err == nil && st.RemoteServer != nil && st.RemoteServer.Mismatch != "" {
			s.OtherServer, s.JoinedID, s.HeldSince = st.RemoteServer.Mismatch, st.RemoteServer.ID, st.RemoteServer.MismatchAt.UTC()
		}
	}
	return s
}

// reachText is the Server line's text after its label.
func reachText(s serverReach, now time.Time) string {
	host := ""
	if s.Host != "" {
		host = s.Host + " "
	}
	switch {
	case s.OtherServer != "":
		at := cmp.Or(s.Host, "the joined address")
		since := ""
		if !s.HeldSince.IsZero() {
			since = fmt.Sprintf(" since %s (%s)", s.HeldSince.Format(time.RFC3339), recall.Age(now.Sub(s.HeldSince)))
		}
		return fmt.Sprintf("a different server answers at %s (server %s, joined %s)%s: writes held; %s queued, %s not sent yet", at, s.OtherServer, s.JoinedID, since,
			plural(s.NotesQueued, "note"), plural(s.SessionsWaiting, "session"))
	case !s.FailingSince.IsZero():
		recallState := "recall on"
		if !s.BreakerOpenUntil.IsZero() {
			recallState = "recall paused until " + s.BreakerOpenUntil.Format(time.RFC3339)
		}
		return fmt.Sprintf("%ssilent since %s (%s): %s; %s queued, %s not sent yet", host, s.FailingSince.Format(time.RFC3339),
			recall.Age(now.Sub(s.FailingSince)), recallState, plural(s.NotesQueued, "note"), plural(s.SessionsWaiting, "session"))
	case !s.LastOK.IsZero():
		return fmt.Sprintf("%sanswering (last answer %s ago)", host, recall.Age(now.Sub(s.LastOK)))
	}
	return host + "not recorded yet"
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
	// Read only: stats never creates the machine id. Without one, every
	// document counts (the tick leaves this machine's out anyway).
	return feedback.SharedMachines(docs, retain.ReadMachineID(d.State), now())
}

// journalCounts sets r's counts that the feedback journal holds, over r's
// window: the prompt recalls and searches whose shown set the boost
// changed (their set_changed flag), and the shows imported from earlier
// history. A journal that cannot be read counts what it could.
func journalCounts(d paths.Dirs, r *stats.Report) {
	events, _, _ := feedback.Read(d.FeedbackDir(), r.From)
	f := &r.Feedback
	for _, e := range events {
		if e.TS.Before(r.From) || !e.TS.Before(r.To) {
			continue
		}
		changed := slices.Contains(e.Flags, feedback.FlagSetChanged)
		switch {
		case e.Kind == "shows" && e.Path == "prompt":
			f.Boost.Prompts++
			if changed {
				f.Boost.PromptsSetChanged++
			}
		case e.Kind == feedback.KindRecall:
			f.Boost.Searches++
			if changed {
				f.Boost.SearchesSetChanged++
			}
		case e.Kind == feedback.KindImport:
			for _, s := range e.Settled {
				f.Imported.Shows++
				if s.Outcome == feedback.OutcomeCredit {
					f.Imported.Credits++
				}
			}
		}
	}
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
	fmt.Fprintf(&b, "Server       %s\n", reachText(r.Server, r.now))
	if r.Runtime != "" {
		fmt.Fprintf(&b, "Runtime      %s\n", runtimeText(r.Runtime, r.RuntimeSwitch))
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
	fmt.Fprintf(&b, "Ledger       settled %d live shows: %d credits, %d confident misses (%s of uncredited), %d unknown, %d late; imported %d shows of earlier history (%d credits); honesty flags: %s",
		st.Shows, st.Credits, st.Misses, pct(st.Misses, st.Shows-st.Credits), st.Unknown, f.Late, f.Imported.Shows, f.Imported.Credits, counts(f.Flags))
	switch {
	case f.SharedMachines == 1:
		b.WriteString("; shared with 1 other machine")
	case f.SharedMachines > 1:
		fmt.Fprintf(&b, "; shared with %d other machines", f.SharedMachines)
	}
	b.WriteString("\n")
	bo := f.Boost
	fmt.Fprintf(&b, "Boost        changed the shown set of %d of %d prompt recalls (%s) and %d of %d searches (%s)\n",
		bo.PromptsSetChanged, bo.Prompts, pct(bo.PromptsSetChanged, bo.Prompts), bo.SearchesSetChanged, bo.Searches, pct(bo.SearchesSetChanged, bo.Searches))
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
