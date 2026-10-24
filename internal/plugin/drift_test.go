package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDrift(t *testing.T) {
	data := t.TempDir()
	settings := filepath.Join(t.TempDir(), "settings.json")
	target := filepath.Join(data, "bin", "v1", "backsight")
	_ = os.MkdirAll(filepath.Dir(target), 0o755)
	_ = os.WriteFile(target, []byte("bin"), 0o755)
	shim := filepath.Join(data, "bin", "current")
	if err := os.Symlink(target, shim); err != nil {
		t.Skip("no symlinks")
	}
	_ = os.WriteFile(settings, []byte(`{"permissions":{"allow":["`+strings.Join(ToolNames, `","`)+`"]}}`), 0o600)
	installed := Installed{Version: Version("1.2.3", shim), ShimTarget: target}
	if got := Drift(data, settings, installed, "1.2.3"); len(got) != 0 {
		t.Errorf("no drift expected: %v", got)
	}
	if got := Drift(data, settings, installed, "1.3.0"); len(got) != 1 || !strings.Contains(got[0], "plugin") {
		t.Errorf("binary updated, plugin not: %v", got)
	}
	_ = os.WriteFile(settings, []byte(`{"permissions":{"allow":["`+ToolNames[0]+`"]}}`), 0o600)
	if got := Drift(data, settings, installed, "1.2.3"); len(got) != 1 || !strings.Contains(got[0], "permission") {
		t.Errorf("permission rule incomplete: %v", got)
	}
	_ = os.Remove(shim)
	if got := Drift(data, settings, installed, "1.2.3"); len(got) != 2 || !strings.Contains(got[1], "shim") {
		t.Errorf("shim missing: %v", got)
	}
	if got := Drift(data, settings, Installed{}, "1.2.3"); len(got) != 0 {
		t.Errorf("never installed: no drift to report: %v", got)
	}
}
