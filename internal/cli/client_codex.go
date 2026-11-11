package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/codexcfg"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// Codex's wiring (block 3, https://github.com/Barben360/backsight/issues/278;
// decision 0085 D1, D2, D5, D8, D10): client install --codex, client
// codex-memories, and what uninstall and setup --undo remove.

// codex is Codex's adapter, for the commands that wire Codex.
var codex = harness.Codex{}

const codexHomeFlag = "Codex's home (default: the one recorded when Codex was wired, else $CODEX_HOME, else ~/.codex)"

// codexTrustText is what the user does once the hooks plugin was added:
// Codex runs no hook it was not shown (0085 D5).
const codexTrustText = "Codex runs backsight's hooks only once you trust them: open Codex and review them in the list it shows at start (or /hooks, or the app's Settings)"

// codexWired is the Codex home client install wired (absolute), "" with
// false when Codex is not wired.
func codexWired(home string, st state.State) (string, bool) {
	c := st.ClientOf(harnessid.Codex)
	if c == nil || c.PluginVersion == "" {
		return "", false
	}
	return codex.Locate(harness.Env{Home: home, Profile: c.Profile}).Dir, true
}

// codexHomeFor is the Codex home a command acts on (absolute): the flag
// when given, else the one recorded at install, else the shell's (0085
// D1: resolved once, then recorded).
func codexHomeFor(d paths.Dirs, home, flag string) (string, error) {
	if flag == "" {
		if st, err := state.Load(d.StateFile()); err == nil {
			if dir, ok := codexWired(home, st); ok {
				return dir, nil
			}
		}
	}
	dir, err := codex.ShellHome(home, flag, os.Getenv)
	if err != nil {
		return "", &output.Error{Code: output.Usage, Message: err.Error()}
	}
	return dir, nil
}

// codexProfile is dir as recorded: "" for ~/.codex.
func codexProfile(home, dir string) string {
	if codex.IsDefaultProfile(home, dir) {
		return ""
	}
	return dir
}

// refuseCodexMove refuses to wire the Codex home dir while backsight is
// still installed in the one recorded, another: recording dir would leave
// that one wired and out of uninstall's reach. Once its plugins are gone
// (removed by hand), dir is recorded instead.
func refuseCodexMove(ctx context.Context, home, dir string, st state.State) error {
	wired, ok := codexWired(home, st)
	if !ok || filepath.Clean(wired) == filepath.Clean(dir) {
		return nil
	}
	if a, errA := os.Stat(wired); errA == nil {
		if b, errB := os.Stat(dir); errB == nil && os.SameFile(a, b) {
			return nil
		}
	}
	there, err := codex.Installed(ctx, harnessEnv(codex, home, codexProfile(home, wired)))
	if err != nil && !errors.Is(err, harness.ErrMissing) {
		return blocked(fmt.Errorf("checking the Codex home backsight is wired to (%s): %w", wired, err))
	}
	if !there && err == nil {
		return nil
	}
	env := "CODEX_HOME=" + wired + " "
	if codexProfile(home, wired) == "" {
		env = ""
	}
	return &output.Error{Code: output.Blocked,
		Message: "backsight is wired into the Codex home " + wired + ", not " + dir + ": wiring " + dir + " would leave " + wired +
			" wired, out of uninstall's reach. Remove backsight's plugins from " + wired + " first, then run this again",
		Actions: []output.Action{{Kind: "run_as_user", Cmd: env + "codex plugin remove " + strings.Join(harness.CodexPluginIDs(), " && "+env+"codex plugin remove ") +
			" && " + env + "codex plugin marketplace remove " + harness.CodexMarketplace,
			Why: "backsight's plugins out of " + wired}},
		Resume: "backsight client install --codex --codex-home " + dir}
}

func needsCodex(msg string) error {
	return &output.Error{Code: output.NeedsUser, Message: msg, Actions: []output.Action{
		{Kind: "install", Cmd: "npm install -g @openai/codex", Why: "backsight wires itself into Codex through its plugin commands"},
		{Kind: "run_as_user", Cmd: "backsight client install --codex", Why: "then wire Codex again"},
	}, Resume: "backsight client install --codex"}
}

