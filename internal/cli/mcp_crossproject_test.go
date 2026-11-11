package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/testtool"
)

// Memories of projects the session neither is nor links, for the
// cross-project verdicts below.
const (
	foreignA = "cccccccc-0000-4000-8000-000000000003"
	foreignB = "c0c0c0c0-0000-4000-8000-00000000000c"
	thirdA   = "dddddddd-0000-4000-8000-000000000004"
)

// showForeign journals, in session S of project proj, a search that
// showed creditA (proj), foreignA and foreignB (other) and thirdA (third),
// each a raw fact on the fake server.
func showForeign(t *testing.T, home string) {
	t.Helper()
	showItems(t, home, "S", "search", "proj", feedback.Item{ID: creditA, Rank: 1, Project: "proj"},
		feedback.Item{ID: foreignA, Rank: 2, Project: "other"}, feedback.Item{ID: foreignB, Rank: 3, Project: "other"},
		feedback.Item{ID: thirdA, Rank: 4, Project: "third"})
	rawFact(creditA, foreignA, foreignB, thirdA)
}

// foreignRefusal is the line a verdict on foreignA answers while
// feedback.cross_project_retire is off.
const foreignRefusal = "> 🧠⚠️ **Not retired (other · cccccccc)** — a memory of project other, outside this session's project and the projects it links · retire it: `backsight forget " + foreignA + "`"

// By default a session retires a wrong memory of any project it was
// shown: the correction goes to that memory's project, the fact is
// invalidated, and the journal names both projects.
func TestCrossProjectRetireStoresTheCorrectionInTheMemorysProject(t *testing.T) {
	cs, home := creditSession(t)
	showForeign(t, home)
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "cccccccc"))
	if isErr || !retiredRE.MatchString(out) {
		t.Fatalf("%v %s", isErr, out)
	}
	if want := "\n> 🧠❌ **Wrong, retired (other · cccccccc)** — <what was wrong, and what shows it> · undo: `backsight forget --restore " + foreignA + "`"; !strings.HasSuffix(out, want) {
		t.Errorf("the line to print, %q, does not end\n%s", want, out)
	}
	if last := retainedTags[len(retainedTags)-1]; !strings.Contains(last, "project:other,") || strings.Contains(last, "project:proj") {
		t.Errorf("the correction went to %q, want project other", last)
	}
	if p := patches(); len(p) != 1 || !strings.Contains(p[0], "/memories/"+foreignA+" ") {
		t.Errorf("patches %q", p)
	}
	ev := retireEvents(t, home, feedback.KindRetire)
	if len(ev) != 1 || ev[0].Project != "proj" || ev[0].MemoryProject != "other" || ev[0].IDs[0] != foreignA {
		t.Errorf("journal: %+v", ev)
	}
}

// feedback.cross_project_retire = false keeps the refusal: nothing stored,
// nothing invalidated. Turned off during the session, it applies to the
// next verdict.
func TestCrossProjectRetireOffKeepsTheRefusal(t *testing.T) {
	cs, home := creditSession(t)
	showForeign(t, home)
	appendConfig(t, home, "[feedback]\ncross_project_retire = false")
	before := retained.Load()
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "cccccccc"))
	if !isErr || !strings.Contains(out, foreignRefusal) {
		t.Errorf("%v %s", isErr, out)
	}
	if !nothingStored(t, home, before) || len(patches()) != 0 {
		t.Errorf("a refused verdict stored or retired: %d sent, patches %q", retained.Load()-before, patches())
	}
	// the session's own memory still goes
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa")); isErr || !retiredRE.MatchString(out) {
		t.Errorf("own: %v %s", isErr, out)
	}
}

// A repository's .backsight.toml cannot turn the setting on: the file is
// read (its related project is retirable) but a feedback key in it is
// not, in a table or at the top.
func TestRepositoryCannotTurnOnCrossProjectRetire(t *testing.T) {
	cs, home := creditSessionWith(t, "\n[feedback]\ncross_project_retire = false\n")
	showForeign(t, home)
	_ = os.MkdirAll(filepath.Join(home, "proj"), 0o700)
	toml := "related = [\"third\"]\ncross_project_retire = true\n\n[feedback]\ncross_project_retire = true\n"
	if err := os.WriteFile(filepath.Join(home, "proj", ".backsight.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	before := retained.Load()
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "cccccccc")); !isErr || !strings.Contains(out, foreignRefusal) {
		t.Errorf("foreign: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Fatal("a refused verdict stored a note")
	}
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "dddddddd")); isErr || !retiredRE.MatchString(out) {
		t.Errorf("related: %v %s", isErr, out)
	}
}

// A correction goes to one project: a verdict whose memories span several
// projects, one of them neither the session's, the pool nor a linked one,
// is refused before anything is stored, naming the projects.
func TestCrossProjectRetireRefusesSeveralProjects(t *testing.T) {
	cs, home := creditSession(t)
	showForeign(t, home)
	before := retained.Load()
	for ids, projects := range map[[2]string]string{{"cccccccc", "aaaaaaaa"}: "other, proj", {"dddddddd", "cccccccc"}: "other, third"} {
		out, isErr := call(t, cs, "memory_credit", verdict("wrong", ids[0], ids[1]))
		if !isErr || !strings.Contains(out, "projects "+projects+":") || !strings.Contains(out, "call memory_credit once per project") {
			t.Errorf("%v: %v %s", ids, isErr, out)
		}
	}
	if !nothingStored(t, home, before) || len(patches()) != 0 {
		t.Fatalf("a refused verdict stored or retired: %d sent, patches %q", retained.Load()-before, patches())
	}
	// two memories of one other project: one call, one note there
	if out, isErr := call(t, cs, "memory_credit", verdict("outdated", "cccccccc", "c0c0c0c0")); isErr || !retiredRE.MatchString(out) {
		t.Fatalf("one project: %v %s", isErr, out)
	}
	if last := retainedTags[len(retainedTags)-1]; !strings.Contains(last, "project:other,") {
		t.Errorf("note tags %q", last)
	}
	if len(patches()) != 2 {
		t.Errorf("patches %q", patches())
	}
}

