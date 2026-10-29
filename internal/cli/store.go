package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	"github.com/Barben360/backsight/internal/state"
)

func notesOutbox(d paths.Dirs) retain.Outbox { return retain.NotesOutbox(d) }

// notesOutboxFor is the notes outbox with the size limits of hooks.outbox_warn_mb
// and hooks.outbox_max_mb: use it wherever notes are appended.
func notesOutboxFor(d paths.Dirs, cfg config.Config) retain.Outbox {
	return retain.NotesOutbox(d).WithLimitsMB(cfg.Hooks.OutboxWarnMB, cfg.Hooks.OutboxMaxMB)
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
			d, home, err := machine()
			if err != nil {
				return err
			}
			generation := sendsGeneration(d) // before the config it goes with
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			cwd, _ := os.Getwd()
			res, failed := storeNote(cmd.Context(), d, home, cfg, sec, generation, os.Getenv("BACKSIGHT_SESSION"), cwd, kind, proj, strings.Join(args, " "))
			if res.Document == "" {
				return failed // refused before anything was queued
			}
			return e.report("store", res, func() string { return storeText(res) }, failed)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "decision, correction, convention, pitfall, state or lesson")
	cmd.Flags().StringVar(&proj, "project", "", `project tag (default: the current directory's project; "transverse" only for a user-wide rule)`)
	return cmd
}

// sendsGeneration is this PC's sends generation now, read before the
// config a process sends with: once connect started the sends over, a
// process holding the old config sends nothing (sendsGate). -1 when
// state.json cannot be read: no send from this process.
func sendsGeneration(d paths.Dirs) int {
	st, err := state.Load(d.StateFile())
	if err != nil {
		return -1
	}
	return st.SendsGeneration
}

// sendsGate refuses once this PC's sends generation is no longer
// generation: connect started the sends over for another server since
// the process loaded its config (retain.ErrStartedOver).
func sendsGate(d paths.Dirs, generation int) func() error {
	return func() error {
		st, err := state.Load(d.StateFile())
		if err != nil {
			return err
		}
		if st.SendsGeneration != generation {
			return retain.ErrStartedOver
		}
		return nil
	}
}

// noteOptions is what a correction from memory_credit adds to its note:
// the memories it replaces and the evidence; checked when the caller ran
// checkNote on the same note before asking the server anything.
type noteOptions struct {
	Replaces []string
	Evidence string
	checked  bool
}

