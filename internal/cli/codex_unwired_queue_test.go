package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// queueToolsBackfill queues a backfill (retain backfill --queue args).
func queueToolsBackfill(t *testing.T, args ...string) {
	t.Helper()
	if code, out, errOut := run(t, append([]string{"retain", "backfill", "--queue", "--json"}, args...)...); code != output.OK {
		t.Fatalf("queue %v: exit %d %s %s", args, code, out, errOut)
	}
}

func backfillQueue(t *testing.T, home string) *state.Backfill {
	t.Helper()
	st, err := state.Load(pathsOf(home).StateFile())
	if err != nil || st.Backfill == nil {
		t.Fatalf("no backfill: %v", err)
	}
	return st.Backfill
}

// client uninstall --codex takes that home's rollouts out of a queued
// backfill (the total too): Claude Code's stay, and jobs tick sends nothing
// of Codex.
func TestClientUninstallCodexEmptiesItsBackfillQueue(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClaude(t)
	withCodex(t, home)
	codexHomeWith(t, home, codexStart)
	withClock(t, time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)) // retain_since before the session
	run(t, "jobs", "tick")
	installCodex(t)
	withClock(t, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	rollout := codexRolloutAt(t, filepath.Join(home, ".codex"))
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	queueToolsBackfill(t, "--tools", "both", "--projects-dir", filepath.Join(home, ".claude", "projects"))
	if b := backfillQueue(t, home); len(b.Sessions) != 2 || b.Total != 2 {
		t.Fatalf("queued %+v", b)
	}

	uninstallCodex(t)
	if b := backfillQueue(t, home); len(b.Sessions) != 1 || slices.Contains(b.Sessions, rollout) || b.Total != 1 {
		t.Errorf("after uninstall: %+v", b)
	}
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || cursorFor(home, optinSession) {
		t.Errorf("jobs tick exit %d, sent the rollout: %s", code, out)
	}
	if b := backfillQueue(t, home); slices.Contains(b.Sessions, rollout) {
		t.Errorf("rollout still queued: %+v", b)
	}
	if !cursorFor(home, "s1") { // Claude Code's queued session was sent
		t.Error("the tick sent nothing of the queue")
	}
}

// A queued rollout whose Codex home is no longer wired (its record removed
// by any other way than client uninstall) is dropped by the backfill, never
// read: no cursor, the queue empty, its total down.
func TestBackfillDropsRolloutsOfAnUnwiredCodexHome(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick")
	withClock(t, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	codexRolloutAt(t, filepath.Join(home, ".codex"))
	wireCodexRecord(t, home)
	queueToolsBackfill(t, "--tools", "codex")
	if b := backfillQueue(t, home); len(b.Sessions) != 1 {
		t.Fatalf("queued %+v", b)
	}
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		delete(s.Clients, harnessid.Codex)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || cursorFor(home, optinSession) {
		t.Errorf("jobs tick exit %d, sent the rollout: %s", code, out)
	}
	if b := backfillQueue(t, home); len(b.Sessions) != 0 || b.Total != b.Done {
		t.Errorf("queue after tick %+v", b)
	}
}

// client uninstall --codex drops that home's codex-memories runs from
// setup's record: a later setup --undo does not put memories = true back
// in a config.toml the user changed since.
func TestClientUninstallCodexDropsItsMemoriesRunsFromSetupsRecord(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	f := withCodex(t, home)
	dir := codexHomeWith(t, home, codexStart)
	installCodex(t)
	f.memories = true
	code, out, _ := run(t, "client", "codex-memories", "off", "--json")
	if code != output.OK {
		t.Fatal(out)
	}
	var mr clientConfigResult
	jsonData(t, out, &mr)
	d := pathsOf(home)
	rec := &setup.Record{SchemaVersion: setup.RecordVersion, Undo: setup.Undo{CodexHomes: []string{dir}, ConfigRuns: []string{mr.Run}}}
	if err := rec.Save(d.SetupFile()); err != nil {
		t.Fatal(err)
	}
	uninstallCodex(t)
	if r, _ := setup.Load(d.SetupFile()); r == nil || len(r.Undo.ConfigRuns) != 0 || len(r.Undo.CodexHomes) != 0 {
		t.Fatalf("record %+v", r)
	}
	// The user turns Codex's memories off again, by hand.
	cfgPath := filepath.Join(dir, "config.toml")
	cfg, _ := os.ReadFile(cfgPath)
	_ = os.WriteFile(cfgPath, []byte(strings.Replace(string(cfg), "memories = true\n", "", 1)), 0o600)
	if code, out, errOut := run(t, "setup", "--undo", "--json"); code != output.OK {
		t.Fatalf("undo: exit %d %s %s", code, out, errOut)
	}
	if after, _ := os.ReadFile(cfgPath); strings.Contains(string(after), "memories = true") {
		t.Errorf("setup --undo put Codex's memories back:\n%s", after)
	}
}

// headlessRolloutAt is the recorded root rollout, started by codex exec,
// without its memory_store call: an explicit save is replayed even from a
// headless session (as Claude Code's), which would queue it for the save.
func headlessRolloutAt(t *testing.T, dir, originator string) {
	t.Helper()
	p := codexRolloutAt(t, dir)
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), `"originator":"codex-tui"`) || !strings.Contains(string(raw), "memory_store") {
		t.Fatal("the fixture changed")
	}
	var lines []string
	for _, l := range strings.SplitAfter(strings.Replace(string(raw), `"originator":"codex-tui"`, `"originator":"`+originator+`"`, 1), "\n") {
		if !strings.Contains(l, "memory_store") {
			lines = append(lines, l)
		}
	}
	st, _ := os.Stat(p)
	_ = os.WriteFile(p, []byte(strings.Join(lines, "")), 0o600)
	_ = os.Chtimes(p, st.ModTime(), st.ModTime())
}

