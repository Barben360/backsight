package hindsight

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
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
	want := []string{"BankStats", "DocumentUnits", "LLMStats", "LLMStatsWindow", "ListCreatedMemories", "MemoriesTimeseries", "Memory", "OperationsPage", "ProjectLastMemory", "ProjectTags", "RetainOperations", "Version"}
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
	if lb[0].Trunc != "day" || lb[1].Trunc != "day" {
		t.Errorf("trunc = %q, %q, want day from the response", lb[0].Trunc, lb[1].Trunc)
	}
	// numeric offsets: 17:00-07:00 is the next day 00:00 UTC
	if want := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC); !lb[0].Time.Equal(want) || lb[0].Time.Location() != time.UTC {
		t.Errorf("llm[0].Time = %v, want %v in UTC", lb[0].Time, want)
	}
	if lb[1].Thoughts != 0 || lb[1].Input != 400 {
		t.Errorf("null thoughts: llm[1] = %+v", lb[1])
	}

	win, err := ro.LLMStatsWindow(ctx, "7d", "")
	if err != nil || win.Trunc != "day" || len(win.Buckets) != 2 || win.Buckets[0].Input != 1000 {
		t.Fatalf("llm window = %+v, %v", win, err)
	}
	// the window's start, a time with its offset: 17:00-07:00 is 00:00 UTC the next day
	if want := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC); !win.Start.Equal(want) || win.Start.Location() != time.UTC {
		t.Errorf("llm window start = %v, want %v in UTC", win.Start, want)
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
	_, _ = ro.LLMStats(ctx, "7d", "retain")
	_, _ = ro.LLMStats(ctx, "1d", "")
	_, _ = ro.OperationsPage(ctx, "failed", "retain", 100, 200)
	_, _ = ro.OperationsPage(ctx, "", "", 20, 0)
	want := []string{
		"/v1/default/banks/b/llm-requests/stats?operation=retain&period=7d",
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

// TestNumericOffsetTimes: microseconds with a numeric offset (the operations
// and stats times) decode to the right UTC instant, in the fields read
// through serverTime and through the nested progress being ignored.
func TestNumericOffsetTimes(t *testing.T) {
	const bp = "/v1/default/banks/b"
	ro, _ := shapeServer(t, map[string]string{bp + "/operations": "operations.json", bp + "/stats": "stats.json"})
	page, err := ro.OperationsPage(context.Background(), "", "", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 30, 9, 14, 0, 250000, time.UTC); !page.Operations[2].UpdatedAt.Equal(want) {
		t.Errorf("updated_at = %v, want %v", page.Operations[2].UpdatedAt, want)
	}
	s, err := ro.BankStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 30, 9, 12, 0, 123456000, time.UTC); !s.LastConsolidatedAt.Equal(want) {
		t.Errorf("last_consolidated_at = %v, want %v", s.LastConsolidatedAt, want)
	}
	if want := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC); !s.LastMemoryWriteAt.Equal(want) {
		t.Errorf("last_memory_write_at = %v, want %v", s.LastMemoryWriteAt, want)
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
	if got := page.Operations[2]; !got.CreatedAt.IsZero() || got.ID != "op3" || got.Status != "pending" || !got.UpdatedAt.Equal(time.Date(2026, 10, 30, 9, 14, 0, 250000, time.UTC)) {
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

// bodyServer answers each path with its body (404 when absent) and records
// every request as "METHOD path?query".
func bodyServer(t *testing.T, bodies map[string]string) (*ReadOnly, func() []string) {
	t.Helper()
	var (
		mu  sync.Mutex
		got []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.Query().Encode())
		mu.Unlock()
		body, ok := bodies[r.URL.EscapedPath()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	ro, err := NewReadOnly(srv.URL, "k", "b", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return ro, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func TestListCreatedMemoriesDecodesNoText(t *testing.T) {
	ro, _ := bodyServer(t, map[string]string{
		"/v1/default/banks/b/memories/list": `{"total":2717,"limit":2,"offset":0,"items":[
			{"id":"m1","fact_type":"world","document_id":"conversation:s1@p#2","text":"CANARY text","context":"CANARY context",
			 "entities":["CANARY entity"],"metadata":{"kind":"CANARY","machine_id":"CANARY","session":"CANARY","source":"CANARY"},
			 "tags":["project:CANARY"],"chunk_id":"CANARY-chunk","mentioned_at":"2026-01-02T03:04:05Z","state":"active"},
			{"id":"m2","fact_type":"observation","text":"CANARY obs","context":"CANARY","entities":[],"tags":["CANARY"],
			 "source_memory_ids":["m1"],"occurred_start":"2026-01-02T03:04:05Z"}]}`,
	})
	items, total, err := ro.ListCreatedMemories(context.Background(), time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []ListedMemory{{ID: "m1", FactType: "world", DocumentID: "conversation:s1@p#2"}, {ID: "m2", FactType: "observation"}}
	if total != 2717 || !reflect.DeepEqual(items, want) {
		t.Fatalf("items = %+v, total %d; want %+v, 2717", items, total, want)
	}
	if s := fmt.Sprintf("%+v", items); strings.Contains(s, "CANARY") {
		t.Fatalf("decoded items hold server text: %s", s)
	}
}

func TestListCreatedMemoriesPagesAndQuery(t *testing.T) {
	const all = 700
	var (
		mu  sync.Mutex
		got []url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		got = append(got, q)
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/v1/default/banks/b/memories/list" {
			http.NotFound(w, r)
			return
		}
		off, _ := strconv.Atoi(q.Get("offset"))
		lim, _ := strconv.Atoi(q.Get("limit"))
		var items []string
		for i := off; i < min(all, off+lim); i++ {
			items = append(items, fmt.Sprintf(`{"id":"m%d","fact_type":"world","document_id":"note:%d","text":"CANARY"}`, i, i))
		}
		_, _ = fmt.Fprintf(w, `{"total":%d,"limit":%d,"offset":%d,"items":[%s]}`, all, lim, off, strings.Join(items, ","))
	}))
	defer srv.Close()
	ro, err := NewReadOnly(srv.URL, "k", "b", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reqs := func() []url.Values { mu.Lock(); defer mu.Unlock(); r := got; got = nil; return r }
	ctx := context.Background()
	// a start in another zone is sent in UTC
	start := time.Date(2026, 10, 30, 2, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

	first, total, err := ro.ListCreatedMemories(ctx, start, end, 0, 500)
	if err != nil || len(first) != 500 || total != all || first[0].ID != "m0" || first[499].ID != "m499" {
		t.Fatalf("offset 0: %d items, total %d, %v", len(first), total, err)
	}
	r := reqs()
	want := url.Values{"time_field": {"created_at"}, "start_date": {"2026-10-30T00:00:00Z"}, "end_date": {"2026-10-31T00:00:00Z"}, "limit": {"500"}, "offset": {"0"}}
	if len(r) != 1 || !reflect.DeepEqual(r[0], want) {
		t.Fatalf("offset 0: requests %v, want one %v", r, want)
	}
	rest, total, err := ro.ListCreatedMemories(ctx, start, end, 500, 500)
	if err != nil || len(rest) != 200 || total != all || rest[0].ID != "m500" || rest[199].ID != "m699" {
		t.Fatalf("offset 500: %d items, total %d, %v", len(rest), total, err)
	}
	want.Set("offset", "500")
	if r := reqs(); len(r) != 1 || !reflect.DeepEqual(r[0], want) {
		t.Fatalf("offset 500: requests %v, want one %v", r, want)
	}

	// zero bounds are left out
	if _, _, err := ro.ListCreatedMemories(ctx, time.Time{}, end, 0, 2); err != nil {
		t.Fatal(err)
	}
	if r := reqs(); len(r) != 1 || r[0].Has("start_date") || r[0].Get("end_date") != "2026-10-31T00:00:00Z" || r[0].Get("time_field") != "created_at" {
		t.Fatalf("zero start: requests %v, want no start_date", r)
	}
	if _, _, err := ro.ListCreatedMemories(ctx, start, time.Time{}, 0, 2); err != nil {
		t.Fatal(err)
	}
	if r := reqs(); len(r) != 1 || r[0].Has("end_date") || r[0].Get("start_date") != "2026-10-30T00:00:00Z" {
		t.Fatalf("zero end: requests %v, want no end_date", r)
	}
}

func TestDocumentUnitsNeverDecodesOriginalText(t *testing.T) {
	const bp = "/v1/default/banks/b/documents/"
	ro, seen := bodyServer(t, map[string]string{
		bp + "conversation%3As1":       `{"id":"conversation:s1","original_text":"CANARY","memory_unit_count":4,"tags":["project:p"]}`,
		bp + "conversation%3As2@p%232": `{"id":"conversation:s2@p#2","original_text":"CANARY","memory_unit_count":0}`,
		bp + "conversation%3As3":       `{"id":"conversation:s3","original_text":"CANARY"}`,
		bp + "conversation%3As4":       `{"id":"conversation:s4","memory_unit_count":null}`,
	})
	ctx := context.Background()
	if n, ok, err := ro.DocumentUnits(ctx, "conversation:s1"); err != nil || n != 4 || !ok {
		t.Fatalf("units = %d, %v, %v; want 4, true", n, ok, err)
	}
	if n, ok, err := ro.DocumentUnits(ctx, "conversation:s2@p#2"); err != nil || n != 0 || !ok {
		t.Fatalf("zero units = %d, %v, %v; want 0, true (a count of 0 is known)", n, ok, err)
	}
	for _, id := range []string{"conversation:s3", "conversation:s4"} {
		if n, ok, err := ro.DocumentUnits(ctx, id); err != nil || n != 0 || ok {
			t.Fatalf("%s: units = %d, %v, %v; want 0, false", id, n, ok, err)
		}
	}
	_, ok, err := ro.DocumentUnits(ctx, "conversation:gone")
	var ae *APIError
	if ok || !errors.As(err, &ae) || ae.Kind != NotFound {
		t.Fatalf("missing document: ok %v, err = %v; want *APIError NotFound", ok, err)
	}
	if got := seen(); !slices.Contains(got, "GET "+bp+"conversation%3As2@p%232?") {
		t.Fatalf("requests = %q, want the id escaped (the colon and #: a raw # would end the path)", got)
	}
}

// opsBody is one operations page of n operations from first, every third
// a consolidation.
func opsBody(first, n, total int) string {
	var ops []string
	for i := first; i < first+n; i++ {
		typ := "retain"
		if i%3 == 2 {
			typ = "consolidation"
		}
		ops = append(ops, fmt.Sprintf(`{"id":"op%d","operation_type":%q,"status":"completed","document_id":"conversation:s%d","error_message":"CANARY"}`, i, typ, i))
	}
	return fmt.Sprintf(`{"total":%d,"operations":[%s]}`, total, strings.Join(ops, ","))
}

func TestRetainOperationsPagesAndFilters(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	const total = 250 // three pages: 100, 100, 50
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.Query().Encode())
		mu.Unlock()
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		_, _ = w.Write([]byte(opsBody(off, max(0, min(100, total-off)), total)))
	}))
	defer srv.Close()
	ro, err := NewReadOnly(srv.URL, "k", "b", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reqs := func() []string { mu.Lock(); defer mu.Unlock(); r := slices.Clone(got); got = nil; return r }
	const q = "GET /v1/default/banks/b/operations?limit=100&offset=%d&status=completed&type=retain"

	ops, err := ro.RetainOperations(context.Background(), "completed", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf(q, 0), fmt.Sprintf(q, 100), fmt.Sprintf(q, 200)}
	if r := reqs(); !reflect.DeepEqual(r, want) {
		t.Fatalf("requests = %q, want %q (stops at the short page)", r, want)
	}
	retains := 0
	for i := range total {
		if i%3 != 2 {
			retains++
		}
	}
	if len(ops) != retains {
		t.Fatalf("got %d operations, want the %d retains", len(ops), retains)
	}
	for _, o := range ops {
		if o.Type != "retain" {
			t.Fatalf("non-retain operation kept: %+v", o)
		}
	}
	if ops[0].ID != "op0" || ops[1].ID != "op1" || ops[2].ID != "op3" || ops[len(ops)-1].ID != "op249" {
		t.Errorf("order or ids: first %s %s %s, last %s", ops[0].ID, ops[1].ID, ops[2].ID, ops[len(ops)-1].ID)
	}

	ops, err = ro.RetainOperations(context.Background(), "completed", 2)
	if err != nil {
		t.Fatal(err)
	}
	if r := reqs(); !reflect.DeepEqual(r, want[:2]) {
		t.Fatalf("pages 2: requests = %q, want %q", r, want[:2])
	}
	if len(ops) != 134 || ops[len(ops)-1].ID != "op199" {
		t.Fatalf("pages 2: %d operations, last %s; want 134, op199", len(ops), ops[len(ops)-1].ID)
	}

	if ops, err := ro.RetainOperations(context.Background(), "completed", 0); err != nil || len(ops) != 0 || len(reqs()) != 0 {
		t.Fatalf("pages 0: %d ops, %v; want no request", len(ops), err)
	}
}

func TestRetainOperationsStopsAtTotalAndOnErrors(t *testing.T) {
	var (
		mu    sync.Mutex
		n     int
		total = 100 // a full page that is the whole list
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		k, tot := n, total
		mu.Unlock()
		if k > 1 && tot > 100 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(opsBody(0, 100, tot)))
	}))
	defer srv.Close()
	ro, _ := NewReadOnly(srv.URL, "k", "b", 5*time.Second)
	requests := func() int { mu.Lock(); defer mu.Unlock(); return n }
	ops, err := ro.RetainOperations(context.Background(), "completed", 5)
	if err != nil || len(ops) != 67 || requests() != 1 {
		t.Fatalf("total reached: %d ops, %d requests, %v; want 67 ops after 1 request", len(ops), requests(), err)
	}
	mu.Lock()
	n, total = 0, 1000
	mu.Unlock()
	_, err = ro.RetainOperations(context.Background(), "completed", 5)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Kind != ServerError || requests() != 2 {
		t.Fatalf("second page fails: err = %v after %d requests; want a ServerError after 2", err, requests())
	}
}

