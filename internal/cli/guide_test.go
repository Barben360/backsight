package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/Barben360/backsight/internal/guide"
	"github.com/Barben360/backsight/internal/output"
)

// withGuideDocs makes the guide show and search a small fixture instead
// of the shipped docs.
func withGuideDocs(t *testing.T) guide.Corpus {
	t.Helper()
	fsys := fstest.MapFS{
		"backup.md": {Data: []byte("# Backups and restore\n\nbacksight keeps encrypted backups.\n\n" +
			"## Make a backup\n\nRun `backsight backup create`; the archive is encrypted with age.\n\n" +
			"## Restore a backup\n\nRun `backsight restore <file>`: the server stops and the database is replaced.\n")},
		"install.md": {Data: []byte("# Install\n\n## Uninstall\n\nRun `backsight uninstall`; `--delete-data` also removes the server volumes.\n")},
	}
	c, err := guide.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	old := guideDocs
	guideDocs = func() (guide.Corpus, error) { return c, nil }
	t.Cleanup(func() { guideDocs = old })
	return c
}

// docsHindsight is a server with a docs bank: documents kept per bank,
// recall answering the bank's documents in the order of rank.
type docsHindsight struct {
	mu      sync.Mutex
	docs    map[string]map[string][]string // bank → document id → tags
	hashes  map[string]string              // document id → section_hash
	calls   []string
	rank    []string
	retains int
	recall  int
}

