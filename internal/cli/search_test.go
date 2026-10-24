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
