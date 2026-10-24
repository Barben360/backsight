package recall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

type hit struct {
	id, text, project string
	score             float64
}

// fakeBank answers recall per scope: the tags of the request pick the
// hits; types pick observations or raw facts.
type fakeBank struct {
	mu       sync.Mutex
	scopes   map[string][]hit // key: comma-joined tags ("" = whole bank)
	raw      map[string][]hit // world/experience answers
	fail     map[string]bool
	requests []hindsight.RecallRequest
}

func (f *fakeBank) client(t *testing.T) *hindsight.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req hindsight.RecallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		key := strings.Join(req.Tags, ",")
		if f.fail[key] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		src := f.scopes
		if len(req.Types) > 0 && req.Types[0] != "observation" {
			src = f.raw
		}
		var out []map[string]any
		for _, h := range src[key] {
			out = append(out, map[string]any{"id": h.id, "text": h.text, "type": "observation", "tags": []string{"project:" + h.project},
				"mentioned_at": "2026-10-20T09:00:00Z", "scores": map[string]float64{"reranker": h.score}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": out})
	}))
	t.Cleanup(srv.Close)
	c, _ := hindsight.New(srv.URL, "k", 5*time.Second)
	return c
}

func engine(t *testing.T, f *fakeBank) *Engine {
	return &Engine{Client: f.client(t), Bank: "b", Transverse: []string{".claude", "user"}, Timeout: 2 * time.Second,
		Now: func() time.Time { return time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC) }}
}

const own = "project:billing,project:.claude,project:user"

func TestSearchMergesScopesWithTheCurrentProjectBoost(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{
		own: {{"m1", "PostgreSQL 16 stays.", "billing", 0.60}, {"m2", "User-wide: answer in French.", ".claude", 0.62}},
		"":  {{"m1", "PostgreSQL 16 stays.", "billing", 0.60}, {"m3", "Infra uses Postgres 15.", "infra", 0.64}, {"m4", "noise", "x", 0.2}},
	}}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"postgres version"}, Current: "billing", MinScore: 0.4, Limit: 10})
	var ids []string
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	if strings.Join(ids, ",") != "m1,m3,m2" || res.Hits[0].Score != 0.65 || res.Hits[0].Reranker != 0.60 {
		t.Fatalf("order %v hits %+v", ids, res.Hits)
	}
	if res.Hits[0].Project != "billing" || res.Hits[0].Age != "4d" || res.Raw {
		t.Errorf("%+v", res.Hits[0])
	}
	if !res.Scopes["project"].OK || !res.Scopes["global"].OK || len(f.requests) != 2 {
		t.Errorf("scopes %+v, %d requests", res.Scopes, len(f.requests))
	}
	for _, r := range f.requests {
		if r.Types[0] != "observation" || (len(r.Tags) > 0 && r.TagsMatch != "any_strict") {
			t.Errorf("request %+v", r)
		}
	}
}

func TestSearchQueriesTimesScopesAndLimit(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{"": {{"a", "a", "x", 0.9}, {"b", "b", "x", 0.8}, {"c", "c", "x", 0.7}}}}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"q1", "q2"}, Current: "billing", MinScore: 0.4, Limit: 2})
	if len(f.requests) != 4 || len(res.Hits) != 2 || res.Hits[1].ID != "b" {
		t.Errorf("%d requests, hits %+v", len(f.requests), res.Hits)
	}
}

func TestSearchAllIsTheWholeBankOnly(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{"": {{"a", "a", "billing", 0.5}}}}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", All: true, MinScore: 0.4})
	if len(f.requests) != 1 || len(f.requests[0].Tags) != 0 || len(res.Hits) != 1 {
		t.Errorf("%+v", f.requests)
	}
}

func TestRelatedScopeExcludesOwnProjectsAndCapsAtSix(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{}}
	engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing",
		Projects: []string{"billing", "a", "b", "c", "d", "e", "f", "g", ".claude"}})
	var related []string
	for _, r := range f.requests {
		if len(r.Tags) > 0 && r.Tags[0] == "project:a" {
			related = r.Tags
		}
	}
	if strings.Join(related, ",") != "project:a,project:b,project:c,project:d,project:e,project:f" {
		t.Errorf("related scope %v", related)
	}
}

func TestAFailingScopeIsSkippedNotFatal(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{"": {{"g", "global hit", "x", 0.7}}}, fail: map[string]bool{own: true}}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4})
	if res.Scopes["project"].OK || res.Scopes["project"].Err == "" || len(res.Hits) != 1 {
		t.Errorf("%+v", res)
	}
}

// Before consolidation (fresh bank, backfill running) there are no
// observations: raw facts stand in (SPEC §4 Reads).
func TestRawFactsWhenNoObservationExists(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{}, raw: map[string][]hit{"": {{"f1", "a raw fact", "billing", 0.8}}}}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4})
	if !res.Raw || len(res.Hits) != 1 || res.Hits[0].ID != "f1" {
		t.Errorf("%+v", res)
	}
	f2 := &fakeBank{scopes: map[string][]hit{"": {{"o1", "weak", "x", 0.1}}}, raw: map[string][]hit{"": {{"f1", "raw", "x", 0.8}}}}
	if res := engine(t, f2).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4}); res.Raw || len(res.Hits) != 0 {
		t.Errorf("observations exist (below threshold): no raw fallback, got %+v", res)
	}
}

func TestEntityIndexMatchesDistinctiveNames(t *testing.T) {
	idx := Index{Names: map[string][]ProjectCount{
		"PostgreSQL":  {{"infra", 9}, {"billing", 2}},
		"Grafana":     {{"garage", 4}},
		"kind:state":  {{"infra", 50}},
		"api":         {{"a", 1}},
		"docker":      {{"p1", 1}, {"p2", 1}, {"p3", 1}, {"p4", 1}, {"p5", 1}, {"p6", 1}, {"p7", 1}, {"p8", 1}, {"p9", 1}},
		"observer-ui": {{"observer", 3}},
	}}
	got := idx.Projects("Why is grafana slow on docker since the postgresql upgrade? see observer-ui", "billing", []string{".claude"})
	if strings.Join(got, ",") != "infra,garage,observer" {
		t.Errorf("projects %v", got)
	}
	if terms := idx.Terms("billing", []string{".claude"}, 3); len(terms) != 1 || terms[0] != "PostgreSQL" {
		t.Errorf("terms %v", terms)
	}
}
