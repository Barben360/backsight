package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/state"
)

// A refresh (issue 222) brings installed scheduler entries to this
// binary's templates: the new binary's client install runs it during an
// update apply, and doctor compares the same way. It keeps the
// environment the entries were installed with (jobs install takes the
// installing shell's), rewrites only the files that differ, through the
// config editor, and never stops a running job: systemd re-reads units on
// daemon-reload and a running service keeps its process; an entry whose
// reload would stop it (launchd's bootout, a replaced task) is left as it
// is while its job runs.

type jobsRefreshResult struct {
	Kind     string                 `json:"kind,omitempty"`
	Status   string                 `json:"status"` // none | unchanged | planned | done | deferred
	Files    []claudecfg.FileResult `json:"files"`
	Commands []string               `json:"commands"`
	Deferred []string               `json:"deferred,omitempty"` // jobs left as they are: they were running
	Planned  bool                   `json:"planned,omitempty"`
	Run      string                 `json:"run,omitempty"` // config editor run (undo)
}

// schedulerDiff compares the installed entries with this binary's.
type schedulerDiff struct {
	installed bool
	plan      scheduler.Plan
	stale     []scheduler.Entry
	differs   map[string]bool // files whose content is not the rendered one
	digest    string          // tasks: the digest to record
	elsewhere string          // why the entries cannot be refreshed in place
}

// tasksDigest is what jobs install records for tasks (no files).
func tasksDigest(p scheduler.Plan) string {
	if p.Kind == "schtasks" {
		return p.Digest()
	}
	return ""
}

func diffScheduler(ctx context.Context, d paths.Dirs) (schedulerDiff, error) {
	var df schedulerDiff
	st, err := state.Load(d.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return df, blocked(err)
	}
	if st.Scheduler == nil {
		return df, nil
	}
	df.installed = true
	p, err := schedulerPlan(ctx)
	if err != nil {
		return df, err
	}
	rec := st.Scheduler
	df.plan = p
	if p.Kind != rec.Kind {
		df.elsewhere = "the entries were installed for " + rec.Kind + ", this binary builds " + p.Kind + " entries"
		return df, nil
	}
	if a, b := slices.Sorted(slices.Values(rec.Files)), slices.Sorted(slices.Values(p.Paths())); !slices.Equal(a, b) {
		df.elsewhere = "the entries are " + strings.Join(a, ", ") + "; this install's would be " + strings.Join(b, ", ")
		return df, nil
	}
	// The environment they were installed with, from their own files.
	for _, f := range p.Files {
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			continue
		}
		if env, ok := scheduler.InstalledEnv(f.Path, string(raw)); ok {
			if p, err = schedulerPlan(ctx, func(o *scheduler.Options) { o.Env = env }); err != nil {
				return df, err
			}
			break
		}
	}
	df.plan, df.differs = p, map[string]bool{}
	for _, f := range p.Files {
		if raw, err := os.ReadFile(f.Path); err != nil || string(raw) != f.Content {
			df.differs[f.Path] = true
		}
	}
	df.digest = tasksDigest(p)
	tasksStale := df.digest != "" && rec.Digest != df.digest
	for _, e := range p.Entries {
		if slices.ContainsFunc(e.Files, func(f string) bool { return df.differs[f] }) || (len(e.Files) == 0 && tasksStale) {
			df.stale = append(df.stale, e)
		}
	}
	return df, nil
}

