package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/config"
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

// A passed upgrade prunes the backups (its safety archive among them)
// with the normal retention: an auto-apply a day must not pile them up. An
// archive younger than 24 h stays whatever the retention: the backup taken
// the same morning survives the upgrade's safety archive.
func TestServerUpgradePrunesWithTheBackupRetention(t *testing.T) {
	for name, c := range map[string]struct {
		at       time.Time // the upgrade; the first backup is 2026-10-25T04:00Z
		keepsOld bool
	}{
		"same day":  {time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), true},
		"next day":  {time.Date(2026, 10, 26, 3, 59, 0, 0, time.UTC), true},
		"past 24 h": {time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), false},
	} {
		t.Run(name, func(t *testing.T) {
			home, _, _, _ := upgradeSetup(t)
			configure(t, home, "[backup]\nkeep_daily = 1\nkeep_weekly = 1")
			withClock(t, c.at)
			before := backupsIn(home)
			code, out, _ := run(t, "server", "upgrade", "--json")
			env := decodeUpgrade(t, out)
			if code != output.OK || env.Data.Safety == "" {
				t.Fatalf("exit %d\n%s", code, out)
			}
			after := backupsIn(home)
			want := []string{filepath.Base(env.Data.Safety)}
			if c.keepsOld {
				want = append(before, want...)
			}
			if len(before) != 1 || strings.Join(after, " ") != strings.Join(want, " ") || strings.Contains(out, before[0]) == c.keepsOld {
				t.Errorf("backups %v → %v, want %v\n%s", before, after, want, out)
			}
		})
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

// The gates before the deploy: a safety backup that does not verify, or a
// restore epoch that cannot be read (a rollback must go above it), refuse
// with nothing deployed and no upgrade marked in progress.
func TestServerUpgradeGatesChangeNothing(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(t *testing.T, home string, dk *restoreDocker, port int)
		want  string
	}{
		"safety backup does not verify": {func(_ *testing.T, _ string, dk *restoreDocker, _ int) { dk.badVerify = true }, "does not verify"},
		"epoch unreadable": {func(t *testing.T, home string, dk *restoreDocker, port int) {
			pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodGet && r.URL.Path == epochDocPath {
					w.WriteHeader(http.StatusInternalServerError)
					return true
				}
				return false
			}))
			dk.sqlFails = true
		}, "restore epoch"},
	} {
		t.Run(name, func(t *testing.T) {
			home, dk, port, oldImage := upgradeSetup(t)
			c.setup(t, home, dk, port)
			code, out, _ := run(t, "server", "upgrade", "--json")
			env := decodeUpgrade(t, out)
			if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, c.want) || !strings.Contains(env.Error.Message, "nothing was changed") {
				t.Fatalf("exit %d, want %q\n%s", code, c.want, out)
			}
			if deployedImage(t, home) != oldImage || len(dk.images) != 0 || len(dk.restores) != 0 || !dk.running || called(dk, " stop") {
				t.Errorf("acted: image %s ups %v restores %v calls %q", deployedImage(t, home), dk.images, dk.restores, dk.calls)
			}
			if state.RestoreIncomplete(pathsOf(home)) {
				t.Error("upgrade marked in progress")
			}
		})
	}
}

