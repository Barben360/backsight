package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/telemetry"
)

type searchArgs struct {
	Queries  []string `json:"queries" jsonschema:"1-4 short keyword queries, one per subject"`
	Projects []string `json:"projects,omitempty" jsonschema:"other projects to search too"`
	Scope    string   `json:"scope,omitempty" jsonschema:"auto (current project, transverse, related projects and the whole memory; default) or all (the whole memory only)"`
	Limit    int      `json:"limit,omitempty" jsonschema:"most memories returned (default 8)"`
	MinScore *float64 `json:"min_score,omitempty" jsonschema:"relevance floor (default 0.4); lower it to widen"`
}

type readArgs struct {
	ID string `json:"id" jsonschema:"the memory id shown by memory_search"`
}

type storeArgs struct {
	Kind    string `json:"kind" jsonschema:"decision, correction, convention, pitfall, state or lesson"`
	Text    string `json:"text" jsonschema:"1-3 sentences: the fact, why, absolute dates"`
	Project string `json:"project,omitempty" jsonschema:"default: the current project; \"transverse\" for user-wide rules"`
}

// mcpDeps is what the tools need, resolved once per server.
type mcpDeps struct {
	dirs    paths.Dirs
	home    string
	cfg     config.Config
	sec     config.Secrets
	engine  *recall.Engine
	current string
	tw      *telemetry.Writer
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errorResult(s string) *mcp.CallToolResult {
	r := textResult(s)
	r.IsError = true
	return r
}

// newMCPServer builds the memory server (SPEC §7).
func newMCPServer(deps mcpDeps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "memory", Version: buildinfo.Version}, &mcp.ServerOptions{Instructions: instructions.ShortGuide})
	mcp.AddTool(s, &mcp.Tool{Name: "memory_search", Description: instructions.SearchDescription, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, a searchArgs) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			if len(a.Queries) == 0 || len(a.Queries) > maxQueries {
				return errorResult("queries: give 1 to 4 short keyword queries"), nil, nil
			}
			if a.Scope != "" && a.Scope != "auto" && a.Scope != "all" {
				return errorResult(`scope: "auto" or "all"`), nil, nil
			}
			limit, minScore := a.Limit, searchMinScore
			if limit <= 0 {
				limit = searchLimit
			}
			if a.MinScore != nil {
				minScore = *a.MinScore
			}
			res := searchMemory(ctx, deps.dirs, deps.cfg, deps.engine, deps.current, a.Queries, a.Projects, a.Scope == "all", limit, minScore)
			deps.tw.Emit(telemetry.Tool{Name: "memory_search", MS: int(time.Since(start).Milliseconds()), Hits: len(res.Hits), OK: scopesOK(res)})
			if len(res.Hits) == 0 && !scopesOK(res) {
				return errorResult("memory unavailable: " + scopeErrors(res)), nil, nil
			}
			return textResult(mcpSearchText(res)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "memory_read", Description: instructions.ReadDescription, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, a readArgs) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			id := strings.TrimSpace(a.ID)
			if !memoryIDRE.MatchString(id) || strings.Contains(id, "..") {
				return errorResult("id: the memory id shown by memory_search"), nil, nil
			}
			m, err := deps.engine.Client.GetMemory(ctx, deps.cfg.Server.Bank, id)
			deps.tw.Emit(telemetry.Tool{Name: "memory_read", MS: int(time.Since(start).Milliseconds()), Hits: map[bool]int{true: 1}[err == nil], OK: err == nil})
			var apiErr *hindsight.APIError
			if errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound {
				return errorResult("no memory " + a.ID), nil, nil
			}
			if err != nil {
				return errorResult("memory unavailable: " + err.Error()), nil, nil
			}
			return textResult(readText(m)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "memory_store", Description: instructions.StoreDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, a storeArgs) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			// Refusals first: nothing leaves the machine for a refused note.
			if err := checkNote(deps.dirs, deps.home, deps.cfg, a.Kind, a.Project, a.Text); err != nil {
				deps.tw.Emit(telemetry.Tool{Name: "memory_store", MS: int(time.Since(start).Milliseconds())})
				return errorResult(err.Error()), nil, nil
			}
			proj := a.Project
			if proj == "" {
				proj = deps.current
			}
			proj = noteProject(deps.home, deps.cfg, proj)
			// What this fact updates or repeats, before it is stored (SPEC §7).
			similar := deps.engine.Search(ctx, recall.Query{Queries: []string{a.Text}, Current: proj, MinScore: searchMinScore, Limit: 3})
			res, err := storeNote(ctx, deps.dirs, deps.home, deps.cfg, deps.sec, a.Kind, proj, a.Text)
			deps.tw.Emit(telemetry.Tool{Name: "memory_store", MS: int(time.Since(start).Milliseconds()), Hits: len(similar.Hits), OK: err == nil})
			if res.Document == "" {
				return errorResult(err.Error()), nil, nil
			}
			var b strings.Builder
			b.WriteString(storeText(res))
			if err != nil {
				fmt.Fprintf(&b, "%s\n", err)
			}
			if len(similar.Hits) > 0 {
				b.WriteString("Most similar memories (does this fact update or repeat one?):\n")
				for _, h := range similar.Hits {
					fmt.Fprintf(&b, "- [%s] %s (%s ago, %s)\n", h.Project, h.Text, h.Age, h.ID)
				}
			}
			return textResult(b.String()), nil, nil
		})
	return s
}

// memoryIDRE: what Hindsight ids look like (UUIDs); nothing that could
// turn into another path.
var memoryIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

func scopesOK(r recall.Result) bool {
	for _, m := range r.Scopes {
		if m.OK {
			return true
		}
	}
	return len(r.Scopes) == 0
}

func scopeErrors(r recall.Result) string {
	var errs []string
	for name, m := range r.Scopes {
		if !m.OK {
			errs = append(errs, name+": "+m.Err)
		}
	}
	return strings.Join(errs, "; ")
}

// mcpSearchText is searchText with the credit format the agent must use.
func mcpSearchText(r recall.Result) string {
	if len(r.Hits) == 0 {
		return "nothing in memory\n"
	}
	return searchText(r) + "\nCite what you use as: " + instructions.Credit + "\n"
}

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp",
		Short:  "Serve the memory tools to Claude Code over stdio (started by the plugin)",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			current := currentProject(home, cfg)
			tw := telemetry.New(d.State, telemetry.Common{V: instructions.Version, Project: current, BinaryVersion: buildinfo.Version})
			s := newMCPServer(mcpDeps{dirs: d, home: home, cfg: cfg, sec: sec, engine: eng, current: current, tw: tw})
			// Claude Code ends a session by closing stdin: a clean exit.
			if err := s.Run(cmd.Context(), &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
				return &output.Error{Code: output.Blocked, Message: err.Error()}
			}
			return nil
		},
	}
}
