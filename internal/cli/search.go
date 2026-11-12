package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/telemetry"
)

const (
	searchMinScore = 0.4 // memory_search default, below the prompt-recall floor
	// searchLimit only caps a long answer: the floor picks the hits.
	// Memories are short (about 120 tokens a hit), so 20 hits cost about
	// 2,500 tokens.
	searchLimit = 20
	// searchMaxTokens: each server call of a search a reader sees brings up
	// to this much memory text, about 25 memories, so one query in one scope
	// can fill the limit. The prompt hook keeps recall's default.
	searchMaxTokens = 3000
	maxQueries      = 4
	recallTimeout   = 17 * time.Second // the recall budget (SPEC §6.2)
)

// currentProject: BACKSIGHT_PROJECT (written by SessionStart for the CLI
// fallback) else the working directory's project.
func currentProject(home string, cfg config.Config) string {
	if p := os.Getenv("BACKSIGHT_PROJECT"); p != "" {
		return p
	}
	cwd, _ := os.Getwd()
	return project.New(home, cfg.Projects.Transverse).Project(cwd)
}

func newEngine(cfg config.Config, sec config.Secrets) (*recall.Engine, error) {
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, recallTimeout)
	if err != nil {
		return nil, blocked(err)
	}
	return &recall.Engine{Client: cl, Bank: cfg.Server.Bank, Transverse: cfg.Projects.Transverse, Timeout: recallTimeout, Now: now, Boost: cfg.Recall.ProjectBoost}, nil
}

// searchMemory is memory_search's engine, shared by the CLI and MCP: the
// project scope adds what cwd's .backsight.toml relates, the related scope
// asked-for projects and those whose names the queries mention (entity
// index). The ledger is read at each search, not with the engine: the MCP
// server's engine lives as long as its session. A ledger that cannot be
// read is logged to tw (ledgerReader).
func searchMemory(ctx context.Context, d paths.Dirs, home string, cfg config.Config, tw *telemetry.Writer, eng *recall.Engine, current, cwd string, queries, projects []string, all bool, limit int, minScore float64) recall.Result {
	adjusted := *eng
	adjusted.Adjust = ledgerAdjust(d, cfg, now(), tw)
	for i, q := range queries {
		queries[i], _ = redact.Text(q) // a key pasted into a query never leaves the machine
	}
	var related []string
	if idx, err := recall.LoadIndex(d.EntitiesFile()); err == nil {
		related = idx.Projects(strings.Join(queries, " "), current, cfg.Projects.Transverse)
	}
	linked := linkedProjects(d, cfg, project.New(home, cfg.Projects.Transverse), cwd, current)
	return adjusted.Search(ctx, recall.Query{Queries: queries, Current: current, Linked: linked, Projects: projects, Related: related, All: all, Limit: limit, MaxTokens: searchMaxTokens, MinScore: minScore, Kinds: true})
}

func newSearchCmd(e *env) *cobra.Command {
	var projects []string
	var all bool
	var limit int
	var minScore float64
	cmd := &cobra.Command{
		Use:   "search <query>...",
		Short: "Search the memory (each argument is one short keyword query, at most 4)",
		Args:  usageArgs(cobra.RangeArgs(1, maxQueries)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, home, err := machine()
			if err != nil {
				return err
			}
			eng, err := newEngine(cfg, sec)
			if err != nil {
				return err
			}
			cwd, _ := os.Getwd()
			tw := telemetry.New(d.State, telemetry.Common{BinaryVersion: buildinfo.Version})
			res := searchMemory(cmd.Context(), d, home, cfg, tw, eng, currentProject(home, cfg), cwd, args, projects, all, limit, minScore)
			var failed error
			if len(res.Hits) == 0 && !scopesOK(res) { // a failing scope alone is skipped (0054)
				failed = &output.Error{Code: output.Blocked, Message: "memory unavailable: " + scopeErrors(res)}
			}
			return e.report("search", res, func() string { return searchText(res) }, failed)
		},
	}
	cmd.Flags().StringSliceVar(&projects, "project", nil, "also search these projects")
	cmd.Flags().BoolVar(&all, "all", false, "search the whole bank only, without project scopes")
	cmd.Flags().IntVar(&limit, "limit", searchLimit, "most hits shown")
	cmd.Flags().Float64Var(&minScore, "min-score", searchMinScore, "score floor (reranker + current-project boost + the boost from what Claude used)")
	return cmd
}

