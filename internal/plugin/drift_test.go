package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/testtool"
)

func TestDrift(t *testing.T) {
	testtool.SkipWindows(t, "Task 4: the shim check (a symlink branch only an admin runner takes, shim named current.exe)")
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

func TestDriftShimTargetsAndSettingsFiles(t *testing.T) {
	testtool.SkipWindows(t, "Task 4: the shim check (a symlink branch only an admin runner takes, shim named current.exe)")
	data := t.TempDir()
	mk := func(v string) string {
		p := filepath.Join(data, "bin", v, "backsight")
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(v), 0o755)
		return p
	}
	v1, v2 := mk("v1"), mk("v2")
	shim := filepath.Join(data, "bin", "current")
	if err := os.Symlink(v2, shim); err != nil {
		t.Skip("no symlinks")
	}
	settings := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(settings, []byte(`{"permissions":{"allow":["`+strings.Join(ToolNames, `","`)+`"]}}`), 0o600)
	in := Installed{Version: Version("1.2.3", shim), ShimTarget: v1}
	if got := Drift(data, settings, in, "1.2.3"); len(got) != 1 || !strings.Contains(got[0], "shim points to") {
		t.Errorf("shim on another version: %v", got)
	}
	in.ShimTarget = ""
	if got := Drift(data, settings, in, "1.2.3"); len(got) != 0 {
		t.Errorf("no recorded target: %v", got)
	}
	// a data directory reached through a symlink resolves to the same target
	link := filepath.Join(t.TempDir(), "data-link")
	_ = os.Symlink(data, link)
	in.ShimTarget = filepath.Join(link, "bin", "v2", "backsight")
	if got := Drift(data, settings, in, "1.2.3"); len(got) != 0 {
		t.Errorf("symlinked data dir: %v", got)
	}
	for _, doc := range []string{"", "{not json"} {
		_ = os.WriteFile(settings, []byte(doc), 0o600)
		if got := Drift(data, settings, in, "1.2.3"); len(got) != 1 || !strings.Contains(got[0], "permission") {
			t.Errorf("settings %q: %v", doc, got)
		}
	}
	_ = os.Remove(settings)
	if got := Drift(data, settings, in, "1.2.3"); len(got) != 1 {
		t.Errorf("settings missing: %v", got)
	}
}
