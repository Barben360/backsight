package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
)

// backupDocker answers the commands a backup and a verify run: psql values,
// pg_dump and pg_restore, compose cp (writes the dump), and the throwaway
// container of verify.
type backupDocker struct {
	mu       sync.Mutex
	calls    []string
	restored string // count the throwaway database returns
	failDump bool
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
		return nil, os.WriteFile(args[len(args)-1], []byte("PGDMP fake"), 0o600)
	case strings.Contains(line, "alembic_version"):
		return []byte("c7e2f9a4d1b8\n"), nil
	case strings.Contains(line, "pg_extension"):
		return []byte("0.8.7\n"), nil
	case strings.HasPrefix(line, "compose") && strings.Contains(line, "count(*)"):
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
	old := stdoutIsTerminal
	t.Cleanup(func() { stdoutIsTerminal = old })
	stdoutIsTerminal = func() bool { return false }
	if code, out, _ := run(t, "backup", "identity", "show"); code != output.NeedsUser || strings.Contains(out, "AGE-SECRET-KEY") {
		t.Errorf("piped: exit %d\n%s", code, out)
	}
	stdoutIsTerminal = func() bool { return true }
	if code, out, _ := run(t, "backup", "identity", "show", "--json"); code != output.NeedsUser || strings.Contains(out, "AGE-SECRET-KEY") {
		t.Errorf("json: exit %d\n%s", code, out)
	}
	code, out, _ := run(t, "backup", "identity", "show")
	if code != output.OK || !strings.Contains(out, "AGE-SECRET-KEY-") {
		t.Fatalf("terminal: exit %d\n%s", code, out)
	}
	key := strings.Fields(out)[0]
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
	withBackupDocker(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	if code, out, _ := run(t, "jobs", "maintain"); code != output.OK || !strings.Contains(out, "done:") || !strings.Contains(out, "backsight-20261025T040000Z.tar.gz.age") {
		t.Fatalf("exit %d\n%s", code, out)
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
	if code, out, _ := run(t, "jobs", "maintain"); code != output.OK || !strings.Contains(out, "backup off") {
		t.Errorf("exit %d\n%s", code, out)
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
