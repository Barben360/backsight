package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
)

// indexMigrateBudget bounds `stats index migrate`, which `update apply`
// runs: a lock or a disk that hangs never holds the update.
var indexMigrateBudget = 60 * time.Second

// indexMigrate is index.Migrate; tests observe the context it gets.
var indexMigrate = index.Migrate

// newStatsIndexCmd is `stats index`: the metrics index as it is, read
// only, with `rebuild` and the hidden `migrate` beneath it.
func newStatsIndexCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Show the local metrics index: its file, how current it is, the lines it skipped (reads only)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			o := indexOptions(d, home, nil)
			o.Now = now
			st, err := index.Inspect(cmd.Context(), o)
			if err != nil {
				return indexError(err)
			}
			return e.emit("stats index", st, func() string { return indexStatusText(st) })
		},
	}
	cmd.AddCommand(newStatsIndexRebuildCmd(e), newStatsIndexMigrateCmd(e))
	return cmd
}

// indexStatusText is `stats index`: names, counts and times only, never a
// path or an error's text.
func indexStatusText(s index.Status) string {
	var b strings.Builder
	line := func(label, format string, args ...any) {
		fmt.Fprintf(&b, "%-12s %s\n", label, fmt.Sprintf(format, args...))
	}
	switch {
	case !s.Exists:
		line("Index", "not built yet: the next jobs tick builds it, or backsight stats index rebuild")
	case !s.Readable:
		line("Index", "%s (%s, unreadable)", s.Using, sizeText(s.Size))
	default:
		line("Index", "%s (%s, schema %d of %d)", s.Using, sizeText(s.Size), s.Schema, s.Head)
	}
	if s.Newer {
		line("Newer", "a newer backsight's index: this one uses %s", s.Using)
	}
	if !s.StoppedAt.IsZero() {
		line("Stopped", "unreadable since %s: run backsight stats index rebuild", s.StoppedAt.UTC().Format(time.RFC3339))
	}
	if s.Exists && s.Readable {
		if s.IngestedAt.IsZero() {
			line("Ingested", "nothing yet")
		} else {
			line("Ingested", "%s UTC (lag %d s)", s.IngestedAt.UTC().Format("2006-01-02 15:04:05"), s.LagSeconds)
		}
		behind, gone := 0, 0
		for _, src := range s.Sources {
			switch {
			case src.Missing:
				gone++
			case src.Behind:
				behind++
			}
		}
		line("Sources", "%d file(s), %d behind, %d gone (rows kept)", len(s.Sources), behind, gone)
		if s.Errors30d == 0 {
			line("Errors", "none in the last 30 days")
		} else {
			line("Errors", "%d line(s) skipped in the last 30 days (%s)", s.Errors30d, counts(s.ErrorsByReason))
		}
	}
	line("Side files", "%s", namesOrNone(s.SideFiles))
	line("Set aside", "%s", namesOrNone(s.AsideFiles))
	if s.Filesystem.Kind != "" && s.Filesystem.Kind != index.FSLocal {
		name := s.Filesystem.Name
		if name == "" {
			name = string(s.Filesystem.Kind)
		}
		line("Folder", "%s (network or synced): the index runs without its write-ahead log", name)
	}
	return b.String()
}

func namesOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// newStatsIndexRebuildCmd is `stats index rebuild`: the index built again
// from the files on disk, beside the old one.
func newStatsIndexRebuildCmd(e *env) *cobra.Command {
	var plan, yes bool
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Build the metrics index again from the files on disk (--plan lists the months that would be lost)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			o := indexOptions(d, home, e.stderr)
			o.Now = now
			res, err := index.Rebuild(cmd.Context(), o, index.RebuildOptions{Plan: plan, Yes: yes})
			if errors.Is(err, index.ErrNeedsYes) {
				return e.report("stats index rebuild", res, func() string { return indexRebuildText(res, false) }, indexError(err))
			}
			if err != nil {
				return indexError(err)
			}
			return e.emit("stats index rebuild", res, func() string { return indexRebuildText(res, !res.Plan) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "list what would be lost and change nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "accept losing the months whose files are gone")
	return cmd
}

// indexRebuildText is a rebuild's result: the months a rebuild loses, then, for
// a rebuild that ran, its line count.
func indexRebuildText(r index.RebuildResult, done bool) string {
	var b strings.Builder
	if done {
		fmt.Fprintf(&b, "metrics index rebuilt: %d lines in %.1f s\n", r.Lines, float64(r.MS)/1000)
		if r.StopNotCleared {
			b.WriteString("the stop of the automatic recovery could not be lifted: run the rebuild again\n")
		}
		return b.String()
	}
	switch {
	case r.LostUnknown:
		b.WriteString("the current index cannot be read: what it holds cannot be listed\n")
	case len(r.Lost) == 0:
		b.WriteString("no month would be lost\n")
	}
	for _, l := range r.Lost {
		month := l.Month
		if month == "" {
			month = "-"
		}
		fmt.Fprintf(&b, "%s  %s  %d row(s) would be lost\n", month, l.SourceFile, l.Rows)
	}
	return b.String()
}

// newStatsIndexMigrateCmd is the hidden `stats index migrate`, which
// `update apply` runs before the new binary's first command: it applies
// the pending migrations of an existing index and ingests nothing.
func newStatsIndexMigrateCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:    "migrate",
		Short:  "Apply the pending migrations of the metrics index (ingests nothing)",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), indexMigrateBudget)
			defer cancel()
			o := indexOptions(d, home, nil)
			o.Now = now
			res, err := indexMigrate(ctx, o, plan)
			if err != nil {
				return indexError(err)
			}
			return e.emit("stats index migrate", res, func() string { return migrateText(res) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "list the pending migrations and apply none")
	return cmd
}

func migrateText(r index.MigrateResult) string {
	switch {
	case !r.Built:
		return "metrics index: not built yet\n"
	case r.Newer:
		return "metrics index: a newer backsight's index: nothing to migrate\n"
	case len(r.Applied) > 0:
		return "metrics index: applied " + strings.Join(r.Applied, ", ") + "\n"
	case len(r.Pending) > 0:
		return "metrics index: pending " + strings.Join(r.Pending, ", ") + "\n"
	}
	return "metrics index: up to date\n"
}

// indexError's fixed sentences, which update apply maps to its reasons.
const (
	indexUpdatingSentence   = "an update is being applied, try again in a minute"
	indexUnreadableSentence = "the metrics index is unreadable and rebuilding it failed today: run backsight stats index rebuild"
)

// indexClassMessage is the sentence of an index error of class.
func indexClassMessage(class string) string { return "the metrics index: " + class }

// indexError is an index error as a command reports it: a sentence or a
// class, never the error's own text, which may hold a path.
func indexError(err error) error {
	var oe *output.Error
	switch {
	case errors.As(err, &oe):
		return err
	case errors.Is(err, index.ErrUpdateInProgress):
		return &output.Error{Code: output.Blocked, Message: indexUpdatingSentence}
	case errors.Is(err, index.ErrInUse):
		return &output.Error{Code: output.Blocked, Message: index.ErrInUse.Error()}
	case errors.Is(err, index.ErrNeedsYes):
		return &output.Error{Code: output.NeedsUser, Message: "rebuilding drops the months listed, whose files are gone: confirm with backsight stats index rebuild --yes",
			Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight stats index rebuild --yes", Why: "rebuild without those months"}}}
	case errors.Is(err, index.ErrUnreadable):
		return &output.Error{Code: output.Blocked, Message: indexUnreadableSentence}
	}
	return &output.Error{Code: output.Blocked, Message: indexClassMessage(index.Class(err))}
}
