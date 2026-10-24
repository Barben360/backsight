package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
)

// clientEnv is what the client commands share: backsight's dirs, its
// config and Claude Code's files.
type clientEnv struct {
	dirs    paths.Dirs
	cfg     config.Config
	loc     claudecode.Location
	managed string // managed-settings directory
}

func loadClientEnv(configDir string) (clientEnv, error) {
	d, home, err := machine()
	if err != nil {
		return clientEnv{}, err
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if errors.Is(err, fs.ErrNotExist) {
		cfg, err = config.Default(d, home), nil
	}
	if err != nil {
		return clientEnv{}, &output.Error{Code: output.Usage, Message: err.Error()}
	}
	getenv := os.Getenv
	if configDir != "" {
		getenv = func(k string) string {
			if k == "CLAUDE_CONFIG_DIR" {
				return configDir
			}
			return os.Getenv(k)
		}
	}
	var de doctor.Env
	probes(&de)
	return clientEnv{dirs: d, cfg: cfg, loc: claudecode.Locate(home, getenv), managed: de.ManagedDir}, nil
}

func (c clientEnv) runner(command string) claudecfg.Runner {
	return claudecfg.Runner{PreImages: c.dirs.PreImagesDir(), Journal: c.dirs.JournalFile(), Command: command, Now: now}
}

// ownedEdits are the edits backsight keeps in Claude Code's files: in M1
// only the transcript retention floor (decisions 0041, 0052). notes say
// what was left alone and why.
func (c clientEnv) ownedEdits() (edits []claudecfg.Edit, notes []string, err error) {
	// The permission rule for the memory tools, once client install wired
	// this profile (SPEC §5): client config apply repairs it.
	if st, err := state.Load(c.dirs.StateFile()); err == nil && st.PluginVersion != "" {
		edits = append(edits, claudecfg.Edit{Target: c.loc.Settings, Apply: permissionRule})
	}
	if days := c.cfg.Client.RetentionDays; days > 0 {
		ret, err := claudecode.ReadRetention(c.loc, c.managed)
		if err != nil {
			return nil, nil, &output.Error{Code: output.Blocked, Message: err.Error() + ": fix the file by hand first, backsight never overwrites it"}
		}
		if ret.ManagedDays != nil {
			notes = append(notes, fmt.Sprintf("a managed setting forces cleanupPeriodDays = %d: backsight leaves it to the administrator", *ret.ManagedDays))
		} else {
			edits = append(edits, claudecfg.Edit{Target: c.loc.Settings, Apply: retentionFloor(days)})
		}
	}
	return edits, notes, nil
}

// permissionRule allows the three memory tools (SPEC §5).
func permissionRule(doc []byte) ([]byte, []claudecfg.Change, error) {
	var changes []claudecfg.Change
	for _, tool := range plugin.ToolNames {
		out, ch, err := claudecfg.EnsureInArray(doc, []string{"permissions", "allow"}, tool)
		if err != nil {
			return nil, nil, err
		}
		doc, changes = out, append(changes, ch)
	}
	return doc, changes, nil
}

// retentionFloor raises cleanupPeriodDays to days; a higher value stays.
func retentionFloor(days int) func([]byte) ([]byte, []claudecfg.Change, error) {
	return func(doc []byte) ([]byte, []claudecfg.Change, error) {
		v, ok, err := claudecfg.Get(doc, []string{"cleanupPeriodDays"})
		if err != nil {
			return nil, nil, err
		}
		if n, isNum := v.(json.Number); ok && isNum {
			if cur, err := n.Int64(); err == nil && cur >= int64(days) {
				return doc, nil, nil
			}
		}
		return setRetention(days)(doc)
	}
}

// setRetention sets cleanupPeriodDays to exactly days.
func setRetention(days int) func([]byte) ([]byte, []claudecfg.Change, error) {
	return func(doc []byte) ([]byte, []claudecfg.Change, error) {
		out, ch, err := claudecfg.Set(doc, []string{"cleanupPeriodDays"}, days)
		return out, []claudecfg.Change{ch}, err
	}
}

// recordBefore keeps, once, the user's retention before backsight's first
// change to it (decision 0052). It runs on the plan, before any write, so a
// state that cannot be updated stops the command with nothing written.
func recordBefore(d paths.Dirs, command string, planned []claudecfg.FileResult, settings string) error {
	for _, fr := range planned {
		if fr.Target != settings {
			continue
		}
		for _, ch := range fr.Changes {
			if ch.Path != "cleanupPeriodDays" {
				continue
			}
			before := json.RawMessage("null")
			if ch.Old != nil {
				before = json.RawMessage(*ch.Old)
			}
			return state.Update(d, command, func(s *state.State) error {
				if s.Client == nil {
					s.Client = &state.Client{}
				}
				if s.Client.RetentionBefore == nil {
					s.Client.RetentionBefore = before
				}
				return nil
			})
		}
	}
	return nil
}

// editError maps engine errors to exit codes: a file that is not valid
// JSON, keeps changing or changed since a run needs the user (20).
func editError(err error) error {
	if err == nil {
		return nil
	}
	var oe *output.Error
	if errors.As(err, &oe) {
		return err
	}
	return &output.Error{Code: output.Blocked, Message: err.Error()}
}

// partialError: some writes happened before err; rerunning resumes (40).
func partialError(err error) error {
	return &output.Error{Code: output.Partial, Message: err.Error() + " (some changes were made: rerun to finish, or restore)"}
}

type clientConfigResult struct {
	Run   string                 `json:"run,omitempty"`
	Files []claudecfg.FileResult `json:"files"`
	Notes []string               `json:"notes,omitempty"`
}

func newClientCmd(e *env) *cobra.Command {
	var configDir string
	cmd := &cobra.Command{Use: "client", Short: "Wire backsight into Claude Code"}
	cmd.PersistentFlags().StringVar(&configDir, "config-dir", "", "Claude Code's directory (default: $CLAUDE_CONFIG_DIR, else ~/.claude)")
	cfgCmd := &cobra.Command{Use: "config", Short: "Plan, apply or undo backsight's edits to Claude Code's files"}
	cfgCmd.AddCommand(newClientConfigApplyCmd(e, &configDir, true), newClientConfigApplyCmd(e, &configDir, false), newClientConfigRestoreCmd(e, &configDir))
	cmd.AddCommand(cfgCmd, newClientRetentionCmd(e, &configDir), newClientInstallCmd(e, &configDir))
	return cmd
}

func newClientConfigApplyCmd(e *env, configDir *string, plan bool) *cobra.Command {
	name, short := "apply", "Apply backsight's edits to Claude Code's files (backed up first)"
	if plan {
		name, short = "plan", "Show backsight's pending edits to Claude Code's files"
	}
	command := "client config " + name
	return &cobra.Command{
		Use:   name,
		Short: short,
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			c, err := loadClientEnv(*configDir)
			if err != nil {
				return err
			}
			edits, notes, err := c.ownedEdits()
			if err != nil {
				return err
			}
			res := clientConfigResult{Notes: notes}
			res.Files, err = claudecfg.Plan(edits)
			if err == nil && !plan {
				if err = recordBefore(c.dirs, command, res.Files, c.loc.Settings); err == nil {
					res.Run, res.Files, err = c.runner(command).Apply(edits)
				}
			}
			if res.Files == nil {
				res.Files = []claudecfg.FileResult{}
			}
			return e.report(command, res, func() string { return clientConfigText(res, plan) }, editError(err))
		},
	}
}

