package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// The image a newer binary pins in these tests.
const newImage = "ghcr.io/vectorize-io/hindsight:1.2.0@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func pinImage(t *testing.T, image string) {
	t.Helper()
	old := server.HindsightImage
	server.HindsightImage = image
	t.Cleanup(func() { server.HindsightImage = old })
}

func deployedImage(t *testing.T, home string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(composeDir(home), "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return server.ServiceImage(raw, "hindsight")
}

// upgradeSetup is restore's fake server and Docker with the server
// deployed on the current pin, then a binary pinning newImage.
func upgradeSetup(t *testing.T) (home string, dk *restoreDocker, port int, oldImage string) {
	t.Helper()
	home, _, dk, port = restoreSetup(t)
	oldImage = server.HindsightImage
	if code, out, _ := run(t, "server", "render"); code != output.OK {
		t.Fatalf("render: exit %d\n%s", code, out)
	}
	pinImage(t, newImage)
	dk.calls, dk.unlocked = nil, nil
	return home, dk, port, oldImage
}

type upgradeEnvelope struct {
	OK    bool          `json:"ok"`
	Error *output.Error `json:"error"`
	Data  struct {
		Runtime    string `json:"runtime"`
		Current    string `json:"current"`
		Target     string `json:"target"`
		Upgrade    bool   `json:"upgrade"`
		Safety     string `json:"safety_archive"`
		RolledBack bool   `json:"rolled_back"`
		Epoch      *struct {
			Epoch int `json:"epoch"`
		} `json:"epoch"`
		Steps []step `json:"steps"`
	} `json:"data"`
}

func decodeUpgrade(t *testing.T, out string) upgradeEnvelope {
	t.Helper()
	var env upgradeEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not one JSON envelope: %v\n%s", err, out)
	}
	return env
}

func TestServerUpgradeDeploysThePinnedImage(t *testing.T) {
	home, dk, _, oldImage := upgradeSetup(t)
	code, out, errOut := run(t, "server", "upgrade", "--json")
	env := decodeUpgrade(t, out)
	if code != output.OK || !env.OK || !env.Data.Upgrade || env.Data.Current != oldImage || env.Data.Target != newImage || env.Data.Runtime != "docker" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := deployedImage(t, home); got != newImage {
		t.Errorf("deployed %s", got)
	}
	if len(dk.images) == 0 || dk.images[len(dk.images)-1] != newImage || !dk.running {
		t.Errorf("ups %v running %v", dk.images, dk.running)
	}
	// The image was pulled before anything was dumped, stopped or started.
	pulled := -1
	for i, c := range dk.calls {
		if c == "pull "+newImage {
			pulled = i
		}
		if (strings.Contains(c, "pg_dump") || strings.Contains(c, " up ") || strings.Contains(c, " stop")) && (pulled < 0 || pulled > i) {
			t.Errorf("%q before the pull", c)
		}
	}
	if pulled < 0 {
		t.Errorf("never pulled: %q", dk.calls)
	}
	// The safety archive records the image its data was taken with.
	side, err := os.ReadFile(env.Data.Safety + ".json")
	var m backup.Manifest
	if err != nil || json.Unmarshal(side, &m) != nil || m.HindsightImage != oldImage || m.Counts["memory_units"] != 25 {
		t.Errorf("safety manifest %v %s", err, side)
	}
	// Verified in a throwaway database, and the data left alone.
	verified := false
	for _, c := range dk.calls {
		verified = verified || strings.HasPrefix(c, "run -d --network none")
	}
	if !verified || len(dk.restores) != 0 || dk.rows != 25 {
		t.Errorf("verified %v, restores %v, rows %d", verified, dk.restores, dk.rows)
	}
	if len(dk.unlocked) > 0 {
		t.Errorf("docker calls without the maintenance lock: %q", dk.unlocked)
	}
	d := pathsOf(home)
	for _, p := range []string{d.MaintenanceLock(), d.RestoreMarker(), filepath.Join(d.LocksDir(), "server.lock")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s left behind", p)
		}
	}
	// Now up to date: nothing runs.
	dk.calls = nil
	code, out, _ = run(t, "server", "upgrade", "--json")
	if env := decodeUpgrade(t, out); code != output.OK || env.Data.Upgrade || len(dk.calls) != 0 {
		t.Errorf("again: exit %d, calls %q\n%s", code, dk.calls, out)
	}
}

func TestServerUpgradePlanChangesNothing(t *testing.T) {
	home, dk, _, oldImage := upgradeSetup(t)
	before := snapshot(t, pathsOf(home))
	code, out, _ := run(t, "server", "upgrade", "--plan", "--json")
	env := decodeUpgrade(t, out)
	if code != output.OK || !env.Data.Upgrade || env.Data.Current != oldImage || env.Data.Target != newImage {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if len(dk.calls) != 0 || deployedImage(t, home) != oldImage || snapshot(t, pathsOf(home)) != before || len(backupsIn(home)) != 1 {
		t.Errorf("the plan acted: calls %q, backups %v", dk.calls, backupsIn(home))
	}
}

// A pin that cannot be pulled changes nothing: no backup, no stop.
func TestServerUpgradeBadPinChangesNothing(t *testing.T) {
	home, dk, _, oldImage := upgradeSetup(t)
	dk.failPull = true
	code, out, _ := run(t, "server", "upgrade", "--json")
	env := decodeUpgrade(t, out)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "nothing was changed") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if deployedImage(t, home) != oldImage || len(backupsIn(home)) != 1 || len(dk.images) != 0 || !dk.running {
		t.Errorf("changed: image %s, backups %v, ups %v", deployedImage(t, home), backupsIn(home), dk.images)
	}
	if state.RestoreIncomplete(pathsOf(home)) {
		t.Error("marker left")
	}
}

