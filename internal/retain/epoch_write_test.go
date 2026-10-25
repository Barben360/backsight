package retain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// banksHS keeps documents per bank. Like Hindsight 1.1.0, an unknown bank
// is 404 for reads, but a retain into it creates it with the default
// settings (an LLM extraction): the fake records that as "auto".
type banksHS struct {
	mu      sync.Mutex
	docs    map[string]map[string]json.RawMessage // bank → document → metadata
	configs map[string]map[string]any             // bank → config set
	calls   []string                              // "METHOD bank /sub"
}

func (h *banksHS) client(t *testing.T) *hindsight.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		rest, _ := strings.CutPrefix(r.URL.EscapedPath(), "/v1/default/banks/")
		esc, sub, _ := strings.Cut(rest, "/")
		bank, _ := url.PathUnescape(esc)
		h.calls = append(h.calls, r.Method+" "+bank+" /"+sub)
		docs, exists := h.docs[bank]
		switch {
		case r.Method == http.MethodPut && sub == "":
			if !exists {
				h.docs[bank] = map[string]json.RawMessage{}
			}
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && sub == "memories":
			var body struct {
				Async bool `json:"async"`
				Items []struct {
					DocumentID string          `json:"document_id"`
					Metadata   json.RawMessage `json:"metadata"`
				} `json:"items"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !exists {
				docs = map[string]json.RawMessage{}
				h.docs[bank] = docs
				h.calls = append(h.calls, "auto "+bank)
			}
			if !body.Async {
				h.calls[len(h.calls)-1] += " sync"
			}
			for _, it := range body.Items {
				docs[it.DocumentID] = it.Metadata
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		case !exists:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Bank not found"}`))
		case r.Method == http.MethodPatch && sub == "config":
			var body struct {
				Updates map[string]any `json:"updates"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if h.configs[bank] == nil {
				h.configs[bank] = map[string]any{}
			}
			for k, v := range body.Updates {
				h.configs[bank][k] = v
			}
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.HasPrefix(sub, "documents/"):
			id, _ := url.PathUnescape(strings.TrimPrefix(sub, "documents/"))
			meta, ok := docs[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail":"Document not found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"` + id + `","document_metadata":` + string(meta) + `}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	cl, _ := hindsight.New(srv.URL, "k", 5*time.Second)
	return cl
}

// The epoch restore writes lives in the memory bank's control bank. That
// bank is set up for chunks without observations before the first retain
// (a retain into a missing bank would create it with the LLM extraction).
// What ReadEpoch reads back is the same epoch: string metadata, RFC 3339
// UTC times, stored before WriteEpoch returns.
func TestEpochLivesInTheControlBank(t *testing.T) {
	h := &banksHS{docs: map[string]map[string]json.RawMessage{"claude-memory": {}}, configs: map[string]map[string]any{}}
	cl := h.client(t)
	ctx := context.Background()
	want := Epoch{Epoch: 3, RestoredAt: time.Date(2026, 10, 25, 14, 0, 0, 0, time.FixedZone("CEST", 7200)),
		BackupCreatedAt: time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)}
	if err := WriteEpoch(ctx, cl, "claude-memory", want); err != nil {
		t.Fatal(err)
	}
	if ControlBank("claude-memory") != "claude-memory-control" {
		t.Errorf("control bank %q", ControlBank("claude-memory"))
	}
	if got := strings.Join(h.calls, ", "); got != "PUT claude-memory-control /, PATCH claude-memory-control /config, POST claude-memory-control /memories sync" {
		t.Errorf("calls: %s", got)
	}
	if cfg := h.configs["claude-memory-control"]; cfg["retain_extraction_mode"] != "chunks" || cfg["enable_observations"] != false {
		t.Errorf("control bank config %v", cfg)
	}
	if len(h.docs["claude-memory"]) != 0 || len(h.configs["claude-memory"]) != 0 {
		t.Errorf("the memory bank was touched: %v %v", h.docs["claude-memory"], h.configs["claude-memory"])
	}
	var meta map[string]string
	_ = json.Unmarshal(h.docs["claude-memory-control"][EpochDocument], &meta)
	if meta["epoch"] != "3" || meta["restored_at"] != "2026-10-25T12:00:00Z" || meta["backup_created_at"] != "2026-10-25T04:00:00Z" || len(meta) != 3 {
		t.Errorf("metadata %v", meta)
	}
	got, err := ReadEpoch(ctx, cl, "claude-memory")
	if err != nil || got.Epoch != 3 || !got.RestoredAt.Equal(want.RestoredAt) || !got.BackupCreatedAt.Equal(want.BackupCreatedAt) {
		t.Errorf("read back %+v %v", got, err)
	}
}

// No control bank yet (every install before its first restore) is epoch
// 0, and an epoch document in the memory bank itself is not the epoch.
func TestNoControlBankIsEpochZero(t *testing.T) {
	stray := json.RawMessage(`{"epoch":"9","restored_at":"2026-10-25T12:00:00Z","backup_created_at":"2026-10-25T04:00:00Z"}`)
	h := &banksHS{docs: map[string]map[string]json.RawMessage{"claude-memory": {EpochDocument: stray}}, configs: map[string]map[string]any{}}
	got, err := ReadEpoch(context.Background(), h.client(t), "claude-memory")
	if err != nil || got.Epoch != 0 {
		t.Errorf("%+v %v", got, err)
	}
	if got := strings.Join(h.calls, ", "); got != "GET claude-memory-control /documents/backsight%3Aepoch" {
		t.Errorf("calls: %s", got)
	}
}