// A new version that does not start: the safety dump goes back into the
// previous version, with a new epoch (exit 30).
func TestServerUpgradeFailedStartRestoresThePreviousVersion(t *testing.T) {
	home, dk, port, oldImage := upgradeSetup(t)
	dk.failImage, dk.migrateImage = newImage, newImage
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
	// The proxy's port in the server's files, on the old pin.
	pinImage(t, oldImage)
	if code, out, _ := run(t, "server", "render"); code != output.OK {
		t.Fatalf("render: %d %s", code, out)
	}
	pinImage(t, newImage)
	dk.migrateImage = newImage // the new version starts and migrates the data
	code, out, errOut := run(t, "server", "upgrade", "--json")
	env := decodeUpgrade(t, out)
	if code != output.RolledBack || !strings.Contains(out, "smoke") || !env.Data.RolledBack {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if deployedImage(t, home) != oldImage || !dk.running || dk.rows != 25 || !slices.Equal(dk.restores, []int{25}) {
		t.Errorf("image %s rows %d restores %v", deployedImage(t, home), dk.rows, dk.restores)
	}
	if e := epochOf(t, port); e.Epoch != 1 || env.Data.Epoch == nil || env.Data.Epoch.Epoch != 1 {
		t.Errorf("rollback epoch %+v, reported %+v", e, env.Data.Epoch)
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
	since := time.Now().UTC().Truncate(time.Second)
	raw, _ := json.Marshal(state.Owner{PID: liveProcess(t), Command: "backup create", Since: since})
	_ = os.WriteFile(d.MaintenanceLock(), raw, 0o600)
	if code, out, _ := run(t, "server", "upgrade", "--json"); code != output.Blocked || !strings.Contains(out, "maintenance in progress") || len(dk.calls) != 0 {
		t.Fatalf("exit %d, calls %q\n%s", code, dk.calls, out)
	}
	// A lock that says "update apply" with this process's PID, but that
	// this process did not take (a dead update's, its PID reused): refused.
	owner := state.Owner{PID: os.Getpid(), Command: "update apply", Since: since}
	raw, _ = json.Marshal(owner)
	_ = os.WriteFile(d.MaintenanceLock(), raw, 0o600)
	if code, out, _ := run(t, "server", "upgrade", "--json"); code != output.Blocked || !strings.Contains(out, "maintenance in progress") || len(dk.calls) != 0 {
		t.Fatalf("a lock this process did not take: exit %d, calls %q\n%s", code, dk.calls, out)
	}
	// update apply's (this process took it): the upgrade runs under it and
	// leaves it held.
	updateLock = &owner
	t.Cleanup(func() { updateLock = nil })
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

// The native versions these tests deploy: the server runs nativeOld, this
// binary pins its lock's version (the same Hindsight release, another
// lock: --allow-unordered), and an older venv lies beside them.
const (
	nativeOld   = "1.1.0-aaaaaaaaaaaa"
	nativeOlder = "1.0.9-cccccccccccc"
)

// venvTools is the native tools with uv building a venv, recording which
// version the server unit ran meanwhile; failPip fails the locked install.
type venvTools struct {
	*nativeDBTools
	n         server.Native // with its UnitDir
	failPip   bool
	builtWhen []string // "<venv> while <deployed>", one per uv venv
}

func (v *venvTools) Run(ctx context.Context, env []string, stdin io.Reader, path string, args ...string) ([]byte, error) {
	if filepath.Base(path) != "uv" || len(args) == 0 {
		return v.nativeDBTools.Run(ctx, env, stdin, path, args...)
	}
	switch args[0] {
	case "venv":
		dir := args[len(args)-1]
		deployed, _ := v.n.Deployed()
		v.builtWhen = append(v.builtWhen, filepath.Base(dir)+" while "+deployed)
		return nil, os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	case "pip":
		if v.failPip {
			return nil, errors.New("uv pip install: hash mismatch")
		}
	}
	return nil, nil
}

// restartWatch is the user manager recording, at each restart of the
// server unit, which venv the unit named.
type restartWatch struct {
	*nativeSysFake
	n        server.Native
	restarts []string
}

func (s *restartWatch) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemctl" && slices.Contains(args, "restart") && slices.Contains(args, s.n.ServerUnit()) {
		deployed, _ := s.n.Deployed()
		s.restarts = append(s.restarts, deployed)
	}
	return s.nativeSysFake.Run(ctx, name, args...)
}

type nativeUpgradeHost struct {
	home  string
	n     server.Native // the runtime as the commands build it
	tools *venvTools
	sys   *restartWatch
}

// nativeUpgradeSetup is a native install running nativeOld, its venv and
// nativeOlder's installed, uv and Python installed: an upgrade fetches the
// pinned venv only.
func nativeUpgradeSetup(t *testing.T) nativeUpgradeHost {
	t.Helper()
	home, _, db, sys, _ := nativeHost(t)
	d := pathsOf(home)
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatal(err)
	}
	n := runtimeFor(cfg).(server.Native)
	h := nativeUpgradeHost{home: home, n: n, tools: &venvTools{nativeDBTools: db, n: n}, sys: &restartWatch{nativeSysFake: sys, n: n}}
	oldSys, oldTools := nativeSys, nativeTools
	nativeSys, nativeTools = h.sys, h.tools
	t.Cleanup(func() { nativeSys, nativeTools = oldSys, oldTools })
	n.Sys, n.Tools = h.sys, h.tools
	h.n = n
	for _, p := range []string{n.UV(), n.PythonBin()} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{nativeOld, nativeOlder} {
		completeVenv(t, n, v)
	}
	if err := os.RemoveAll(n.VenvDir(n.Version())); err != nil { // the pin's: the upgrade fetches it
		t.Fatal(err)
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil {
		t.Fatal(err)
	}
	rt := n.WithVersion(nativeOld)
	files, err := renderOn(rt, sec, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Write(files); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := n.Deployed(); err != nil || got != nativeOld {
		t.Fatalf("deployed %q %v", got, err)
	}
	if n.Version() == nativeOld || n.Version() == nativeOlder {
		t.Fatalf("the pin %s is one of the test's versions", n.Version())
	}
	h.sys.calls, h.sys.restarts, h.tools.calls = nil, nil, nil
	return h
}

func (h nativeUpgradeHost) deployed(t *testing.T) string {
	t.Helper()
	v, err := h.n.Deployed()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (h nativeUpgradeHost) venvs(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(h.n.Root(), "venv"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// The new venv is installed beside the running one while the unit still
// names the old, then the unit is pointed at it and the server restarted;
// venvs older than the previous one are removed.
func TestNativeUpgradeInstallsBesideAndSwitches(t *testing.T) {
	h := nativeUpgradeSetup(t)
	pin := h.n.Version()
	code, out, errOut := run(t, "server", "upgrade", "--allow-unordered", "--json")
	env := decodeUpgrade(t, out)
	if code != output.OK || !env.OK || env.Data.Runtime != "native" || env.Data.Current != nativeOld || env.Data.Target != pin {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if want := []string{pin + " while " + nativeOld}; !slices.Equal(h.tools.builtWhen, want) {
		t.Errorf("venvs built %q, want %q (beside the running one)", h.tools.builtWhen, want)
	}
	if got := h.deployed(t); got != pin {
		t.Errorf("the unit runs %s, want %s", got, pin)
	}
	if !slices.Equal(h.sys.restarts, []string{pin}) {
		t.Errorf("server restarts on %q, want one on %s", h.sys.restarts, pin)
	}
	if want := sorted([]string{nativeOld, pin}); !slices.Equal(h.venvs(t), want) {
		t.Errorf("venvs %q, want %q (%s pruned)", h.venvs(t), want, nativeOlder)
	}
	// The step names what native does; its safety archive is native's.
	i := slices.IndexFunc(env.Data.Steps, func(s step) bool { return s.Name == "install" })
	if i < 0 || env.Data.Steps[i].Status != "done" || slices.ContainsFunc(env.Data.Steps, func(s step) bool { return s.Name == "pull" }) {
		t.Errorf("steps %+v", env.Data.Steps)
	}
	if m, _ := sidecar(t, env.Data.Safety); m.Runtime != "native" || m.HindsightVersion != "1.1.0" || m.Counts["memory_units"] != 10 {
		t.Errorf("safety manifest %+v", m)
	}
	if state.RestoreIncomplete(pathsOf(h.home)) {
		t.Error("marker left")
	}
}

// A new version whose smoke fails: the unit is pointed back at the old
// venv and the server restarted on it, the safety dump restored (a new
// epoch, exit 30 as on Docker); the new venv and the safety archive stay,
// nothing is pruned.
func TestNativeUpgradeRollsBackToTheOldVenv(t *testing.T) {
	h := nativeUpgradeSetup(t)
	pin := h.n.Version()
	recallStatus.Store(500)
	t.Cleanup(func() { recallStatus.Store(0) })
	before := backupsIn(h.home)
	code, out, errOut := run(t, "server", "upgrade", "--allow-unordered", "--json")
	env := decodeUpgrade(t, out)
	if code != output.RolledBack || !env.Data.RolledBack || env.Error == nil || !strings.Contains(env.Error.Message, "smoke") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := h.deployed(t); got != nativeOld {
		t.Errorf("the unit runs %s, want %s again", got, nativeOld)
	}
	if n := len(h.sys.restarts); n < 2 || h.sys.restarts[0] != pin || h.sys.restarts[n-1] != nativeOld {
		t.Errorf("server restarts on %q: want %s, then %s last", h.sys.restarts, pin, nativeOld)
	}
	if want := sorted([]string{nativeOlder, nativeOld, pin}); !slices.Equal(h.venvs(t), want) {
		t.Errorf("venvs %q, want %q (nothing pruned)", h.venvs(t), want)
	}
	if !slices.Equal(h.tools.restores, []int{10}) {
		t.Errorf("restores %v: the safety dump goes back", h.tools.restores)
	}
	if env.Data.Epoch == nil || env.Data.Epoch.Epoch != 1 {
		t.Errorf("rollback epoch %+v", env.Data.Epoch)
	}
	if _, err := os.Stat(env.Data.Safety); err != nil || len(backupsIn(h.home)) != len(before)+1 {
		t.Errorf("safety archive %s: %v; backups %v", env.Data.Safety, err, backupsIn(h.home))
	}
	if state.RestoreIncomplete(pathsOf(h.home)) {
		t.Error("marker left after a finished rollback")
	}
}

// A venv that does not install changes nothing: no backup, nothing
// stopped, the unit and the venvs as they were.
func TestNativeUpgradeFetchFailureChangesNothing(t *testing.T) {
	h := nativeUpgradeSetup(t)
	h.tools.failPip = true
	unitPath := filepath.Join(h.n.UnitDir, h.n.ServerUnit())
	unit := readFile0(unitPath)
	venvs := h.venvs(t)
	before := backupsIn(h.home)
	code, out, errOut := run(t, "server", "upgrade", "--allow-unordered", "--json")
	env := decodeUpgrade(t, out)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "nothing was changed") ||
		!strings.Contains(env.Error.Message, "cannot install the venv "+h.n.Version()) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if readFile0(unitPath) != unit || !slices.Equal(h.venvs(t), venvs) {
		t.Errorf("changed: venvs %q (were %q), unit\n%s", h.venvs(t), venvs, readFile0(unitPath))
	}
	for _, c := range h.sys.calls {
		if strings.Contains(c, " stop ") || strings.Contains(c, "restart") || strings.Contains(c, "disable") {
			t.Errorf("stopped: %q", c)
		}
	}
	if len(backupsIn(h.home)) != len(before) || state.RestoreIncomplete(pathsOf(h.home)) {
		t.Errorf("backups %v, marker %v", backupsIn(h.home), state.RestoreIncomplete(pathsOf(h.home)))
	}
}

// The plan names what native does, and a native host without its server
// unit is told so, not about compose.yaml (ruling R26).
func TestNativeUpgradePlanAndNoServer(t *testing.T) {
	h := nativeUpgradeSetup(t)
	pin := h.n.Version()
	code, out, _ := run(t, "server", "upgrade", "--allow-unordered", "--plan", "--json")
	env := decodeUpgrade(t, out)
	want := "would install the venv " + pin + " beside " + nativeOld + " (a failure changes nothing)"
	if code != output.OK || !slices.ContainsFunc(env.Data.Steps, func(s step) bool { return s.Name == "install" && s.Detail == want }) ||
		strings.Contains(out, "download") || len(h.tools.builtWhen) != 0 || h.deployed(t) != nativeOld {
		t.Fatalf("exit %d, want %q\n%s", code, want, out)
	}
	if err := os.Remove(filepath.Join(h.n.UnitDir, h.n.ServerUnit())); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, "server", "upgrade", "--json")
	env = decodeUpgrade(t, out)
	if code != output.Blocked || env.Error == nil || strings.Contains(env.Error.Message, "compose") ||
		!strings.Contains(env.Error.Message, "no "+h.n.ServerUnit()+" in "+h.n.UnitDir) {
		t.Errorf("exit %d\n%s", code, out)
	}
}
