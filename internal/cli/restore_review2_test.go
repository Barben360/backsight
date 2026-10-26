package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// A server upgrade's rollback puts the safety dump back, every bank with
// it: each other memory bank gets an epoch above its own, without a
// question (issue 285 review M3).
func TestServerUpgradeRollbackBumpsTheOtherBanks(t *testing.T) {
	_, dk, port, _ := upgradeSetup(t)
	ctx := context.Background()
	cl := clientOn(port)
	if err := retain.WriteEpoch(ctx, cl, "team-memory", retain.Epoch{Epoch: 3}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"team-memory", "notes"} {
		if err := cl.CreateBank(ctx, b, b, ""); err != nil {
			t.Fatal(err)
		}
	}
	dk.failImage, dk.migrateImage = newImage, newImage
	if code, out, errOut := run(t, "server", "upgrade", "--json"); code != output.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := bankEpoch(t, port, "team-memory"); got != 4 {
		t.Errorf("team-memory epoch %d, want 4", got)
	}
	if got := bankEpoch(t, port, "notes"); got != 1 {
		t.Errorf("notes epoch %d, want 1", got)
	}
}

// A server upgrade's rollback whose last start on the API key fails stops
// at the key step with the image it rolled back to: server start starts
// that image (this binary's pin would refuse the rolled-back server) and
// finishes; doctor names the rollback and keeps the safety archive as the
// fallback (issue 285 review M4).
func TestServerUpgradeRollbackKeyStepStartsTheRolledBackImage(t *testing.T) {
	home, dk, _, oldImage := upgradeSetup(t)
	sec, _ := config.LoadSecrets(secretsFile(home))
	dk.failImage, dk.migrateImage, dk.failKey = newImage, newImage, sec.APIKey
	code, _, errOut := run(t, "server", "upgrade")
	if code != output.Blocked || !strings.Contains(errOut, "run backsight server start") {
		t.Fatalf("exit %d %s", code, errOut)
	}
	d := pathsOf(home)
	m, err := state.ReadRestoreMarker(d)
	if err != nil || m == nil || m.Step != state.KeyStep || m.Image != oldImage || !m.RolledBack {
		t.Fatalf("marker %+v %v", m, err)
	}
	_, out, _ := run(t, "doctor")
	if !strings.Contains(out, "was rolled back: the server holds the safety archive") || !strings.Contains(out, m.Safety+" --no-safety") ||
		strings.Contains(out, "the archive restored is") {
		t.Errorf("doctor:\n%s", out)
	}
	dk.failKey = ""
	if code, out, errOut := run(t, "server", "start"); code != output.OK || !strings.Contains(out, "is finished") {
		t.Fatalf("server start: exit %d\n%s%s", code, out, errOut)
	}
	if got := deployedImage(t, home); got != oldImage {
		t.Errorf("server start deployed %s, want the rolled-back %s", got, oldImage)
	}
	if state.RestoreIncomplete(d) {
		t.Error("marker left")
	}
}

// banksDown makes the bank listing fail until the database is restored:
// restore reads the banks from the database then.
func banksDown(t *testing.T, home string, port int, dk *restoreDocker) {
	t.Helper()
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/default/banks" && dk.restored() == 0 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return true
		}
		return false
	}))
}

// With the API down, the banks come from the database, where no control
// bank can be proven backsight's: each other bank is asked about, even one
// whose control bank is backsight's (issue 285 review M2). With the answer
// the restore goes on; a control bank that turns out someone else's is a
// warning, not a rollback.
func TestRestoreFromTheDatabaseListAsksAboutEveryBank(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	ctx := context.Background()
	cl := clientOn(port)
	if err := retain.WriteEpoch(ctx, cl, "team-memory", retain.Epoch{Epoch: 3}); err != nil {
		t.Fatal(err)
	}
	if err := cl.CreateBank(ctx, "other-control", "someone else's", ""); err != nil {
		t.Fatal(err)
	}
	dk.sqlBanks = `["claude-memory", "claude-memory-control", "team-memory", "team-memory-control", "other", "other-control"]`
	banksDown(t, home, port, dk)
	code, out, errOut := run(t, "restore", archive, "--json")
	if code != output.NeedsUser || !strings.Contains(out, "bank(s) other, team-memory,") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	code, env, out, errOut := runRestoreJSON(t, archive, "--answer", "other_banks=revert")
	if code != output.OK {
		t.Fatalf("with the answer: exit %d\n%s%s", code, out, errOut)
	}
	if got := bankEpoch(t, port, "team-memory"); got != 4 {
		t.Errorf("team-memory epoch %d", got)
	}
	if w := strings.Join(env.Data.Warnings, "\n"); !strings.Contains(w, "bank other: bank other-control is not backsight's") || !strings.Contains(w, "read from the database") {
		t.Errorf("warnings %q", env.Data.Warnings)
	}
}

