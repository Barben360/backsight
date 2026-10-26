package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agelib "filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/crypto/ssh"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

const epochDocPath = "/v1/default/banks/claude-memory-control/documents/backsight:epoch"

func epochJSON(n int) string {
	return `{"id":"backsight:epoch","document_metadata":{"epoch":"` + strconv.Itoa(n) + `","restored_at":"2026-10-20T10:00:00Z","backup_created_at":"2026-10-20T03:00:00Z"}}`
}

func (f *restoreDocker) restored() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.restores)
}

type restoreEnvelope struct {
	OK    bool `json:"ok"`
	Error struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Actions []output.Action `json:"actions"`
	} `json:"error"`
	Data struct {
		Safety     string       `json:"safety_archive"`
		RolledBack bool         `json:"rolled_back"`
		Steps      []step       `json:"steps"`
		Warnings   []string     `json:"warnings"`
		Epoch      retain.Epoch `json:"epoch"`
	} `json:"data"`
}

func runRestoreJSON(t *testing.T, args ...string) (int, restoreEnvelope, string, string) {
	t.Helper()
	code, out, errOut := run(t, append(append([]string{"restore"}, args...), "--json")...)
	var env restoreEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, errOut)
	}
	return code, env, out, errOut
}

func stepsOf(env restoreEnvelope) string {
	var s []string
	for _, st := range env.Data.Steps {
		s = append(s, st.Name+" "+st.Status)
	}
	return strings.Join(s, ", ")
}

func called(dk *restoreDocker, part string) bool {
	dk.mu.Lock()
	defer dk.mu.Unlock()
	for _, c := range dk.calls {
		if strings.Contains(c, part) {
			return true
		}
	}
	return false
}

// The next epoch is above the server's before the restore, the archive's
// (what the restored database holds) and this client's: the server answers
// A until the database is replaced, then B.
func TestRestoreNextEpochIsAboveEveryKnownOne(t *testing.T) {
	for _, c := range []struct{ before, archive, local, want int }{{7, 3, 0, 8}, {2, 5, 0, 6}, {2, 3, 9, 10}} {
		t.Run(strconv.Itoa(c.want), func(t *testing.T) {
			home, archive, dk, port := restoreSetup(t)
			if c.local > 0 {
				if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
					s.RestoreEpoch = &state.RestoreEpoch{Bank: "claude-memory", Epoch: c.local}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodGet || r.URL.Path != epochDocPath {
					return false
				}
				if dk.restored() == 0 {
					_, _ = w.Write([]byte(epochJSON(c.before)))
				} else {
					_, _ = w.Write([]byte(epochJSON(c.archive)))
				}
				return true
			}))
			if code, out, errOut := run(t, "restore", archive); code != output.OK {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			if e := epochOf(t, port); e.Epoch != c.want {
				t.Errorf("epoch %d, want %d", e.Epoch, c.want)
			}
		})
	}
}

// The epoch before the restore comes from the database when the API cannot
// give it; when neither can, restore refuses before anything stops.
func TestRestoreNeedsTheEpochBeforeItStops(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == epochDocPath && dk.restored() == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}))
	dk.sqlFails = true
	before := backupsIn(home)
	code, env, out, errOut := runRestoreJSON(t, archive)
	if code != output.Blocked || !strings.Contains(env.Error.Message, "restore epoch") || !strings.Contains(env.Error.Message, "nothing was changed") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if called(dk, "stop") || called(dk, "pg_dump") || len(backupsIn(home)) != len(before) || state.RestoreIncomplete(pathsOf(home)) {
		t.Errorf("acted before refusing: %q, backups %v", dk.calls, backupsIn(home))
	}

	dk.sqlFails, dk.sqlEpoch = false, `{"epoch":"4","restored_at":"2026-10-20T10:00:00Z","backup_created_at":"2026-10-20T03:00:00Z"}`
	if code, out, errOut := run(t, "restore", archive); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if e := epochOf(t, port); e.Epoch != 5 {
		t.Errorf("epoch %d, want 5 (the database's 4 + 1)", e.Epoch)
	}
}

