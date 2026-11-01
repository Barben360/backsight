package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

type undoAction struct {
	Name   string `json:"name"`
	Status string `json:"status"` // done | planned | refused | failed | lost
	Detail string `json:"detail,omitempty"`
}

type undoResult struct {
	Actions []undoAction `json:"actions"`
	Kept    []string     `json:"kept"`
	Plan    bool         `json:"plan"`
}

// runSetupUndo reverses what setup changed (decision 0064): the config
// editor runs first, newest first, each by its inverse edit; then the
// scheduler entries, the plugin in each profile setup wired, the server it
// started, and last the reference timers it stopped (a timer that cannot
// start again never leaves backsight wired). Config, secrets and the
// database volume are kept. Each action done is struck from setup.json at
// once, so an interrupted undo continues where it stopped.
func runSetupUndo(ctx context.Context, f setupFlags, force bool) (undoResult, error) {
	res := undoResult{Plan: f.plan, Actions: []undoAction{},
		Kept: []string{"config.toml and secrets.toml: the database volume only opens with the password they hold",
			"the memory server's data: backsight uninstall --delete-data removes it", "backups"}}
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	if !f.plan {
		unlock, err := setupLock(d, "setup --undo")
		if err != nil {
			return res, err
		}
		defer unlock()
	}
	rec, err := setup.Load(d.SetupFile())
	if err != nil {
		return res, blocked(err)
	}
	if rec == nil || rec.Undo.Empty() {
		res.Actions = append(res.Actions, undoAction{Name: "nothing", Status: "done", Detail: "setup changed nothing that is still in place"})
		return res, nil
	}
	add := func(a undoAction) { res.Actions = append(res.Actions, a) }
	save := func() {
		if !f.plan {
			_ = rec.Save(d.SetupFile())
		}
	}
	fail := func(name string, err error) (undoResult, error) {
		add(undoAction{Name: name, Status: "failed", Detail: errMessage(err)})
		save()
		return res, &output.Error{Code: output.Partial, Message: name + ": " + errMessage(err), Resume: "backsight setup --undo"}
	}
	var lost []string

	// Config-editor runs, newest first. Every manifest is read before
	// anything is changed; a run whose pre-images are gone is reported and
	// skipped, never a reason to leave the rest wired.
	runner := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "setup --undo", Now: now}
	type run struct {
		ts string
		m  claudecfg.Manifest
	}
	var runs []run
	for _, ts := range rec.Undo.ConfigRuns {
		m, err := claudecfg.LoadManifest(d.PreImagesDir(), ts)
		if err != nil {
			lost = append(lost, ts)
			continue
		}
		runs = append(runs, run{ts, m})
	}
	for _, ts := range lost {
		add(undoAction{Name: "restore " + ts, Status: "lost", Detail: "its backup copies are gone: check the files it edited by hand"})
		if !f.plan {
			rec.Undo.ConfigRuns = without(rec.Undo.ConfigRuns, ts)
			delete(rec.Undo.Retention, ts)
		}
	}
	save()
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		name := "undo " + r.m.Command + " (" + r.ts + ")"
		kept := map[string]string{}
		edits, whole, err := inverseEdits(d.PreImagesDir(), r.m, rec.Undo.Retention[r.ts], kept)
		if err != nil {
			return fail(name, err)
		}
		var targets []string
		for _, mf := range r.m.Files {
			targets = append(targets, mf.Target)
		}
		if f.plan {
			if whole {
				pr, perr := runner.RestorePlan(r.ts, force)
				status := "planned"
				for _, x := range pr {
					if x.Status == "refused" {
						status = "refused"
					}
				}
				detail := strings.Join(targets, ", ")
				if perr != nil {
					detail += ": " + perr.Error()
				}
				add(undoAction{Name: name, Status: status, Detail: detail})
				continue
			}
			fr, err := claudecfg.Plan(edits)
			if err != nil {
				return fail(name, err)
			}
			add(undoAction{Name: name, Status: "planned", Detail: changedPaths(fr)})
			res.Kept = append(res.Kept, sortedValues(kept)...)
			continue
		}
		if whole {
			for _, t := range targets {
				if force && filepath.Base(t) == ".claude.json" {
					return fail(name, errors.New("--force never puts back a whole ~/.claude.json: Claude Code rewrites it at every start"))
				}
			}
			if _, _, err := runner.Restore(r.ts, force); err != nil {
				if errors.Is(err, claudecfg.ErrChangedSince) {
					add(undoAction{Name: name, Status: "refused", Detail: err.Error()})
					save()
					return res, &output.Error{Code: output.Partial, Resume: "backsight setup --undo --force",
						Message: fmt.Sprintf("%s: %v (setup --undo --force puts the backup copy back over that edit)", name, err)}
				}
				return fail(name, err)
			}
			add(undoAction{Name: name, Status: "done", Detail: strings.Join(targets, ", ")})
		} else {
			_, fr, err := runner.Apply(edits)
			if err != nil {
				return fail(name, err)
			}
			add(undoAction{Name: name, Status: "done", Detail: changedPaths(fr)})
			res.Kept = append(res.Kept, sortedValues(kept)...)
		}
		claudecfg.Unpin(d.PreImagesDir(), r.ts)
		rec.Undo.ConfigRuns = without(rec.Undo.ConfigRuns, r.ts)
		delete(rec.Undo.Retention, r.ts)
		save()
	}

	if rec.Undo.Scheduler {
		if !f.plan {
			if out := subcommand(ctx, "jobs", "uninstall"); out.Err != nil {
				return fail("scheduler", out.Err)
			}
			rec.Undo.Scheduler = false
			save()
		}
		add(undoAction{Name: "scheduler", Status: done(f.plan), Detail: "jobs tick and jobs maintain entries removed"})
	}

	for _, prof := range append([]string(nil), rec.Undo.ClientProfiles...) {
		if !f.plan {
			if err := removePlugin(ctx, profileEnv(home, prof)); err != nil {
				return fail("client "+prof, err)
			}
			if err := state.Update(d, "setup --undo", func(s *state.State) error {
				// Only when the state records this profile ("" is ~/.claude):
				// another profile wired since keeps its plugin.
				wired := filepath.Join(home, ".claude")
				if s.Client != nil && s.Client.Profile != "" {
					wired = filepath.Clean(s.Client.Profile)
				}
				if wired == prof {
					s.PluginVersion = ""
					if s.Client != nil {
						s.Client.Profile = ""
					}
				}
				return nil
			}); err != nil {
				return fail("client "+prof, err)
			}
			rec.Undo.ClientProfiles = without(rec.Undo.ClientProfiles, prof)
			save()
		}
		add(undoAction{Name: "client " + prof, Status: done(f.plan), Detail: "plugin " + plugin.PluginID() + " and its marketplace removed"})
	}

	if rec.Undo.ServerStarted {
		if !f.plan {
			if out := subcommand(ctx, "server", "stop"); out.Err != nil {
				return fail("server", out.Err)
			}
			rec.Undo.ServerStarted = false
			save()
		}
		add(undoAction{Name: "server", Status: done(f.plan), Detail: "stopped; its data is kept"})
	}

	// The reference timers, once their hooks and servers are back. A unit
	// deleted since cannot start again: it is lost, not a reason to stop.
	var lostTimers []string
	for _, t := range append([]string(nil), rec.Undo.Timers...) {
		if !f.plan {
			if out, err := sysRunner.Run(ctx, "systemctl", "--user", "enable", "--now", t); err != nil {
				if !unitGone(ctx, t) {
					return fail("timer "+t, fmt.Errorf("%w %s", err, strings.TrimSpace(string(out))))
				}
				lostTimers = append(lostTimers, t)
				add(undoAction{Name: "timer " + t, Status: "lost", Detail: "its unit file is gone: nothing to start again"})
				rec.Undo.Timers = without(rec.Undo.Timers, t)
				save()
				continue
			}
			rec.Undo.Timers = without(rec.Undo.Timers, t)
			save()
		}
		add(undoAction{Name: "timer " + t, Status: done(f.plan), Detail: "systemd user timer started again"})
	}
	if !f.plan {
		rec.Steps, rec.Finished = nil, false
		if err := rec.Save(d.SetupFile()); err != nil {
			return res, blocked(err)
		}
	}
	var except []string
	if len(lost) > 0 {
		except = append(except, "the config runs whose backup copies are gone ("+strings.Join(lost, ", ")+"): check those files by hand")
	}
	if len(lostTimers) > 0 {
		except = append(except, "the timers whose unit files are gone ("+strings.Join(lostTimers, ", ")+")")
	}
	if len(except) > 0 && !f.plan {
		return res, &output.Error{Code: output.Partial, Message: "undone, except " + strings.Join(except, "; ")}
	}
	return res, nil
}

