package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"slices"
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
	hasValues   = []string{"used", "wrong", "undeclared", "memory"}
	sortValues  = []string{"shown", "used", "rate", "last_shown"}
	stateValues = []string{"hidden", "wrong", "never_used"}
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
func unknownView(word string) error {
	return &output.Error{Code: output.Usage, Message: fmt.Sprintf("unknown view %q: one of %s", word, strings.Join(append(slices.Clone(metrics.Views), "index"), ", "))}
}

func usage(format string, args ...any) error {
	return &output.Error{Code: output.Usage, Message: fmt.Sprintf(format, args...)}
}

// newStatsViewCmd is `stats <view>`: the view as JSON, or its data indented.
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
			if _, err := time.Parse("2006-01-02", id); view == "days" && id != "" && err != nil {
				return usage("a day is written YYYY-MM-DD")
			}
			q, err := viewQuery(*f, id, now())
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
			return e.emit("stats "+view, res, func() string {
				b, _ := json.MarshalIndent(res, "", "  ") // until the text rendering
				return string(b) + "\n"
			})
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

// viewQuery maps the flags and the id of a view to its Query. The window
// is --days (default 7) or --since/--until read in the zone of --tz (this
// machine's by default); a date alone is midnight in that zone.
func viewQuery(f viewFlags, id string, now time.Time) (metrics.Query, error) {
	loc, name, err := viewZone(f.tz)
	if err != nil {
		return metrics.Query{}, err
	}
	var w metrics.Window
	switch {
	case (f.daysSet || f.days != 0) && (f.since != "" || f.until != ""):
		return metrics.Query{}, usage("give either --days or --since/--until")
	case f.until != "" && f.since == "":
		return metrics.Query{}, usage("--until needs --since")
	case f.since != "":
		since, err := parseViewTime(f.since, loc)
		if err != nil {
			return metrics.Query{}, usage("--since: %v", err)
		}
		var until time.Time
		if f.until != "" {
			if until, err = parseViewTime(f.until, loc); err != nil {
				return metrics.Query{}, usage("--until: %v", err)
			}
		}
		if w, err = metrics.NewWindow(now, 0, since, until, loc, name); err != nil {
			return metrics.Query{}, usage("--since must be before --until (or now)")
		}
	default:
		days := 7
		if f.daysSet || f.days != 0 {
			days = f.days
		}
		if days < 1 || days > 365 {
			return metrics.Query{}, usage("--days: a whole number from 1 to 365")
		}
		if w, err = metrics.NewWindow(now, days, time.Time{}, time.Time{}, loc, name); err != nil {
			return metrics.Query{}, usage("%v", err)
		}
	}
	if f.limit < 1 || f.limit > metrics.MaxLimit {
		return metrics.Query{}, usage("--limit: a whole number from 1 to %d", metrics.MaxLimit)
	}
	if f.offset < 0 {
		return metrics.Query{}, usage("--offset: a whole number, 0 or more")
	}
	for _, c := range []struct {
		flag, value string
		allowed     []string
	}{{"has", f.has, hasValues}, {"sort", f.sort, sortValues}, {"state", f.state, stateValues}} {
		if c.value != "" && !slices.Contains(c.allowed, c.value) {
			return metrics.Query{}, usage("--%s: one of %s", c.flag, strings.Join(c.allowed, ", "))
		}
	}
	if f.text && id == "" {
		return metrics.Query{}, usage("--text needs a memory id")
	}
	return metrics.Query{Window: w, Project: f.project, Path: f.path, ID: id, Limit: f.limit, Offset: f.offset,
		Has: f.has, Sort: f.sort, State: f.state, Dev: f.dev, Text: f.text}, nil
}

// viewZone is the zone of --tz, this machine's when empty, and its name.
func viewZone(tz string) (*time.Location, string, error) {
	loc := time.Local
	if tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			return nil, "", usage("--tz: unknown zone %q (a name such as Europe/Paris or UTC)", tz)
		}
	}
	return loc, metrics.ZoneName(loc, os.Getenv, os.Readlink), nil
}

// parseViewTime reads a window bound: RFC 3339 or YYYY-MM-DDTHH:MM with a
// zone, or a date alone, which is midnight in loc.
func parseViewTime(s string, loc *time.Location) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a time (RFC 3339, YYYY-MM-DDTHH:MMZ or YYYY-MM-DD)", s)
}

// viewError maps a metrics error to the command's: an id that names
// nothing is a usage error, anything else an index error.
func viewError(err error) error {
	var nf metrics.NotFoundError
	if !errors.As(err, &nf) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &output.Error{Code: output.Blocked, Message: "the view was interrupted or ran out of time"}
		}
		return indexError(err)
	}
	switch {
	case nf.Kind == "metric":
		return usage("no metric %q: backsight stats defs lists them", nf.ID)
	case nf.Kind == "project":
		return usage("no project %q in the window", nf.ID)
	case nf.TooShort:
		return usage("%s id: give at least 8 characters", nf.Kind)
	case len(nf.Matches) > 1:
		return usage("ambiguous %s id %q: %s", nf.Kind, nf.ID, strings.Join(nf.Matches[:min(5, len(nf.Matches))], ", "))
	}
	return usage("no %s %q in the metrics index", nf.Kind, nf.ID)
}

// viewSource builds what a view reads: the metrics index caught up (the
// first build within firstBuildBudget with progress on stderr, else within
// the view budget), the configuration, the server through a GET-only client
// and the doctor. The returned func closes the index.
func viewSource(ctx context.Context, d paths.Dirs, home string, stderr io.Writer) (metrics.Source, func(), error) {
	src := metrics.Source{Now: now}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	switch {
	case err == nil:
		src.Configured = true
	case errors.Is(err, fs.ErrNotExist):
		// no config: defaults, no server
	default:
		return src, nil, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	src.Config = cfg
	if src.Configured && cfg.Server.URL != "" {
		sec, err := config.LoadSecrets(d.SecretsFile())
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return src, nil, &output.Error{Code: output.Blocked, Message: err.Error()}
		}
		if ro, err := hindsight.NewReadOnly(cfg.Server.URL, sec.APIKey, cfg.Server.Bank, metrics.ServerTimeout); err == nil {
			src.Server = ro
			if u, err := url.Parse(cfg.Server.URL); err == nil {
				src.ServerHost = u.Host
			}
		}
	}
	src.Doctor = func(ctx context.Context) ([]metrics.DoctorCheck, error) { return runDoctorChecks(ctx, d, home) }

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