// clientInstallCodex wires Codex: the shim (shared with Claude Code), the
// two plugins in their own marketplace, added with the codex command, and
// the Codex home recorded (0085 D2). A user MCP server already named memory
// is refused, named (D3). Nothing is written to config.toml: Codex's own
// commands add the plugins, and the tools' hints spare the approvals (D6).
func clientInstallCodex(ctx context.Context, flag string, plan bool) (clientInstallResult, error) {
	res := clientInstallResult{Plan: plan, Steps: []installStep{}, Client: harnessid.Codex}
	c, err := loadClientEnv("")
	if err != nil {
		return res, err
	}
	_, home, err := machine()
	if err != nil {
		return res, err
	}
	dir, err := codexHomeFor(c.dirs, home, flag)
	if err != nil {
		return res, err
	}
	res.Profile = dir
	profile := codexProfile(home, dir)
	env := harnessEnv(codex, home, profile)
	add := func(name, status, detail string) { res.Steps = append(res.Steps, installStep{name, status, detail}) }
	did := func(changed bool) string {
		switch {
		case plan && changed:
			return "planned"
		case changed:
			return "done"
		}
		return "unchanged"
	}
	if !plan {
		l, _, err := state.Acquire(filepath.Join(c.dirs.LocksDir(), "client-install.lock"), "client install")
		if err != nil {
			return res, &output.Error{Code: output.Blocked, Message: "another client install is running"}
		}
		defer func() { _ = l.Release() }()
	}
	before, _ := state.Load(c.dirs.StateFile())
	if err := refuseCodexMove(ctx, home, dir, before); err != nil {
		return res, err
	}

	// 1. A recent enough codex (0085 D9).
	version, err := codex.Detect(ctx, env)
	if err != nil {
		return res, needsCodex("the codex command is missing or failed: " + err.Error())
	}
	if !atLeast(version, codex.MinVersion()) {
		return res, needsCodex(fmt.Sprintf("Codex %s is older than %s, the oldest version backsight was verified on", version, codex.MinVersion()))
	}
	add("codex", "done", version+" in "+dir)

	// 2. No server of the user's named memory: its tools would be named as
	// backsight's, and backsight's hooks would take its calls for its own.
	shim := filepath.Join(c.dirs.Data, "bin", plugin.ShimName(runtime.GOOS))
	clash, err := codex.NameClash(ctx, env, shim)
	if err != nil {
		return res, blocked(err)
	}
	if clash != "" {
		return res, &output.Error{Code: output.NeedsUser,
			Message: "Codex already has an MCP server named " + plugin.Server + ": " + clash + ". backsight's memory server has that name, so both would share their tool names (" + codex.ToolPrefix() + "…). " +
				"Rename or remove that server, then wire Codex again",
			Actions: []output.Action{{Kind: "run_as_user", Cmd: "codex mcp remove " + plugin.Server, Why: "or rename it in config.toml: the name memory is backsight's"}},
			Resume:  "backsight client install --codex"}
	}

	// 3. The shim every generated file runs.
	shim, target, err := shimSteps(c, plan, before, add, did)
	if err != nil {
		return res, err
	}

	// 4. The two plugins, generated, then added with the codex command.
	inst, err := codex.Install(ctx, env, harness.Integration{Data: c.dirs.Data, Shim: shim, BinaryVersion: buildinfo.Version, Config: c.cfg}, plan)
	for _, s := range inst.Steps {
		if s.Name == "trust" {
			res.Trust = codexTrustText
			if plan {
				res.Trust = "Codex will ask to review backsight's hooks: " + strings.TrimPrefix(codexTrustText, "Codex runs backsight's hooks only once you trust them: ")
			}
			add("trust", map[bool]string{true: "planned", false: "deferred"}[plan], s.Detail+": "+codexTrustText)
			continue
		}
		add(s.Name, did(s.Changed), s.Detail)
	}
	if err != nil {
		return res, installError(err, plan)
	}
	if plan {
		schedulerStep(ctx, true, add)
		bankStep(ctx, true, add)
		return res, nil
	}

	// The Codex home this install wired, from --codex-home, the record or
	// the shell: every later command reads the recorded one. The hash of
	// the program behind the shim: Codex trusts the hooks, not it (D5).
	shimSum, _ := fsutil.HashFile(shim) // "" when it cannot be read
	if err := state.Update(c.dirs, "client install", func(s *state.State) error {
		if s.ShimTarget != "" && s.ShimTarget != target {
			s.ClientFor(harnessid.ClaudeCode).ShimPrevious = s.ShimTarget
		}
		s.ShimTarget = target
		if err := codex.RecordHome(s, profile); err != nil {
			return err
		}
		e := s.ClientFor(harnessid.Codex)
		e.PluginVersion, e.ShimSHA256, e.WiredAt = inst.Version, shimSum, now().UTC()
		return nil
	}); err != nil {
		return res, partialError(err)
	}
	schedulerStep(ctx, false, add)
	bankStep(ctx, false, add)
	return res, nil
}

