package retain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

// The tombstones document goes through WriteControlDocument and stays as
// it was: the same retain, byte for byte, into a control bank set up the
// same way.
func TestTombstonesStillWrite(t *testing.T) {
	h := &banksHS{docs: map[string]map[string]json.RawMessage{}, configs: map[string]map[string]any{}}
	cl := h.client(t)
	ts := []state.Tombstone{{Document: "conversation:s1", Session: "s1", LastUUID: "u9", PurgedAt: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}}
	n, err := WriteTombstones(context.Background(), cl, "notes", ts)
	if err != nil || n != 1 {
		t.Fatalf("WriteTombstones: %d %v", n, err)
	}
	want := `{"async":false,"items":[{"content":"purge tombstones of bank notes: 1 document(s) deleted by forget --purge","document_id":"backsight:tombstones","tags":[],"metadata":{"tombstones":"[{\"bank\":\"\",\"document\":\"conversation:s1\",\"session\":\"s1\",\"last_uuid\":\"u9\",\"purged_at\":\"2026-10-24T09:00:00Z\"}]"}}]}`
	if len(h.retains) != 1 || h.retains[0] != want {
		t.Errorf("retains %q\nwant %q", h.retains, want)
	}
	if got := h.names["notes-control"]; got != ControlMarker.Name {
		t.Errorf("control bank named %q", got)
	}
}

// A shared document goes into the memory bank's control bank, set up with
// its marker, synchronously and with only the metadata given; listing by
// prefix returns those documents only, with their metadata, and none
// while the control bank does not exist.
func TestShareWritesAndListsControlDocuments(t *testing.T) {
	ctx := context.Background()
	h := &banksHS{docs: map[string]map[string]json.RawMessage{}, configs: map[string]map[string]any{}}
	cl := h.client(t)
	if docs, err := ControlDocuments(ctx, cl, "notes", "backsight:feedback:"); err != nil || len(docs) != 0 {
		t.Fatalf("no control bank: %v %v", docs, err)
	}
	if err := WriteControlDocument(ctx, cl, "notes", "backsight:feedback:pc-a", "feedback counts of pc-a", map[string]string{"feedback": `{"v":1}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTombstones(ctx, cl, "notes", nil); err != nil {
		t.Fatal(err)
	}
	if got := h.names["notes-control"]; got != ControlMarker.Name {
		t.Errorf("control bank named %q", got)
	}
	want := `{"async":false,"items":[{"content":"feedback counts of pc-a","document_id":"backsight:feedback:pc-a","tags":[],"metadata":{"feedback":"{\"v\":1}"}}]}`
	if len(h.retains) == 0 || h.retains[0] != want {
		t.Errorf("retain %q\nwant %q", h.retains, want)
	}
	docs, err := ControlDocuments(ctx, cl, "notes", "backsight:feedback:")
	if err != nil || len(docs) != 1 || docs[0].ID != "backsight:feedback:pc-a" || docs[0].Metadata["feedback"] != `{"v":1}` {
		t.Errorf("listed %+v %v", docs, err)
	}
	// A foreign control bank is left alone.
	h.mu.Lock()
	h.docs["other-control"] = map[string]json.RawMessage{}
	h.names = map[string]string{"other-control": "someone's"}
	h.mu.Unlock()
	var foreign *ForeignBank
	if err := WriteControlDocument(ctx, cl, "other", "backsight:feedback:pc-a", "x", map[string]string{"feedback": "{}"}); !errors.As(err, &foreign) {
		t.Errorf("foreign control bank: %v", err)
	}
}

// DeleteControlDocument deletes that document alone; a document or a
// control bank already gone is nothing to do, and a control bank that is
// not backsight's gets no delete.
func TestDeleteControlDocument(t *testing.T) {
	ctx := context.Background()
	h := &banksHS{docs: map[string]map[string]json.RawMessage{}, configs: map[string]map[string]any{}}
	cl := h.client(t)
	if err := DeleteControlDocument(ctx, cl, "notes", "backsight:feedback:pc-a"); err != nil {
		t.Errorf("no control bank: %v", err)
	}
	for _, pc := range []string{"pc-a", "pc-b"} {
		if err := WriteControlDocument(ctx, cl, "notes", "backsight:feedback:"+pc, "x", map[string]string{"feedback": "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteControlDocument(ctx, cl, "notes", "backsight:feedback:pc-a"); err != nil {
		t.Fatal(err)
	}
	if docs, _ := ControlDocuments(ctx, cl, "notes", "backsight:feedback:"); len(docs) != 1 || docs[0].ID != "backsight:feedback:pc-b" {
		t.Errorf("left %+v", docs)
	}
	if err := DeleteControlDocument(ctx, cl, "notes", "backsight:feedback:pc-a"); err != nil {
		t.Errorf("already gone: %v", err)
	}
	h.mu.Lock()
	h.docs["other-control"] = map[string]json.RawMessage{"backsight:feedback:pc-a": json.RawMessage(`{}`)}
	h.names["other-control"] = "someone's"
	h.calls = nil
	h.mu.Unlock()
	var foreign *ForeignBank
	if err := DeleteControlDocument(ctx, cl, "other", "backsight:feedback:pc-a"); !errors.As(err, &foreign) {
		t.Errorf("foreign control bank: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.calls {
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("a delete reached someone else's bank: %s", c)
		}
	}
	if _, ok := h.docs["other-control"]["backsight:feedback:pc-a"]; !ok {
		t.Error("someone else's document deleted")
	}
}
