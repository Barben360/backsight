package hindsight

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func shape(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "shapes", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// shapeServer serves the shape files by path, counting requests by method.
func shapeServer(t *testing.T, routes map[string]string) (*ReadOnly, func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method]++
		mu.Unlock()
		file := routes[r.URL.Path]
		if r.URL.Path == "/v1/default/banks/b/tags" && r.URL.Query().Get("offset") != "0" {
			file = "tags_2.json"
		}
		if file == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(shape(t, file))
	}))
	t.Cleanup(srv.Close)
	ro, err := NewReadOnly(srv.URL, "k", "b", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return ro, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return map[string]int{"GET": seen["GET"], "total": len(seen)}
	}
}

func TestReadOnlyRefusesOtherMethods(t *testing.T) {
	var mu sync.Mutex
	n := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n[r.Method]++
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	ro, err := NewReadOnly(srv.URL, "k", "b", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD"} {
		err := ro.c.do(context.Background(), m, "/x", nil, map[string]string{"a": "b"}, nil)
		if !errors.Is(err, ErrNotGET) {
			t.Errorf("%s: err = %v, want ErrNotGET", m, err)
		}
	}
	mu.Lock()
	if len(n) != 0 {
		t.Fatalf("server saw requests %v, want none", n)
	}
	mu.Unlock()
	if err := ro.c.do(context.Background(), "GET", "/x", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n["GET"] != 1 || len(n) != 1 {
		t.Fatalf("server saw %v, want one GET", n)
	}
}

func TestReadOnlyExposesOnlyReads(t *testing.T) {
	var got []string
	typ := reflect.TypeOf(&ReadOnly{})
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	want := []string{"BankStats", "LLMStats", "MemoriesTimeseries", "Memory", "OperationsPage", "ProjectTags", "Version"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

func TestReadOnlyDecodesTheShapes(t *testing.T) {
	const bp = "/v1/default/banks/b"
	ro, seen := shapeServer(t, map[string]string{
		"/version": "version.json", bp + "/stats": "stats.json", bp + "/stats/memories-timeseries": "timeseries.json",
		bp + "/llm-requests/stats": "llm_stats.json", bp + "/operations": "operations.json",
		bp + "/tags": "tags_1.json", bp + "/memories/m1": "memory.json",
	})
	ctx := context.Background()

	v, err := ro.Version(ctx)
	if err != nil || v.APIVersion != "1.1.0" {
		t.Fatalf("version = %+v, %v", v, err)
	}
	s, err := ro.BankStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalLinks != 4210 || s.TotalObservations != 218 || s.OperationsByStatus["pending"] != 2 || s.OperationsByStatus["completed"] != 40 || s.LinksByLinkType["temporal"] != 1210 || s.NodesByFactType["world"] != 200 {
		t.Errorf("stats = %+v", s)
	}
	if want := time.Date(2026, 10, 30, 9, 12, 0, 123456000, time.UTC); !s.LastConsolidatedAt.Equal(want) {
		t.Errorf("last_consolidated_at = %v, want %v", s.LastConsolidatedAt, want)
	}
	if want := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC); !s.LastMemoryWriteAt.Equal(want) {
		t.Errorf("last_memory_write_at = %v, want %v", s.LastMemoryWriteAt, want)
	}

	ts, err := ro.MemoriesTimeseries(ctx, "7d")
	if err != nil || ts.Period != "7d" || ts.Trunc != "day" || len(ts.Buckets) != 2 {
		t.Fatalf("timeseries = %+v, %v", ts, err)
	}
	if b := ts.Buckets[1]; b.World != 0 || b.Experience != 5 || b.Observation != 4 || !b.Time.Equal(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("bucket = %+v", b)
	}

	lb, err := ro.LLMStats(ctx, "7d", "")
	if err != nil || len(lb) != 2 {
		t.Fatalf("llm = %+v, %v", lb, err)
	}
	if b := lb[0]; b.Total != 10 || b.Statuses["error"] != 1 || b.Input != 1000 || b.Output != 200 || b.Cached != 300 || b.Thoughts != 50 {
		t.Errorf("llm[0] = %+v", b)
	}
	if lb[1].Thoughts != 0 || lb[1].Input != 400 {
		t.Errorf("null thoughts: llm[1] = %+v", lb[1])
	}

	page, err := ro.OperationsPage(ctx, "", "", 3, 0)
	if err != nil || page.Total != 57 || len(page.Operations) != 3 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if o := page.Operations[0]; o.ID != "op1" || o.TaskType != "batch_retain" || o.Type != "retain" || o.RetryCount != 0 || o.DocumentID != "conversation:s1" || !o.CreatedAt.Equal(time.Date(2026, 10, 30, 9, 12, 0, 123456000, time.UTC)) {
		t.Errorf("op1 = %+v", o)
	}
	if o := page.Operations[1]; o.RetryCount != 2 || o.Status != "failed" || !o.UpdatedAt.IsZero() {
		t.Errorf("op2 = %+v", o)
	}

	tags, err := ro.ProjectTags(ctx)
	if err != nil || len(tags) != 2 || tags[0] != (TagCount{"project:alpha", 5}) || tags[1] != (TagCount{"project:beta", 2}) {
		t.Fatalf("tags = %+v, %v", tags, err)
	}
	m, err := ro.Memory(ctx, "m1")
	if err != nil || m.ID != "m1" || m.State != "active" {
		t.Fatalf("memory = %+v, %v", m, err)
	}
	if c := seen(); c["total"] != 1 || c["GET"] == 0 {
		t.Errorf("methods seen = %v, want GET only", c)
	}
}

func TestReadOnlyQueries(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path+"?"+r.URL.Query().Encode())
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ro, _ := NewReadOnly(srv.URL, "", "b", time.Second)
	ctx := context.Background()
	_, _ = ro.LLMStats(ctx, "7d", "retain_extract_facts")
	_, _ = ro.LLMStats(ctx, "1d", "")
	_, _ = ro.OperationsPage(ctx, "failed", "retain", 100, 200)
	_, _ = ro.OperationsPage(ctx, "", "", 20, 0)
	want := []string{
		"/v1/default/banks/b/llm-requests/stats?operation=retain_extract_facts&period=7d",
		"/v1/default/banks/b/llm-requests/stats?period=1d",
		"/v1/default/banks/b/operations?limit=100&offset=200&status=failed&type=retain",
		"/v1/default/banks/b/operations?limit=20&offset=0",
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}

func TestZonelessTimes(t *testing.T) {
	const bp = "/v1/default/banks/b"
	ro, _ := shapeServer(t, map[string]string{bp + "/operations": "operations.json", bp + "/stats": "stats.json"})
	page, err := ro.OperationsPage(context.Background(), "", "", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 30, 9, 12, 0, 0, time.UTC)
	if got := page.Operations[1].CreatedAt; !got.Equal(want) { // with zone
		t.Errorf("zoned created_at = %v", got)
	}
	if got := page.Operations[0].CreatedAt.Truncate(time.Second); !got.Equal(want) { // zone-less, fractional
		t.Errorf("zone-less created_at = %v", got)
	}
	// the existing field too: zone-less updated_at is UTC, not a decode failure
	if got := page.Operations[0].UpdatedAt; !got.Equal(time.Date(2026, 10, 30, 9, 13, 0, 0, time.UTC)) {
		t.Errorf("zone-less updated_at = %v", got)
	}
	if got := page.Operations[2]; !got.CreatedAt.IsZero() || got.ID != "op3" || got.Status != "pending" || !got.UpdatedAt.Equal(time.Date(2026, 10, 30, 9, 14, 0, 0, time.UTC)) {
		t.Errorf("garbage created_at: op3 = %+v", got)
	}
	s, err := ro.BankStats(context.Background())
	if err != nil || s.LastConsolidatedAt.IsZero() {
		t.Errorf("zone-less last_consolidated_at: %+v, %v", s, err)
	}
}

func TestOperationsNeverDecodeErrorText(t *testing.T) {
	ro, _ := shapeServer(t, map[string]string{"/v1/default/banks/b/operations": "operations.json"})
	page, err := ro.OperationsPage(context.Background(), "failed", "", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Operations[1].Status != "failed" {
		t.Fatal("fixture should carry the failed operation")
	}
	if s := fmt.Sprintf("%#v", page); strings.Contains(s, "ERR-CANARY") {
		t.Fatalf("decoded page holds the error text: %s", s)
	}
}

func TestReadOnlyCallsTimeOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = w.Write(shape(t, "version.json"))
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	// the blocked call gets a tiny timeout (the handler waits for the
	// request context, so no duration matters); the fast call gets a long
	// one, so a slow runner cannot turn it into a timeout.
	slow, err := NewReadOnly(srv.URL, "k", "b", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := NewReadOnly(srv.URL, "k", "b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slow.BankStats(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatalf("slow call: err = %v, want ErrTimeout", err)
	}
	if v, err := ro.Version(context.Background()); err != nil || v.APIVersion != "1.1.0" {
		t.Fatalf("fast call: %+v, %v", v, err)
	}
}
