package cli

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

func bankEpoch(t *testing.T, port int, bank string) int {
	t.Helper()
	e, err := retain.ReadEpoch(context.Background(), clientOn(port), bank)
	if err != nil {
		t.Fatal(err)
	}
	return e.Epoch
}

// The archive's database holds every bank of the server: each memory bank
// with a backsight control bank gets a new epoch above its own, and the
// purged documents the archive brings back are deleted there too. A docs
// bank and the control banks themselves get none (M5 review rules, issue
// 285).
func TestRestoreBumpsTheEpochOfEveryMemoryBank(t *testing.T) {
	_, archive, _, port := restoreSetup(t)
	cl := clientOn(port)
	ctx := context.Background()
	if err := retain.WriteEpoch(ctx, cl, "team-memory", retain.Epoch{Epoch: 3}); err != nil {
		t.Fatal(err)
	}
	purged := state.Tombstone{Bank: "team-memory", Document: "conversation:t1", Session: "t1", Project: "infra", LastUUID: "x1",
		PurgedAt: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
	if _, err := retain.WriteTombstones(ctx, cl, "team-memory", []state.Tombstone{purged}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"claude-memory", "claude-memory-docs", "team-memory"} {
		if err := cl.CreateBank(ctx, b, b, ""); err != nil {
			t.Fatal(err)
		}
	}
	documentUnits["conversation:t1"] = 2 // the archive brings it back
	bankCalls, curation = nil, nil
	code, env, out, errOut := runRestoreJSON(t, archive)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := bankEpoch(t, port, "claude-memory"); got != 1 {
		t.Errorf("own epoch %d", got)
	}
	if got := bankEpoch(t, port, "team-memory"); got != 4 {
		t.Errorf("team-memory epoch %d, want 4", got)
	}
	for _, c := range bankCalls {
		if strings.Contains(c, "claude-memory-docs-control") || strings.Contains(c, "-control-control") {
			t.Errorf("an epoch for a docs or control bank: %q", c)
		}
	}
	if !strings.Contains(strings.Join(curation, "\n"), "DELETE /v1/default/banks/team-memory/documents/conversation%3At1") {
		t.Errorf("the purged document of team-memory is back: %q", curation)
	}
	if !strings.Contains(stepsOf(env), "epoch done") || !strings.Contains(out, "bank team-memory at 4") || !strings.Contains(out, "team-memory/conversation:t1") {
		t.Errorf("steps %s\n%s", stepsOf(env), out)
	}
}

// A memory bank with no control bank may not be backsight's: restore asks
// before anything changes (exit 10), the plan says it will ask, and
// --answer other_banks=revert goes on and gives it an epoch too.
func TestRestoreAsksBeforeRevertingABankWithoutControlBank(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	if err := clientOn(port).CreateBank(context.Background(), "notes", "notes", ""); err != nil {
		t.Fatal(err)
	}
	dk.calls = nil
	before := backupsIn(home)
	code, out, errOut := run(t, "restore", archive, "--json")
	if code != output.NeedsUser || !strings.Contains(out, `"other_banks"`) || !strings.Contains(out, "notes") || !strings.Contains(out, "--answer other_banks=revert") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if called(dk, "stop") || called(dk, "pg_dump") || len(backupsIn(home)) != len(before) || state.RestoreIncomplete(pathsOf(home)) {
		t.Errorf("acted before the answer: %q", dk.calls)
	}
	if e := epochOf(t, port); e.Epoch != 0 {
		t.Errorf("epoch %+v", e)
	}
	if code, out, _ := run(t, "restore", archive, "--plan"); code != output.OK || !strings.Contains(out, "bank(s) notes") || !strings.Contains(out, "other_banks=revert") {
		t.Errorf("plan: exit %d\n%s", code, out)
	}
	if code, _, errOut := run(t, "restore", archive, "--answer", "other_banks=keep"); code != output.Usage || !strings.Contains(errOut, "revert") {
		t.Errorf("a wrong answer: exit %d %s", code, errOut)
	}
	if code, out, errOut := run(t, "restore", archive, "--answer", "other_banks=revert"); code != output.OK {
		t.Fatalf("with the answer: exit %d\n%s%s", code, out, errOut)
	}
	if got := bankEpoch(t, port, "notes"); got != 1 {
		t.Errorf("notes epoch %d", got)
	}
}

// The restore marker goes only once the server runs on its API key again:
// when that last start fails, the marker stays at its key step, doctor
// says to run server start, and server start finishes the restore.
func TestRestoreMarkerStaysUntilTheKeyStep(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	sec, _ := config.LoadSecrets(secretsFile(home))
	dk.failKey = sec.APIKey
	code, _, errOut := run(t, "restore", archive)
	if code != output.Blocked || !strings.Contains(errOut, "run backsight server start") {
		t.Fatalf("exit %d %s", code, errOut)
	}
	d := pathsOf(home)
	m, err := state.ReadRestoreMarker(d)
	if err != nil || m == nil || m.Step != state.KeyStep {
		t.Fatalf("marker %+v %v", m, err)
	}
	if e := epochOf(t, port); e.Epoch != 1 {
		t.Errorf("epoch %+v", e)
	}
	if _, out, _ := run(t, "doctor"); !strings.Contains(out, "backsight server start (it finishes the restore)") {
		t.Errorf("doctor:\n%s", out)
	}
	// Another command that changes the server still refuses.
	if code, _, errOut := run(t, "server", "stop"); code != output.Blocked || !strings.Contains(errOut, "restore") {
		t.Errorf("server stop: exit %d %s", code, errOut)
	}
	dk.failKey = ""
	code, out, errOut := run(t, "server", "start")
	if code != output.OK || !strings.Contains(out, "is finished") {
		t.Fatalf("server start: exit %d\n%s%s", code, out, errOut)
	}
	if state.RestoreIncomplete(d) {
		t.Error("the marker is still there")
	}
	if k := dk.keys[len(dk.keys)-1]; k != sec.APIKey {
		t.Errorf("the server runs on %q", k)
	}
}

// A rollback whose last start on the API key fails keeps the marker at
// its key step too.
func TestRollbackMarkerStaysUntilTheKeyStep(t *testing.T) {
	home, archive, dk, _ := restoreSetup(t)
	sec, _ := config.LoadSecrets(secretsFile(home))
	dk.lose, dk.failKey = 1, sec.APIKey
	code, _, errOut := run(t, "restore", archive)
	if code != output.Blocked || !strings.Contains(errOut, "rolled back") || !strings.Contains(errOut, "run backsight server start") {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if m, err := state.ReadRestoreMarker(pathsOf(home)); err != nil || m == nil || m.Step != state.KeyStep {
		t.Fatalf("marker %+v %v", m, err)
	}
}

// server start takes over a maintenance lock whose owner is gone, and
// says so; a live owner's lock still refuses it.
func TestServerStartTakesOverADeadOwnersMaintenanceLock(t *testing.T) {
	home := devInstall(t, true)
	f := withCompose(t)
	pointAt(t, home, fakeHindsight(t))
	writeDeadMaintenanceLock(t, home)
	d := pathsOf(home)
	code, out, errOut := run(t, "server", "start")
	if code != output.OK || !strings.Contains(out, `removed a maintenance lock left by "restore"`) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(d.MaintenanceLock()); err == nil {
		t.Error("the dead owner's lock is still there")
	}
	if len(f.calls) == 0 {
		t.Error("the server was not started")
	}
	live := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"command":"backup create","since":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(d.MaintenanceLock(), []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "server", "start"); code != output.Blocked || !strings.Contains(errOut, "maintenance in progress") {
		t.Errorf("a live lock: exit %d %s", code, errOut)
	}
	if raw, _ := os.ReadFile(d.MaintenanceLock()); string(raw) != live {
		t.Error("a live owner's lock was removed")
	}
}

// update apply's rollback restores the upgrade's safety archive through
// the previous binary, under update apply's lock, with nobody to answer:
// it does not ask.
func TestRestoreUnderUpdateApplyDoesNotAsk(t *testing.T) {
	home, archive, _, port := restoreSetup(t)
	if err := clientOn(port).CreateBank(context.Background(), "notes", "notes", ""); err != nil {
		t.Fatal(err)
	}
	d := pathsOf(home)
	owner := state.Owner{PID: os.Getpid(), Command: "update apply", Since: time.Now().UTC().Truncate(time.Second)}
	raw, _ := json.Marshal(owner)
	if err := os.WriteFile(d.MaintenanceLock(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	updateLock = &owner
	t.Cleanup(func() { updateLock = nil })
	if code, out, errOut := run(t, "restore", archive, "--no-safety", "--non-interactive"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := bankEpoch(t, port, "notes"); got != 1 {
		t.Errorf("notes epoch %d", got)
	}
}
