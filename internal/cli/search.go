package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
)

const (
	searchMinScore = 0.4 // memory_search default, below the prompt-recall floor
	searchLimit    = 8
	maxQueries     = 4
	recallTimeout  = 17 * time.Second // the recall budget (SPEC §6.2)
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
// server's engine lives as long as its session.
func searchMemory(ctx context.Context, d paths.Dirs, home string, cfg config.Config, eng *recall.Engine, current, cwd string, queries, projects []string, all bool, limit int, minScore float64) recall.Result {
	adjusted := *eng
	adjusted.Adjust = ledgerAdjust(d, cfg, now())
	for i, q := range queries {
		queries[i], _ = redact.Text(q) // a key pasted into a query never leaves the machine
	}
	var related []string
	if idx, err := recall.LoadIndex(d.EntitiesFile()); err == nil {
		related = idx.Projects(strings.Join(queries, " "), current, cfg.Projects.Transverse)
	}
	linked := linkedProjects(d, cfg, project.New(home, cfg.Projects.Transverse), cwd, current)
	return adjusted.Search(ctx, recall.Query{Queries: queries, Current: current, Linked: linked, Projects: projects, Related: related, All: all, Limit: limit, MinScore: minScore, Kinds: true})
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
			res := searchMemory(cmd.Context(), d, home, cfg, eng, currentProject(home, cfg), cwd, args, projects, all, limit, minScore)
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
		fmt.Fprintf(&b, "[%s]\n", p)
		for _, i := range groups[p] {
			h := r.Hits[i]
			fmt.Fprintf(&b, "- #%d %s (%s%s, %s ago, %s, %.2f)\n", i+1, oneLine.Replace(h.Text), h.Date.Format("2006-01-02"), kindLabel(h), h.Age, h.ID, h.Score)
		}
	}
	return b.String()
}

// kindLabel is " [pitfall, state]" after the date: the kinds of the facts
// behind a hit, "subagent" added when one came from a subagent's report.
func kindLabel(h recall.Hit) string {
	parts := h.Kinds
	if h.Subagent {
		parts = append(parts[:len(parts):len(parts)], "subagent")
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, ", ") + "]"
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
			m, err := cl.GetMemory(cmd.Context(), cfg.Server.Bank, args[0])
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

func readText(m hindsight.MemoryDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s · project %s · mentioned %s", m.Text, m.Type, recall.ProjectOf(m.Tags), m.MentionedAt)
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
