package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

type fakeRename struct {
	banks   []string
	pending int
	failOn  string // "from→to" of the real rename that fails
	calls   []string
}

func (f *fakeRename) Banks(context.Context) ([]string, error) { return slices.Clone(f.banks), nil }

func (f *fakeRename) Pending(_ context.Context, banks []string) (int, error) {
	f.calls = append(f.calls, "pending "+strings.Join(banks, ","))
	return f.pending, nil
}

func (f *fakeRename) Rename(_ context.Context, from, to string, dry bool) error {
	if dry {
		f.calls = append(f.calls, "dry "+from+"→"+to)
		return nil
	}
	f.calls = append(f.calls, "rename "+from+"→"+to)
	if f.failOn == from+"→"+to {
		return errors.New("rename-bank: lock timeout")
	}
	return nil
}

func (f *fakeRename) Alias(_ context.Context, bank, alias string) error {
	f.calls = append(f.calls, "alias "+alias+"→"+bank)
	return nil
}

func withRename(t *testing.T, f *fakeRename) {
	t.Helper()
	old, oldWait, oldPoll := renameOpsFor, renameWaitHand, renamePoll
	renameOpsFor = func(config.Config, config.Secrets) (renameOps, error) { return f, nil }
	t.Cleanup(func() { renameOpsFor, renameWaitHand, renamePoll = old, oldWait, oldPoll })
}

var legacyBanks = []string{"claude-memory", "claude-memory-control", "claude-memory-docs", "other"}

