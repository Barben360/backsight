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
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/retain"
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
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "answer a question: confirm_bank=<id>, retention=keep|restore, identity_saved=yes, pre_images_ack=yes, drop_queued=yes")
	cmd.Flags().StringVar(&f.answersFile, "answers-file", "", "read answers from a file (id=value lines or a JSON object)")
	return cmd
}

// resume is the command that continues this run, every flag kept.
func (f uninstallFlags) resume() string {
	cmd := "backsight uninstall"
	for _, fl := range []struct {
		on   bool
		name string
	}{{f.stop, "--stop"}, {f.deleteData, "--delete-data"}, {f.purgeLocal, "--purge-local"}, {f.nonInteractive, "--non-interactive"}} {
		if fl.on {
			cmd += " " + fl.name
		}
	}
	if f.answersFile != "" {
		cmd += " --answers-file " + shellWord(f.answersFile)
	}
	for _, a := range f.answers {
		cmd += " --answer " + shellWord(a)
	}
	return cmd
}

// shellWord quotes s for a POSIX shell when it is not one plain word.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_=./:,@+%") == "" {
		return s
	}
	return shellQuote(s)
}

// jobLockWait bounds the wait for a running jobs tick or jobs maintain.
var jobLockWait = 2 * time.Minute

var uninstallAnswers = map[string][]string{"confirm_bank": nil, "retention": {"keep", "restore"}, "identity_saved": {"yes"},
	"pre_images_ack": {"yes"}, "drop_queued": {"yes"}}

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
			return res, &output.Error{Code: output.Usage, Message: "unknown answer " + k + ": uninstall asks confirm_bank, retention, identity_saved, pre_images_ack and drop_queued"}
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
		return res, &output.Error{Code: output.Blocked, Message: "server.mode is " + cfg.Server.Mode + ": the server runs on another machine; run backsight uninstall " + res.flag() + " there"}
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
	// Archives are kept wherever they are: the configured directory and
	// the default one, which holds those made before backup.dir changed.
	keep := []string{backups, filepath.Join(d.Data, "backups")}
	deployed := ""
	if cfg.Server.Mode == "local" {
		if deployed, err = runtimeFor(cfg).Deployed(); err != nil {
			return res, blocked(err)
		}
	}
	profiles := wiredProfiles(home, st, rec)
	offer := retentionOfferFor(home, st, rec, cfg)
	retired := 0
	if rec != nil {
		retired = retiredRuns(d, rec)
	}
	queued := queuedWrites(d)
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
		// --purge-local deletes the state and secrets: what only they hold
		// is asked for first (0067 block 3 review rules).
		purgeAsk := func(id, prompt, saved string, act output.Action) {
			if !f.purgeLocal || answers[id] == "yes" {
				return
			}
			if interactive && confirm(e, "\n"+prompt+"\n"+saved+" [y/N] ") {
				return
			}
			missing = append(missing, setup.Question{ID: id, Required: true, Prompt: prompt + ": answer yes once done",
				Options: []setup.Option{{Value: "yes", Label: saved}}})
			actions = append(actions, act)
		}
		if sec.BackupIdentity != "" {
			// Whatever backup.recipient says: archives made before it was set
			// open only with this identity.
			purgeAsk("identity_saved", "--purge-local deletes secrets.toml and the backup identity it holds, the only key to the backups made with it",
				"the backup identity is saved outside this machine",
				output.Action{Kind: "run_as_user", Cmd: "backsight backup identity show", Why: "print the backup identity to keep it somewhere safe: without it the backups cannot be restored"})
		}
		if retired > 0 {
			purgeAsk("pre_images_ack", "--purge-local deletes the pre-images of setup's reference retire, the only way to put the reference install's hooks and MCP servers back (backsight setup --undo)",
				"the reference install is not wanted back, or setup --undo already ran",
				output.Action{Kind: "run_as_user", Cmd: "backsight setup --undo", Why: "put the reference install's pieces back before their pre-images go"})
		}
		if queued.total() > 0 {
			purgeAsk("drop_queued", "--purge-local deletes "+queued.String()+", never sent to the server",
				"they can be dropped",
				output.Action{Kind: "run_as_user", Cmd: "backsight jobs tick", Why: "send the queued notes while the server answers; quarantined ones need backsight retain retry-quarantine"})
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

	// What exists before uninstall's own locks create the state directory.
	var local []string
	for _, dir := range []string{d.Config, d.State} {
		if _, err := os.Stat(dir); err == nil {
			local = append(local, dir)
		}
	}
	// The locks, in the state directory: setup's, then the jobs' (a running
	// tick or maintain finishes first, and none starts until the end), then
	// the server's when it is stopped or deleted. Released before
	// --purge-local deletes their directory.
	var held []func()
	releaseLocks := func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i]()
		}
		held = nil
	}
	defer releaseLocks()
	if !f.plan {
		unlock, err := setupLock(d, "uninstall")
		if err != nil {
			return res, err
		}
		held = append(held, unlock)
		unlocks, err := waitForJobs(ctx, d, func(msg string) {
			if !e.json {
				fmt.Fprintln(e.stderr, msg)
			}
		})
		held = append(held, unlocks...)
		if err != nil {
			releaseLocks()
			cleanLocks(d, local)
			return res, err
		}
		if res.Server != "keep" && deployed != "" {
			unlock, err := serverLock(d, "uninstall")
			if err != nil {
				releaseLocks()
				cleanLocks(d, local)
				return res, err
			}
			held = append(held, unlock)
		}
	} else {
		for _, name := range jobLocks {
			if h := state.Inspect(filepath.Join(d.LocksDir(), name+".lock")); h != nil && !h.Stale {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s is running (pid %d): uninstall waits up to %s for it to finish", h.Owner.Command, h.Owner.PID, jobLockWait))
			}
		}
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
	if _, err := os.Stat(d.StateFile()); err == nil && !f.plan { // none on a machine without state
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
	}
	saveRecord(func(u *setup.Undo) { u.ClientProfiles = nil })

	// 4. The server: kept, stopped, or deleted with its data.
	if a, err := uninstallServer(ctx, cfg, res.Server, f.plan); err != nil {
		return fail("server", err)
	} else {
		add(a)
		if a.Status == "done" {
			saveRecord(func(u *setup.Undo) { u.ServerStarted = false })
		}
	}

	// 5. Local files, before the binaries: a failure here still resumes.
	switch {
	case len(local) == 0:
		add(undoAction{Name: "local files", Status: "skipped", Detail: "no config or state here"})
	case f.purgeLocal:
		var foreign []string
		if f.plan {
			foreign = foreignEntries(d)
		} else {
			releaseLocks()
			var err error
			if foreign, err = purgeLocal(d, keep); err != nil {
				return fail("local files", err)
			}
		}
		detail := strings.Join(local, ", ") + " deleted (config.toml, secrets.toml, the state)"
		if n := queued.total(); n > 0 {
			detail += ", with " + queued.String()
		}
		add(undoAction{Name: "local files", Status: done(f.plan), Detail: detail})
		for _, p := range foreign {
			res.Kept = append(res.Kept, p+": not written by backsight")
		}
		if f.plan && queued.total() > 0 {
			res.Warnings = append(res.Warnings, "--purge-local drops "+queued.String()+" (asks drop_queued): backsight jobs tick sends the queued notes first")
		}
	default:
		add(undoAction{Name: "local files", Status: map[bool]string{true: "planned", false: "kept"}[f.plan], Detail: "kept: " + strings.Join(local, ", ")})
		res.Kept = append(res.Kept, "config.toml and secrets.toml in "+d.Config+" (the server's keys, the LLM key and the backup identity)", "the state in "+d.State+", the config editor's pre-images included")
	}

	// 6. Last, the command on PATH, the shim, the binaries and the
	// marketplace. A failure here is not resumable (the state may be gone
	// and the binary with it): what is left is listed, to remove by hand.
	a, warn, err := uninstallBinaries(d, keep, f.plan)
	res.Warnings = append(res.Warnings, warn...)
	if err != nil {
		add(undoAction{Name: "binaries", Status: "failed", Detail: errMessage(err)})
		return res, &output.Error{Code: output.Blocked, Message: "binaries: " + errMessage(err) + "; remove by hand: " + strings.Join(stillThere(a.paths), ", ")}
	}
	add(a.undoAction)
	if !f.plan {
		removeIfEmpty(filepath.Join(d.Data, "server"))
		removeIfEmpty(d.Data)
	}
	if res.Server != "delete-data" && deployed != "" {
		res.Kept = append(res.Kept, "the memory server's data (backsight uninstall --delete-data removes it)")
	}
	res.Kept = append(res.Kept, "backups in "+backups+": never deleted")

	if rec != nil && (len(rec.Undo.Timers) > 0 || retired > 0) {
		res.Warnings = append(res.Warnings, "setup retired a reference install's pieces (hooks, MCP servers or timers): uninstall does not bring them back; run backsight setup --undo first to put them back")
	}
	if f.purgeLocal && res.Server != "delete-data" && deployed != "" {
		res.Warnings = append(res.Warnings, "the kept server's database password now lives only in "+filepath.Join(cfg.Server.ComposeDir, ".env")+": keep that file to reach its data again")
	}
	if !f.plan {
		releaseLocks()
		cleanLocks(d, local)
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
	// systemd: the services too, once their timers are off (disable
	// --now stops a timer, not the job it started).
	var services []string
	if r.Kind == "systemd" {
		for _, f := range r.Files {
			if strings.HasSuffix(f.Target, ".service") {
				services = append(services, filepath.Base(f.Target))
			}
		}
	}
	if !plan {
		if out := subcommand(ctx, "jobs", "uninstall"); out.Err != nil {
			return a, out.Err
		}
		if len(services) > 0 {
			args := append([]string{"--user", "stop"}, services...)
			if out, err := sysRunner.Run(ctx, "systemctl", args...); err != nil && !notLoaded(string(out)) {
				return a, fmt.Errorf("systemctl --user stop %s: %w %s", strings.Join(services, " "), err, strings.TrimSpace(string(out)))
			}
		}
	}
	a.Status, a.Detail = done(plan), "jobs tick and jobs maintain entries removed ("+strings.Join(there, ", ")+")"
	if len(services) > 0 {
		a.Detail += ", " + strings.Join(services, " and ") + " stopped"
	}
	return a, nil
}

// jobLocks are the locks jobs tick and jobs maintain hold while they run.
var jobLocks = []string{"job-tick", "job-maintain"}

// waitForJobs takes the jobs' locks, waiting up to jobLockWait for a
// running one to finish: none runs, and none starts, while uninstall
// removes what it uses. Still held after the wait → exit 20, nothing
// changed. The release functions come back even on error.
func waitForJobs(ctx context.Context, d paths.Dirs, say func(string)) ([]func(), error) {
	var unlocks []func()
	deadline := time.Now().Add(jobLockWait)
	said := map[string]bool{}
	for _, name := range jobLocks {
		path := filepath.Join(d.LocksDir(), name+".lock")
		for {
			l, _, err := state.Acquire(path, "uninstall")
			if err == nil {
				unlocks = append(unlocks, func() { _ = l.Release() })
				break
			}
			var held *state.HeldError
			if !errors.As(err, &held) {
				return unlocks, blocked(err)
			}
			if !said[name] {
				said[name] = true
				say(fmt.Sprintf("waiting up to %s for %s (pid %d) to finish...", jobLockWait, held.Owner.Command, held.Owner.PID))
			}
			if time.Now().After(deadline) {
				return unlocks, &output.Error{Code: output.Blocked, Message: fmt.Sprintf("%s is still running after %s (%s): nothing was changed; rerun once it finishes",
					held.Owner.Command, jobLockWait, held.Error())}
			}
			select {
			case <-ctx.Done():
				return unlocks, blocked(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return unlocks, nil
}

// cleanLocks leaves no locks or state directory behind when there was no
// state before uninstall (local lists what existed then).
func cleanLocks(d paths.Dirs, local []string) {
	if !slices.Contains(local, d.State) {
		removeIfEmpty(d.LocksDir())
		removeIfEmpty(d.State)
	}
}

// pendingWrites is what has not reached the server yet (doctor's writes check).
type pendingWrites struct{ notes, qSessions, qNotes int }

func queuedWrites(d paths.Dirs) pendingWrites {
	ob := retain.Outbox{Path: filepath.Join(d.OutboxDir(), "notes.jsonl"), QuarantinePath: filepath.Join(d.QuarantineDir(), "notes.jsonl")}
	var q pendingWrites
	q.notes, _ = ob.Pending()
	q.qNotes, _ = ob.Quarantined()
	sessions, _ := retain.QuarantinedSessions(d.CursorsDir())
	q.qSessions = len(sessions)
	return q
}

func (q pendingWrites) total() int { return q.notes + q.qSessions + q.qNotes }

func (q pendingWrites) String() string {
	return fmt.Sprintf("%d note(s) queued, %d session(s) and %d note(s) quarantined", q.notes, q.qSessions, q.qNotes)
}

// ownEntry reports whether name, directly in dir, is one backsight writes.
func ownEntry(d paths.Dirs, dir, name string) bool {
	if strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-") {
		return true // an atomic write's temporary file
	}
	var own []string
	switch dir {
	case d.Config:
		own = []string{d.ConfigFile(), d.SecretsFile()}
	case d.State:
		if strings.HasPrefix(name, "update-") {
			return true // an update's staging directory
		}
		own = []string{d.StateFile(), d.LocksDir(), d.MaintenanceLock(), d.CursorsDir(), d.SessionsDir(), d.SentIndexDir(),
			d.OutboxDir(), d.QuarantineDir(), d.JournalFile(), d.PreImagesDir(), d.TelemetryDir(), d.EntitiesFile(), d.SetupFile(),
			d.RestoreMarker(), d.UpdateMarker(), d.UpdateSnapshot(), d.BankTemplateFile()}
		for _, n := range []string{"staging", "logs", "breaker.json", "doctor-snapshot.json", "machine_id"} {
			own = append(own, filepath.Join(d.State, n))
		}
	}
	return slices.Contains(own, filepath.Join(dir, name))
}

// foreignEntries are the entries of the config and state directories that
// backsight did not write: --purge-local keeps them.
func foreignEntries(d paths.Dirs) []string {
	var out []string
	for _, dir := range []string{d.Config, d.State} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if !ownEntry(d, dir, e.Name()) {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	return out
}

// purgeLocal deletes what backsight wrote in the config and state
// directories, never the backups (keep) nor an entry it did not write,
// which it returns.
func purgeLocal(d paths.Dirs, keep []string) ([]string, error) {
	foreign := foreignEntries(d)
	for _, dir := range []string{d.Config, d.State} {
		ents, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return foreign, err
		}
		for _, e := range ents {
			if ownEntry(d, dir, e.Name()) {
				if err := removeAllExcept(filepath.Join(dir, e.Name()), keep...); err != nil {
					return foreign, err
				}
			}
		}
		removeIfEmpty(dir)
	}
	return foreign, nil
}

func uninstallServer(ctx context.Context, cfg config.Config, mode string, plan bool) (undoAction, error) {
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
	// The caller holds the server lock.
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

// binariesAction is the binaries step and the paths it removes.
type binariesAction struct {
	undoAction
	paths []string
}

// uninstallBinaries removes the command on PATH when it is backsight's
// link, then <data>/bin (shim and versioned binaries) and the marketplace.
// A command on PATH that is not that link stays, with a warning.
func uninstallBinaries(d paths.Dirs, keep []string, plan bool) (binariesAction, []string, error) {
	a := binariesAction{undoAction: undoAction{Name: "binaries"}}
	var warn []string
	bin, mkt := filepath.Join(d.Data, "bin"), filepath.Join(d.Data, "marketplace")
	link := ""
	if runtime.GOOS != "windows" && d.Bin != "" {
		p := filepath.Join(d.Bin, "backsight")
		if _, err := os.Lstat(p); err == nil {
			dest, err := os.Readlink(p)
			if err == nil && !filepath.IsAbs(dest) {
				dest = filepath.Join(d.Bin, dest)
			}
			if err == nil && strings.HasPrefix(filepath.Clean(dest), bin+string(filepath.Separator)) {
				link = p
				a.paths = append(a.paths, p)
			} else {
				warn = append(warn, p+" is not backsight's link into "+bin+": kept; remove it by hand if it is an old backsight")
			}
		}
	}
	for _, p := range []string{bin, mkt} {
		if _, err := os.Lstat(p); err == nil {
			a.paths = append(a.paths, p)
		}
	}
	if len(a.paths) == 0 {
		a.Status, a.Detail = "skipped", "none installed"
		return a, warn, nil
	}
	if !plan {
		if link != "" {
			if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return a, warn, err
			}
		}
		for _, p := range []string{bin, mkt} {
			if err := removeAllExcept(p, keep...); err != nil {
				return a, warn, err
			}
		}
	}
	a.Status, a.Detail = done(plan), strings.Join(a.paths, ", ")
	return a, warn, nil
}

// stillThere are the paths that exist.
func stillThere(list []string) []string {
	var out []string
	for _, p := range list {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// removeAllExcept removes root and everything under it, except the keep
// paths and the directories that lead to them. A keep path is also
// compared through its symlinks, so a backup directory reached by a link
// into root is kept.
func removeAllExcept(root string, keep ...string) error {
	var keeps []string
	for _, k := range keep {
		keeps = append(keeps, filepath.Clean(k))
		if r, err := filepath.EvalSymlinks(k); err == nil {
			keeps = append(keeps, r)
		}
	}
	return removeKeeping(filepath.Clean(root), keeps)
}

func removeKeeping(root string, keeps []string) error {
	real := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		real = r
	}
	inside := false
	for _, k := range keeps {
		for _, base := range []string{root, real} {
			rel, err := filepath.Rel(base, k)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if rel == "." {
				return nil
			}
			inside = true
		}
	}
	if !inside {
		return removeAll(root) // a link is removed, never what it points at
	}
	ents, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := removeKeeping(filepath.Join(root, e.Name()), keeps); err != nil {
			return err
		}
	}
	removeIfEmpty(root)
	return nil
}

// removeAll is os.RemoveAll with an error that is safe to print: Go
// 1.26.0's can return one whose Error() panics (an internal errSymlink,
// for a link it could not remove from a read-only directory).
func removeAll(p string) error {
	if err := os.RemoveAll(p); err != nil {
		return printable(err, p)
	}
	return nil
}

func printable(err error, p string) (out error) {
	defer func() {
		if recover() != nil {
			out = fmt.Errorf("cannot remove everything under %s", p)
		}
	}()
	_ = err.Error()
	return err
}

func removeIfEmpty(dir string) {
	if ents, err := os.ReadDir(dir); err == nil && len(ents) == 0 {
		_ = os.Remove(dir)
	}
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
