package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/recall"
)

// projectsIndex writes an entity index whose projects are billing (the
// session's), garage and infra-tools (listed), a stray directory and an
// idle project (left out), plus n filler projects.
func projectsIndex(t *testing.T, home string, n int) {
	t.Helper()
	now := time.Now().UTC()
	idx := recall.Index{BuiltAt: now, Names: map[string][]recall.ProjectCount{"Grafana": {{Project: "garage", Count: 3}}}, Known: []recall.ProjectInfo{
		{Name: "billing", Memories: 500, Last: now},
		{Name: "garage", Memories: 300, Last: now.Add(-time.Hour)},
		{Name: "infra-tools", Memories: 40, Last: now.Add(-48 * time.Hour)},
		{Name: "Downloads", Memories: 3, Last: now},                               // a stray directory
		{Name: "old-thesis", Memories: 200, Last: now.Add(-200 * 24 * time.Hour)}, // idle
		{Name: filepath.Base(home), Memories: 900, Last: now},                     // the transverse pool
	}}
	for i := range n {
		idx.Known = append(idx.Known, recall.ProjectInfo{Name: "filler-" + itoa(i), Memories: 20, Last: now.Add(-time.Duration(100+i) * time.Hour)})
	}
	if err := idx.Save(pathsOf(home).EntitiesFile()); err != nil {
		t.Fatal(err)
	}
}

func startCtx(t *testing.T, home, sid, source string) string {
	t.Helper()
	return fire(t, "session-start", map[string]any{"session_id": sid, "source": source, "cwd": filepath.Join(home, "billing")}).Specific.Context
}

func TestSessionStartNamesTheOtherProjects(t *testing.T) {
	home := hookHome(t)
	projectsIndex(t, home, 0)
	c := startCtx(t, home, "s1", "startup")
	want := "Projects in memory: this one `billing`. Others, reached only by a strong match unless named: garage, infra-tools. " +
		"When the task involves one of them (shared code, infrastructure, a decision made there), pass its exact name in memory_search `projects`."
	if !strings.HasPrefix(c, instructions.Guide+"\n\n"+want) {
		t.Errorf("startup context:\n%s", c)
	}
	for _, out := range []string{"Downloads", "old-thesis", filepath.Base(home) + ","} {
		if strings.Contains(c, out) {
			t.Errorf("%s listed:\n%s", out, c)
		}
	}
	if c := startCtx(t, home, "s2", "clear"); !strings.Contains(c, want) {
		t.Errorf("clear (its context starts empty) lacks the line:\n%s", c)
	}
	for _, src := range []string{"resume", "fork"} {
		if c := startCtx(t, home, "s3", src); c != instructions.Guide {
			t.Errorf("%s keeps its context, the line is not repeated: %q", src, c)
		}
	}
	if c := startCtx(t, home, "s1", "compact"); !strings.Contains(c, want) {
		t.Errorf("compact (the context was cut) lacks the line:\n%s", c)
	}
}

func TestSessionStartCapsTheProjectsAndCountsTheRest(t *testing.T) {
	home := hookHome(t)
	projectsIndex(t, home, 13) // 2 + 13 listed: 12 named, 3 counted
	c := startCtx(t, home, "s1", "startup")
	if !strings.Contains(c, "garage, infra-tools, filler-0,") || !strings.Contains(c, "filler-9 (+3 more).") || strings.Contains(c, "filler-10") {
		t.Errorf("cap or count:\n%s", c)
	}
}

func TestSessionStartNamesNoProjectsWithoutAnIndexOrOthers(t *testing.T) {
	home := hookHome(t)
	if c := startCtx(t, home, "s1", "startup"); strings.Contains(c, "Projects in memory") {
		t.Errorf("no index, still a line:\n%s", c)
	}
	if c := startCtx(t, home, "s1", "clear"); c != instructions.Guide {
		t.Errorf("no index, clear: %q", c)
	}
	idx := recall.Index{BuiltAt: time.Now(), Known: []recall.ProjectInfo{{Name: "billing", Memories: 50, Last: time.Now()}}}
	_ = idx.Save(pathsOf(home).EntitiesFile())
	if c := startCtx(t, home, "s2", "startup"); strings.Contains(c, "Projects in memory") {
		t.Errorf("no other project, still a line:\n%s", c)
	}
}

