package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/views"
)

// firstBuildBudget bounds the catch-up of a view that finds the index never
// ingested: the first build may read months of files, with progress on
// stderr (a var for the tests).
var firstBuildBudget = 5 * time.Minute

// viewFlags are the persistent flags of `stats` and the flags of the views.
type viewFlags struct {
	days             int
	daysSet          bool // --days was given (0 is then refused)
	since, until, tz string
	project, path    string
	dev              bool
	limit, offset    int
	has, sort, state string
	text             bool
}

// Allowed values of the view flags.
var (
	hasValues   = views.HasValues()
	sortValues  = views.SortValues()
	stateValues = views.StateValues()
)

// viewFlagNames are the flags that only a view takes, which the legacy
// report and `stats index` refuse.
var viewFlagNames = []string{"tz", "project", "path", "dev", "limit", "offset"}

// addViewFlags declares the persistent flags of `stats`.
func addViewFlags(cmd *cobra.Command, f *viewFlags) {
	p := cmd.PersistentFlags()
	p.IntVar(&f.days, "days", 0, "window of the last N days, 1 to 365 (default 7)")
	p.StringVar(&f.since, "since", "", "start of the window (RFC 3339, YYYY-MM-DDTHH:MMZ or YYYY-MM-DD) instead of --days; a date alone is midnight UTC for the report, midnight in --tz for a view")
	p.StringVar(&f.until, "until", "", "end of the window (default now), with --since")
	p.StringVar(&f.tz, "tz", "", "views: the zone that days and dates are read in (a name such as Europe/Paris or UTC; default this machine's)")
	p.StringVar(&f.project, "project", "", "views: only this project's sessions")
	p.StringVar(&f.path, "path", "", "views: only this recall path")
	p.BoolVar(&f.dev, "dev", false, "views: add the developer metrics")
	p.IntVar(&f.limit, "limit", metrics.DefaultLimit, "views: rows per page, 1 to 500")
	p.IntVar(&f.offset, "offset", 0, "views: rows to skip")
}

// refuseViewFlags is the usage error for a view-only flag given to the
// legacy report or to `stats index`.
func refuseViewFlags(cmd *cobra.Command, names ...string) error {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return &output.Error{Code: output.Usage, Message: fmt.Sprintf("--%s applies to a view, e.g. backsight stats overview --%s …", n, n)}
		}
	}
	return nil
}

// unknownView is the usage error for a word that is neither a number nor
// a view.
func unknownView(word string) error { return views.UnknownView(word, true) }

func usage(format string, args ...any) error { return views.Usage(format, args...) }

// newStatsViewCmd is `stats <view>`: the view as JSON, or as plain text.
func newStatsViewCmd(e *env, view string, f *viewFlags) *cobra.Command {
	use, short, args := view, "", cobra.PositionalArgs(cobra.NoArgs)
	switch view {
	case "overview":
		short = "Show the overview: the verdict, the six tiles, what needs attention and the latest sessions"
	case "quality":
		short = "Show how well memory worked: found, used, wrong and the review queue"
	case "cost":
		short = "Show what memory costs: context sent, search, extraction tokens and money"
	case "bank":
		short = "Show the memory bank: size, growth, stale and retired memories"
	case "pipeline":
		short = "Show the write pipeline: notes sent, queue, failures and delays"
	case "system":
		short = "Show the system: doctor checks, server reach and scheduled jobs"
	case "releases":
		short = "Compare the binary versions that ran in the window"
	case "projects":
		use, short, args = "projects [<name>]", "List the projects of the window, or show one", cobra.MaximumNArgs(1)
	case "sessions":
		use, short, args = "sessions [<id>]", "List the sessions of the window, or show one (an id prefix of 8 characters is enough)", cobra.MaximumNArgs(1)
	case "memories":
		use, short, args = "memories [<id>]", "List the memories shown in the window, or show one (an id prefix of 8 characters is enough)", cobra.MaximumNArgs(1)
	case "days":
		use, short, args = "days [<YYYY-MM-DD>]", "List the days of the window, or show one", cobra.MaximumNArgs(1)
	case "defs":
		use, short, args = "defs [<id>]", "List the metrics with their definitions, or show one", cobra.MaximumNArgs(1)
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  usageArgs(args),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			f.daysSet = cmd.Flags().Changed("days")
			q, err := views.Query(view, f.params(), id, now())
			if err != nil {
				return err
			}
			d, home, err := machine()
			if err != nil {
				return err
			}
			src, closeSrc, err := viewSource(cmd.Context(), d, home, e.stderr)
			if err != nil {
				return err
			}
			defer closeSrc()
			res, err := metrics.Run(cmd.Context(), view, src, q)
			if err != nil {
				return viewError(err)
			}
			return e.emit("stats "+view, res, func() string { return viewText(res, f.dev) })
		},
	}
	switch view {
	case "sessions":
		cmd.Flags().StringVar(&f.has, "has", "", "only sessions that have this: "+strings.Join(hasValues, ", "))
	case "memories":
		cmd.Flags().StringVar(&f.sort, "sort", "", "order: "+strings.Join(sortValues, ", ")+" (default used)")
		cmd.Flags().StringVar(&f.state, "state", "", "only memories in this state: "+strings.Join(stateValues, ", "))
		cmd.Flags().BoolVar(&f.text, "text", false, "with an id: add the memory's text, read from the server")
	}
	return cmd
}

