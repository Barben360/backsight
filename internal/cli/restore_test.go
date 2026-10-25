package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agelib "filippo.io/age"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// restoreDocker plays the server's compose project: a database holding
// rows rows in each counted table, dumps that carry their row count
// ("PGDMP n=<rows>"), and the hindsight service's state. Each call notes
// whether the maintenance lock was held at that moment.
type restoreDocker struct {
	mu        sync.Mutex
	lock      string // the maintenance lock's path
	calls     []string
	unlocked  []string       // calls made without the lock held
	rows      int            // rows in each counted table; -1: no database
	copies    map[string]int // dump copied into the container → its rows
	restores  []int          // rows of each dump restored into the database
	running   bool           // the hindsight service
	lose      int            // the next restores lose one row each (a bad archive)
	failStart bool           // compose up fails
	failStop  bool           // compose stop fails
	failDump  bool           // pg_dump fails
	failLoad  int            // the next pg_restore into the database fail (after dropdb)
	sqlEpoch  string         // the epoch document's metadata read from the database ("" = no row)
	sqlFails  bool           // reading the database fails
	keys      []string       // HINDSIGHT_API_TENANT_API_KEY of each compose up
	images    []string       // the hindsight image of each compose up
	pulls     []string       // images pulled
	failPull  bool           // docker pull fails
	failImage string         // compose up of this hindsight image fails
}

// upImage reads the hindsight image of the compose.yaml a `compose ... up`
// call runs.
func upImage(args []string) string {
	for i, a := range args {
		if a == "--project-directory" && i+1 < len(args) {
			raw, _ := os.ReadFile(filepath.Join(args[i+1], "compose.yaml"))
			return server.ServiceImage(raw, "hindsight")
		}
	}
	return ""
}

// upKey reads the API key the .env in the compose project directory of a
// `compose ... up` call gives the server.
func upKey(args []string) string {
	for i, a := range args {
		if a == "--project-directory" && i+1 < len(args) {
			raw, _ := os.ReadFile(filepath.Join(args[i+1], ".env"))
			for _, l := range strings.Split(string(raw), "\n") {
				if v, ok := strings.CutPrefix(l, "HINDSIGHT_API_TENANT_API_KEY="); ok {
					return strings.Trim(v, "'")
				}
			}
		}
	}
	return ""
}

func (f *restoreDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if !state.MaintenanceHeld(f.lock) {
		f.unlocked = append(f.unlocked, line)
	}
	last := args[len(args)-1]
	switch {
	case strings.Contains(line, "retain_params"):
		if f.sqlFails {
			return []byte("connection refused"), os.ErrClosed
		}
		if f.sqlEpoch == "" {
			return []byte("null\n"), nil
		}
		return []byte(f.sqlEpoch + "\n"), nil
	case strings.Contains(line, "pg_dump") && f.failDump:
		return []byte("pg_dump: error"), os.ErrClosed
	case strings.Contains(line, "alembic_version"):
		return []byte("c7e2f9a4d1b8\n"), nil
	case strings.Contains(line, "pg_extension"):
		return []byte("0.8.7\n"), nil
	case strings.Contains(line, "pg_restore --data-only -t"):
		return []byte(strconv.Itoa(f.rows) + "\n"), nil
	case strings.Contains(line, "select count(*) from"):
		if f.rows < 0 {
			return []byte("database does not exist"), os.ErrNotExist
		}
		return []byte(strconv.Itoa(f.rows) + "\n"), nil
	case strings.Contains(line, " cp db:"): // dump out
		return nil, os.WriteFile(last, []byte(fmt.Sprintf("PGDMP n=%d", f.rows)), 0o600)
	case strings.Contains(line, " cp ") && strings.HasPrefix(last, "db:"): // dump in
		raw, err := os.ReadFile(args[len(args)-2])
		if err != nil {
			return nil, err
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(string(raw), "PGDMP n="))
		f.copies[strings.TrimPrefix(last, "db:")] = n
	case strings.Contains(line, "exec -T db dropdb"):
		f.rows = -1
	case strings.Contains(line, "exec -T db createdb"):
		f.rows = 0
	case strings.Contains(line, "exec -T db pg_restore -U hindsight -d hindsight"):
		if f.failLoad > 0 {
			f.failLoad--
			return []byte("pg_restore: error"), os.ErrClosed
		}
		f.rows = f.copies[last]
		f.restores = append(f.restores, f.rows)
		if f.lose > 0 {
			f.lose--
			f.rows--
		}
	case strings.HasSuffix(line, "stop hindsight"):
		if f.failStop {
			return []byte("cannot stop"), os.ErrClosed
		}
		f.running = false
	case args[0] == "pull":
		f.pulls = append(f.pulls, last)
		if f.failPull {
			return []byte("manifest unknown"), os.ErrNotExist
		}
	case strings.HasSuffix(line, "up -d --wait"):
		img := upImage(args)
		f.images = append(f.images, img)
		if f.failImage != "" && img == f.failImage {
			f.running = false
			return []byte("container hindsight exited (1)"), os.ErrClosed
		}
		if f.failStart {
			return []byte("container exited"), os.ErrClosed
		}
		f.running = true
		if k := upKey(args); k != "" {
			f.keys = append(f.keys, k)
			if requireKey.Load() {
				acceptedKey.Store(k)
			}
		}
	case strings.Contains(line, " ps --all"):
		st := "exited"
		if f.running {
			st = "running"
		}
		return []byte(`{"Service":"db","State":"running","Health":"healthy"}` + "\n" + `{"Service":"hindsight","State":"` + st + `","Health":""}`), nil
	}
	return nil, nil
}

