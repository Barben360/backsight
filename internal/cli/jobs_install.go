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
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// sysCLI runs the OS scheduler's commands; tests replace it.
type sysCLI interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execSys runs the system's service tools; ownGroup puts each in its own
// process group (the native runtime's rollback: a Ctrl-C at the terminal
// must not kill its systemctl).
type execSys struct{ ownGroup bool }

// InOwnGroup is s with its children in their own process group.
func (s execSys) InOwnGroup() server.SysRunner { s.ownGroup = true; return s }

func (s execSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if s.ownGroup {
		server.OwnGroup(cmd)
	}
	return cmd.CombinedOutput()
}

var sysRunner sysCLI = execSys{}

// schedulerGOOS is the system the entries are built for; tests replace it.
var schedulerGOOS = runtime.GOOS

// jobsUserSID is the user's SID, the jobs' tasks' principal (Windows);
// tests replace it.
var jobsUserSID = setup.UserSID

type jobsInstallResult struct {
	Kind     string                 `json:"kind"`
	Files    []claudecfg.FileResult `json:"files"`
	Commands []string               `json:"commands"`
	Planned  bool                   `json:"planned,omitempty"`
	Run      string                 `json:"run,omitempty"` // config editor run (undo)
}

// schedulerPlan builds the entries for this install. The job runs with
// none of the installing shell's environment: the directories the install
// resolved and PATH go into the entries. tweak changes the options last
// (a refresh keeps the environment the entries were installed with).
func schedulerPlan(ctx context.Context, tweak ...func(*scheduler.Options)) (scheduler.Plan, error) {
	d, home, err := machine()
	if err != nil {
		return scheduler.Plan{}, err
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return scheduler.Plan{}, &output.Error{Code: output.Usage, Message: err.Error()}
	}
	o := scheduler.Options{Shim: filepath.Join(d.Data, "bin", plugin.ShimName(schedulerGOOS)), Home: home, UID: os.Getuid(),
		LogDir: filepath.Join(d.State, "logs"), Env: scheduler.TaskEnv(schedulerGOOS, directoryEnv(), os.Getenv), Name: cfg.Server.Project}
	if v := os.Getenv("PATH"); v != "" {
		o.Env["PATH"] = v
	}
	if schedulerGOOS == "linux" {
		o.UnitDir = userUnitDir(ctx, home)
	}
	if schedulerGOOS == "windows" {
		o.State = d.State
		if o.UserSID, err = jobsUserSID(); err != nil {
			return scheduler.Plan{}, &output.Error{Code: output.Blocked, Message: "the jobs' tasks need your user's SID: " + err.Error()}
		}
	}
	for _, f := range tweak {
		f(&o)
	}
	p, err := scheduler.For(schedulerGOOS, o)
	if err != nil {
		return p, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	return p, nil
}

// directoryEnv are the directories the install resolved (BACKSIGHT_HOME,
// XDG_*_HOME), carried into what the user manager runs so that it finds
// the same config and state as the shell.
func directoryEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"BACKSIGHT_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

// userUnitDir is where the systemd user manager reads units: its own
// XDG_CONFIG_HOME, which a shell setting does not change, else ~/.config.
func userUnitDir(ctx context.Context, home string) string {
	dir := filepath.Join(home, ".config", "systemd", "user")
	if out, err := sysRunner.Run(ctx, "systemctl", "--user", "show-environment"); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "XDG_CONFIG_HOME="); ok && v != "" {
				dir = filepath.Join(v, "systemd", "user")
			}
		}
	}
	return dir
}

// unitDirAndEnv are the scheduler's unit directory and directories, which
// the native runtime's units share.
func unitDirAndEnv(ctx context.Context, home string) (string, map[string]string) {
	return userUnitDir(ctx, home), scheduler.TaskEnv(runtime.GOOS, directoryEnv(), os.Getenv)
}

func cmdLine(c []string) string { return strings.Join(c, " ") }