func newDocsHindsight(t *testing.T, home string) *docsHindsight {
	t.Helper()
	f := &docsHindsight{docs: map[string]map[string][]string{}, hashes: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest, _ := strings.CutPrefix(r.URL.EscapedPath(), "/v1/default/banks/")
		rawBank, sub, _ := strings.Cut(rest, "/")
		bank, _ := url.PathUnescape(rawBank)
		f.calls = append(f.calls, r.Method+" "+bank+" /"+sub)
		docs, ok := f.docs[bank]
		switch {
		case r.Method == http.MethodPut:
			if !ok {
				f.docs[bank] = map[string][]string{}
			}
			_, _ = w.Write([]byte(`{}`))
		case !ok:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPatch && sub == "config":
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && sub == "documents":
			var items []map[string]any
			if r.URL.Query().Get("offset") == "0" {
				for id, tags := range docs {
					items = append(items, map[string]any{"id": id, "tags": tags, "document_metadata": map[string]string{"section_hash": f.hashes[id]}})
				}
			}
			raw, _ := json.Marshal(map[string]any{"items": items, "total": len(docs)})
			_, _ = w.Write(raw)
		case r.Method == http.MethodPost && sub == "memories":
			var body struct {
				Items []struct {
					DocumentID string            `json:"document_id"`
					Tags       []string          `json:"tags"`
					Metadata   map[string]string `json:"metadata"`
				} `json:"items"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, it := range body.Items {
				docs[it.DocumentID] = it.Tags
				f.hashes[it.DocumentID] = it.Metadata["section_hash"]
				f.retains++
			}
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && sub == "memories/recall":
			f.recall++
			var results []map[string]any
			for i, id := range f.rank {
				results = append(results, map[string]any{"id": "u" + strconv.Itoa(i), "text": "t", "document_id": id, "scores": map[string]any{"reranker": 0.9 - float64(i)/10}})
			}
			raw, _ := json.Marshal(map[string]any{"results": results})
			_, _ = w.Write(raw)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	pointAt(t, home, port)
	return f
}

type guideEnvelope struct {
	OK   bool              `json:"ok"`
	Data guideSearchResult `json:"data"`
}

func guideRun(t *testing.T, args ...string) (int, guideEnvelope, string) {
	t.Helper()
	code, out, errOut := run(t, append(args, "--json")...)
	var env guideEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("exit %d, not an envelope: %s %s", code, out, errOut)
	}
	return code, env, out
}

func TestGuideListsTopics(t *testing.T) {
	isolate(t)
	withGuideDocs(t)
	code, out, _ := run(t, "guide")
	if code != output.OK || !strings.Contains(out, "backup   Backups and restore") || !strings.Contains(out, "install") || !strings.Contains(out, "guide search") {
		t.Errorf("exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "guide", "--json")
	var env struct {
		Data struct {
			Topics []guideTopic `json:"topics"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || len(env.Data.Topics) != 2 || len(env.Data.Topics[0].Sections) != 3 || env.Data.Topics[0].Sections[2].Ref != "backup#restore-a-backup" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestGuideShowsATopicOrASectionPlainWhenNotATerminal(t *testing.T) {
	isolate(t)
	c := withGuideDocs(t)
	old := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdoutIsTerminal = old })
	t.Setenv("PAGER", "/nonexistent/pager")
	topic, _ := c.Topic("backup")
	if code, out, _ := run(t, "guide", "backup"); code != output.OK || out != topic.Text {
		t.Errorf("exit %d\n%q", code, out)
	}
	code, out, _ := run(t, "guide", "backup#restore-a-backup")
	if code != output.OK || !strings.Contains(out, "## Restore a backup") || strings.Contains(out, "## Make a backup") {
		t.Errorf("exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "guide", "backup#restore-a-backup", "--json")
	if code != output.OK || !strings.Contains(out, `"heading": "Restore a backup"`) || !strings.Contains(out, `the server stops`) {
		t.Errorf("exit %d\n%s", code, out)
	}
	for _, bad := range []string{"nope", "backup#nope", "install#"} {
		if code, out, errOut := run(t, "guide", bad); code != output.Usage || !strings.Contains(out+errOut, "backup, install") {
			t.Errorf("%s: exit %d %s %s", bad, code, out, errOut)
		}
	}
}

func TestGuidePagesAtATerminal(t *testing.T) {
	home := isolate(t)
	c := withGuideDocs(t)
	old := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdoutIsTerminal = old })
	paged := filepath.Join(home, "paged.txt")
	pager := filepath.Join(home, "pager.sh")
	if err := os.WriteFile(pager, []byte("#!/bin/sh\ncat > "+paged+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAGER", pager)
	code, out, _ := run(t, "guide", "install")
	got, _ := os.ReadFile(paged)
	topic, _ := c.Topic("install")
	if code != output.OK || out != "" || string(got) != topic.Text {
		t.Errorf("exit %d, stdout %q, pager got %q", code, out, got)
	}
	// --json never pages
	_ = os.Remove(paged)
	if code, out, _ := run(t, "guide", "install", "--json"); code != output.OK || !strings.Contains(out, `"text"`) {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(paged); err == nil {
		t.Error("--json went through the pager")
	}
}

func TestGuideSearchFallsBackToKeywordsWithoutAServer(t *testing.T) {
	isolate(t)
	withGuideDocs(t)
	code, env, _ := guideRun(t, "guide", "search", "restore", "a", "backup")
	if code != output.OK || env.Data.Mode != "keyword" || !strings.Contains(env.Data.Reason, "no server configured") || env.Data.Query != "restore a backup" {
		t.Fatalf("exit %d %+v", code, env.Data)
	}
	if len(env.Data.Hits) == 0 || env.Data.Hits[0].Ref != "backup#restore-a-backup" || env.Data.Hits[0].Excerpt == "" {
		t.Errorf("hits %+v", env.Data.Hits)
	}
	code, out, _ := run(t, "guide", "search", "restore a backup")
	if code != output.OK || !strings.Contains(out, "keyword search (offline): no server configured") || !strings.Contains(out, "backsight guide backup#restore-a-backup") {
		t.Errorf("text:\n%s", out)
	}
}

func TestGuideSearchFallsBackWhenTheServerIsDown(t *testing.T) {
	devInstall(t, true) // points at a port that hangs up
	withGuideDocs(t)
	code, env, _ := guideRun(t, "guide", "search", "uninstall")
	if code != output.OK || env.Data.Mode != "keyword" || !strings.Contains(env.Data.Reason, "unreachable") || env.Data.Hits[0].Ref != "install#uninstall" {
		t.Errorf("exit %d %+v", code, env.Data)
	}
	// Semantic only: the failure is the answer, exit 20.
	code, env, _ = guideRun(t, "guide", "search", "--mode", "semantic", "uninstall")
	if code != output.Blocked || env.OK || len(env.Data.Hits) != 0 {
		t.Errorf("exit %d %+v", code, env.Data)
	}
	if code, _, _ := run(t, "guide", "search", "--mode", "fuzzy", "x"); code != output.Usage {
		t.Errorf("unknown mode: exit %d", code)
	}
}

func TestGuideSearchRecallsFromTheDocsBankAndRefreshesItOnce(t *testing.T) {
	home := devInstall(t, true)
	c := withGuideDocs(t)
	f := newDocsHindsight(t, home)
	f.rank = []string{"docs:install#uninstall", "docs:gone#old", "docs:backup#restore-a-backup"}
	code, env, _ := guideRun(t, "guide", "search", "comment désinstaller ?")
	if code != output.OK || env.Data.Mode != "semantic" || env.Data.Bank != "claude-memory-docs" || env.Data.Refreshed == nil || env.Data.Refreshed.Sent != 4 {
		t.Fatalf("exit %d %+v", code, env.Data)
	}
	if !strings.Contains(env.Data.Reason, "refreshed") || len(env.Data.Hits) != 2 || env.Data.Hits[0].Ref != "install#uninstall" || env.Data.Hits[0].Score != 0.9 {
		t.Errorf("%+v", env.Data)
	}
	for id, tags := range f.docs["claude-memory-docs"] {
		if len(tags) != 1 || tags[0] != c.Tag() {
			t.Errorf("%s tagged %v", id, tags)
		}
	}
	// The bank holds these docs now: the next search and index send nothing.
	f.retains = 0
	code, out, _ := run(t, "guide", "search", "uninstall")
	if code != output.OK || out != "semantic search: the docs bank claude-memory-docs answered\n\n"+
		"1. Uninstall (backsight guide install#uninstall, 0.90)\n   Run `backsight uninstall`; `--delete-data` also removes the server volumes.\n\n"+
		"2. Restore a backup (backsight guide backup#restore-a-backup, 0.70)\n   Run `backsight restore <file>`: the server stops and the database is replaced.\n" {
		t.Errorf("text:\n%s", out)
	}
	code, _, out = guideRun(t, "guide", "index")
	if code != output.OK || !strings.Contains(out, `"sent": 0`) || !strings.Contains(out, `"unchanged": 4`) || f.retains != 0 {
		t.Errorf("index: exit %d %s", code, out)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "claude-memory ") {
			t.Errorf("the guide touched the memory bank: %s", call)
		}
	}
	// Asked for: the keyword search, whatever the server.
	f.recall = 0
	code, env, _ = guideRun(t, "guide", "search", "--mode", "keyword", "uninstall")
	if code != output.OK || env.Data.Mode != "keyword" || !strings.Contains(env.Data.Reason, "--mode keyword") || f.recall != 0 {
		t.Errorf("exit %d %+v (recalls %d)", code, env.Data, f.recall)
	}
}

func TestGuideIndexNeedsAServer(t *testing.T) {
	devInstall(t, true)
	withGuideDocs(t)
	if code, _, _ := guideRun(t, "guide", "index"); code != output.Blocked {
		t.Errorf("exit %d", code)
	}
	isolate(t)
	if code, _, _ := guideRun(t, "guide", "index"); code != output.NeedsUser {
		t.Errorf("no config: exit %d", code)
	}
}

// guide's own words must stay free for topic names.
func TestNoShippedTopicShadowsAGuideSubcommand(t *testing.T) {
	c, err := guide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range newGuideCmd(&env{}).Commands() {
		if _, ok := c.Topic(sub.Name()); ok {
			t.Errorf("docs/%s.md is shadowed by guide %s", sub.Name(), sub.Name())
		}
	}
}

// The docs bank is apart from the memories (decision 0069): recall,
// doctor (its untagged and drift checks included), bank apply, stats and
// projects read the memory bank alone, even with a docs bank on the server.
func TestMemoryCommandsNeverReadTheDocsBank(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/default/banks/claude-memory-docs", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	listAnswer["world"] = `[{"id":"f1","text":"x","fact_type":"world","tags":["project:a"]}]`
	projectEntities["a"] = `[]`
	bankCalls = nil
	for _, args := range [][]string{{"bank", "apply"}, {"search", "restore a backup"}, {"search", "--all", "restore"}, {"doctor"}, {"stats"}, {"projects"}} {
		if code, out, errOut := run(t, append(args, "--json")...); code != output.OK {
			t.Fatalf("%v: exit %d %s %s", args, code, out, errOut)
		}
	}
	calls := strings.Join(bankCalls, ", ")
	for _, want := range []string{"POST claude-memory /memories/recall", "GET claude-memory /memories/list", "GET claude-memory /config", "PATCH claude-memory /config", "GET claude-memory /llm-requests/stats", "GET claude-memory /tags"} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q: the commands did not run their checks\n%s", want, calls)
		}
	}
	for _, c := range bankCalls {
		bank := strings.Fields(c)[1]
		if strings.HasSuffix(bank, "-docs") {
			t.Errorf("a memory command read the docs bank: %s", c)
		}
	}
}

// update apply has the new binary index its docs once the update is
// committed; a failure there is a warning, never a rollback.
func TestUpdateApplyIndexesTheNewBinarysDocs(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0")
	withGuideDocs(t)
	f := newDocsHindsight(t, home)
	configure(t, home, "[backup]\nenabled = false")
	key := trustTestKey(t)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", key, relOpts{})
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || bins.ran("0.2.0 guide index") != 1 || !hasApplyStep(env.Data.Steps, "docs", "done", "4 section(s) in bank claude-memory-docs") || len(f.docs["claude-memory-docs"]) != 4 {
		t.Fatalf("exit %d calls %q\n%s", code, bins.calls, out)
	}
	if i, j := applyStepIndex(env.Data.Steps, "server"), applyStepIndex(env.Data.Steps, "docs"); i < 0 || j < i {
		t.Errorf("docs step before the server step: %+v", env.Data.Steps)
	}
}

func TestUpdateApplyGoesOnWhenTheDocsCannotBeIndexed(t *testing.T) {
	home, _, bins := installedAt(t, "0.1.0") // its server hangs up
	d := pathsOf(home)
	withGuideDocs(t)
	key := trustTestKey(t)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", key, relOpts{})
	code, out, _ := run(t, "update", "apply", "--source", src, "--json")
	env := decodeApply(t, out)
	if code != output.OK || env.Data.RolledBack || !strings.HasPrefix(shimOf(t, d), filepath.Join(d.Data, "bin", "0.2.0-")) {
		t.Fatalf("a docs failure must not undo the update: exit %d\n%s", code, out)
	}
	if bins.ran("0.2.0 guide index") != 1 || !hasApplyStep(env.Data.Steps, "docs", "failed", "guide index") ||
		!slices.ContainsFunc(env.Data.Warnings, func(w string) bool {
			return strings.Contains(w, "docs were not indexed") && strings.Contains(w, "first use")
		}) {
		t.Errorf("calls %q\n%s", bins.calls, out)
	}
	// The plan says it would index them, and runs nothing.
	signedRelease(t, src, "0.3.0", key, relOpts{})
	bins.calls = nil
	code, out, _ = run(t, "update", "apply", "--plan", "--source", src, "--json")
	if env := decodeApply(t, out); code != output.OK || !hasApplyStep(env.Data.Steps, "docs", "planned", "guide index") || bins.ran("0.3.0 guide index") != 0 {
		t.Errorf("plan: exit %d calls %q\n%s", code, bins.calls, out)
	}
}

func hasApplyStep(steps []step, name, status, detail string) bool {
	i := applyStepIndex(steps, name)
	return i >= 0 && steps[i].Status == status && strings.Contains(steps[i].Detail, detail)
}

func applyStepIndex(steps []step, name string) int {
	for i, s := range steps {
		if s.Name == name {
			return i
		}
	}
	return -1
}
