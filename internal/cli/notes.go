package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
)

// noteLine is one stored note as `memory notes` shows it.
type noteLine struct {
	Document string    `json:"document"`
	Created  time.Time `json:"created_at"`
	Project  string    `json:"project"`
	Kind     string    `json:"kind"`
	Session  string    `json:"session,omitempty"` // "" when stored from a shell, or before sessions were recorded
	Text     string    `json:"text"`              // redacted, clipped; "" once deleted
	Deleted  bool      `json:"deleted,omitempty"`
}

type notesResult struct {
	Since time.Time  `json:"since"`
	Notes []noteLine `json:"notes"`
}

const notesTextMax = 300

// newMemoryNotesCmd lists the notes stored recently (memory_store, store,
// /backsight:remember): the memory tools are allowed without a prompt, so
// what they wrote is easy to review (decision 0072 M6).
func newMemoryNotesCmd(e *env) *cobra.Command {
	var limit, days int
	var proj string
	cmd := &cobra.Command{
		Use:   "notes",
		Short: "List the notes stored recently, newest first, with their project, session and time",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > 200 {
				return &output.Error{Code: output.Usage, Message: "--limit: 1 to 200"}
			}
			if days < 1 {
				return &output.Error{Code: output.Usage, Message: "--days: at least 1"}
			}
			cfg, _, err := loadServerConfig()
			if err != nil {
				return err
			}
			cl, bank, err := curateClient()
			if err != nil {
				return err
			}
			if proj != "" {
				proj = canonicalProject(cfg, proj)
			}
			res := notesResult{Since: now().UTC().AddDate(0, 0, -days), Notes: []noteLine{}}
			docs, err := cl.TaggedDocumentsSince(cmd.Context(), bank, res.Since, "source:note")
			if err != nil {
				return blocked(err)
			}
			for _, d := range docs {
				if len(res.Notes) == limit {
					break
				}
				n := noteLine{Document: d.ID, Created: d.CreatedAt, Project: recall.ProjectOf(d.Tags), Session: d.Metadata["session"]}
				if proj != "" && n.Project != proj {
					continue
				}
				for _, t := range d.Tags {
					if k, ok := strings.CutPrefix(t, "kind:"); ok {
						n.Kind = k
					}
				}
				doc, err := cl.GetDocument(cmd.Context(), bank, d.ID)
				var apiErr *hindsight.APIError
				switch {
				case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
					n.Deleted = true
				case err != nil:
					return blocked(err)
				default:
					red, _ := redact.Text(doc.OriginalText)
					n.Text = clip(red, notesTextMax)
				}
				res.Notes = append(res.Notes, n)
			}
			return e.emit("memory notes", res, func() string { return memoryNotesText(res, days) })
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "most notes listed (1 to 200)")
	cmd.Flags().IntVar(&days, "days", 7, "notes stored in the last this many days")
	cmd.Flags().StringVar(&proj, "project", "", `only this project's notes ("transverse" for the transverse pool)`)
	return cmd
}

func memoryNotesText(res notesResult, days int) string {
	if len(res.Notes) == 0 {
		return fmt.Sprintf("no note stored in the last %d days\n", days)
	}
	var b strings.Builder
	for _, n := range res.Notes {
		sess := "no session"
		if n.Session != "" {
			sess = "session " + n.Session
		}
		fmt.Fprintf(&b, "%s  project %s  %s  %s  %s\n", n.Created.Local().Format("2006-01-02 15:04"), n.Project, sess, n.Kind, n.Document)
		if n.Deleted {
			b.WriteString("  (deleted)\n")
		} else {
			fmt.Fprintf(&b, "  %s\n", n.Text)
		}
	}
	b.WriteString("A wrong note: find its memories with `backsight search \"<its words>\"`, then `backsight forget <id>` (`--purge` deletes it).\n")
	return b.String()
}
