package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// now is the clock of retain commands; tests replace it.
var now = time.Now

// sendsConfigLoaded runs once a sending process read its sends
// generation, then its config; a test seam (a whole connect happening
// then), a no-op in the binary.
var sendsConfigLoaded = func() {}

func newRetainCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "retain", Short: "Send settled sessions to the memory", Args: cobra.NoArgs}
	cmd.AddCommand(newRetainFlushCmd(e), newRetainRetryCmd(e), newRetainBackfillCmd(e), newRetainReattributeCmd(e), newRetainPurgeAgainCmd(e))
	return cmd
}

// newFlusher builds the flusher for this machine and records retain_since
// the first time live retain runs.
// Only jobs tick records retain_since (decision 0050); other callers use a
// transient boundary of now, so history stays with backfill either way.
// generation is sendsGeneration's, read before cfg and sec were: a
// connect that wrote another server's config since makes the flusher
// send nothing (-1: it never sends).
func newFlusher(d paths.Dirs, home string, cfg config.Config, sec config.Secrets, generation int, setSince bool) (*retain.Flusher, error) {
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 60*time.Second)
	if err != nil {
		return nil, blocked(err)
	}
	machine, err := retain.MachineID(d.State)
	if err != nil {
		return nil, blocked(err)
	}
	idx, err := retain.LoadSentIndex(d.SentIndexDir())
	if err != nil {
		return nil, blocked(err)
	}
	since := now().UTC().Truncate(time.Second)
	var joined *state.RemoteServer
	if setSince {
		err = state.Update(d, "jobs tick", func(s *state.State) error {
			if s.RetainSince == nil {
				s.RetainSince = &since
			}
			since, joined = *s.RetainSince, s.RemoteServer
			return nil
		})
	} else if st, lerr := state.Load(d.StateFile()); lerr == nil {
		if st.RetainSince != nil {
			since = *st.RetainSince
		}
		joined = st.RemoteServer
	} else {
		err = lerr
	}
	if err != nil {
		return nil, blocked(err)
	}
	// A client checks it still reaches the server it joined (decision 0073).
	serverID := ""
	if joined != nil && cfg.Server.Mode == "remote" {
		serverID = joined.ID
	}
	res := project.New(home, cfg.Projects.Transverse)
	return &retain.Flusher{Client: cl, Bank: cfg.Server.Bank, Dirs: d, Resolver: res,
		Exclude: cfg.Privacy.ExcludePaths, Headless: cfg.Privacy.SkipsEntrypoint, MachineID: machine, Version: buildinfo.Version, Now: now, RetainSince: since, Index: idx,
		ServerID: serverID, SendsGeneration: generation,
		// A rebuilt subagent report follows SubagentStop's rules (issue 285).
		Outbox: notesOutboxFor(d, cfg), NoReports: !cfg.Components.Write, ReportProject: func(cwd string) string { return canonicalProject(cfg, res.Project(cwd)) },
		Attribution: attributor(d, cfg, res)}, nil
}

// attributor applies the path rule with this configuration (decision 0072).
func attributor(d paths.Dirs, cfg config.Config, res *project.Resolver) retain.Attributor {
	return retain.Attributor{Resolver: res, Transverse: cfg.Projects.Transverse, Owners: cfg.Projects.OwnOwners, Deny: cfg.Projects.DenyOwners,
		Exclude: cfg.Privacy.ExcludePaths, Dir: d.AttributionDir()}
}

// attributionEvent is the telemetry of one recorded turn.
func attributionEvent(e retain.TurnEntry) telemetry.Attribution {
	return telemetry.Attribution{Turn: e.Turn, Subagent: e.Subagent(), Base: e.Base, Applied: e.Applied, PathRule: e.PathRule, Source: e.Source}
}

// transcripts lists the session transcripts of this machine's Claude Code.
// transcripts are the sessions of the Claude Code profile backsight is wired
// to: CLAUDE_CONFIG_DIR when set (a hook, or the user's shell), else the
// profile client install recorded, else ~/.claude. A scheduled job has no
// CLAUDE_CONFIG_DIR: the recorded profile is how it reads the right one.
func transcripts(home string) []string {
	getenv := os.Getenv
	if os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		if d, _, err := machine(); err == nil {
			if st, err := state.Load(d.StateFile()); err == nil && st.Client != nil && st.Client.Profile != "" {
				getenv = func(k string) string {
					if k == "CLAUDE_CONFIG_DIR" {
						return st.Client.Profile
					}
					return os.Getenv(k)
				}
			}
		}
	}
	loc := claudecode.Locate(home, getenv)
	files, _ := filepath.Glob(filepath.Join(loc.Projects, "*", "*.jsonl"))
	sort.Strings(files)
	return files
}

