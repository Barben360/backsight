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
	"github.com/Barben360/backsight/internal/session"
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
	rawFact(creditA)
	rawFactIn("other", foreignA, foreignB)
	rawFactIn("third", thirdA)
}

// foreignRefusal is the line a verdict on foreignA answers while
// feedback.cross_project_retire is off.
const foreignRefusal = "> 🧠⚠️ **Not retired (other · cccccccc)** — a memory of project other, outside this session's project and the projects it links · retire it: `backsight forget " + foreignA + "`"

// crossOn is the config that lets a session retire another project's
// memory (off by default since decision 0089 P1).
const crossOn = "\n[feedback]\ncross_project_retire = true\n"

// With feedback.cross_project_retire = true a session retires a wrong
// memory of any project it was shown: the correction goes to that
// memory's project, the fact is invalidated, and the journal names both
// projects.
func TestCrossProjectRetireStoresTheCorrectionInTheMemorysProject(t *testing.T) {
	cs, home := creditSessionWith(t, crossOn)
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

// By default (no key) another project's memory is refused: nothing
// stored, nothing invalidated, and the answer says plainly that it is off
// and what the user runs; it never tells a possibly injected agent how to
// lift the guard (that is in the docs only).
func TestCrossProjectRetireIsOffByDefault(t *testing.T) {
	cs, home := creditSession(t)
	showForeign(t, home)
	before := retained.Load()
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "cccccccc"))
	if !isErr || !strings.Contains(out, foreignRefusal) ||
		!strings.Contains(out, "retiring another project's memory is off on this machine; the user can retire it with `backsight forget <id>`") ||
		strings.Contains(out, "cross_project_retire") || strings.Contains(out, "let you") {
		t.Errorf("%v %s", isErr, out)
	}
	if !nothingStored(t, home, before) || len(patches()) != 0 {
		t.Errorf("a refused verdict stored or retired: %d sent, patches %q", retained.Load()-before, patches())
	}
	if out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa")); isErr || !retiredRE.MatchString(out) {
		t.Errorf("own: %v %s", isErr, out)
	}
}

// An explicit true, turned to false during the session, applies to the
// next verdict: nothing stored, nothing invalidated.
func TestCrossProjectRetireOffKeepsTheRefusal(t *testing.T) {
	cs, home := creditSessionWith(t, crossOn)
	showForeign(t, home)
	cfg := pathsOf(home).ConfigFile()
	raw, err := os.ReadFile(cfg)
	if err != nil || !strings.Contains(string(raw), "cross_project_retire = true") {
		t.Fatalf("%v %s", err, raw)
	}
	if err := os.WriteFile(cfg, []byte(strings.Replace(string(raw), "cross_project_retire = true", "cross_project_retire = false", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
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
	cs, home := creditSession(t)
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
	cs, home := creditSessionWith(t, crossOn)
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
	cs, home := creditSessionWith(t, "\n[feedback]\nmax_retirements = 3\ncross_project_retire = true\n")
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
	cs, home := creditSessionWith(t, crossOn)
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
	cs, home := creditSessionWith(t, crossOn)
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

// The memory's project comes from the server, never from a line's text:
// a session in foo#1 shown its own memory as foo_1 (a search header)
// retires it, and the correction goes to foo#1 (0089 review S7).
func TestOwnProjectWithAShownNameIsStillOwn(t *testing.T) {
	cs, home := creditSession(t)
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4848, session.Live{SessionID: "S", Cwd: filepath.Join(home, "proj"), Project: "foo#1", At: time.Now()})
	showItems(t, home, "S", "search", "foo#1", feedback.Item{ID: creditA, Rank: 1, Project: "foo_1"})
	memories[creditA] = `{"id":"` + creditA + `","text":"Billing runs PostgreSQL 14.","type":"world","tags":["project:foo#1"]}`
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa"))
	if isErr || !retiredRE.MatchString(out) || strings.Contains(out, "off on this machine") {
		t.Fatalf("%v %s", isErr, out)
	}
	if last := retainedTags[len(retainedTags)-1]; !strings.Contains(last, "project:foo#1,") {
		t.Errorf("the correction went to %q, want project foo#1", last)
	}
}

// The other way round, the guard holds: a session in foo_1 shown another
// project's memory (foo#1, foo[1, foo|1) under the same shown name cannot
// retire it, nothing is stored; the server's project decides (0089
// review round 2).
func TestForeignProjectWithTheSameShownNameIsRefused(t *testing.T) {
	for _, foreign := range []string{"foo#1", "foo[1", "foo|1"} {
		t.Run(foreign, func(t *testing.T) {
			cs, home := creditSession(t)
			_ = session.WriteLive(pathsOf(home).SessionsDir(), 4848, session.Live{SessionID: "S", Cwd: filepath.Join(home, "proj"), Project: "foo_1", At: time.Now()})
			showItems(t, home, "S", "search", "foo_1", feedback.Item{ID: foreignA, Rank: 1, Project: "foo_1"})
			memories[foreignA] = `{"id":"` + foreignA + `","text":"Another project's fact.","type":"world","tags":["project:` + foreign + `"]}`
			before := retained.Load()
			out, isErr := call(t, cs, "memory_credit", verdict("wrong", "cccccccc"))
			if !isErr || !strings.Contains(out, "off on this machine") {
				t.Errorf("%v %s", isErr, out)
			}
			if !nothingStored(t, home, before) || len(patches()) != 0 {
				t.Errorf("a refused verdict stored or retired: %d sent, patches %q", retained.Load()-before, patches())
			}
		})
	}
}

// A memory the server cannot read has only its journaled project, which a
// search header may show for another project's name too: unless that name
// is exactly the session's project, a linked one or the pool, the call is
// refused, even with cross_project_retire on: nothing stored, nothing
// hidden (0089 review round 3).
func TestUnreadableMemoryUnderAForeignShownNameIsRefused(t *testing.T) {
	cs, home := creditSessionWith(t, crossOn)
	_ = session.WriteLive(pathsOf(home).SessionsDir(), 4848, session.Live{SessionID: "S", Cwd: filepath.Join(home, "proj"), Project: "foo#1", At: time.Now()})
	showItems(t, home, "S", "search", "foo#1", feedback.Item{ID: foreignA, Rank: 1, Project: "foo_1"})
	delete(memories, foreignA) // the server cannot read it
	before := retained.Load()
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "cccccccc"))
	if !isErr || !strings.Contains(out, "could not be read to confirm whose memory it is") {
		t.Errorf("%v %s", isErr, out)
	}
	if !nothingStored(t, home, before) || len(patches()) != 0 || len(retireEvents(t, home, feedback.KindHide)) != 0 {
		t.Errorf("stored %d, patches %q, hides %+v", retained.Load()-before, patches(), retireEvents(t, home, feedback.KindHide))
	}
}