// One budget: another project's retirements count toward the session's
// feedback.max_retirements with its own.
func TestCrossProjectRetireCountsTowardTheSessionBudget(t *testing.T) {
	cs, home := creditSessionWith(t, "\n[feedback]\nmax_retirements = 3\n")
	showForeign(t, home)
	retireIn(t, home, "S", feedback.KindRetire, "11111111-0000-4000-8000-000000000001", "22222222-0000-4000-8000-000000000002")
	before := retained.Load()
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "cccccccc", "c0c0c0c0"))
	if !isErr || !strings.Contains(out, "at most 3 per session (feedback.max_retirements), 2 already") ||
		!strings.Contains(out, "> 🧠⚠️ **Not retired (other · cccccccc)** — at most 3 retirements per session") {
		t.Errorf("over the budget: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Fatal("a refused verdict stored a note")
	}
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "cccccccc")); isErr || !retiredRE.MatchString(out) {
		t.Fatalf("the third: %v %s", isErr, out)
	}
	before = retained.Load()
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa")); !isErr || !strings.Contains(out, "at most 3 per session") {
		t.Errorf("the session's own, past the budget: %v %s", isErr, out)
	}
	if !nothingStored(t, home, before) {
		t.Error("a refused verdict stored a note")
	}
}

// A memory's project kept out of memory refuses the correction meant for
// it: a directory of that name in privacy.exclude_paths, or its known
// root under an excluded directory.
func TestCrossProjectRetireHonoursTheMemorysProjectPrivacy(t *testing.T) {
	cs, home := creditSession(t)
	showForeign(t, home)
	clients := filepath.Join(home, "clients")
	for _, dir := range []string{filepath.Join(home, "work", "other"), filepath.Join(clients, "acme")} {
		_ = os.MkdirAll(dir, 0o700)
	}
	raw, _ := json.Marshal(retain.Families{BuiltAt: time.Now(), Roots: map[string]string{"third": filepath.Join(clients, "acme")}})
	_ = os.MkdirAll(pathsOf(home).State, 0o700)
	if err := os.WriteFile(pathsOf(home).FamiliesFile(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	appendConfig(t, home, "[privacy]\nexclude_paths = ["+testtool.TOMLStr(filepath.Join(home, "work", "other"))+", "+testtool.TOMLStr(clients)+"]")
	before := retained.Load()
	for id, project := range map[string]string{"cccccccc": "other", "dddddddd": "third"} {
		if out, isErr := call(t, cs, "memory_credit", verdict("wrong", id)); !isErr || !strings.Contains(out, "project "+project+" is excluded from memory") {
			t.Errorf("%s: %v %s", project, isErr, out)
		}
	}
	if !nothingStored(t, home, before) || len(patches()) != 0 {
		t.Errorf("a refused verdict stored or retired: %d sent, patches %q", retained.Load()-before, patches())
	}
}

// What a session retired of another project is listed for review with
// both projects, by memory stale and by stats.
func TestCrossProjectRetirementsAreListedForReview(t *testing.T) {
	cs, home := creditSession(t)
	showForeign(t, home)
	for _, id := range []string{"cccccccc", "aaaaaaaa"} {
		if out, isErr := call(t, cs, "memory_credit", verdict("wrong", id)); isErr || !retiredRE.MatchString(out) {
			t.Fatalf("%s: %v %s", id, isErr, out)
		}
	}
	code, out, errOut := run(t, "memory", "stale")
	if code != output.OK {
		t.Fatalf("memory stale: exit %d: %s %s", code, out, errOut)
	}
	if line := lineWith(out, "cccccccc  shown "); !strings.HasSuffix(line, "  proj → other  backsight forget "+foreignA+" --purge") ||
		!strings.Contains(out, "\n<session's project> → <memory's project>: ") {
		t.Errorf("memory stale does not name both projects:\n%s", out)
	}
	if line := lineWith(out, "aaaaaaaa"); line == "" || strings.Contains(line, "→") {
		t.Errorf("the session's own memory: %q\n%s", line, out)
	}
	if _, js, _ := run(t, "memory", "stale", "--json"); !strings.Contains(js, `"session_project": "proj"`) || !strings.Contains(js, `"memory_project": "other"`) {
		t.Errorf("memory stale --json:\n%s", js)
	}
	_, st, _ := run(t, "stats", "1")
	if !strings.Contains(st, "\nAcross projects  1 memory retired by a session of another project: proj → other cccccccc (wrong)") {
		t.Errorf("stats:\n%s", st)
	}
	if _, js, _ := run(t, "stats", "1", "--json"); !strings.Contains(js, `"cross_project": [`) || !strings.Contains(js, `"memory_project": "other"`) {
		t.Errorf("stats --json:\n%s", js)
	}
}

// lineWith is the first line of s that holds sub, "" when none does.
func lineWith(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}
