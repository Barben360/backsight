package cli

import (
	"context"
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
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/guide"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/testtool"
	"github.com/Barben360/backsight/internal/views"
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
	meta    map[string]map[string]string   // document id → its metadata
	names   map[string]string              // bank → name (PUT)
	calls   []string
	rank    []string
	retains int
	recall  int
	status  map[string]int // "METHOD /sub" (or "*") → the status answered instead
	delay   time.Duration  // recall answers after this long
}

func newDocsHindsight(t *testing.T, home string) *docsHindsight {
	t.Helper()
	f := &docsHindsight{docs: map[string]map[string][]string{}, meta: map[string]map[string]string{}, names: map[string]string{}, status: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/v1/default/banks" {
			f.calls = append(f.calls, "LIST "+r.URL.Query().Get("q")+" /")
			if st := f.status["*"]; st != 0 {
				w.WriteHeader(st)
				return
			}
			banks := []map[string]string{}
			for b := range f.docs {
				banks = append(banks, map[string]string{"bank_id": b, "name": f.names[b]})
			}
			raw, _ := json.Marshal(map[string]any{"banks": banks, "total": len(banks)})
			_, _ = w.Write(raw)
			return
		}
		rest, _ := strings.CutPrefix(r.URL.EscapedPath(), "/v1/default/banks/")
		rawBank, sub, _ := strings.Cut(rest, "/")
		bank, _ := url.PathUnescape(rawBank)
		f.calls = append(f.calls, r.Method+" "+bank+" /"+sub)
		if st := max(f.status["*"], f.status[r.Method+" /"+sub]); st != 0 {
			w.WriteHeader(st)
			return
		}
		docs, ok := f.docs[bank]
		switch {
		case r.Method == http.MethodPut:
			if !ok {
				f.docs[bank] = map[string][]string{}
			}
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.names[bank] = body.Name
			_, _ = w.Write([]byte(`{}`))
		case !ok:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPatch && sub == "config":
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(sub, "documents/"):
			id, _ := url.PathUnescape(strings.TrimPrefix(sub, "documents/"))
			var body struct {
				Tags []string `json:"tags"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			docs[id] = body.Tags
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(sub, "documents/"):
			id, _ := url.PathUnescape(strings.TrimPrefix(sub, "documents/"))
			delete(docs, id)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && sub == "documents":
			var items []map[string]any
			if r.URL.Query().Get("offset") == "0" {
				for id, tags := range docs {
					items = append(items, map[string]any{"id": id, "tags": tags, "document_metadata": f.meta[id]})
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
				f.meta[it.DocumentID] = it.Metadata
				f.retains++
			}
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && sub == "memories/recall":
			f.recall++
			if f.delay > 0 {
				f.mu.Unlock()
				select {
				case <-time.After(f.delay):
				case <-r.Context().Done():
				}
				f.mu.Lock()
			}
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
	pager := testtool.Write(t, home, "pager", testtool.Fake{Script: "stdinto " + paged})
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
	if code != output.Blocked || env.OK || len(env.Data.Hits) != 0 || env.Data.Mode != "semantic" || !strings.Contains(env.Data.Reason, "unreachable") {
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
		if id != guide.VersionsID && (len(tags) != 1 || tags[0] != c.Tag()) {
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
	if code != output.OK || bins.ran("0.2.0 guide index") != 1 || !hasApplyStep(env.Data.Steps, "docs", "done", "4 section(s) in bank claude-memory-docs") || len(f.docs["claude-memory-docs"]) != 5 {
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
			return strings.Contains(w, "docs were not indexed") && strings.Contains(w, "keyword search meanwhile")
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

// Each reason the semantic search cannot answer reaches the keyword
// fallback's reason, so the user knows what to fix.
func TestGuideSearchSaysWhyItFellBack(t *testing.T) {
	old := guideCallTimeout
	guideCallTimeout = 300 * time.Millisecond
	t.Cleanup(func() { guideCallTimeout = old })
	for name, c := range map[string]struct {
		set    func(home string, f *docsHindsight)
		reason string
	}{
		"config unreadable": {func(home string, _ *docsHindsight) {
			_ = os.WriteFile(filepath.Join(home, "backsight", "config", "config.toml"), []byte("[server\nurl = "), 0o600)
		}, "config unreadable"},
		"key refused":   {func(_ string, f *docsHindsight) { f.status["*"] = http.StatusUnauthorized }, "the server refused the API key"},
		"timeout":       {func(_ string, f *docsHindsight) { f.delay = 2 * time.Second }, "did not answer in time"},
		"refresh fails": {func(_ string, f *docsHindsight) { f.status["POST /memories"] = http.StatusInternalServerError }, "could not be refreshed"},
		"no hit":        {func(_ string, f *docsHindsight) { f.rank = nil }, "the semantic search found nothing in bank claude-memory-docs"},
		"foreign bank": {func(_ string, f *docsHindsight) {
			f.docs["claude-memory-docs"], f.names["claude-memory-docs"] = map[string][]string{"note:1": nil}, "team docs"
		}, `the docs bank claude-memory-docs is not backsight's (its name is "team docs", not "backsight docs")`},
	} {
		t.Run(name, func(t *testing.T) {
			home := devInstall(t, true)
			withGuideDocs(t)
			f := newDocsHindsight(t, home)
			f.rank = []string{"docs:install#uninstall"}
			c.set(home, f)
			code, env, out := guideRun(t, "guide", "search", "uninstall")
			if code != output.OK || env.Data.Mode != "keyword" || !strings.Contains(env.Data.Reason, c.reason) {
				t.Fatalf("exit %d: %s", code, out)
			}
			if len(env.Data.Hits) == 0 || env.Data.Hits[0].Ref != "install#uninstall" {
				t.Errorf("the keyword search did not answer: %+v", env.Data.Hits)
			}
		})
	}
}

// A foreign docs bank is left as it is by every guide command: search
// reads, index and its plan refuse with the reason (exit 20).
func TestGuideLeavesAForeignDocsBankAlone(t *testing.T) {
	home := devInstall(t, true)
	withGuideDocs(t)
	f := newDocsHindsight(t, home)
	f.docs["claude-memory-docs"], f.names["claude-memory-docs"] = map[string][]string{"docs:x": {"mine"}}, "my notes"
	for _, args := range [][]string{{"guide", "index"}, {"guide", "index", "--plan"}} {
		code, out, errOut := run(t, append(args, "--json")...)
		if code != output.Blocked || !strings.Contains(out+errOut, "is not backsight's") || !strings.Contains(out+errOut, `its name is \"my notes\"`) {
			t.Errorf("%v: exit %d %s %s", args, code, out, errOut)
		}
	}
	if code, env, _ := guideRun(t, "guide", "search", "uninstall"); code != output.OK || env.Data.Mode != "keyword" {
		t.Errorf("search: exit %d %+v", code, env.Data)
	}
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "GET ") && !strings.HasPrefix(call, "LIST ") {
			t.Errorf("wrote to a bank that is not backsight's: %s", call)
		}
	}
	if f.names["claude-memory-docs"] != "my notes" || len(f.docs["claude-memory-docs"]) != 1 {
		t.Errorf("the bank changed: %v %v", f.names, f.docs)
	}
}