// A rollback with other banks gives each an epoch above the one it
// attempted; one it cannot write (someone else's control bank, found only
// then) is a warning, and the rollback still finishes on the key: exit 30
// (issue 285 review M2).
func TestRollbackWithOtherBanks(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	ctx := context.Background()
	cl := clientOn(port)
	if err := retain.WriteEpoch(ctx, cl, "team-memory", retain.Epoch{Epoch: 3}); err != nil {
		t.Fatal(err)
	}
	if err := cl.CreateBank(ctx, "other-control", "someone else's", ""); err != nil {
		t.Fatal(err)
	}
	dk.sqlBanks = `["claude-memory", "team-memory", "team-memory-control", "other", "other-control"]`
	banksDown(t, home, port, dk)
	dk.lose = 1
	code, env, out, errOut := runRestoreJSON(t, archive, "--answer", "other_banks=revert")
	if code != output.RolledBack || !env.Data.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := bankEpoch(t, port, "team-memory"); got != 4 {
		t.Errorf("team-memory epoch after the rollback %d, want 4", got)
	}
	if !strings.Contains(strings.Join(env.Data.Warnings, "\n"), "rollback: no restore epoch written in bank other") || state.RestoreIncomplete(pathsOf(home)) {
		t.Errorf("warnings %q, marker %v", env.Data.Warnings, state.RestoreIncomplete(pathsOf(home)))
	}
}

// A purge again that fails rolls back to the safety archive, which does
// not hold the document: exit 30.
func TestRestorePurgeAgainFailureRollsBack(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	seedTombstones(t, home, port)
	documentUnits["conversation:s8"] = 1
	docDeleteFails.Store(true)
	code, env, out, errOut := runRestoreJSON(t, archive)
	if code != output.RolledBack || !strings.Contains(stepsOf(env), "purge again failed, rollback done") {
		t.Fatalf("exit %d steps %s\n%s%s", code, stepsOf(env), out, errOut)
	}
	if dk.rows != 25 || !dk.running {
		t.Errorf("rows %d running %v", dk.rows, dk.running)
	}
}

