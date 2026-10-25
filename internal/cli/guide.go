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
	guideTimeout     = 3 * time.Minute
	guideCallTimeout = 30 * time.Second
)

func newGuideCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guide [<topic>|<topic>#<section>]",
		Short: "Read backsight's documentation: the topics, one topic or section, or search them",
		Long: "backsight guide lists the documentation's topics; backsight guide <topic> shows one (through $PAGER at a terminal), " +
			"<topic>#<section> one of its sections. backsight guide search <query> finds the sections that answer a question.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
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

// showDoc prints a topic or one section: through the pager at a terminal,
// plain otherwise.
func showDoc(e *env, c guide.Corpus, ref string) error {
	data := map[string]any{"ref": ref}
	var text string
	if t, ok := c.Topic(ref); ok {
		text = t.Text
		data["topic"], data["title"] = t.Name, t.Title
	} else if s, ok := c.Section(ref); ok && strings.Contains(ref, "#") {
		text = s.Text
		data["topic"], data["heading"] = s.Topic, s.Heading
	} else {
		var names []string
		for _, t := range c.Topics {
			names = append(names, t.Name)
		}
		return &output.Error{Code: output.Usage, Message: fmt.Sprintf("no topic or section %q; topics: %s (backsight guide lists their sections with --json)", ref, strings.Join(names, ", "))}
	}
	if e.json {
		data["text"] = text
		return e.emit("guide", data, nil)
	}
	return page(e, text)
}

// page writes text through $PAGER (default less -FRX) when stdout is a
// terminal, plain otherwise or when no pager runs.
func page(e *env, text string) error {
	if stdoutIsTerminal() {
		args := strings.Fields(os.Getenv("PAGER"))
		if len(args) == 0 {
			args = []string{"less", "-FRX"}
		}
		if bin, err := exec.LookPath(args[0]); err == nil {
			cmd := exec.Command(bin, args[1:]...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(text), e.stdout, e.stderr
			if err := cmd.Run(); err == nil {
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

// guideSearch answers query semantically from the docs bank, refreshing it
// when it holds another docs version, else (or with mode keyword) by the
// local keyword search, saying why. With mode semantic a failure is an
// error instead of a fallback.
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
	hits, refreshed, err := guide.Search(ctx, cl, cfg.Server.Bank, c, query, limit)
	res.Refreshed = refreshed
	if err != nil {
		return keyword(searchFailure(cfg, res.Bank, err))
	}
	if len(hits) == 0 {
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
	switch {
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
	return &cobra.Command{
		Use:   "index",
		Short: "Index the documentation into the server's docs bank (setup and update apply run it; 0 LLM calls)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := guideDocs()
			if err != nil {
				return blocked(err)
			}
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, guideCallTimeout)
			if err != nil {
				return blocked(err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), guideTimeout)
			defer cancel()
			res, err := guide.Index(ctx, cl, cfg.Server.Bank, c)
			if err != nil {
				return blocked(fmt.Errorf("indexing the docs into %s: %w", res.Bank, err))
			}
			return e.emit("guide index", res, func() string { return indexText(res) })
		},
	}
}

func indexText(r guide.IndexResult) string {
	return fmt.Sprintf("docs indexed in bank %s: %d section(s), %d sent, %d retagged, %d unchanged, %d removed\n", r.Bank, r.Sections, r.Sent, r.Retagged, r.Unchanged, r.Deleted)
}