func TestPromptNamingAnotherProjectSearchesItAndSaysSoOnce(t *testing.T) {
	home := hookHome(t)
	projectsIndex(t, home, 0)
	recallAnswer = "[]"
	recallTags = nil
	const named = "Which PostgreSQL version does the garage service run, and why did we pick it last month?"
	line := "Your prompt names project `garage`: a search reaches its memories only by a strong match unless named, so pass projects: [\"garage\"] to memory_search."
	if c := prompt(t, home, "s1", named).Specific.Context; !strings.Contains(c, line) {
		t.Errorf("first naming:\n%s", c)
	}
	if !strings.Contains(strings.Join(recallTags, "|"), "project:garage") {
		t.Errorf("the named project was not searched with the prompt: %q", recallTags)
	}
	if c := prompt(t, home, "s1", named+" Again, please.").Specific.Context; strings.Contains(c, "Your prompt names") {
		t.Errorf("named twice in a session:\n%s", c)
	}
	if c := prompt(t, home, "s2", longPrompt).Specific.Context; strings.Contains(c, "Your prompt names") {
		t.Errorf("a prompt naming no other project got the line:\n%s", c)
	}
	if c := prompt(t, home, "s3", "Compare billing, Downloads and old-thesis with "+filepath.Base(home)+" for the batch jobs").Specific.Context; strings.Contains(c, "Your prompt names project `billing`") ||
		strings.Contains(c, "Downloads") || strings.Contains(c, "`"+filepath.Base(home)+"`") || !strings.Contains(c, "Your prompt names project `old-thesis`") {
		t.Errorf("this project, the transverse pool and a stray directory are not named; an idle project is:\n%s", c)
	}
	startCtx(t, home, "s1", "compact")
	if c := prompt(t, home, "s1", named).Specific.Context; !strings.Contains(c, line) {
		t.Errorf("after a compaction the line comes again:\n%s", c)
	}
}

func TestPromptNamingTwoProjectsNamesBothInOneLine(t *testing.T) {
	home := hookHome(t)
	projectsIndex(t, home, 0)
	recallAnswer = "[]"
	c := prompt(t, home, "s1", "Move the garage backups onto the infra-tools runner before the end of this week").Specific.Context
	if !strings.Contains(c, "Your prompt names projects `garage`, `infra-tools`:") || !strings.Contains(c, `projects: ["garage", "infra-tools"]`) {
		t.Errorf("two names:\n%s", c)
	}
}

func TestMCPSearchNamesAnUnknownProjectAndTheClosest(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	projectsIndex(t, home, 0)
	cs := mcpConnect(t, home)
	recallAnswer = `[{"id":"m1","text":"PostgreSQL 16 stays for billing.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.7}}]`
	out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}, "projects": []string{"garag"}})
	if isErr || !strings.Contains(out, "PostgreSQL 16 stays") || !strings.HasSuffix(out, "No project named `garag` in memory; closest: garage.\n") {
		t.Errorf("unknown project: %v\n%s", isErr, out)
	}
	if !strings.Contains(strings.Join(recallTags, "|"), "project:garag") {
		t.Errorf("the search still runs with the name asked for: %q", recallTags)
	}
	if out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}, "projects": []string{"garage"}}); strings.Contains(out, "No project named") {
		t.Errorf("a known project got the note:\n%s", out)
	}
	recallAnswer = "[]"
	if out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}, "projects": []string{"nowhere"}}); !strings.HasPrefix(out, "nothing in memory\n") || !strings.HasSuffix(out, "No project named `nowhere` in memory.\n") {
		t.Errorf("nothing found, unknown project:\n%s", out)
	}
}

func TestMCPSearchSaysNothingOfProjectsWithAnIndexWithoutThem(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	old := recall.Index{BuiltAt: time.Now(), Names: map[string][]recall.ProjectCount{"Grafana": {{Project: "garage", Count: 3}}}}
	_ = old.Save(pathsOf(home).EntitiesFile())
	cs := mcpConnect(t, home)
	recallAnswer = "[]"
	if out, _ := call(t, cs, "memory_search", map[string]any{"queries": []string{"postgres"}, "projects": []string{"anything"}}); strings.Contains(out, "No project named") {
		t.Errorf("an index written before the projects cannot tell:\n%s", out)
	}
}

func TestJobsTickStoresTheProjectsAndRebuildsAnIndexWithoutThem(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	path := pathsOf(home).EntitiesFile()
	old := recall.Index{BuiltAt: time.Now(), Names: map[string][]recall.ProjectCount{"Grafana": {{Project: "garage", Count: 3}}}}
	_ = old.Save(path)
	projectEntities["garage"] = `[{"canonical_name":"Grafana","mention_count":3}]`
	projectEntities["infra-tools"] = `[]`
	tagCounts["garage"] = 42
	recentByTag["project:garage"] = `[{"id":"a","text":"t","mentioned_at":"2026-11-09T08:00:00Z"}]`
	run(t, "jobs", "tick")
	idx, err := recall.LoadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]recall.ProjectInfo{}
	for _, p := range idx.Known {
		got[p.Name] = p
	}
	if g := got["garage"]; g.Memories != 42 || !g.Last.Equal(time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("garage: %+v (index %+v)", g, idx.Known)
	}
	if i, ok := got["infra-tools"]; !ok || i.Memories != 1 {
		t.Errorf("infra-tools (no memory listed: activity unknown): %+v %v", i, ok)
	}
}
