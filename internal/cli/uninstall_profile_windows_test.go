//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
)

// A recorded profile naming ~/.claude in another case is the default
// profile on Windows: claude acts on it without CLAUDE_CONFIG_DIR, and
// the plugin entry its own removal deletes is not put back.
func TestUninstallSeesTheDefaultProfileInAnotherCase(t *testing.T) {
	m := installMachine(t)
	d := pathsOf(m.home)
	other := filepath.Join(m.home, strings.ToUpper(".claude"))
	if err := state.Update(d, "test", func(s *state.State) error { s.Client.Profile = other; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.settings, []byte("{\n  \"enabledPlugins\": {\n    \"backsight@backsight\": true\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.claude.onRun = func(line, _ string) {
		if line == "plugin uninstall "+plugin.PluginID()+" --scope user" {
			_ = os.WriteFile(m.settings, []byte("{\n  \"enabledPlugins\": {}\n}\n"), 0o644)
		}
	}
	if code, _, out := uninstall(t); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := m.claude.profilesOf("plugin uninstall"); !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("plugin uninstall ran on %q, want ~/.claude without CLAUDE_CONFIG_DIR", got)
	}
	if _, there, _ := claudecfg.Get([]byte(readFile(t, m.settings)), []string{"enabledPlugins", plugin.PluginID()}); there {
		t.Errorf("the uninstalled profile's plugin entry was put back:\n%s", readFile(t, m.settings))
	}
}
