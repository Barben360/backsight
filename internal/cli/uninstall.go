package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

type uninstallFlags struct {
	plan, stop, deleteData, purgeLocal, nonInteractive bool
	answers                                            []string
	answersFile                                        string
}

// retentionOffer is the cleanupPeriodDays backsight raised, offered back
// (decision 0041). Values are JSON ("null": the key was absent).
type retentionOffer struct {
	Settings string `json:"settings"`
	Before   string `json:"before"`
	Current  string `json:"current"`
	// Action: kept | restore (planned) | restored | changed_since (the
	// value is no longer the one backsight wrote: the user's, kept).
	Action string `json:"action"`
}

type uninstallResult struct {
	Plan      bool            `json:"plan"`
	Server    string          `json:"server"` // keep | stop | delete-data
	Steps     []undoAction    `json:"steps"`
	Retention *retentionOffer `json:"retention,omitempty"`
	Kept      []string        `json:"kept"`
	Warnings  []string        `json:"warnings,omitempty"`
	NextSteps []string        `json:"next_steps,omitempty"`
}

func newUninstallCmd(e *env) *cobra.Command {
	var f uninstallFlags
	cmd := &cobra.Command{
		Use:   "uninstall [--plan] [--stop | --delete-data] [--purge-local]",
		Short: "Remove backsight from this machine: plugin, permission rule, scheduler entries, shim and binaries",
		Long: "Removes the plugin and its marketplace, the permission rule, the scheduler entries, the shim, the command on PATH and the binaries; " +
			"offers back the transcript retention backsight raised. Config, secrets and state stay unless --purge-local. " +
			"The server stays (default), stops with --stop, or is deleted with its memories with --delete-data, which needs the bank id typed " +
			"(or --answer confirm_bank=<id>). Backups are never deleted.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runUninstall(cmd.Context(), e, f)
			return e.report("uninstall", res, func() string { return uninstallText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&f.plan, "plan", false, "list what would be removed and kept, changing nothing")
	cmd.Flags().BoolVar(&f.stop, "stop", false, "also stop the local server (its data is kept)")
	cmd.Flags().BoolVar(&f.deleteData, "delete-data", false, "also delete the local server and every memory in its bank (asks for the bank id)")
	cmd.Flags().BoolVar(&f.purgeLocal, "purge-local", false, "also delete config.toml, secrets.toml and the state directory (backups stay)")
	cmd.Flags().BoolVar(&f.nonInteractive, "non-interactive", false, "never prompt: exit 10 when an answer is needed")
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "answer a question: confirm_bank=<id>, retention=keep|restore, identity_saved=yes")
	cmd.Flags().StringVar(&f.answersFile, "answers-file", "", "read answers from a file (id=value lines or a JSON object)")
	return cmd
}

func (f uninstallFlags) resume() string {
	cmd := "backsight uninstall"
	for _, fl := range []struct {
		on   bool
		name string
	}{{f.stop, "--stop"}, {f.deleteData, "--delete-data"}, {f.purgeLocal, "--purge-local"}} {
		if fl.on {
			cmd += " " + fl.name
		}
	}
	for _, a := range f.answers {
		cmd += " --answer " + a
	}
	return cmd
}

var uninstallAnswers = map[string][]string{"confirm_bank": nil, "retention": {"keep", "restore"}, "identity_saved": {"yes"}}

func runUninstall(ctx context.Context, e *env, f uninstallFlags) (uninstallResult, error) {
	res := uninstallResult{Plan: f.plan, Server: "keep", Steps: []undoAction{}, Kept: []string{}}
	switch {
	case f.stop && f.deleteData:
		return res, &output.Error{Code: output.Usage, Message: "--stop and --delete-data exclude each other"}
	case f.stop:
		res.Server = "stop"
	case f.deleteData:
		res.Server = "delete-data"
	}
	answers, err := parseAnswers(f.answers, f.answersFile)
	if err != nil {
		return res, err
	}
	for k, v := range answers {
		allowed, known := uninstallAnswers[k]
		if !known {
			return res, &output.Error{Code: output.Usage, Message: "unknown answer " + k + ": uninstall asks confirm_bank, retention and identity_saved"}
		}
		if allowed != nil && !slices.Contains(allowed, v) {
			return res, &output.Error{Code: output.Usage, Message: fmt.Sprintf("answer %s=%s: want %s", k, v, strings.Join(allowed, " or "))}
		}
	}
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	// Never in the middle of a restore, an update or a backup: the way
	// back from each needs the binary and its state (§11.4, 0067).
	if err := refuseWhileBusy(d); err != nil {
		return res, err
	}
	if state.UpdateIncomplete(d) {
		return res, &output.Error{Code: output.Blocked, Message: "an update did not finish (" + d.UpdateMarker() + "): run backsight doctor for the way back"}
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if errors.Is(err, fs.ErrNotExist) {
		cfg, err = config.Default(d, home), nil
	}
	if err != nil {
		return res, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	if cfg.Server.Mode != "local" && (f.stop || f.deleteData) {
		return res, &output.Error{Code: output.Usage, Message: "server.mode is " + cfg.Server.Mode + ": the server runs on another machine; run backsight uninstall " + res.flag() + " there"}
	}
	st, err := state.Load(d.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, blocked(err)
	}
	rec, err := setup.Load(d.SetupFile())
	if err != nil {
		return res, blocked(err)
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, blocked(err)
	}
	backups := cfg.Backup.Dir
	if backups == "" {
		backups = filepath.Join(d.Data, "backups")
	}
	deployed := ""
	if cfg.Server.Mode == "local" {
		if deployed, err = runtimeFor(cfg).Deployed(); err != nil {
			return res, blocked(err)
		}
	}
	profiles := wiredProfiles(home, st, rec)
	offer := retentionOfferFor(home, st, rec, cfg)
	interactive := !e.json && !f.nonInteractive && !f.plan && stdinIsTerminal() && stdoutIsTerminal()

	// Every answer before any change.
	if !f.plan {
		var missing []setup.Question
		var actions []output.Action
		if f.deleteData && deployed != "" {
			got, ok := answers["confirm_bank"]
			if !ok && interactive {
				fmt.Fprintf(e.stdout, "\nThis deletes the memory server and every memory in bank %s. Backups are kept.\nType the bank id (%s) to confirm: ", cfg.Server.Bank, cfg.Server.Bank)
				if got, err = readLine(); err != nil {
					return res, blocked(err)
				}
				ok = true
			}
			switch {
			case !ok:
				missing = append(missing, setup.Question{ID: "confirm_bank", Required: true, Free: true,
					Prompt: "--delete-data deletes the memory server and every memory in bank " + cfg.Server.Bank + " (backups are kept): type the bank id to confirm"})
			case got != cfg.Server.Bank:
				return res, &output.Error{Code: output.Usage, Message: fmt.Sprintf("the bank id %q is not this server's (%s): nothing was changed", got, cfg.Server.Bank)}
			}
		}
		if offer != nil && offer.Action == "kept" {
			choice, ok := answers["retention"]
			if !ok && interactive {
				q := setup.Question{ID: "retention", Default: "keep", Recommended: "keep",
					Prompt: fmt.Sprintf("backsight raised Claude Code's transcript retention (cleanupPeriodDays) from %s to %s.", showJSON(offer.Before), offer.Current),
					Options: []setup.Option{{Value: "keep", Label: "keep " + offer.Current + " days: transcripts stay available for a later reinstall or backfill"},
						{Value: "restore", Label: "put back " + showJSON(offer.Before), Caveat: "Claude Code then deletes the transcripts older than that"}}}
				if choice, err = ask(e, q); err != nil {
					return res, err
				}
				if !slices.Contains(uninstallAnswers["retention"], choice) {
					return res, &output.Error{Code: output.Usage, Message: "retention: want keep or restore: nothing was changed"}
				}
			}
			if choice == "restore" {
				offer.Action = "restore"
			}
		}
		if f.purgeLocal && sec.BackupIdentity != "" && cfg.Backup.Recipient == "" && hasFiles(backups) && answers["identity_saved"] != "yes" {
			saved := false
			if interactive {
				saved = confirm(e, "\nYour backups in "+backups+" open only with the backup identity in secrets.toml, which --purge-local deletes.\n"+
					"Saved it elsewhere (backsight backup identity show)? [y/N] ")
			}
			if !saved {
				missing = append(missing, setup.Question{ID: "identity_saved", Required: true, Options: []setup.Option{{Value: "yes", Label: "the backup identity is saved outside this machine"}},
					Prompt: "--purge-local deletes secrets.toml, which holds the only key to the backups in " + backups + ": save it first, then answer yes"})
				actions = append(actions, output.Action{Kind: "run_as_user", Cmd: "backsight backup identity show", Why: "print the backup identity to keep it somewhere safe: without it the backups cannot be restored"})
			}
		}
		if len(missing) > 0 {
			var ids []string
			for _, q := range missing {
				ids = append(ids, q.ID)
			}
			return res, &output.Error{Code: output.NeedsUser, Questions: missing, Actions: actions, Resume: f.resume() + " --answer id=value",
				Message: "uninstall needs answers to " + strings.Join(ids, ", ") + " (--answer id=value): nothing was changed"}
		}
	} else if offer != nil && offer.Action == "kept" && answers["retention"] == "restore" {
		offer.Action = "restore"
	}
	res.Retention = offer

	// What exists before uninstall's own lock creates the state directory.
	var local []string
	for _, dir := range []string{d.Config, d.State} {
		if _, err := os.Stat(dir); err == nil {
			local = append(local, dir)
		}
	}
	var release func()
	if !f.plan {
		if release, err = setupLock(d, "uninstall"); err != nil {
			return res, err
		}
		defer func() {
			if release != nil {
				release()
			}
		}()
	}
	add := func(a undoAction) { res.Steps = append(res.Steps, a) }
	fail := func(name string, err error) (uninstallResult, error) {
		add(undoAction{Name: name, Status: "failed", Detail: errMessage(err)})
		return res, &output.Error{Code: output.Partial, Message: name + ": " + errMessage(err) + " (rerun to finish)", Resume: f.resume()}
	}
	saveRecord := func(change func(u *setup.Undo)) {
		if rec != nil && !f.plan {
			change(&rec.Undo)
			_ = rec.Save(d.SetupFile())
		}
	}

	// 1. The scheduler entries first: no job runs while the rest goes.
	if a, err := uninstallScheduler(ctx, f.plan, st); err != nil {
		return fail("scheduler", err)
	} else {
		add(a)
		saveRecord(func(u *setup.Undo) { u.Scheduler = false })
	}

	// 2. The plugin and its marketplace, in each profile backsight wired.
	if len(profiles) == 0 {
		add(undoAction{Name: "client", Status: "skipped", Detail: "no Claude Code profile is wired"})
	}
	for _, prof := range profiles {
		name := "client " + prof
		env := profileEnv(home, prof)
		there, err := pluginPresent(ctx, env)
		switch {
		case errors.Is(err, errClaudeMissing):
			add(undoAction{Name: name, Status: "skipped", Detail: "the claude command was not found"})
			res.Warnings = append(res.Warnings, "claude was not found: remove the plugin by hand once it is back (claude plugin uninstall "+plugin.PluginID()+"; claude plugin marketplace remove "+plugin.Marketplace+")")
			continue
		case err != nil:
			return fail(name, err)
		case !there:
			add(undoAction{Name: name, Status: "skipped", Detail: "plugin not installed there"})
			continue
		}
		if !f.plan {
			if err := removePlugin(ctx, env); err != nil {
				return fail(name, err)
			}
		}
		add(undoAction{Name: name, Status: done(f.plan), Detail: "plugin " + plugin.PluginID() + " and its marketplace removed"})
	}

	// 3. Claude Code's settings, through the config editor (0042): the
	// permission rule, and the retention when the user takes it back.
	var edits []claudecfg.Edit
	for _, prof := range profiles {
		settings := claudecode.Locate(home, profileGetenv(home, prof)).Settings
		if _, err := os.Stat(settings); err != nil {
			continue
		}
		edits = append(edits, claudecfg.Edit{Target: settings, Apply: removeRules})
	}
	if offer != nil && offer.Action == "restore" {
		pre := []byte(`{"cleanupPeriodDays":` + offer.Before + `}`)
		if offer.Before == "null" {
			pre = []byte(`{}`) // absent before: removed again
		}
		edits = append(edits, claudecfg.Edit{Target: offer.Settings, Apply: restoreKey(pre, "cleanupPeriodDays", offer.Current, func(string) {})})
	}
	planned, err := claudecfg.Plan(edits)
	if err != nil {
		return fail("claude settings", editError(err))
	}
	switch {
	case !hasChanges(planned):
		add(undoAction{Name: "claude settings", Status: "skipped", Detail: "nothing of backsight's left"})
	case f.plan:
		add(undoAction{Name: "claude settings", Status: "planned", Detail: changedPaths(planned)})
	default:
		run, fr, err := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "uninstall", Now: now}.Apply(edits)
		if err != nil {
			return fail("claude settings", editError(err))
		}
		add(undoAction{Name: "claude settings", Status: "done", Detail: changedPaths(fr) + " (pre-images: run " + run + ")"})
	}
	if offer != nil && offer.Action == "restore" && !f.plan {
		offer.Action = "restored"
	}
	if !f.plan {
		if err := state.Update(d, "uninstall", func(s *state.State) error {
			s.PluginVersion, s.ShimTarget = "", ""
			if s.Client != nil {
				s.Client.Profile, s.Client.ShimPrevious = "", ""
				if offer != nil && offer.Action == "restored" {
					s.Client.RetentionBefore = nil
				}
			}
			return nil
		}); err != nil {
			return fail("claude settings", err)
		}
		saveRecord(func(u *setup.Undo) { u.ClientProfiles = nil })
	}

	// 4. The server: kept, stopped, or deleted with its data.
	if a, err := uninstallServer(ctx, d, cfg, res.Server, f.plan); err != nil {
		return fail("server", err)
	} else {
		add(a)
		if a.Status == "done" {
			saveRecord(func(u *setup.Undo) { u.ServerStarted = false })
		}
	}

	// 5. The command on PATH, the shim, the binaries and the marketplace.
	if a, err := uninstallBinaries(d, backups, f.plan); err != nil {
		return fail("binaries", err)
	} else {
		add(a)
	}

	// 6. Local files.
	switch {
	case len(local) == 0:
		add(undoAction{Name: "local files", Status: "skipped", Detail: "no config or state here"})
	case f.purgeLocal:
		if !f.plan {
			release()
			release = nil
			for _, dir := range local {
				if err := removeAllExcept(dir, backups); err != nil {
					return fail("local files", err)
				}
			}
			removeIfEmpty(filepath.Join(d.Data, "server"))
			removeIfEmpty(d.Data)
		}
		add(undoAction{Name: "local files", Status: done(f.plan), Detail: strings.Join(local, ", ") + " deleted (config.toml, secrets.toml, the state)"})
	default:
		add(undoAction{Name: "local files", Status: map[bool]string{true: "planned", false: "kept"}[f.plan], Detail: "kept: " + strings.Join(local, ", ")})
		res.Kept = append(res.Kept, "config.toml and secrets.toml in "+d.Config+" (the server's keys, the LLM key and the backup identity)", "the state in "+d.State+", the config editor's pre-images included")
	}
	if res.Server != "delete-data" && deployed != "" {
		res.Kept = append(res.Kept, "the memory server's data (backsight uninstall --delete-data removes it)")
	}
	res.Kept = append(res.Kept, "backups in "+backups+": never deleted")

	if rec != nil && (len(rec.Undo.Timers) > 0 || retiredRuns(d, rec) > 0) {
		res.Warnings = append(res.Warnings, "setup retired a reference install's pieces (hooks, MCP servers or timers): uninstall does not bring them back; run backsight setup --undo first to put them back")
	}
	if f.purgeLocal && res.Server != "delete-data" && deployed != "" {
		res.Warnings = append(res.Warnings, "the kept server's database password now lives only in "+filepath.Join(cfg.Server.ComposeDir, ".env")+": keep that file to reach its data again")
	}
	if !f.plan && !slices.Contains(local, d.State) && release != nil {
		// No state before: leave none behind for the lock.
		release()
		release = nil
		removeIfEmpty(d.LocksDir())
		removeIfEmpty(d.State)
	}
	if len(profiles) > 0 {
		res.NextSteps = append(res.NextSteps, "restart Claude Code (or /reload-plugins): its sessions still run the removed plugin")
	}
	return res, nil
}

func (r uninstallResult) flag() string {
	if r.Server == "stop" {
		return "--stop"
	}
	return "--delete-data"
}

// wiredProfiles are the Claude Code directories (absolute) backsight's
// plugin may be in: the one client install recorded and those setup wired.
func wiredProfiles(home string, st state.State, rec *setup.Record) []string {
	var out []string
	addOne := func(p string) {
		if p = filepath.Clean(p); !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if st.PluginVersion != "" {
		p := filepath.Join(home, ".claude")
		if st.Client != nil && st.Client.Profile != "" {
			p = st.Client.Profile
		}
		addOne(p)
	}
	if rec != nil {
		for _, p := range rec.Undo.ClientProfiles {
			addOne(p)
		}
	}
	return out
}

// profileGetenv locates a profile as Claude Code would with it.
func profileGetenv(home, profile string) func(string) string {
	return func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" && filepath.Clean(profile) != filepath.Join(home, ".claude") {
			return profile
		}
		return ""
	}
}

// retentionOfferFor is the retention backsight raised, read from the
// profile client install wired; nil when backsight never changed it.
func retentionOfferFor(home string, st state.State, rec *setup.Record, cfg config.Config) *retentionOffer {
	if st.Client == nil || len(st.Client.RetentionBefore) == 0 {
		return nil
	}
	prof := filepath.Join(home, ".claude")
	if st.Client.Profile != "" {
		prof = st.Client.Profile
	}
	settings := claudecode.Locate(home, profileGetenv(home, prof)).Settings
	raw, err := os.ReadFile(settings)
	if err != nil {
		return nil
	}
	cur, there, err := claudecfg.Get(raw, []string{"cleanupPeriodDays"})
	if err != nil || !there {
		return nil // removed since: nothing of backsight's is left
	}
	now, _ := json.Marshal(cur)
	before := canonJSON(string(st.Client.RetentionBefore))
	if before == "" {
		before = "null"
	}
	if string(now) == before {
		return nil
	}
	o := &retentionOffer{Settings: settings, Before: before, Current: string(now), Action: "changed_since"}
	var wrote []string
	if cfg.Client.RetentionDays > 0 {
		wrote = append(wrote, fmt.Sprint(cfg.Client.RetentionDays))
	}
	if rec != nil {
		for _, v := range rec.Undo.Retention {
			wrote = append(wrote, canonJSON(v))
		}
	}
	if slices.Contains(wrote, string(now)) {
		o.Action = "kept"
	}
	return o
}

func showJSON(v string) string {
	if v == "null" {
		return "Claude Code's default (30 days)"
	}
	return v + " days"
}

// removeRules drops the memory tools' permission rules (SPEC §5): the
// tools are gone with the plugin.
func removeRules(doc []byte) ([]byte, []claudecfg.Change, error) {
	if _, there, err := claudecfg.Get(doc, []string{"permissions", "allow"}); err != nil || !there {
		return doc, nil, err
	}
	return claudecfg.RemoveWhere(doc, []string{"permissions", "allow"}, func(_ string, v any) bool {
		s, _ := v.(string)
		return slices.Contains(plugin.ToolNames, s)
	})
}

func hasChanges(fr []claudecfg.FileResult) bool {
	for _, f := range fr {
		if len(f.Changes) > 0 {
			return true
		}
	}
	return false
}

// pluginPresent reports whether backsight's plugin or marketplace is
// registered with the profile env points at (read only).
func pluginPresent(ctx context.Context, env []string) (bool, error) {
	known, err := pluginIDs(ctx, env)
	if err != nil {
		return false, err
	}
	path, err := marketplacePath(ctx, env)
	if err != nil {
		return false, err
	}
	return known[plugin.PluginID()] || path != "", nil
}

// uninstallScheduler removes the jobs entries through jobs uninstall,
// when any was installed.
func uninstallScheduler(ctx context.Context, plan bool, st state.State) (undoAction, error) {
	a := undoAction{Name: "scheduler"}
	p := subcommand(ctx, "jobs", "uninstall", "--plan")
	if p.Err != nil {
		return a, p.Err
	}
	var r jobsInstallResult
	_ = json.Unmarshal(p.Data, &r)
	var there []string
	for _, f := range r.Files {
		if _, err := os.Lstat(f.Target); err == nil {
			there = append(there, f.Target)
		}
	}
	if st.Scheduler == nil && len(there) == 0 {
		a.Status, a.Detail = "skipped", "no entries"
		return a, nil
	}
	if !plan {
		if out := subcommand(ctx, "jobs", "uninstall"); out.Err != nil {
			return a, out.Err
		}
	}
	a.Status, a.Detail = done(plan), "jobs tick and jobs maintain entries removed ("+strings.Join(there, ", ")+")"
	return a, nil
}

func uninstallServer(ctx context.Context, d paths.Dirs, cfg config.Config, mode string, plan bool) (undoAction, error) {
	a := undoAction{Name: "server"}
	if cfg.Server.Mode != "local" {
		a.Status, a.Detail = "skipped", "the server runs on another machine ("+cfg.Server.URL+"): uninstall it there"
		return a, nil
	}
	rt := runtimeFor(cfg)
	deployed, err := rt.Deployed()
	if err != nil {
		return a, err
	}
	if deployed == "" {
		a.Status, a.Detail = "skipped", "no server installed here"
		return a, nil
	}
	if mode == "keep" {
		a.Status, a.Detail = "kept", "it keeps its memories; stop it by hand with: "+rt.StopCmd()
		return a, nil
	}
	if plan {
		a.Status = "planned"
		a.Detail = map[string]string{"stop": "stopped, its data kept", "delete-data": "deleted with every memory in bank " + cfg.Server.Bank + " (asks for the bank id); backups kept"}[mode]
		return a, nil
	}
	unlock, err := serverLock(d, "uninstall")
	if err != nil {
		return a, err
	}
	defer unlock()
	if mode == "stop" {
		if err := rt.Stop(ctx); err != nil {
			return a, err
		}
		a.Status, a.Detail = "done", "stopped, its data kept"
		return a, nil
	}
	if err := rt.Delete(ctx); err != nil {
		return a, err
	}
	removeIfEmpty(cfg.Server.ComposeDir)
	a.Status, a.Detail = "done", "deleted with every memory in bank "+cfg.Server.Bank+"; backups kept"
	return a, nil
}

// uninstallBinaries removes the command on PATH when it is backsight's
// link, then <data>/bin (shim and versioned binaries) and the marketplace.
func uninstallBinaries(d paths.Dirs, backups string, plan bool) (undoAction, error) {
	a := undoAction{Name: "binaries"}
	bin, mkt := filepath.Join(d.Data, "bin"), filepath.Join(d.Data, "marketplace")
	var gone []string
	link := ""
	if runtime.GOOS != "windows" && d.Bin != "" {
		p := filepath.Join(d.Bin, "backsight")
		if dest, err := os.Readlink(p); err == nil {
			if !filepath.IsAbs(dest) {
				dest = filepath.Join(d.Bin, dest)
			}
			if strings.HasPrefix(filepath.Clean(dest), bin+string(filepath.Separator)) {
				link = p
				gone = append(gone, p)
			}
		}
	}
	for _, p := range []string{bin, mkt} {
		if _, err := os.Lstat(p); err == nil {
			gone = append(gone, p)
		}
	}
	if len(gone) == 0 {
		a.Status, a.Detail = "skipped", "none installed"
		return a, nil
	}
	if !plan {
		if link != "" {
			if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return a, err
			}
		}
		for _, p := range []string{bin, mkt} {
			if err := removeAllExcept(p, backups); err != nil {
				return a, err
			}
		}
	}
	a.Status, a.Detail = done(plan), strings.Join(gone, ", ")
	return a, nil
}

