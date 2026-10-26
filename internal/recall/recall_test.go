package recall

import (
	"context"
	"encoding/json"
	"fmt"
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
	return &Engine{Client: f.client(t), Bank: "b", Transverse: []string{".claude", "annuser"}, Timeout: 2 * time.Second,
		Now: func() time.Time { return time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC) }}
}

const own = "project:billing,project:.claude,project:annuser"

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
	// 2 queries × 2 scopes, then the project scope (no observation) asked
	// again for raw facts
	if len(f.requests) != 6 || len(res.Hits) != 2 || res.Hits[1].ID != "b" {
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

func TestRelatedScopeKeepsAskedProjectsAndCapsIndexMatches(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{}}
	engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing",
		Projects: []string{"billing", "x", "y", ".claude", "x"},
		Related:  []string{"annuser", "y", "a", "b", "c", "d", "e", "f", "g"}})
	var related []string
	for _, r := range f.requests {
		if len(r.Tags) > 0 && r.Tags[0] == "project:x" {
			related = r.Tags
		}
	}
	// own projects (current, transverse) and duplicates dropped; asked
	// projects first, then at most six index matches
	if strings.Join(related, ",") != "project:x,project:y,project:a,project:b,project:c,project:d,project:e,project:f" {
		t.Errorf("related scope %v", related)
	}
}

// Decision 0071 C3: linked projects join the current project's call, their
// hits are near (no foreign floor, no boost), and the related scope does not
// repeat them.
func TestLinkedProjectsJoinTheProjectScopeAndAreNear(t *testing.T) {
	linked := own + ",project:atlas-spec"
	f := &fakeBank{scopes: map[string][]hit{
		linked: {{"l1", "Spec work happened in atlas-spec.", "atlas-spec", 0.55}},
		"":     {{"l1", "Spec work happened in atlas-spec.", "atlas-spec", 0.55}, {"f1", "foreign", "infra", 0.55}, {"f2", "strong foreign", "infra", 0.65}},
	}}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing",
		Linked: []string{"atlas-spec", "billing", ".claude"}, GlobalMinReranker: 0.6, MinReranker: 0.5})
	var ids []string
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	if strings.Join(ids, ",") != "f2,l1" || res.Hits[1].Score != 0.55 {
		t.Errorf("hits %+v (linked kept unboosted under the foreign floor, foreign 0.55 dropped)", res.Hits)
	}
	f.requests = nil
	engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing",
		Linked: []string{"atlas-spec"}, Related: []string{"atlas-spec", "ops"}})
	var tags []string
	for _, r := range f.requests {
		tags = append(tags, strings.Join(r.Tags, ","))
	}
	if j := "|" + strings.Join(tags, "|") + "|"; !strings.Contains(j, "|"+linked+"|") || !strings.Contains(j, "|project:ops|") {
		t.Errorf("scopes %q: linked project in the project call, related scope without it", tags)
	}
}

func TestAFailingScopeIsSkippedNotFatal(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{"": {{"g", "global hit", "x", 0.7}}}, fail: map[string]bool{own: true}}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4})
	if res.Scopes["project"].OK || res.Scopes["project"].Err == "" || len(res.Hits) != 1 {
		t.Errorf("%+v", res)
	}
}

// A scope with no observation yet (a project still being backfilled) is
// asked for raw facts, while other scopes keep their observations (SPEC §4
// Reads).
func TestRawFactsStandInPerScope(t *testing.T) {
	f := &fakeBank{
		scopes: map[string][]hit{"": {{"o1", "an observation of another project", "infra", 0.7}}},
		raw:    map[string][]hit{own: {{"f1", "a raw fact of billing", "billing", 0.8}}},
	}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4})
	if !res.Raw || len(res.Hits) != 2 || res.Hits[0].ID != "f1" {
		t.Fatalf("%+v", res)
	}
	var rawTypes []string
	for _, r := range f.requests {
		if r.Types[0] != "observation" {
			rawTypes = r.Types
			if strings.Join(r.Tags, ",") != own {
				t.Errorf("raw facts asked of a scope that has observations: %v", r.Tags)
			}
		}
	}
	if strings.Join(rawTypes, ",") != "world,experience" {
		t.Errorf("raw types %v", rawTypes)
	}
	f2 := &fakeBank{scopes: map[string][]hit{own: {{"o1", "weak", "billing", 0.1}}, "": {{"o2", "weak", "x", 0.1}}}, raw: map[string][]hit{own: {{"f1", "raw", "billing", 0.8}}}}
	if res := engine(t, f2).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4}); res.Raw || len(res.Hits) != 0 {
		t.Errorf("observations exist (below the floor): no raw fallback, got %+v", res)
	}
	f3 := &fakeBank{scopes: map[string][]hit{}, raw: map[string][]hit{own: {{"f1", "raw", "billing", 0.8}}}, fail: map[string]bool{own: true}}
	if res := engine(t, f3).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4}); res.Raw || res.Scopes["project"].OK {
		t.Errorf("a failed scope is not empty: no raw fallback for it, got %+v", res)
	}
}