// restoreSetup is a dev install with a fake server and Docker, an archive
// of the database at 10 rows taken at 04:00, and the server grown to 25
// rows since; the clock is at noon.
func restoreSetup(t *testing.T) (home, archive string, dk *restoreDocker, port int) {
	t.Helper()
	home = devInstall(t, true)
	port = fakeHindsight(t)
	pointAt(t, home, port)
	dk = &restoreDocker{lock: pathsOf(home).MaintenanceLock(), rows: 10, copies: map[string]int{}, running: true}
	old, oldPoll := composeRunner, healthPoll
	composeRunner, healthPoll = dk, time.Millisecond
	t.Cleanup(func() { composeRunner, healthPoll = old, oldPoll })
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	if code, out, errOut := run(t, "backup", "create"); code != output.OK {
		t.Fatalf("backup: exit %d\n%s%s", code, out, errOut)
	}
	archive = filepath.Join(pathsOf(home).Data, "backups", "backsight-20261025T040000Z.tar.gz.age")
	withClock(t, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	dk.rows, dk.calls, dk.unlocked = 25, nil, nil
	return home, archive, dk, port
}

func epochOf(t *testing.T, port int) retain.Epoch {
	t.Helper()
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "", 5*time.Second)
	e, err := retain.ReadEpoch(context.Background(), cl, "claude-memory")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func backupsIn(home string) []string {
	ents, _ := os.ReadDir(filepath.Join(pathsOf(home).Data, "backups"))
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".age") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestRestoreReplacesTheDatabaseAndStartsANewEpoch(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	code, out, errOut := run(t, "restore", archive, "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Safety   string           `json:"safety_archive"`
			Restored map[string]int64 `json:"restored_counts"`
			Epoch    struct {
				Epoch           int    `json:"epoch"`
				BackupCreatedAt string `json:"backup_created_at"`
			} `json:"epoch"`
			Manifest backup.Manifest `json:"manifest"`
			Steps    []step          `json:"steps"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || !env.OK {
		t.Fatalf("%v\n%s", err, out)
	}
	// The database holds the archive's rows, the server runs again.
	if dk.rows != 10 || !dk.running || env.Data.Restored["memory_units"] != 10 || env.Data.Manifest.Counts["memory_units"] != 10 {
		t.Errorf("rows %d, running %v, %+v", dk.rows, dk.running, env.Data)
	}
	// Only the API stopped: the database is restored in place.
	var stopped bool
	for _, c := range dk.calls {
		stopped = stopped || strings.HasSuffix(c, "stop hindsight")
		if strings.HasSuffix(c, " stop") {
			t.Errorf("the whole project was stopped: %q", c)
		}
	}
	if !stopped {
		t.Errorf("hindsight never stopped: %q", dk.calls)
	}
	// The safety archive holds the server as it was, and the archive
	// restored was not pruned for it.
	if env.Data.Safety != filepath.Join(pathsOf(home).Data, "backups", "backsight-20261025T120000Z.tar.gz.age") {
		t.Errorf("safety archive %q", env.Data.Safety)
	}
	if side, err := os.ReadFile(env.Data.Safety + ".json"); err != nil || !strings.Contains(string(side), `"memory_units": 25`) {
		t.Errorf("safety manifest %v %s", err, side)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("the restored archive is gone: %v", err)
	}
	// The epoch the clients reconcile on (0044), in the control bank: the
	// memory bank gets no retain and no setting from a restore.
	var epochWrites []string
	for _, c := range bankCalls {
		if (strings.HasPrefix(c, "POST ") && !strings.HasSuffix(c, "/recall")) || strings.HasPrefix(c, "PUT ") || strings.HasPrefix(c, "PATCH ") {
			epochWrites = append(epochWrites, c)
		}
	}
	if got := strings.Join(epochWrites, ", "); got != "PUT claude-memory-control /, PATCH claude-memory-control /config, POST claude-memory-control /memories" {
		t.Errorf("writes to the server: %s", got)
	}
	e := epochOf(t, port)
	if e.Epoch != 1 || !e.BackupCreatedAt.Equal(time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)) || !e.RestoredAt.Equal(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)) ||
		env.Data.Epoch.Epoch != 1 || env.Data.Epoch.BackupCreatedAt != "2026-10-25T04:00:00Z" {
		t.Errorf("epoch %+v, reported %+v", e, env.Data.Epoch)
	}
	// Held from the first Docker call to the last, released after.
	if len(dk.unlocked) > 0 {
		t.Errorf("docker calls without the maintenance lock: %q", dk.unlocked)
	}
	if _, err := os.Stat(pathsOf(home).MaintenanceLock()); err == nil {
		t.Error("the maintenance lock was left behind")
	}
	if ents, _ := os.ReadDir(filepath.Join(pathsOf(home).State, "staging")); len(ents) != 0 {
		t.Errorf("clear dumps left in staging: %v", ents)
	}
	// A second restore moves past the epoch the server holds.
	if code, out, _ := run(t, "restore", archive); code != output.OK || !strings.Contains(out, "restore epoch 2") {
		t.Errorf("second restore: exit %d\n%s", code, out)
	}
	if e := epochOf(t, port); e.Epoch != 2 {
		t.Errorf("second epoch %d", e.Epoch)
	}
}

// The next epoch differs from the one the clients hold, even when the
// archive brings back an older one.
func TestRestoreEpochFollowsTheServersNotTheArchives(t *testing.T) {
	_, archive, _, port := restoreSetup(t)
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "", 5*time.Second)
	if err := retain.WriteEpoch(context.Background(), cl, "claude-memory", retain.Epoch{Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run(t, "restore", archive); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if e := epochOf(t, port); e.Epoch != 8 {
		t.Errorf("epoch %d, want 8", e.Epoch)
	}
}

// The lock is held through the epoch write too: no client flushes between
// the restore and the epoch that tells it to reconcile.
func TestRestoreHoldsTheLockUntilTheEpochIsWritten(t *testing.T) {
	home, archive, _, port := restoreSetup(t)
	var heldAtEpoch []bool
	pointAt(t, home, proxyHindsight(t, port, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories") {
			heldAtEpoch = append(heldAtEpoch, state.MaintenanceHeld(pathsOf(home).MaintenanceLock()))
		}
		return false
	}))
	if code, out, errOut := run(t, "restore", archive); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if len(heldAtEpoch) != 1 || !heldAtEpoch[0] {
		t.Errorf("lock at the epoch write: %v", heldAtEpoch)
	}
}

// A restore that does not verify puts the safety archive back (exit 30)
// and writes an epoch above every one seen or attempted.
func TestRestoreRollsBackWhenTheCountsDiffer(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	dk.lose = 1
	code, out, errOut := run(t, "restore", archive)
	if code != output.RolledBack || !strings.Contains(errOut, "rolled back") || !strings.Contains(out, "rollback") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	// The archive's dump went in, then the safety dump: the server holds
	// its rows of before the restore, and runs.
	if len(dk.restores) != 2 || dk.restores[0] != 10 || dk.restores[1] != 25 || dk.rows != 25 || !dk.running {
		t.Errorf("restores %v, rows %d, running %v", dk.restores, dk.rows, dk.running)
	}
	// The rollback writes an epoch too, from the safety dump's time: a
	// client that sent to the restored data reconciles. The memory bank
	// gets no retain.
	if e := epochOf(t, port); e.Epoch != 1 || !e.BackupCreatedAt.Equal(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("rollback epoch %+v", e)
	}
	for _, c := range bankCalls {
		if strings.HasPrefix(c, "POST ") && strings.HasSuffix(c, " /memories") && !strings.HasPrefix(c, "POST claude-memory-control ") {
			t.Errorf("retain into the memory bank during a failed restore: %q", bankCalls)
		}
	}
	if got := backupsIn(home); len(got) != 2 {
		t.Errorf("the safety archive is not kept: %v", got)
	}
	if len(dk.unlocked) > 0 {
		t.Errorf("docker calls without the maintenance lock: %q", dk.unlocked)
	}
	if _, err := os.Stat(pathsOf(home).MaintenanceLock()); err == nil {
		t.Error("the maintenance lock was left behind")
	}
}

// A server that does not answer after the restore is rolled back too; the
// JSON envelope carries the steps and the safety archive.
func TestRestoreRollsBackWhenTheRecallFails(t *testing.T) {
	_, archive, dk, port := restoreSetup(t)
	recallStatus.Store(http.StatusInternalServerError)
	code, out, _ := run(t, "restore", archive, "--json")
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
		Data struct {
			Safety     string `json:"safety_archive"`
			RolledBack bool   `json:"rolled_back"`
			Steps      []step `json:"steps"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if code != output.RolledBack || env.OK || env.Error.Code != output.RolledBack || !env.Data.RolledBack || env.Data.Safety == "" {
		t.Errorf("exit %d\n%s", code, out)
	}
	if n := len(env.Data.Steps); n < 2 || env.Data.Steps[n-2].Name != "verify" || env.Data.Steps[n-2].Status != "failed" || env.Data.Steps[n-1].Name != "rollback" {
		t.Errorf("steps %+v", env.Data.Steps)
	}
	if dk.rows != 25 || !dk.running {
		t.Errorf("rows %d, running %v", dk.rows, dk.running)
	}
	if e := epochOf(t, port); e.Epoch != 1 {
		t.Errorf("rollback epoch %+v", e)
	}
}

// When even the rollback fails, the exit is 20 and the message names the
// safety archive to restore by hand.
func TestRestoreReportsAFailedRollback(t *testing.T) {
	_, archive, dk, _ := restoreSetup(t)
	dk.failStart = true
	code, _, errOut := run(t, "restore", archive)
	if code != output.Blocked || !strings.Contains(errOut, "rollback") || !strings.Contains(errOut, "backsight restore ") || !strings.Contains(errOut, "backsight-20261025T120000Z.tar.gz.age") {
		t.Errorf("exit %d %q", code, errOut)
	}
}

// Refusals come before anything stops: the server keeps running untouched,
// no safety archive is written, no epoch, no lock left.
func TestRestoreRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		code int
		want string
		prep func(t *testing.T, home, archive string) []string // extra arguments
	}{
		{"newer image", output.Blocked, "newer than this install's", func(t *testing.T, home, _ string) []string {
			return []string{writeArchive(t, home, backup.Manifest{HindsightImage: "ghcr.io/vectorize-io/hindsight:9.0.0@sha256:ffff", Bank: "claude-memory"})}
		}},
		{"image it cannot place", output.Blocked, "cannot tell", func(t *testing.T, home, _ string) []string {
			return []string{writeArchive(t, home, backup.Manifest{HindsightImage: "ghcr.io/vectorize-io/hindsight@sha256:ffff", Bank: "claude-memory"})}
		}},
		{"another bank", output.Blocked, `bank "other"`, func(t *testing.T, home, _ string) []string {
			return []string{writeArchive(t, home, backup.Manifest{HindsightImage: server.HindsightImage, Bank: "other"})}
		}},
		{"identity that does not decrypt it", output.Usage, "--identity", func(t *testing.T, _, archive string) []string {
			other, _ := agelib.GenerateX25519Identity()
			f := filepath.Join(t.TempDir(), "key.txt")
			_ = os.WriteFile(f, []byte(other.String()+"\n"), 0o600)
			return []string{archive, "--identity", f}
		}},
		{"unreadable identity", output.Usage, "malformed secret key", func(t *testing.T, _, archive string) []string {
			f := filepath.Join(t.TempDir(), "key.txt")
			_ = os.WriteFile(f, []byte("not a key\n"), 0o600)
			return []string{archive, "--identity", f}
		}},
		{"missing archive", output.Usage, "no such file", func(t *testing.T, home, _ string) []string {
			return []string{filepath.Join(home, "nope.tar.gz.age")}
		}},
		{"lock held", output.Blocked, "maintenance in progress", func(t *testing.T, home, archive string) []string {
			_ = os.WriteFile(pathsOf(home).MaintenanceLock(), []byte(`{"pid":1,"command":"update"}`), 0o600)
			return []string{archive}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, archive, dk, port := restoreSetup(t)
			args := c.prep(t, home, archive)
			dk.calls = nil // writing a test archive dumps too
			before := backupsIn(home)
			code, out, errOut := run(t, append([]string{"restore"}, args...)...)
			if code != c.code || !strings.Contains(errOut, c.want) {
				t.Errorf("exit %d, want %d with %q\n%s%s", code, c.code, c.want, out, errOut)
			}
			for _, call := range dk.calls {
				if strings.Contains(call, "stop") || strings.Contains(call, "dropdb") || strings.Contains(call, "pg_dump") {
					t.Errorf("acted before refusing: %q", call)
				}
			}
			if dk.rows != 25 || !dk.running {
				t.Errorf("server changed: rows %d, running %v", dk.rows, dk.running)
			}
			if got := backupsIn(home); len(got) != len(before) {
				t.Errorf("backups %v, before %v", got, before)
			}
			if e := epochOf(t, port); e.Epoch != 0 {
				t.Errorf("epoch %+v", e)
			}
			if c.name != "lock held" {
				if _, err := os.Stat(pathsOf(home).MaintenanceLock()); err == nil {
					t.Error("lock left")
				}
			}
		})
	}
}