// An epoch that cannot be read back or written after the start rolls back
// (exit 30); the rollback writes an epoch above every one seen or
// attempted, from the safety dump's time, and clears the marker.
func TestRestoreRollsBackWhenTheEpochFails(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(r *http.Request) bool
		want int
	}{
		{"read", func(r *http.Request) bool { return r.Method == http.MethodGet && r.URL.Path == epochDocPath }, 1},
		{"write", func(r *http.Request) bool {
			return r.Method == http.MethodPost && r.URL.Path == "/v1/default/banks/claude-memory-control/memories"
		}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, archive, dk, port := restoreSetup(t)
			var once sync.Once
			pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
				failed := false
				if dk.restored() == 1 && c.fail(r) {
					once.Do(func() { failed = true; w.WriteHeader(http.StatusInternalServerError) })
				}
				return failed
			}))
			code, env, out, errOut := runRestoreJSON(t, archive)
			steps := stepsOf(env)
			if code != output.RolledBack || !env.Data.RolledBack || !strings.Contains(steps, "epoch failed") || !strings.HasSuffix(steps, "rollback done") {
				t.Fatalf("exit %d, steps %s\n%s%s", code, steps, out, errOut)
			}
			if len(dk.restores) != 2 || dk.restores[0] != 10 || dk.restores[1] != 25 || dk.rows != 25 || !dk.running {
				t.Errorf("restores %v rows %d running %v", dk.restores, dk.rows, dk.running)
			}
			if e := epochOf(t, port); e.Epoch != c.want || !e.BackupCreatedAt.Equal(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)) {
				t.Errorf("rollback epoch %+v, want %d from the safety dump of 12:00", e, c.want)
			}
			if state.RestoreIncomplete(pathsOf(home)) {
				t.Error("marker left after a finished rollback")
			}
		})
	}
}

