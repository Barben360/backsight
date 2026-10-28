package retain

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestEnsureServerIDIsStable(t *testing.T) {
	h := &banksHS{docs: map[string]map[string]json.RawMessage{"claude-memory": {}}, configs: map[string]map[string]any{}}
	cl := h.client(t)
	ctx := context.Background()
	first, err := EnsureServerID(ctx, cl, "claude-memory")
	if err != nil || !uuidV4.MatchString(first) {
		t.Fatalf("first id %q %v", first, err)
	}
	second, err := EnsureServerID(ctx, cl, "claude-memory")
	if err != nil || second != first {
		t.Fatalf("second id %q %v, want %q", second, err, first)
	}
	if n := strings.Count(strings.Join(h.calls, ", "), "POST claude-memory-control /memories"); n != 1 {
		t.Errorf("%d retains, want 1: %v", n, h.calls)
	}
	var meta map[string]string
	_ = json.Unmarshal(h.docs["claude-memory-control"][ServerDocument], &meta)
	if ServerDocument != "backsight:server" || meta["server_id"] != first || meta["created_at"] == "" || len(h.docs["claude-memory-control"]) != 1 {
		t.Errorf("metadata %v in %v", meta, h.docs["claude-memory-control"])
	}
	if got, err := ReadServerID(ctx, cl, "claude-memory"); err != nil || got != first {
		t.Errorf("read back %q %v", got, err)
	}
	if len(h.docs["claude-memory"]) != 0 {
		t.Errorf("the memory bank was touched: %v", h.docs["claude-memory"])
	}
}

func TestReadServerIDMissingIsEmpty(t *testing.T) {
	h := &banksHS{docs: map[string]map[string]json.RawMessage{"claude-memory": {}}, configs: map[string]map[string]any{}}
	cl := h.client(t)
	if id, err := ReadServerID(context.Background(), cl, "claude-memory"); id != "" || err != nil {
		t.Errorf("no control bank: %q %v", id, err)
	}
	h.docs["claude-memory-control"] = map[string]json.RawMessage{}
	if id, err := ReadServerID(context.Background(), cl, "claude-memory"); id != "" || err != nil {
		t.Errorf("no document: %q %v", id, err)
	}
}

func TestReadServerIDBrokenMetadata(t *testing.T) {
	h := &banksHS{docs: map[string]map[string]json.RawMessage{"claude-memory-control": {ServerDocument: json.RawMessage(`{"created_at":"2026-10-27T00:00:00Z"}`)}}, configs: map[string]map[string]any{}}
	cl := h.client(t)
	if id, err := ReadServerID(context.Background(), cl, "claude-memory"); err == nil || id != "" {
		t.Errorf("got %q %v, want an error", id, err)
	}
	if id, err := EnsureServerID(context.Background(), cl, "claude-memory"); err == nil || id != "" {
		t.Errorf("ensure replaced a broken document: %q %v", id, err)
	}
}