// writeArchive writes an archive of 10 rows with manifest m's images and
// bank, encrypted to the install's identity, and returns its path.
func writeArchive(t *testing.T, home string, m backup.Manifest) string {
	t.Helper()
	sec, _ := config.LoadSecrets(secretsFile(home))
	id, err := agelib.ParseX25519Identity(sec.BackupIdentity)
	if err != nil {
		t.Fatal(err)
	}
	m.CreatedAt = time.Date(2026, 10, 24, 4, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path, _, err := backup.Create(context.Background(), server.DB{Compose: composeFor(config.Config{})}, dir, t.TempDir(), m, nil, []agelib.Recipient{id.Recipient()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// --plan decrypts and checks the archive, names the safety archive it
// would write, and changes nothing.
func TestRestorePlanChangesNothing(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	bankCalls = nil
	code, out, errOut := run(t, "restore", archive, "--plan", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var env struct {
		Data struct {
			Plan     bool            `json:"plan"`
			Safety   string          `json:"safety_archive"`
			Manifest backup.Manifest `json:"manifest"`
			Steps    []step          `json:"steps"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if !env.Data.Plan || env.Data.Safety != filepath.Join(pathsOf(home).Data, "backups", "backsight-20261025T120000Z.tar.gz.age") ||
		env.Data.Manifest.Counts["memory_units"] != 10 || !env.Data.Manifest.CreatedAt.Equal(time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("%+v", env.Data)
	}
	var names []string
	for _, s := range env.Data.Steps {
		names = append(names, s.Name)
		if s.Status != "planned" {
			t.Errorf("step %+v", s)
		}
	}
	if got := strings.Join(names, ","); got != "check,lock,safety backup,stop,restore,counts,start,health,recall,epoch,key" {
		t.Errorf("steps %s", got)
	}
	if len(dk.calls) != 0 {
		t.Errorf("docker calls: %q", dk.calls)
	}
	for _, c := range bankCalls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("a plan wrote to the server: %q", c)
		}
	}
	if got := backupsIn(home); len(got) != 1 {
		t.Errorf("backups %v", got)
	}
	if e := epochOf(t, port); e.Epoch != 0 {
		t.Errorf("epoch %+v", e)
	}
	if _, err := os.Stat(pathsOf(home).MaintenanceLock()); err == nil {
		t.Error("lock left")
	}
	if ents, _ := os.ReadDir(filepath.Join(pathsOf(home).State, "staging")); len(ents) != 0 {
		t.Errorf("staging left: %v", ents)
	}
	// Text mode shows the same plan.
	if code, out, _ := run(t, "restore", archive, "--plan"); code != output.OK || !strings.Contains(out, "planned  safety backup: would write ") || !strings.Contains(out, "nothing was changed") {
		t.Errorf("text plan: exit %d\n%s", code, out)
	}
}

func TestRestoreOfARemoteServerIsRefused(t *testing.T) {
	home := devInstall(t, true)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(raw), "[server]", "[server]\nmode = \"remote\"", 1)), 0o600)
	if code, _, errOut := run(t, "restore", filepath.Join(home, "x.tar.gz.age")); code != output.Usage || !strings.Contains(errOut, "remote") {
		t.Errorf("exit %d %q", code, errOut)
	}
}