// A stop that fails restores nothing: the server is started again.
func TestRestoreStopFailureRestoresNothing(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	dk.failStop = true
	code, _, errOut := run(t, "restore", archive)
	if code != output.Blocked || !strings.Contains(errOut, "nothing was restored") {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if !called(dk, "up -d --wait") || called(dk, "pg_restore -U hindsight -d") || called(dk, "dropdb") || dk.rows != 25 {
		t.Errorf("calls %q", dk.calls)
	}
	if state.RestoreIncomplete(pathsOf(home)) || epochOf(t, port).Epoch != 0 {
		t.Error("marker or epoch left by a restore that changed nothing")
	}
}

// A safety backup that fails changes nothing: no stop, no marker.
func TestRestoreSafetyBackupFailureChangesNothing(t *testing.T) {
	home, archive, dk, _ := restoreSetup(t)
	dk.failDump = true
	code, _, errOut := run(t, "restore", archive)
	if code != output.Blocked || !strings.Contains(errOut, "nothing was changed") {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if called(dk, "stop") || state.RestoreIncomplete(pathsOf(home)) || !dk.running {
		t.Errorf("acted after a failed safety backup: %q", dk.calls)
	}
}

// A database restore that fails after the drop rolls back to the safety
// dump.
func TestRestoreRollsBackWhenTheDatabaseRestoreFails(t *testing.T) {
	home, archive, dk, _ := restoreSetup(t)
	dk.failLoad = 1
	code, _, errOut := run(t, "restore", archive)
	if code != output.RolledBack || len(dk.restores) != 1 || dk.restores[0] != 25 || dk.rows != 25 || !dk.running {
		t.Fatalf("exit %d, restores %v rows %d\n%s", code, dk.restores, dk.rows, errOut)
	}
	if state.RestoreIncomplete(pathsOf(home)) {
		t.Error("marker left")
	}
}

// A rollback that fails too exits 20: the locks are released, no clear
// dump is left, and the marker stays for doctor to name the way back.
func TestRestoreFailedRollbackKeepsTheMarker(t *testing.T) {
	for name, prep := range map[string]func(dk *restoreDocker){
		"counts":   func(dk *restoreDocker) { dk.lose = 2 },
		"no start": func(dk *restoreDocker) { dk.failStart = true },
	} {
		t.Run(name, func(t *testing.T) {
			home, archive, dk, _ := restoreSetup(t)
			prep(dk)
			code, _, errOut := run(t, "restore", archive)
			safety := filepath.Join(pathsOf(home).Data, "backups", "backsight-20261025T120000Z.tar.gz.age")
			if code != output.Blocked || !strings.Contains(errOut, "rollback") || !strings.Contains(errOut, safety) {
				t.Fatalf("exit %d %s", code, errOut)
			}
			d := pathsOf(home)
			m, err := state.ReadRestoreMarker(d)
			if err != nil || m == nil || m.Safety != safety || m.Archive != archive {
				t.Errorf("marker %+v %v", m, err)
			}
			if _, err := os.Stat(d.MaintenanceLock()); err == nil {
				t.Error("maintenance lock left")
			}
			if _, err := os.Stat(filepath.Join(d.LocksDir(), "server.lock")); err == nil {
				t.Error("server.lock left")
			}
			if ents, _ := os.ReadDir(filepath.Join(d.State, "staging")); len(ents) != 0 {
				t.Errorf("staging left: %v", ents)
			}
		})
	}
}

// The restored server first runs on a key only restore knows: a client on
// the normal key is refused until the epoch is written; then the server
// runs on its key again. The marker and both locks are held meanwhile.
func TestRestoreClosesTheWindowWithATemporaryKey(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	sec, _ := config.LoadSecrets(secretsFile(home))
	requireKey.Store(true)
	acceptedKey.Store(sec.APIKey)
	normal, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), sec.APIKey, 5*time.Second)
	var during []string
	pointAt(t, home, proxyHindsight(t, port, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/default/banks/claude-memory-control/memories" {
			_, err := normal.Health(context.Background())
			d := pathsOf(home)
			during = append(during, strconv.FormatBool(err != nil), strconv.FormatBool(state.RestoreIncomplete(d)),
				strconv.FormatBool(state.MaintenanceHeld(d.MaintenanceLock())), strconv.FormatBool(state.Inspect(filepath.Join(d.LocksDir(), "server.lock")) != nil))
		}
		return false
	}))
	if code, out, errOut := run(t, "restore", archive); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if strings.Join(during, " ") != "true true true true" {
		t.Errorf("at the epoch write (normal key refused, marker, maintenance, server.lock): %v", during)
	}
	if len(dk.keys) != 2 || dk.keys[0] == sec.APIKey || dk.keys[1] != sec.APIKey {
		t.Errorf("the server started %d time(s); want a temporary key, then the normal one", len(dk.keys))
	}
	if _, err := normal.Health(context.Background()); err != nil {
		t.Errorf("the normal key is refused after the restore: %v", err)
	}
	if env, _ := os.ReadFile(filepath.Join(composeDir(home), ".env")); !strings.Contains(string(env), "HINDSIGHT_API_TENANT_API_KEY='"+sec.APIKey+"'") {
		t.Error(".env is not back on the normal key")
	}
	if state.RestoreIncomplete(pathsOf(home)) {
		t.Error("marker left after a restore")
	}
}