// Every bank not proven backsight's is asked about (issue 285 review L1):
// someone else's control bank, of a listed bank or an orphan, and a docs
// bank without backsight's marker; a proven docs bank is not. Someone
// else's control bank gets no epoch; an unproven -docs bank is taken for a
// memory bank and gets one.
func TestRestoreAsksAboutEveryBankNotProvenBackSights(t *testing.T) {
	_, archive, _, port := restoreSetup(t)
	ctx := context.Background()
	cl := clientOn(port)
	for b, name := range map[string]string{"x": "x", "x-control": "someone else's", "y-control": "someone else's", "z-docs": "z docs", "team-docs": "backsight docs"} {
		if err := cl.CreateBank(ctx, b, name, ""); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ := run(t, "restore", archive, "--json")
	if code != output.NeedsUser {
		t.Fatalf("exit %d\n%s", code, out)
	}
	var env struct {
		Error struct {
			Questions []struct {
				Prompt string `json:"prompt"`
			} `json:"questions"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if len(env.Error.Questions) != 1 || !strings.Contains(env.Error.Questions[0].Prompt, "bank(s) x, x-control, y-control, z-docs,") {
		t.Fatalf("question %+v\n%s", env.Error.Questions, out)
	}
	bankCalls = nil
	code, env2, out, errOut := runRestoreJSON(t, archive, "--answer", "other_banks=revert")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, c := range bankCalls {
		if strings.HasPrefix(c, "POST x-control") || strings.HasPrefix(c, "POST y-control") || strings.Contains(c, "-control-control") {
			t.Errorf("wrote into someone else's control bank: %q", c)
		}
	}
	if got := bankEpoch(t, port, "z-docs"); got != 1 {
		t.Errorf("z-docs epoch %d", got)
	}
	if !strings.Contains(strings.Join(env2.Data.Warnings, "\n"), "bank x: its control bank x-control is not backsight's") {
		t.Errorf("warnings %q", env2.Data.Warnings)
	}
}

// The other banks' purges are written back into their control banks: the
// restored control bank is the archive's, without them.
func TestRestoreWritesBackTheOtherBanksPurges(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	ctx := context.Background()
	cl := clientOn(port)
	purged := state.Tombstone{Bank: "team-memory", Document: "conversation:t1", Session: "t1", LastUUID: "x1", PurgedAt: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
	if _, err := retain.WriteTombstones(ctx, cl, "team-memory", []state.Tombstone{purged}); err != nil {
		t.Fatal(err)
	}
	if err := cl.CreateBank(ctx, "team-memory", "team-memory", ""); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	written := false
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if dk.restored() == 0 {
			return false
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/team-memory-control/memories") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			written = written || strings.Contains(string(body), retain.TombstonesDocument)
		}
		// the archive's control bank: no tombstones until restore writes them
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/team-memory-control/documents/"+retain.TombstonesDocument) && !written {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return false
	}))
	if code, _, out, errOut := runRestoreJSON(t, archive); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !written {
		t.Error("team-memory's purges were not written back")
	}
}

// server start takes over a dead owner's maintenance lock, but a restore
// that did not reach its key step still refuses it.
func TestServerStartWithADeadLockStillRefusesAnUnfinishedRestore(t *testing.T) {
	home := devInstall(t, true)
	f := withCompose(t)
	pointAt(t, home, fakeHindsight(t))
	writeDeadMaintenanceLock(t, home)
	if err := state.WriteRestoreMarker(pathsOf(home), state.RestoreMarker{Command: "restore", Safety: "/b/s.age", Archive: "/b/a.age"}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run(t, "server", "start"); code != output.Blocked || !strings.Contains(errOut, "a restore did not finish") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	for _, c := range f.calls {
		if slices.Contains(c, "up") {
			t.Errorf("started anyway: %q", c)
		}
	}
}

// restore run by update apply's rollback as a child: the lock is its
// parent's, named in BACKSIGHT_UPDATE_LOCK; it does not ask about the
// other banks.
func TestRestoreAsAChildOfUpdateApplyDoesNotAsk(t *testing.T) {
	home, archive, _, port := restoreSetup(t)
	if err := clientOn(port).CreateBank(context.Background(), "notes", "notes", ""); err != nil {
		t.Fatal(err)
	}
	parent := state.Owner{PID: os.Getppid(), Command: "update apply", Since: time.Now().UTC().Truncate(time.Second)}
	raw, _ := json.Marshal(parent)
	if err := os.WriteFile(pathsOf(home).MaintenanceLock(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(updateLockEnv, lockToken(parent))
	if code, out, errOut := run(t, "restore", archive, "--no-safety", "--non-interactive"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := bankEpoch(t, port, "notes"); got != 1 {
		t.Errorf("notes epoch %d", got)
	}
}

// Another bank's next epoch is above this client's record of it too, not
// only the server's (issue 285 review L2).
func TestRestoreOtherBankEpochIsAboveThisClientsToo(t *testing.T) {
	home, archive, _, port := restoreSetup(t)
	ctx := context.Background()
	if err := retain.WriteEpoch(ctx, clientOn(port), "team-memory", retain.Epoch{Epoch: 3}); err != nil {
		t.Fatal(err)
	}
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: "team-memory", Epoch: 7}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run(t, "restore", archive); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := bankEpoch(t, port, "team-memory"); got != 8 {
		t.Errorf("team-memory epoch %d, want 8", got)
	}
}
