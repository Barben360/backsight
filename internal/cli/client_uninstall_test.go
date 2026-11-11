package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// wireClaudeRecord records Claude Code as wired (what client install
// writes), so a test sees whether unwiring Codex touches it.
func wireClaudeRecord(t *testing.T, home string) {
	t.Helper()
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.PluginVersion = "1.2.3+claude"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func uninstallCodex(t *testing.T, args ...string) clientUninstallResult {
	t.Helper()
	code, out, errOut := run(t, append([]string{"client", "uninstall", "--codex", "--json"}, args...)...)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var res clientUninstallResult
	jsonData(t, out, &res)
	return res
}

// client uninstall --codex: the plan changes nothing; then the plugins and
// their marketplace go with codex's own commands, the hook trust Codex
// left goes from config.toml through the config editor (journaled under
// this command), Codex's own memories come back on, the Codex record is
// forgotten. Claude Code's record and claude itself are untouched.
func TestClientUninstallCodexKeepsClaudeCode(t *testing.T) {
	home := devInstall(t, true)
	claude := withClaude(t)
	f := withCodex(t, home)
	dir := codexHomeWith(t, home, codexStart)
	installCodex(t)
	trustAll(t, dir)
	f.memories = true
	if code, out, _ := run(t, "client", "codex-memories", "off"); code != output.OK {
		t.Fatalf("off: %s", out)
	}
	wireClaudeRecord(t, home)
	d := pathsOf(home)
	before, _ := os.ReadFile(filepath.Join(dir, "config.toml"))

	f.calls = nil
	plan := uninstallCodex(t, "--plan")
	if len(f.mutating()) != 0 || !plan.Plan || len(plan.Steps) != 1 || plan.Steps[0].Status != "planned" ||
		plan.Steps[0].Name != "codex "+dir || !strings.Contains(plan.Steps[0].Detail, "memories turned on again") {
		t.Fatalf("plan ran %v: %+v", f.mutating(), plan)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "config.toml")); string(after) != string(before) {
		t.Fatal("a plan changed config.toml")
	}
	if st, _ := state.Load(d.StateFile()); st.ClientOf(harnessid.Codex) == nil {
		t.Fatal("a plan forgot Codex")
	}

	res := uninstallCodex(t)
	if got := f.mutating(); !slices.Equal(got, []string{"plugin remove backsight@backsight", "plugin remove backsight-hooks@backsight", "plugin marketplace remove backsight"}) {
		t.Errorf("codex calls %q", got)
	}
	if res.Client != "codex" || len(res.Steps) != 1 || res.Steps[0].Status != "done" || !strings.Contains(res.Steps[0].Detail, "backup copies: run ") {
		t.Errorf("result %+v", res)
	}
	if cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml")); string(cfg) != codexStart+"\n[hooks.state]\n" {
		t.Errorf("config.toml after:\n%q\nwant\n%q", cfg, codexStart+"\n[hooks.state]\n")
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "cache", "backsight")); !os.IsNotExist(err) {
		t.Errorf("the plugin cache is left: %v", err)
	}
	runs, _ := claudecfg.Runs(d.PreImagesDir())
	if len(runs) == 0 {
		t.Fatal("no config editor run")
	}
	if m, err := claudecfg.LoadManifest(d.PreImagesDir(), runs[len(runs)-1]); err != nil || m.Command != "client uninstall" {
		t.Errorf("last run %+v %v", m, err)
	}
	st, _ := state.Load(d.StateFile())
	if st.ClientOf(harnessid.Codex) != nil {
		t.Errorf("Codex still recorded: %+v", st.ClientOf(harnessid.Codex))
	}
	if st.PluginVersion != "1.2.3+claude" || st.ShimTarget == "" {
		t.Errorf("Claude Code's record changed: plugin %q shim %q", st.PluginVersion, st.ShimTarget)
	}
	if len(claude.calls) != 0 {
		t.Errorf("claude ran: %v", claude.calls)
	}
	// Again: nothing wired, nothing done.
	f.calls = nil
	if res := uninstallCodex(t); len(f.mutating()) != 0 || res.Steps[0].Status != "skipped" || !strings.Contains(res.Steps[0].Detail, "not wired") {
		t.Errorf("second run %+v, ran %v", res, f.mutating())
	}
}