// A flush to retry is "retry": "queued" is a note or report put in the
// outbox, which is the normal path.
var telemetryStatus = map[string]string{retain.Sent: "ok", retain.Retry: "retry", retain.Quarantined: "quarantined"}

// flushAll runs the flusher over every transcript and logs telemetry for
// what was attempted.
func flushAll(cmd *cobra.Command, f *retain.Flusher, files []string, opt retain.Options, d paths.Dirs) []retain.Result {
	tw := telemetry.New(d.State, telemetry.Common{BinaryVersion: buildinfo.Version})
	var out []retain.Result
	for _, file := range files {
		start := time.Now()
		r := f.FlushFile(cmd.Context(), file, opt)
		for _, ev := range retainEvents(r, int(time.Since(start).Milliseconds())) {
			tw.Emit(ev)
		}
		if len(r.Unknown) > 0 {
			tw.Emit(telemetry.Raw{Name: "transcript_unknown", Fields: map[string]any{"types": r.Unknown}})
		}
		reportUnconfirmed(d.State, cmd.ErrOrStderr(), r)
		if len(r.Turns) > 0 {
			sw := telemetry.New(d.State, telemetry.Common{Session: r.Session, BinaryVersion: buildinfo.Version})
			for _, t := range r.Turns {
				sw.Emit(attributionEvent(t))
			}
		}
		out = append(out, r)
		if r.Stop {
			break // the server or setup is failing: the next run retries
		}
	}
	return out
}

// reportUnconfirmed reports the replacing parts a flush gave up on
// unconfirmed, retain.PendingLimit after their send
// (retain.Result.Unconfirmed): one replace_unconfirmed event each, and a
// line on errw (the scheduler's log for a tick). Their records may be lost
// on the server; verify --live compares. The event carries the session in
// the writer's common field (telemetry.Raw cannot set it), which doctor's
// replace-unconfirmed check names.
func reportUnconfirmed(stateDir string, errw io.Writer, r retain.Result) {
	if len(r.Unconfirmed) == 0 {
		return
	}
	tw := telemetry.New(stateDir, telemetry.Common{Session: r.Session, BinaryVersion: buildinfo.Version})
	for _, u := range r.Unconfirmed {
		tw.Emit(telemetry.Raw{Name: "replace_unconfirmed", Fields: map[string]any{"document": u.Document, "reason": u.Reason}})
		_, _ = fmt.Fprintf(errw, "backsight: session %s: %s was never confirmed on the server (%s); backsight verify --live compares it\n", r.Session, u.Document, u.Reason)
	}
}

// retainEvents are one flush's telemetry: one event per document it wrote
// (a session's records of each project have their own document, decision
// 0071 C2a), then one for the document whose send failed, or for the
// session's home document when the flush failed before sending (reading
// the server's position). ms is the whole flush's duration.
func retainEvents(r retain.Result, ms int) []telemetry.Retain {
	var out []telemetry.Retain
	for _, d := range r.Documents {
		if d != r.FailedDocument {
			out = append(out, telemetry.Retain{Document: d, Items: 1, Status: "ok", MS: ms})
		}
	}
	st, ok := telemetryStatus[r.Status]
	switch {
	case r.FailedDocument != "" && st == "ok":
		st = "retry" // partly sent: the rest waits for the next run
	case !ok || st == "ok":
		return out
	}
	doc := r.FailedDocument
	if doc == "" {
		doc = "conversation:" + r.Session
	}
	return append(out, telemetry.Retain{Document: doc, Items: 1, Status: st, MS: ms})
}

type flushReport struct {
	Results []retain.Result `json:"results"`
	Counts  map[string]int  `json:"counts"`
	Epoch   string          `json:"epoch,omitempty"`
}