// Forced semantic with no hit is an answer, not a failure: exit 0, mode
// semantic, no result (decision 0067, block 4 review).
func TestGuideForcedSemanticWithNoHitIsAnAnswer(t *testing.T) {
	home := devInstall(t, true)
	withGuideDocs(t)
	newDocsHindsight(t, home) // recall finds nothing
	code, env, out := guideRun(t, "guide", "search", "--mode", "semantic", "kubernetes")
	if code != output.OK || !env.OK || env.Data.Mode != "semantic" || len(env.Data.Hits) != 0 || !strings.Contains(env.Data.Reason, "found nothing") {
		t.Errorf("exit %d %s", code, out)
	}
}

// guide index --plan lists the documents it would send, retag and delete,
// and writes nothing; index then does just that.
func TestGuideIndexPlanWritesNothing(t *testing.T) {
	home := devInstall(t, true)
	withGuideDocs(t)
	f := newDocsHindsight(t, home)
	code, out, _ := run(t, "guide", "index", "--plan", "--json")
	var env struct {
		Data guide.IndexPlan `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || env.Data.Exists || strings.Join(env.Data.Create, ",") != "docs:backup,docs:backup#make-a-backup,docs:backup#restore-a-backup,docs:install#uninstall" ||
		len(env.Data.Delete) != 0 || env.Data.Bank != "claude-memory-docs" {
		t.Errorf("exit %d %s", code, out)
	}
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "GET ") && !strings.HasPrefix(call, "LIST ") {
			t.Errorf("the plan wrote: %s", call)
		}
	}
	code, out, _ = run(t, "guide", "index", "--plan")
	if code != output.OK || out != "bank claude-memory-docs would be created, then:\n"+
		"  create (4): docs:backup, docs:backup#make-a-backup, docs:backup#restore-a-backup, docs:install#uninstall\n"+
		"  resend (0)\n  retag (0)\n  untag (0)\n  prune (0)\n  delete (0)\n  drop docs versions (0)\n  unchanged: 0\n"+
		"  record: docs:_versions would hold this docs version's first index time\nnothing was changed (--plan)\n" {
		t.Errorf("text plan:\n%s", out)
	}
	if code, _, _ := run(t, "guide", "index"); code != output.OK || len(f.docs["claude-memory-docs"]) != 5 || f.meta[guide.VersionsID] == nil {
		t.Errorf("index after the plan: exit %d %v", code, f.docs)
	}
}

// A pager that ran is the output, whatever its exit status: no plain copy
// after it. Only a pager that cannot start falls back to plain output.
func TestGuidePagerFallsBackOnlyWhenItCannotStart(t *testing.T) {
	home := isolate(t)
	c := withGuideDocs(t)
	old := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdoutIsTerminal = old })
	pager := testtool.Write(t, home, "pager", testtool.Fake{Script: "stdinto " + os.DevNull, Code: 3})
	t.Setenv("PAGER", pager)
	if code, out, _ := run(t, "guide", "install"); code != output.OK || out != "" {
		t.Errorf("a pager that exited 3 was followed by a copy: exit %d %q", code, out)
	}
	t.Setenv("PAGER", filepath.Join(home, "no-such-pager"))
	topic, _ := c.Topic("install")
	if code, out, _ := run(t, "guide", "install"); code != output.OK || out != topic.Text {
		t.Errorf("no pager: exit %d %q", code, out)
	}
}

// toClient turns the install newDocsHindsight pointed at the fake into a
// client of it (server.mode remote).
func toClient(t *testing.T, home string) {
	t.Helper()
	p := pathsOf(home).ConfigFile()
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(raw), "[server]\n", "[server]\nmode = \"remote\"\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// docsWrites lists the calls that changed the fake's banks.
func docsWrites(f *docsHindsight) []string {
	var w []string
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "GET ") && !strings.HasPrefix(call, "LIST ") && !strings.Contains(call, "/memories/recall") {
			w = append(w, call)
		}
	}
	return w
}

// The docs bank is its host's: guide index on a client refuses, names
// the host, and writes nothing.
func TestGuideIndexRefusedOnAClient(t *testing.T) {
	home := devInstall(t, true)
	withGuideDocs(t)
	f := newDocsHindsight(t, home)
	toClient(t, home)
	for _, args := range [][]string{{"guide", "index"}, {"guide", "index", "--plan"}} {
		code, _, errOut := run(t, args...)
		if code != output.Usage || !strings.Contains(errOut, "run backsight guide index on that server's host") || !strings.Contains(errOut, "127.0.0.1") {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	if len(f.calls) != 0 || f.retains != 0 {
		t.Errorf("a client called the docs bank: %v", f.calls)
	}
}

// guide search on a client reads the docs bank as the host left it: on
// these docs it answers, else it falls back to the keyword search; it
// never creates, refreshes or retags the bank.
func TestGuideSearchOnAClientFallsBackToKeyword(t *testing.T) {
	home := devInstall(t, true)
	c := withGuideDocs(t)
	f := newDocsHindsight(t, home)
	f.rank = []string{"docs:install#uninstall"}
	toClient(t, home)
	const reason = "the server's docs bank holds the host's docs version"

	// No docs bank yet.
	code, env, out := guideRun(t, "guide", "search", "uninstall")
	if code != output.OK || env.Data.Mode != "keyword" || env.Data.Reason != reason || len(env.Data.Hits) == 0 || env.Data.Hits[0].Ref != "install#uninstall" {
		t.Errorf("missing bank: exit %d %s", code, out)
	}
	code, env, out = guideRun(t, "guide", "search", "--mode", "semantic", "uninstall")
	if code != output.Blocked || env.Data.Mode != "semantic" || env.Data.Reason != reason {
		t.Errorf("missing bank, semantic only: exit %d %s", code, out)
	}
	if w := docsWrites(f); len(w) != 0 || len(f.docs) != 0 {
		t.Fatalf("a client wrote the docs bank: %v", w)
	}

	// The host indexed other docs.
	other, err := guide.Load(fstest.MapFS{"install.md": {Data: []byte("# Install\n\n## Uninstall\n\nRun `backsight uninstall --stop`.\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guide.Index(context.Background(), docsClient(t, home), "claude-memory", other); err != nil {
		t.Fatal(err)
	}
	f.calls, f.retains = nil, 0
	code, env, out = guideRun(t, "guide", "search", "uninstall")
	if code != output.OK || env.Data.Mode != "keyword" || env.Data.Reason != reason || env.Data.Refreshed != nil {
		t.Errorf("another docs version: exit %d %s", code, out)
	}
	if w := docsWrites(f); len(w) != 0 || f.retains != 0 {
		t.Errorf("a client refreshed the docs bank: %v", w)
	}

	// The host indexed these docs: the client's search answers from them.
	if _, err := guide.Index(context.Background(), docsClient(t, home), "claude-memory", c); err != nil {
		t.Fatal(err)
	}
	f.calls, f.retains = nil, 0
	code, env, out = guideRun(t, "guide", "search", "uninstall")
	if code != output.OK || env.Data.Mode != "semantic" || env.Data.Refreshed != nil || len(env.Data.Hits) != 1 {
		t.Errorf("these docs: exit %d %s", code, out)
	}
	if w := docsWrites(f); len(w) != 0 {
		t.Errorf("a client wrote the docs bank: %v", w)
	}
}

// docsClient is a Hindsight client of the server home's config names, as
// the server's host would hold it.
func docsClient(t *testing.T, home string) *hindsight.Client {
	t.Helper()
	cfg, err := config.Load(pathsOf(home).ConfigFile(), pathsOf(home), home)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := hindsight.New(cfg.Server.URL, "k", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// The shipped dashboard guide tells how to open it, how to reach it from
// another PC and how to get a new link.
func TestGuideDashboardTopic(t *testing.T) {
	code, out, _ := run(t, "guide", "dashboard")
	if code != output.OK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"From another PC", "ssh -L 8765:127.0.0.1:8765", "backsight dashboard link"} {
		if !strings.Contains(out, want) {
			t.Errorf("guide dashboard lacks %q", want)
		}
	}
}

// The shipped dashboard guide asks each page's question, says what a missing
// number means and names the keyboard and preference features a reader needs.
func TestGuideDashboardPages(t *testing.T) {
	code, out, _ := run(t, "guide", "dashboard")
	if code != output.OK {
		t.Fatalf("exit %d", code)
	}
	if len(views.Questions) == 0 {
		t.Fatal("no view questions")
	}
	for view, q := range views.Questions {
		if !strings.Contains(out, q) {
			t.Errorf("guide dashboard lacks the question of %s: %q", view, q)
		}
	}
	for _, want := range []string{"Show as a table", "Developer mode", "Skip to content", "When a number is missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("guide dashboard lacks %q", want)
		}
	}
}