// The whole search, raw fallback included, stays within one budget; a
// hanging scope does not hold back the others' hits.
func TestSearchStaysWithinItsBudget(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req hindsight.RecallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tags) > 0 {
			select {
			case <-block:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"g","text":"global","type":"observation","scores":{"reranker":0.9}}]}`))
	}))
	defer srv.Close()
	c, _ := hindsight.New(srv.URL, "k", time.Minute)
	e := &Engine{Client: c, Bank: "b", Transverse: []string{".claude"}, Timeout: 300 * time.Millisecond}
	start := time.Now()
	res := e.Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4})
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("search took %s", el)
	}
	if res.Scopes["project"].OK || len(res.Hits) != 1 || res.Hits[0].ID != "g" {
		t.Errorf("%+v", res)
	}
}

func TestRerankerFloorBlankTextAndTies(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{"": {
		{"b", "tie b", "billing", 0.46}, {"a", "tie a", "billing", 0.46}, {"blank", "  ", "billing", 0.9}, {"c", "boosted but weak", "billing", 0.49},
	}}}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4})
	var ids []string
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	if strings.Join(ids, ",") != "c,a,b" {
		t.Errorf("order %v (blank text dropped, ties by id)", ids)
	}
	res = engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4, MinReranker: 0.48})
	if len(res.Hits) != 1 || res.Hits[0].ID != "c" {
		t.Errorf("reranker floor on the server's score: %+v", res.Hits)
	}
	res = engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing", MinScore: 0.4, MinReranker: 0.5})
	if len(res.Hits) != 0 {
		t.Errorf("0.49 + boost must not pass a 0.5 reranker floor: %+v", res.Hits)
	}
}

func TestAgeUnits(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Minute: "5m", 3 * time.Hour: "3h", 4 * 24 * time.Hour: "4d", 20 * 24 * time.Hour: "2w", 90 * 24 * time.Hour: "3mo", 400 * 24 * time.Hour: "1y"} {
		if got := Age(d); got != want {
			t.Errorf("%s: %s, want %s", d, got, want)
		}
	}
}

func eight(name string) []ProjectCount {
	var out []ProjectCount
	for i := 0; i < 8; i++ {
		out = append(out, ProjectCount{name + itoa(i), 1})
	}
	return out
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestEntityIndexMatchesDistinctiveNames(t *testing.T) {
	idx := Index{Names: map[string][]ProjectCount{
		"PostgreSQL":  {{"infra", 9}, {"billing", 2}},
		"Grafana":     {{"garage", 4}},
		"kind:state":  {{"kindproj", 50}, {"billing", 50}},
		"api":         {{"apiproj", 1}, {"billing", 9}},
		"docker":      append(eight("d"), ProjectCount{"d9", 1}), // 9 projects: says nothing
		"terraform":   eight("t"),                                // 8 projects: still distinctive
		"observer-ui": {{"observer", 3}},
		"dotfiles":    {{".claude", 7}},
	}}
	got := idx.Projects("Why is grafana slow on docker since the postgresql upgrade? see observer-ui, kind:state, the api, dotfiles", "billing", []string{".claude"})
	if strings.Join(got, ",") != "infra,garage,observer" {
		t.Errorf("projects %v", got)
	}
	if got := idx.Projects("mygrafana and grafanas", "billing", nil); len(got) != 0 {
		t.Errorf("names match as whole words only: %v", got)
	}
	if got := idx.Projects("terraform", "billing", nil); len(got) != MaxRelated {
		t.Errorf("a name in 8 projects matches, capped at %d: %v", MaxRelated, got)
	}
	if terms := idx.Terms("billing", []string{".claude"}, 3); strings.Join(terms, ",") != "PostgreSQL" {
		t.Errorf("terms %v (kind labels and short names excluded)", terms)
	}
	idx.Names["Kafka"] = []ProjectCount{{"billing", 5}}
	idx.Names["Redis"] = []ProjectCount{{"billing", 1}}
	if terms := idx.Terms("billing", nil, 2); strings.Join(terms, ",") != "Kafka,PostgreSQL" {
		t.Errorf("terms capped at n, most mentioned first: %v", terms)
	}
}

func TestIndexFileFormat(t *testing.T) {
	idx := Index{BuiltAt: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC), Names: map[string][]ProjectCount{"Grafana": {{"garage", 4}}}}
	raw, _ := json.Marshal(idx)
	if string(raw) != `{"built_at":"2026-10-24T09:00:00Z","names":{"Grafana":[["garage",4]]}}` {
		t.Errorf("A.3 format: %s", raw)
	}
	var back Index
	if err := json.Unmarshal(raw, &back); err != nil || back.Names["Grafana"][0] != (ProjectCount{"garage", 4}) {
		t.Errorf("%+v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"names":{"x":[["only-one"]]}}`), &back); err == nil {
		t.Error("a malformed pair must not load")
	}
}

func TestZeroScoresAndEmptyIDs(t *testing.T) {
	f := &fakeBank{scopes: map[string][]hit{"": {{"z", "zero score", "x", 0}, {"", "no id", "x", 0.9}}}}
	res := engine(t, f).Search(context.Background(), Query{Queries: []string{"q"}, Current: "billing"})
	if len(res.Hits) != 1 || res.Hits[0].ID != "z" {
		t.Errorf("%+v", res.Hits)
	}
}

// Matching a prompt against a large index stays cheap (M2 block 1 review:
// one regex per name took seconds).
func TestEntityMatchingIsCheap(t *testing.T) {
	idx := Index{Names: map[string][]ProjectCount{}}
	for i := 0; i < 10000; i++ {
		idx.Names[fmt.Sprintf("entity-%05d", i)] = []ProjectCount{{fmt.Sprintf("p%d", i%50), 1}}
	}
	text := strings.Repeat("a long prompt about nothing in particular ", 2500) // 100 KB
	start := time.Now()
	idx.Projects(text, "billing", nil)
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("matching took %s", el)
	}
	if got := idx.Projects("see entity-00042 please", "billing", nil); len(got) != 1 || got[0] != "p42" {
		t.Errorf("%v", got)
	}
}
