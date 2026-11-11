package cli

import (
	"context"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/report"
	"github.com/Barben360/backsight/internal/state"
)

// withCodexProbes has doctor ask the (fake) codex command and this
// binary's memory server, as the real probes do.
func withCodexProbes(t *testing.T) {
	t.Helper()
	old := probes
	probes = func(e *doctor.Env) {
		old(e)
		e.CodexRun = toolRunner(harnessid.Codex)
		e.CodexTools = func(ctx context.Context) (map[string]map[string]any, error) {
			return servedAnnotations(ctx, e.Dirs, e.Home)
		}
	}
	t.Cleanup(func() { probes = old })
}

// client install --codex records the program behind the shim and when it
// wired Codex; doctor then finds Codex wired as install left it: its
// version, both plugins, the files, the served tools' hints and the shim's
// program all pass.
func TestDoctorOnACodexInstall(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	withCodex(t, home)
	codexHomeWith(t, home, codexStart)
	before := time.Now().Add(-time.Second)
	installCodex(t)
	d := pathsOf(home)
	st, _ := state.Load(d.StateFile())
	c := st.ClientOf(harnessid.Codex)
	sum, err := fsutil.HashFile(filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS)))
	if err != nil || sum == "" || c.ShimSHA256 != sum || c.WiredAt.Before(before) {
		t.Fatalf("recorded %+v, shim's sha256 %s %v", c, sum, err)
	}
	withCodexProbes(t)
	_, checks := doctorChecks(t)
	for _, id := range []string{"codex", "codex-plugins", "codex-drift", "codex-tools", "codex-memories", "codex-mcp-name", "codex-shim"} {
		if got := checks[id]; got.Status != doctor.OK {
			t.Errorf("%s: %s %s (%s)", id, got.Status, got.Summary, got.Detail)
		}
	}
	// The hooks are not trusted yet: the user reviews them in Codex.
	if got := checks["codex-hooks"]; got.Status != doctor.Warn {
		t.Errorf("codex-hooks: %s %s", got.Status, got.Summary)
	}
}

// The server doctor lists for Codex is this binary's own: its write tools
// carry the hints Codex runs them with (a server that lost them fails the
// check).
func TestServedAnnotationsAreTheServersOwn(t *testing.T) {
	home := devInstall(t, true)
	got, err := servedAnnotations(context.Background(), pathsOf(home), home)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"memory_store", "memory_credit"} {
		if got[name]["destructiveHint"] != false || got[name]["openWorldHint"] != false {
			t.Errorf("%s: %v", name, got[name])
		}
	}
	for _, name := range []string{"memory_search", "memory_read"} {
		if got[name]["readOnlyHint"] != true {
			t.Errorf("%s: %v", name, got[name])
		}
	}
}

// report names Codex once it is wired: the codex command's version, the
// plugin's, and Codex's paths, a home outside the user's scrubbed as
// <codex>; the summary line names Codex's version. Without Codex wired,
// the diagnostic has no Codex section.
func TestReportNamesCodexScrubbed(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	withCodex(t, home)
	reportJSON := func() report.Report {
		t.Helper()
		code, out, _ := run(t, "report", "--json")
		if code != output.OK {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		var r report.Report
		jsonData(t, out, &r)
		return r
	}
	if r := reportJSON(); r.Diagnostic.Codex != nil || strings.Contains(r.Paste, `"codex"`) {
		t.Errorf("Codex not wired, yet %+v", r.Diagnostic.Codex)
	}
	outside := t.TempDir()
	installCodex(t, "--codex-home", outside)
	r := reportJSON()
	c := r.Diagnostic.Codex
	sep := string(filepath.Separator)
	if c == nil || c.Version != "0.167.0" || c.Plugin == "" || c.Home != "<codex>" || c.Config != "<codex>"+sep+"config.toml" ||
		c.Sessions != "<codex>"+sep+"sessions" || strings.Contains(c.Marketplace, home) || !strings.HasSuffix(c.Marketplace, sep+"codex-marketplace") {
		t.Fatalf("codex section %+v", c)
	}
	if strings.Contains(r.Paste, outside) || strings.Contains(r.IssueURL, url.QueryEscape(outside)) {
		t.Error("the Codex home is in the report")
	}
	u, _ := url.Parse(r.IssueURL)
	if body := u.Query().Get("body"); !strings.Contains(body, "Codex 0.167.0 (plugin "+c.Plugin+")") {
		t.Errorf("summary:\n%s", body)
	}
}

// A Codex SessionStart records the hooks' heartbeat for Codex, with its
// session; a subagent's does not, nor does Claude Code's.
func TestCodexSessionStartRecordsTheHeartbeat(t *testing.T) {
	home := hookHome(t)
	d := pathsOf(home)
	start := time.Now().Add(-time.Second)
	cxHook(t, harness.EventSessionStart, cxPayload(t, home, "SessionStart.startup", map[string]any{"agent_id": "01a1c72b-ae08-75b3-a4db-8a06f93c0c83"}))
	if r, ok := state.LastHookRun(d, harnessid.Codex); ok {
		t.Errorf("a subagent's start recorded %+v", r)
	}
	cxHook(t, harness.EventSessionStart, cxPayload(t, home, "SessionStart.startup", nil))
	r, ok := state.LastHookRun(d, harnessid.Codex)
	if !ok || r.Session != cxRoot || r.At.Before(start) {
		t.Errorf("heartbeat %+v %v", r, ok)
	}
	if r, ok := state.LastHookRun(d, harnessid.ClaudeCode); ok {
		t.Errorf("Claude Code's heartbeat %+v", r)
	}
}
