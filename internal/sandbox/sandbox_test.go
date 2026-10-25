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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Barben360/backsight/internal/cli"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/guide"
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

// llmCalls counts the LLM requests the server logged for bank today
// (embeddings and reranking are local models, logged apart).
func llmCalls(t *testing.T, cfg config.Config, bank string) int {
	t.Helper()
	d, _ := paths.Default()
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, cfg.Server.URL+"/v1/default/banks/"+url.PathEscape(bank)+"/llm-requests/stats?period=1d&call_type=llm", nil)
	req.Header.Set("Authorization", "Bearer "+sec.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Buckets []struct {
			Total int `json:"total"`
		} `json:"buckets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("llm-requests/stats: HTTP %d %v", resp.StatusCode, err)
	}
	n := 0
	for _, b := range out.Buckets {
		n += b.Total
	}
	return n
}

var guideFixture = fstest.MapFS{
	"backup.md": {Data: []byte("# Backups and restore\n\nbacksight keeps encrypted backups of the server's database.\n\n" +
		"## Make a backup\n\nRun `backsight backup create`. The archive is encrypted with age; `backsight backup list` shows the archives kept.\n\n" +
		"## Restore a backup\n\nRun `backsight restore <file>`: the server stops, its database is replaced by the archive's, and every client re-reads what the server holds.\n")},
	"install.md": {Data: []byte("# Install\n\n## Requirements\n\nLinux with Docker, Claude Code, and an API key for an LLM provider.\n\n" +
		"## Uninstall\n\nRun `backsight uninstall` to remove the hooks, the plugin and the scheduled jobs; `--delete-data` also removes the server and its volumes.\n\n" +
		"## Pause recording\n\nRun `backsight pause` to stop recording the current session; `backsight resume` starts it again.\n")},
}

// The docs bank (decision 0069) on the real server: indexing makes no LLM
// call and leaves no observation, a French question finds the English
// section, and a second index sends only the edited section.
func TestGuideDocsBankOnTheRealServer(t *testing.T) {
	cl, cfg, _ := setup(t)
	ctx := context.Background()
	mem := "sbx-guide-" + time.Now().UTC().Format("150405")
	db := guide.Bank(mem)
	t.Cleanup(func() { _ = cl.DeleteBank(context.Background(), db) })
	c, err := guide.Load(guideFixture)
	if err != nil {
		t.Fatal(err)
	}
	res, err := guide.Index(ctx, cl, mem, c)
	if err != nil || res.Sent != 6 {
		t.Fatalf("index: %v %+v", err, res)
	}
	for q, want := range map[string]string{
		"comment restaurer une sauvegarde ?":        "backup#restore-a-backup",
		"comment désinstaller backsight":            "install#uninstall",
		"arrêter l'enregistrement de cette session": "install#pause-recording",
	} {
		hits, refreshed, err := guide.Search(ctx, cl, mem, c, q, 3)
		if err != nil || refreshed != nil || len(hits) == 0 || hits[0].Ref() != want {
			var got []string
			for _, h := range hits {
				got = append(got, h.Ref())
			}
			t.Errorf("%q: %v refreshed %v, hits %v, want %s first", q, err, refreshed, got, want)
		}
	}
	edited := fstest.MapFS{"backup.md": guideFixture["backup.md"], "install.md": {Data: []byte(strings.Replace(string(guideFixture["install.md"].Data), "an API key for an LLM provider", "an LLM provider's API key", 1))}}
	c2, _ := guide.Load(edited)
	res, err = guide.Index(ctx, cl, mem, c2)
	if err != nil || res.Sent != 1 || res.Retagged != 5 {
		t.Errorf("second index: %v %+v", err, res)
	}
	if hits, _, err := guide.Search(ctx, cl, mem, c2, "comment restaurer une sauvegarde ?", 1); err != nil || len(hits) != 1 || hits[0].Ref() != "backup#restore-a-backup" {
		t.Errorf("a retagged section must stay searchable: %v %+v", err, hits)
	}
	if n := llmCalls(t, cfg, db); n != 0 {
		t.Errorf("indexing and searching the docs made %d LLM call(s)", n)
	}
	st, err := cl.BankStats(ctx, db)
	if err != nil || st.NodesByFactType["observation"] != 0 || st.TotalDocuments != 6 {
		t.Errorf("docs bank stats %+v %v", st, err)
	}
}

// The shipped docs through the command line: guide index, then a search
// answered by the docs bank, with no LLM call.
func TestGuideCommandsOnTheShippedDocs(t *testing.T) {
	_, cfg, _ := setup(t)
	code, out := run(t, "guide", "index")
	if code != 0 || !strings.Contains(out, `"bank": "`+guide.Bank(cfg.Server.Bank)+`"`) {
		t.Fatalf("guide index: exit %d\n%s", code, out)
	}
	code, out = run(t, "guide", "search", "comment désinstaller backsight ?")
	if code != 0 || !strings.Contains(out, `"mode": "semantic"`) || strings.Contains(out, `"hits": []`) {
		t.Errorf("guide search: exit %d\n%s", code, out)
	}
	if n := llmCalls(t, cfg, guide.Bank(cfg.Server.Bank)); n != 0 {
		t.Errorf("the docs bank logged %d LLM call(s)", n)
	}
}
