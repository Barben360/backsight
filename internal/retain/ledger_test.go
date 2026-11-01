package retain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/attribution"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/testtool"
)

// ownRepo is e.repo with an origin remote: owner me unless given.
func (e *env) ownRepo(name, remote string) string {
	e.t.Helper()
	testtool.RequireGit(e.t)
	dir := e.repo(name)
	if remote == "" {
		remote = "git@github.com:me/" + name + ".git"
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", remote).CombinedOutput(); err != nil {
		e.t.Fatalf("git remote: %v\n%s", err, out)
	}
	return dir
}

// attributed turns the path rule on, its ledger in the state directory.
func (e *env) attributed() {
	e.f.Attribution = Attributor{Resolver: e.f.Resolver, Transverse: []string{".claude"}, Dir: filepath.Join(e.f.Dirs.State, "attribution")}
}

// tool appends to session s1 an assistant record making one tool call,
// in cwd.
func (e *env) tool(uuid, cwd string, minute int, name string, input map[string]string) {
	e.t.Helper()
	e.toolLine(filepath.Join(e.dir, "s1.jsonl"), false, "s1", uuid, cwd, minute, name, input)
}

func (e *env) toolLine(file string, sidechain bool, sid, uuid, cwd string, minute int, name string, input map[string]string) {
	e.t.Helper()
	ts := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute).Format(time.RFC3339)
	in, _ := json.Marshal(input)
	line := fmt.Sprintf(`{"type":"assistant","isSidechain":%v,"uuid":%q,"sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"t-%s","name":%q,"input":%s}]}}`+"\n",
		sidechain, uuid, sid, cwd, ts, uuid, name, in)
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	fh, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = fh.WriteString(line)
	_ = fh.Close()
}

// agent writes subagent agent's transcript of session sid: its prompt,
// then one tool call per entry of calls ({tool, key, value}), all in cwd,
// then its report.
func (e *env) agent(sid, agent, cwd, prompt string, calls ...[3]string) string {
	e.t.Helper()
	file := filepath.Join(e.dir, sid, "subagents", "agent-"+agent+".jsonl")
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	head := fmt.Sprintf(`{"type":"user","isSidechain":true,"uuid":"%s-0","cwd":%q,"timestamp":"2026-10-24T10:00:00Z","message":{"role":"user","content":%q}}`+"\n", agent, cwd, prompt)
	if err := os.WriteFile(file, []byte(head), 0o600); err != nil {
		e.t.Fatal(err)
	}
	for i, c := range calls {
		e.toolLine(file, true, sid, fmt.Sprintf("%s-%d", agent, i+1), cwd, 61+i, c[0], map[string]string{c[1]: c[2]})
	}
	return file
}

