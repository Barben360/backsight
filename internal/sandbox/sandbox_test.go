//go:build sandbox

// Package sandbox runs the client against a real Hindsight started by
// scripts/sandbox.sh (SPEC §16): what fakes cannot prove, the server's
// actual answers to backsight's requests.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/cli"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
)

func setup(t *testing.T) (*hindsight.Client, config.Config, string) {
	t.Helper()
	bh := os.Getenv("SANDBOX_HOME")
	if bh == "" {
		t.Skip("SANDBOX_HOME not set: run scripts/sandbox.sh")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BACKSIGHT_HOME", bh)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	d, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil {
		t.Fatal(err)
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return cl, cfg, home
}

func run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Execute(append(args, "--json"), &out, &errb)
	return code, out.String() + errb.String()
}

// document waits for an async retain to land.
func document(t *testing.T, cl *hindsight.Client, bank, id string) hindsight.Document {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		doc, err := cl.GetDocument(context.Background(), bank, id)
		if err == nil {
			return doc
		}
		var apiErr *hindsight.APIError
		if !errors.As(err, &apiErr) || apiErr.Kind != hindsight.NotFound || time.Now().After(deadline) {
			t.Fatalf("document %s: %v", id, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func TestBankTemplateIsAppliedAndStable(t *testing.T) {
	cl, cfg, _ := setup(t)
	ov, err := cl.BankConfigOverrides(context.Background(), cfg.Server.Bank)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ov["retain_strategies"]), "Write every fact in English") {
		t.Errorf("strategies not applied: %s", ov["retain_strategies"])
	}
	code, out := run(t, "bank", "apply")
	if code != 0 || !strings.Contains(out, `"sent": false`) {
		t.Errorf("second apply should be a no-op: exit %d\n%s", code, out)
	}
}

func TestFlushSendsASessionTheServerAccepts(t *testing.T) {
	cl, cfg, home := setup(t)
	sid := "sbx-" + time.Now().UTC().Format("150405.000000")
	dir := filepath.Join(home, ".claude", "projects", "-home-u-billing")
	_ = os.MkdirAll(dir, 0o700)
	ts := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	lines := `{"type":"user","uuid":"` + sid + `-u1","sessionId":"` + sid + `","cwd":"` + filepath.Join(home, "billing") + `","timestamp":"` + ts + `","message":{"role":"user","content":"We keep PostgreSQL 16 for billing because of partial indexes."}}` + "\n" +
		`{"type":"assistant","uuid":"` + sid + `-a1","sessionId":"` + sid + `","cwd":"` + filepath.Join(home, "billing") + `","timestamp":"` + ts + `","message":{"role":"assistant","content":[{"type":"text","text":"Noted: PostgreSQL 16 stays."}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, "retain", "flush", "--session", sid)
	if code != 0 || !strings.Contains(out, `"sent"`) {
		t.Fatalf("flush: exit %d\n%s", code, out)
	}
	meta := document(t, cl, cfg.Server.Bank, "conversation:"+sid).Metadata
	if meta["session_id"] != sid || meta["last_uuid"] != sid+"-a1" || meta["source"] == "" {
		t.Errorf("metadata %v", meta)
	}
	if code, out := run(t, "retain", "flush", "--session", sid); code != 0 || strings.Contains(out, `"sent": 1`) {
		t.Errorf("second flush must send nothing: exit %d\n%s", code, out)
	}
}

func TestStoreReachesTheServer(t *testing.T) {
	cl, cfg, _ := setup(t)
	code, out := run(t, "store", "--kind", "decision", "--project", "billing", "Sandbox decision "+time.Now().UTC().Format(time.RFC3339Nano))
	var env struct {
		Data struct {
			Status   string `json:"status"`
			Document string `json:"document"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != 0 || env.Data.Status != "stored" {
		t.Fatalf("store: exit %d\n%s", code, out)
	}
	document(t, cl, cfg.Server.Bank, env.Data.Document)
}

func TestDoctorSeesTheSandbox(t *testing.T) {
	setup(t)
	code, out := run(t, "doctor")
	for _, id := range []string{`"id": "server"`, `"id": "bank"`} {
		i := strings.Index(out, id)
		if i < 0 || !strings.Contains(out[i:min(len(out), i+200)], `"status": "ok"`) {
			t.Errorf("doctor %s not ok (exit %d)\n%s", id, code, out)
		}
	}
}
