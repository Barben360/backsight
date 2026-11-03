package retain

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/project"
)

func (e *env) planFailed(named, resent map[string]time.Time) FailedPlan {
	e.t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.dir, "*.jsonl"))
	return e.f.PlanFailedResends(named, resent, e.listed(), files, Prices{Input: 1, Output: 2, Currency: "USD"})
}

// A later part accepted by the server but failed there (a rejected key)
// leaves its document without it, and nothing reports it (issue 149). The
// session named by the failed retain is planned whole; applied, its
// document is deleted and the next flush sends every record again, the
// one that failed included. Once recorded as sent again, the same failure
// plans nothing.
func TestAFailedPartsSessionIsSentAgainWhole(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "Billing keeps PostgreSQL 16.")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.hs.mu.Lock()
	docsBefore, contentBefore := maps.Clone(e.hs.docs), maps.Clone(e.hs.content)
	for k, v := range e.hs.docs {
		docsBefore[k] = maps.Clone(v)
	}
	e.hs.mu.Unlock()
	e.clock = clockAt(12, 30)
	e.record("s1", "u2", "user", 1, "The cart uses Redis.")
	expect(t, e.flush("s1", Options{Force: true}), Sent, 1, 2)
	// the server failed that append: it holds only the first part
	e.hs.mu.Lock()
	e.hs.docs, e.hs.content = docsBefore, contentBefore
	e.hs.mu.Unlock()
	failedAt := clockAt(12, 31)
	named := map[string]time.Time{"s1": failedAt, "other-pc-session": failedAt}
	p := e.planFailed(named, nil)
	if p.Named != 2 || p.NotHere != 1 || len(p.Resends) != 1 || len(p.Held) != 0 {
		t.Fatalf("plan %+v", p)
	}
	r := p.Resends[0]
	if r.Session != "s1" || r.Reason != ResendFailed || r.Records != 2 || len(r.Documents) != 1 || !strings.HasPrefix(r.Documents[0], "conversation:s1") || r.Estimate.InputTokens == 0 || p.Cost.InputTokens == 0 {
		t.Fatalf("resend %+v", r)
	}
	if len(e.hs.deleted) != 0 {
		t.Fatal("planning deleted something")
	}
	sent := len(e.hs.retains)
	if err := e.f.ApplyResend(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.hs.deleted, r.Documents) {
		t.Fatalf("deleted %v, want %v", e.hs.deleted, r.Documents)
	}
	e.clock = clockAt(13, 0)
	if res := e.flush("s1", Options{}); res.Status != Sent || res.Records != 2 {
		t.Fatalf("the re-send: %+v", res)
	}
	var text string
	for _, it := range e.hs.retains[sent:] {
		text += it.Content
	}
	if !strings.Contains(text, "PostgreSQL 16") || !strings.Contains(text, "Redis") {
		t.Errorf("not every record went again: %q", text)
	}
	if err := RecordFailedResend(e.f.Dirs.State, "s1", e.clock, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	resent, err := FailedResends(e.f.Dirs.State)
	if err != nil || !resent["s1"].Equal(e.clock) {
		t.Fatalf("record %v %v", resent, err)
	}
	if again := e.planFailed(named, resent); len(again.Resends) != 0 || again.Done != 1 {
		t.Errorf("planned again after its re-send: %+v", again)
	}
	// a failure after that re-send plans it again
	if again := e.planFailed(map[string]time.Time{"s1": clockAt(14, 0)}, resent); len(again.Resends) != 1 {
		t.Errorf("a later failure is not planned: %+v", again)
	}
}

// A first part given up (its document absent: no document listed) is
// planned too, with nothing to delete.
func TestAGivenUpSessionWithNoDocumentIsPlanned(t *testing.T) {
	e := newEnv(t)
	e.record("s1", "u1", "user", 0, "Billing keeps PostgreSQL 16.")
	expect(t, e.flush("s1", Options{}), Sent, 1, 1)
	e.hs.mu.Lock()
	e.hs.docs, e.hs.content = map[string]map[string]string{}, map[string]string{}
	e.hs.mu.Unlock()
	p := e.planFailed(map[string]time.Time{"s1": clockAt(12, 0)}, nil)
	if len(p.Resends) != 1 || len(p.Resends[0].Documents) != 0 || p.Resends[0].Records != 1 {
		t.Fatalf("plan %+v", p)
	}
}

