package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
)

const curateTimeout = 30 * time.Second

func curateClient() (*hindsight.Client, string, error) {
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return nil, "", err
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, curateTimeout)
	if err != nil {
		return nil, "", blocked(err)
	}
	return cl, cfg.Server.Bank, nil
}

type forgetResult struct {
	ID        string   `json:"id"`
	Action    string   `json:"action"` // forgotten | restored | purge_planned | purged
	Sources   []string `json:"sources,omitempty"`
	Documents []string `json:"documents,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// newForgetCmd is `forget <id> [--purge [--yes]] [--restore]` (SPEC §2,
// §13): the only way memory is deleted, always by the user.
func newForgetCmd(e *env) *cobra.Command {
	var purge, yes, restore bool
	cmd := &cobra.Command{
		Use:   "forget <id>",
		Short: "Retire a memory (reversible), or with --purge delete it with its source facts and documents",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if purge && restore {
				return &output.Error{Code: output.Usage, Message: "--purge and --restore exclude each other"}
			}
			cl, bank, err := curateClient()
			if err != nil {
				return err
			}
			ctx, id := cmd.Context(), args[0]
			res := forgetResult{ID: id}
			switch {
			case restore:
				if err := cl.Invalidate(ctx, bank, id, "restored by the user (backsight forget --restore)", true); err != nil {
					return curateError(err, id)
				}
				res.Action = "restored"
			case !purge:
				if err := cl.Invalidate(ctx, bank, id, "forgotten by the user (backsight forget)", false); err != nil {
					return curateError(err, id)
				}
				res.Action = "forgotten"
			default:
				res, err = purgeMemory(ctx, cl, bank, id, yes)
				if err != nil {
					return curateError(err, id)
				}
				if !yes {
					return e.report("forget", res, func() string { return forgetText(res) }, &output.Error{Code: output.NeedsUser,
						Message: "purging deletes the documents listed (every fact extracted from them): confirm with backsight forget " + id + " --purge --yes",
						Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight forget " + id + " --purge --yes", Why: "delete for good what is listed"}}})
				}
			}
			return e.emit("forget", res, func() string { return forgetText(res) })
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "delete the memory, its source facts and the documents they came from (leaked secret)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm --purge")
	cmd.Flags().BoolVar(&restore, "restore", false, "undo a plain forget")
	return cmd
}

// purgeMemory plans, and with yes performs, the purge: retire the memory,
// clear the observations of its source facts, delete their documents.
func purgeMemory(ctx context.Context, cl *hindsight.Client, bank, id string, yes bool) (forgetResult, error) {
	res := forgetResult{ID: id, Action: "purge_planned"}
	m, err := cl.GetMemory(ctx, bank, id)
	if err != nil {
		return res, err
	}
	docs := map[string]bool{}
	if m.DocumentID != "" {
		docs[m.DocumentID] = true
	}
	for _, s := range m.Sources {
		res.Sources = append(res.Sources, s.ID)
		if s.DocumentID != "" {
			docs[s.DocumentID] = true
		}
	}
	for d := range docs {
		res.Documents = append(res.Documents, d)
	}
	sort.Strings(res.Documents)
	if !yes {
		return res, nil
	}
	if err := cl.Invalidate(ctx, bank, id, "purged by the user (backsight forget --purge)", false); err != nil {
		return res, err
	}
	for _, s := range res.Sources {
		if err := cl.ClearObservations(ctx, bank, s); err != nil {
			return res, err
		}
	}
	for _, d := range res.Documents {
		if err := cl.DeleteDocument(ctx, bank, d); err != nil {
			return res, err
		}
	}
	res.Action, res.Note = "purged", "backups taken before now still hold it (backups come with M5): delete or re-key them if the text was a secret"
	return res, nil
}

func curateError(err error, id string) error {
	var apiErr *hindsight.APIError
	if errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound {
		return &output.Error{Code: output.Usage, Message: "no memory " + id}
	}
	return blocked(err)
}

func forgetText(r forgetResult) string {
	switch r.Action {
	case "forgotten":
		return fmt.Sprintf("%s forgotten: out of recall and consolidation (undo: backsight forget --restore %s)\n", r.ID, r.ID)
	case "restored":
		return fmt.Sprintf("%s restored\n", r.ID)
	}
	var b strings.Builder
	verb := map[bool]string{true: "deleted", false: "would delete"}[r.Action == "purged"]
	fmt.Fprintf(&b, "%s: memory %s, %d source fact(s), document(s):\n", verb, r.ID, len(r.Sources))
	for _, d := range r.Documents {
		fmt.Fprintf(&b, "- %s (every fact extracted from it)\n", d)
	}
	if r.Note != "" {
		b.WriteString(r.Note + "\n")
	}
	return b.String()
}

type scanFinding struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Project string   `json:"project"`
	Kinds   []string `json:"kinds"`
	Preview string   `json:"preview"` // redacted
}

// newMemoryCmd groups `memory scan` (SPEC §13 threats).
func newMemoryCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "memory", Short: "Inspect the memory itself"}
	cmd.AddCommand(&cobra.Command{
		Use:   "scan",
		Short: "List stored memories that look like they hold a secret, with the command that purges each",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, bank, err := curateClient()
			if err != nil {
				return err
			}
			findings := []scanFinding{}
			for _, ft := range []string{"world", "experience", "observation"} {
				err := cl.EachMemory(cmd.Context(), bank, ft, func(m hindsight.Memory) error {
					red, kinds := redact.Text(m.Text)
					if len(kinds) > 0 {
						findings = append(findings, scanFinding{ID: m.ID, Type: ft, Project: recall.ProjectOf(m.Tags), Kinds: kinds, Preview: clip(red, 100)})
					}
					return nil
				})
				if err != nil {
					return blocked(err)
				}
			}
			return e.emit("memory scan", findings, func() string {
				if len(findings) == 0 {
					return "no secret shape found in the memory\n"
				}
				var b strings.Builder
				for _, f := range findings {
					fmt.Fprintf(&b, "%s (%s, %s, %s): %s\n  backsight forget %s --purge\n", f.ID, f.Type, f.Project, strings.Join(f.Kinds, ", "), f.Preview, f.ID)
				}
				return b.String()
			})
		},
	})
	return cmd
}

type askResult struct {
	Answer       string `json:"answer"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// newAskCmd is `ask <question>` (Hindsight reflect): one LLM call over the
// memory, scoped to the project and the transverse aliases unless --all.
func newAskCmd(e *env) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ask <question>",
		Short: "Ask the server's LLM a question over the memory (costs one LLM call over many memories)",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			_, home, err := machine()
			if err != nil {
				return err
			}
			cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 3*time.Minute)
			if err != nil {
				return blocked(err)
			}
			q, _ := redact.Text(strings.Join(args, " "))
			var tags []string
			if !all {
				tags = []string{"project:" + currentProject(home, cfg)}
				for _, t := range cfg.Projects.Transverse {
					tags = append(tags, "project:"+t)
				}
			}
			a, err := cl.Reflect(cmd.Context(), cfg.Server.Bank, q, tags, 1000)
			if err != nil {
				return blocked(err)
			}
			res := askResult{Answer: a.Text, InputTokens: a.Usage.InputTokens, OutputTokens: a.Usage.OutputTokens}
			return e.emit("ask", res, func() string {
				return fmt.Sprintf("%s\n\n(%d input, %d output tokens)\n", strings.TrimSpace(res.Answer), res.InputTokens, res.OutputTokens)
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "the whole memory, not only this project and the transverse one")
	return cmd
}

type projectInfo struct {
	Name     string    `json:"name"`
	Memories int       `json:"memories"`
	Last     time.Time `json:"last"`
}

// newProjectsCmd lists the projects of the bank with counts and last
// activity.
func newProjectsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "projects",
		Short: "List the projects in the memory with their memory counts and last activity",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, bank, err := curateClient()
			if err != nil {
				return err
			}
			tags, err := cl.Tags(cmd.Context(), bank, "project:")
			if err != nil {
				return blocked(err)
			}
			list := []projectInfo{}
			for _, t := range tags {
				p := projectInfo{Name: strings.TrimPrefix(t.Tag, "project:"), Memories: t.Count}
				if m, err := cl.Recent(cmd.Context(), bank, t.Tag, "", 1); err == nil && len(m) > 0 {
					p.Last = m[0].UpdatedAt
				}
				list = append(list, p)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Last.After(list[j].Last) })
			return e.emit("projects", list, func() string {
				var b strings.Builder
				for _, p := range list {
					last := "-"
					if !p.Last.IsZero() {
						last = p.Last.Format("2006-01-02")
					}
					fmt.Fprintf(&b, "%-30s %6d  %s\n", safeProject(p.Name), p.Memories, last)
				}
				return b.String()
			})
		},
	}
}