// newJobsStartCmd runs one job now through the OS scheduler, without
// waiting for it: setup starts the first tick right after it queued the
// past sessions, so the import does not wait for the next 15-minute slot.
// The scheduler's run takes the job's own lock, so it never overlaps one
// a timer started.
func newJobsStartCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:       "start <tick|maintain>",
		Short:     "Start a job now through the OS scheduler, without waiting for it",
		Args:      usageArgs(cobra.ExactArgs(1)),
		ValidArgs: []string{"tick", "maintain"},
		Hidden:    true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := schedulerPlan(cmd.Context())
			if err != nil {
				return err
			}
			c, ok := p.Start[args[0]]
			if !ok {
				return &output.Error{Code: output.Usage, Message: "no job " + strconv.Quote(args[0]) + ": tick or maintain"}
			}
			if out, err := sysRunner.Run(cmd.Context(), c[0], c[1:]...); err != nil {
				return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("%s: %v %s", cmdLine(c), err, strings.TrimSpace(string(out)))}
			}
			res := map[string]string{"job": args[0], "command": cmdLine(c)}
			return e.emit("jobs start", res, func() string { return "jobs " + args[0] + " started: " + cmdLine(c) + "\n" })
		},
	}
}

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
			p, err := schedulerPlan(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(d.Data, "bin", plugin.ShimName(schedulerGOOS))); err != nil {
				return &output.Error{Code: output.NeedsUser, Message: "no backsight shim yet: the entries would run nothing. Run backsight client install first",
					Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight client install", Why: "install the shim the scheduler calls"}}}
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
			// Recorded before activation: if it fails, uninstall still knows
			// the files. A reinstall keeps the first install time, so that
			// doctor's grace period is not reset.
			if err := state.Update(d, "jobs install", func(s *state.State) error {
				at := now().UTC()
				if s.Scheduler != nil && s.Scheduler.Kind == p.Kind {
					at = s.Scheduler.InstalledAt
				}
				s.Scheduler = &state.Scheduler{Kind: p.Kind, Files: p.Paths(), InstalledAt: at, Digest: tasksDigest(p)}
				return nil
			}); err != nil {
				return blocked(err)
			}
			for _, c := range p.Activate {
				out, err := sysRunner.Run(cmd.Context(), c[0], c[1:]...)
				// launchd's bootout of an agent not loaded yet fails: expected
				if expected := p.Kind == "launchd" && c[1] == "bootout"; err != nil && !expected {
					if noUserBus(string(out)) {
						// A rerun from this shell fails the same way.
						return &output.Error{Code: output.NeedsUser, Message: fmt.Sprintf("%s: %s: %s (the files are written: backsight jobs uninstall removes them)",
							cmdLine(c), strings.TrimSpace(string(out)), noBusHint), Actions: noBusActions(d)}
					}
					return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("%s: %v %s (the files are written: backsight jobs uninstall removes them)",
						cmdLine(c), err, strings.TrimSpace(string(out)))}
				}
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
			p, err := schedulerPlan(cmd.Context())
			if err != nil {
				return err
			}
			files, remove := p.Paths(), p.Remove
			if st, err := state.Load(d.StateFile()); err == nil && st.Scheduler != nil && len(st.Scheduler.Files) > 0 {
				// Where they were written and the names they were written
				// under, whatever the paths and the project are now.
				files = st.Scheduler.Files
				remove = scheduler.InstalledFrom(files).Remove(st.Scheduler.Kind, os.Getuid())
			}
			res := jobsInstallResult{Kind: p.Kind, Planned: plan}
			for _, c := range remove {
				res.Commands = append(res.Commands, cmdLine(c))
			}
			if !plan {
				var outcomes []deactivation
				for _, c := range remove {
					out, err := sysRunner.Run(cmd.Context(), c[0], c[1:]...)
					outcomes = append(outcomes, deactivation{cmd: c, out: string(out), err: err})
				}
				failed, noBus := stillLoaded(outcomes, p.Kind, files)
				if len(failed) > 0 {
					if noBus {
						return &output.Error{Code: output.Blocked, Message: "could not deactivate the entries: " + strings.Join(failed, "; ") + ": " + noBusHint}
					}
					// The entries may still be loaded: keep their files and the
					// record, so that a rerun (with a user session) finishes.
					return &output.Error{Code: output.Blocked, Message: "could not deactivate the entries: " + strings.Join(failed, "; ")}
				}
				for _, f := range files {
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
			for _, f := range files {
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

// deactivation is one remove command's outcome.
type deactivation struct {
	cmd []string
	out string
	err error
}

// stillLoaded lists the failed deactivations that may leave an entry
// loaded, and whether one failed for want of a user session. An entry
// already gone is not a failure, nor, for systemd, a missing user session
// when no timer was ever enabled (an install whose activation failed):
// nothing can have loaded them.
func stillLoaded(outcomes []deactivation, kind string, files []string) (failed []string, noBus bool) {
	neverEnabled := kind == "systemd" && !scheduler.TimersEnabled(files)
	for _, o := range outcomes {
		line := fmt.Sprintf("%s: %v %s", cmdLine(o.cmd), o.err, strings.TrimSpace(o.out))
		switch {
		case o.err == nil:
		case noUserBus(o.out):
			if !neverEnabled {
				noBus = true
				failed = append(failed, line)
			}
		case !notLoaded(o.out):
			failed = append(failed, line)
		}
	}
	return failed, noBus
}

// noBusActions: after a setup run, its resume without the scheduler;
// alone, the install again from a login session, or the removal of what
// it wrote.
func noBusActions(d paths.Dirs) []output.Action {
	if r, err := setup.Load(d.SetupFile()); err == nil && r != nil {
		return []output.Action{{Kind: "run_as_user", Cmd: "backsight setup --resume --answer scheduler=off", Why: "without a user session the timers cannot load; setup goes on without them"}}
	}
	return []output.Action{
		{Kind: "relogin", Cmd: "backsight jobs install", Why: "run again from a login session of this user (an ssh login, not su or sudo): the timers load into its systemd user manager"},
		{Kind: "run_as_user", Cmd: "backsight jobs uninstall", Why: "on a machine without a user session, remove the entries written; then run backsight jobs tick and jobs maintain by hand"},
	}
}

// noBusHint says where systemd's user manager can be reached.
const noBusHint = setup.NoUserBus + "; on a machine without one (a container), setup takes --answer scheduler=off"

// noUserBus: systemctl --user found no user manager to talk to (a
// container without systemd, a shell started by su or sudo).
func noUserBus(out string) bool {
	o := strings.ToLower(out)
	return strings.Contains(o, "failed to connect to bus") || strings.Contains(o, "failed to connect to user scope bus") ||
		strings.Contains(o, "not been booted with systemd")
}

// notLoaded: the scheduler says the entry is not there, which uninstall
// wants anyway.
func notLoaded(out string) bool {
	o := strings.ToLower(out)
	return strings.Contains(o, "not loaded") || strings.Contains(o, "does not exist") || strings.Contains(o, "not found") ||
		strings.Contains(o, "no such") || strings.Contains(o, "cannot find")
}