func newClientCodexMemoriesCmd(e *env) *cobra.Command {
	var plan bool
	var codexHome string
	cmd := &cobra.Command{
		Use:   "codex-memories <off|on>",
		Short: "Turn Codex's own memories off (or on again) in its config.toml, a copy kept first",
		Long: "Codex has a memory of its own (its memories feature): beside backsight's, two memories would answer the same questions. " +
			"off removes memories = true from config.toml's [features], as codex features disable memories does; on sets it again. " +
			"backsight setup --undo puts back what off removed, and uninstall turns them on again.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "off" && args[0] != "on" {
				return &output.Error{Code: output.Usage, Message: "codex-memories takes off or on"}
			}
			res, err := codexMemories(cmd.Context(), args[0] == "off", codexHome, plan)
			return e.report("client codex-memories", res, func() string { return codexMemoriesText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show the change without making it")
	cmd.Flags().StringVar(&codexHome, "codex-home", "", codexHomeFlag)
	return cmd
}

// codexMemories turns Codex's own memories off (or on) in config.toml,
// through the config editor (0085 D8, D10): the state records that
// backsight turned them off, for uninstall.
func codexMemories(ctx context.Context, off bool, flag string, plan bool) (clientConfigResult, error) {
	res := clientConfigResult{Files: []claudecfg.FileResult{}}
	c, err := loadClientEnv("")
	if err != nil {
		return res, err
	}
	_, home, err := machine()
	if err != nil {
		return res, err
	}
	dir, err := codexHomeFor(c.dirs, home, flag)
	if err != nil {
		return res, err
	}
	env := harnessEnv(codex, home, codexProfile(home, dir))
	apply := codexcfg.MemoriesTrue
	if off {
		apply = codexcfg.MemoriesOff
	}
	edit := claudecfg.Edit{Target: codex.Locate(env).Settings, Apply: apply}
	if plan {
		fr, err := claudecfg.Plan([]claudecfg.Edit{edit})
		res.Files = fr
		return res, editError(err)
	}
	run, fr, err := c.runner("client codex-memories").Apply([]claudecfg.Edit{edit})
	res.Run, res.Files = run, fr
	if err != nil {
		return res, editError(err)
	}
	if err := state.Update(c.dirs, "client codex-memories", func(s *state.State) error {
		e := s.ClientFor(harnessid.Codex)
		if !off {
			e.MemoriesOff = false
		} else if len(fr) > 0 && fr[0].Written {
			e.MemoriesOff = true
		}
		return nil
	}); err != nil {
		return res, partialError(err)
	}
	// config.toml is one of the places that turn them on: Codex says
	// whether they still are.
	if on, err := codex.MemoriesOn(ctx, env); err == nil && on == off {
		if off {
			res.Notes = append(res.Notes, "Codex's memories are still on: a profile or a command line flag turns them on (codex features list shows it), turn them off there")
		} else {
			res.Notes = append(res.Notes, "Codex's memories are still off: a profile or a command line flag turns them off (codex features list shows it)")
		}
	}
	return res, nil
}

func codexMemoriesText(r clientConfigResult) string {
	var b strings.Builder
	for _, f := range r.Files {
		for _, ch := range f.Changes {
			fmt.Fprintf(&b, "%s: %s\n", f.Target, ch.Path)
		}
	}
	if b.Len() == 0 {
		b.WriteString("nothing to change\n")
	}
	if r.Run != "" {
		b.WriteString("backup copies: run " + r.Run + "\n")
	}
	for _, n := range r.Notes {
		b.WriteString(n + "\n")
	}
	return b.String()
}

// removeCodex removes backsight from the Codex home dir: its plugins and
// marketplace with Codex's own commands, then, through the config editor
// (command names the run), the [hooks.state] tables Codex leaves behind
// and, with memoriesOn, Codex's own memories turned on again (0085 D8).
// The codex command missing is ErrMissing, with nothing done. An edit
// config.toml holds in a form the narrow editor does not handle is left
// to the user: warnings say what to change by hand, and the removal goes
// on (a rerun would only meet the same file).
func removeCodex(ctx context.Context, d paths.Dirs, home, dir, command string, memoriesOn bool) (detail string, warnings []string, err error) {
	env := harnessEnv(codex, home, codexProfile(home, dir))
	if err := codex.Uninstall(ctx, env); err != nil {
		return "", nil, err
	}
	settings := codex.Locate(env).Settings
	if _, err := os.Stat(settings); errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	runner := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: command, Now: now}
	var done []string
	// One run per edit: a trust table the editor cannot remove never keeps
	// Codex's memories off.
	apply := func(edit func([]byte) ([]byte, []claudecfg.Change, error), byHand func() string) error {
		run, fr, err := runner.Apply([]claudecfg.Edit{{Target: settings, Apply: edit}})
		switch {
		case errors.Is(err, codexcfg.ErrUnsupported):
			warnings = append(warnings, byHand()+" ("+err.Error()+")")
		case err != nil:
			return editError(err)
		case hasChanges(fr):
			done = append(done, changedPaths(fr)+" (backup copies: run "+run+")")
		}
		return nil
	}
	if err := apply(codexcfg.RemoveTrust, func() string {
		raw, _ := os.ReadFile(settings)
		keys, _ := codexcfg.TrustKeys(raw)
		var tables []string
		for _, k := range keys {
			tables = append(tables, `[hooks.state."`+k+`"]`)
		}
		if len(tables) == 0 {
			tables = []string{`[hooks.state."backsight@backsight:…"] and [hooks.state."backsight-hooks@backsight:…"]`}
		}
		return "backsight's hook trust stays in " + settings + ": delete " + strings.Join(tables, ", ") + " (and what they hold) by hand"
	}); err != nil {
		return "", warnings, err
	}
	if memoriesOn {
		if err := apply(codexcfg.MemoriesBackOn, func() string {
			return "Codex's own memories stay off: set memories = true under [features] in " + settings + " by hand, or run codex features enable memories"
		}); err != nil {
			return strings.Join(done, "; "), warnings, err
		}
	}
	return strings.Join(done, "; "), warnings, nil
}

// codexRemovedDetail says what removeCodex did, edits the details.
func codexRemovedDetail(edits string) string {
	out := "plugins " + strings.Join(harness.CodexPluginIDs(), " and ") + " and their marketplace removed"
	if edits != "" {
		out += "; " + edits
	}
	return out
}

// codexHomes are the Codex homes (absolute) backsight wired: the one
// recorded, and those setup's record holds (an install it did not finish).
func codexHomes(home string, st state.State, rec *setup.Record) []string {
	var out []string
	if dir, ok := codexWired(home, st); ok {
		out = append(out, filepath.Clean(dir))
	}
	if rec != nil {
		for _, dir := range rec.Undo.CodexHomes {
			out = appendNew(out, filepath.Clean(dir))
		}
	}
	return out
}

// forgetCodex clears the state's Codex entry when it records dir.
func forgetCodex(d paths.Dirs, home, dir, command string) error {
	return state.Update(d, command, func(s *state.State) error {
		if wired, ok := codexWired(home, *s); ok && filepath.Clean(wired) == filepath.Clean(dir) {
			delete(s.Clients, harnessid.Codex)
		}
		return nil
	})
}