// A headless Codex session (codex exec) is skipped by default: setup's
// estimate does not count it, the backfill queues none. The same session
// started in the TUI is counted (the control).
func TestHeadlessCodexSessionsAreNeitherEstimatedNorQueued(t *testing.T) {
	for _, c := range []struct {
		originator string
		headless   bool
	}{{"codex_exec", true}, {"codex-tui", false}} {
		t.Run(c.originator+"/estimate", func(t *testing.T) {
			home, _ := setupMachine(t)
			withClock(t, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
			withCodex(t, home)
			codexHomeWith(t, home, codexStart)
			headlessRolloutAt(t, filepath.Join(home, ".codex"), c.originator)
			if q, asked := backfillQuestion(t, "--answer", "tools=codex"); asked == c.headless {
				t.Errorf("asked %v (%q), headless %v", asked, q.Prompt, c.headless)
			}
		})
		t.Run(c.originator+"/queue", func(t *testing.T) {
			home := devInstall(t, true)
			pointAt(t, home, fakeHindsight(t))
			withClock(t, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
			headlessRolloutAt(t, filepath.Join(home, ".codex"), c.originator)
			wireCodexRecord(t, home)
			code, out, _ := run(t, "retain", "backfill", "--queue", "--tools", "codex", "--json")
			var env struct {
				Data backfillResult `json:"data"`
			}
			_ = json.Unmarshal([]byte(out), &env)
			want := map[bool]int{true: 0, false: 1}[c.headless]
			if code != output.OK || env.Data.Total != want || env.Data.Plan.OptedOut != 1-want {
				t.Errorf("exit %d, queued %d (want %d), opted out %d: %s", code, env.Data.Total, want, env.Data.Plan.OptedOut, out)
			}
		})
	}
}

// verify --codex without the shim names Codex and its install.
func TestVerifyCodexNeedsTheShim(t *testing.T) {
	home := devInstall(t, true)
	wireCodexRecord(t, home)
	withShim(t, "/nonexistent/backsight")
	code, out, _ := run(t, "verify", "--codex", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "Codex has nothing to run") || !strings.Contains(out, "backsight client install --codex") {
		t.Errorf("exit %d\n%s", code, out)
	}
}
