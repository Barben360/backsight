package retain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// briefingServer keeps mental models as their JSON and applies PATCHes to
// them, recording each; a PATCH of an id in fail answers 500.
type briefingServer struct {
	mu      sync.Mutex
	models  map[string]map[string]any
	order   []string
	patches []string
	fail    map[string]bool
	gone    map[string]bool // a PATCH of it answers 404
	listErr bool
}

func (s *briefingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	const prefix = "/v1/default/banks/b/mental-models"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == prefix:
		if s.listErr {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var items []map[string]any
		for _, id := range s.order {
			m := map[string]any{"id": id}
			if r.URL.Query().Get("detail") == "content" { // like the server: metadata leaves it null
				m["trigger"] = s.models[id]["trigger"]
			}
			items = append(items, m)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, prefix+"/"):
		id := strings.TrimPrefix(r.URL.Path, prefix+"/")
		body, _ := io.ReadAll(r.Body)
		s.patches = append(s.patches, id+" "+string(body))
		if s.fail[id] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if s.gone[id] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		s.models[id]["trigger"] = in["trigger"]
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newBriefingServer(t *testing.T, models map[string]string) (*briefingServer, *hindsight.Client) {
	t.Helper()
	s := &briefingServer{models: map[string]map[string]any{}, fail: map[string]bool{}, gone: map[string]bool{}}
	for id, trigger := range models {
		var tr any
		if err := json.Unmarshal([]byte(trigger), &tr); err != nil {
			t.Fatal(err)
		}
		s.models[id] = map[string]any{"trigger": tr}
		s.order = append(s.order, id)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	cl, err := hindsight.New(srv.URL, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return s, cl
}

const (
	floorTrigger   = `{"budget":"low","mode":"full","refresh_after_consolidation":true,"min_refresh_interval_seconds":21600,"refresh_cron":null}`
	noFloorTrigger = `{"budget":"low","mode":"full","refresh_after_consolidation":true,"min_refresh_interval_seconds":null}`
	wantPatchBody  = `{"trigger":{"budget":"low","mode":"full","refresh_after_consolidation":true,"min_refresh_interval_seconds":21600}}`
)

func TestReconcileBriefingTriggersPatchesOnlyDriftedPagesOnce(t *testing.T) {
	ok, drifted := BriefingModelID("billing"), BriefingModelID("ops")
	s, cl := newBriefingServer(t, map[string]string{ok: floorTrigger, drifted: noFloorTrigger, "team-notes": noFloorTrigger})
	ctx := context.Background()
	r, err := ReconcileBriefingTriggers(ctx, cl, "b", BriefingTrigger(21600))
	if err != nil || r.Checked != 2 || len(r.Patched) != 1 || r.Patched[0] != drifted || len(r.Failed) != 0 {
		t.Fatalf("first run: %v %+v", err, r)
	}
	if len(s.patches) != 1 || s.patches[0] != drifted+" "+wantPatchBody {
		t.Fatalf("want one PATCH of %s with the whole trigger, got %q", drifted, s.patches)
	}
	r, err = ReconcileBriefingTriggers(ctx, cl, "b", BriefingTrigger(21600))
	if err != nil || r.Checked != 2 || len(r.Patched) != 0 || len(s.patches) != 1 {
		t.Errorf("second run must patch nothing: %v %+v %q", err, r, s.patches)
	}
	// Another floor configured: both pages move to it.
	r, _ = ReconcileBriefingTriggers(ctx, cl, "b", BriefingTrigger(0))
	if len(r.Patched) != 2 || !strings.Contains(s.patches[len(s.patches)-1], `"min_refresh_interval_seconds":0}`) {
		t.Errorf("floor 0: %+v %q", r, s.patches)
	}
	for _, p := range s.patches {
		if strings.HasPrefix(p, "team-notes ") {
			t.Errorf("a mental model that is not a briefing page was patched: %s", p)
		}
	}
}

func TestReconcileBriefingTriggersGoesOnPastAFailedPage(t *testing.T) {
	a, b, c := BriefingModelID("a"), BriefingModelID("b"), BriefingModelID("c")
	s, cl := newBriefingServer(t, map[string]string{a: noFloorTrigger, b: `null`, c: noFloorTrigger})
	s.order = []string{a, b, c}
	s.fail[a] = true
	r, err := ReconcileBriefingTriggers(context.Background(), cl, "b", BriefingTrigger(21600))
	if err != nil || r.Checked != 3 || len(r.Failed) != 1 || !strings.HasPrefix(r.Failed[0], a+": ") || len(r.Patched) != 2 || r.Patched[0] != b || r.Patched[1] != c {
		t.Errorf("a failed page must be reported and the others patched (a null trigger included): %v %+v", err, r)
	}
	s.listErr = true
	if _, err := ReconcileBriefingTriggers(context.Background(), cl, "b", BriefingTrigger(21600)); err == nil {
		t.Error("a failed listing must be an error")
	}
}

// A page deleted between the listing and its update is done, not failed:
// it would otherwise be retried for ever.
func TestReconcileBriefingTriggersCountsADeletedPageAsDone(t *testing.T) {
	a, b := BriefingModelID("a"), BriefingModelID("b")
	s, cl := newBriefingServer(t, map[string]string{a: noFloorTrigger, b: noFloorTrigger})
	s.order = []string{a, b}
	s.gone[a] = true
	r, err := ReconcileBriefingTriggers(context.Background(), cl, "b", BriefingTrigger(21600))
	if err != nil || len(r.Failed) != 0 || len(r.Gone) != 1 || r.Gone[0] != a || len(r.Patched) != 1 || r.Patched[0] != b {
		t.Errorf("deleted page: %v %+v", err, r)
	}
}

// With no floor wanted, a null floor is no floor: the server may echo a
// stored 0 as null, which must not be patched at every run.
func TestNoFloorWantedAcceptsANullFloor(t *testing.T) {
	zero, six := 0, 21600
	null := &hindsight.MentalModelTrigger{Budget: "low", Mode: "full", RefreshAfterConsolidation: true}
	if !BriefingTriggerIs(null, BriefingTrigger(0)) {
		t.Error("a null floor differs from no floor wanted")
	}
	if BriefingTriggerIs(null, BriefingTrigger(21600)) {
		t.Error("a null floor taken for the 6 h floor")
	}
	explicit := &hindsight.MentalModelTrigger{Budget: "low", Mode: "full", RefreshAfterConsolidation: true, MinRefreshIntervalSeconds: &zero}
	if !BriefingTriggerIs(explicit, BriefingTrigger(0)) || BriefingTriggerIs(explicit, BriefingTrigger(21600)) {
		t.Error("an explicit 0")
	}
	other := &hindsight.MentalModelTrigger{Budget: "mid", Mode: "full", RefreshAfterConsolidation: true}
	if BriefingTriggerIs(other, BriefingTrigger(0)) || BriefingTriggerIs(nil, BriefingTrigger(0)) {
		t.Error("another budget, or no trigger, taken for the wanted one")
	}
	if null.MinRefreshIntervalSeconds != nil || *BriefingTrigger(six).MinRefreshIntervalSeconds != six {
		t.Error("the comparison changed its arguments")
	}
	s, cl := newBriefingServer(t, map[string]string{BriefingModelID("a"): noFloorTrigger})
	if r, _ := ReconcileBriefingTriggers(context.Background(), cl, "b", BriefingTrigger(0)); len(r.Patched) != 0 || len(s.patches) != 0 {
		t.Errorf("floor 0 patched a page with a null floor: %+v", r)
	}
}
