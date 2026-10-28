package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agelib "filippo.io/age"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
)

// backupDocker answers the commands a backup and a verify run: psql values,
// pg_dump and pg_restore, compose cp (writes the dump), and the throwaway
// container of verify.
type backupDocker struct {
	mu          sync.Mutex
	calls       []string
	restored    string // count the throwaway database returns
	failDump    bool
	failRestore bool
	cpDest      string // where compose cp wrote the clear dump
}

func (f *backupDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	switch {
	case strings.Contains(line, "pg_dump") && f.failDump:
		return []byte("pg_dump: error"), os.ErrPermission
	case strings.HasPrefix(line, "compose") && strings.Contains(line, " cp db:"):
		f.cpDest = args[len(args)-1]
		return nil, os.WriteFile(args[len(args)-1], []byte("PGDMP fake"), 0o600)
	case strings.HasPrefix(line, "exec backsight-verify-") && strings.Contains(line, "pg_restore") && f.failRestore:
		return []byte("pg_restore: error"), os.ErrInvalid
	case strings.Contains(line, "alembic_version"):
		return []byte("c7e2f9a4d1b8\n"), nil
	case strings.Contains(line, "pg_extension"):
		return []byte("0.8.7\n"), nil
	case strings.HasPrefix(line, "compose") && strings.Contains(line, "pg_restore --data-only -t"):
		return []byte("42\n"), nil
	case strings.HasPrefix(line, "exec backsight-verify-") && strings.Contains(line, "count(*)"):
		return []byte(f.restored + "\n"), nil
	}
	return nil, nil
}

func withBackupDocker(t *testing.T) *backupDocker {
	t.Helper()
	f := &backupDocker{restored: "42"}
	old := composeRunner
	composeRunner = f
	t.Cleanup(func() { composeRunner = old })
	return f
}

func TestBackupCreateListVerify(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dk := withBackupDocker(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	code, out, errOut := run(t, "backup", "create")
	if code != output.OK || !strings.Contains(out, "backsight-20261025T040000Z.tar.gz.age") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	sec, _ := config.LoadSecrets(secretsFile(home))
	if !strings.HasPrefix(sec.BackupIdentity, "AGE-SECRET-KEY-") {
		t.Error("no identity generated")
	}
	if _, err := os.Stat(pathsOf(home).MaintenanceLock()); err == nil {
		t.Error("the maintenance lock was left behind")
	}
	if code, out, _ := run(t, "backup", "list"); code != output.OK || !strings.Contains(out, "backsight-20261025T040000Z") || !strings.Contains(out, "memory_units:42") {
		t.Errorf("list: exit %d\n%s", code, out)
	}
	path := filepath.Join(pathsOf(home).Data, "backups", "backsight-20261025T040000Z.tar.gz.age")
	if code, out, _ := run(t, "backup", "verify", path); code != output.OK || !strings.Contains(out, ": ok") {
		t.Errorf("verify: exit %d\n%s", code, out)
	}
	removed := false
	for _, c := range dk.calls {
		removed = removed || strings.HasPrefix(c, "rm -f -v backsight-verify-")
	}
	if !removed {
		t.Errorf("the throwaway container was not removed: %q", dk.calls)
	}
	dk.restored = "41"
	if code, out, _ := run(t, "backup", "verify", path); code != output.Blocked || !strings.Contains(out, "MISMATCH") {
		t.Errorf("a short restore: exit %d\n%s", code, out)
	}
}

// A second backup the same day keeps the first (taken before a forget, say)
// even with one daily slot; past 24 h the retention prunes it.
func TestBackupCreateKeepsTheSameDaysEarlierArchive(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withBackupDocker(t)
	configure(t, home, "[backup]\nkeep_daily = 1\nkeep_weekly = 1")
	for i, c := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 10, 25, 21, 51, 10, 0, time.UTC), "backsight-20261025T215110Z.tar.gz.age"},
		{time.Date(2026, 10, 25, 22, 10, 8, 0, time.UTC), "backsight-20261025T215110Z.tar.gz.age backsight-20261025T221008Z.tar.gz.age"},
		{time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC), "backsight-20261025T221008Z.tar.gz.age backsight-20261026T220000Z.tar.gz.age"},
	} {
		withClock(t, c.at)
		if code, out, errOut := run(t, "backup", "create"); code != output.OK {
			t.Fatalf("backup %d: exit %d\n%s%s", i, code, out, errOut)
		}
		if got := strings.Join(backupsIn(home), " "); got != c.want {
			t.Errorf("after backup %d: %s, want %s", i, got, c.want)
		}
	}
}

