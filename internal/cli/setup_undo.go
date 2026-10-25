package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

type undoAction struct {
	Name   string `json:"name"`
	Status string `json:"status"` // done | planned | refused | failed
	Detail string `json:"detail,omitempty"`
}

type undoResult struct {
	Actions []undoAction `json:"actions"`
	Kept    []string     `json:"kept"`
	Plan    bool         `json:"plan"`
}

// runSetupUndo reverses what setup changed, newest first (decision 0064):
// the reference timers it stopped, the config-editor runs it made, the
// scheduler entries, the plugin, and the server it started. Config,
// secrets and the database volume are kept. Each action done is struck
// from setup.json at once, so an interrupted undo continues where it
// stopped.
func runSetupUndo(ctx context.Context, f setupFlags, force bool) (undoResult, error) {
	res := undoResult{Plan: f.plan, Actions: []undoAction{},
		Kept: []string{"config.toml and secrets.toml: the database volume only opens with the password they hold",
			"the memory server's data: backsight uninstall --delete-data removes it", "backups"}}
	d, _, err := machine()
	if err != nil {
		return res, err
	}
	rec, err := setup.Load(d.SetupFile())
	if err != nil {
		return res, blocked(err)
	}
	if rec == nil || rec.Undo.Empty() {
		res.Actions = append(res.Actions, undoAction{Name: "nothing", Status: "done", Detail: "setup changed nothing that is still in place"})
		return res, nil
	}
	if !f.plan {
		lock, err := state.TryAcquire(filepath.Join(d.LocksDir(), "setup.lock"), "setup --undo")
		if err != nil {
			return res, &output.Error{Code: output.Blocked, Message: "a setup is running"}
		}
		defer func() { _ = lock.Release() }()
	}
	add := func(a undoAction) { res.Actions = append(res.Actions, a) }
	save := func() error { return rec.Save(d.SetupFile()) }
	fail := func(name string, err error) (undoResult, error) {
		add(undoAction{Name: name, Status: "failed", Detail: errMessage(err)})
		_ = save()
		return res, &output.Error{Code: output.Partial, Message: name + ": " + errMessage(err), Resume: "backsight setup --undo"}
	}
	var env []string
	if rec.Profile != "" {
		env = []string{"CLAUDE_CONFIG_DIR=" + rec.Profile}
	}

	for len(rec.Undo.Timers) > 0 {
		t := rec.Undo.Timers[len(rec.Undo.Timers)-1]
		if !f.plan {
			if out, err := sysRunner.Run(ctx, "systemctl", "--user", "enable", "--now", t); err != nil {
				return fail("timer "+t, fmt.Errorf("%w %s", err, strings.TrimSpace(string(out))))
			}
		}
		add(undoAction{Name: "timer " + t, Status: done(f.plan), Detail: "systemd user timer started again"})
		if f.plan {
			break
		}
		rec.Undo.Timers = rec.Undo.Timers[:len(rec.Undo.Timers)-1]
		_ = save()
	}

	runner := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "setup --undo", Now: now}
	runs := append([]string(nil), rec.Undo.ConfigRuns...)
	for i := len(runs) - 1; i >= 0; i-- {
		ts := runs[i]
		m, err := claudecfg.LoadManifest(d.PreImagesDir(), ts)
		if err != nil {
			return fail("restore "+ts, fmt.Errorf("its pre-images are gone (%w): restore the files by hand", err))
		}
		var targets []string
		for _, mf := range m.Files {
			targets = append(targets, mf.Target)
		}
		name := "restore " + ts + " (" + m.Command + ")"
		if f.plan {
			add(undoAction{Name: name, Status: "planned", Detail: strings.Join(targets, ", ")})
			continue
		}
		if _, _, err := runner.Restore(ts, force); err != nil {
			if errors.Is(err, claudecfg.ErrChangedSince) {
				add(undoAction{Name: name, Status: "refused", Detail: err.Error()})
				_ = save()
				return res, &output.Error{Code: output.Partial, Resume: "backsight setup --undo --force",
					Message: fmt.Sprintf("%s: %v (backsight setup --undo --force puts the pre-image back over that edit)", name, err)}
			}
			return fail(name, err)
		}
		add(undoAction{Name: name, Status: "done", Detail: strings.Join(targets, ", ")})
		rec.Undo.ConfigRuns = rec.Undo.ConfigRuns[:i]
		_ = save()
	}

	if rec.Undo.Scheduler {
		if !f.plan {
			if out := subcommand(ctx, "jobs", "uninstall"); out.Err != nil {
				return fail("scheduler", out.Err)
			}
			rec.Undo.Scheduler = false
			_ = save()
		}
		add(undoAction{Name: "scheduler", Status: done(f.plan), Detail: "jobs tick and jobs maintain entries removed"})
	}

	if rec.Undo.Client {
		if !f.plan {
			if err := removePlugin(ctx, env); err != nil {
				return fail("client", err)
			}
			if err := state.Update(d, "setup --undo", func(s *state.State) error {
				s.PluginVersion = ""
				if s.Client != nil {
					s.Client.Profile = ""
				}
				return nil
			}); err != nil {
				return fail("client", err)
			}
			rec.Undo.Client = false
			_ = save()
		}
		add(undoAction{Name: "client", Status: done(f.plan), Detail: "plugin " + plugin.PluginID() + " and its marketplace removed from Claude Code"})
	}

	if rec.Undo.ServerStarted {
		if !f.plan {
			if out := subcommand(ctx, "server", "stop"); out.Err != nil {
				return fail("server", out.Err)
			}
			rec.Undo.ServerStarted = false
			_ = save()
		}
		add(undoAction{Name: "server", Status: done(f.plan), Detail: "stopped; its data is kept"})
	}
	if !f.plan {
		rec.Steps, rec.Finished = nil, false
		if err := save(); err != nil {
			return res, blocked(err)
		}
	}
	return res, nil
}

func done(plan bool) string { return map[bool]string{true: "planned", false: "done"}[plan] }

// removePlugin uninstalls backsight's plugin and marketplace when present.
func removePlugin(ctx context.Context, env []string) error {
	known, err := listed(ctx, env, "id", "plugin", "list", "--json")
	if err != nil {
		return err
	}
	if known[plugin.PluginID()] {
		if _, err := claudeRunner.Run(ctx, env, "plugin", "uninstall", plugin.PluginID(), "--scope", "user"); err != nil {
			return err
		}
	}
	path, err := marketplacePath(ctx, env)
	if err != nil {
		return err
	}
	if path != "" {
		if _, err := claudeRunner.Run(ctx, env, "plugin", "marketplace", "remove", plugin.Marketplace); err != nil {
			return err
		}
	}
	return nil
}

func undoText(r undoResult) string {
	var b strings.Builder
	for _, a := range r.Actions {
		fmt.Fprintf(&b, "%-8s %s", a.Status, a.Name)
		if a.Detail != "" {
			b.WriteString(": " + a.Detail)
		}
		b.WriteString("\n")
	}
	for _, k := range r.Kept {
		b.WriteString("kept: " + k + "\n")
	}
	return b.String()
}