// --json streams one progress line per step on stderr, and the safety
// backup's warnings reach the report.
func TestRestoreStreamsProgressAndKeepsSafetyWarnings(t *testing.T) {
	home, archive, _, port := restoreSetup(t)
	pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/default/banks/claude-memory/config" {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}))
	code, env, out, errOut := runRestoreJSON(t, archive)
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(errOut), "\n") {
		var p struct {
			Event, Command, Step, Status string
		}
		if json.Unmarshal([]byte(l), &p) == nil && p.Event == "progress" && p.Command == "restore" && p.Step != "" && p.Status != "" {
			n++
		}
	}
	if n != len(env.Data.Steps) || n < 10 {
		t.Errorf("%d progress lines for %d steps:\n%s", n, len(env.Data.Steps), errOut)
	}
	if !strings.Contains(strings.Join(env.Data.Warnings, "\n"), "bank config not saved") {
		t.Errorf("safety backup warnings lost: %v", env.Data.Warnings)
	}
}

// --no-safety restores without a new safety backup (restoring the safety
// archive by hand); a failure after the drop then has nothing to roll back
// to: exit 20, marker kept.
func TestRestoreWithoutSafety(t *testing.T) {
	home, archive, dk, port := restoreSetup(t)
	before := backupsIn(home)
	code, env, out, errOut := runRestoreJSON(t, archive, "--no-safety")
	if code != output.OK || len(backupsIn(home)) != len(before) || !strings.Contains(stepsOf(env), "safety backup skipped") || dk.rows != 10 {
		t.Fatalf("exit %d, backups %v, steps %s\n%s%s", code, backupsIn(home), stepsOf(env), out, errOut)
	}
	if epochOf(t, port).Epoch != 1 {
		t.Error("no epoch")
	}
	dk.lose = 1
	code, _, errOut = run(t, "restore", archive, "--no-safety")
	if code != output.Blocked || !strings.Contains(errOut, "rollback") {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if m, _ := state.ReadRestoreMarker(pathsOf(home)); m == nil || m.Safety != archive {
		t.Errorf("marker %+v", m)
	}
}

// The plan runs the refusals too: a held maintenance lock, an archive this
// install cannot run.
func TestRestorePlanRefuses(t *testing.T) {
	for name, prep := range map[string]func(t *testing.T, home, archive string) (string, string){
		"lock held": func(t *testing.T, home, archive string) (string, string) {
			_ = os.WriteFile(pathsOf(home).MaintenanceLock(), []byte(`{"pid":1,"command":"update"}`), 0o600)
			return archive, "maintenance in progress"
		},
		"newer image": func(t *testing.T, home, _ string) (string, string) {
			return writeArchive(t, home, backup.Manifest{HindsightImage: "ghcr.io/vectorize-io/hindsight:9.0.0@sha256:ffff", Bank: "claude-memory"}), "newer than this install's"
		},
	} {
		t.Run(name, func(t *testing.T) {
			home, archive, dk, _ := restoreSetup(t)
			a, want := prep(t, home, archive)
			dk.calls = nil
			code, out, errOut := run(t, "restore", a, "--plan")
			if code != output.Blocked || !strings.Contains(errOut, want) || len(dk.calls) != 0 {
				t.Errorf("exit %d, calls %q\n%s%s", code, dk.calls, out, errOut)
			}
		})
	}
}

// More refusals before anything stops: another embedding model, no backup
// identity on this machine, an identity file that is not a key.
func TestRestoreRefusesMore(t *testing.T) {
	for name, c := range map[string]struct {
		code int
		want string
		prep func(t *testing.T, home, archive string) []string
	}{
		"other embeddings": {output.Blocked, "embedding", func(t *testing.T, home, _ string) []string {
			return []string{writeArchive(t, home, backup.Manifest{HindsightImage: server.HindsightImage, Bank: "claude-memory", Embeddings: "english: bge-small"})}
		}},
		"no identity": {output.NeedsUser, "no backup identity", func(t *testing.T, home, archive string) []string {
			_ = config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { s.BackupIdentity = ""; return nil })
			return []string{archive}
		}},
		"not a key": {output.Usage, "is not an age identity file or an SSH private key", func(t *testing.T, _, archive string) []string {
			f := filepath.Join(t.TempDir(), "key.txt")
			_ = os.WriteFile(f, []byte("not a key\n"), 0o600)
			return []string{archive, "--identity", f}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			home, archive, dk, _ := restoreSetup(t)
			args := c.prep(t, home, archive)
			dk.calls = nil
			code, out, errOut := run(t, append([]string{"restore"}, args...)...)
			if code != c.code || !strings.Contains(errOut, c.want) {
				t.Errorf("exit %d, want %d with %q\n%s%s", code, c.code, c.want, out, errOut)
			}
			if called(dk, "stop") || called(dk, "pg_dump") || dk.rows != 25 {
				t.Errorf("acted: %q", dk.calls)
			}
		})
	}
}

