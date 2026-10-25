package retain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// The epoch restore writes is the one ReadEpoch reads back: string
// metadata, RFC 3339 UTC times, stored before WriteEpoch returns.
func TestWriteEpochIsReadBack(t *testing.T) {
	docs := map[string]json.RawMessage{}
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories"):
			var body struct {
				Async bool `json:"async"`
				Items []struct {
					DocumentID string          `json:"document_id"`
					Metadata   json.RawMessage `json:"metadata"`
				} `json:"items"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body)
			bodies = append(bodies, string(raw))
			for _, it := range body.Items {
				docs[it.DocumentID] = it.Metadata
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/documents/"):
			meta, ok := docs[r.URL.Path[strings.Index(r.URL.Path, "/documents/")+len("/documents/"):]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"id":"backsight:epoch","document_metadata":` + string(meta) + `}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	cl, _ := hindsight.New(srv.URL, "k", 5*time.Second)
	want := Epoch{Epoch: 3, RestoredAt: time.Date(2026, 10, 25, 14, 0, 0, 0, time.FixedZone("CEST", 7200)),
		BackupCreatedAt: time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)}
	if err := WriteEpoch(context.Background(), cl, "b", want); err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	_ = json.Unmarshal(docs[EpochDocument], &meta)
	if meta["epoch"] != "3" || meta["restored_at"] != "2026-10-25T12:00:00Z" || meta["backup_created_at"] != "2026-10-25T04:00:00Z" || len(meta) != 3 {
		t.Errorf("metadata %v", meta)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"async":false`) {
		t.Errorf("retain %q", bodies)
	}
	got, err := ReadEpoch(context.Background(), cl, "b")
	if err != nil || got.Epoch != 3 || !got.RestoredAt.Equal(want.RestoredAt) || !got.BackupCreatedAt.Equal(want.BackupCreatedAt) {
		t.Errorf("read back %+v %v", got, err)
	}
}