// refreshScheduler rewrites and reloads the stale entries (plan: says
// which, changes nothing).
func refreshScheduler(ctx context.Context, plan bool) (jobsRefreshResult, error) {
	res := jobsRefreshResult{Status: "none", Planned: plan, Files: []claudecfg.FileResult{}, Commands: []string{}}
	d, _, err := machine()
	if err != nil {
		return res, err
	}
	df, err := diffScheduler(ctx, d)
	if err != nil || !df.installed {
		return res, err
	}
	res.Kind = df.plan.Kind
	if df.elsewhere != "" {
		return res, &output.Error{Code: output.Blocked, Message: df.elsewhere + ": run backsight jobs uninstall, then backsight jobs install"}
	}
	res.Status = "unchanged"
	if len(df.stale) == 0 {
		return res, nil
	}
	var edits []claudecfg.Edit
	var reload [][]string
	for _, e := range df.stale {
		if e.Running != nil {
			if out, err := sysRunner.Run(ctx, e.Running[0], e.Running[1:]...); err == nil && strings.Contains(string(out), e.RunningMark) {
				res.Deferred = append(res.Deferred, e.Job)
				continue
			}
		}
		for _, f := range df.plan.Files {
			if df.differs[f.Path] && slices.Contains(e.Files, f.Path) {
				edits = append(edits, wholeFile(f))
			}
		}
		reload = append(reload, e.Reload...)
	}
	if len(reload) == 0 && len(edits) == 0 {
		res.Status = "deferred"
		return res, nil
	}
	reload = append(reload, df.plan.Reload...)
	for _, c := range reload {
		res.Commands = append(res.Commands, cmdLine(c))
	}
	if plan {
		res.Status = "planned"
		if res.Files, err = claudecfg.Plan(edits); err != nil {
			return res, blocked(err)
		}
		return res, nil
	}
	if len(edits) > 0 {
		runner := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "jobs refresh", Now: now}
		if res.Run, res.Files, err = runner.Apply(edits); err != nil {
			return res, blocked(err)
		}
	}
	if df.digest != "" && len(res.Deferred) == 0 {
		if err := state.Update(d, "jobs refresh", func(s *state.State) error {
			if s.Scheduler != nil {
				s.Scheduler.Digest = df.digest
			}
			return nil
		}); err != nil {
			return res, blocked(err)
		}
	}
	for _, c := range reload {
		out, err := sysRunner.Run(ctx, c[0], c[1:]...)
		// launchd's bootout of an agent not loaded fails: expected
		if expected := df.plan.Kind == "launchd" && c[1] == "bootout"; err != nil && !expected {
			return res, &output.Error{Code: output.Blocked, Message: fmt.Sprintf("%s: %v %s (the files are rewritten: backsight jobs refresh retries the reload)",
				cmdLine(c), err, strings.TrimSpace(string(out)))}
		}
	}
	res.Status = "done"
	return res, nil
}

// wholeFile is an edit that replaces a generated file.
func wholeFile(f scheduler.File) claudecfg.Edit {
	content := f.Content
	return claudecfg.Edit{Target: f.Path, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
		old := string(doc)
		var before *string
		if doc != nil {
			before = &old
		}
		return []byte(content), []claudecfg.Change{{Path: "(whole file)", Old: before, New: &content}}, nil
	}}
}

func newJobsRefreshCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:    "refresh",
		Short:  "Rewrite the installed scheduler entries whose template changed, keeping their environment, without stopping a running job",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := refreshScheduler(cmd.Context(), plan)
			if err != nil {
				return err
			}
			return e.emit("jobs refresh", res, func() string { return jobsRefreshText(res) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show the files and commands without writing or running them")
	return cmd
}

func jobsRefreshText(r jobsRefreshResult) string {
	var b strings.Builder
	switch r.Status {
	case "none":
		return "no scheduler entries installed (backsight jobs install)\n"
	case "unchanged":
		return r.Kind + " entries up to date\n"
	}
	b.WriteString(jobsInstallText(jobsInstallResult{Files: r.Files, Commands: r.Commands, Planned: r.Planned}))
	for _, j := range r.Deferred {
		fmt.Fprintf(&b, "%-12s %s: its job is running and reloading it would stop it; backsight jobs refresh once it ended\n", "left", j)
	}
	return b.String()
}

// refreshDetail is one line for a step (client install, update apply).
func refreshDetail(r jobsRefreshResult) string {
	var parts []string
	for _, f := range r.Files {
		parts = append(parts, f.Target)
	}
	s := r.Kind + " entries"
	if len(parts) > 0 {
		s += map[bool]string{true: " to rewrite: ", false: " rewritten: "}[r.Planned] + strings.Join(parts, ", ")
	}
	if len(r.Deferred) > 0 {
		s += "; left while running: " + strings.Join(r.Deferred, ", ") + " (backsight jobs refresh once it ended)"
	}
	return s
}

// schedulerEntries is doctor's comparison: the jobs a refresh would
// rewrite, whether they run now or not.
func schedulerEntries(ctx context.Context, d paths.Dirs) (doctor.SchedulerEntries, error) {
	df, err := diffScheduler(ctx, d)
	if err != nil || !df.installed {
		return doctor.SchedulerEntries{}, err
	}
	s := doctor.SchedulerEntries{Installed: true, Kind: df.plan.Kind, Elsewhere: df.elsewhere}
	for _, e := range df.stale {
		s.Stale = append(s.Stale, e.Job)
	}
	return s, nil
}