// A legacy install: its bank and this machine's records of it.
func legacyInstall(t *testing.T) string {
	t.Helper()
	home := devInstall(t, true)
	d := pathsOf(home)
	_ = state.Update(d, "test", func(s *state.State) error {
		s.Tombstones = []state.Tombstone{{Bank: "claude-memory", Document: "conversation:s0"}, {Bank: "other", Document: "conversation:s9"}}
		s.RestoreEpoch = &state.RestoreEpoch{Bank: "claude-memory", Epoch: 2}
		s.PurgeGuard = &state.PurgeGuard{Bank: "claude-memory"}
		s.PurgeHold = &state.PurgeHold{Bank: "claude-memory", Documents: 1}
		return nil
	})
	_ = os.MkdirAll(d.CursorsDir(), 0o700)
	if err := retain.SaveCursor(d.CursorsDir(), "s1", retain.Cursor{Transcript: "/t/s1.jsonl", Offset: 42, Bank: "claude-memory"}); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestBankRenameMovesTheLegacyBanksAndThisMachinesRecords(t *testing.T) {
	home := legacyInstall(t)
	f := &fakeRename{banks: legacyBanks}
	withRename(t, f)
	if code, out, errOut := run(t, "bank", "rename", "--plan"); code != output.OK || !strings.Contains(out, "claude-memory → backsight") || len(f.calls) != 0 {
		t.Fatalf("plan: exit %d, calls %q\n%s%s", code, f.calls, out, errOut)
	}
	code, out, errOut := run(t, "bank", "rename")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	want := []string{"pending claude-memory,claude-memory-docs,claude-memory-control",
		"dry claude-memory→backsight", "dry claude-memory-docs→backsight-docs", "dry claude-memory-control→backsight-control",
		"rename claude-memory→backsight", "rename claude-memory-docs→backsight-docs", "rename claude-memory-control→backsight-control",
		"alias claude-memory→backsight", "alias claude-memory-docs→backsight-docs", "alias claude-memory-control→backsight-control"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls\n%q\nwant\n%q", f.calls, want)
	}
	d := pathsOf(home)
	if cfg, err := config.Load(d.ConfigFile(), d, home); err != nil || cfg.Server.Bank != "backsight" {
		t.Errorf("config bank %q (%v)", cfg.Server.Bank, err)
	}
	st, _ := state.Load(d.StateFile())
	if st.Tombstones[0].Bank != "backsight" || st.Tombstones[1].Bank != "other" || st.RestoreEpoch.Bank != "backsight" || st.PurgeGuard.Bank != "backsight" || st.PurgeHold.Bank != "backsight" {
		t.Errorf("state not moved: %+v %+v %+v %+v", st.Tombstones, st.RestoreEpoch, st.PurgeGuard, st.PurgeHold)
	}
	// A cursor left on the old bank would send its whole session again.
	if cur, _, _ := retain.LoadCursor(d.CursorsDir(), "s1"); cur.Bank != "backsight" || cur.Offset != 42 {
		t.Errorf("cursor %+v", cur)
	}
	// Done: the config names the new bank, nothing is asked of the server.
	f.calls = nil
	if code, out, _ := run(t, "bank", "rename"); code != output.OK || !strings.Contains(out, "keeps its name") || len(f.calls) != 0 {
		t.Errorf("second run: exit %d, calls %q\n%s", code, f.calls, out)
	}
}

// Nothing changes when a bank cannot move: both names taken, operations
// still running, or a rename failing half way (the ones before it go back).
func TestBankRenameChangesNothingWhenABankCannotMove(t *testing.T) {
	for _, c := range []struct {
		name    string
		f       fakeRename
		message string
		renamed []string // real renames run, in order
	}{
		{"both names", fakeRename{banks: append(slices.Clone(legacyBanks), "backsight")}, "both exist", nil},
		{"busy", fakeRename{banks: legacyBanks, pending: 3}, "3 operation(s) still running", nil},
		{"half way", fakeRename{banks: legacyBanks, failOn: "claude-memory-docs→backsight-docs"}, "renamed back",
			[]string{"rename claude-memory→backsight", "rename claude-memory-docs→backsight-docs", "rename backsight→claude-memory"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := legacyInstall(t)
			f := c.f
			withRename(t, &f)
			renameWaitHand, renamePoll = 30*time.Millisecond, 5*time.Millisecond
			code, _, errOut := run(t, "bank", "rename")
			if code != output.Blocked || !strings.Contains(errOut, c.message) {
				t.Errorf("exit %d %q, want %q", code, errOut, c.message)
			}
			var renamed []string
			for _, call := range f.calls {
				if strings.HasPrefix(call, "rename ") || strings.HasPrefix(call, "alias ") {
					renamed = append(renamed, call)
				}
			}
			if !slices.Equal(renamed, c.renamed) {
				t.Errorf("renames %q, want %q", renamed, c.renamed)
			}
			d := pathsOf(home)
			if cfg, _ := config.Load(d.ConfigFile(), d, home); cfg.Server.Bank != "claude-memory" {
				t.Errorf("config moved to %q", cfg.Server.Bank)
			}
			if cur, _, _ := retain.LoadCursor(d.CursorsDir(), "s1"); cur.Bank != "claude-memory" {
				t.Errorf("cursor moved to %q", cur.Bank)
			}
		})
	}
}

// Only a local legacy bank moves: a remote server is renamed on its host
// (this client reaches it through the alias), another name stays.
func TestBankRenameLeavesOtherBanksAlone(t *testing.T) {
	for _, c := range []struct{ cfg, want string }{
		{"\nbank = \"team\"\n", "bank team keeps its name"},
		{"\nmode = \"remote\"\n", "the server is remote"},
	} {
		home := devInstall(t, true)
		d := pathsOf(home)
		raw, _ := os.ReadFile(d.ConfigFile())
		_ = os.WriteFile(d.ConfigFile(), []byte(strings.Replace(string(raw), "\nui_port", c.cfg+"ui_port", 1)), 0o600)
		f := &fakeRename{banks: legacyBanks}
		withRename(t, f)
		if code, out, errOut := run(t, "bank", "rename"); code != output.OK || !strings.Contains(out, c.want) || len(f.calls) != 0 {
			t.Errorf("%q: exit %d, calls %q\n%s%s", c.cfg, code, f.calls, out, errOut)
		}
	}
}

// update apply runs the new binary's bank apply after its commit point:
// that is where an older install's banks move, before the template goes to
// the renamed bank.
func TestBankApplyRenamesALegacyBankFirst(t *testing.T) {
	home := legacyInstall(t)
	pointAt(t, home, fakeHindsight(t))
	f := &fakeRename{banks: legacyBanks}
	withRename(t, f)
	code, out, errOut := run(t, "bank", "apply")
	if code != output.OK || !strings.Contains(out, "claude-memory → backsight renamed") || !strings.Contains(out, "bank backsight") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !slices.Contains(f.calls, "rename claude-memory→backsight") {
		t.Errorf("calls %q", f.calls)
	}
	// Server busy: the template goes to the old bank, which keeps working.
	home = legacyInstall(t)
	pointAt(t, home, fakeHindsight(t))
	busy := &fakeRename{banks: legacyBanks, pending: 1}
	withRename(t, busy)
	renameWaitApply, renamePoll = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { renameWaitApply = 2 * time.Minute })
	if code, out, errOut := run(t, "bank", "apply"); code != output.OK || !strings.Contains(out, "bank claude-memory") || !strings.Contains(out, "not renamed") {
		t.Errorf("busy: exit %d\n%s\n%s", code, out, errOut)
	}
}

// A run that stopped after the renames (a crash, a cancel) is finished by
// the next one: every old id gets its alias, the records and config move,
// and nothing is renamed twice.
func TestBankRenameFinishesARunThatStopped(t *testing.T) {
	home := legacyInstall(t)
	f := &fakeRename{banks: []string{"backsight", "backsight-docs", "backsight-control", "other"}}
	withRename(t, f)
	code, out, errOut := run(t, "bank", "rename")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	want := []string{"alias claude-memory→backsight", "alias claude-memory-docs→backsight-docs", "alias claude-memory-control→backsight-control"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls %q, want %q", f.calls, want)
	}
	d := pathsOf(home)
	if cfg, _ := config.Load(d.ConfigFile(), d, home); cfg.Server.Bank != "backsight" {
		t.Errorf("config bank %q", cfg.Server.Bank)
	}
	if cur, _, _ := retain.LoadCursor(d.CursorsDir(), "s1"); cur.Bank != "backsight" {
		t.Errorf("cursor %+v", cur)
	}
}

