package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// closedPort is a loopback port where no server answers: tests must never
// reach a real server (the author's dev install listens on 18888).
func closedPort(t *testing.T) int {
	t.Helper()
	ln := hangUpListener()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// hangUpListener is a port where nothing answers: it closes every
// connection at once. Unlike a port listened on then closed, no other
// process can take it while the test runs.
func hangUpListener() net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln
}

// devInstall writes a dev config pointing at a closed port and every secret
// under an isolated home; tests needing a server call pointAt.
func devInstall(t *testing.T, withLLMKey bool) string {
	t.Helper()
	home := isolate(t)
	_ = os.MkdirAll(filepath.Join(home, "backsight", "config"), 0o700)
	pointAt(t, home, closedPort(t))
	_ = config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error {
		config.GenerateMissing(s)
		if withLLMKey {
			return s.Set("llm_api_key", "fakellmkey")
		}
		return nil
	})
	return home
}

func composeDir(home string) string { return filepath.Join(home, "backsight", "data", "server") }

func TestServerRenderWritesFiles(t *testing.T) {
	home := devInstall(t, true)
	code, out, errOut := run(t, "server", "render", "--json")
	if code != output.OK {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if strings.Contains(out, "fakellmkey") {
		t.Error("a secret reached the output")
	}
	c, err := os.ReadFile(filepath.Join(composeDir(home), "compose.yaml"))
	if err != nil || !strings.Contains(string(c), `"127.0.0.1:`) || !strings.Contains(string(c), `:8888"`) {
		t.Errorf("%v\n%s", err, c)
	}
	if e, _ := os.ReadFile(filepath.Join(composeDir(home), ".env")); !strings.Contains(string(e), "fakellmkey") {
		t.Error(".env lacks the LLM key")
	}
	_, out, _ = run(t, "server", "render", "--json")
	var env struct {
		Data struct {
			Compose bool `json:"compose_changed"`
			Env     bool `json:"env_changed"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if env.Data.Compose || env.Data.Env {
		t.Errorf("second render changed files: %s", out)
	}
}

func TestServerRenderMissingSecretNeedsUser(t *testing.T) {
	devInstall(t, false)
	code, out, _ := run(t, "server", "render", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "llm_api_key") || !strings.Contains(out, "secret set") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestServerRenderWithoutConfig(t *testing.T) {
	isolate(t)
	if code, _, errOut := run(t, "server", "render"); code != output.NeedsUser || !strings.Contains(errOut, "run backsight setup") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

// With no config, the hint names setup (0067 block 4), the command that
// writes one; server install alone leaves Claude Code unwired.
func TestRecallPromptWithoutConfigNamesSetup(t *testing.T) {
	home := isolate(t)
	code, _, errOut := run(t, "recall-prompt", "--cwd", home, "a prompt")
	if code != output.NeedsUser || !strings.Contains(errOut, "run backsight setup") || strings.Contains(errOut, "server install") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

type fakeCompose struct {
	calls        [][]string
	ps           string
	failOn       string // subcommand that fails like a port clash
	volumeExists bool
	logs         string
}

func (f *fakeCompose) Run(_ context.Context, args ...string) ([]byte, error) {
	if args[0] == "volume" {
		if f.volumeExists {
			return []byte(`[{"Name":"backsight_pg_data"}]`), nil
		}
		return []byte("Error: no such volume"), errors.New("exit status 1")
	}
	f.calls = append(f.calls, args)
	for _, a := range args {
		if a == "logs" {
			return []byte(f.logs), nil
		}
	}
	for _, a := range args {
		if f.failOn != "" && a == f.failOn {
			return []byte("Error: port is already allocated"), errors.New("exit status 1")
		}
	}
	for _, a := range args {
		if a == "ps" {
			return []byte(f.ps), nil
		}
	}
	return nil, nil
}

func (f *fakeCompose) subcommands() []string {
	var out []string
	for _, c := range f.calls {
		for i, a := range c {
			if a == "compose.yaml" || strings.HasSuffix(a, "compose.yaml") {
				out = append(out, strings.Join(c[i+1:], " "))
				break
			}
		}
	}
	return out
}

// retained counts retain items the fake server accepted; llmInput is the
// bank's LLM input-token total the fake reports, raised by llmPerRetain on
// each accepted item.
var retained, llmInput, llmPerRetain, queued, processing, failRetains, statsCalls, statsFailAfter, retainStatus, recallStatus atomic.Int64

// statsDown makes the LLM usage endpoint fail; retainedTags records the
// tags of every accepted item.
var (
	statsDown    atomic.Bool
	llmThoughts  atomic.Int64 // thoughts tokens in /llm-requests/stats
	llmPeriod    string       // the period of the last /llm-requests/stats
	llmOldInput  atomic.Int64 // input tokens 10 days ago, in the 30d answer only
	extractFails atomic.Bool
	// queueAtRetain: pending + processing seen by each accepted retain.
	queueAtRetain []int64
	// recallAnswer answers every recall (JSON results array); recallTags
	// records the tags of each recall request; memories answers GET
	// /memories/{id}; projectEntities answers /tags and /entities.
	recallAnswer string
	recallTags   []string
	// recallSourceFacts is the "source_facts" JSON object added to every
	// recall answer when set; recallAskedSources counts the recalls that
	// asked for source facts in their include.
	recallSourceFacts  string
	recallAskedSources int
	recallQueries      []string
	recentAnswer       map[string]string // memories/list items by type ("" = any)
	recentByTag        map[string]string // memories/list items by tags (wins over recentAnswer)
	tagCounts          map[string]int    // /tags count by project (default 1)
	listAnswer         map[string]string // memories/list (paged listing) items by type; "invalidated:<type>" for the archive
	documentUnits      map[string]int    // GET /documents/{id} memory_unit_count, for documents not retained in the test
	curation           []string          // PATCH/DELETE calls on memories and documents
	recentDown         atomic.Bool
	recentHasEventTime atomic.Bool       // memories/list answers time_field=mentioned_at too
	mentalModels       map[string]string // id → mental model JSON
	mentalCalls        []string          // POST/DELETE on mental models, with the body
	mentalGetDelay     time.Duration     // a slow page read
	mentalCreateStatus atomic.Int32      // non-zero: answer page creation with this status
	docDeleteFails     atomic.Bool       // DELETE /documents/… answers 500
	docPatchFails      atomic.Bool       // PATCH /documents/… answers 503
	queueOnWrite       atomic.Bool       // a document PATCH or DELETE leaves an operation processing (a consolidation)
	docPatches         []string          // documents whose tags a PATCH replaced
	epochDoc           string            // GET /documents/backsight:epoch answers this metadata ("" = 404)
	epochStatus        atomic.Int32      // non-zero: reading the restore epoch answers this status
	serverIDStatus     atomic.Int32      // non-zero: reading the server id answers this status
	fakeAPIVersion     string            // what /version answers; "": it fails
	requireKey         atomic.Bool       // the fake checks the bearer key against acceptedKey
	acceptedKey        atomic.Value      // the API key the fake server runs on (a string), set by compose up
	refusedCalls       atomic.Int64      // requests the fake refused for their key
	bankCalls          []string          // "METHOD bank /rest" of every bank-scoped request
	retainBody         string            // body of a refused retain
	memories           map[string]string
	memoryStatus       map[string]int    // "<METHOD> <memory id>" → the status that call answers ("GET <id>/history" for a history)
	memoryHistory      map[string]string // GET /memories/{id}/history answer (a JSON list) by id; absent: 404
	projectEntities    map[string]string
	retainedTags       []string
	retainedContents   []string // the content of every accepted item
	// fakeDocCreated overrides a document's created_at (default: when the
	// fake accepted it).
	fakeDocCreated   map[string]time.Time
	bankConfigStatus atomic.Int32 // non-zero: bank claude-memory's /config answers this status
	// fakeHook, when not nil, sees every request before the fake answers
	// it (outside the fake's lock).
	fakeHook func(r *http.Request)
	// bankOverrides reads a bank's config overrides on the fake server,
	// after replacing them with set when it is not nil.
	bankOverrides func(bank string, set map[string]json.RawMessage) map[string]json.RawMessage
)

// fakeHindsight answers like a started server: health, version, banks and
// bank config (overrides kept in memory).
func fakeHindsight(t *testing.T) (port int) {
	t.Helper()
	// Shared counters start clean for every test.
	retained.Store(0)
	llmInput.Store(0)
	llmPerRetain.Store(0)
	queued.Store(0)
	processing.Store(0)
	failRetains.Store(0)
	statsCalls.Store(0)
	statsFailAfter.Store(0)
	retainStatus.Store(0)
	recallStatus.Store(0)
	statsDown.Store(false)
	llmThoughts.Store(0)
	llmOldInput.Store(0)
	llmPeriod = ""
	extractFails.Store(false)
	retainedTags, queueAtRetain, retainedContents = nil, nil, nil
	fakeDocCreated = map[string]time.Time{}
	recallAnswer, recallTags, memories, projectEntities = "[]", nil, map[string]string{}, map[string]string{}
	memoryStatus, memoryHistory = map[string]int{}, map[string]string{}
	recallQueries, recallSourceFacts, recallAskedSources = nil, "", 0
	recentAnswer, listAnswer, curation = map[string]string{}, map[string]string{}, nil
	recentByTag, tagCounts, documentUnits = map[string]string{}, map[string]int{}, map[string]int{}
	recentDown.Store(false)
	recentHasEventTime.Store(false)
	mentalModels, mentalCalls, mentalGetDelay = map[string]string{}, nil, 0
	mentalCreateStatus.Store(0)
	docDeleteFails.Store(false)
	docPatchFails.Store(false)
	queueOnWrite.Store(false)
	docPatches = nil
	epochDoc = ""
	epochStatus.Store(0)
	serverIDStatus.Store(0)
	fakeAPIVersion = "1.1.0"
	requireKey.Store(false)
	acceptedKey.Store("")
	refusedCalls.Store(0)
	bankCalls, retainBody = nil, `{"detail":"refused"}`
	fakeHook = nil
	var mu sync.Mutex
	banks := map[string]string{} // bank → name
	bankDocs := map[string]map[string]bool{}
	overrides := map[string]map[string]json.RawMessage{} // bank → its config overrides
	bankConfigStatus.Store(0)
	bankOverrides = func(bank string, set map[string]json.RawMessage) map[string]json.RawMessage {
		mu.Lock()
		defer mu.Unlock()
		if set != nil {
			overrides[bank] = set
		}
		out := map[string]json.RawMessage{}
		for k, v := range overrides[bank] {
			out[k] = v
		}
		return out
	}
	docs := map[string]json.RawMessage{}
	type docInfo struct {
		tags    []string
		text    string
		created time.Time
	}
	docInfos := map[string]docInfo{} // "<bank>/<id>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k, _ := acceptedKey.Load().(string); requireKey.Load() && r.Header.Get("Authorization") != "Bearer "+k {
			refusedCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"invalid API key"}`))
			return
		}
		if fakeHook != nil {
			fakeHook(r)
		}
		mu.Lock()
		defer mu.Unlock()
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v1/default/banks/"); ok {
			bank, sub, _ := strings.Cut(rest, "/")
			bankCalls = append(bankCalls, r.Method+" "+bank+" /"+sub)
		}
		switch {
		case strings.Contains(r.URL.Path, "/memories/") && memoryStatus[r.Method+" "+r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]] != 0:
			w.WriteHeader(memoryStatus[r.Method+" "+r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
			_, _ = w.Write([]byte(`{"detail":"internal error"}`))
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{"status":"healthy","database":"connected"}`))
		case r.URL.Path == "/version" && fakeAPIVersion == "":
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/version":
			_, _ = w.Write([]byte(`{"api_version":"` + fakeAPIVersion + `"}`))
		case r.URL.Path == "/v1/default/banks":
			var items []string
			for b, name := range banks {
				items = append(items, `{"bank_id":"`+b+`","name":"`+name+`"}`)
			}
			_, _ = w.Write([]byte(`{"banks":[` + strings.Join(items, ",") + `],"total":` + strconv.Itoa(len(items)) + `}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/default/banks/"):
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			banks[strings.TrimPrefix(r.URL.Path, "/v1/default/banks/")] = body.Name
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/documents") && r.URL.Query().Has("tags"):
			// Like the server: the documents carrying every tag asked,
			// created at or after start_date (time_field=created_at).
			bank := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/default/banks/"), "/documents")
			q := r.URL.Query()
			since, _ := time.Parse(time.RFC3339, q.Get("start_date"))
			var items []map[string]any
			for id := range bankDocs[bank] {
				in := docInfos[bank+"/"+id]
				created := in.created
				if c, ok := fakeDocCreated[id]; ok {
					created = c
				}
				if q.Get("time_field") == "created_at" && created.Before(since) {
					continue
				}
				keep := true
				for _, tg := range q["tags"] {
					keep = keep && slices.Contains(in.tags, tg)
				}
				if keep {
					var meta map[string]any
					_ = json.Unmarshal(docs[bank+"/"+id], &meta)
					items = append(items, map[string]any{"id": id, "tags": in.tags, "created_at": created.Format(time.RFC3339Nano), "document_metadata": meta})
				}
			}
			off, _ := strconv.Atoi(q.Get("offset"))
			lim, _ := strconv.Atoi(q.Get("limit"))
			page := items[min(off, len(items)):min(off+lim, len(items))]
			_ = json.NewEncoder(w).Encode(map[string]any{"items": page, "total": len(items)})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/documents"):
			bank := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/default/banks/"), "/documents")
			var items []string
			for id := range bankDocs[bank] {
				tags, _ := json.Marshal(docInfos[bank+"/"+id].tags)
				meta := docs[bank+"/"+id]
				if len(meta) == 0 {
					meta = json.RawMessage(`{}`)
				}
				items = append(items, `{"id":"`+id+`","tags":`+string(tags)+`,"document_metadata":`+string(meta)+`}`)
			}
			if epochDoc != "" && strings.HasSuffix(bank, "-control") && !bankDocs[bank]["backsight:epoch"] {
				items = append(items, `{"id":"backsight:epoch","tags":[]}`)
			}
			_, _ = w.Write([]byte(`{"items":[` + strings.Join(items, ",") + `],"total":` + strconv.Itoa(len(items)) + `}`))
		case strings.HasSuffix(r.URL.Path, "/config") && bankConfigStatus.Load() != 0 && configBank(r.URL.Path) == "claude-memory":
			w.WriteHeader(int(bankConfigStatus.Load()))
			_, _ = w.Write([]byte(`{"detail":"config refused"}`))
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/config"):
			var body struct {
				Updates map[string]json.RawMessage `json:"updates"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			bank := configBank(r.URL.Path)
			if overrides[bank] == nil {
				overrides[bank] = map[string]json.RawMessage{}
			}
			for k, v := range body.Updates {
				overrides[bank][k] = v
			}
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories/recall"):
			var body struct {
				Query   string         `json:"query"`
				Tags    []string       `json:"tags"`
				Include map[string]any `json:"include"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body.Include["source_facts"]; ok {
				recallAskedSources++
			}
			recallTags = append(recallTags, strings.Join(body.Tags, ","))
			recallQueries = append(recallQueries, body.Query)
			if st := recallStatus.Load(); st != 0 {
				w.WriteHeader(int(st))
				return
			}
			extra := ""
			if recallSourceFacts != "" {
				extra = `,"source_facts":` + recallSourceFacts
			}
			_, _ = w.Write([]byte(`{"results":` + recallAnswer + extra + `}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/memories/list") && r.URL.Query().Get("time_field") != "":
			// Like the server: only memories with a value on the asked
			// time field (the fake's items carry updated_at, and
			// mentioned_at when the test says so).
			if f := r.URL.Query().Get("time_field"); f != "updated_at" && !recentHasEventTime.Load() {
				_, _ = w.Write([]byte(`{"items":[]}`))
				return
			}
			if recentDown.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			items := recentAnswer[r.URL.Query().Get("type")]
			if !strings.HasPrefix(r.URL.Query().Get("tags"), "project:") {
				items = "" // like the server: the answer depends on the tag asked
			}
			if v, ok := recentByTag[r.URL.Query().Get("tags")]; ok {
				items = v
			}
			if items == "" {
				items = "[]"
			}
			_, _ = w.Write([]byte(`{"items":` + items + `}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/memories/list"):
			key := r.URL.Query().Get("type")
			if st := r.URL.Query().Get("state"); st != "" {
				key = st + ":" + key
			}
			items := listAnswer[key]
			if items == "" || r.URL.Query().Get("offset") != "0" {
				items = "[]"
			}
			if doc := r.URL.Query().Get("document_id"); doc != "" { // like the server: that document's facts only
				var all, keep []json.RawMessage
				_ = json.Unmarshal([]byte(items), &all)
				for _, it := range all {
					var m struct {
						DocumentID string `json:"document_id"`
					}
					if json.Unmarshal(it, &m) == nil && m.DocumentID == doc {
						keep = append(keep, it)
					}
				}
				raw, _ := json.Marshal(keep)
				if items = string(raw); items == "null" {
					items = "[]"
				}
			}
			_, _ = w.Write([]byte(`{"items":` + items + `,"total":` + strconv.Itoa(strings.Count(items, `"id"`)) + `}`))
		case strings.Contains(r.URL.Path, "/mental-models"):
			id := ""
			if i := strings.Index(r.URL.Path, "/mental-models/"); i >= 0 {
				id = r.URL.Path[i+len("/mental-models/"):]
			}
			body, _ := io.ReadAll(r.Body)
			switch {
			case r.Method == http.MethodGet && id == "":
				var items []string
				for _, m := range mentalModels {
					items = append(items, m)
				}
				_, _ = w.Write([]byte(`{"items":[` + strings.Join(items, ",") + `]}`))
			case r.Method == http.MethodGet:
				if mentalGetDelay > 0 {
					select {
					case <-time.After(mentalGetDelay):
					case <-r.Context().Done():
						return
					}
				}
				m, ok := mentalModels[id]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(m))
			case r.Method == http.MethodPost && mentalCreateStatus.Load() != 0:
				mentalCalls = append(mentalCalls, r.Method+" "+id+" "+string(body))
				w.WriteHeader(int(mentalCreateStatus.Load()))
			default:
				mentalCalls = append(mentalCalls, r.Method+" "+id+" "+string(body))
				if r.Method == http.MethodDelete {
					delete(mentalModels, id)
				}
				_, _ = w.Write([]byte(`{"operation_id":"op1"}`))
			}
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/documents/"):
			bank, id, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/default/banks/"), "/documents/")
			in, ok := docInfos[bank+"/"+id]
			switch {
			case docPatchFails.Load():
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			case !ok:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var body struct {
				Tags []string `json:"tags"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			in.tags = body.Tags
			docInfos[bank+"/"+id] = in
			docPatches = append(docPatches, id)
			if queueOnWrite.Load() {
				processing.Store(1)
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/documents/") && docDeleteFails.Load():
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPatch || r.Method == http.MethodDelete:
			body, _ := io.ReadAll(r.Body)
			if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/documents/") {
				bank, id, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/default/banks/"), "/documents/")
				delete(bankDocs[bank], id)
				delete(docs, bank+"/"+id)
				if queueOnWrite.Load() {
					processing.Store(1)
				}
			}
			// Strict like Hindsight 1.1.0: unknown memories are 404 and an
			// observation cannot be curated (400).
			if strings.Contains(r.URL.Path, "/memories/") {
				id := strings.TrimSuffix(r.URL.Path[strings.Index(r.URL.Path, "/memories/")+len("/memories/"):], "/observations")
				m, ok := memories[id]
				switch {
				case !ok:
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"detail":"Memory unit '` + id + `' not found"}`))
					return
				case r.Method == http.MethodPatch && strings.Contains(m, `"type":"observation"`):
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"detail":"only world/experience facts can be curated"}`))
					return
				}
			}
			curation = append(curation, r.Method+" "+r.URL.EscapedPath()+" "+string(body))
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reflect"):
			body, _ := io.ReadAll(r.Body)
			recallTags = append(recallTags, string(body))
			_, _ = w.Write([]byte(`{"text":"Billing keeps PostgreSQL 16.","usage":{"input_tokens":1200,"output_tokens":40}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/memories/") && strings.HasSuffix(r.URL.Path, "/history"):
			id := strings.TrimSuffix(r.URL.Path[strings.Index(r.URL.Path, "/memories/")+len("/memories/"):], "/history")
			h, ok := memoryHistory[id]
			switch {
			case memoryStatus["GET "+id+"/history"] != 0:
				w.WriteHeader(memoryStatus["GET "+id+"/history"])
			case !ok:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail":"Memory unit '` + id + `' not found"}`))
			default:
				_, _ = w.Write([]byte(h))
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/memories/") && !strings.HasSuffix(r.URL.Path, "/list"):
			m, ok := memories[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(m))
		case strings.HasSuffix(r.URL.Path, "/tags"):
			var items []string
			for p := range projectEntities {
				n := tagCounts[p]
				if n == 0 {
					n = 1
				}
				items = append(items, `{"tag":"project:`+p+`","count":`+strconv.Itoa(n)+`}`)
			}
			_, _ = w.Write([]byte(`{"items":[` + strings.Join(items, ",") + `],"total":` + strconv.Itoa(len(items)) + `}`))
		case strings.HasSuffix(r.URL.Path, "/entities"):
			_, _ = w.Write([]byte(`{"items":` + projectEntities[strings.TrimPrefix(r.URL.Query().Get("tags"), "project:")] + `}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories"):
			var body struct {
				Items []struct {
					DocumentID string          `json:"document_id"`
					Content    string          `json:"content"`
					Metadata   json.RawMessage `json:"metadata"`
					Tags       []string        `json:"tags"`
				} `json:"items"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if st := retainStatus.Load(); st != 0 {
				w.WriteHeader(int(st))
				_, _ = w.Write([]byte(retainBody))
				return
			}
			if failRetains.Load() > 0 && retained.Load() >= failRetains.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			queueAtRetain = append(queueAtRetain, queued.Load()+processing.Load())
			bank := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/default/banks/"), "/memories")
			if bankDocs[bank] == nil {
				bankDocs[bank] = map[string]bool{}
			}
			for _, it := range body.Items {
				bankDocs[bank][it.DocumentID] = true
				docs[bank+"/"+it.DocumentID] = it.Metadata // documents are per bank, like the server's
				docInfos[bank+"/"+it.DocumentID] = docInfo{tags: it.Tags, text: it.Content, created: time.Now().UTC()}
				retained.Add(1)
				llmInput.Add(llmPerRetain.Load())
				retainedTags = append(retainedTags, strings.Join(it.Tags, ","))
				retainedContents = append(retainedContents, it.Content)
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/documents/"):
			id := r.URL.Path[strings.Index(r.URL.Path, "/documents/")+len("/documents/"):]
			if id == "backsight:epoch" && epochStatus.Load() != 0 {
				w.WriteHeader(int(epochStatus.Load()))
				return
			}
			if id == "backsight:server" && serverIDStatus.Load() != 0 {
				w.WriteHeader(int(serverIDStatus.Load()))
				return
			}
			if id == "backsight:epoch" && epochDoc != "" {
				_, _ = w.Write([]byte(`{"id":"` + id + `","document_metadata":` + epochDoc + `}`))
				return
			}
			// per bank: "<bank>/<id>", or the bare id for claude-memory and
			// verify's scratch bank (named at random)
			bank, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/default/banks/"), "/documents/")
			n, ok := documentUnits[bank+"/"+id]
			if !ok && (bank == "claude-memory" || strings.HasPrefix(bank, "backsight-verify-")) {
				n, ok = documentUnits[id]
			}
			if ok {
				_, _ = w.Write([]byte(`{"id":"` + id + `","memory_unit_count":` + strconv.Itoa(n) + `}`))
				return
			}
			bank, _, _ = strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/default/banks/"), "/documents/")
			meta, ok := docs[bank+"/"+id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			text, _ := json.Marshal(docInfos[bank+"/"+id].text)
			tags, _ := json.Marshal(docInfos[bank+"/"+id].tags)
			_, _ = w.Write([]byte(`{"id":"` + id + `","document_metadata":` + string(meta) + `,"original_text":` + string(text) + `,"tags":` + string(tags) + `}`))
		case strings.HasSuffix(r.URL.Path, "/llm-requests/stats"):
			calls := statsCalls.Add(1)
			if statsDown.Load() || (statsFailAfter.Load() > 0 && calls > statsFailAfter.Load()) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			// Like Hindsight 1.1.0: 1d or 30d; any other period is 7 days.
			llmPeriod = r.URL.Query().Get("period")
			day := time.Now().UTC().Format("2006-01-02")
			buckets := `{"time":"` + day + `T00:00:00+00:00","tokens":{"input":` + strconv.FormatInt(llmInput.Load(), 10) +
				`,"output":0,"cached":0,"thoughts":` + strconv.FormatInt(llmThoughts.Load(), 10) + `}}`
			if llmPeriod == "30d" {
				old := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")
				buckets += `,{"time":"` + old + `T00:00:00+00:00","tokens":{"input":` + strconv.FormatInt(llmOldInput.Load(), 10) + `,"output":0,"cached":0,"thoughts":0}}`
			}
			_, _ = w.Write([]byte(`{"buckets":[` + buckets + `]}`))
		case strings.HasSuffix(r.URL.Path, "/operations"):
			// Pending and processing are reported separately, like Hindsight;
			// only pending drains as it is polled.
			var n int64
			switch r.URL.Query().Get("status") {
			case "pending":
				n = queued.Load()
				if n > 0 {
					queued.Add(-1)
				}
			case "processing":
				n = processing.Load()
			}
			_, _ = w.Write([]byte(`{"total":` + strconv.FormatInt(n, 10) + `,"operations":[]}`))
		case strings.HasSuffix(r.URL.Path, "/stats"):
			_, _ = w.Write([]byte(`{"total_nodes":0,"pending_operations":0,"pending_consolidation":0}`))
		case strings.HasSuffix(r.URL.Path, "/memories/dry-run-extract") && extractFails.Load():
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"detail":"provider said: invalid api key fakellmkey"}`))
		case strings.HasSuffix(r.URL.Path, "/memories/dry-run-extract"):
			_, _ = w.Write([]byte(`{"facts":[{"text":"The billing service keeps PostgreSQL 16 for partial indexes.","fact_type":"world"}],"usage":{"input_tokens":900,"output_tokens":40}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/config"):
			ov, _ := json.Marshal(overrides[configBank(r.URL.Path)])
			_, _ = w.Write([]byte(`{"config":{},"overrides":` + string(ov) + `}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ = strconv.Atoi(u.Port())
	return port
}

// configBank is the bank of a /v1/default/banks/<bank>/config path.
func configBank(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/v1/default/banks/"), "/config")
}

// pointAt rewrites the dev config to use the fake server's port.
func pointAt(t *testing.T, home string, port int) {
	t.Helper()
	cfgPath := filepath.Join(home, "backsight", "config", "config.toml")
	_ = os.WriteFile(cfgPath, []byte("[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\n"), 0o600)
}

func withCompose(t *testing.T) *fakeCompose {
	t.Helper()
	f := &fakeCompose{ps: `{"Service":"db","State":"running","Health":"healthy"}` + "\n" + `{"Service":"hindsight","State":"running","Health":""}`}
	old, oldPoll := composeRunner, healthPoll
	composeRunner, healthPoll = f, 10*time.Millisecond
	t.Cleanup(func() { composeRunner, healthPoll = old, oldPoll })
	return f
}

func TestServerInstallFromScratch(t *testing.T) {
	home := isolate(t)
	f := withCompose(t)
	port := fakeHindsight(t)
	t.Setenv("LLM", "fakellmkey")
	if code, _, errOut := run(t, "secret", "set", "llm_api_key", "--from-env", "LLM"); code != output.OK {
		t.Fatal(errOut)
	}
	code, out, errOut := run(t, "server", "install", "--api-port", strconv.Itoa(port), "--ui-port", "19999")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(f.subcommands(), " | "); got != "pull | up -d --wait" {
		t.Errorf("compose calls: %s", got)
	}
	cfg, err := config.Load(filepath.Join(home, "backsight", "config", "config.toml"), pathsOf(home), home)
	if err != nil || cfg.Server.UIPort != 19999 || !strings.HasSuffix(cfg.Server.URL, ":"+strconv.Itoa(port)) {
		t.Errorf("%+v %v", cfg.Server, err)
	}
	s, _ := config.LoadSecrets(secretsFile(home))
	if s.APIKey == "" || s.UIKey == "" || s.DBPassword == "" || s.LLMAPIKey != "fakellmkey" {
		t.Errorf("secrets: %v", s.Names())
	}
	if !strings.Contains(out, "bank") || !strings.Contains(out, "provider") || !strings.Contains(out, "healthy") || strings.Contains(out+errOut, s.APIKey) {
		t.Errorf("output:\n%s", out)
	}
	// Idempotent: a second run keeps config and secrets.
	before := s
	if code, _, errOut := run(t, "server", "install"); code != output.OK {
		t.Fatalf("rerun: %s", errOut)
	}
	if s2, _ := config.LoadSecrets(secretsFile(home)); s2 != before {
		t.Error("a rerun regenerated secrets")
	}
}

func TestServerInstallStopsForTheLLMKey(t *testing.T) {
	home := isolate(t)
	f := withCompose(t)
	code, out, _ := run(t, "server", "install", "--json")
	var env struct {
		Error output.Error `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.NeedsUser || strings.Join(env.Error.Secrets, ",") != "llm_api_key" || env.Error.Resume != "backsight server install" ||
		len(env.Error.Actions) != 1 || env.Error.Actions[0].Cmd != "backsight secret set llm_api_key" || env.Error.Actions[0].Kind != "run_as_user" {
		t.Errorf("exit %d\n%s", code, out)
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing may start without the key: %v", f.calls)
	}
	// Config and generated secrets are kept, so the rerun resumes.
	if s, _ := config.LoadSecrets(secretsFile(home)); s.APIKey == "" {
		t.Error("generated secrets were not kept")
	}
}

func TestServerInstallRefusesConflictingFlags(t *testing.T) {
	devInstall(t, true)
	withCompose(t)
	if code, _, errOut := run(t, "server", "install", "--api-port", "28888"); code != output.Usage || !strings.Contains(errOut, "server.url") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestServerInstallPlanDoesNothing(t *testing.T) {
	home := isolate(t)
	f := withCompose(t)
	t.Setenv("LLM", "fakellmkey")
	run(t, "secret", "set", "llm_api_key", "--from-env", "LLM")
	code, out, _ := run(t, "server", "install", "--plan")
	if code != output.OK || !strings.Contains(out, "would write") || !strings.Contains(out, "would generate api_key, db_password, ui_key") || len(f.calls) != 0 {
		t.Errorf("exit %d calls %v\n%s", code, f.calls, out)
	}
	if _, err := os.Stat(filepath.Join(home, "backsight", "config", "config.toml")); err == nil {
		t.Error("--plan wrote the config")
	}
}

func TestServerInstallComposeFailure(t *testing.T) {
	isolate(t)
	f := withCompose(t)
	f.failOn = "up"
	t.Setenv("LLM", "fakellmkey")
	run(t, "secret", "set", "llm_api_key", "--from-env", "LLM")
	code, _, errOut := run(t, "server", "install", "--api-port", strconv.Itoa(closedPort(t)), "--ui-port", "19999")
	if code != output.Blocked || !strings.Contains(errOut, "port is already allocated") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestServerInstallHealthTimeout(t *testing.T) {
	isolate(t)
	withCompose(t)
	old := healthTimeout
	healthTimeout = 50 * time.Millisecond
	t.Cleanup(func() { healthTimeout = old })
	t.Setenv("LLM", "fakellmkey")
	run(t, "secret", "set", "llm_api_key", "--from-env", "LLM")
	code, _, errOut := run(t, "server", "install", "--api-port", strconv.Itoa(closedPort(t)), "--ui-port", "19999")
	if code != output.Blocked || !strings.Contains(errOut, "health") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestServerStatus(t *testing.T) {
	home := devInstall(t, true)
	withCompose(t)
	pointAt(t, home, fakeHindsight(t))
	code, out, _ := run(t, "server", "status", "--json")
	var env struct {
		Data struct {
			Containers []server.Container `json:"containers"`
			API        struct {
				Healthy bool   `json:"healthy"`
				Version string `json:"version"`
			} `json:"api"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d %v\n%s", code, err, out)
	}
	if len(env.Data.Containers) != 2 || !env.Data.API.Healthy || env.Data.API.Version != "1.1.0" {
		t.Errorf("%s", out)
	}
}

func TestServerStatusAPIDown(t *testing.T) {
	devInstall(t, true) // a closed port
	f := withCompose(t)
	f.ps = ""
	code, out, _ := run(t, "server", "status")
	if code != output.Blocked || !strings.Contains(out, "not running") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestServerStartStop(t *testing.T) {
	devInstall(t, true)
	f := withCompose(t)
	if code, _, errOut := run(t, "server", "start"); code != output.OK {
		t.Fatal(errOut)
	}
	if code, _, errOut := run(t, "server", "stop"); code != output.OK {
		t.Fatal(errOut)
	}
	if got := strings.Join(f.subcommands(), " | "); got != "up -d --wait | stop" {
		t.Errorf("%s", got)
	}
}

func pathsOf(home string) paths.Dirs {
	d, _ := paths.Resolve("linux", home, func(k string) string {
		if k == "BACKSIGHT_HOME" {
			return filepath.Join(home, "backsight")
		}
		return ""
	})
	return d
}

func TestErrorEnvelopeNamesTheFullCommand(t *testing.T) {
	isolate(t)
	_, out, _ := run(t, "server", "render", "--json")
	if !strings.Contains(out, `"command": "server render"`) {
		t.Errorf("%s", out)
	}
}

func TestBankApply(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	code, out, errOut := run(t, "bank", "apply", "--json")
	if code != output.OK || !strings.Contains(out, `"created": true`) || !strings.Contains(out, `"sent": true`) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	st, _ := state.Load(filepath.Join(home, "backsight", "state", "state.json"))
	if st.BankConfigHash != server.TemplateHash() {
		t.Errorf("hash not recorded: %q", st.BankConfigHash)
	}
	_, out, _ = run(t, "bank", "apply", "--json")
	if !strings.Contains(out, `"sent": false`) {
		t.Errorf("an unchanged template was re-sent:\n%s", out)
	}
	_, out, _ = run(t, "bank", "apply", "--plan")
	if !strings.Contains(out, "unchanged") {
		t.Errorf("plan: %s", out)
	}
}

func TestBankApplyServerDown(t *testing.T) {
	devInstall(t, true)
	if code, _, errOut := run(t, "bank", "apply"); code != output.Blocked || !strings.Contains(errOut, "unreachable") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestServerLLMCheck(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	code, out, errOut := run(t, "server", "llm", "check")
	if code != output.OK || !strings.Contains(out, "1 fact") || !strings.Contains(out, "PostgreSQL 16") {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestServerLLMCheckServerDown(t *testing.T) {
	devInstall(t, true)
	if code, _, errOut := run(t, "server", "llm", "check"); code != output.Blocked || !strings.Contains(errOut, "LLM") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

// A state hash with no bank on the server (volume wiped) must recreate and
// reconfigure the bank.
func TestBankApplyRecreatesAWipedBank(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	_ = state.Update(d, "test", func(s *state.State) error { s.BankConfigHash = server.TemplateHash(); return nil })
	_, out, _ := run(t, "bank", "apply", "--json")
	if !strings.Contains(out, `"created": true`) || !strings.Contains(out, `"sent": true`) {
		t.Errorf("%s", out)
	}
}

func TestInstallRefusesNewDBPasswordForAnExistingVolume(t *testing.T) {
	isolate(t)
	f := withCompose(t)
	f.volumeExists = true
	t.Setenv("LLM", "fakellmkey")
	run(t, "secret", "set", "llm_api_key", "--from-env", "LLM")
	code, _, errOut := run(t, "server", "install", "--api-port", strconv.Itoa(closedPort(t)), "--ui-port", "19999")
	if code != output.Blocked || !strings.Contains(errOut, "backsight_pg_data") || !strings.Contains(errOut, "restore the secrets.toml") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing may start: %v", f.calls)
	}
}

func TestInstallHeldLock(t *testing.T) {
	home := devInstall(t, true)
	withCompose(t)
	l, err := state.TryAcquire(filepath.Join(pathsOf(home).LocksDir(), "server.lock"), "server install")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	for _, args := range [][]string{{"server", "install"}, {"server", "start"}, {"server", "render"}} {
		if code, _, errOut := run(t, args...); code != output.Blocked || !strings.Contains(errOut, "server install") {
			t.Errorf("%v: exit %d: %s", args, code, errOut)
		}
	}
}

// A crash loop fails fast with the container's last lines, secrets masked.
func TestInstallCrashLoopFailsFastWithRedactedLogs(t *testing.T) {
	home := devInstall(t, true)
	f := withCompose(t)
	f.ps = `{"Service":"hindsight","State":"restarting","Health":""}`
	s, _ := config.LoadSecrets(secretsFile(home))
	f.logs = "FATAL: password authentication failed; url postgresql://hindsight:" + s.DBPassword + "@db"
	start := time.Now()
	code, _, errOut := run(t, "server", "install")
	if code != output.Blocked || !strings.Contains(errOut, "restarting") || !strings.Contains(errOut, "password authentication failed") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if strings.Contains(errOut, s.DBPassword) {
		t.Error("the database password reached the output")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("a crash loop must not wait for the health timeout")
	}
}

func TestInstallWarnsAboutRemoteDockerHost(t *testing.T) {
	home := devInstall(t, true)
	withCompose(t)
	pointAt(t, home, fakeHindsight(t))
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	_, out, _ := run(t, "server", "install")
	if !strings.Contains(out, "warning  DOCKER_HOST=tcp://10.0.0.5:2375") {
		t.Errorf("%s", out)
	}
}

func TestServerInstallPlanRunsTheRealChecks(t *testing.T) {
	devInstall(t, true)
	withCompose(t)
	if code, _, _ := run(t, "server", "install", "--plan", "--ui-port", "29999"); code != output.Usage {
		t.Errorf("conflicting flag under --plan: exit %d", code)
	}
	isolate(t)
	if code, out, _ := run(t, "server", "install", "--plan", "--json"); code != output.NeedsUser || !strings.Contains(out, `"resume": "backsight server install"`) {
		t.Errorf("missing key under --plan: exit %d\n%s", code, out)
	}
}

func TestServerCommandsRefuseRemoteMode(t *testing.T) {
	home := isolate(t)
	withCompose(t)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	_ = os.MkdirAll(filepath.Dir(cfg), 0o700)
	_ = os.WriteFile(cfg, []byte("[server]\nmode = \"remote\"\nurl = \"https://mem.example\"\n"), 0o600)
	for _, args := range [][]string{{"server", "render"}, {"server", "start"}, {"server", "stop"}, {"server", "status"}, {"server", "install"}} {
		if code, _, errOut := run(t, args...); code != output.Usage || !strings.Contains(errOut, "https://mem.example") || !strings.Contains(errOut, "backsight "+strings.Join(args, " ")) {
			t.Errorf("%v: exit %d: %s", args, code, errOut)
		}
	}
}

func TestServerInstallStreamsStepsInTextMode(t *testing.T) {
	home := devInstall(t, true)
	withCompose(t)
	pointAt(t, home, fakeHindsight(t))
	_, out, _ := run(t, "server", "install")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 10 || !strings.HasPrefix(lines[0], "skipped  config") || !strings.HasPrefix(lines[7], "done     server-id: ") || !strings.HasPrefix(lines[8], "done     provider") || !strings.Contains(out, "UI http://127.0.0.1:19999") {
		t.Errorf("%s", out)
	}
	if strings.Count(out, "skipped  config:") != 1 || strings.Count(out, "done     provider:") != 1 {
		t.Errorf("each step must be printed once:\n%s", out)
	}
}

// The server id is a step, not a gate: when it cannot be read the install
// finishes with a warning.
func TestServerInstallServerIDFailureIsAWarning(t *testing.T) {
	home := devInstall(t, true)
	withCompose(t)
	pointAt(t, home, fakeHindsight(t))
	serverIDStatus.Store(http.StatusInternalServerError)
	code, out, _ := run(t, "server", "install")
	if code != output.OK || !strings.Contains(out, "warning  server-id: ") || !strings.Contains(out, "done     provider") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestServerInstallFailureKeepsTheStepsDone(t *testing.T) {
	isolate(t)
	withCompose(t)
	old := healthTimeout
	healthTimeout = 50 * time.Millisecond
	t.Cleanup(func() { healthTimeout = old })
	t.Setenv("LLM", "fakellmkey")
	run(t, "secret", "set", "llm_api_key", "--from-env", "LLM")
	code, out, _ := run(t, "server", "install", "--json", "--api-port", strconv.Itoa(closedPort(t)), "--ui-port", "19999")
	var env struct {
		OK   bool          `json:"ok"`
		Data installResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.Blocked || env.OK || len(env.Data.Steps) < 3 || env.Data.Steps[len(env.Data.Steps)-1].Name != "start" {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestServerStartAndStopPlanTouchNothing(t *testing.T) {
	home := devInstall(t, true)
	f := withCompose(t)
	for _, args := range [][]string{{"server", "start", "--plan"}, {"server", "stop", "--plan"}} {
		code, out, _ := run(t, args...)
		if code != output.OK || !strings.Contains(out, "would") {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("docker called: %v", f.calls)
	}
	if _, err := os.Stat(filepath.Join(pathsOf(home).Data, "server", ".env")); !os.IsNotExist(err) {
		t.Error("start --plan rendered the server files")
	}
}

func TestServerLLMCheckMasksSecretsInErrors(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	extractFails.Store(true)
	code, out, errOut := run(t, "server", "llm", "check")
	if code == output.OK || strings.Contains(out+errOut, "fakellmkey") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

// A second install's server commands act on its own Compose project and
// volume, never the first's (block 4 review).
func TestServerCommandsUseTheConfiguredProject(t *testing.T) {
	home := devInstall(t, true)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(raw), "[server]\n", "[server]\nproject = \"bs-fresh\"\n", 1)), 0o600)
	f := withCompose(t)
	if code, out, errOut := run(t, "server", "stop"); code != output.OK {
		t.Fatalf("exit %d %s %s", code, out, errOut)
	}
	if len(f.calls) == 0 {
		t.Fatal("no compose call")
	}
	for _, c := range f.calls {
		if j := strings.Join(c, " "); c[0] == "compose" && !strings.Contains(j, "-p bs-fresh ") {
			t.Errorf("compose call outside the project: %s", j)
		}
	}
	// Without db_password, install checks the project's volume and names it.
	_ = os.Remove(secretsFile(home))
	f.volumeExists = true
	code, out, errOut := run(t, "server", "install")
	if code != output.Blocked || !strings.Contains(out+errOut, "bs-fresh_pg_data") || strings.Contains(out+errOut, "backsight_pg_data") {
		t.Errorf("exit %d: %s %s", code, out, errOut)
	}
}

// The marker can vanish between the check and the refusal's own look: the
// lock must still refuse, never return (nil, "", nil), and leave no lock.
func TestMaintenanceLockRefusesWhenTheMarkerVanishes(t *testing.T) {
	home := legacyInstall(t)
	d := pathsOf(home)
	old := restoreIncomplete
	t.Cleanup(func() { restoreIncomplete = old })
	restoreIncomplete = func(paths.Dirs) bool { return true }
	unlock, _, err := serverMaintenanceLock(d, "server expose")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != output.Blocked || !strings.Contains(oe.Message, "restore-incomplete.json") {
		t.Fatalf("err = %v", err)
	}
	if unlock != nil {
		t.Error("an unlock came back with the refusal")
	}
	if h := state.Inspect(d.MaintenanceLock()); h != nil {
		t.Errorf("maintenance lock left: %+v", h)
	}
}
