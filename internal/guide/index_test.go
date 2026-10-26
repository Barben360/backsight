package guide

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/retain"
)

// fakeDoc is a document the fake server holds.
type fakeDoc struct {
	content string
	tags    []string
	meta    map[string]string
}

// fakeServer is a Hindsight holding documents per bank: retains replace
// documents, PATCH replaces tags, DELETE removes; recall answers the
// documents of the bank tagged with one of the asked tags, ordered by
// rank (document ids, best first; others after, by id).
type fakeServer struct {
	mu      sync.Mutex
	banks   map[string]map[string]*fakeDoc
	names   map[string]string // bank → its name, set by PUT
	configs map[string]map[string]any
	calls   []string // METHOD bank /rest
	retains []string // document ids sent, in order
	recall  []map[string]any
	rank    []string
	downAt  string   // a call whose METHOD /rest starts so answers 500
	phantom []string // listed, but gone when deleted (another client removed them)
}

func newFake(t *testing.T) (*fakeServer, *hindsight.Client) {
	t.Helper()
	f := &fakeServer{banks: map[string]map[string]*fakeDoc{}, names: map[string]string{}, configs: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	cl, err := hindsight.New(srv.URL, "k", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return f, cl
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/v1/default/banks" { // like Hindsight: q matches ids and names as substrings
		q := r.URL.Query().Get("q")
		f.calls = append(f.calls, "LIST "+q+" /")
		if f.downAt == "LIST" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		banks := []map[string]string{}
		for b := range f.banks {
			if strings.Contains(b, q) || strings.Contains(f.names[b], q) {
				banks = append(banks, map[string]string{"bank_id": b, "name": f.names[b]})
			}
		}
		raw, _ := json.Marshal(map[string]any{"banks": banks, "total": len(banks)})
		_, _ = w.Write(raw)
		return
	}
	rest, _ := strings.CutPrefix(r.URL.EscapedPath(), "/v1/default/banks/")
	rawBank, sub, _ := strings.Cut(rest, "/")
	bank, _ := url.PathUnescape(rawBank)
	sub = "/" + sub
	f.calls = append(f.calls, r.Method+" "+bank+" "+sub)
	if f.downAt != "" && strings.HasPrefix(r.Method+" "+sub, f.downAt) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	docs, exists := f.banks[bank]
	switch {
	case r.Method == http.MethodPut && sub == "/":
		if !exists {
			f.banks[bank] = map[string]*fakeDoc{}
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.names[bank] = body.Name
		_, _ = w.Write([]byte(`{}`))
	case !exists:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Bank not found"}`))
	case r.Method == http.MethodPatch && sub == "/config":
		var body struct {
			Updates map[string]any `json:"updates"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.configs[bank] == nil {
			f.configs[bank] = map[string]any{}
		}
		for k, v := range body.Updates {
			f.configs[bank][k] = v
		}
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodGet && sub == "/documents":
		if r.URL.Query().Get("offset") != "0" {
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
			return
		}
		var items []map[string]any
		for id, d := range docs {
			items = append(items, map[string]any{"id": id, "tags": d.tags, "document_metadata": d.meta})
		}
		for _, id := range f.phantom {
			items = append(items, map[string]any{"id": id, "tags": []string{}})
		}
		raw, _ := json.Marshal(map[string]any{"items": items, "total": len(items)})
		_, _ = w.Write(raw)
	case r.Method == http.MethodPost && sub == "/memories":
		var body struct {
			Items []struct {
				Content    string            `json:"content"`
				DocumentID string            `json:"document_id"`
				Tags       []string          `json:"tags"`
				Metadata   map[string]string `json:"metadata"`
			} `json:"items"`
			Async bool `json:"async"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, it := range body.Items {
			docs[it.DocumentID] = &fakeDoc{it.Content, it.Tags, it.Metadata}
			f.retains = append(f.retains, it.DocumentID)
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	case r.Method == http.MethodPatch && strings.HasPrefix(sub, "/documents/"):
		id, _ := url.PathUnescape(strings.TrimPrefix(sub, "/documents/"))
		var body struct {
			Tags []string `json:"tags"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if d, ok := docs[id]; ok {
			d.tags = body.Tags
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(sub, "/documents/"):
		id, _ := url.PathUnescape(strings.TrimPrefix(sub, "/documents/"))
		if _, ok := docs[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Document not found"}`))
			return
		}
		delete(docs, id)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && sub == "/memories/recall":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.recall = append(f.recall, body)
		want := map[string]bool{}
		tags, _ := body["tags"].([]any)
		for _, t := range tags {
			want[t.(string)] = true
		}
		var ids []string
		for id, d := range docs {
			for _, t := range d.tags {
				if want[t] {
					ids = append(ids, id)
					break
				}
			}
		}
		pos := func(id string) int {
			for i, r := range f.rank {
				if r == id {
					return i
				}
			}
			return len(f.rank)
		}
		sort.Slice(ids, func(a, b int) bool {
			if pos(ids[a]) != pos(ids[b]) {
				return pos(ids[a]) < pos(ids[b])
			}
			return ids[a] < ids[b]
		})
		var results []map[string]any
		for i, id := range ids {
			// two chunks of the first document: hits are per section
			n := 1
			if i == 0 {
				n = 2
			}
			for j := 0; j < n; j++ {
				results = append(results, map[string]any{"id": id + "/u" + string(rune('0'+j)), "text": docs[id].content, "type": "world", "document_id": id,
					"scores": map[string]any{"final": 1 - float64(i)/10 - float64(j)/100, "reranker": 0.9 - float64(i)/10}})
			}
		}
		results = append(results, map[string]any{"id": "x", "text": "?", "document_id": "docs:gone#old", "scores": map[string]any{"final": 0.1}})
		raw, _ := json.Marshal(map[string]any{"results": results})
		_, _ = w.Write(raw)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func (f *fakeServer) writes() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") && !strings.HasPrefix(c, "LIST ") && !strings.Contains(c, "/memories/recall") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeServer) reset() { f.calls, f.retains, f.recall = nil, nil, nil }

func TestIndexSetsUpAQuietBankAndSendsEverySection(t *testing.T) {
	f, cl := newFake(t)
	c := load(t)
	res, err := Index(context.Background(), cl, "mem", c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bank != "mem-docs" || res.Sections != 6 || res.Sent != 6 || res.Unchanged != 0 || res.DocsHash != c.Hash {
		t.Errorf("%+v", res)
	}
	cfg := f.configs["mem-docs"]
	if cfg["retain_extraction_mode"] != "chunks" || cfg["enable_observations"] != false {
		t.Errorf("docs bank config %v: must cost no LLM call", cfg)
	}
	// Recall ranks by embeddings and the reranker only (E31).
	if cfg["enable_graph_retrieval"] != false || cfg["enable_temporal_retrieval"] != false || cfg["enable_text_search"] != false {
		t.Errorf("docs bank retrieval %v", cfg)
	}
	if !strings.HasPrefix(strings.Join(f.calls, ", "), "LIST mem-docs /, PUT mem-docs /, PATCH mem-docs /config, PATCH mem-docs /config, POST mem-docs /memories") {
		t.Errorf("the bank must be set up before anything is retained: %v", f.calls)
	}
	for _, call := range f.calls {
		if !strings.Contains(call, " mem-docs ") {
			t.Errorf("indexing touched another bank: %s", call)
		}
	}
	d := f.banks["mem-docs"]["docs:backup#restore-a-backup"]
	s, _ := c.Section("backup#restore-a-backup")
	if d == nil || d.content != s.Text || len(d.tags) != 1 || d.tags[0] != c.Tag() || d.meta["section_hash"] != s.Hash || d.meta["topic"] != "backup" || d.meta["heading"] != "Restore a backup" {
		t.Errorf("document %+v", d)
	}
}

// edited is the fixture with install.md changed: Requirements edited, the
// second Uninstall removed, Update added.
func edited(t *testing.T) Corpus {
	t.Helper()
	fs := fixture()
	fs["install.md"] = &fstest.MapFile{Data: []byte("# Install\n\n## Requirements\n\nLinux, Docker, Claude Code and an LLM key.\n\n" +
		"## Uninstall\n\nRun `backsight uninstall`; `--delete-data` also removes the server volumes.\n\n## Update\n\nRun `backsight update apply`.\n")}
	c, err := Load(fs)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tagsOf(f *fakeServer, id string) string {
	d := f.banks["mem-docs"][id]
	if d == nil {
		return "missing"
	}
	return strings.Join(d.tags, ",")
}

// A new docs version sends only its new and edited sections, adds its tag
// to the unchanged ones and leaves the previous version's sections it no
// longer has: they still carry that version's tag (decision 0067, block 4
// review).
func TestIndexSendsOnlyWhatChanged(t *testing.T) {
	f, cl := newFake(t)
	c1 := load(t)
	if _, err := Index(context.Background(), cl, "mem", c1); err != nil {
		t.Fatal(err)
	}
	c2 := edited(t)
	f.reset()
	res, err := Index(context.Background(), cl, "mem", c2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.retains, ", "); got != "docs:install#requirements, docs:install#update" {
		t.Errorf("sent %s: only the edited and the new section", got)
	}
	if res.Sent != 2 || res.Retagged != 4 || res.Deleted != 0 || res.Untagged != 0 || res.Unchanged != 0 {
		t.Errorf("%+v", res)
	}
	both := c1.Tag() + "," + c2.Tag()
	for id, want := range map[string]string{
		"docs:backup": both, "docs:backup#make-a-backup": both, "docs:backup#restore-a-backup": both, "docs:install#uninstall": both,
		"docs:install#requirements": c2.Tag(), "docs:install#update": c2.Tag(),
		"docs:install#uninstall-1": c1.Tag(), // the previous version's, kept
	} {
		if got := tagsOf(f, id); got != want {
			t.Errorf("%s tagged %s, want %s", id, got, want)
		}
	}
	// Once more: nothing to send, retag or delete.
	f.reset()
	res, err = Index(context.Background(), cl, "mem", c2)
	if err != nil || res.Sent+res.Retagged+res.Deleted+res.Untagged != 0 || res.Unchanged != 6 || len(f.retains) != 0 {
		t.Errorf("%v %+v %v", err, res, f.calls)
	}
}

// Two docs versions on one server (two clients, or a restore) no longer
// refresh each other's unchanged sections: each search of the older one
// sends only the section the versions disagree on, retags nothing, and
// finds its own copy of every section.
func TestTwoDocsVersionsShareTheBank(t *testing.T) {
	f, cl := newFake(t)
	c1, c2 := load(t), edited(t)
	for _, c := range []Corpus{c1, c2} {
		if _, err := Index(context.Background(), cl, "mem", c); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []Corpus{c1, c2} {
		f.reset()
		_, refreshed, err := Search(context.Background(), cl, "mem", c, "uninstall", 10)
		if err != nil || refreshed == nil || refreshed.Sent != 1 || refreshed.Retagged != 0 || refreshed.Deleted != 0 ||
			strings.Join(f.retains, ",") != "docs:install#requirements" {
			t.Errorf("version %s: %v %+v sent %v", c.Tag(), err, refreshed, f.retains)
		}
		if got := f.recall[len(f.recall)-1]["tags"].([]any); len(got) != 1 || got[0] != c.Tag() {
			t.Errorf("recall tags %v: its own version only", got)
		}
	}
	if got := tagsOf(f, "docs:install#update"); got != c2.Tag() {
		t.Errorf("the older version touched the newer one's section: %s", got)
	}
	if got := tagsOf(f, "docs:install#uninstall-1"); got != c1.Tag() {
		t.Errorf("the newer version touched the older one's section: %s", got)
	}
}

// A document leaves the bank only when no docs version holds it: a
// section these docs lack loses only their tag, and another version's
// section stays as it is.
func TestIndexRemovesOnlyItsOwnTag(t *testing.T) {
	f, cl := newFake(t)
	c := load(t)
	other := "docs-version:ffffffffffffffff"
	f.banks["mem-docs"] = map[string]*fakeDoc{
		"docs:gone#mine":   {tags: []string{c.Tag()}},
		"docs:gone#shared": {tags: []string{other, c.Tag()}},
		"docs:gone#theirs": {tags: []string{other}},
		"docs:gone#legacy": {tags: []string{}},
	}
	f.names["mem-docs"] = "backsight docs"
	res, err := Index(context.Background(), cl, "mem", c)
	if err != nil || res.Deleted != 2 || res.Untagged != 1 || res.Sent != 6 {
		t.Fatalf("%v %+v", err, res)
	}
	for id, want := range map[string]string{"docs:gone#mine": "missing", "docs:gone#legacy": "missing", "docs:gone#shared": other, "docs:gone#theirs": other} {
		if got := tagsOf(f, id); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
}

// Only `docs:` documents are ever deleted or retagged, whatever the bank
// holds.
func TestDiffTouchesOnlyDocsDocuments(t *testing.T) {
	c := load(t)
	p := diff(c, []hindsight.DocumentItem{{ID: "note:1", Tags: []string{c.Tag()}}, {ID: "conversation:s1"}, {ID: "docs:gone#x"}})
	if len(p.remove) != 1 || p.remove[0] != "docs:gone#x" || len(p.untag) != 0 {
		t.Errorf("remove %v untag %v", p.remove, p.untag)
	}
}

// A `<bank>-docs` someone else made (another name, or documents that are
// not backsight's sections) is never renamed, reconfigured or emptied:
// index refuses, search reads nothing more and says why (decision 0067,
// block 4 review).
func TestIndexAndSearchLeaveAForeignDocsBankAlone(t *testing.T) {
	for name, c := range map[string]struct {
		bankName string
		docs     map[string]*fakeDoc
		reason   string
	}{
		"another name":      {"team docs", map[string]*fakeDoc{"docs:x": {content: "theirs"}}, `its name is "team docs"`},
		"other documents":   {"backsight docs", map[string]*fakeDoc{"note:1": {content: "theirs"}, "docs:x": {}}, "1 document(s)"},
		"an empty bank too": {"", map[string]*fakeDoc{}, `its name is ""`},
	} {
		f, cl := newFake(t)
		f.banks["mem-docs"], f.names["mem-docs"] = c.docs, c.bankName
		_, err := Index(context.Background(), cl, "mem", load(t))
		var foreign *retain.ForeignBank
		if !errors.As(err, &foreign) || foreign.Bank != "mem-docs" || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: index %v", name, err)
		}
		_, _, err = Search(context.Background(), cl, "mem", load(t), "restore", 5)
		var se *SearchError
		if !errors.As(err, &se) || se.Stage != "check" || !errors.As(err, &foreign) {
			t.Errorf("%s: search %v", name, err)
		}
		_, err = PlanIndex(context.Background(), cl, "mem", load(t))
		if !errors.As(err, &foreign) {
			t.Errorf("%s: plan %v", name, err)
		}
		if w := f.writes(); len(w) != 0 || len(f.recall) != 0 || len(f.configs) != 0 || len(f.banks["mem-docs"]) != len(c.docs) || f.names["mem-docs"] != c.bankName {
			t.Errorf("%s: touched the bank: %v recalls %d configs %v", name, w, len(f.recall), f.configs)
		}
	}
}

// --plan lists what index would do, by document id, and writes nothing.
func TestPlanIndexListsTheWorkAndWritesNothing(t *testing.T) {
	f, cl := newFake(t)
	c1, c2 := load(t), edited(t)
	p, err := PlanIndex(context.Background(), cl, "mem", c1)
	if err != nil || p.Exists || len(p.Create) != 6 || p.Create[0] != "docs:backup" || len(p.Resend)+len(p.Retag)+len(p.Delete)+len(p.Untag) != 0 || p.Bank != "mem-docs" {
		t.Errorf("missing bank: %v %+v", err, p)
	}
	if w := f.writes(); len(w) != 0 || len(f.banks) != 0 {
		t.Fatalf("the plan wrote: %v", w)
	}
	if _, err := Index(context.Background(), cl, "mem", c1); err != nil {
		t.Fatal(err)
	}
	f.reset()
	p, err = PlanIndex(context.Background(), cl, "mem", c2)
	if err != nil || !p.Exists || strings.Join(p.Create, ",") != "docs:install#update" || strings.Join(p.Resend, ",") != "docs:install#requirements" ||
		len(p.Retag) != 4 || len(p.Delete)+len(p.Untag) != 0 || p.Unchanged != 0 || p.Sections != 6 || p.DocsHash != c2.Hash {
		t.Errorf("%v %+v", err, p)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("the plan wrote: %v", w)
	}
}

// Two clients refreshing at once: a document the other already deleted
// is no failure.
func TestIndexToleratesADocumentAlreadyDeleted(t *testing.T) {
	f, cl := newFake(t)
	f.banks["mem-docs"], f.names["mem-docs"] = map[string]*fakeDoc{}, "backsight docs"
	f.phantom = []string{"docs:old#gone"}
	res, err := Index(context.Background(), cl, "mem", load(t))
	if err != nil || res.Deleted != 1 || res.Sent != 6 {
		t.Errorf("%v %+v", err, res)
	}
	f.downAt = "DELETE /documents/"
	if _, err := Index(context.Background(), cl, "mem", load(t)); err == nil {
		t.Error("a delete the server refuses must fail the index")
	}
}

func TestSearchOnAFreshBankOnlyReads(t *testing.T) {
	f, cl := newFake(t)
	c := load(t)
	if _, err := Index(context.Background(), cl, "mem", c); err != nil {
		t.Fatal(err)
	}
	f.reset()
	f.rank = []string{"docs:install#uninstall", "docs:backup#restore-a-backup"}
	hits, refreshed, err := Search(context.Background(), cl, "mem", c, "comment désinstaller ?", 3)
	if err != nil || refreshed != nil {
		t.Fatalf("%v %+v", err, refreshed)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("a search on a fresh bank wrote: %v", w)
	}
	if got := strings.Join(refs(hits), ", "); got != "install#uninstall, backup#restore-a-backup, backup" {
		t.Errorf("hits %s: one per section, best first, limit 3", got)
	}
	if hits[0].Excerpt == "" || hits[0].Score != 0.9 || hits[0].Heading != "Uninstall" {
		t.Errorf("hit %+v", hits[0])
	}
	req := f.recall[0]
	if req["query"] != "comment désinstaller ?" || req["tags_match"] != "any_strict" || len(req["tags"].([]any)) != 1 || req["tags"].([]any)[0] != c.Tag() {
		t.Errorf("recall %v: only this docs version", req)
	}
}

func TestSearchRefreshesABankOnAnotherVersion(t *testing.T) {
	f, cl := newFake(t)
	old := load(t)
	if _, err := Index(context.Background(), cl, "mem", old); err != nil {
		t.Fatal(err)
	}
	fs := fixture()
	fs["backup.md"].Data = append(fs["backup.md"].Data, []byte("\n## Verify a backup\n\nRun `backsight backup verify <file>`.\n")...)
	c, _ := Load(fs)
	f.reset()
	f.rank = []string{"docs:backup#verify-a-backup"}
	hits, refreshed, err := Search(context.Background(), cl, "mem", c, "check an archive", 5)
	if err != nil || refreshed == nil || refreshed.Sent != 1 || refreshed.Retagged != 6 {
		t.Fatalf("%v %+v", err, refreshed)
	}
	if hits[0].Ref() != "backup#verify-a-backup" {
		t.Errorf("hits %v", refs(hits))
	}
}

func TestSearchIndexesAMissingBank(t *testing.T) {
	f, cl := newFake(t)
	c := load(t)
	hits, refreshed, err := Search(context.Background(), cl, "mem", c, "restore", 5)
	if err != nil || refreshed == nil || refreshed.Sent != 6 || len(hits) == 0 {
		t.Fatalf("%v %+v %v", err, refreshed, refs(hits))
	}
	if f.configs["mem-docs"]["retain_extraction_mode"] != "chunks" {
		t.Error("a missing bank must be created quiet before the retain")
	}
}

func TestSearchFailuresSayWhere(t *testing.T) {
	f, cl := newFake(t)
	c := load(t)
	f.downAt = "POST /memories"
	_, _, err := Search(context.Background(), cl, "mem", c, "restore", 5)
	var se *SearchError
	if !errors.As(err, &se) || se.Stage != "refresh" {
		t.Errorf("a failed refresh: %v", err)
	}
	f.downAt = ""
	if _, err := Index(context.Background(), cl, "mem", c); err != nil {
		t.Fatal(err)
	}
	f.downAt = "POST /memories/recall"
	if _, _, err := Search(context.Background(), cl, "mem", c, "restore", 5); !errors.As(err, &se) || se.Stage != "recall" {
		t.Errorf("a failed recall: %v", err)
	}
	f.downAt = "GET /documents"
	if _, _, err := Search(context.Background(), cl, "mem", c, "restore", 5); !errors.As(err, &se) || se.Stage != "check" {
		t.Errorf("a failed listing: %v", err)
	}
}
