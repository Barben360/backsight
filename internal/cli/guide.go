package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/guide"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
)

// guideDocs is the docs the guide shows and searches; tests replace it.
var guideDocs = guide.Embedded

// Search modes. guideDefaultMode is semantic: it ranked the right section
// higher than the keyword search on the decision 0069 gate (E31).
const (
	modeSemantic     = "semantic"
	modeKeyword      = "keyword"
	guideDefaultMode = modeSemantic
	guideLimit       = 5
	// guideTimeout bounds one guide search or index, a refresh of the docs
	// bank included; each server call has guideCallTimeout.
	guideTimeout = 3 * time.Minute
)

// guideCallTimeout bounds each server call of guide; tests shorten it.
var guideCallTimeout = 30 * time.Second

func newGuideCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guide [<topic>|<topic>#<section>]",
		Short: "Read backsight's documentation: the topics, one topic or section, or search them",
		Long: "backsight guide lists the documentation's topics; backsight guide <topic> shows one (through $PAGER at a terminal), " +
			"<topic>#<section> one of its sections. backsight guide search <query> finds the sections that answer a question.",
		Args: usageArgs(guideArgs),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := guideDocs()
			if err != nil {
				return blocked(err)
			}
			if len(args) == 0 {
				return e.emit("guide", topicList(c), func() string { return topicsText(c) })
			}
			return showDoc(e, c, args[0])
		},
	}
	cmd.AddCommand(newGuideSearchCmd(e), newGuideIndexCmd(e))
	return cmd
}

type guideTopic struct {
	Name     string         `json:"name"`
	Title    string         `json:"title"`
	Sections []guideSection `json:"sections"`
}

type guideSection struct {
	Ref     string `json:"ref"`
	Heading string `json:"heading"`
}

func topicList(c guide.Corpus) map[string]any {
	topics := []guideTopic{}
	for _, t := range c.Topics {
		gt := guideTopic{Name: t.Name, Title: t.Title, Sections: []guideSection{}}
		for _, s := range t.Sections {
			gt.Sections = append(gt.Sections, guideSection{s.Ref(), s.Heading})
		}
		topics = append(topics, gt)
	}
	return map[string]any{"topics": topics, "docs_hash": c.Hash}
}

func topicsText(c guide.Corpus) string {
	var b strings.Builder
	width := 0
	for _, t := range c.Topics {
		width = max(width, len(t.Name))
	}
	b.WriteString("backsight's documentation:\n")
	for _, t := range c.Topics {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, t.Name, t.Title)
	}
	b.WriteString("\nbacksight guide <topic> shows one; backsight guide search <question> finds the sections that answer it.\n")
	return b.String()
}

// guideArgs accepts at most one argument, a topic or topic#section of the
// docs: `guide serach` is a usage error before anything runs.
func guideArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.MaximumNArgs(1)(cmd, args); err != nil || len(args) == 0 {
		return err
	}
	c, err := guideDocs()
	if err != nil {
		return nil // RunE reports the docs that cannot load
	}
	_, err = docRef(c, args[0])
	return err
}

// docRef finds ref in c: a topic, or one section as topic#section.
func docRef(c guide.Corpus, ref string) (map[string]any, error) {
	data := map[string]any{"ref": ref}
	if t, ok := c.Topic(ref); ok {
		data["topic"], data["title"], data["text"] = t.Name, t.Title, t.Text
		return data, nil
	}
	if s, ok := c.Section(ref); ok && strings.Contains(ref, "#") {
		data["topic"], data["heading"], data["text"] = s.Topic, s.Heading, s.Text
		return data, nil
	}
	var names []string
	for _, t := range c.Topics {
		names = append(names, t.Name)
	}
	return nil, &output.Error{Code: output.Usage, Message: fmt.Sprintf("no topic or section %q; topics: %s (backsight guide lists their sections with --json)", ref, strings.Join(names, ", "))}
}

// showDoc prints a topic or one section: through the pager at a terminal,
// plain otherwise.
func showDoc(e *env, c guide.Corpus, ref string) error {
	data, err := docRef(c, ref)
	if err != nil {
		return err
	}
	text, _ := data["text"].(string)
	if e.json {
		return e.emit("guide", data, nil)
	}
	return page(e, text)
}

