package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
)

// surfaceRT is a runtime that is not Docker: no compose.yaml anywhere,
// every answer the test's; whatever it does not set reaches the refusing
// Docker of TestMain.
type surfaceRT struct {
	server.Runtime
	deployed    string
	secretFiles []string
	db          *surfaceDB
	renames     []string
}

func (r *surfaceRT) Deployed() (string, error) { return r.deployed, nil }
func (r *surfaceRT) SecretFiles() []string     { return r.secretFiles }
func (r *surfaceRT) Database() server.Database { return r.db }
func (r *surfaceRT) RenameBank(_ context.Context, from, to string, dryRun bool) ([]byte, error) {
	r.renames = append(r.renames, strings.Join([]string{from, to, map[bool]string{true: "dry", false: "real"}[dryRun]}, " "))
	return nil, nil
}

type surfaceDB struct {
	server.Database
	banks   []string
	pending []string
}

func (d *surfaceDB) BankIDs(context.Context) ([]string, error) { return slices.Clone(d.banks), nil }

func (d *surfaceDB) PendingOperations(_ context.Context, banks []string) (int, error) {
	d.pending = append(d.pending, strings.Join(banks, ","))
	return 0, nil
}

// withRuntime makes rt the runtime of every config, own group or not.
func withRuntime(t *testing.T, rt *surfaceRT) {
	t.Helper()
	old := runtimeWith
	runtimeWith = func(cfg config.Config, _ bool) server.Runtime {
		if rt.Runtime == nil {
			rt.Runtime = server.Docker{Compose: composeFor(cfg), Cfg: cfg}
		}
		return rt
	}
	t.Cleanup(func() { runtimeWith = old })
}

// update apply asks the runtime what it deployed, not compose.yaml: a
// native server has none.
func TestUpdateApplyReadsDeployedThroughTheRuntime(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Server.ComposeDir, "compose.yaml")); err == nil {
		t.Fatal("the fixture has a compose.yaml")
	}
	rt := &surfaceRT{deployed: "1.2.0-0123456789ab"}
	withRuntime(t, rt)
	sp := serverPlan{Current: "1.1.0-0123456789ab", Target: "1.2.0-0123456789ab", Upgrade: true}
	if why := serverStepSkipped(d, cfg); why != "" {
		t.Errorf("a deployed server skipped: %q", why)
	}
	if !upgradePassed(d, cfg, sp) {
		t.Error("the server runs the target, yet the upgrade did not pass")
	}
	rt.deployed = sp.Current // a rollback put the old version back
	if upgradePassed(d, cfg, sp) {
		t.Error("the server runs the old version, yet the upgrade passed")
	}
	rt.deployed = ""
	if why := serverStepSkipped(d, cfg); why != "no server deployed on this machine" {
		t.Errorf("nothing deployed: %q", why)
	}
}

// bank rename lists, waits for and renames the banks through the runtime.
func TestBankRenameGoesThroughTheRuntime(t *testing.T) {
	home := legacyInstall(t)
	pointAt(t, home, fakeHindsight(t))
	rt := &surfaceRT{db: &surfaceDB{banks: []string{"claude-memory", "other"}}}
	withRuntime(t, rt)
	code, out, errOut := run(t, "bank", "rename", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if want := []string{"claude-memory backsight dry", "claude-memory backsight real"}; !slices.Equal(rt.renames, want) {
		t.Errorf("renames %q, want %q", rt.renames, want)
	}
	if want := []string{"claude-memory"}; !slices.Equal(rt.db.pending, want) {
		t.Errorf("pending asked for %q, want %q", rt.db.pending, want)
	}
}

// The report scrubs every value of the files the runtime says hold the
// server's keys, wherever they are.
func TestReportRedactsTheRuntimesSecretFiles(t *testing.T) {
	home := isolate(t)
	withClaude(t)
	d := pathsOf(home)
	// Shorter than redact.MinSecret (8), "s3cret" alone is never scrubbed:
	// a longer value that starts with it, low in entropy for secret scanners.
	const planted = "s3cretzebrazebra"
	env := filepath.Join(t.TempDir(), "server.env")
	if err := os.WriteFile(env, []byte("KEY='"+planted+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withRuntime(t, &surfaceRT{secretFiles: []string{env}})
	_ = os.MkdirAll(d.Config, 0o700)
	_ = os.WriteFile(d.ConfigFile(), []byte("[server]\nurl = \"http://127.0.0.1:"+itoa(closedPort(t))+"\"\nui_port = "+itoa(closedPort(t))+"\n"), 0o600)
	now := time.Now().UTC()
	raw, _ := json.Marshal(map[string]any{"ts": now.Add(-time.Minute).Format(time.RFC3339Nano), "event": "dial_failed", "error": "dial " + planted + ": refused"})
	_ = os.MkdirAll(d.TelemetryDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.TelemetryDir(), "events-"+now.Format("2006-01")+".jsonl"), append(raw, '\n'), 0o600)
	assertScrubbed(t, map[string]string{"server key": planted}, []string{"dial [redacted:secret]: refused"})
}