func clientConfigText(r clientConfigResult, plan bool) string {
	var b strings.Builder
	n := 0
	for _, f := range r.Files {
		for _, ch := range f.Changes {
			n++
			fmt.Fprintf(&b, "%s: %s: %s → %s\n", f.Target, ch.Path, showValue(ch.Old), showValue(ch.New))
		}
	}
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	switch {
	case n == 0:
		b.WriteString("nothing to change\n")
	case plan:
		b.WriteString("run backsight client config apply to make these changes (each file is backed up first)\n")
	default:
		fmt.Fprintf(&b, "done; undo with backsight client config restore %s\n", r.Run)
	}
	return b.String()
}

func showValue(v *string) string {
	if v == nil {
		return "(absent)"
	}
	return *v
}

type clientRestoreResult struct {
	Restored string                    `json:"restored"`
	Run      string                    `json:"run,omitempty"` // the restore's own run, to undo it
	Files    []claudecfg.RestoreResult `json:"files"`
	Plan     bool                      `json:"plan"`
}

func newClientConfigRestoreCmd(e *env, configDir *string) *cobra.Command {
	var force, plan bool
	cmd := &cobra.Command{
		Use:   "restore [<run>]",
		Short: "Put Claude Code's files back as they were before a run (default: the last one)",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := loadClientEnv(*configDir)
			if err != nil {
				return err
			}
			ts := ""
			if len(args) == 1 {
				ts = args[0]
			}
			restored := ts
			if restored == "" {
				runs, _ := claudecfg.Runs(c.dirs.PreImagesDir())
				if len(runs) > 0 {
					restored = runs[len(runs)-1]
				}
			}
			var run string
			var files []claudecfg.RestoreResult
			if plan {
				files, err = c.runner("client config restore").RestorePlan(ts, force)
			} else {
				run, files, err = c.runner("client config restore").Restore(ts, force)
			}
			res := clientRestoreResult{Restored: restored, Files: files, Plan: plan}
			if run != restored {
				res.Run = run
			}
			if err != nil && res.Run != "" {
				err = partialError(err)
			}
			if res.Files == nil {
				res.Files = []claudecfg.RestoreResult{}
			}
			return e.report("client config restore", res, func() string {
				var b strings.Builder
				for _, f := range res.Files {
					status := f.Status
					if plan && (status == "restore" || status == "delete") {
						status = "would " + status
					}
					fmt.Fprintf(&b, "%s: %s", f.Target, status)
					if f.Reason != "" {
						fmt.Fprintf(&b, " (%s)", f.Reason)
					}
					b.WriteString("\n")
				}
				if err == nil && res.Run != "" {
					fmt.Fprintf(&b, "undo with backsight client config restore %s\n", res.Run)
				}
				return b.String()
			}, editError(err))
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restore files changed since that run too")
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would be restored without writing")
	return cmd
}