// After client uninstall --codex no Codex rollout is read: jobs tick flushes
// nothing of the home it unwired, retain waiting and backfill list nothing.
func TestClientUninstallCodexStopsReadingItsRollouts(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClaude(t)
	withCodex(t, home)
	codexHomeWith(t, home, codexStart)
	withClock(t, time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)) // retain_since before the session
	run(t, "jobs", "tick")
	installCodex(t)
	withClock(t, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	codexRolloutAt(t, filepath.Join(home, ".codex"))
	if got := conversations(home, codex); len(got) != 1 {
		t.Fatalf("wired: listed %v", got)
	}

	uninstallCodex(t)
	if got := conversations(home, codex); len(got) != 0 {
		t.Errorf("unwired: listed %v", got)
	}
	ids, send := waitingSessions(t)
	if slices.Contains(ids, optinSession) || send != 0 {
		t.Errorf("unwired: waiting %v, backfill would send %d", ids, send)
	}
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || cursorFor(home, optinSession) {
		t.Errorf("unwired: jobs tick exit %d, flushed the rollout: %s", code, out)
	}
}

// A home setup's record holds is dropped from it: setup --undo then has no
// Codex left to remove, and runs.
func TestClientUninstallCodexDropsItFromSetupsRecord(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	f := withCodex(t, home)
	dir := codexHomeWith(t, home, codexStart)
	installCodex(t)
	d := pathsOf(home)
	rec := &setup.Record{SchemaVersion: setup.RecordVersion, Undo: setup.Undo{CodexHomes: []string{dir}}}
	if err := rec.Save(d.SetupFile()); err != nil {
		t.Fatal(err)
	}
	uninstallCodex(t)
	if r, _ := setup.Load(d.SetupFile()); r == nil || len(r.Undo.CodexHomes) != 0 {
		t.Fatalf("record %+v", r)
	}
	f.calls = nil
	code, out, errOut := run(t, "setup", "--undo", "--json")
	if code != output.OK || strings.Contains(out, `"codex `) || len(f.mutating()) != 0 {
		t.Errorf("undo: exit %d %s %s, ran %v", code, out, errOut, f.mutating())
	}
}

// Refusals: without --codex (Claude Code's half is backsight uninstall's),
// the other tool's flags; codex missing leaves Codex wired, saying what to do.
func TestClientUninstallRefusals(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	f := withCodex(t, home)
	codexHomeWith(t, home, codexStart)
	installCodex(t)
	code, out, _ := run(t, "client", "uninstall", "--json")
	if code != output.Usage || !strings.Contains(out, "backsight uninstall") || !strings.Contains(out, "--codex") {
		t.Errorf("without --codex: exit %d %s", code, out)
	}
	if code, _, _ := run(t, "client", "uninstall", "--codex-home", home); code != output.Usage {
		t.Errorf("--codex-home alone: exit %d", code)
	}
	if code, _, _ := run(t, "client", "--config-dir", home, "uninstall", "--codex"); code != output.Usage {
		t.Errorf("--config-dir with --codex: exit %d", code)
	}
	if code, _, _ := run(t, "client", "uninstall", "--codex", "--codex-home", filepath.Join(home, "nope")); code != output.Usage {
		t.Errorf("a missing --codex-home: exit %d", code)
	}
	f.missing = true
	code, out, _ = run(t, "client", "uninstall", "--codex", "--non-interactive", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "codex plugin remove backsight-hooks@backsight") {
		t.Errorf("missing codex: exit %d %s", code, out)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.ClientOf(harnessid.Codex) == nil {
		t.Error("Codex forgotten though its plugins stay")
	}
}