func TestBackupRefusesDuringMaintenanceAndReportsAFailedDump(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dk := withBackupDocker(t)
	d := pathsOf(home)
	_ = os.MkdirAll(filepath.Dir(d.MaintenanceLock()), 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":1,"command":"update"}`), 0o600)
	if code, _, errOut := run(t, "backup", "create"); code != output.Blocked || !strings.Contains(errOut, "maintenance") {
		t.Errorf("exit %d %q", code, errOut)
	}
	_ = os.Remove(d.MaintenanceLock())
	dk.failDump = true
	if code, _, errOut := run(t, "backup", "create"); code != output.Blocked || !strings.Contains(errOut, "dumping the database") {
		t.Errorf("exit %d %q", code, errOut)
	}
	if list, _ := os.ReadDir(filepath.Join(d.Data, "backups")); len(list) != 0 {
		t.Errorf("a failed backup left files: %v", list)
	}
}

// The identity is a secret: shown at a terminal only, never in JSON or to
// a pipe an agent reads; import stores one.
func TestBackupIdentityShowAndImport(t *testing.T) {
	home := devInstall(t, true)
	oldOut, oldIn, oldErr := stdoutIsTerminal, stdinIsTerminal, stderrIsTerminal
	t.Cleanup(func() { stdoutIsTerminal, stdinIsTerminal, stderrIsTerminal = oldOut, oldIn, oldErr })
	no := func() bool { return false }
	stdoutIsTerminal, stdinIsTerminal, stderrIsTerminal = no, no, no
	// No terminal at all: an agent's command, refused.
	if code, out, _ := run(t, "backup", "identity", "show"); code != output.NeedsUser || strings.Contains(out, "AGE-SECRET-KEY") {
		t.Errorf("no terminal: exit %d\n%s", code, out)
	}
	stdoutIsTerminal = func() bool { return true }
	if code, out, _ := run(t, "backup", "identity", "show", "--json"); code != output.NeedsUser || strings.Contains(out, "AGE-SECRET-KEY") {
		t.Errorf("json: exit %d\n%s", code, out)
	}
	// A person's pipe to a password manager: stdout is not a terminal,
	// stderr is, and stdout holds the key alone (issue 61).
	stdoutIsTerminal, stderrIsTerminal = no, func() bool { return true }
	code, out, errOut := run(t, "backup", "identity", "show")
	if code != output.OK || !strings.HasPrefix(out, "AGE-SECRET-KEY-") || strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") || !strings.Contains(errOut, "Keep it somewhere safe") {
		t.Fatalf("pipe: exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	key := strings.TrimSpace(out)
	other := isolate(t)
	_ = os.MkdirAll(filepath.Join(other, "backsight", "config"), 0o700)
	f := filepath.Join(t.TempDir(), "key.txt")
	_ = os.WriteFile(f, []byte("# created: today\n"+key+"\n"), 0o600)
	if code, _, _ := run(t, "backup", "identity", "import", f); code != output.OK {
		t.Fatalf("import: exit %d", code)
	}
	if sec, _ := config.LoadSecrets(secretsFile(other)); sec.BackupIdentity != key {
		t.Error("imported identity not stored")
	}
	_ = os.WriteFile(f, []byte("nothing here"), 0o600)
	if code, _, _ := run(t, "backup", "identity", "import", f); code != output.Usage {
		t.Errorf("no key: exit %d", code)
	}
	_ = home
}

// jobs maintain takes the daily backup; a purge afterwards names it as
// still holding the text.
func TestMaintainBacksUpAndPurgeNamesTheBackups(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dk := withBackupDocker(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	if code, out, _ := run(t, "jobs", "maintain"); code != output.OK || !strings.Contains(out, "done:") || !strings.Contains(out, "backsight-20261025T040000Z.tar.gz.age") ||
		!strings.Contains(out, "re-attribution 0 moved") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// nothing to move: no throwaway restore to verify the backup
	for _, c := range dk.calls {
		if strings.HasPrefix(c, "exec backsight-verify-") {
			t.Errorf("verified a backup with nothing to move: %s", c)
		}
	}
	// after its backup, the daily job judged the memory and recorded it
	if st, ok, err := retain.LoadReattributionStatus(pathsOf(home).State); !ok || err != nil || !st.JudgedAt.Equal(time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("status %+v %v %v", st, ok, err)
	}
	withClock(t, time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC))
	memories["f9"] = fact("f9", "conversation:s9")
	documentUnits["conversation:s9"] = 1
	if code, out, _ := run(t, "forget", "f9", "--purge", "--yes"); code != output.OK || !strings.Contains(out, "backsight-20261025T040000Z.tar.gz.age") {
		t.Errorf("purge: exit %d\n%s", code, out)
	}
}

// With backups off, maintain does not touch Docker.
func TestMaintainWithBackupsOff(t *testing.T) {
	home := devInstall(t, true)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[backup]\nenabled = false\n")...), 0o600)
	if code, out, _ := run(t, "jobs", "maintain"); code != output.OK || !strings.Contains(out, "backup off") || !strings.Contains(out, "re-attribution skipped: backup.enabled is off") {
		t.Errorf("exit %d\n%s", code, out)
	}
	// not judged, and the reason is kept for doctor
	if st, ok, _ := retain.LoadReattributionStatus(pathsOf(home).State); !ok || !st.JudgedAt.IsZero() || !strings.Contains(st.Skipped, "backup.enabled is off") {
		t.Errorf("status %+v %v", st, ok)
	}
}

// A failed backup makes the daily job partial (exit 40): the next run retries.
func TestMaintainReportsAFailedBackup(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withBackupDocker(t).failDump = true
	if code, out, _ := run(t, "jobs", "maintain"); code != output.Partial || !strings.Contains(out, "backup failed") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// stdoutDocker prints a Compose warning on stderr, as Compose 5 does for an
// obsolete `version:` key: values come from stdout alone.
type stdoutDocker struct{ *backupDocker }

func (s stdoutDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := s.backupDocker.Run(ctx, args...)
	if strings.Contains(strings.Join(args, " "), " exec ") {
		out = append([]byte("WARN[0000] the attribute `version` is obsolete\n"), out...)
	}
	return out, err
}

func (s stdoutDocker) RunStdout(ctx context.Context, args ...string) ([]byte, error) {
	return s.backupDocker.Run(ctx, args...)
}

func TestBackupIgnoresComposeWarnings(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	f := &backupDocker{restored: "42"}
	old := composeRunner
	composeRunner = stdoutDocker{f}
	t.Cleanup(func() { composeRunner = old })
	if code, out, errOut := run(t, "backup", "create"); code != output.OK || !strings.Contains(out, "memory_units:42") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	// the clear dump was staged under the state directory, never in the backup dir
	if !strings.HasPrefix(f.cpDest, filepath.Join(pathsOf(home).State, "staging")) {
		t.Errorf("dump staged at %s", f.cpDest)
	}
}

func TestBackupOffTouchesNoDocker(t *testing.T) {
	home := devInstall(t, true)
	dk := withBackupDocker(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[backup]\nenabled = false\n")...), 0o600)
	run(t, "jobs", "maintain")
	if len(dk.calls) != 0 {
		t.Errorf("docker calls: %q", dk.calls)
	}
}

func TestBackupCreateOfARemoteServerIsRefusedButTheJobSkips(t *testing.T) {
	home := devInstall(t, true)
	dk := withBackupDocker(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(raw), "[server]", "[server]\nmode = \"remote\"", 1)), 0o600)
	if !strings.Contains(string(raw), "[server]") {
		_ = os.WriteFile(cfg, append(raw, []byte("\n[server]\nmode = \"remote\"\n")...), 0o600)
	}
	if code, _, errOut := run(t, "backup", "create"); code != output.Usage || !strings.Contains(errOut, "backsight backup create") || len(dk.calls) != 0 {
		t.Errorf("exit %d, calls %q\n%s", code, dk.calls, errOut)
	}
	// the daily job does not fail on a client: it skips the backup
	if code, out, errOut := run(t, "jobs", "maintain"); code != output.OK || len(dk.calls) != 0 {
		t.Errorf("jobs maintain: exit %d, calls %q\n%s%s", code, dk.calls, out, errOut)
	}
}

// A lock left by a dead owner does not stop the hooks; backup create breaks
// it and says so.
func TestAStaleMaintenanceLockStopsNothing(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withBackupDocker(t)
	d := pathsOf(home)
	_ = os.MkdirAll(filepath.Dir(d.MaintenanceLock()), 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":999999,"command":"backup create","since":"2026-10-25T03:00:00Z"}`), 0o600)
	// curation names a stale lock (it never breaks one)
	if code, _, errOut := run(t, "projects"); code != output.Blocked || !strings.Contains(errOut, "process is gone") {
		t.Errorf("projects: exit %d %q", code, errOut)
	}
	code, out, errOut := run(t, "backup", "create")
	if code != output.OK || !strings.Contains(out, "stale maintenance lock") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(d.MaintenanceLock()); err == nil {
		t.Error("lock left")
	}
}

func TestVerifyContainerFlagsAndCleanup(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dk := withBackupDocker(t)
	run(t, "backup", "create")
	list, _ := os.ReadDir(filepath.Join(pathsOf(home).Data, "backups"))
	var path string
	for _, e := range list {
		if strings.HasSuffix(e.Name(), ".age") {
			path = filepath.Join(pathsOf(home).Data, "backups", e.Name())
		}
	}
	dk.failRestore = true
	dk.calls = nil
	if code, _, _ := run(t, "backup", "verify", path); code != output.Blocked {
		t.Errorf("a failed restore: exit %d", code)
	}
	var started, removed bool
	for _, c := range dk.calls {
		started = started || (strings.HasPrefix(c, "run -d --network none --shm-size 1g --name backsight-verify-"))
		removed = removed || strings.HasPrefix(c, "rm -f -v backsight-verify-")
	}
	if !started || !removed {
		t.Errorf("calls %q", dk.calls)
	}
	if ents, _ := os.ReadDir(filepath.Join(pathsOf(home).State, "staging")); len(ents) != 0 {
		t.Errorf("verify left files: %v", ents)
	}
}

// Importing a different identity would make the existing backups
// unreadable here: refused unless --force.
func TestIdentityImportRefusesToReplace(t *testing.T) {
	home := devInstall(t, true)
	first, _ := agelib.GenerateX25519Identity()
	second, _ := agelib.GenerateX25519Identity()
	_ = config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { return s.Set("backup_identity", first.String()) })
	before, _ := config.LoadSecrets(secretsFile(home))
	f := filepath.Join(t.TempDir(), "key.txt")
	_ = os.WriteFile(f, []byte(second.String()+"\n"), 0o600)
	if code, _, errOut := run(t, "backup", "identity", "import", f); code != output.Usage || !strings.Contains(errOut, "--force") {
		t.Errorf("exit %d %q", code, errOut)
	}
	if after, _ := config.LoadSecrets(secretsFile(home)); after.BackupIdentity != before.BackupIdentity {
		t.Error("replaced without --force")
	}
	if code, _, _ := run(t, "backup", "identity", "import", "--force", f); code != output.OK {
		t.Errorf("--force: exit %d", code)
	}
	if after, _ := config.LoadSecrets(secretsFile(home)); after.BackupIdentity != second.String() {
		t.Error("--force did not store the new identity")
	}
}

// A backup holds, beside the dump, the files the install is rebuilt from,
// as they were when it was taken.
func TestBackupArchiveHoldsTheInstallFiles(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, proxyHindsight(t, fakeHindsight(t), func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/default/banks/claude-memory/config" {
			_, _ = w.Write([]byte(`{"config":{"retain_mission":"keep the decisions"},"overrides":{}}`))
			return true
		}
		return false
	}))
	withBackupDocker(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	d := pathsOf(home)
	override := filepath.Join(composeDir(home), "compose.override.yaml")
	_ = os.MkdirAll(filepath.Dir(override), 0o700)
	_ = os.WriteFile(override, []byte("services:\n  hindsight:\n    mem_limit: 4g\n"), 0o600)
	_ = os.MkdirAll(filepath.Dir(d.StateFile()), 0o700)
	_ = os.WriteFile(d.StateFile(), []byte(`{"schema_version":1,"welcomed":true}`+"\n"), 0o600)
	if code, out, errOut := run(t, "backup", "create"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	sec, _ := config.LoadSecrets(secretsFile(home))
	id, err := agelib.ParseX25519Identity(sec.BackupIdentity)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	m, err := backup.Open(filepath.Join(d.Data, "backups", "backsight-20261025T040000Z.tar.gz.age"), []agelib.Identity{id}, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.Files, " "); got != "manifest.json db.dump bank-config.json compose.override.yaml config.toml secrets.toml state.json" {
		t.Errorf("manifest files: %s", got)
	}
	for name, src := range map[string]string{"config.toml": d.ConfigFile(), "secrets.toml": d.SecretsFile(), "state.json": d.StateFile(), "compose.override.yaml": override} {
		want, _ := os.ReadFile(src)
		if got, err := os.ReadFile(filepath.Join(out, name)); err != nil || len(want) == 0 || string(got) != string(want) {
			t.Errorf("%s: %v\ngot  %q\nwant %q", name, err, got, want)
		}
	}
	var bank map[string]any
	raw, _ := os.ReadFile(filepath.Join(out, "bank-config.json"))
	if err := json.Unmarshal(raw, &bank); err != nil || bank["retain_mission"] != "keep the decisions" {
		t.Errorf("bank-config.json: %v %s", err, raw)
	}
}

// backup verify records its outcome in state.json: doctor's
// reference-server check reads it (0067 block 4).
func TestBackupVerifyRecordsItsResultAndWhetherTheArchiveWasTheNewest(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dk := withBackupDocker(t)
	at := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	withClock(t, at)
	if code, out, errOut := run(t, "backup", "create"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	older := filepath.Join(pathsOf(home).Data, "backups", "backsight-20261025T040000Z.tar.gz.age")
	withClock(t, at.Add(24*time.Hour)) // a day later: pruning keeps one per day
	if code, out, errOut := run(t, "backup", "create"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	newest := filepath.Join(pathsOf(home).Data, "backups", "backsight-20261026T040000Z.tar.gz.age")

	withClock(t, at.Add(26*time.Hour))
	if code, out, errOut := run(t, "backup", "verify", older); code != output.OK {
		t.Fatalf("verify: exit %d\n%s%s", code, out, errOut)
	}
	st, _ := stateLoad(home)
	if v := st.BackupVerify; v == nil || v.Archive != older || !v.OK || v.Newest || !v.VerifiedAt.Equal(at.Add(26*time.Hour)) || !v.CreatedAt.Equal(at) {
		t.Fatalf("after verifying the older archive: %+v", v)
	}
	// A relative path names the same archive.
	t.Chdir(filepath.Dir(newest))
	if code, out, _ := run(t, "backup", "verify", filepath.Base(newest)); code != output.OK {
		t.Fatalf("verify: exit %d\n%s", code, out)
	}
	if st, _ = stateLoad(home); st.BackupVerify == nil || st.BackupVerify.Archive != newest || !st.BackupVerify.Newest || !st.BackupVerify.OK {
		t.Fatalf("after verifying the newest: %+v", st.BackupVerify)
	}
	// A mismatch replaces the pass: the last verify is what counts.
	dk.restored = "41"
	if code, _, _ := run(t, "backup", "verify", newest); code != output.Blocked {
		t.Fatalf("a short restore: exit %d", code)
	}
	if st, _ = stateLoad(home); st.BackupVerify == nil || st.BackupVerify.OK {
		t.Errorf("a failed verify left %+v", st.BackupVerify)
	}
}