// oneLine keeps a hit on its line: a memory's text never starts a line of
// its own, which the rank parser would read as a hit.
var oneLine = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// searchText groups hits by project, best first, each with its rank.
func searchText(r recall.Result) string {
	if len(r.Hits) == 0 {
		return "nothing in memory\n"
	}
	var b strings.Builder
	if r.Raw {
		b.WriteString("(raw facts: the memory is not consolidated yet)\n")
	}
	// Each hit keeps its merged rank (#n) in its project's group: the
	// PostToolUse hook journals search shows from it.
	var order []string
	groups := map[string][]int{}
	for i, h := range r.Hits {
		if groups[h.Project] == nil {
			order = append(order, h.Project)
		}
		groups[h.Project] = append(groups[h.Project], i)
	}
	for _, p := range order {
		fmt.Fprintf(&b, "[%s]\n", tagProject(p))
		for _, i := range groups[p] {
			h := r.Hits[i]
			fmt.Fprintf(&b, "- #%d %s (%s%s%s, %s ago, %s, %.2f)\n", i+1, oneLine.Replace(h.Text), h.Date.Format("2006-01-02"), kindLabel(h), sourceLabel(h.Source), h.Age, h.ID, h.Score)
		}
	}
	return b.String()
}

// kindLabel is " [pitfall, state]" after the date: the kinds of the facts
// behind a hit (a subagent's report is named by sourceLabel since
// decision 0089).
func kindLabel(h recall.Hit) string {
	if len(h.Kinds) == 0 {
		return ""
	}
	return " [" + strings.Join(h.Kinds, ", ") + "]"
}

// sourceLabel is ", from <source>" after the kinds, "" when the memory's
// tags do not say (decision 0089 P2).
func sourceLabel(source string) string {
	if source == "" {
		return ""
	}
	return ", from " + tagProject(source)
}

func newReadCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "read <id>",
		Short: "Show one memory with the facts it was built from",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, recallTimeout)
			if err != nil {
				return blocked(err)
			}
			id, err := readCmdID(args[0])
			if err != nil {
				return err
			}
			m, err := cl.GetMemory(cmd.Context(), cfg.Server.Bank, id)
			var apiErr *hindsight.APIError
			if errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound {
				return &output.Error{Code: output.Usage, Message: "no memory " + args[0]}
			}
			if err != nil {
				return blocked(err)
			}
			return e.emit("read", m, func() string { return readText(m) })
		},
	}
}

// shortRefRE: the start of a memory id, as the hook lines and the tools
// print it (8 characters or more), not a whole one.
var shortRefRE = regexp.MustCompile(`^[0-9a-f]{8}[0-9a-f-]{0,27}$`)

// readCmdID is the memory id read's ref names: a whole id or any other
// ref as it is, a short one resolved among the ids this PC's journal
// holds, as memory_read does among a session's (issue 317). The server
// takes only whole ids: a short one the journal does not hold is refused
// here.
func readCmdID(ref string) (string, error) {
	if fullUUIDRE.MatchString(ref) || !shortRefRE.MatchString(ref) {
		return ref, nil
	}
	var events []feedback.Event
	if d, _, err := machine(); err == nil {
		events, _, _ = feedback.Read(d.FeedbackDir(), time.Time{})
	}
	id, err := resolveLogged(ref, events)
	switch {
	case err != nil:
		return "", &output.Error{Code: output.Usage, Message: err.Error()}
	case id == ref:
		return "", &output.Error{Code: output.Usage, Message: "no memory shown on this PC starts with " + ref + ": give its full id"}
	}
	return id, nil
}

func readText(m hindsight.MemoryDetail) string {
	var b strings.Builder
	tags := [][]string{m.Tags}
	for _, s := range m.Sources {
		tags = append(tags, s.Tags)
	}
	fmt.Fprintf(&b, "%s\n\n%s · project %s%s · mentioned %s", m.Text, m.Type, tagProject(recall.ProjectOf(m.Tags)), strings.Replace(sourceLabel(recall.SourceOf(tags...)), ",", " ·", 1), m.MentionedAt)
	if m.OccurredStart != "" {
		fmt.Fprintf(&b, " · occurred %s", m.OccurredStart)
		if m.OccurredEnd != "" && m.OccurredEnd != m.OccurredStart {
			fmt.Fprintf(&b, " → %s", m.OccurredEnd)
		}
	}
	if m.DocumentID != "" {
		fmt.Fprintf(&b, " · document %s", m.DocumentID)
	}
	if m.State != "" && m.State != "valid" {
		fmt.Fprintf(&b, " · %s", m.State)
	}
	b.WriteString("\n")
	if len(m.Sources) > 0 {
		b.WriteString("\nfrom:\n")
	}
	for _, s := range m.Sources {
		kind := ""
		for _, t := range s.Tags {
			if k, ok := strings.CutPrefix(t, "kind:"); ok {
				kind = k + ", "
			}
		}
		doc := ""
		if s.DocumentID != "" {
			doc = ", " + s.DocumentID
		}
		fmt.Fprintf(&b, "- %s (%s%s%s)\n", s.Text, kind, s.Type, doc)
	}
	return b.String()
}