// unitGone reports whether systemd no longer knows unit (its file deleted).
func unitGone(ctx context.Context, unit string) bool {
	out, err := sysRunner.Run(ctx, "systemctl", "--user", "show", "-p", "LoadState", unit)
	return err == nil && strings.TrimSpace(string(out)) == "LoadState=not-found"
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// inverseEdits are the edits that reverse run m (decision 0064
// amendment); whole is true for a run of an unknown command, which only a
// whole-file restore can reverse. wrote is the retention value the run set
// (JSON), and kept collects, per file, a value left as the user changed it.
func inverseEdits(dir string, m claudecfg.Manifest, wrote string, kept map[string]string) (edits []claudecfg.Edit, whole bool, err error) {
	pre := func(mf claudecfg.ManifestFile) ([]byte, error) {
		if mf.PreImage == nil {
			return nil, nil // the run created the file
		}
		return os.ReadFile(filepath.Join(dir, m.TS, *mf.PreImage))
	}
	switch m.Command {
	case "client retire":
		// The servers retired, read from the ~/.claude.json pre-image: their
		// permission rules come back with them.
		var servers []string
		for _, mf := range m.Files {
			if filepath.Base(mf.Target) == ".claude.json" && mf.PreImage != nil {
				if servers, err = referenceServers(filepath.Join(dir, m.TS, *mf.PreImage)); err != nil {
					return nil, false, err
				}
			}
		}
		for _, mf := range m.Files {
			b, err := pre(mf)
			if err != nil {
				return nil, false, err
			}
			if filepath.Base(mf.Target) == ".claude.json" {
				edits = append(edits, claudecfg.Edit{Target: mf.Target, Apply: unretireMCP(b)})
			} else {
				edits = append(edits, claudecfg.Edit{Target: mf.Target, Apply: unretireSettings(b, servers)})
			}
		}
	case "client install":
		for _, mf := range m.Files {
			b, err := pre(mf)
			if err != nil {
				return nil, false, err
			}
			edits = append(edits, claudecfg.Edit{Target: mf.Target, Apply: removeAdded(b)})
		}
	case "client retention":
		for _, mf := range m.Files {
			b, err := pre(mf)
			if err != nil {
				return nil, false, err
			}
			target := mf.Target
			edits = append(edits, claudecfg.Edit{Target: target, Apply: restoreKey(b, "cleanupPeriodDays", wrote, func(why string) {
				if why == "" {
					delete(kept, target)
				} else {
					kept[target] = target + ": " + why
				}
			})})
		}
	default:
		return nil, true, nil
	}
	return edits, false, nil
}

func changedPaths(fr []claudecfg.FileResult) string {
	var out []string
	for _, f := range fr {
		for _, ch := range f.Changes {
			// Removing several items of one list changes the same index
			// each time: each path is listed once.
			if p := f.Target + ": " + ch.Path; !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		return "already as before"
	}
	return strings.Join(out, ", ")
}

func without(list []string, item string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != item {
			out = append(out, x)
		}
	}
	return out
}

func done(plan bool) string { return map[bool]string{true: "planned", false: "done"}[plan] }

// profileEnv is the environment that points the claude CLI at profile:
// none for the default ~/.claude (CLAUDE_CONFIG_DIR would move
// .claude.json into it), and claudeEnviron drops the shell's.
func profileEnv(home, profile string) []string {
	if profile == "" || filepath.Clean(profile) == filepath.Join(home, ".claude") {
		return nil
	}
	return []string{"CLAUDE_CONFIG_DIR=" + profile}
}

// removePlugin uninstalls backsight's plugin and marketplace when present.
func removePlugin(ctx context.Context, env []string) error {
	known, err := pluginIDs(ctx, env)
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

// setupLock takes setup's lock; a stale one (its process gone) is broken
// and said, a live one names its holder.
func setupLock(d paths.Dirs, command string) (func(), error) {
	path := filepath.Join(d.LocksDir(), "setup.lock")
	l, err := state.TryAcquire(path, command)
	if err != nil {
		if held := state.Inspect(path); held != nil && held.Stale {
			if state.BreakStale(held) == nil {
				if l, err = state.TryAcquire(path, command); err == nil {
					return func() { _ = l.Release() }, nil
				}
			}
		}
		return nil, &output.Error{Code: output.Blocked, Message: "another setup is running (" + err.Error() + "); if it is not, remove " + path}
	}
	return func() { _ = l.Release() }, nil
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
