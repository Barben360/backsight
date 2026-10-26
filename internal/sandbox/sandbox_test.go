//go:build sandbox

// Package sandbox runs the client against a real Hindsight started by
// scripts/sandbox.sh (SPEC §16): what fakes cannot prove, the server's
// actual answers to backsight's requests.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Barben360/backsight/internal/cli"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/guide"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
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
// (embeddings and reranking are local models, logged apart). Every bucket
// must carry its total: a renamed field would read as 0 calls.
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
		Buckets *[]map[string]json.RawMessage `json:"buckets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.Buckets == nil {
		t.Fatalf("llm-requests/stats: HTTP %d %v, buckets %v", resp.StatusCode, err, out.Buckets)
	}
	n := 0
	for _, b := range *out.Buckets {
		var total *int
		if err := json.Unmarshal(b["total"], &total); err != nil || total == nil {
			t.Fatalf("llm-requests/stats: a bucket without its total: %v", b)
		}
		n += *total
	}
	return n
}

// llmCountsWork is llmCalls' positive control: a retain into a bank with
// the default settings (the mock LLM's extraction) shows up as LLM calls,
// so a 0 elsewhere means no call, not a count that cannot see any. Run
// after the work checked, it also waits out the stats' lag.
func llmCountsWork(t *testing.T, cl *hindsight.Client, cfg config.Config) {
	t.Helper()
	bank := "sbx-llm-" + suffix(t)
	t.Cleanup(func() { _ = cl.DeleteBank(context.Background(), bank) })
	if err := cl.RetainNow(context.Background(), bank, []hindsight.RetainItem{{Content: "We keep PostgreSQL 16 for billing because of partial indexes.", DocumentID: "control", Tags: []string{}}}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); llmCalls(t, cfg, bank) == 0; time.Sleep(2 * time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("an extracting retain logged no LLM call: the count cannot prove 0 calls")
		}
	}
}

// suffix makes a bank name unique to this run (two runs in one second
// shared one before).
func suffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return time.Now().UTC().Format("150405") + "-" + hex.EncodeToString(b)
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
	mem := "sbx-guide-" + suffix(t)
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
	// Both versions on one bank: the older one sends back only the section
	// they disagree on, and retags nothing.
	if hits, refreshed, err := guide.Search(ctx, cl, mem, c, "comment désinstaller backsight", 1); err != nil || refreshed == nil || refreshed.Sent != 1 || refreshed.Retagged != 0 ||
		len(hits) != 1 || hits[0].Ref() != "install#uninstall" {
		t.Errorf("the older version on a shared bank: %v %+v %+v", err, refreshed, hits)
	}
	llmCountsWork(t, cl, cfg)
	if n := llmCalls(t, cfg, db); n != 0 {
		t.Errorf("indexing and searching the docs made %d LLM call(s)", n)
	}
	// Six sections and the record of both versions' first index times.
	st, err := cl.BankStats(ctx, db)
	if err != nil || st.NodesByFactType["observation"] != 0 || st.TotalDocuments != 7 {
		t.Errorf("docs bank stats %+v %v", st, err)
	}
	docs, err := cl.ListDocuments(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.ID == guide.VersionsID && (d.Metadata["docs-version-at:"+c.Hash[:16]] == "" || d.Metadata["docs-version-at:"+c2.Hash[:16]] == "" || len(d.Tags) != 0) {
			t.Errorf("versions record %+v: both versions' times, no tag", d)
		}
	}
	if !slices.ContainsFunc(docs, func(d hindsight.DocumentItem) bool { return d.ID == guide.VersionsID }) {
		t.Errorf("no %s among %d documents", guide.VersionsID, len(docs))
	}
}

// The shipped docs through the command line: guide index, then a search
// answered by the docs bank, with no LLM call.
func TestGuideCommandsOnTheShippedDocs(t *testing.T) {
	cl, cfg, _ := setup(t)
	code, out := run(t, "guide", "index")
	if code != 0 || !strings.Contains(out, `"bank": "`+guide.Bank(cfg.Server.Bank)+`"`) {
		t.Fatalf("guide index: exit %d\n%s", code, out)
	}
	code, out = run(t, "guide", "search", "comment désinstaller backsight ?")
	if code != 0 || !strings.Contains(out, `"mode": "semantic"`) || strings.Contains(out, `"hits": []`) {
		t.Errorf("guide search: exit %d\n%s", code, out)
	}
	llmCountsWork(t, cl, cfg)
	if n := llmCalls(t, cfg, guide.Bank(cfg.Server.Bank)); n != 0 {
		t.Errorf("the docs bank logged %d LLM call(s)", n)
	}
}

// A `<bank>-docs` someone else made on the real server: index refuses and
// leaves its name, config and documents as they were (decision 0067, block
// 4 review).
func TestGuideLeavesAForeignDocsBankAlone(t *testing.T) {
	cl, _, _ := setup(t)
	ctx := context.Background()
	mem := "sbx-foreign-" + suffix(t)
	db := guide.Bank(mem)
	t.Cleanup(func() { _ = cl.DeleteBank(context.Background(), db) })
	if err := cl.CreateBank(ctx, db, "team docs", "our own notes"); err != nil {
		t.Fatal(err)
	}
	if err := cl.PatchBankConfig(ctx, db, map[string]any{"retain_extraction_mode": "chunks"}); err != nil {
		t.Fatal(err)
	}
	if err := cl.RetainNow(ctx, db, []hindsight.RetainItem{{Content: "Our release checklist lives in the wiki.", DocumentID: "note:1", Tags: []string{}}}); err != nil {
		t.Fatal(err)
	}
	c, _ := guide.Load(guideFixture)
	_, err := guide.Index(ctx, cl, mem, c)
	var foreign *retain.ForeignBank
	if !errors.As(err, &foreign) || !strings.Contains(err.Error(), `"team docs"`) {
		t.Fatalf("index: %v", err)
	}
	b, found, err := cl.FindBank(ctx, db)
	docs, lerr := cl.ListDocuments(ctx, db)
	ov, cerr := cl.BankConfigOverrides(ctx, db)
	if err != nil || !found || b.Name != "team docs" || lerr != nil || len(docs) != 1 || docs[0].ID != "note:1" || cerr != nil || ov["enable_text_search"] != nil || ov["enable_observations"] != nil {
		t.Errorf("the bank changed: %+v %v %v %v %v %v", b, err, docs, lerr, ov, cerr)
	}
}