// A dump left in the database container by a killed backup or restore (it
// holds the memory in clear) is removed under the lock before a new one.
func TestStaleContainerDumpsAreRemoved(t *testing.T) {
	_, archive, dk, _ := restoreSetup(t)
	if code, out, errOut := run(t, "restore", archive); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	first := func(part string) int {
		for i, c := range dk.calls {
			if strings.Contains(c, part) {
				return i
			}
		}
		return -1
	}
	if i, j := first("rm -f /tmp/backsight-*.dump"), first("pg_dump"); i < 0 || i > j {
		t.Errorf("stale dumps removed at %d, first dump at %d: %q", i, j, dk.calls)
	}
	dk.calls = nil
	if code, _, errOut := run(t, "backup", "create"); code != output.OK {
		t.Fatalf("backup: %s", errOut)
	}
	if i, j := first("rm -f /tmp/backsight-*.dump"), first("pg_dump"); i < 0 || i > j {
		t.Errorf("backup create: stale dumps removed at %d, first dump at %d", i, j)
	}
}

// Commands that start or change the server refuse while a restore did not
// finish or maintenance runs.
func TestServerCommandsRefuseDuringARestore(t *testing.T) {
	for name, block := range map[string]func(home string){
		"marker": func(home string) {
			_ = state.WriteRestoreMarker(pathsOf(home), state.RestoreMarker{Command: "restore", Safety: "/b/s.age", Archive: "/b/a.age"})
		},
		"maintenance": func(home string) {
			_ = os.MkdirAll(pathsOf(home).State, 0o700)
			_ = os.WriteFile(pathsOf(home).MaintenanceLock(), []byte(`{"pid":1,"command":"restore"}`), 0o600)
		},
	} {
		for _, args := range [][]string{{"server", "start"}, {"server", "stop"}, {"server", "install"}, {"bank", "apply"}} {
			t.Run(name+" "+strings.Join(args, " "), func(t *testing.T) {
				home := devInstall(t, true)
				f := withCompose(t)
				pointAt(t, home, fakeHindsight(t))
				block(home)
				code, out, errOut := run(t, args...)
				if code != output.Blocked || (!strings.Contains(errOut, "restore") && !strings.Contains(errOut, "maintenance")) {
					t.Errorf("exit %d\n%s%s", code, out, errOut)
				}
				if len(f.calls) != 0 {
					t.Errorf("docker calls: %q", f.calls)
				}
				for _, c := range bankCalls {
					if !strings.HasPrefix(c, "GET ") {
						t.Errorf("server changed: %q", c)
					}
				}
			})
		}
	}
}

// encryptedSSHKey writes an OpenSSH ed25519 key encrypted with passphrase
// and returns its path and its age recipient.
func encryptedSSHKey(t *testing.T, passphrase string) (string, agelib.Recipient) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	r, err := agessh.NewEd25519Recipient(sp)
	if err != nil {
		t.Fatal(err)
	}
	return p, r
}