// A session continued on another PC (a document whose last record this
// transcript lacks, or one this PC found split), quarantined, or whose
// directory is gone is left alone, saying why: deleting its documents
// could lose what this PC cannot send again.
func TestFailedResendsHoldWhatAReattributionHolds(t *testing.T) {
	cases := map[string]func(e *env){
		HeldElsewhere: func(e *env) {
			e.hs.mu.Lock()
			for _, m := range e.hs.docs {
				m["last_uuid"] = "u9-from-machine-b"
			}
			e.hs.mu.Unlock()
		},
		"split": func(e *env) {
			if err := RecordSplit(e.f.Dirs.State, "s1", "conversation:s1", e.clock); err != nil {
				e.t.Fatal(err)
			}
		},
		HeldQuarantined: func(e *env) {
			cur, _, _ := LoadCursor(e.f.Dirs.CursorsDir(), "s1")
			cur.Quarantined = true
			if err := SaveCursor(e.f.Dirs.CursorsDir(), "s1", cur); err != nil {
				e.t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.record("s1", "u1", "user", 0, "Billing keeps PostgreSQL 16.")
			expect(t, e.flush("s1", Options{}), Sent, 1, 1)
			setup(e)
			p := e.planFailed(map[string]time.Time{"s1": clockAt(12, 0)}, nil)
			want := name
			if name == "split" {
				want = HeldElsewhere
			}
			if len(p.Resends) != 0 || len(p.Held) != 1 || p.Held[0].Reason != want {
				t.Errorf("plan %+v", p)
			}
		})
	}
}

