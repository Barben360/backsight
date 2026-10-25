package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/state"
)

// sysCLI runs the OS scheduler's commands; tests replace it.
type sysCLI interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execSys struct{}

func (execSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

var sysRunner sysCLI = execSys{}

// schedulerGOOS is the system the entries are built for; tests replace it.
var schedulerGOOS = runtime.GOOS

type jobsInstallResult struct {
	Kind     string                 `json:"kind"`
	Files    []claudecfg.FileResult `json:"files"`
	Commands []string               `json:"commands"`
	Planned  bool                   `json:"planned,omitempty"`
	Run      string                 `json:"run,omitempty"` // config editor run (undo)
}

func schedulerPlan() (scheduler.Plan, error) {
	d, home, err := machine()
	if err != nil {
		return scheduler.Plan{}, err
	}
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(home, ".config")
	}
	shim := filepath.Join(d.Data, "bin", plugin.ShimName(schedulerGOOS))
	p, err := scheduler.For(schedulerGOOS, shim, home, configDir, os.Getuid())
	if err != nil {
		return p, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return p, nil
}

func cmdLine(c []string) string { return strings.Join(c, " ") }

// newJobsInstallCmd writes the two scheduler entries (decision 0025)
// through the config editor and activates them. setup (M4) calls it.
func newJobsInstallCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:    "install",
		Short:  "Install the OS scheduler entries for jobs tick (15 min) and jobs maintain (daily)",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, _, err := machine()
			if err != nil {
				return err
			}
			p, err := schedulerPlan()
			if err != nil {
				return err
			}
			edits := make([]claudecfg.Edit, 0, len(p.Files))
			for _, f := range p.Files {
				content := f.Content
				edits = append(edits, claudecfg.Edit{Target: f.Path, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
					old := string(doc)
					var before *string
					if doc != nil {
						before = &old
					}
					return []byte(content), []claudecfg.Change{{Path: "(whole file)", Old: before, New: &content}}, nil
				}})
			}
			res := jobsInstallResult{Kind: p.Kind}
			for _, c := range p.Activate {
				res.Commands = append(res.Commands, cmdLine(c))
			}
			if plan {
				res.Planned = true
				if res.Files, err = claudecfg.Plan(edits); err != nil {
					return blocked(err)
				}
				return e.emit("jobs install", res, func() string { return jobsInstallText(res) })
			}
			runner := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "jobs install", Now: now}
			if res.Run, res.Files, err = runner.Apply(edits); err != nil {
				return blocked(err)
			}
			for _, c := range p.Activate {
				out, err := sysRunner.Run(cmd.Context(), c[0], c[1:]...)
				// launchd's bootout of an agent not loaded yet fails: expected
				if expected := p.Kind == "launchd" && c[1] == "bootout"; err != nil && !expected {
					return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("%s: %v %s", cmdLine(c), err, strings.TrimSpace(string(out)))}
				}
			}
			if err := state.Update(d, "jobs install", func(s *state.State) error {
				s.Scheduler = &state.Scheduler{Kind: p.Kind, Files: p.Paths(), InstalledAt: now().UTC()}
				return nil
			}); err != nil {
				return blocked(err)
			}
			return e.emit("jobs install", res, func() string { return jobsInstallText(res) })
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show the files and commands without writing or running them")
	return cmd
}

func jobsInstallText(r jobsInstallResult) string {
	var b strings.Builder
	verb := map[bool]string{true: "would write", false: "wrote"}[r.Planned]
	for _, f := range r.Files {
		state := "unchanged"
		if len(f.Changes) > 0 && f.Changes[0].Changed() {
			state = verb
		}
		fmt.Fprintf(&b, "%-12s %s\n", state, f.Target)
	}
	run := map[bool]string{true: "would run", false: "ran"}[r.Planned]
	for _, c := range r.Commands {
		fmt.Fprintf(&b, "%-12s %s\n", run, c)
	}
	return b.String()
}

// newJobsUninstallCmd deactivates the entries and removes their files.
func newJobsUninstallCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:    "uninstall",
		Short:  "Remove the OS scheduler entries",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, _, err := machine()
			if err != nil {
				return err
			}
			p, err := schedulerPlan()
			if err != nil {
				return err
			}
			res := jobsInstallResult{Kind: p.Kind, Planned: plan}
			for _, c := range p.Remove {
				res.Commands = append(res.Commands, cmdLine(c))
			}
			if !plan {
				for _, c := range p.Remove {
					_, _ = sysRunner.Run(cmd.Context(), c[0], c[1:]...) // an entry already gone is fine
				}
				for _, f := range p.Paths() {
					if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return blocked(err)
					}
				}
				if p.Kind == "systemd" {
					_, _ = sysRunner.Run(cmd.Context(), "systemctl", "--user", "daemon-reload")
				}
				if err := state.Update(d, "jobs uninstall", func(s *state.State) error { s.Scheduler = nil; return nil }); err != nil {
					return blocked(err)
				}
			}
			for _, f := range p.Paths() {
				res.Files = append(res.Files, claudecfg.FileResult{Target: f, Written: !plan})
			}
			return e.emit("jobs uninstall", res, func() string {
				var b strings.Builder
				verb := map[bool]string{true: "would remove", false: "removed"}[plan]
				for _, f := range res.Files {
					fmt.Fprintf(&b, "%-12s %s\n", verb, f.Target)
				}
				for _, c := range res.Commands {
					fmt.Fprintf(&b, "%-12s %s\n", map[bool]string{true: "would run", false: "ran"}[plan], c)
				}
				return b.String()
			})
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would be removed")
	return cmd
}
