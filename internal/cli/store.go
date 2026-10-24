package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
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
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, home, err := machine()
			if err != nil {
				return err
			}
			res, failed := storeNote(cmd.Context(), d, home, cfg, sec, kind, proj, strings.Join(args, " "))
			if res.Document == "" {
				return failed // refused before anything was queued
			}
			return e.report("store", res, func() string { return storeText(res) }, failed)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "decision, correction, convention, pitfall, state or lesson")
	cmd.Flags().StringVar(&proj, "project", "", "project tag (default: the current directory's project; a transverse alias for user-wide rules)")
	return cmd
}

// storeNote validates and queues one note, then tries to send it now (the
// outbox keeps it otherwise). Shared by the CLI and memory_store.
func storeNote(ctx context.Context, d paths.Dirs, home string, cfg config.Config, sec config.Secrets, kind, proj, text string) (storeResult, error) {
	if err := checkNote(d, home, cfg, kind, proj, text); err != nil {
		return storeResult{}, err
	}
	text = strings.TrimSpace(text)
	proj = noteProject(home, cfg, proj)
	n := retain.NewNote(cfg.Server.Bank, kind, proj, text, now())
	ob := notesOutbox(d)
	warn, err := ob.Append(n)
	if err != nil {
		return storeResult{}, blocked(err)
	}
	res := storeResult{Status: "queued", Document: n.DocumentID, Project: proj, Kind: kind}
	var failed error
	if warn {
		res.Warning = "the notes outbox is over 50 MB: the server has not accepted notes for a while"
	}
	machineID, _ := retain.MachineID(d.State)
	if _, err := os.Stat(d.MaintenanceLock()); err == nil {
		res.Warning = "maintenance in progress: the note goes out after it"
	} else if cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 15*time.Second); err == nil {
		fctx, cancel := context.WithTimeout(ctx, storeFlushBudget)
		fr, err := ob.Flush(fctx, cl, cfg.Server.Bank, machineID)
		cancel()
		switch {
		case err != nil && !slices.Contains(fr.SentDocs, n.DocumentID):
		case slices.Contains(fr.QuarantinedDocs, n.DocumentID):
			res.Status = "rejected"
			failed = &output.Error{Code: output.Blocked, Message: "the server refused the note (" + fr.LastError + "); it is kept in quarantine: see backsight doctor"}
		case slices.Contains(fr.SentDocs, n.DocumentID):
			res.Status = "stored"
		}
	}
	return res, failed
}

// storeFlushBudget bounds the immediate send: a backlog left by an outage
// is the jobs tick's work, not the caller's wait.
const storeFlushBudget = 10 * time.Second

// TransverseKeyword names the transverse project in tool calls, whatever
// the configured aliases are.
const TransverseKeyword = "transverse"

// checkNote refuses a note before anything leaves the machine: bad kind,
// empty text, a secret, or a session or directory opted out of memory (SPEC
// §7, §13; notes honour pause and opt-outs, 0051).
func checkNote(d paths.Dirs, home string, cfg config.Config, kind, proj, text string) error {
	if !retain.Kinds[kind] {
		return &output.Error{Code: output.Usage, Message: fmt.Sprintf("kind %q: one of %s", kind, kindList())}
	}
	if strings.TrimSpace(text) == "" {
		return &output.Error{Code: output.Usage, Message: "empty text"}
	}
	if !validProject(proj) {
		return &output.Error{Code: output.Usage, Message: "project: one line, at most 64 characters, no control characters"}
	}
	if _, kinds := redact.Text(text); len(kinds) > 0 {
		return &output.Error{Code: output.Usage, Message: "the text holds a secret (" + strings.Join(kinds, ", ") + "): remove it, secrets never go to memory"}
	}
	if sid := os.Getenv("BACKSIGHT_SESSION"); sid != "" {
		if paused, why := session.Paused(d.SessionsDir(), sid); paused {
			return &output.Error{Code: output.Blocked, Message: "memory is paused for this session (" + why + "): nothing stored"}
		}
	}
	cwd, _ := os.Getwd()
	if out, why := project.New(home, cfg.Projects.Transverse).OptedOut(cwd, cfg.Privacy.ExcludePaths); out {
		return &output.Error{Code: output.Blocked, Message: "this directory is excluded from memory (" + why + "): nothing stored"}
	}
	return nil
}

// validProject: a project name given by a caller ("" = the default).
func validProject(p string) bool {
	if len([]rune(p)) > 64 {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// noteProject: the asked project, else the working directory's; any
// transverse alias, or the keyword, maps to the first alias (one pool, SPEC
// §4).
func noteProject(home string, cfg config.Config, proj string) string {
	if proj == "" {
		cwd, _ := os.Getwd()
		proj = project.New(home, cfg.Projects.Transverse).Project(cwd)
	}
	if len(cfg.Projects.Transverse) > 0 && (proj == TransverseKeyword || slices.Contains(cfg.Projects.Transverse, proj)) {
		proj = cfg.Projects.Transverse[0]
	}
	return proj
}

func storeText(res storeResult) string {
	msg := fmt.Sprintf("%s (%s, project %s) %s\n", res.Status, res.Kind, res.Project, res.Document)
	if res.Status == "queued" {
		msg = fmt.Sprintf("queued (%s, project %s) %s: the server did not take it now, the next jobs tick sends it\n", res.Kind, res.Project, res.Document)
	}
	if res.Warning != "" {
		msg += "warning: " + res.Warning + "\n"
	}
	return msg
}