// page writes text through $PAGER (default less -FRX) when stdout is a
// terminal, plain otherwise or when no pager starts. A pager that started
// is the output, whatever its exit status (the user quit it, a pipe
// closed): a plain copy after it would print the text twice.
func page(e *env, text string) error {
	if stdoutIsTerminal() {
		args := strings.Fields(os.Getenv("PAGER"))
		if len(args) == 0 {
			args = []string{"less", "-FRX"}
		}
		if bin, err := exec.LookPath(args[0]); err == nil {
			cmd := exec.Command(bin, args[1:]...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(text), e.stdout, e.stderr
			if err := cmd.Start(); err == nil {
				_ = cmd.Wait()
				return nil
			}
		}
	}
	_, err := io.WriteString(e.stdout, text)
	return err
}

// guideSearchResult is what guide search answers, and which search did.
type guideSearchResult struct {
	Query     string             `json:"query"`
	Mode      string             `json:"mode"`   // semantic | keyword
	Reason    string             `json:"reason"` // why this mode answered
	Bank      string             `json:"bank,omitempty"`
	Refreshed *guide.IndexResult `json:"refreshed,omitempty"`
	Hits      []guideHit         `json:"hits"`
}

type guideHit struct {
	Ref     string  `json:"ref"`
	Topic   string  `json:"topic"`
	Heading string  `json:"heading"`
	Score   float64 `json:"score"`
	Excerpt string  `json:"excerpt"`
}

func newGuideSearchCmd(e *env) *cobra.Command {
	var mode string
	var limit int
	cmd := &cobra.Command{
		Use:   "search <question>...",
		Short: "Find the documentation sections answering a question (semantic search on the server, keyword search offline)",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if mode != "" && mode != modeSemantic && mode != modeKeyword {
				return usageError{fmt.Errorf("--mode %q: semantic or keyword", mode)}
			}
			c, err := guideDocs()
			if err != nil {
				return blocked(err)
			}
			res, err := guideSearch(cmd.Context(), c, strings.Join(args, " "), mode, limit)
			if err != nil {
				return e.report("guide search", res, func() string { return "" }, err)
			}
			return e.emit("guide search", res, func() string { return guideSearchText(res) })
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", "semantic or keyword only, without the fallback (default: "+guideDefaultMode+", keyword when the server cannot answer)")
	cmd.Flags().IntVar(&limit, "limit", guideLimit, "most sections shown")
	return cmd
}

// hostIndexesDocs is the docs step on a client (setup, update apply): the
// docs bank is the server host's, indexed by its backsight.
const hostIndexesDocs = "the server's host indexes its docs"

// guideSearch answers query semantically from the docs bank, refreshing it
// when it holds another docs version, else (or with mode keyword) by the
// local keyword search, saying why. With mode semantic a failure is an
// error instead of a fallback. A client of a shared server never writes
// the docs bank, its host's: on other docs it searches by keyword.
func guideSearch(ctx context.Context, c guide.Corpus, query, mode string, limit int) (guideSearchResult, error) {
	res := guideSearchResult{Query: query, Hits: []guideHit{}}
	keyword := func(reason string) (guideSearchResult, error) {
		if mode == modeSemantic {
			res.Mode, res.Reason = modeSemantic, reason
			return res, &output.Error{Code: output.Blocked, Message: "semantic search unavailable: " + reason}
		}
		res.Mode, res.Reason = modeKeyword, reason
		for _, h := range c.Keyword(query, limit) {
			res.Hits = append(res.Hits, toGuideHit(h))
		}
		return res, nil
	}
	if mode == modeKeyword || (mode == "" && guideDefaultMode == modeKeyword) {
		return keyword(map[bool]string{true: "asked for (--mode keyword)", false: "the default search"}[mode == modeKeyword])
	}
	cfg, sec, err := loadServerConfig()
	if errors.Is(err, fs.ErrNotExist) || isNeedsUser(err) {
		return keyword("no server configured yet (backsight setup installs one)")
	}
	if err != nil {
		return keyword("config unreadable: " + errMessage(err))
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, guideCallTimeout)
	if err != nil {
		return keyword(err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, guideTimeout)
	defer cancel()
	res.Bank = guide.Bank(cfg.Server.Bank)
	hits, refreshed, err := guide.Search(ctx, cl, cfg.Server.Bank, c, query, limit, cfg.Server.Mode == "local")
	res.Refreshed = refreshed
	if errors.Is(err, guide.ErrNotIndexed) {
		return keyword("the server's docs bank holds the host's docs version")
	}
	if err != nil {
		return keyword(searchFailure(cfg, res.Bank, err))
	}
	if len(hits) == 0 {
		if mode == modeSemantic { // asked for: no hit is the answer
			res.Mode, res.Reason = modeSemantic, "the semantic search found nothing in bank "+res.Bank
			return res, nil
		}
		return keyword("the semantic search found nothing in bank " + res.Bank)
	}
	res.Mode = modeSemantic
	res.Reason = "the docs bank " + res.Bank + " answered"
	if refreshed != nil {
		res.Reason += fmt.Sprintf(", refreshed first to these docs (%d section(s) sent)", refreshed.Sent)
	}
	for _, h := range hits {
		res.Hits = append(res.Hits, toGuideHit(h))
	}
	return res, nil
}

func isNeedsUser(err error) bool {
	var oe *output.Error
	return errors.As(err, &oe) && oe.Code == output.NeedsUser
}

// searchFailure says in a few words why the server could not answer.
func searchFailure(cfg config.Config, bank string, err error) string {
	var apiErr *hindsight.APIError
	var se *guide.SearchError
	var foreign *retain.ForeignBank
	switch {
	case errors.As(err, &foreign):
		return "the docs bank " + bank + " is not backsight's (" + foreign.Reason + "), so backsight leaves it as it is"
	case errors.Is(err, hindsight.ErrUnreachable):
		return "server unreachable at " + cfg.Server.URL
	case errors.Is(err, hindsight.ErrTimeout) || errors.Is(err, context.DeadlineExceeded):
		return "the server at " + cfg.Server.URL + " did not answer in time"
	case errors.As(err, &apiErr) && apiErr.Kind == hindsight.Unauthorized:
		return "the server refused the API key"
	case errors.As(err, &se) && se.Stage == "refresh":
		return "the docs bank " + bank + " is missing or on other docs and could not be refreshed: " + se.Err.Error()
	}
	return err.Error()
}

func toGuideHit(h guide.Hit) guideHit {
	return guideHit{Ref: h.Ref(), Topic: h.Topic, Heading: h.Heading, Score: h.Score, Excerpt: h.Excerpt}
}

func guideSearchText(r guideSearchResult) string {
	var b strings.Builder
	if r.Mode == modeSemantic {
		fmt.Fprintf(&b, "semantic search: %s\n", r.Reason)
	} else {
		fmt.Fprintf(&b, "keyword search (offline): %s\n", r.Reason)
	}
	if len(r.Hits) == 0 {
		b.WriteString("no section matches; backsight guide lists the topics\n")
		return b.String()
	}
	for i, h := range r.Hits {
		fmt.Fprintf(&b, "\n%d. %s (backsight guide %s, %.2f)\n", i+1, h.Heading, h.Ref, h.Score)
		if h.Excerpt != "" {
			fmt.Fprintf(&b, "   %s\n", h.Excerpt)
		}
	}
	return b.String()
}

func newGuideIndexCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Index the documentation into the server's docs bank (setup and update apply run it; 0 LLM calls)",
		Long: "backsight guide index brings the server's docs bank <bank>-docs to this binary's docs: new and edited sections are sent, " +
			"unchanged ones get this docs version's tag, the tags of docs versions older than the last 5 (by first index) are removed, and " +
			"sections no kept docs version holds are deleted. A <bank>-docs that backsight did not make is left as it is. --plan lists " +
			"the documents it would create, resend, retag, untag, prune and delete, and the docs versions it would drop, and changes nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := guideDocs()
			if err != nil {
				return blocked(err)
			}
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			if err := requireLocal(cfg, "guide index"); err != nil {
				return err
			}
			cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, guideCallTimeout)
			if err != nil {
				return blocked(err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), guideTimeout)
			defer cancel()
			if plan {
				p, err := guide.PlanIndex(ctx, cl, cfg.Server.Bank, c)
				if err != nil {
					return blocked(fmt.Errorf("planning the docs index of %s: %w", p.Bank, err))
				}
				return e.emit("guide index", p, func() string { return indexPlanText(p) })
			}
			res, err := guide.Index(ctx, cl, cfg.Server.Bank, c)
			if err != nil {
				return blocked(fmt.Errorf("indexing the docs into %s: %w", res.Bank, err))
			}
			return e.emit("guide index", res, func() string { return indexText(res) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "list the documents index would create, resend, retag, untag, prune and delete, and the docs versions it would drop, without changing anything")
	return cmd
}

func indexText(r guide.IndexResult) string {
	s := fmt.Sprintf("docs indexed in bank %s: %d section(s), %d sent, %d retagged, %d unchanged, %d untagged, %d pruned, %d removed\n",
		r.Bank, r.Sections, r.Sent, r.Retagged, r.Unchanged, r.Untagged, r.Pruned, r.Deleted)
	if len(r.DroppedVersions) > 0 {
		s += "dropped docs version(s) older than the last 5: " + strings.Join(r.DroppedVersions, ", ") + "\n"
	}
	return s
}

func indexPlanText(p guide.IndexPlan) string {
	var b strings.Builder
	if p.Exists {
		fmt.Fprintf(&b, "bank %s would get its settings again, then:\n", p.Bank)
	} else {
		fmt.Fprintf(&b, "bank %s would be created, then:\n", p.Bank)
	}
	for _, l := range []struct {
		name string
		ids  []string
	}{{"create", p.Create}, {"resend", p.Resend}, {"retag", p.Retag}, {"untag", p.Untag}, {"prune", p.Prune}, {"delete", p.Delete},
		{"drop docs versions", p.DropVersions}} {
		fmt.Fprintf(&b, "  %s (%d)", l.name, len(l.ids))
		if len(l.ids) > 0 {
			b.WriteString(": " + strings.Join(l.ids, ", "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "  unchanged: %d\n", p.Unchanged)
	if p.RecordVersions {
		fmt.Fprintf(&b, "  record: %s would hold this docs version's first index time\n", guide.VersionsID)
	}
	b.WriteString("nothing was changed (--plan)\n")
	return b.String()
}
