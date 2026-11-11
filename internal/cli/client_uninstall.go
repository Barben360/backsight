package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/termstyle"
)

// client uninstall --codex unwires Codex alone, Claude Code and the server
// kept (the Codex parity gaps, epic https://github.com/Barben360/backsight/issues/276):
// what uninstall does for Codex (removeCodex, forgetCodex), on its own.
// Claude Code's half is not mirrored: uninstall's offers it back (the
// transcript retention, every profile wired) are more than a small mirror,
// so client uninstall without --codex points to backsight uninstall.

type clientUninstallResult struct {
	Plan      bool         `json:"plan"`
	Client    string       `json:"client"`
	Steps     []undoAction `json:"steps"`
	Warnings  []string     `json:"warnings,omitempty"`
	NextSteps []string     `json:"next_steps,omitempty"`
}

// clientUninstallClaude is the refusal of client uninstall without --codex.
const clientUninstallClaude = "client uninstall unwires Codex alone (--codex), keeping Claude Code; " +
	"to remove backsight from Claude Code, run backsight uninstall (it removes backsight from this machine, Codex's wiring too)"

func newClientUninstallCmd(e *env, configDir *string) *cobra.Command {
	var plan, codexOn, nonInteractive bool
	var codexHome string
	cmd := &cobra.Command{
		Use:   "uninstall --codex",
		Short: "Unwire Codex alone (--codex): its two plugins, their hook trust, and its recorded home; Claude Code and the server stay",
		Long: "Removes backsight's two plugins and their marketplace from Codex with the codex command, the hook trust Codex leaves in config.toml " +
			"(a copy kept first), turns Codex's own memories on again when backsight turned them off, and forgets the Codex home: " +
			"no Codex conversation is read after it. Claude Code's wiring, the server and the memory stay. " +
			"backsight client install --codex wires Codex again.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = nonInteractive // nothing is asked
			if !codexOn {
				if codexHome != "" {
					return &output.Error{Code: output.Usage, Message: "--codex-home goes with --codex"}
				}
				return &output.Error{Code: output.Usage, Message: clientUninstallClaude,
					Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight uninstall --plan", Why: "lists what backsight uninstall removes and keeps"}}}
			}
			if *configDir != "" {
				return &output.Error{Code: output.Usage, Message: "--config-dir is Claude Code's directory: give Codex's with --codex-home"}
			}
			res, err := clientUninstallCodex(cmd.Context(), codexHome, plan)
			return e.report("client uninstall", res, func() string { return clientUninstallText(termstyle.StyleOf(e.stdout), res) }, err)
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would be removed without doing it")
	cmd.Flags().BoolVar(&codexOn, "codex", false, "unwire Codex, keeping Claude Code")
	cmd.Flags().StringVar(&codexHome, "codex-home", "", "Codex's home to unwire (default: the one recorded when Codex was wired)")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "never prompt (this command asks nothing)")
	return cmd
}

// clientUninstallCodex removes backsight from each Codex home it wired (the
// one recorded, and one setup's unfinished run holds), or from --codex-home:
// the plugins, the hook trust, Codex's memories on again when backsight
// turned them off there, then the record, so that no job reads that home's
// conversations again. The config edits go through the config editor
// (copies and a journal, client config restore undoes them); setup's
// record drops the home, so setup --undo does not remove it twice.
func clientUninstallCodex(ctx context.Context, flag string, plan bool) (clientUninstallResult, error) {
	res := clientUninstallResult{Plan: plan, Client: harnessid.Codex, Steps: []undoAction{}}
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	if !plan {
		l, _, err := state.Acquire(filepath.Join(d.LocksDir(), "client-install.lock"), "client uninstall")
		if err != nil {
			return res, &output.Error{Code: output.Blocked, Message: "a client install or uninstall is running"}
		}
		defer func() { _ = l.Release() }()
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return res, blocked(err)
	}
	rec, err := setup.Load(d.SetupFile())
	if err != nil {
		return res, blocked(err)
	}
	homes := codexHomes(home, st, rec)
	if flag != "" {
		dir, err := codex.ShellHome(home, flag, nil)
		if err != nil {
			return res, &output.Error{Code: output.Usage, Message: err.Error()}
		}
		homes = []string{filepath.Clean(dir)}
	}
	if len(homes) == 0 {
		res.Steps = append(res.Steps, undoAction{Name: "codex", Status: "skipped", Detail: "Codex is not wired on this machine"})
		return res, nil
	}
	recorded, _ := codexWired(home, st)
	for _, dir := range homes {
		name := "codex " + dir
		// Codex's memories come back on only in the home whose record says
		// backsight turned them off.
		memories := filepath.Clean(recorded) == dir && st.ClientOf(harnessid.Codex) != nil && st.ClientOf(harnessid.Codex).MemoriesOff
		if plan {
			detail := "plugins " + strings.Join(harness.CodexPluginIDs(), " and ") + ", their marketplace and their hook trust removed; this Codex home's conversations no longer read"
			if memories {
				detail += "; Codex's own memories turned on again"
			}
			res.Steps = append(res.Steps, undoAction{Name: name, Status: "planned", Detail: detail})
			continue
		}
		detail, warnings, err := removeCodex(ctx, d, home, dir, "client uninstall", memories)
		switch {
		case errors.Is(err, harness.ErrMissing):
			res.Steps = append(res.Steps, undoAction{Name: name, Status: "failed", Detail: "the codex command was not found"})
			return res, &output.Error{Code: output.NeedsUser,
				Message: "the codex command was not found, so backsight's plugins cannot be removed from " + dir + "; Codex stays wired",
				Actions: []output.Action{
					{Kind: "install", Cmd: codex.Texts().InstallCmd, Why: "the codex command removes its plugins"},
					{Kind: "run_as_user", Cmd: "codex plugin remove " + strings.Join(harness.CodexPluginIDs(), "; codex plugin remove ") + "; codex plugin marketplace remove " + harness.CodexMarketplace,
						Why: "or remove them by hand once codex is back, then run this again"},
				},
				Resume: "backsight client uninstall --codex"}
		case err != nil:
			res.Steps = append(res.Steps, undoAction{Name: name, Status: "failed", Detail: errMessage(err)})
			return res, &output.Error{Code: output.Partial, Message: name + ": " + errMessage(err) + " (rerun to finish)", Resume: "backsight client uninstall --codex"}
		}
		res.Warnings = append(res.Warnings, warnings...)
		if err := forgetCodex(d, home, dir, "client uninstall"); err != nil {
			return res, partialError(err)
		}
		// Its queued past sessions go too: nothing of this home is read now,
		// and the backfill's total says so.
		if err := state.Update(d, "client uninstall", func(s *state.State) error {
			if b := s.Backfill; b != nil {
				var keep []string
				for _, p := range b.Sessions {
					if !underDir(codex.Locate(harness.Env{Home: home, Profile: dir}).Conversations, p) {
						keep = append(keep, p)
					}
				}
				b.Sessions, b.Total = keep, max(b.Total-(len(b.Sessions)-len(keep)), b.Done)
			}
			return nil
		}); err != nil {
			return res, partialError(err)
		}
		if rec != nil && forgetInRecord(d, rec, dir) {
			if err := rec.Save(d.SetupFile()); err != nil {
				return res, partialError(err)
			}
		}
		res.Steps = append(res.Steps, undoAction{Name: name, Status: "done", Detail: codexRemovedDetail(detail)})
	}
	if !plan {
		res.NextSteps = append(res.NextSteps, "restart Codex so it unloads backsight's plugins", "backsight client install --codex wires Codex again")
	}
	return res, nil
}

// forgetInRecord drops the Codex home dir from setup's record: the home
// (setup --undo would remove it again) and its client codex-memories runs
// (setup --undo would put memories = true back in a config.toml backsight
// no longer owns). It reports a change to save.
func forgetInRecord(d paths.Dirs, rec *setup.Record, dir string) bool {
	changed := false
	var homes []string
	for _, h := range rec.Undo.CodexHomes {
		if filepath.Clean(h) == dir {
			changed = true
			continue
		}
		homes = append(homes, h)
	}
	rec.Undo.CodexHomes = homes
	settings := filepath.Join(dir, "config.toml")
	var runs []string
	for _, ts := range rec.Undo.ConfigRuns {
		if m, err := claudecfg.LoadManifest(d.PreImagesDir(), ts); err == nil && m.Command == "client codex-memories" &&
			slices.ContainsFunc(m.Files, func(f claudecfg.ManifestFile) bool { return filepath.Clean(f.Target) == settings }) {
			changed = true
			delete(rec.Undo.Retention, ts)
			claudecfg.Unpin(d.PreImagesDir(), ts)
			continue
		}
		runs = append(runs, ts)
	}
	rec.Undo.ConfigRuns = runs
	return changed
}

func clientUninstallText(st termstyle.Style, r clientUninstallResult) string {
	var b strings.Builder
	for _, a := range r.Steps {
		fmt.Fprintf(&b, "%s %s", statusCell(st, a.Status, 8), a.Name)
		if a.Detail != "" {
			b.WriteString(": " + a.Detail)
		}
		b.WriteString("\n")
	}
	for _, w := range r.Warnings {
		b.WriteString(st.Paint(termstyle.Warn, "warning:") + " " + w + "\n")
	}
	for _, n := range r.NextSteps {
		b.WriteString(st.Paint(termstyle.Label, "next:") + " " + n + "\n")
	}
	return b.String()
}
