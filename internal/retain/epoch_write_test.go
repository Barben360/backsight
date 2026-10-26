package retain

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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
	names   map[string]string                     // bank → name (PUT); "" for a bank made otherwise
	calls   []string                              // "METHOD bank /sub"
}

func (h *banksHS) client(t *testing.T) *hindsight.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.URL.Path == "/v1/default/banks" {
			h.calls = append(h.calls, "LIST "+r.URL.Query().Get("q"))
			var banks []map[string]string
			for b := range h.docs {
				banks = append(banks, map[string]string{"bank_id": b, "name": h.names[b]})
			}
			raw, _ := json.Marshal(map[string]any{"banks": banks, "total": len(banks)})
			_, _ = w.Write(raw)
			return
		}
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
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if h.names == nil {
				h.names = map[string]string{}
			}
			h.names[bank] = body.Name
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
		case r.Method == http.MethodGet && sub == "documents":
			var items []map[string]any
			for id := range docs {
				items = append(items, map[string]any{"id": id, "tags": []string{}})
			}
			raw, _ := json.Marshal(map[string]any{"items": items, "total": len(items)})
			_, _ = w.Write(raw)
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
	if got := strings.Join(h.calls, ", "); got != "GET claude-memory-control /documents/backsight%3Aepoch, LIST claude-memory-control, PUT claude-memory-control /, PATCH claude-memory-control /config, POST claude-memory-control /memories sync" {
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
	if meta["epoch"] != "3" || meta["restored_at"] != "2026-10-25T12:00:00Z" || meta["backup_created_at"] != "2026-10-25T04:00:00Z" ||
		meta["history"] != `[{"epoch":3,"backup_created_at":"2026-10-25T04:00:00Z"}]` || len(meta) != 4 {
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

// Each write appends to the epoch history, which keeps the last 10: the
// document on the server (after a restore, the archive's), the history the
// caller passes (the server's before the restore) and the new epoch.
func TestEpochHistoryKeepsTheLastTen(t *testing.T) {
	h := &banksHS{docs: map[string]map[string]json.RawMessage{"claude-memory": {}}, configs: map[string]map[string]any{}}
	cl := h.client(t)
	ctx := context.Background()
	at := func(i int) time.Time { return time.Date(2026, 10, 1+i, 4, 0, 0, 0, time.UTC) }
	for i := 1; i <= 12; i++ {
		if err := WriteEpoch(ctx, cl, "claude-memory", Epoch{Epoch: i, RestoredAt: at(i).Add(time.Hour), BackupCreatedAt: at(i)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadEpoch(ctx, cl, "claude-memory")
	if err != nil || got.Epoch != 12 || len(got.History) != 10 || got.History[0].Epoch != 3 || got.History[9].Epoch != 12 || !got.History[0].BackupCreatedAt.Equal(at(3)) {
		t.Fatalf("%+v %v", got, err)
	}
	// A restore brought back an archive's document (epoch 2) and passes the
	// server's history from before: both are kept with the new epoch.
	h.docs["claude-memory-control"][EpochDocument] = json.RawMessage(`{"epoch":"2","restored_at":"2026-10-02T05:00:00Z","backup_created_at":"2026-10-02T04:00:00Z"}`)
	if err := WriteEpoch(ctx, cl, "claude-memory", Epoch{Epoch: 13, RestoredAt: at(14), BackupCreatedAt: at(2), History: []EpochEntry{{Epoch: 12, BackupCreatedAt: at(12)}}}); err != nil {
		t.Fatal(err)
	}
	got, _ = ReadEpoch(ctx, cl, "claude-memory")
	var epochs []int
	for _, e := range got.History {
		epochs = append(epochs, e.Epoch)
	}
	if !reflect.DeepEqual(epochs, []int{2, 12, 13}) {
		t.Errorf("history after a restore: %v", epochs)
	}
}

// A history that does not parse is an unreadable epoch, like a bad number.
func TestUnparsableEpochHistory(t *testing.T) {
	bad := json.RawMessage(`{"epoch":"2","restored_at":"2026-10-25T12:00:00Z","backup_created_at":"2026-10-25T04:00:00Z","history":"[{oops"}`)
	h := &banksHS{docs: map[string]map[string]json.RawMessage{"claude-memory-control": {EpochDocument: bad}}, configs: map[string]map[string]any{}}
	_, err := ReadEpoch(context.Background(), h.client(t), "claude-memory")
	var unreadable *EpochUnreadable
	if !errors.As(err, &unreadable) || !strings.Contains(err.Error(), "history") {
		t.Errorf("%v", err)
	}
}

// A `<bank>-control` backsight did not make (another name, or a document
// that is not one of its control records) is never reconfigured nor
// written: WriteEpoch refuses before any write (block 4 review).
func TestWriteEpochLeavesAForeignControlBankAlone(t *testing.T) {
	for name, c := range map[string]struct {
		bankName string
		docs     map[string]json.RawMessage
		reason   string
	}{
		"another name":     {"team notes", map[string]json.RawMessage{}, `its name is "team notes"`},
		"another document": {"backsight control", map[string]json.RawMessage{EpochDocument: json.RawMessage(`{}`), "notes:1": json.RawMessage(`{}`)}, "1 document(s)"},
	} {
		h := &banksHS{docs: map[string]map[string]json.RawMessage{"claude-memory-control": c.docs}, configs: map[string]map[string]any{},
			names: map[string]string{"claude-memory-control": c.bankName}}
		err := WriteEpoch(context.Background(), h.client(t), "claude-memory", Epoch{Epoch: 1, RestoredAt: time.Now(), BackupCreatedAt: time.Now()})
		var foreign *ForeignBank
		if !errors.As(err, &foreign) || foreign.Bank != "claude-memory-control" || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: %v", name, err)
		}
		for _, call := range h.calls {
			if !strings.HasPrefix(call, "GET ") && !strings.HasPrefix(call, "LIST ") {
				t.Errorf("%s: wrote to a bank that is not backsight's: %s", name, call)
			}
		}
		if len(h.configs) != 0 || len(h.docs["claude-memory-control"]) != len(c.docs) {
			t.Errorf("%s: configs %v docs %v", name, h.configs, h.docs)
		}
	}
}

// A control bank backsight creates for a bank it cannot prove its own is
// adopted: named apart, written to like its own, never proof of
// ownership (issue 285 review MED-2). An existing adopted bank stays
// adopted; an existing proven one stays proven.
func TestAdoptedControlBank(t *testing.T) {
	ctx := context.Background()
	h := &banksHS{docs: map[string]map[string]json.RawMessage{}, configs: map[string]map[string]any{}}
	cl := h.client(t)
	banks := func() map[string]string {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := map[string]string{}
		for k, v := range h.names {
			out[k] = v
		}
		return out
	}
	if err := WriteEpochAs(ctx, cl, "notes", Epoch{Epoch: 1}, true); err != nil {
		t.Fatal(err)
	}
	if got := banks()["notes-control"]; got != AdoptedControlMarker.Name {
		t.Fatalf("adopted control bank named %q", got)
	}
	if _, _, err := OwnedBank(ctx, cl, "notes-control", ControlMarker); err == nil {
		t.Error("an adopted control bank passes as proof")
	}
	// Later writes, adopted or not, keep it adopted.
	if _, err := WriteTombstones(ctx, cl, "notes", nil); err != nil {
		t.Fatal(err)
	}
	if got := banks()["notes-control"]; got != AdoptedControlMarker.Name {
		t.Errorf("after a plain write: %q", got)
	}
	if adopted, exists, err := OwnedControl(ctx, cl, "notes"); err != nil || !exists || !adopted {
		t.Errorf("OwnedControl: adopted %v exists %v %v", adopted, exists, err)
	}
	// A proven control bank stays proven when an adopting write reaches it.
	if err := WriteEpoch(ctx, cl, "mine", Epoch{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	if err := WriteEpochAs(ctx, cl, "mine", Epoch{Epoch: 2}, true); err != nil {
		t.Fatal(err)
	}
	if got := banks()["mine-control"]; got != ControlMarker.Name {
		t.Errorf("proven control bank renamed %q", got)
	}
}