// reconcile brings the cursors to the bank's restore epoch before any flush
// (decision 0044); when it cannot, the flusher holds every session until a
// later run. It returns a line for the report, "" when nothing changed.
func reconcile(ctx context.Context, f *retain.Flusher) string {
	rec, err := f.Reconcile(ctx)
	switch {
	case err != nil:
		return "held: " + err.Error()
	case rec.From == rec.To:
		return ""
	}
	line := fmt.Sprintf("restore epoch %d -> %d: %d session(s) resume from the server", rec.From, rec.To, len(rec.Resend))
	if len(rec.Purged) > 0 {
		line += fmt.Sprintf(", %d purged document(s) the server held again purged again", len(rec.Purged))
	}
	if len(rec.Reports) > 0 {
		line += fmt.Sprintf(", %d subagent report(s) queued again", len(rec.Reports))
	}
	return line
}

func reportFlush(results []retain.Result) (flushReport, error) {
	rep := flushReport{Counts: map[string]int{}}
	var failed error
	for _, r := range results {
		rep.Counts[r.Status]++
		if r.Status != retain.Nothing && r.Status != retain.Backfill {
			rep.Results = append(rep.Results, r)
		}
		if r.Status == retain.Retry && failed == nil {
			failed = &output.Error{Code: output.Partial, Message: "some sessions could not be sent now; the next jobs tick retries them"}
		}
		if r.NeedsFix {
			failed = &output.Error{Code: output.Blocked, Message: "the server refused backsight (" + r.Detail + "): run backsight doctor"}
		}
	}
	return rep, failed
}

func flushText(rep flushReport) string {
	var b strings.Builder
	for _, r := range rep.Results {
		fmt.Fprintf(&b, "%-11s %s", r.Status, r.Session)
		if r.Records > 0 {
			fmt.Fprintf(&b, " (%s, %d records, segment %d)", r.Project, r.Records, r.Segment)
		}
		if r.Detail != "" {
			fmt.Fprintf(&b, ": %s", r.Detail)
		}
		b.WriteString("\n")
	}
	var parts []string
	for _, s := range []string{retain.Sent, retain.Waiting, retain.Nothing, retain.Backfill, retain.OptedOut, retain.Retry, retain.Quarantined, retain.Held, retain.Locked, retain.Maintenance} {
		if n := rep.Counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	if len(parts) == 0 {
		parts = []string{"no transcript"}
	}
	if rep.Epoch != "" {
		parts = append(parts, rep.Epoch)
	}
	b.WriteString(strings.Join(parts, ", ") + "\n")
	return b.String()
}

func newRetainFlushCmd(e *env) *cobra.Command {
	var session string
	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Send the unsent turns of every session (or one) now, without the 30-minute wait",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			generation := sendsGeneration(d) // before the config it goes with
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			sendsConfigLoaded()
			f, err := newFlusher(d, home, cfg, sec, generation, false)
			if err != nil {
				return err
			}
			files := transcripts(home)
			opt := retain.Options{Force: true}
			if session != "" {
				var match []string
				for _, file := range files {
					if strings.TrimSuffix(filepath.Base(file), ".jsonl") == session {
						match = append(match, file)
					}
				}
				if len(match) == 0 {
					return &output.Error{Code: output.Usage, Message: "no transcript for session " + session}
				}
				files, opt.IgnoreSince = match, true
			}
			epoch := reconcile(cmd.Context(), f)
			rep, failed := reportFlush(flushAll(cmd, f, files, opt, d))
			rep.Epoch = epoch
			return e.report("retain flush", rep, func() string { return flushText(rep) }, failed)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "flush only this session id (even one older than live retain)")
	return cmd
}

type retryResult struct {
	Sessions []string `json:"sessions"`
	Notes    int      `json:"notes"`
}

func newRetainRetryCmd(e *env) *cobra.Command {
	var session string
	var notesOnly bool
	cmd := &cobra.Command{
		Use:   "retry-quarantine",
		Short: "Give quarantined sessions and notes another try on the next flush",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d, _, err := machine()
			if err != nil {
				return err
			}
			var res retryResult
			if !notesOnly {
				if res.Sessions, err = retain.ReleaseQuarantine(d.CursorsDir(), session); err != nil {
					return blocked(err)
				}
			}
			if session == "" {
				if res.Notes, err = notesOutbox(d).Requeue(); err != nil {
					return blocked(err)
				}
			}
			if res.Sessions == nil {
				res.Sessions = []string{}
			}
			return e.emit("retain retry-quarantine", res, func() string {
				return fmt.Sprintf("%d session(s) will resync from the server, %d note(s) queued again; they go out on the next flush or tick\n", len(res.Sessions), res.Notes)
			})
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "only this session")
	cmd.Flags().BoolVar(&notesOnly, "notes", false, "only the notes")
	return cmd
}