// When a bank moved but this machine's records could not follow, bank
// apply stops: sending the template to the old id would create an empty
// bank there, next to the renamed one.
func TestBankApplyStopsAfterAHalfDoneRename(t *testing.T) {
	home := legacyInstall(t)
	pointAt(t, home, fakeHindsight(t))
	_ = os.WriteFile(filepath.Join(pathsOf(home).CursorsDir(), "s2.json"), []byte("{not json"), 0o600)
	f := &fakeRename{banks: legacyBanks}
	withRename(t, f)
	code, out, errOut := run(t, "bank", "apply")
	if code == output.OK || strings.Contains(out, "created") || !strings.Contains(errOut, "this machine's records are not") {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !slices.Contains(f.calls, "rename claude-memory→backsight") {
		t.Errorf("calls %q", f.calls)
	}
}

// A send that began before the maintenance lock would save its cursor on
// the old bank, and its session would be sent again: the rename waits for
// it, and changes nothing if it does not end.
func TestBankRenameWaitsForARunningSend(t *testing.T) {
	home := legacyInstall(t)
	d := pathsOf(home)
	_ = os.MkdirAll(d.LocksDir(), 0o700)
	l, err := state.TryAcquire(filepath.Join(d.LocksDir(), "session-s1.lock"), "retain flush")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRename{banks: legacyBanks}
	withRename(t, f)
	renameWaitHand, renamePoll = 30*time.Millisecond, 5*time.Millisecond
	if code, _, errOut := run(t, "bank", "rename"); code != output.Blocked || !strings.Contains(errOut, "a send of session s1 is still running") {
		t.Errorf("exit %d %q", code, errOut)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "pending ") {
			t.Errorf("ran %q with a send running", c)
		}
	}
	_ = l.Release()
	if code, _, errOut := run(t, "bank", "rename"); code != output.OK {
		t.Errorf("after the send: exit %d %s", code, errOut)
	}
}

// The rename runs under the maintenance lock: another maintenance stops it
// before anything moves, update apply's own lets it run, and it leaves no
// lock of its own behind.
func TestBankRenameTakesTheMaintenanceLock(t *testing.T) {
	home := legacyInstall(t)
	d := pathsOf(home)
	f := &fakeRename{banks: legacyBanks}
	withRename(t, f)
	other, err := state.TryAcquire(d.MaintenanceLock(), "backup create")
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "bank", "rename"); code != output.Blocked || !strings.Contains(errOut, "maintenance in progress") || len(f.calls) != 0 {
		t.Errorf("under another maintenance: exit %d %q, calls %q", code, errOut, f.calls)
	}
	_ = other.Release()
	upd, err := state.TryAcquire(d.MaintenanceLock(), "update apply")
	if err != nil {
		t.Fatal(err)
	}
	owner := upd.Owner()
	updateLock = &owner
	t.Cleanup(func() { updateLock = nil; _ = upd.Release() })
	if code, _, errOut := run(t, "bank", "rename"); code != output.OK {
		t.Fatalf("under update apply's lock: exit %d %s", code, errOut)
	}
	if h := state.Inspect(d.MaintenanceLock()); h == nil || h.Owner.Command != "update apply" {
		t.Errorf("update apply's lock was not left as it was: %+v", h)
	}
	_ = upd.Release()
	raw, _ := os.ReadFile(d.ConfigFile())
	_ = os.WriteFile(d.ConfigFile(), []byte(strings.Replace(string(raw), `bank = "backsight"`, `bank = "claude-memory"`, 1)), 0o600)
	if code, _, errOut := run(t, "bank", "rename"); code != output.OK || state.Inspect(d.MaintenanceLock()) != nil {
		t.Errorf("by hand: exit %d %s, lock left: %v", code, errOut, state.Inspect(d.MaintenanceLock()) != nil)
	}
}

// The daily job finishes a rename the update left (the server was busy),
// right after its backup.
func TestMaintainRenamesALegacyBankAfterItsBackup(t *testing.T) {
	home := legacyInstall(t)
	pointAt(t, home, fakeHindsight(t))
	withBackupDocker(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	f := &fakeRename{banks: legacyBanks}
	withRename(t, f)
	code, out, errOut := run(t, "jobs", "maintain")
	if code != output.OK || !strings.Contains(out, "bank rename claude-memory → backsight") || !slices.Contains(f.calls, "rename claude-memory→backsight") {
		t.Fatalf("exit %d, calls %q\n%s\n%s", code, f.calls, out, errOut)
	}
	d := pathsOf(home)
	if cfg, _ := config.Load(d.ConfigFile(), d, home); cfg.Server.Bank != "backsight" {
		t.Errorf("config bank %q", cfg.Server.Bank)
	}
}