// ledger lists session s1's recorded main turns.
func (e *env) ledger() []TurnEntry {
	e.t.Helper()
	all, err := e.f.Attribution.Turns(time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []TurnEntry
	for _, t := range all {
		if t.Session == "s1" && !t.Subagent() {
			out = append(out, t)
		}
	}
	return out
}

// On main turns the path rule runs in shadow: what is sent does not
// change, and each finished turn records the rule's answer beside the
// applied one, once.
func TestMainTurnsRunThePathRuleInShadow(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	billing := e.ownRepo("billing", "")
	shop := e.ownRepo("shop", "")
	e.cwd = billing
	e.record("s1", "u1", "user", 0, "Fix the checkout in the shop.")
	e.tool("a1", billing, 1, "Edit", map[string]string{"file_path": filepath.Join(shop, "cart.go")})
	e.tool("a2", billing, 2, "Write", map[string]string{"file_path": filepath.Join(shop, "cart_test.go")})
	e.record("s1", "u2", "user", 3, "And the invoices here.")
	e.tool("a3", billing, 4, "Read", map[string]string{"file_path": filepath.Join(billing, "invoice.go")})
	r := e.flush("s1", Options{})
	if r.Status != Sent {
		t.Fatalf("%+v", r)
	}
	for _, it := range e.hs.retains {
		if it.DocumentID != "conversation:s1" || it.Tags[0] != "project:billing" {
			t.Errorf("the shadow rule changed what was sent: %s %v", it.DocumentID, it.Tags)
		}
	}
	got := map[string][3]string{}
	for _, x := range r.Turns {
		got[x.Turn] = [3]string{x.Applied, x.PathRule, x.Source}
	}
	want := map[string][3]string{"u1": {"billing", "shop", attribution.SourceEvidence}, "u2": {"billing", "billing", attribution.SourceCWD}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("turns %v, want %v", got, want)
	}
	before := e.ledger()
	if r := e.flush("s1", Options{}); len(r.Turns) != 0 {
		t.Errorf("a turn was recorded twice: %+v", r.Turns)
	}
	// A resync (the cursor lost) sends nothing again and records nothing
	// new; recording from scratch gives the same answers.
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	sent := len(e.hs.retains)
	e.flush("s1", Options{})
	if len(e.hs.retains) != sent {
		t.Errorf("the resync sent again: %d → %d", sent, len(e.hs.retains))
	}
	if after := e.ledger(); !reflect.DeepEqual(after, before) {
		t.Errorf("the ledger changed on a resync:\n%+v\n%+v", before, after)
	}
	_ = os.RemoveAll(e.f.Attribution.Dir)
	e.flush("s1", Options{})
	if again := e.ledger(); !reflect.DeepEqual(again, before) {
		t.Errorf("recorded again from scratch, the answers differ:\n%+v\n%+v", before, again)
	}
}

// A turn still running is recorded once it ends: another prompt follows,
// the session settles, or the flush is forced (session end).
func TestARunningTurnIsRecordedWhenItEnds(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	e.cwd = e.ownRepo("billing", "")
	e.record("s1", "u1", "user", 170, "first") // 11:50, 10 min before the clock
	if r := e.flush("s1", Options{}); len(r.Turns) != 0 {
		t.Fatalf("a running turn was recorded: %+v", r.Turns)
	}
	e.record("s1", "u2", "user", 172, "second")
	if r := e.flush("s1", Options{}); len(r.Turns) != 1 || r.Turns[0].Turn != "u1" {
		t.Fatalf("the finished turn: %+v", r.Turns)
	}
	if r := e.flush("s1", Options{Force: true}); len(r.Turns) != 1 || r.Turns[0].Turn != "u2" {
		t.Errorf("forced: %+v", r.Turns)
	}
}

// A turn kept out of memory is never recorded: its directory's name stays
// on no disk but the transcript.
func TestKeptOutTurnsAreNotRecorded(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	billing := e.ownRepo("billing", "")
	secret := e.ownRepo("secret-plans", "")
	e.f.Exclude = []string{secret}
	e.f.Attribution.Exclude = e.f.Exclude
	e.recordIn(billing, "s1", "u1", "user", 0, "work")
	e.recordIn(secret, "s1", "u2", "user", 1, "private")
	e.tool("a2", secret, 2, "Edit", map[string]string{"file_path": filepath.Join(secret, "plan.md")})
	e.recordIn(billing, "s1", "u3", "user", 3, "back, reading the plan")
	e.tool("a3", billing, 4, "Read", map[string]string{"file_path": filepath.Join(secret, "plan.md")})
	e.flush("s1", Options{})
	var turns []string
	for _, x := range e.ledger() {
		turns = append(turns, x.Turn)
		if x.PathRule == "secret-plans" {
			t.Errorf("an excluded repository became the path rule's answer: %+v", x)
		}
	}
	if !reflect.DeepEqual(turns, []string{"u1", "u3"}) {
		t.Errorf("recorded %v", turns)
	}
	raw, _ := os.ReadFile(filepath.Join(e.f.Attribution.Dir, "s1", "main.jsonl"))
	if strings.Contains(string(raw), "secret-plans") {
		t.Errorf("the excluded project's name was written:\n%s", raw)
	}
}

// subagentEnv: a session launched in launch, the user's repositories
// billing and shop (owner me), a third-party clone vendor-tool.
func subagentEnv(t *testing.T) (e *env, billing, shop, vendor string) {
	e = newEnv(t)
	e.attributed()
	billing, shop = e.ownRepo("billing", ""), e.ownRepo("shop", "")
	vendor = e.ownRepo("vendor-tool", "https://github.com/someone/vendor-tool.git")
	return e, billing, shop, vendor
}

// Each situation E32 graded, on a subagent's own transcript.
func TestSubagentReportsFollowTheirWork(t *testing.T) {
	e, billing, shop, vendor := subagentEnv(t)
	home := filepath.Dir(filepath.Dir(billing))
	claude := filepath.Join(home, ".claude")
	_ = os.MkdirAll(claude, 0o700)
	e.f.Attribution.Owners = nil
	read := func(p string) [3]string { return [3]string{"Read", "file_path", p} }
	edit := func(p string) [3]string { return [3]string{"Edit", "file_path", p} }
	cases := []struct {
		name, launch, base string
		prompt             string
		calls              [][3]string
		owners             []string
		want, source       string
	}{
		{"launched in a repository, edits another own one", billing, "billing", "Fix the cart.",
			[][3]string{edit(filepath.Join(shop, "cart.go"))}, nil, "shop", attribution.SourceEvidence},
		{"launched in a repository, two reads elsewhere stay", billing, "billing", "Look around.",
			[][3]string{read(filepath.Join(shop, "a.go")), read(filepath.Join(shop, "b.go"))}, nil, "billing", attribution.SourceCWD},
		{"writes outweigh reads", billing, "billing", "Port the change.",
			[][3]string{read(filepath.Join(billing, "a.go")), edit(filepath.Join(shop, "b.go"))}, nil, "shop", attribution.SourceEvidence},
		{"the same turn reading instead of editing stays", billing, "billing", "Port the change.",
			[][3]string{read(filepath.Join(billing, "a.go")), read(filepath.Join(shop, "b.go"))}, nil, "billing", attribution.SourceCWD},
		{"a third-party clone is never a project", billing, "billing", "Build the package.",
			[][3]string{edit(filepath.Join(vendor, "PKGBUILD")), edit(filepath.Join(vendor, "x.patch"))}, nil, "billing", attribution.SourceCWD},
		{"launched in ~/.claude, works in a repository of the same owner", claude, ".claude", "Set up the hooks.",
			[][3]string{edit(filepath.Join(shop, "a.go")), read(filepath.Join(shop, "b.go"))}, nil, ".claude", attribution.SourceNone},
		{"launched in ~/.claude, the owner configured", claude, ".claude", "Set up the hooks.",
			[][3]string{edit(filepath.Join(shop, "a.go")), read(filepath.Join(shop, "b.go"))}, []string{"github.com/me"}, "shop", attribution.SourceEvidence},
		{"launched in ~/.claude, too little evidence", claude, ".claude", "Set up the hooks.",
			[][3]string{read(filepath.Join(shop, "a.go")), read(filepath.Join(shop, "b.go"))}, []string{"github.com/me"}, ".claude", attribution.SourceNone},
		{"launched in ~, nothing own is known", home, ".claude", "Clean the machine.",
			[][3]string{edit(filepath.Join(shop, "a.go")), edit(filepath.Join(shop, "b.go"))}, nil, ".claude", attribution.SourceNone},
	}
	for i, c := range cases {
		sid := fmt.Sprintf("s%d", i)
		e.recordIn(c.launch, sid, "u1", "user", 0, "go")
		e.f.Attribution.Owners = c.owners
		sess := filepath.Join(e.dir, sid+".jsonl")
		p := e.agent(sid, "ag1", c.launch, c.prompt, c.calls...)
		got, _ := e.f.Attribution.Subagent(sid, "ag1", sess, p, c.base, "", clockAt(10, 5))
		if got.Applied != c.want || got.Source != c.source || got.Base != c.base {
			t.Errorf("%s: %q (%s) from %q, want %q (%s); votes %+v", c.name, got.Applied, got.Source, got.Base, c.want, c.source, got.Votes)
		}
	}
}

// A report rebuilt after a restore lands where SubagentStop filed it, even
// once the worktree its work was in is removed; and a ledger lost too is
// rebuilt the same from the session's own worktree command.
func TestARebuiltReportKeepsItsProject(t *testing.T) {
	e, billing, shop, _ := subagentEnv(t)
	testtool.RequireGit(t)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	_ = os.WriteFile(filepath.Join(shop, "a.go"), []byte("package a\n"), 0o600)
	run("-C", shop, "add", ".")
	run("-C", shop, "commit", "-q", "-m", "init")
	wt := filepath.Join(filepath.Dir(filepath.Dir(billing)), ".cache", "dev", "wt-cart")
	e.cwd = billing
	e.record("s1", "u1", "user", 0, "Fix the cart, in a worktree.")
	e.tool("a1", billing, 1, "Bash", map[string]string{"command": "git -C " + shop + " worktree add " + wt + " -b cart"})
	run("-C", shop, "worktree", "add", "-q", wt, "-b", "cart")
	sess := filepath.Join(e.dir, "s1.jsonl")
	p := e.agent("s1", "ag1", wt, "Fix the cart.", [3]string{"Edit", "file_path", filepath.Join(wt, "a.go")}, [3]string{"Bash", "command", "git -C " + wt + " commit -am fix"})
	e.subagentReply("s1", "ag1", wt, "Fixed the cart rounding.")
	first, _ := e.f.Attribution.Subagent("s1", "ag1", sess, p, "billing", "", clockAt(12, 50))
	if first.Applied != "shop" {
		t.Fatalf("first answer %+v", first)
	}
	run("-C", shop, "worktree", "remove", "--force", wt)

	// Recomputed without the ledger (another directory): the session's
	// own worktree command still names the removed directory.
	fresh := e.f.Attribution
	fresh.Dir = t.TempDir()
	if again, _ := fresh.Subagent("s1", "ag1", sess, p, "billing", "", clockAt(12, 50)); again.Applied != "shop" || !reflect.DeepEqual(again.Votes, first.Votes) {
		t.Errorf("recomputed after the worktree was removed: %+v, first %+v", again, first)
	}
	// The restore lost the report, and nothing on disk names the worktree
	// any more (the command gone too): the rebuilt report keeps the
	// project SubagentStop gave it, from the ledger.
	if err := os.WriteFile(sess, []byte(`{"type":"user","uuid":"u1","sessionId":"s1","cwd":"`+testtool.JSONPath(billing)+`","timestamp":"2026-10-24T09:00:00Z","message":{"role":"user","content":"Fix the cart."}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if lost := fresh; true {
		lost.Dir = t.TempDir()
		if got, _ := lost.Subagent("s1", "ag1", sess, p, "billing", "", clockAt(12, 50)); got.Applied == "shop" {
			t.Fatalf("the fixture still names the worktree: %+v", got)
		}
	}
	if _, err := e.f.rebuildReports(context.Background(), "s1", sess, nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	notes, _ := NotesOutbox(e.f.Dirs).read()
	if len(notes) != 1 || notes[0].Project != "shop" {
		t.Fatalf("rebuilt %+v", notes)
	}
}

// subagentReply appends the subagent's final report to its transcript.
func (e *env) subagentReply(sid, agent, cwd, reply string) {
	file := filepath.Join(e.dir, sid, "subagents", "agent-"+agent+".jsonl")
	line := `{"type":"assistant","isSidechain":true,"uuid":"` + agent + `-end","timestamp":"2026-10-24T12:50:00Z","cwd":"` + testtool.JSONPath(cwd) + `","message":{"role":"assistant","content":[{"type":"text","text":` + quote(reply) + `}]}}` + "\n"
	fh, _ := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	_, _ = fh.WriteString(line)
	_ = fh.Close()
}

// A report whose evidence lies in a repository kept out of memory, by a
// path, a retain = false file or a paused project, stays with its
// session's project, and never names that repository.
func TestSubagentReportsNeverMoveToAnExcludedRepository(t *testing.T) {
	e, billing, shop, _ := subagentEnv(t)
	edit := [3]string{"Edit", "file_path", filepath.Join(shop, "cart.go")}
	for i, setup := range []func(){
		func() { e.f.Attribution.Exclude = []string{shop} }, // privacy.exclude_paths, and pause --project
		func() {
			e.f.Attribution.Exclude = nil
			_ = os.WriteFile(filepath.Join(shop, ".backsight.toml"), []byte("retain = false\n"), 0o600)
		},
	} {
		setup()
		sid := fmt.Sprintf("x%d", i)
		e.recordIn(billing, sid, "u1", "user", 0, "go")
		p := e.agent(sid, "ag1", billing, "Fix the cart.", edit, edit)
		got, err := e.f.Attribution.Subagent(sid, "ag1", filepath.Join(e.dir, sid+".jsonl"), p, "billing", "", clockAt(10, 5))
		if err != nil || got.Applied != "billing" || slices.Contains(got.Votes.Projects(), "shop") {
			t.Errorf("case %d: %+v %v", i, got, err)
		}
	}
}

// A decision kept before the user excluded its repository gives way to
// the exclusion: a rebuilt report never goes to a project now kept out.
func TestAKeptDecisionHonoursALaterExclusion(t *testing.T) {
	e, billing, shop, _ := subagentEnv(t)
	e.recordIn(billing, "s1", "u1", "user", 0, "go")
	sess := filepath.Join(e.dir, "s1.jsonl")
	p := e.agent("s1", "ag1", billing, "Fix the cart.", [3]string{"Edit", "file_path", filepath.Join(shop, "cart.go")})
	if first, _ := e.f.Attribution.Subagent("s1", "ag1", sess, p, "billing", "", clockAt(10, 5)); first.Applied != "shop" {
		t.Fatalf("first %+v", first)
	}
	e.f.Attribution.Exclude = []string{shop} // backsight pause --project, later
	again, _ := e.f.Attribution.Subagent("s1", "ag1", sess, p, "billing", "", clockAt(10, 5))
	if again.Applied != "billing" || again.PathRule != "billing" || again.Source != SourceExcluded || slices.Contains(again.Votes.Projects(), "shop") || again.Votes.Roots["shop"] != "" {
		t.Errorf("after the exclusion, the decision still names it: %+v", again)
	}
	if r := e.f.Attribution.Redecide(TurnEntry{Base: "billing", Votes: attribution.Votes{Explicit: []attribution.Count{{Project: "shop", N: 9}}, Roots: map[string]string{"shop": shop}}}); r.Applied != "billing" {
		t.Errorf("redecided %+v", r)
	}
}

// A transcript behind the report, or one that cannot be read, is never
// judged on partial evidence: the report keeps its session's project, and
// that decision is provisional: a later reader, once the transcript is
// complete, decides on the evidence and keeps that.
func TestALaggingOrUnreadableTranscriptIsDecidedLater(t *testing.T) {
	e, billing, shop, _ := subagentEnv(t)
	e.recordIn(billing, "s1", "u1", "user", 0, "go")
	sess := filepath.Join(e.dir, "s1.jsonl")
	p := e.agent("s1", "ag1", billing, "Fix.", [3]string{"Edit", "file_path", filepath.Join(shop, "a.go")})
	got, err := e.f.Attribution.Subagent("s1", "ag1", sess, p, "billing", "Fixed the cart.", clockAt(10, 5))
	if err != nil || got.Applied != "billing" || got.Source != SourceIncomplete || !got.Provisional {
		t.Fatalf("lagging: %+v %v", got, err)
	}
	// still behind: the provisional answer stands
	if again, _ := e.f.Attribution.Subagent("s1", "ag1", sess, p, "billing", "Fixed the cart.", clockAt(10, 5)); again.Applied != "billing" {
		t.Errorf("still lagging: %+v", again)
	}
	e.subagentReply("s1", "ag1", billing, "Fixed the cart.") // the transcript catches up
	later, _ := e.f.Attribution.Subagent("s1", "ag1", sess, p, "billing", "", clockAt(10, 5))
	if later.Applied != "shop" || later.Provisional {
		t.Errorf("decided on the evidence: %+v", later)
	}
	if kept, _ := e.f.Attribution.loadAgent("s1", "ag1"); kept.Applied != "shop" || kept.Provisional {
		t.Errorf("not kept: %+v", kept)
	}
	missing := filepath.Join(e.dir, "s1", "subagents", "agent-ag2.jsonl")
	got, _ = e.f.Attribution.Subagent("s1", "ag2", sess, missing, "billing", "", clockAt(10, 5))
	if got.Source != SourceUnread || !got.Provisional || got.Applied != "billing" {
		t.Errorf("unreadable: %+v", got)
	}
	e.agent("s1", "ag2", billing, "Fix.", [3]string{"Edit", "file_path", filepath.Join(shop, "a.go")})
	if later, _ := e.f.Attribution.Subagent("s1", "ag2", sess, missing, "billing", "", clockAt(10, 5)); later.Applied != "shop" {
		t.Errorf("readable later: %+v", later)
	}
	// past the hook's budget, nothing is resolved: provisional too
	a := e.f.Attribution
	a.Deadline = time.Now().Add(-time.Second)
	e.agent("s1", "ag3", billing, "Fix.", [3]string{"Edit", "file_path", filepath.Join(shop, "a.go")})
	if got, _ := a.Subagent("s1", "ag3", sess, filepath.Join(e.dir, "s1", "subagents", "agent-ag3.jsonl"), "billing", "", clockAt(10, 5)); got.Source != SourceTimeout || !got.Provisional {
		t.Errorf("past the budget: %+v", got)
	}
	// Past it the transcript is not even read (a SubagentStop that started
	// late has no time to): a lagging one times out, never found
	// incomplete, which only its read tells.
	p4 := e.agent("s1", "ag4", billing, "Fix.", [3]string{"Edit", "file_path", filepath.Join(shop, "a.go")})
	if got, _ := a.Subagent("s1", "ag4", sess, p4, "billing", "Fixed the cart.", clockAt(10, 5)); got.Source != SourceTimeout || !got.Provisional {
		t.Errorf("past the budget, a lagging transcript: %+v, want timeout (not read)", got)
	}
}

// The hook and a rebuild may decide the same report at once: one decision
// is kept and both get it.
func TestOneDecisionPerReportUnderConcurrentWriters(t *testing.T) {
	e, billing, shop, _ := subagentEnv(t)
	e.recordIn(billing, "s1", "u1", "user", 0, "go")
	sess := filepath.Join(e.dir, "s1.jsonl")
	p := e.agent("s1", "ag1", billing, "Fix.", [3]string{"Edit", "file_path", filepath.Join(shop, "a.go")})
	var wg sync.WaitGroup
	got := make([]TurnEntry, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := e.f.Attribution
			a.Resolver = project.New(filepath.Dir(filepath.Dir(billing)), []string{".claude"}) // a resolver per process
			base := "billing"
			if i%2 == 1 {
				base = "other" // the rebuild's base differs: whoever is first wins
			}
			got[i], _ = a.Subagent("s1", "ag1", sess, p, base, "", clockAt(10, 5))
		}()
	}
	wg.Wait()
	for _, g := range got[1:] {
		if g.Base != got[0].Base || g.Applied != got[0].Applied {
			t.Fatalf("two decisions for one report: %+v and %+v", got[0], g)
		}
	}
}

// commitAs makes one commit in dir authored by email, with dir's own
// user.email set to me@example.org (the identity backsight reads).
func commitAs(t *testing.T, dir, email string) {
	t.Helper()
	testtool.RequireGit(t)
	for _, args := range [][]string{{"config", "user.email", "me@example.org"}, {"config", "user.name", "u"},
		{"-c", "user.email=" + email, "commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// Own owners are learnt from the launch repositories of retained sessions
// that the user committed to: a session started in ~/.claude can then move
// a report to another repository of that owner. A checkout of someone
// else's code, a remote without an owner, a session kept out (excluded,
// paused, headless) teaches nothing; projects.deny_owners wins.
func TestOwnersAreLearntOnlyFromRepositoriesTheUserCommittedTo(t *testing.T) {
	e, billing, shop, _ := subagentEnv(t)
	home := filepath.Dir(filepath.Dir(billing))
	claude := filepath.Join(home, ".claude")
	_ = os.MkdirAll(claude, 0o700)
	edits := [][3]string{{"Edit", "file_path", filepath.Join(shop, "a.go")}, {"Edit", "file_path", filepath.Join(shop, "b.go")}}
	n := 0
	decide := func() TurnEntry {
		n++
		sid := fmt.Sprintf("c%d", n)
		e.recordIn(claude, sid, "u1", "user", 0, "go")
		p := e.agent(sid, "ag1", claude, "Set up.", edits...)
		got, _ := e.f.Attribution.Subagent(sid, "ag1", filepath.Join(e.dir, sid+".jsonl"), p, ".claude", "", clockAt(10, 5))
		return got
	}
	launch := func(dir, sid string) {
		e.recordIn(dir, sid, "u1", "user", 0, "go")
		e.flush(sid, Options{})
	}
	// someone else's checkout of owner me: the user never committed to it
	commitAs(t, billing, "them@example.org")
	launch(billing, "r0")
	if got := decide(); got.Applied != ".claude" {
		t.Fatalf("learnt from a checkout the user never committed to: %+v", got)
	}
	// a remote with no owner segment
	hostOnly := e.ownRepo("solo", "https://git.example.org/solo.git")
	commitAs(t, hostOnly, "me@example.org")
	launch(hostOnly, "r1")
	if got := e.f.Attribution.Learnt(); len(got) != 0 {
		t.Fatalf("learnt %v from a remote without an owner", got)
	}
	// the user's commit, but the session paused: nothing
	commitAs(t, billing, "me@example.org")
	_ = os.MkdirAll(e.f.Dirs.SessionsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(e.f.Dirs.SessionsDir(), "r2.json"), []byte(`{"paused":true}`), 0o600)
	launch(billing, "r2")
	if got := e.f.Attribution.Learnt(); len(got) != 0 {
		t.Fatalf("learnt %v from a paused session", got)
	}
	// a retained session in a repository the user committed to
	launch(billing, "r3")
	if got := e.f.Attribution.Learnt(); !slices.Equal(got, []string{"github.com/me"}) {
		t.Fatalf("learnt %v", got)
	}
	if got := decide(); got.Applied != "shop" {
		t.Errorf("after a retained session in the user's repository: %+v", got)
	}
	// projects.deny_owners wins over what was learnt
	e.f.Attribution.Deny = []string{"https://github.com/Me"}
	if got := decide(); got.Applied != ".claude" {
		t.Errorf("a denied owner still counts: %+v", got)
	}
}

// The product waits about a second for the owners file's lock.
func TestLearnOwnerWaitsAboutASecond(t *testing.T) {
	if learnOwnerWait != time.Second {
		t.Errorf("learnOwnerWait = %v, want 1s", learnOwnerWait)
	}
}

// Sessions learning owners at once lose none. The wait's bound is the
// test's own, far past the queue: how many fsync'd writes fit in the
// product's 1 s is the disk's speed (windows-latest once kept 1 of 6).
func TestOwnersLearntConcurrentlyAreAllKept(t *testing.T) {
	old := learnOwnerWait
	learnOwnerWait = time.Minute
	t.Cleanup(func() { learnOwnerWait = old })
	e := newEnv(t)
	e.attributed()
	var dirs []string
	for i := range 6 {
		d := e.ownRepo(fmt.Sprintf("r%d", i), fmt.Sprintf("git@github.com:owner%d/r.git", i))
		commitAs(t, d, "me@example.org")
		dirs = append(dirs, d)
	}
	var wg sync.WaitGroup
	for _, d := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := e.f.Attribution
			a.Resolver = project.New(filepath.Dir(filepath.Dir(d)), []string{".claude"})
			if !a.learnOwner(d) {
				t.Error("an owner was not recorded")
			}
		}()
	}
	wg.Wait()
	if got := e.f.Attribution.Learnt(); len(got) != 6 {
		t.Errorf("learnt %v", got)
	}
}

// The decision travels in the report's document metadata with its project.
func TestAReportCarriesItsDecision(t *testing.T) {
	n := NewSubagentNote("b", "s1", "ag1", "Explore", "shop", "Fixed.", clockAt(10, 0))
	if _, ok := n.Item("m").Metadata["attribution"]; ok {
		t.Error("a report with no decision claims one")
	}
	n.Attribution = "evidence"
	if m := n.Item("m").Metadata; m["attribution"] != "evidence" || m["project"] != "shop" {
		t.Errorf("metadata %v", m)
	}
}

func TestOldSessionsRecordsArePruned(t *testing.T) {
	dir := t.TempDir()
	a := Attributor{Dir: dir}
	for _, sid := range []string{"old", "fresh"} {
		_ = os.MkdirAll(filepath.Join(dir, sid), 0o700)
		_ = os.WriteFile(filepath.Join(dir, sid, "main.jsonl"), []byte("{}\n"), 0o600)
	}
	_ = os.WriteFile(filepath.Join(dir, "owners.json"), []byte(`["github.com/me"]`), 0o600)
	old := time.Now().Add(-200 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "old", "main.jsonl"), old, old)
	a.PruneAttribution(time.Now(), AttributionKeep)
	if _, err := os.Stat(filepath.Join(dir, "old")); err == nil {
		t.Error("an old session's records were kept")
	}
	for _, p := range []string{filepath.Join(dir, "fresh", "main.jsonl"), filepath.Join(dir, "owners.json")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was pruned", p)
		}
	}
}