// storeNote validates and queues one note, then tries to send it now (the
// outbox keeps it otherwise), unless connect started this PC's sends over
// since generation was read with cfg: then the next jobs tick sends it.
// Shared by the CLI, memory_store and memory_credit (opts: at most one).
func storeNote(ctx context.Context, d paths.Dirs, home string, cfg config.Config, sec config.Secrets, generation int, sid, cwd, kind, proj, text string, opts ...noteOptions) (storeResult, error) {
	if !slices.ContainsFunc(opts, func(o noteOptions) bool { return o.checked }) {
		if err := checkNote(d, home, cfg, sid, cwd, kind, proj, text); err != nil {
			return storeResult{}, err
		}
	}
	text = strings.TrimSpace(text)
	proj, err := noteTarget(cfg, proj, project.New(home, cfg.Projects.Transverse).Project(cwd), "--project transverse")
	if err != nil {
		return storeResult{}, err
	}
	n := retain.NewNote(cfg.Server.Bank, kind, proj, text, now())
	n.Session = sid
	for _, o := range opts {
		n.Replaces, n.Evidence = o.Replaces, o.Evidence
	}
	ob := notesOutboxFor(d, cfg)
	warn, err := ob.Append(n)
	if err != nil {
		return storeResult{}, blocked(err)
	}
	res := storeResult{Status: "queued", Document: n.DocumentID, Project: proj, Kind: kind}
	var failed error
	if warn {
		res.Warning = fmt.Sprintf("the notes outbox is over %d MB: the server has not accepted notes for a while", cfg.Hooks.OutboxWarnMB)
	}
	machineID, _ := retain.MachineID(d.State)
	if state.MaintenanceHeld(d.MaintenanceLock()) {
		res.Warning = "maintenance in progress: the note goes out after it"
	} else if otherServer(d, cfg) != nil {
		res.Warning = "a different server answers at the joined address: the note waits on this PC; run backsight doctor"
	} else if cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 15*time.Second); err == nil {
		fctx, cancel := context.WithTimeout(ctx, storeFlushBudget)
		ob.Gate = sendsGate(d, generation)
		fr, err := ob.Flush(fctx, cl, cfg.Server.Bank, machineID)
		cancel()
		if errors.Is(err, retain.ErrStartedOver) {
			res.Warning = "this PC joined another server since this process started: the note goes out with the next jobs tick"
		}
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

// otherServer is the mismatch the last check recorded: another server
// answers at the joined address (Flusher.Reconcile records it, decision
// 0073), and nothing is sent or written to it; nil otherwise. Read from
// state.json: no request on the hot path. Only a client (server.mode
// remote) holds: a record left after a switch to local holds nothing.
func otherServer(d paths.Dirs, cfg config.Config) *retain.ServerMismatch {
	if cfg.Server.Mode != "remote" {
		return nil
	}
	st, err := state.Load(d.StateFile())
	if err != nil || st.RemoteServer == nil || st.RemoteServer.Mismatch == "" {
		return nil
	}
	return &retain.ServerMismatch{Want: st.RemoteServer.ID, Got: st.RemoteServer.Mismatch}
}

// otherServerRefusal refuses a command that writes to the server while
// another server answers at the joined address, before any write.
func otherServerRefusal(d paths.Dirs, cfg config.Config) error {
	if mm := otherServer(d, cfg); mm != nil {
		return &output.Error{Code: output.Blocked, Message: mm.Error() + "; nothing was changed (backsight doctor)"}
	}
	return nil
}

// storeFlushBudget bounds the immediate send: a backlog left by an outage
// is the jobs tick's work, not the caller's wait.
const storeFlushBudget = 10 * time.Second

// TransverseKeyword names the transverse project in tool calls, whatever
// the configured aliases are.
const TransverseKeyword = retain.TransverseKeyword

// noteChecked, when set (tests), runs at each checkNote.
var noteChecked func()

// checkNote refuses a note before anything leaves the machine: bad kind,
// empty text, a secret, or a session or directory opted out of memory (SPEC
// §7, §13; notes honour pause and opt-outs, 0051).
func checkNote(d paths.Dirs, home string, cfg config.Config, sid, cwd, kind, proj, text string) error {
	if noteChecked != nil {
		noteChecked()
	}
	if !retain.Kinds[kind] {
		return &output.Error{Code: output.Usage, Message: fmt.Sprintf("kind %q: one of %s", kind, kindList())}
	}
	if strings.TrimSpace(text) == "" {
		return &output.Error{Code: output.Usage, Message: "empty text"}
	}
	if !cfg.Components.Write {
		return &output.Error{Code: output.Blocked, Message: "components.write is off in config.toml: another integration writes to this bank; nothing stored"}
	}
	if !validProject(proj) {
		return &output.Error{Code: output.Usage, Message: "project: one line, at most 64 characters, no control characters"}
	}
	if _, kinds := redact.Text(text); len(kinds) > 0 {
		return &output.Error{Code: output.Usage, Message: "the text holds a secret (" + strings.Join(kinds, ", ") + "): remove it, secrets never go to memory"}
	}
	if sid != "" {
		if paused, why := session.Paused(d.SessionsDir(), sid); paused {
			return &output.Error{Code: output.Blocked, Message: "memory is paused for this session (" + why + "): nothing stored"}
		}
	}
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

// noteTarget is the project a note goes to: the one asked for, else the
// current one. The transverse pool is shown in every project, so a note
// goes there only when asked for by name (the keyword or an alias, mapped
// to the first alias: one pool, SPEC §4); a note without a project in a
// directory of the pool (home, ~/.claude) is refused, naming how to ask
// (decision 0072 M6).
func noteTarget(cfg config.Config, asked, current, how string) (string, error) {
	switch {
	case asked != "":
		return canonicalProject(cfg, asked), nil
	case isPool(cfg, current):
		return "", &output.Error{Code: output.Usage, Message: "this directory belongs to the transverse pool, whose notes reach every project: " +
			"add " + how + " for a user-wide rule, or name the project the fact belongs to; nothing stored"}
	}
	return current, nil
}

// isPool: p names the transverse pool (the keyword or an alias).
func isPool(cfg config.Config, p string) bool {
	return retain.IsPool(cfg.Projects.Transverse, p)
}

// canonicalProject maps the keyword and every transverse alias to the first
// alias (one pool, SPEC §4); any other name is kept.
func canonicalProject(cfg config.Config, p string) string {
	return retain.CanonicalProject(cfg.Projects.Transverse, p)
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