// recordingRT answers every request with a body by path and records the
// methods it was given.
type recordingRT struct {
	mu      sync.Mutex
	methods []string
	bodies  map[string]string
}

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.methods = append(r.methods, req.Method)
	r.mu.Unlock()
	body := r.bodies[req.URL.EscapedPath()]
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestNewReadsAreGETOnly(t *testing.T) {
	ro, err := NewReadOnly("http://server.invalid", "k", "b", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := ro.c.http.Transport.(getOnly)
	if !ok {
		t.Fatalf("transport = %T, want getOnly", ro.c.http.Transport)
	}
	const bp = "/v1/default/banks/b"
	rec := &recordingRT{bodies: map[string]string{
		bp + "/memories/list":               `{"total":1,"items":[{"id":"m1","fact_type":"world"}]}`,
		bp + "/documents/conversation%3As1": `{"memory_unit_count":2}`,
		bp + "/operations":                  opsBody(0, 3, 3),
	}}
	g.next = rec
	ro.c.http.Transport = g
	ctx := context.Background()
	if items, total, err := ro.ListCreatedMemories(ctx, time.Now().Add(-24*time.Hour), time.Now(), 0, 10); err != nil || total != 1 || len(items) != 1 || items[0].ID != "m1" {
		t.Fatalf("ListCreatedMemories: %+v, %d, %v", items, total, err)
	}
	if n, ok, err := ro.DocumentUnits(ctx, "conversation:s1"); err != nil || !ok || n != 2 {
		t.Fatalf("DocumentUnits: %d, %v, %v", n, ok, err)
	}
	if ops, err := ro.RetainOperations(ctx, "completed", 3); err != nil || len(ops) != 2 {
		t.Fatalf("RetainOperations: %d, %v", len(ops), err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !reflect.DeepEqual(rec.methods, []string{"GET", "GET", "GET"}) {
		t.Fatalf("methods = %v, want three GETs", rec.methods)
	}
}

// TestProjectLastMemory (issue 326): a project's latest memory by event
// time, asked for one memory of its tag only; when none has an event time
// the server lists none, and the latest by write time answers; a project
// with no memory answers the zero time. Only the times are decoded.
func TestProjectLastMemory(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+"?"+q.Encode())
		mu.Unlock()
		switch q.Get("tags") + " " + q.Get("time_field") {
		case "project:alpha mentioned_at":
			_, _ = w.Write([]byte(`{"items":[{"id":"m1","text":"CANARY","mentioned_at":"2026-11-10T22:27:21.880695+00:00","updated_at":"2026-11-11T08:00:00Z"}]}`))
		case "project:beta updated_at":
			_, _ = w.Write([]byte(`{"items":[{"id":"m2","text":"CANARY","mentioned_at":null,"updated_at":"2026-11-09T07:00:00Z"}]}`))
		default:
			_, _ = w.Write([]byte(`{"items":[]}`))
		}
	}))
	defer srv.Close()
	ro, _ := NewReadOnly(srv.URL, "", "b", time.Second)
	ctx := context.Background()
	for _, c := range []struct {
		project string
		want    time.Time
	}{
		{"alpha", time.Date(2026, 11, 10, 22, 27, 21, 880695000, time.UTC)},
		{"beta", time.Date(2026, 11, 9, 7, 0, 0, 0, time.UTC)},
		{"gamma", time.Time{}},
	} {
		at, err := ro.ProjectLastMemory(ctx, c.project)
		if err != nil || !at.Equal(c.want) {
			t.Errorf("%s: last memory %v, %v; want %v", c.project, at, err, c.want)
		}
	}
	want := []string{
		"GET /v1/default/banks/b/memories/list?limit=1&tags=project%3Aalpha&tags_match=any_strict&time_field=mentioned_at",
		"GET /v1/default/banks/b/memories/list?limit=1&tags=project%3Abeta&tags_match=any_strict&time_field=mentioned_at",
		"GET /v1/default/banks/b/memories/list?limit=1&tags=project%3Abeta&tags_match=any_strict&time_field=updated_at",
		"GET /v1/default/banks/b/memories/list?limit=1&tags=project%3Agamma&tags_match=any_strict&time_field=mentioned_at",
		"GET /v1/default/banks/b/memories/list?limit=1&tags=project%3Agamma&tags_match=any_strict&time_field=updated_at",
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}