// removeAllExcept removes root and everything under it, except keep and
// the directories that lead to it.
func removeAllExcept(root, keep string) error {
	root, keep = filepath.Clean(root), filepath.Clean(keep)
	rel, err := filepath.Rel(root, keep)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return os.RemoveAll(root) // keep is elsewhere
	}
	if rel == "." {
		return nil
	}
	ents, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := removeAllExcept(filepath.Join(root, e.Name()), keep); err != nil {
			return err
		}
	}
	removeIfEmpty(root)
	return nil
}

func removeIfEmpty(dir string) {
	if ents, err := os.ReadDir(dir); err == nil && len(ents) == 0 {
		_ = os.Remove(dir)
	}
}

func hasFiles(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(_ string, de fs.DirEntry, err error) error {
		if err == nil && de.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// retiredRuns counts setup's config runs that retired reference pieces.
func retiredRuns(d paths.Dirs, rec *setup.Record) int {
	n := 0
	for _, ts := range rec.Undo.ConfigRuns {
		if m, err := claudecfg.LoadManifest(d.PreImagesDir(), ts); err == nil && m.Command == "client retire" {
			n++
		}
	}
	return n
}

func uninstallText(r uninstallResult) string {
	var b strings.Builder
	for _, a := range r.Steps {
		fmt.Fprintf(&b, "%-8s %s", a.Status, a.Name)
		if a.Detail != "" {
			b.WriteString(": " + a.Detail)
		}
		b.WriteString("\n")
	}
	if o := r.Retention; o != nil {
		switch o.Action {
		case "kept":
			fmt.Fprintf(&b, "kept     transcript retention %s days (it was %s): --answer retention=restore puts it back\n", o.Current, showJSON(o.Before))
		case "restore", "restored":
			fmt.Fprintf(&b, "%-8s transcript retention back to %s\n", map[bool]string{true: "planned", false: "done"}[r.Plan], showJSON(o.Before))
		case "changed_since":
			fmt.Fprintf(&b, "kept     transcript retention %s days: changed since backsight set it\n", o.Current)
		}
	}
	for _, k := range r.Kept {
		b.WriteString("kept: " + k + "\n")
	}
	for _, w := range r.Warnings {
		b.WriteString("warning: " + w + "\n")
	}
	for _, n := range r.NextSteps {
		b.WriteString("next: " + n + "\n")
	}
	return b.String()
}