// params are the flags as the views' parameters.
func (f viewFlags) params() views.Params {
	return views.Params{Days: f.days, DaysSet: f.daysSet, Since: f.since, Until: f.until, TZ: f.tz,
		Project: f.project, Path: f.path, Dev: f.dev, Limit: f.limit, Offset: f.offset,
		Has: f.has, Sort: f.sort, State: f.state, Text: f.text}
}

// viewQuery maps the flags and the id of a view to its Query.
func viewQuery(f viewFlags, id string, now time.Time) (metrics.Query, error) {
	return views.Query("", f.params(), id, now)
}

// viewError maps a metrics error to the command's.
func viewError(err error) error { return views.Error(err) }

// viewServer is what a view reads besides the index: the configuration, the
// server through a GET-only client and the doctor.
func viewServer(d paths.Dirs, home string) (metrics.Source, error) {
	src := metrics.Source{Now: now}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	switch {
	case err == nil:
		src.Configured = true
	case errors.Is(err, fs.ErrNotExist):
		// no config: defaults, no server
	default:
		return src, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	src.Config = cfg
	if src.Configured && cfg.Server.URL != "" {
		sec, err := config.LoadSecrets(d.SecretsFile())
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return src, &output.Error{Code: output.Blocked, Message: err.Error()}
		}
		if ro, err := hindsight.NewReadOnly(cfg.Server.URL, sec.APIKey, cfg.Server.Bank, metrics.ServerTimeout); err == nil {
			src.Server = ro
			if u, err := url.Parse(cfg.Server.URL); err == nil {
				src.ServerHost = u.Host
			}
		}
	}
	src.Doctor = func(ctx context.Context) ([]metrics.DoctorCheck, error) { return runDoctorChecks(ctx, d, home) }
	return src, nil
}

// viewSource builds what a view reads: viewServer's source and the metrics
// index caught up (the first build within firstBuildBudget with progress on
// stderr, else within the view budget). The returned func closes the index.
func viewSource(ctx context.Context, d paths.Dirs, home string, stderr io.Writer) (metrics.Source, func(), error) {
	src, err := viewServer(d, home)
	if err != nil {
		return src, nil, err
	}
	st, err := index.Inspect(ctx, indexOptionsAt(d, home, nil))
	if err != nil {
		return src, nil, indexError(err)
	}
	budget, progress := index.ViewBudget, io.Writer(nil)
	if !st.Exists || st.IngestedAt.IsZero() {
		budget, progress = firstBuildBudget, stderr
	}
	ix, err := index.Open(ctx, indexOptionsAt(d, home, progress))
	if err != nil {
		return src, nil, indexError(err)
	}
	fr, err := ix.CatchUp(ctx, budget)
	if err != nil {
		_ = ix.Close()
		return src, nil, indexError(err)
	}
	src.Index, src.Fresh = ix, fr
	return src, func() { _ = ix.Close() }, nil
}

// indexOptionsAt is indexOptions with the cli's clock.
func indexOptionsAt(d paths.Dirs, home string, progress io.Writer) index.Options {
	o := indexOptions(d, home, progress)
	o.Now = now
	return o
}

// doctorRun is doctor.Run; tests replace it.
var doctorRun = doctor.Run

// runDoctorChecks runs the doctor and keeps each check's id, status and fix
// (never its summary or detail, which may quote paths or errors).
func runDoctorChecks(ctx context.Context, d paths.Dirs, home string) ([]metrics.DoctorCheck, error) {
	r := doctorRun(ctx, doctorEnv(d, home))
	out := make([]metrics.DoctorCheck, 0, len(r.Checks))
	for _, c := range r.Checks {
		out = append(out, metrics.DoctorCheck{ID: c.ID, Status: string(c.Status), Fix: c.Fix})
	}
	return out, nil
}

// legacyDays is the report's window length from [days] and --days: the
// argument and the flag name the same thing, once.
func legacyDays(arg string, hasArg bool, f viewFlags, flagSet bool) (int, error) {
	switch {
	case hasArg && flagSet:
		return 0, usage("give either [days] or --days")
	case hasArg:
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 || n > 365 {
			return 0, usage("days: a whole number from 1 to 365")
		}
		return n, nil
	case flagSet:
		if f.days < 1 || f.days > 365 {
			return 0, usage("days: a whole number from 1 to 365")
		}
		return f.days, nil
	}
	return 7, nil
}
