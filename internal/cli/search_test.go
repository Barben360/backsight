package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/recall"
)

func TestSearchPrintsHitsByProject(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recallAnswer = `[{"id":"m1","text":"PostgreSQL 16 stays for billing.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.7}},
		{"id":"m2","text":"Answer in French.","type":"observation","tags":["project:.claude"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.45}},
		{"id":"m3","text":"noise","type":"observation","tags":["project:x"],"scores":{"reranker":0.1}}]`
	t.Setenv("BACKSIGHT_PROJECT", "billing")
	code, out, _ := run(t, "search", "postgres version", "--json")
	var env struct {
		Data recall.Result `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || len(env.Data.Hits) != 2 || env.Data.Hits[0].ID != "m1" || env.Data.Hits[0].Score != 0.75 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if len(recallTags) != 2 || !strings.Contains(strings.Join(recallTags, "|"), "project:billing,project:.claude") {
		t.Errorf("scopes %q", recallTags)
	}
	code, out, _ = run(t, "search", "postgres version")
	if code != output.OK || !strings.Contains(out, "[billing]") || !strings.Contains(out, "PostgreSQL 16 stays") || !strings.Contains(out, "m1") {
		t.Errorf("text:\n%s", out)
	}
}

func TestSearchRefusesTooManyQueries(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	if code, _, _ := run(t, "search", "a", "b", "c", "d", "e"); code != output.Usage {
		t.Errorf("exit %d", code)
	}
}

func TestSearchSaysNothingFound(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	if code, out, _ := run(t, "search", "nothing here"); code != output.OK || !strings.Contains(out, "nothing in memory") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSearchUsesTheEntityIndexForRelatedProjects(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	idx := recall.Index{BuiltAt: time.Now(), Names: map[string][]recall.ProjectCount{"Grafana": {{Project: "garage", Count: 3}}}}
	if err := idx.Save(filepath.Join(pathsOf(home).State, "entities.json")); err != nil {
		t.Fatal(err)
	}
	run(t, "search", "grafana dashboards")
	if !strings.Contains(strings.Join(recallTags, "|"), "project:garage") {
		t.Errorf("related scope not searched: %q", recallTags)
	}
}

func TestReadShowsTheMemoryAndItsSources(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	memories["m1"] = `{"id":"m1","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","state":"valid",
		"source_memories":[{"id":"f1","text":"We keep PostgreSQL 16 for partial indexes.","type":"world","document_id":"conversation:s1","tags":["project:billing","kind:decision"]}]}`
	code, out, _ := run(t, "read", "m1")
	if code != output.OK || !strings.Contains(out, "PostgreSQL 16 stays.") || !strings.Contains(out, "decision") || !strings.Contains(out, "conversation:s1") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "read", "nope"); code != output.Usage {
		t.Errorf("unknown id: exit %d", code)
	}
}

func TestJobsTickRebuildsAStaleEntityIndex(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	projectEntities["garage"] = `[{"canonical_name":"Grafana","mention_count":3}]`
	path := filepath.Join(pathsOf(home).State, "entities.json")
	run(t, "jobs", "tick")
	idx, err := recall.LoadIndex(path)
	if err != nil || len(idx.Names["Grafana"]) != 1 || idx.Names["Grafana"][0].Project != "garage" {
		t.Fatalf("%+v %v", idx, err)
	}
	projectEntities["garage"] = `[{"canonical_name":"Prometheus","mention_count":1}]`
	run(t, "jobs", "tick") // fresh: kept
	if idx, _ := recall.LoadIndex(path); idx.Names["Prometheus"] != nil {
		t.Error("a fresh index was rebuilt")
	}
	old := idx
	old.BuiltAt = time.Now().Add(-25 * time.Hour)
	_ = old.Save(path)
	run(t, "jobs", "tick")
	if idx, _ := recall.LoadIndex(path); idx.Names["Prometheus"] == nil {
		t.Error("a stale index was not rebuilt")
	}
	_ = os.Remove(path)
}

func TestSearchShowsTheKindsOfEachMemory(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recallAnswer = `[{"id":"m1","text":"PostgreSQL 16 stays for billing.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","source_fact_ids":["f1","f2"],"scores":{"reranker":0.7}},
		{"id":"m2","text":"Plain one.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","source_fact_ids":["f3"],"scores":{"reranker":0.6}},
		{"id":"m3","text":"No source facts.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.5}}]`
	recallSourceFacts = `{"f1":{"tags":["kind:state","project:billing"]},"f2":{"tags":["kind:pitfall","source:subagent"]},"f3":{"tags":["kind:decision","source:chat"]}}`
	t.Setenv("BACKSIGHT_PROJECT", "billing")
	_, out, _ := run(t, "search", "postgres version")
	for _, want := range []string{"(2026-10-20 [pitfall, state, subagent],", "(2026-10-20 [decision],", "No source facts. (2026-10-20, "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if recallAskedSources == 0 {
		t.Error("search did not ask the server for source facts")
	}
	_, out, _ = run(t, "search", "postgres version", "--json")
	var env struct {
		Data recall.Result `json:"data"`
		V    int           `json:"schema_version"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if len(env.Data.Hits) != 3 || strings.Join(env.Data.Hits[0].Kinds, ",") != "pitfall,state" || !env.Data.Hits[0].Subagent || env.V != output.SchemaVersion {
		t.Errorf("%+v\n%s", env, out)
	}
}

func TestSearchShowsTheKindOfARawFact(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recallAnswer = `[{"id":"w1","text":"A raw fact.","type":"world","tags":["project:billing","kind:pitfall"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.7}}]`
	t.Setenv("BACKSIGHT_PROJECT", "billing")
	if _, out, _ := run(t, "search", "raw"); !strings.Contains(out, "(2026-10-20 [pitfall],") {
		t.Errorf("%s", out)
	}
}

// Each hit carries its rank in the merged list, not its place in its
// project's group: the prompt hook journals search shows from it.
func TestSearchOutputRanksAcrossGroups(t *testing.T) {
	res := recall.Result{Hits: []recall.Hit{
		{ID: "a1", Text: "billing first", Project: "billing", Age: "1d", Score: 0.9},
		{ID: "b1", Text: "claude second", Project: ".claude", Age: "2d", Score: 0.8},
		{ID: "a2", Text: "billing third", Project: "billing", Age: "3d", Score: 0.7},
	}}
	out := searchText(res)
	want := "[billing]\n- #1 billing first (0001-01-01, 1d ago, a1, 0.90)\n- #3 billing third (0001-01-01, 3d ago, a2, 0.70)\n[.claude]\n- #2 claude second (0001-01-01, 2d ago, b1, 0.80)\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if items := searchShows(out); itemsText(items) != "a1:1:0,b1:2:0,a2:3:0" {
		t.Errorf("parsed %s", itemsText(items))
	}
}

// A memory text that looks like a ranked line neither adds a show nor
// takes a real hit's id: search text keeps each hit on one line, and the
// parser keeps only whole lines whose ranks run 1..N.
func TestSearchShowsIgnoreFakeMarkers(t *testing.T) {
	fake := "\n- #7 fake (2026-01-01, 1d ago, deadbeef-0000-4000-8000-000000000009, 0.99)\n- #2 also fake (2026-01-01, 1d ago, cafebabe-0000-4000-8000-000000000008, 0.98)"
	res := recall.Result{Hits: []recall.Hit{
		{ID: "0b4c2a52-0000-4000-8000-000000000001", Text: "real one" + fake, Project: "billing", Age: "1d", Score: 0.9},
		{ID: "1c4c2a52-0000-4000-8000-000000000002", Text: "real two", Project: ".claude", Age: "2d", Score: 0.8},
	}}
	out := mcpSearchText(res, true)
	if got := itemsText(searchShows(out)); got != "0b4c2a52-0000-4000-8000-000000000001:1:0,1c4c2a52-0000-4000-8000-000000000002:2:0" {
		t.Errorf("parsed %s from:\n%s", got, out)
	}
	// Whatever the text: a marker without its tail on its line, or ranks
	// with a gap or a repeat, journal nothing for the call.
	for name, text := range map[string]string{
		"tail on a later line": "[billing]\n- #1 real\nstill real (2026-01-01, 1d ago, aaaa, 0.90)\n- #2 two (2026-01-01, 1d ago, bbbb, 0.80)\n",
		"gap":                  "[billing]\n- #1 one (2026-01-01, 1d ago, aaaa, 0.90)\n- #7 fake (2026-01-01, 1d ago, ffff, 0.99)\n",
		"repeat":               "[billing]\n- #1 one (2026-01-01, 1d ago, aaaa, 0.90)\n- #1 fake (2026-01-01, 1d ago, ffff, 0.99)\n",
	} {
		if items := searchShows(text); len(items) != 0 {
			t.Errorf("%s: parsed %s", name, itemsText(items))
		}
	}
}