// A passphrase-protected SSH key is asked for its passphrase in a terminal
// and decrypts; without a terminal it is a needs_user exit naming the
// command to run by hand.
func TestPassphraseSSHIdentity(t *testing.T) {
	key, rcpt := encryptedSSHKey(t, "correct horse")
	var buf strings.Builder
	w, _ := agelib.Encrypt(&nopWriter{&buf}, rcpt)
	_, _ = io.WriteString(w, "secret payload")
	_ = w.Close()

	if _, err := readIdentities(key, nil); !errors.Is(err, errPassphrase) {
		t.Errorf("no prompt: %v", err)
	}
	ids, err := readIdentities(key, func() ([]byte, error) { return []byte("correct horse"), nil })
	if err != nil {
		t.Fatal(err)
	}
	r, err := agelib.Decrypt(strings.NewReader(buf.String()), ids...)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(r); string(got) != "secret payload" {
		t.Errorf("%q", got)
	}

	_, archive, _, _ := restoreSetup(t)
	code, env, out, errOut := runRestoreJSON(t, archive, "--identity", key, "--non-interactive")
	if code != output.NeedsUser || len(env.Error.Actions) != 1 || env.Error.Actions[0].Kind != "run_as_user" || !strings.Contains(env.Error.Actions[0].Cmd, "--identity "+key) {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

type nopWriter struct{ b *strings.Builder }

func (n *nopWriter) Write(p []byte) (int, error) { return n.b.Write(p) }

// A restore that did not finish does not stop the next one: restoring
// again is the way back doctor names. It is reported and replaced.
func TestRestoreAfterAnUnfinishedOne(t *testing.T) {
	home, archive, _, _ := restoreSetup(t)
	_ = state.WriteRestoreMarker(pathsOf(home), state.RestoreMarker{Command: "restore", Safety: "/b/s.age", Archive: "/b/a.age"})
	code, env, out, errOut := runRestoreJSON(t, archive)
	if code != output.OK || !strings.Contains(strings.Join(env.Data.Warnings, "\n"), "did not finish") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if state.RestoreIncomplete(pathsOf(home)) {
		t.Error("marker left")
	}
}

// A `<bank>-control` backsight did not make refuses the restore before
// anything changes (exit 20): no lock, no Docker call (no stop, no dump),
// no safety archive, no marker, no write to the server; the plan too
// (issue 279).
func TestRestoreRefusesAForeignControlBankUpFront(t *testing.T) {
	for _, plan := range []bool{false, true} {
		t.Run("plan="+strconv.FormatBool(plan), func(t *testing.T) {
			home, archive, dk, port := restoreSetup(t)
			var mu sync.Mutex
			var seen []string
			pointAt(t, home, proxyHindsight(t, port, func(w http.ResponseWriter, r *http.Request) bool {
				mu.Lock()
				seen = append(seen, r.Method+" "+r.URL.Path)
				mu.Unlock()
				if r.Method == http.MethodGet && r.URL.Path == "/v1/default/banks" {
					_, _ = w.Write([]byte(`{"banks":[{"bank_id":"claude-memory-control","name":"team control"}],"total":1}`))
					return true
				}
				return false
			}))
			before := backupsIn(home)
			bankCalls = nil
			args := []string{archive}
			if plan {
				args = append(args, "--plan")
			}
			code, env, out, errOut := runRestoreJSON(t, args...)
			if code != output.Blocked || !strings.Contains(env.Error.Message, "claude-memory-control") || !strings.Contains(env.Error.Message, `"team control"`) ||
				!strings.Contains(env.Error.Message, "nothing was changed") {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			if len(dk.calls) != 0 || !dk.running || dk.rows != 25 {
				t.Errorf("docker calls %q, running %v, rows %d", dk.calls, dk.running, dk.rows)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range append(seen, bankCalls...) {
				if !strings.HasPrefix(c, "GET ") {
					t.Errorf("a write to the server: %s", c)
				}
			}
			if got := backupsIn(home); len(got) != len(before) {
				t.Errorf("backups %v, before %v", got, before)
			}
			if state.RestoreIncomplete(pathsOf(home)) {
				t.Error("restore marked in progress")
			}
			if _, err := os.Stat(pathsOf(home).MaintenanceLock()); err == nil {
				t.Error("lock left")
			}
		})
	}
}
