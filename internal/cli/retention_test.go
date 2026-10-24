package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

func cfgFile(home string) string { return filepath.Join(home, "backsight", "config", "config.toml") }

func retentionRun(t *testing.T, args ...string) (int, retentionResult, string) {
	t.Helper()
	code, out, _ := run(t, append([]string{"client", "retention", "--json"}, args...)...)
	var env struct {
		Data retentionResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	return code, env.Data, out
}

func TestRetentionDefaultRaisesTo3650AndRemembersIt(t *testing.T) {
	home, settings := clientHome(t, userSettings, 0)
	_ = os.WriteFile(cfgFile(home), []byte("# my config\n[server]\nbank = \"claude-memory\"\n"), 0o600)
	code, res, out := retentionRun(t)
	if code != output.OK || res.Target != 3650 || res.Before.EffectiveDays != 30 || res.Run == "" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(readFile(t, settings), `"cleanupPeriodDays": 3650`) {
		t.Error("settings not raised")
	}
	if got := readFile(t, cfgFile(home)); got != "# my config\n[server]\nbank = \"claude-memory\"\n\n[client]\nretention_days = 3650\n" {
		t.Errorf("config:\n%s", got)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.Client == nil || string(st.Client.RetentionBefore) != "30" {
		t.Errorf("retention_before %+v", st.Client)
	}
}

func TestRetentionDefaultLeavesAHigherValue(t *testing.T) {
	home, settings := clientHome(t, `{"cleanupPeriodDays": 9999}`, 0)
	code, res, out := retentionRun(t)
	if code != output.OK || res.Run != "" || readFile(t, settings) != `{"cleanupPeriodDays": 9999}` {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(readFile(t, cfgFile(home)), "retention_days = 3650") {
		t.Error("the floor was not recorded")
	}
}

func TestRetentionDaysSetsExactlyEvenLower(t *testing.T) {
	_, settings := clientHome(t, `{"cleanupPeriodDays": 3650}`, 3650)
	code, _, out := retentionRun(t, "--days", "90")
	if code != output.OK || readFile(t, settings) != `{"cleanupPeriodDays": 90}` {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "client", "config", "apply"); code != output.OK || readFile(t, settings) != `{"cleanupPeriodDays": 90}` {
		t.Error("a later apply moved the explicit value")
	}
}

func TestRetentionKeepStopsManagingWithoutTouchingTheFile(t *testing.T) {
	home, settings := clientHome(t, userSettings, 3650)
	code, res, out := retentionRun(t, "--keep")
	if code != output.OK || res.Run != "" || readFile(t, settings) != userSettings || strings.Contains(readFile(t, cfgFile(home)), "retention_days") {
		t.Fatalf("exit %d\n%s\nconfig: %s", code, out, readFile(t, cfgFile(home)))
	}
}

func TestRetentionPlanWritesNothing(t *testing.T) {
	home, settings := clientHome(t, userSettings, 0)
	code, res, out := retentionRun(t, "--plan")
	if code != output.OK || len(res.Files) != 1 || len(res.Files[0].Changes) != 1 || !res.ConfigChange {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if readFile(t, settings) != userSettings {
		t.Error("plan wrote settings")
	}
	if _, err := os.Stat(cfgFile(home)); !os.IsNotExist(err) {
		t.Error("plan wrote the config")
	}
	if _, err := os.Stat(pathsOf(home).PreImagesDir()); !os.IsNotExist(err) {
		t.Error("plan made a run")
	}
}

func TestRetentionCreatesAMinimalConfig(t *testing.T) {
	home, _ := clientHome(t, userSettings, 0)
	if code, _, out := retentionRun(t); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := readFile(t, cfgFile(home)); got != "schema_version = 1\n\n[client]\nretention_days = 3650\n" {
		t.Errorf("config:\n%s", got)
	}
	if fi, _ := os.Stat(cfgFile(home)); fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v", fi.Mode().Perm())
	}
}

func TestRetentionReportsAManagedValueAndWritesNothing(t *testing.T) {
	home, settings := clientHome(t, userSettings, 0)
	_ = os.MkdirAll(filepath.Join(home, "etc"), 0o700)
	_ = os.WriteFile(filepath.Join(home, "etc", "managed-settings.json"), []byte(`{"cleanupPeriodDays": 14}`), 0o600)
	code, res, out := retentionRun(t)
	if code != output.OK || res.Before.Source != "managed" || len(res.Notes) == 0 || readFile(t, settings) != userSettings {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(cfgFile(home)); !os.IsNotExist(err) {
		t.Error("config written despite the managed value")
	}
}

func TestRetentionReportsTranscripts(t *testing.T) {
	home, _ := clientHome(t, userSettings, 0)
	p := filepath.Join(home, ".claude", "projects", "-home-u-x", "s.jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte("{}\n"), 0o600)
	code, res, out := retentionRun(t, "--plan")
	if code != output.OK || res.Transcripts.Count != 1 || res.Transcripts.Oldest == "" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestRetentionRefusesBadFlags(t *testing.T) {
	clientHome(t, userSettings, 0)
	for _, args := range [][]string{{"--days", "0"}, {"--days", "-5"}, {"--days", "90", "--keep"}} {
		if code, _, out := retentionRun(t, args...); code != output.Usage {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
}

func TestRetentionRefusesAConfigItCannotRead(t *testing.T) {
	home, settings := clientHome(t, userSettings, 0)
	_ = os.WriteFile(cfgFile(home), []byte("[client\n"), 0o600)
	if code, _, out := retentionRun(t); code != output.Usage || readFile(t, settings) != userSettings {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestRetentionDaysIsWrittenToTheConfig(t *testing.T) {
	home, _ := clientHome(t, userSettings, 3650)
	retentionRun(t, "--days", "90")
	if got := readFile(t, cfgFile(home)); got != "[client]\nretention_days = 90\n" {
		t.Errorf("config %q", got)
	}
}

func TestRetentionHonoursConfigDir(t *testing.T) {
	home, settings := clientHome(t, userSettings, 0)
	other := filepath.Join(home, "profile-dev")
	_ = os.MkdirAll(other, 0o700)
	_ = os.WriteFile(filepath.Join(other, "settings.json"), []byte(`{"cleanupPeriodDays": 7}`), 0o600)
	code, res, out := retentionRun(t, "--config-dir", other)
	if code != output.OK || res.Before.EffectiveDays != 7 || readFile(t, settings) != userSettings ||
		readFile(t, filepath.Join(other, "settings.json")) != `{"cleanupPeriodDays": 3650}` {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestRetentionRefusesAConfigFormItCannotEdit(t *testing.T) {
	home, settings := clientHome(t, userSettings, 0)
	_ = os.WriteFile(cfgFile(home), []byte("client = { retention_days = 30 }\n"), 0o600)
	if code, _, out := retentionRun(t); code != output.Usage || readFile(t, settings) != userSettings {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestRetentionTextShowsThePathAndErrors(t *testing.T) {
	_, settings := clientHome(t, userSettings, 0)
	code, out, _ := run(t, "client", "retention", "--plan")
	if code != output.OK || !strings.Contains(out, "cleanupPeriodDays: 30 → 3650") {
		t.Errorf("exit %d\n%s", code, out)
	}
	_ = os.WriteFile(settings, []byte(`{"a": 1,}`), 0o600)
	code, _, errOut := run(t, "client", "retention")
	if code != output.Blocked || !strings.Contains(errOut, "fix the file by hand") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

// The user's value is recorded before Claude Code's file is written: if the
// state cannot be updated, nothing is written (M1 review).
func TestRetentionRecordsTheOldValueBeforeWriting(t *testing.T) {
	home, settings := clientHome(t, userSettings, 0)
	d := pathsOf(home)
	_ = os.MkdirAll(d.LocksDir(), 0o700)
	lock, err := state.TryAcquire(filepath.Join(d.LocksDir(), "state.lock"), "test")
	if err != nil {
		t.Fatal(err)
	}
	code, _, out := retentionRun(t)
	_ = lock.Release()
	if code == output.OK || readFile(t, settings) != userSettings {
		t.Fatalf("exit %d, settings written while the state was locked\n%s", code, out)
	}
	if code, _, out := retentionRun(t); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	st, _ := state.Load(d.StateFile())
	if st.Client == nil || string(st.Client.RetentionBefore) != "30" {
		t.Errorf("retention_before %+v", st.Client)
	}
}
