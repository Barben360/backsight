package cli

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/migrate"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// Each --plan below must leave the install as it found it: treeOf and
// sameTree (uninstall_test.go) compare every file and link under the home.
// Each test then runs the command for real and sees it change the tree,
// so an empty or unrelated snapshot cannot pass.

func treeChanged(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	if maps.Equal(before, after) {
		t.Errorf("%s left the tree as it was: the snapshot misses its changes", what)
	}
}

func TestUpdateMigratePlanRunsNothing(t *testing.T) {
	home := devInstall(t, true)
	ran := false
	old := migrations
	migrations = func() []migrate.Migration {
		return []migrate.Migration{{ID: "0001-x", Run: func(context.Context, migrate.Env) error { ran = true; return nil }}}
	}
	t.Cleanup(func() { migrations = old })
	before := treeOf(t, home)
	code, out, _ := run(t, "update", "migrate", "--plan", "--json")
	if code != output.OK || !strings.Contains(out, `"0001-x"`) || ran {
		t.Errorf("exit %d, ran %v\n%s", code, ran, out)
	}
	sameTree(t, before, treeOf(t, home))
	if st, _ := state.Load(pathsOf(home).StateFile()); len(st.AppliedMigrations) != 0 {
		t.Errorf("recorded as applied: %v", st.AppliedMigrations)
	}
	if code, out, _ := run(t, "update", "migrate", "--json"); code != output.OK || !ran {
		t.Fatalf("migrate: exit %d, ran %v\n%s", code, ran, out)
	}
	treeChanged(t, "update migrate", before, treeOf(t, home))
}

func TestJobsUninstallPlanChangesNothing(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "linux")
	if code, out, _ := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	sys.calls = nil
	before := treeOf(t, home)
	code, out, _ := run(t, "jobs", "uninstall", "--plan")
	if code != output.OK || !strings.Contains(out, "disable --now backsight-tick.timer") {
		t.Errorf("plan: exit %d\n%s", code, out)
	}
	// Right after the plan, before anything else runs.
	if len(sys.calls) != 0 {
		t.Errorf("the plan ran %q", sys.calls)
	}
	sameTree(t, before, treeOf(t, home))
	if _, err := os.Stat(filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")); err != nil {
		t.Errorf("the timer is gone: %v", err)
	}
	if code, out, _ := run(t, "jobs", "uninstall"); code != output.OK || len(sys.calls) == 0 {
		t.Fatalf("uninstall: exit %d, calls %q\n%s", code, sys.calls, out)
	}
	treeChanged(t, "jobs uninstall", before, treeOf(t, home))
}

// bank apply --plan never calls the server: neither for a bank that does
// not exist yet nor for a template that changed since it was applied.
func TestBankApplyPlanSendsNothing(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)

	before := treeOf(t, home)
	code, out, _ := run(t, "bank", "apply", "--plan", "--json")
	if code != output.OK || !strings.Contains(out, `"would_send": true`) {
		t.Errorf("new bank: exit %d\n%s", code, out)
	}
	if len(bankCalls) != 0 {
		t.Errorf("new bank: the plan called the server: %q", bankCalls)
	}
	sameTree(t, before, treeOf(t, home))

	if code, out, _ := run(t, "bank", "apply", "--json"); code != output.OK || !strings.Contains(out, `"created": true`) {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	_ = state.Update(d, "test", func(s *state.State) error { s.BankConfigHash = "older-template"; return nil })
	bankCalls = nil
	before = treeOf(t, home)
	code, out, _ = run(t, "bank", "apply", "--plan", "--json")
	if code != output.OK || !strings.Contains(out, `"would_send": true`) {
		t.Errorf("changed template: exit %d\n%s", code, out)
	}
	for _, c := range bankCalls {
		t.Errorf("changed template: the plan called the server: %s", c)
	}
	sameTree(t, before, treeOf(t, home))
	if st, _ := state.Load(d.StateFile()); st.BankConfigHash != "older-template" {
		t.Errorf("hash %q recorded", st.BankConfigHash)
	}
	if code, out, _ := run(t, "bank", "apply", "--json"); code != output.OK || !strings.Contains(out, `"sent": true`) || !slices.ContainsFunc(bankCalls, func(c string) bool { return !strings.HasPrefix(c, "GET ") }) {
		t.Fatalf("apply: exit %d, calls %q\n%s", code, bankCalls, out)
	}
	treeChanged(t, "bank apply", before, treeOf(t, home))
	if st, _ := state.Load(d.StateFile()); st.BankConfigHash != server.TemplateHash() {
		t.Errorf("apply recorded %q", st.BankConfigHash)
	}
}

// server render --plan writes neither file, the first time or when both
// would change.
func TestServerRenderPlanChangesNoFile(t *testing.T) {
	home := devInstall(t, true)
	before := treeOf(t, home)
	code, out, _ := run(t, "server", "render", "--plan")
	if code != output.OK || strings.Count(out, "would be written") != 2 || strings.Contains(out, "fakellmkey") {
		t.Errorf("first render: exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, home))
	for _, f := range []string{"compose.yaml", ".env"} {
		if _, err := os.Stat(filepath.Join(composeDir(home), f)); err == nil {
			t.Errorf("--plan wrote %s", f)
		}
	}

	if code, out, _ := run(t, "server", "render"); code != output.OK {
		t.Fatalf("render: exit %d\n%s", code, out)
	}
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(raw), "ui_port = 19999", "ui_port = 19998", 1)), 0o600)
	_ = config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { return s.Set("llm_api_key", "otherllmkey") })
	before = treeOf(t, home)
	code, out, _ = run(t, "server", "render", "--plan")
	if code != output.OK || strings.Count(out, "would be written") != 2 || strings.Contains(out, "llmkey") {
		t.Errorf("changed render: exit %d\n%s", code, out)
	}
	sameTree(t, before, treeOf(t, home))
	if code, out, _ := run(t, "server", "render"); code != output.OK {
		t.Fatalf("render: exit %d\n%s", code, out)
	}
	after := treeOf(t, home)
	for _, f := range []string{"compose.yaml", ".env"} {
		if p := filepath.Join(composeDir(home), f); after[p] == before[p] {
			t.Errorf("render left %s as it was: the change does not reach it", f)
		}
	}
}
