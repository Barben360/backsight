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

// clientHome is an isolated machine with a Claude Code settings file and a
// backsight config asking for retention days (0: not managed).
func clientHome(t *testing.T, settings string, days int) (home, settingsPath string) {
	t.Helper()
	home = isolate(t)
	settingsPath = filepath.Join(home, ".claude", "settings.json")
	_ = os.MkdirAll(filepath.Dir(settingsPath), 0o700)
	if settings != "" {
		if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	_ = os.MkdirAll(filepath.Dir(cfg), 0o700)
	if days > 0 {
		_ = os.WriteFile(cfg, []byte("[client]\nretention_days = "+itoa(days)+"\n"), 0o600)
	}
	return home, settingsPath
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const userSettings = "{\n  \"model\": \"opus\",\n  \"cleanupPeriodDays\": 30\n}\n"

func TestClientConfigPlanThenApplyThenNothing(t *testing.T) {
	home, settings := clientHome(t, userSettings, 3650)
	code, out, _ := run(t, "client", "config", "plan")
	if code != output.OK || !strings.Contains(out, "cleanupPeriodDays: 30 → 3650") || readFile(t, settings) != userSettings {
		t.Fatalf("plan: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "client", "config", "apply", "--json")
	var env struct {
		OK   bool               `json:"ok"`
		Data clientConfigResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || !env.OK || env.Data.Run == "" || len(env.Data.Files) != 1 || !env.Data.Files[0].Written {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	if got := readFile(t, settings); got != strings.Replace(userSettings, ": 30", ": 3650", 1) {
		t.Errorf("settings:\n%s", got)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.Client == nil || string(st.Client.RetentionBefore) != "30" {
		t.Errorf("retention_before not recorded: %+v", st.Client)
	}
	code, out, _ = run(t, "client", "config", "apply")
	if code != output.OK || !strings.Contains(out, "nothing to change") {
		t.Errorf("second apply: exit %d\n%s", code, out)
	}
}

func TestClientConfigNeverLowersAHigherRetention(t *testing.T) {
	_, settings := clientHome(t, "{\"cleanupPeriodDays\": 9999}", 3650)
	code, out, _ := run(t, "client", "config", "apply")
	if code != output.OK || !strings.Contains(out, "nothing to change") || readFile(t, settings) != "{\"cleanupPeriodDays\": 9999}" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestClientConfigWithoutRetentionManagedChangesNothing(t *testing.T) {
	_, settings := clientHome(t, userSettings, 0)
	code, out, _ := run(t, "client", "config", "apply")
	if code != output.OK || !strings.Contains(out, "nothing to change") || readFile(t, settings) != userSettings {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestClientConfigRecordsAnAbsentKeyAsNull(t *testing.T) {
	home, _ := clientHome(t, "{}", 3650)
	run(t, "client", "config", "apply")
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.Client == nil || string(st.Client.RetentionBefore) != "null" {
		t.Errorf("want null, got %+v", st.Client)
	}
	// a later change keeps the first recorded value
	_ = os.WriteFile(filepath.Join(home, "backsight", "config", "config.toml"), []byte("[client]\nretention_days = 7300\n"), 0o600)
	run(t, "client", "config", "apply")
	st, _ = state.Load(pathsOf(home).StateFile())
	if string(st.Client.RetentionBefore) != "null" {
		t.Errorf("first value overwritten: %s", st.Client.RetentionBefore)
	}
}

func TestClientConfigLeavesAManagedRetentionAlone(t *testing.T) {
	home, settings := clientHome(t, userSettings, 3650)
	_ = os.MkdirAll(filepath.Join(home, "etc"), 0o700)
	_ = os.WriteFile(filepath.Join(home, "etc", "managed-settings.json"), []byte(`{"cleanupPeriodDays": 14}`), 0o600)
	code, out, _ := run(t, "client", "config", "apply")
	if code != output.OK || !strings.Contains(out, "managed setting forces cleanupPeriodDays = 14") || readFile(t, settings) != userSettings {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestClientConfigRefusesInvalidSettings(t *testing.T) {
	_, settings := clientHome(t, `{"model": "opus",}`, 3650)
	code, out, _ := run(t, "client", "config", "apply", "--json")
	if code != output.Blocked || !strings.Contains(out, "fix the file by hand") || readFile(t, settings) != `{"model": "opus",}` {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestClientConfigHonoursConfigDir(t *testing.T) {
	home, settings := clientHome(t, userSettings, 3650)
	other := filepath.Join(home, "profile-dev")
	_ = os.MkdirAll(other, 0o700)
	code, _, _ := run(t, "client", "config", "apply", "--config-dir", other)
	if code != output.OK || readFile(t, settings) != userSettings {
		t.Fatalf("exit %d or the default profile was touched", code)
	}
	if !strings.Contains(readFile(t, filepath.Join(other, "settings.json")), `"cleanupPeriodDays": 3650`) {
		t.Error("--config-dir profile not edited")
	}
}

func TestClientConfigRestoreUndoesTheLastRun(t *testing.T) {
	_, settings := clientHome(t, userSettings, 3650)
	run(t, "client", "config", "apply")
	edited := readFile(t, settings)
	code, out, _ := run(t, "client", "config", "restore", "--json")
	var env struct {
		Data clientRestoreResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || readFile(t, settings) != userSettings || env.Data.Run == "" || env.Data.Run == env.Data.Restored {
		t.Fatalf("restore: exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "client", "config", "restore", env.Data.Run); code != output.OK || readFile(t, settings) != edited {
		t.Errorf("undoing the restore: exit %d", code)
	}
}

func TestClientConfigRestoreRefusesALaterHandEdit(t *testing.T) {
	_, settings := clientHome(t, userSettings, 3650)
	run(t, "client", "config", "apply")
	_ = os.WriteFile(settings, []byte(`{"model": "sonnet"}`), 0o644)
	code, out, _ := run(t, "client", "config", "restore")
	if code != output.Blocked || !strings.Contains(out, "refused") || readFile(t, settings) != `{"model": "sonnet"}` {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "client", "config", "restore", "--force"); code != output.OK || readFile(t, settings) != userSettings {
		t.Errorf("--force: exit %d", code)
	}
}

func TestClientConfigRestoreWithNoRun(t *testing.T) {
	clientHome(t, userSettings, 3650)
	if code, _, _ := run(t, "client", "config", "restore"); code != output.Blocked {
		t.Errorf("exit %d", code)
	}
}

func TestClientConfigRestorePlanWritesNothing(t *testing.T) {
	_, settings := clientHome(t, userSettings, 3650)
	run(t, "client", "config", "apply")
	edited := readFile(t, settings)
	code, out, _ := run(t, "client", "config", "restore", "--plan")
	if code != output.OK || !strings.Contains(out, "would restore") || readFile(t, settings) != edited {
		t.Fatalf("exit %d\n%s", code, out)
	}
	_ = os.WriteFile(settings, []byte(`{}`), 0o644)
	if code, out, _ := run(t, "client", "config", "restore", "--plan"); code != output.Blocked || !strings.Contains(out, "refused") {
		t.Errorf("plan over a hand edit: exit %d\n%s", code, out)
	}
}

func TestClientConfigRestoreTextExplainsAMissingRun(t *testing.T) {
	clientHome(t, userSettings, 3650)
	code, _, errOut := run(t, "client", "config", "restore")
	if code != output.Blocked || !strings.Contains(errOut, "no config editor run") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

func TestClientConfigAcceptsAnEmptySettingsFile(t *testing.T) {
	_, settings := clientHome(t, "", 3650)
	_ = os.WriteFile(settings, nil, 0o600)
	if code, out, errOut := run(t, "client", "config", "apply"); code != output.OK || !strings.Contains(readFile(t, settings), `"cleanupPeriodDays": 3650`) {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}
