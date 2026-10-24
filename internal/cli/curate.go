package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
)

const (
	curateTimeout   = 30 * time.Second
	projectsTimeout = 20 * time.Second
)

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
	ID          string     `json:"id"`
	Action      string     `json:"action"` // forgotten | restored | purge_planned | purged | partly_purged
	Sources     []string   `json:"sources,omitempty"`
	Documents   []purgeDoc `json:"documents,omitempty"`
	Undeletable []string   `json:"undeletable,omitempty"` // facts with no document: retired, kept in Hindsight's archive
	Note        string     `json:"note,omitempty"`
}

// purgeDoc is a document a purge deletes, with every memory extracted from
// it: one secret costs the whole conversation segment.
type purgeDoc struct {
	ID       string `json:"id"`
	Memories int    `json:"memories"`
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
			case restore, !purge:
				// Only raw facts can be curated: an observation is retired
				// through its source facts and regenerates without them.
				targets, err := curatable(ctx, cl, bank, id)
				if err != nil {
					return curateError(err, id)
				}
				reason := map[bool]string{true: "restored by the user (backsight forget --restore)", false: "forgotten by the user (backsight forget)"}[restore]
				for _, t := range targets {
					if err := cl.Invalidate(ctx, bank, t, reason, restore); err != nil {
						return curateError(err, t)
					}
				}
				res.Action = map[bool]string{true: "restored", false: "forgotten"}[restore]
				if len(targets) != 1 || targets[0] != id {
					res.Sources = targets
				}
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

// curatable are the raw facts behind id: id itself for a world or
// experience fact, its source facts for an observation (the API curates
// raw facts only).
func curatable(ctx context.Context, cl *hindsight.Client, bank, id string) ([]string, error) {
	m, err := cl.GetMemory(ctx, bank, id)
	if err != nil {
		return nil, err
	}
	if m.Type != "observation" {
		return []string{id}, nil
	}
	var out []string
	for _, s := range m.Sources {
		out = append(out, s.ID)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("observation %s lists no source facts to retire", id)
	}
	return out, nil
}

// purgeMemory plans, and with yes performs, the purge (Hindsight 1.1.0
// has no single-memory delete). The raw facts behind id are read one by
// one, since an observation's source list carries no document. Each live
// fact has its observations cleared while the API still sees it, then is
// retired; last, the documents go, and with them every fact, live or
// archived, extracted from them. A fact with no document stays in the
// archive: the purge says so instead of claiming it.
func purgeMemory(ctx context.Context, cl *hindsight.Client, bank, id string, yes bool) (forgetResult, error) {
	res := forgetResult{ID: id, Action: "purge_planned"}
	m, err := cl.GetMemory(ctx, bank, id)
	if err != nil {
		return res, err
	}
	facts := []hindsight.MemoryDetail{m}
	if m.Type == "observation" {
		facts = nil
		for _, s := range m.Sources {
			f, err := cl.GetMemory(ctx, bank, s.ID)
			if err != nil {
				return res, fmt.Errorf("reading source fact %s: %w", s.ID, err)
			}
			facts = append(facts, f)
			res.Sources = append(res.Sources, s.ID)
		}
		if len(facts) == 0 {
			return res, fmt.Errorf("observation %s lists no source facts to purge", id)
		}
	}
	docs := map[string]bool{}
	for _, f := range facts {
		if f.DocumentID == "" {
			res.Undeletable = append(res.Undeletable, f.ID)
		} else {
			docs[f.DocumentID] = true
		}
	}
	for d := range docs {
		doc, err := cl.GetDocument(ctx, bank, d)
		if err != nil {
			return res, fmt.Errorf("reading document %s: %w", d, err)
		}
		res.Documents = append(res.Documents, purgeDoc{ID: d, Memories: doc.MemoryUnits})
	}
	sort.Slice(res.Documents, func(i, j int) bool { return res.Documents[i].ID < res.Documents[j].ID })
	if !yes {
		return res, nil
	}
	for _, f := range facts {
		if f.State == "invalidated" {
			continue // already archived: only its document can remove it
		}
		if err := cl.ClearObservations(ctx, bank, f.ID); err != nil {
			return res, fmt.Errorf("clearing the observations of %s: %w", f.ID, err)
		}
		if err := cl.Invalidate(ctx, bank, f.ID, "purged by the user (backsight forget --purge)", false); err != nil {
			return res, fmt.Errorf("retiring %s: %w", f.ID, err)
		}
	}
	for _, d := range res.Documents {
		if err := cl.DeleteDocument(ctx, bank, d.ID); err != nil {
			return res, fmt.Errorf("deleting document %s: %w", d.ID, err)
		}
	}
	res.Action = "purged"
	res.Note = "backups taken before now still hold it (backups come with M5): delete or re-key them if the text was a secret"
	if len(res.Undeletable) > 0 {
		res.Action = "partly_purged"
		res.Note = "the facts with no document are retired but stay in Hindsight's archive (no API deletes them); " + res.Note
	}
	return res, nil
}

func curateError(err error, id string) error {
	var apiErr *hindsight.APIError
	// Only a read of the asked id itself means "no such memory"; any other
	// NotFound (a source fact, a document) is the server's state, reported.
	if errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound && apiErr.Method == http.MethodGet &&
		strings.HasSuffix(apiErr.Path, "/memories/"+url.PathEscape(id)) {
		return &output.Error{Code: output.Usage, Message: "no memory " + id}
	}
	return blocked(err)
}

func forgetText(r forgetResult) string {
	switch r.Action {
	case "forgotten", "restored":
		what := r.ID
		if len(r.Sources) > 0 {
			what = fmt.Sprintf("%s (through its %d source fact(s): observations are derived)", r.ID, len(r.Sources))
		}
		if r.Action == "restored" {
			return what + " restored\n"
		}
		return fmt.Sprintf("%s forgotten: out of recall and consolidation (undo: backsight forget --restore %s)\n", what, r.ID)
	}
	var b strings.Builder
	verb := map[bool]string{true: "deleted", false: "would delete"}[r.Action != "purge_planned"]
	fmt.Fprintf(&b, "%s: memory %s, %d source fact(s), document(s):\n", verb, r.ID, len(r.Sources))
	for _, d := range r.Documents {
		fmt.Fprintf(&b, "- %s (with all %d memories extracted from it)\n", d.ID, d.Memories)
	}
	if len(r.Undeletable) > 0 {
		fmt.Fprintf(&b, "not deletable (no document, retired only): %s\n", strings.Join(r.Undeletable, ", "))
	}
	if r.Note != "" {
		b.WriteString(r.Note + "\n")
	}
	return b.String()
}

type scanFinding struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	State   string   `json:"state"` // live | forgotten
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
			// A plain forget keeps the text on the server (archive, exports):
			// soft-forgotten facts are scanned too. Observations are never
			// archived.
			passes := []struct{ ft, state string }{{"world", ""}, {"experience", ""}, {"observation", ""}, {"world", "invalidated"}, {"experience", "invalidated"}}
			for _, p := range passes {
				err := cl.EachMemory(cmd.Context(), bank, p.ft, p.state, func(m hindsight.Memory) error {
					red, kinds := redact.Text(m.Text)
					if len(kinds) > 0 {
						findings = append(findings, scanFinding{ID: m.ID, Type: p.ft, State: map[bool]string{true: "forgotten", false: "live"}[p.state != ""],
							Project: recall.ProjectOf(m.Tags), Kinds: kinds, Preview: clip(red, 100)})
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
					fmt.Fprintf(&b, "%s (%s, %s, %s, %s): %s\n  backsight forget %s --purge\n", f.ID, f.Type, f.State, f.Project, strings.Join(f.Kinds, ", "), f.Preview, f.ID)
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

// newAskCmd is `ask <question>` (Hindsight reflect): an agentic LLM loop
// over the memory, scoped to the project and the transverse aliases unless --all.
func newAskCmd(e *env) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ask <question>",
		Short: "Ask the server's LLM a question over the memory (an LLM loop over many memories: costly, tokens shown)",
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
			// Last activity is one read per project: four at a time, all
			// within one bound; a project whose read fails shows "-".
			list := make([]projectInfo, len(tags))
			ctx, cancel := context.WithTimeout(cmd.Context(), projectsTimeout)
			defer cancel()
			var wg sync.WaitGroup
			sem := make(chan struct{}, 4)
			for i, t := range tags {
				list[i] = projectInfo{Name: strings.TrimPrefix(t.Tag, "project:"), Memories: t.Count}
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					if m, err := cl.Recent(ctx, bank, t.Tag, "", 1); err == nil && len(m) > 0 {
						list[i].Last = m[0].UpdatedAt
					}
				}()
			}
			wg.Wait()
			sort.SliceStable(list, func(i, j int) bool { return list[i].Last.After(list[j].Last) })
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
