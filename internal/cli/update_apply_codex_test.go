package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

// With Codex wired, update apply runs the new binary's client install
// --codex in the recorded home: the main plugin is added again, the hooks
// plugin (unchanged) is not, so the hooks stay trusted.
func TestUpdateApplyUpdatesCodex(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	f := withCodex(t, home)
	codexHomeWith(t, home, codexStart)
	installCodex(t)
	before, _ := state.Load(d.StateFile())
	f.calls = nil
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if bins.ran("0.2.0 client install --codex") != 1 {
		t.Errorf("binary calls %q", bins.calls)
	}
	if got := f.mutating(); !slices.Equal(got, []string{"plugin add backsight@backsight"}) {
		t.Errorf("codex calls %v", got)
	}
	st, _ := state.Load(d.StateFile())
	if c := st.ClientOf(harnessid.Codex); c == nil || c.PluginVersion == before.ClientOf(harnessid.Codex).PluginVersion || !strings.HasPrefix(c.PluginVersion, "0.2.0") {
		t.Errorf("Codex's version %+v", c)
	}
	if !strings.Contains(out, "client install --codex: plugins regenerated") {
		t.Errorf("no codex step: %s", out)
	}
}

// A failed Codex step rolls the update back, the previous binary wiring
// Codex again.
func TestUpdateApplyFailingCodexStepRollsBack(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	d := pathsOf(home)
	f := withCodex(t, home)
	codexHomeWith(t, home, codexStart)
	installCodex(t)
	old := shimOf(t, d)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	f.failOn, f.failOnce = "plugin add backsight@backsight", true
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	if code != output.RolledBack {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if shimOf(t, d) != old || bins.ran("0.1.0 client install --codex") != 1 {
		t.Errorf("shim %s, calls %q", shimOf(t, d), bins.calls)
	}
}

// The plan names the Codex step; a machine without Codex has none.
func TestUpdateApplyPlanNamesCodex(t *testing.T) {
	home, _, _ := installedAt(t, "0.1.0")
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	_, out, _ := run(t, "update", "apply", "--source", src, "--plan", "--json")
	if strings.Contains(out, `"name":"codex"`) {
		t.Errorf("a plan without Codex names it: %s", out)
	}
	withCodex(t, home)
	codexHomeWith(t, home, codexStart)
	installCodex(t)
	setBuild(t, "0.1.0", "abc1234")
	_, out, _ = run(t, "update", "apply", "--source", src, "--plan", "--json")
	if !strings.Contains(out, "client install --codex") {
		t.Errorf("plan: %s", out)
	}
}