// addWorktree gives repository repo a first commit and a linked worktree
// at wt.
func addWorktree(t *testing.T, repo, wt string) {
	t.Helper()
	for _, args := range [][]string{{"commit", "-q", "--allow-empty", "-m", "init"}, {"worktree", "add", "-q", wt}} {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// removeWorktrees deletes the worktrees wts, as their session's cleanup
// does, and starts a fresh resolver (a later process).
func (e *env) removeWorktrees(wts ...string) {
	e.t.Helper()
	for _, wt := range wts {
		if err := os.RemoveAll(wt); err != nil {
			e.t.Fatal(err)
		}
	}
	e.f.Resolver = project.New(filepath.Dir(filepath.Dir(e.cwd)), []string{".claude"})
	e.f.Attribution.Resolver = e.f.Resolver
}

// A session that worked a while in a git worktree removed since (issue
// 168): the worktree's turns are filed, as they were when they ran, under
// its repository, which this PC's record of the session's turns says, and
// the session is sent again whole, saying how many records were in a
// removed directory. Only a basename could name that directory now.
func TestAFailedSessionWithARemovedWorktreeIsSentAgainAsItWasFiled(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	billing := e.ownRepo("billing", "")
	wt := filepath.Join(billing, ".claude", "worktrees", "fix-export")
	addWorktree(t, billing, wt)
	e.recordIn(billing, "s1", "u1", "user", 0, "Plan the export.")
	e.recordIn(billing, "s1", "u2", "assistant", 1, "Planned.")
	e.recordIn(wt, "s1", "u3", "user", 2, "Fix the export in the worktree.")
	e.recordIn(wt, "s1", "u4", "assistant", 3, "Fixed the CSV quoting in the worktree.")
	e.recordIn(billing, "s1", "u5", "user", 4, "Merged it.")
	expect(t, e.flush("s1", Options{}), Sent, 5, 1)
	e.removeWorktrees(wt)

	p := e.planFailed(map[string]time.Time{"s1": clockAt(12, 30)}, nil)
	if len(p.Held) != 0 || len(p.Resends) != 1 {
		t.Fatalf("plan %+v", p)
	}
	r := p.Resends[0]
	if r.Records != 5 || r.Gone != 2 || !reflect.DeepEqual(r.Projects, []string{"billing"}) {
		t.Fatalf("resend %+v", r)
	}
	sent := len(e.hs.retains)
	if err := e.f.ApplyResend(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	e.clock = clockAt(13, 0)
	if res := e.flush("s1", Options{}); res.Status != Sent || res.Records != 5 {
		t.Fatalf("the re-send: %+v", res)
	}
	var text string
	for _, it := range e.hs.retains[sent:] {
		if it.DocumentID != "conversation:s1" || !slices.Contains(it.Tags, "project:billing") {
			t.Errorf("sent again to %s %v, want conversation:s1 under billing", it.DocumentID, it.Tags)
		}
		text += it.Content
	}
	if !strings.Contains(text, "CSV quoting") {
		t.Errorf("the worktree's turns did not go again: %q", text)
	}
}

// A removed directory whose records cannot be filed as they were holds
// the whole session (its documents would be deleted, then its records
// sent under a guess), and the reason names each such directory and why;
// one that can be filed as before is not named.
func TestAFailedSessionIsHeldForARemovedDirectoryItCannotFile(t *testing.T) {
	type fixture struct {
		e        *env
		flushed  bool     // sent by the fixture itself
		gone     []string // removed after the flush
		named    []string // the directories the reason names
		unnamed  []string // and those it does not
		contains []string
	}
	cases := map[string]func(t *testing.T) fixture{
		// a worktree of another repository than the session's: its turns
		// went to beta; today they would follow alpha's
		"another repository": func(t *testing.T) fixture {
			e := newEnv(t)
			e.attributed()
			alpha, beta := e.ownRepo("alpha", ""), e.ownRepo("beta", "")
			wt := filepath.Join(beta, ".claude", "worktrees", "fix")
			addWorktree(t, beta, wt)
			e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
			e.recordIn(wt, "s1", "u2", "user", 1, "beta in its worktree")
			e.recordIn(wt, "s1", "u3", "assistant", 2, "done in beta's worktree")
			return fixture{e: e, gone: []string{wt}, named: []string{wt}, contains: []string{"filed under beta", "alpha"}}
		},
		// no record on this PC of where its turns were filed
		"turns not recorded": func(t *testing.T) fixture {
			e := newEnv(t)
			e.attributed()
			billing := e.ownRepo("billing", "")
			wt := filepath.Join(billing, ".claude", "worktrees", "fix")
			addWorktree(t, billing, wt)
			e.recordIn(billing, "s1", "u1", "user", 0, "billing one")
			e.recordIn(wt, "s1", "u2", "user", 1, "billing in its worktree")
			return fixture{e: e, gone: []string{wt, e.f.Attribution.Dir}, named: []string{wt}, contains: []string{"no record"}}
		},
		// every directory of the session is gone: its first would be
		// named by its basename
		"every directory gone": func(t *testing.T) fixture {
			e := newEnv(t)
			e.attributed()
			billing := e.ownRepo("billing", "")
			wt := filepath.Join(billing, ".claude", "worktrees", "fix")
			addWorktree(t, billing, wt)
			e.recordIn(wt, "s1", "u1", "user", 0, "billing in its worktree")
			e.recordIn(wt, "s1", "u2", "assistant", 1, "done")
			return fixture{e: e, gone: []string{wt}, named: []string{wt}, contains: []string{"filed under billing", "under fix"}}
		},
		// its repository was kept out of memory: that turn was neither sent
		// nor recorded, and must not go out now that nothing says so
		"kept out when it ran": func(t *testing.T) fixture {
			e := newEnv(t)
			e.attributed()
			alpha, beta := e.ownRepo("alpha", ""), e.ownRepo("beta", "")
			e.f.Exclude = []string{beta}
			wt := filepath.Join(filepath.Dir(beta), "beta-secret")
			addWorktree(t, beta, wt)
			e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
			e.recordIn(wt, "s1", "u2", "user", 1, "the secret beta work")
			e.recordIn(alpha, "s1", "u3", "user", 2, "alpha again")
			return fixture{e: e, gone: []string{wt}, named: []string{wt}, contains: []string{"no record"}}
		},
		// kept out of memory part-way (a retain = false file in the
		// worktree, gone with it): its later turn was never sent, though
		// an earlier one says where the worktree's records went
		"kept out part-way": func(t *testing.T) fixture {
			e := newEnv(t)
			e.attributed()
			alpha := e.ownRepo("alpha", "")
			wt := filepath.Join(alpha, ".claude", "worktrees", "fix")
			addWorktree(t, alpha, wt)
			e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
			e.recordIn(wt, "s1", "u2", "user", 1, "alpha in its worktree")
			if res := e.flush("s1", Options{}); res.Status != Sent {
				t.Fatalf("first send: %+v", res)
			}
			if err := os.WriteFile(filepath.Join(wt, ".backsight.toml"), []byte("retain = false\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			e.recordIn(wt, "s1", "u3", "user", 2, "the private part")
			e.recordIn(wt, "s1", "u4", "assistant", 3, "private answer")
			if res := e.flush("s1", Options{}); res.Status == Sent {
				t.Fatalf("the private part was sent: %+v", res)
			}
			return fixture{e: e, flushed: true, gone: []string{wt}, named: []string{wt}, contains: []string{"no record"}}
		},
		// a few records of a turn filed under alpha: what they were filed
		// under is not known
		"a minority of its turn": func(t *testing.T) fixture {
			e := newEnv(t)
			e.attributed()
			alpha, beta := e.ownRepo("alpha", ""), e.ownRepo("beta", "")
			wt := filepath.Join(beta, ".claude", "worktrees", "fix")
			addWorktree(t, beta, wt)
			e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
			e.recordIn(alpha, "s1", "u2", "assistant", 1, "alpha two")
			e.recordIn(alpha, "s1", "u3", "assistant", 2, "alpha three")
			e.recordIn(wt, "s1", "u4", "assistant", 3, "a glance at beta")
			return fixture{e: e, gone: []string{wt}, named: []string{wt}, contains: []string{"no record"}}
		},
		// two worktrees removed: alpha's is filed as before, beta's is not
		"a mix": func(t *testing.T) fixture {
			e := newEnv(t)
			e.attributed()
			alpha, beta := e.ownRepo("alpha", ""), e.ownRepo("beta", "")
			wtA := filepath.Join(alpha, ".claude", "worktrees", "a")
			wtB := filepath.Join(beta, ".claude", "worktrees", "b")
			addWorktree(t, alpha, wtA)
			addWorktree(t, beta, wtB)
			e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
			e.recordIn(wtA, "s1", "u2", "user", 1, "alpha in its worktree")
			e.recordIn(wtB, "s1", "u3", "user", 2, "beta in its worktree")
			return fixture{e: e, gone: []string{wtA, wtB}, named: []string{wtB}, unnamed: []string{wtA}, contains: []string{"filed under beta"}}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			fx := setup(t)
			e := fx.e
			if res := e.flush("s1", Options{}); !fx.flushed && res.Status != Sent {
				t.Fatalf("first send: %+v", res)
			}
			e.removeWorktrees(fx.gone...)
			p := e.planFailed(map[string]time.Time{"s1": clockAt(12, 30)}, nil)
			if len(p.Resends) != 0 || len(p.Held) != 1 || p.Held[0].Session != "s1" {
				t.Fatalf("plan %+v", p)
			}
			why := p.Held[0].Reason
			if !strings.HasPrefix(why, HeldGone) {
				t.Errorf("reason %q does not say a directory is gone", why)
			}
			for _, d := range fx.named {
				if !strings.Contains(why, d) {
					t.Errorf("reason %q does not name %s", why, d)
				}
			}
			for _, d := range fx.unnamed {
				if strings.Contains(why, d) {
					t.Errorf("reason %q names %s, which is filed as before", why, d)
				}
			}
			for _, s := range fx.contains {
				if !strings.Contains(why, s) {
					t.Errorf("reason %q lacks %q", why, s)
				}
			}
			if len(e.hs.deleted) != 0 {
				t.Error("planning deleted something")
			}
		})
	}
}
