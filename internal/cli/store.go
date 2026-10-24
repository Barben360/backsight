package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/retain"
)

func notesOutbox(d paths.Dirs) retain.Outbox {
	return retain.Outbox{Path: filepath.Join(d.OutboxDir(), "notes.jsonl"), LockPath: filepath.Join(d.LocksDir(), "outbox.lock"),
		QuarantinePath: filepath.Join(d.QuarantineDir(), "notes.jsonl")}
}

type storeResult struct {
	Status   string `json:"status"` // stored | queued
	Document string `json:"document"`
	Project  string `json:"project"`
	Kind     string `json:"kind"`
	Warning  string `json:"warning,omitempty"`
}

func kindList() string {
	var k []string
	for name := range retain.Kinds {
		k = append(k, name)
	}
	sort.Strings(k)
	return strings.Join(k, ", ")
}

func newStoreCmd(e *env) *cobra.Command {
	var kind, proj string
	cmd := &cobra.Command{
		Use:   "store --kind <kind> <text>",
		Short: "Store one durable fact (a decision, correction, convention, pitfall, state or lesson)",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !retain.Kinds[kind] {
				return &output.Error{Code: output.Usage, Message: fmt.Sprintf("--kind %q: one of %s", kind, kindList())}
			}
			text := strings.TrimSpace(strings.Join(args, " "))
			if text == "" {
				return &output.Error{Code: output.Usage, Message: "empty text"}
			}
			if _, kinds := redact.Text(text); len(kinds) > 0 {
				return &output.Error{Code: output.Usage, Message: "the text holds a secret (" + strings.Join(kinds, ", ") + "): remove it, secrets never go to memory"}
			}
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, home, err := machine()
			if err != nil {
				return err
			}
			if proj == "" {
				cwd, _ := os.Getwd()
				proj = project.New(home, cfg.Projects.Transverse).Project(cwd)
			}
			n := retain.NewNote(cfg.Server.Bank, kind, proj, text, now())
			ob := notesOutbox(d)
			warn, err := ob.Append(n)
			if err != nil {
				return blocked(err)
			}
			res := storeResult{Status: "queued", Document: n.DocumentID, Project: proj, Kind: kind}
			if warn {
				res.Warning = "the notes outbox is over 50 MB: the server has not accepted notes for a while"
			}
			machineID, _ := retain.MachineID(d.State)
			if _, err := os.Stat(d.MaintenanceLock()); err == nil {
				res.Warning = "maintenance in progress: the note goes out after it"
			} else if cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 15*time.Second); err == nil {
				if fr, err := ob.Flush(cmd.Context(), cl, cfg.Server.Bank, machineID); err == nil && fr.Kept == 0 {
					res.Status = "stored"
				}
			}
			return e.emit("store", res, func() string {
				msg := fmt.Sprintf("%s (%s, project %s)\n", res.Status, kind, proj)
				if res.Status == "queued" {
					msg = fmt.Sprintf("queued (%s, project %s): the server is unreachable, the next jobs tick sends it\n", kind, proj)
				}
				if res.Warning != "" {
					msg += "warning: " + res.Warning + "\n"
				}
				return msg
			})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "decision, correction, convention, pitfall, state or lesson")
	cmd.Flags().StringVar(&proj, "project", "", "project tag (default: the current directory's project; a transverse alias for user-wide rules)")
	return cmd
}