// A new version that does not start: the safety dump goes back into the
// previous version, with a new epoch (exit 30).
func TestServerUpgradeFailedStartRestoresThePreviousVersion(t *testing.T) {
	home, dk, port, oldImage := upgradeSetup(t)
	dk.failImage = newImage
	dk.rows = 25
	code, out, errOut := run(t, "server", "upgrade", "--json")
	env := decodeUpgrade(t, out)
	if code != output.RolledBack || !env.Data.RolledBack || env.Error == nil || !strings.Contains(env.Error.Message, "server upgrade failed") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := deployedImage(t, home); got != oldImage {
		t.Errorf("deployed %s, want the previous %s", got, oldImage)
	}
	if len(dk.restores) != 1 || dk.restores[0] != 25 || dk.rows != 25 || !dk.running || dk.images[len(dk.images)-1] != oldImage {
		t.Errorf("restores %v rows %d running %v ups %v", dk.restores, dk.rows, dk.running, dk.images)
	}
	if e := epochOf(t, port); e.Epoch != 1 || env.Data.Epoch == nil || env.Data.Epoch.Epoch != 1 {
		t.Errorf("rollback epoch %+v, reported %+v", e, env.Data.Epoch)
	}
	if state.RestoreIncomplete(pathsOf(home)) {
		t.Error("marker left after a finished rollback")
	}
}

// A failing smoke (the recall) rolls back the same way.
func TestServerUpgradeFailedSmokeRollsBack(t *testing.T) {
	home, dk, port, oldImage := upgradeSetup(t)
	failed := 0
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/recall") && failed == 0 {
			failed++
			http.Error(w, "boom", http.StatusInternalServerError)
			return true
		}
		return false
	}))
	pinImage(t, oldImage)
	if code, out, _ := run(t, "server", "render"); code != output.OK { // the proxy's port, on the old pin
		t.Fatalf("render: %d %s", code, out)
	}
	pinImage(t, newImage)
	code, out, errOut := run(t, "server", "upgrade", "--json")
	if code != output.RolledBack || !strings.Contains(out, "smoke") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if deployedImage(t, home) != oldImage || len(dk.restores) != 1 || !dk.running {
		t.Errorf("image %s restores %v", deployedImage(t, home), dk.restores)
	}
}

// A rollback that fails exits 20 and keeps the marker naming the safety
// archive.
func TestServerUpgradeFailedRollbackKeepsTheMarker(t *testing.T) {
	home, dk, _, _ := upgradeSetup(t)
	dk.failImage, dk.failLoad = newImage, 5
	code, out, _ := run(t, "server", "upgrade", "--json")
	env := decodeUpgrade(t, out)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "--no-safety") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	m, err := state.ReadRestoreMarker(pathsOf(home))
	if err != nil || m == nil || m.Safety != env.Data.Safety || m.Command != "server upgrade" {
		t.Errorf("marker %+v %v", m, err)
	}
}

func TestServerUpgradeLock(t *testing.T) {
	home, dk, _, oldImage := upgradeSetup(t)
	d := pathsOf(home)
	// Someone else's maintenance: refused, nothing changed.
	raw, _ := json.Marshal(state.Owner{PID: os.Getppid(), Command: "backup create"})
	_ = os.WriteFile(d.MaintenanceLock(), raw, 0o600)
	if code, out, _ := run(t, "server", "upgrade", "--json"); code != output.Blocked || !strings.Contains(out, "maintenance in progress") || len(dk.calls) != 0 {
		t.Fatalf("exit %d, calls %q\n%s", code, dk.calls, out)
	}
	// update apply's (this process): the upgrade runs under it and leaves
	// it held.
	raw, _ = json.Marshal(state.Owner{PID: os.Getpid(), Command: "update apply"})
	_ = os.WriteFile(d.MaintenanceLock(), raw, 0o600)
	if code, out, _ := run(t, "server", "upgrade", "--json"); code != output.OK {
		t.Fatalf("under update apply's lock: exit %d\n%s", code, out)
	}
	if h := state.Inspect(d.MaintenanceLock()); h == nil || h.Owner.Command != "update apply" {
		t.Errorf("update apply's lock was released: %+v", h)
	}
	if deployedImage(t, home) == oldImage || len(dk.unlocked) > 0 {
		t.Errorf("image %s, unlocked %q", deployedImage(t, home), dk.unlocked)
	}
}

// A binary pinning an older version than the deployed one does not
// downgrade the server.
func TestServerUpgradeNeverDowngrades(t *testing.T) {
	home, dk, _, _ := upgradeSetup(t)
	redeploy(t, home, newImage) // as a newer binary's upgrade left it
	pinImage(t, olderImage)
	dk.calls = nil
	if code, out, _ := run(t, "server", "upgrade", "--json"); code != output.Blocked || !strings.Contains(out, "newer than this binary") || len(dk.calls) != 0 {
		t.Errorf("exit %d calls %q\n%s", code, dk.calls, out)
	}
	if deployedImage(t, home) != newImage {
		t.Error("downgraded")
	}
}
