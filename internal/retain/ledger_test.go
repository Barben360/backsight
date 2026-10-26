package retain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/attribution"
)

// ownRepo is e.repo with an origin remote: owner me unless given.
func (e *env) ownRepo(name, remote string) string {
	e.t.Helper()
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
		got := e.f.Attribution.Subagent(sid, "ag1", sess, p, c.base, clockAt(10, 5))
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
	first := e.f.Attribution.Subagent("s1", "ag1", sess, p, "billing", clockAt(12, 50))
	if first.Applied != "shop" {
		t.Fatalf("first answer %+v", first)
	}
	run("-C", shop, "worktree", "remove", "--force", wt)

	// Recomputed without the ledger (another directory): the session's
	// own worktree command still names the removed directory.
	fresh := e.f.Attribution
	fresh.Dir = t.TempDir()
	if again := fresh.Subagent("s1", "ag1", sess, p, "billing", clockAt(12, 50)); again.Applied != "shop" || !reflect.DeepEqual(again.Votes, first.Votes) {
		t.Errorf("recomputed after the worktree was removed: %+v, first %+v", again, first)
	}
	// The restore lost the report, and nothing on disk names the worktree
	// any more (the command gone too): the rebuilt report keeps the
	// project SubagentStop gave it, from the ledger.
	if err := os.WriteFile(sess, []byte(`{"type":"user","uuid":"u1","sessionId":"s1","cwd":"`+billing+`","timestamp":"2026-10-24T09:00:00Z","message":{"role":"user","content":"Fix the cart."}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if lost := fresh; true {
		lost.Dir = t.TempDir()
		if got := lost.Subagent("s1", "ag1", sess, p, "billing", clockAt(12, 50)); got.Applied == "shop" {
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
	line := `{"type":"assistant","isSidechain":true,"uuid":"` + agent + `-end","timestamp":"2026-10-24T12:50:00Z","cwd":"` + cwd + `","message":{"role":"assistant","content":[{"type":"text","text":` + quote(reply) + `}]}}` + "\n"
	fh, _ := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	_, _ = fh.WriteString(line)
	_ = fh.Close()
}
