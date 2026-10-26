package retain

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// listed is the fake server's document listing, as ListDocuments reads it.
func (e *env) listed() []hindsight.DocumentItem {
	e.t.Helper()
	docs, err := e.f.Client.ListDocuments(context.Background(), e.f.Bank)
	if err != nil {
		e.t.Fatal(err)
	}
	return docs
}

func (e *env) plan() ReattributionPlan {
	e.t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.dir, "*.jsonl"))
	return e.f.PlanReattribution(e.listed(), files, Prices{Input: 1, Output: 2, Currency: "USD"})
}

// apply applies a plan's re-tags and re-sends.
func (e *env) apply(p ReattributionPlan) {
	e.t.Helper()
	for _, r := range p.Retags {
		if err := e.f.ApplyRetag(context.Background(), r, nil); err != nil {
			e.t.Fatal(err)
		}
	}
	e.f.KeepDecisions(p)
	for _, r := range p.Resends {
		if err := e.f.ApplyResend(context.Background(), r); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) tagsOf(doc string) []string {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	return e.hs.tags[doc]
}

// A subagent report filed under its session's directory moves, for free,
// to the own repository its work was in; the second run finds nothing.
func TestReattributeMovesAReportToItsWork(t *testing.T) {
	e, billing, shop, _ := subagentEnv(t)
	e.cwd = billing
	e.record("s1", "u1", "user", 0, "Fix the cart.")
	e.flush("s1", Options{})
	// an older build filed it under the session's directory
	e.agent("s1", "ag1", billing, "Fix the cart.", [3]string{"Edit", "file_path", filepath.Join(shop, "cart.go")})
	ob := NotesOutbox(e.f.Dirs)
	if _, err := ob.Append(NewSubagentNote("claude-memory", "s1", "ag1", "general-purpose", "billing", "Fixed the cart.", clockAt(10, 0))); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.Flush(context.Background(), e.f.Client, e.f.Bank, "m1"); err != nil {
		t.Fatal(err)
	}
	p := e.plan()
	if len(p.Retags) != 1 || p.Retags[0].Document != "subagent:s1:ag1" || p.Retags[0].From != "billing" || p.Retags[0].To != "shop" || len(p.Resends) != 0 {
		t.Fatalf("plan %+v", p)
	}
	if len(e.hs.patched) != 0 {
		t.Fatal("planning wrote to the server")
	}
	e.apply(p)
	if got := e.tagsOf("subagent:s1:ag1"); !reflect.DeepEqual(got, []string{"project:shop", "source:subagent", "harness:claude-code", "agent:general-purpose"}) {
		t.Errorf("tags %v", got)
	}
	if again := e.plan(); len(again.Retags)+len(again.Resends) != 0 {
		t.Errorf("a second run moves again: %+v", again)
	}
	// its decision is kept: the same answer once the transcript is gone
	_ = os.RemoveAll(filepath.Join(e.dir, "s1", "subagents"))
	if again := e.plan(); len(again.Retags) != 0 || again.Judged == 0 {
		t.Errorf("without the transcript: %+v", again)
	}
	raw, _ := os.ReadFile(filepath.Join(e.f.Dirs.State, "reattribute.jsonl"))
	if !strings.Contains(string(raw), `"document":"subagent:s1:ag1"`) || !strings.Contains(string(raw), `"to":"shop"`) {
		t.Errorf("journal %s", raw)
	}
}

// An older build's append relabelled a whole document (a cd into a plain
// directory named the session): every record is the home project's under
// today's rule, so the document is re-tagged in place.
func TestReattributeRetagsARelabelledDocument(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	billing := e.ownRepo("billing", "")
	dist := filepath.Join(filepath.Dir(billing), "npm", "dist")
	_ = os.MkdirAll(dist, 0o700)
	e.recordIn(billing, "s1", "u1", "user", 0, "Build the export.")
	e.recordIn(dist, "s1", "u2", "user", 1, "Look at the bundle.")
	// what an older build left: one document, no project metadata, tagged dist
	e.hs.mu.Lock()
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "u2", "segment": "2"}
	e.hs.tags = map[string][]string{"conversation:s1": {"project:dist", "source:chat", "harness:claude-code"}}
	e.hs.mu.Unlock()
	p := e.plan()
	if len(p.Retags) != 1 || p.Retags[0].Document != "conversation:s1" || p.Retags[0].From != "dist" || p.Retags[0].To != "billing" || len(p.Resends) != 0 {
		t.Fatalf("plan %+v", p)
	}
	e.apply(p)
	if got := e.tagsOf("conversation:s1"); got[0] != "project:billing" || len(e.hs.deleted) != 0 {
		t.Errorf("tags %v, deleted %v", got, e.hs.deleted)
	}
	if again := e.plan(); len(again.Retags)+len(again.Resends) != 0 || again.Unchanged != 1 {
		t.Errorf("a second run: %+v", again)
	}
}

// A document mixing two repositories' records cannot be split by tags: the
// session is sent again per project, at its extraction cost, and the
// resync sends it to one document per project; a second run finds nothing.
func TestReattributeResendsAMixedSession(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	alpha, beta := e.ownRepo("alpha", ""), e.ownRepo("beta", "")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	e.recordIn(alpha, "s1", "u3", "user", 2, "alpha three")
	expect(t, e.flush("s1", Options{}), Sent, 3, 2)
	// what an older build left instead: one mixed document tagged beta
	e.hs.mu.Lock()
	delete(e.hs.docs, "conversation:s1@beta")
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "u3", "segment": "1"}
	e.hs.tags["conversation:s1"] = []string{"project:beta", "source:chat", "harness:claude-code"}
	e.hs.mu.Unlock()
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	p := e.plan()
	if len(p.Retags) != 0 || len(p.Resends) != 1 {
		t.Fatalf("plan %+v", p)
	}
	r := p.Resends[0]
	if r.Session != "s1" || r.Reason != ResendMixed || r.Records != 3 || !reflect.DeepEqual(r.Projects, []string{"alpha", "beta"}) ||
		!reflect.DeepEqual(r.Documents, []string{"conversation:s1"}) || r.Estimate.InputTokens == 0 || p.Cost.Cost == 0 {
		t.Errorf("resend %+v, cost %+v", r, p.Cost)
	}
	sent := len(e.hs.retains)
	e.apply(p)
	if !reflect.DeepEqual(e.hs.deleted, []string{"conversation:s1"}) || len(e.hs.retains) != sent {
		t.Fatalf("deleted %v; sent %d → %d (the re-send is the next flush's)", e.hs.deleted, sent, len(e.hs.retains))
	}
	e.clock = clockAt(13, 0)
	if res := e.flush("s1", Options{}); res.Status != Sent || res.Records != 3 {
		t.Fatalf("the re-send: %+v", res)
	}
	got := map[string]string{}
	for _, it := range e.hs.retains[sent:] {
		got[it.DocumentID] += it.Tags[0] + " "
	}
	if !reflect.DeepEqual(got, map[string]string{"conversation:s1": "project:alpha project:alpha ", "conversation:s1@beta": "project:beta "}) {
		t.Errorf("re-sent %v", got)
	}
	if again := e.plan(); len(again.Retags)+len(again.Resends) != 0 {
		t.Errorf("a second run: %+v", again)
	}
}

// Documents consistent with today's rules, notes, and documents of
// sessions this machine has no transcript of are left alone.
func TestReattributeLeavesTheRestAlone(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	alpha, beta := e.ownRepo("alpha", ""), e.ownRepo("beta", "")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	e.hs.mu.Lock()
	e.hs.docs["note:n1"] = map[string]string{"source": "note"}
	e.hs.tags["note:n1"] = []string{"project:beta", "source:note"}
	e.hs.docs["conversation:other"] = map[string]string{"session_id": "other", "last_uuid": "x", "segment": "1", "project": "zeta"}
	e.hs.tags["conversation:other"] = []string{"project:zeta"}
	e.hs.docs["subagent:other:ag"] = map[string]string{"source": "subagent"}
	e.hs.tags["subagent:other:ag"] = []string{"project:zeta"}
	e.hs.mu.Unlock()
	p := e.plan()
	if len(p.Retags)+len(p.Resends) != 0 || p.Judged != 2 || p.Unchanged != 2 || p.Documents != 5 {
		t.Errorf("plan %+v", p)
	}
}

// A re-send stopped after its marks, before the deletes, loses nothing:
// the server still holds the records, the resync marks them sent again,
// and the next run plans the same re-send.
func TestAnInterruptedResendIsPlannedAgain(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	alpha, beta := e.ownRepo("alpha", ""), e.ownRepo("beta", "")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	e.hs.mu.Lock()
	delete(e.hs.docs, "conversation:s1@beta")
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "u2", "segment": "1"}
	e.hs.deleteStatus = 503 // the deletes fail
	e.hs.mu.Unlock()
	p := e.plan()
	if len(p.Resends) != 1 {
		t.Fatalf("plan %+v", p)
	}
	if err := e.f.ApplyResend(context.Background(), p.Resends[0]); err == nil {
		t.Fatal("a failed delete was not reported")
	}
	sent := len(e.hs.retains)
	e.clock = clockAt(13, 0)
	e.flush("s1", Options{})
	if len(e.hs.retains) != sent {
		t.Errorf("records the server still holds were sent again: %d", len(e.hs.retains)-sent)
	}
	if again := e.plan(); len(again.Resends) != 1 {
		t.Errorf("the re-send is not planned again: %+v", again)
	}
}

// mixedSession: session s1 moved from alpha to beta and was sent by an
// older build as one document tagged beta, its cursor lost since.
func mixedSession(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.attributed()
	alpha, beta := e.ownRepo("alpha", ""), e.ownRepo("beta", "")
	e.recordIn(alpha, "s1", "u1", "user", 0, "alpha one")
	e.recordIn(beta, "s1", "u2", "user", 1, "beta two")
	expect(t, e.flush("s1", Options{}), Sent, 2, 1)
	e.hs.mu.Lock()
	delete(e.hs.docs, "conversation:s1@beta")
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "u2", "segment": "1"}
	e.hs.tags["conversation:s1"] = []string{"project:beta", "source:chat"}
	e.hs.mu.Unlock()
	_ = os.RemoveAll(e.f.Dirs.CursorsDir())
	return e
}

// Retains still queued on the server: nothing is judged as final nor
// deleted (a late append to a deleted document would come back alone).
func TestNothingIsDeletedWhileOperationsAreQueued(t *testing.T) {
	e := mixedSession(t)
	p := e.plan()
	if len(p.Resends) != 1 {
		t.Fatalf("plan %+v", p)
	}
	e.hs.mu.Lock()
	e.hs.queued = 2
	e.hs.mu.Unlock()
	if err := e.f.ReattributionGuard(context.Background()); err == nil || !strings.Contains(err.Error(), "2 operation(s)") {
		t.Errorf("guard: %v", err)
	}
	if err := e.f.ApplyResend(context.Background(), p.Resends[0]); err == nil || len(e.hs.deleted) != 0 {
		t.Fatalf("deleted %v while operations were queued (%v)", e.hs.deleted, err)
	}
	// the records stay held: the next flush sends nothing again
	e.hs.mu.Lock()
	e.hs.queued = 0
	e.hs.mu.Unlock()
	sent := len(e.hs.retains)
	e.clock = clockAt(13, 0)
	e.flush("s1", Options{})
	if len(e.hs.retains) != sent {
		t.Errorf("records sent again: %d", len(e.hs.retains)-sent)
	}
}

func TestTheGuardRefusesAnUnfinishedRestoreOrUpdate(t *testing.T) {
	e := mixedSession(t)
	if err := state.WriteRestoreMarker(e.f.Dirs, state.RestoreMarker{Command: "restore"}); err != nil {
		t.Fatal(err)
	}
	if err := e.f.ReattributionGuard(context.Background()); err == nil || !strings.Contains(err.Error(), "restore") {
		t.Errorf("restore marker: %v", err)
	}
	_ = state.ClearRestoreMarker(e.f.Dirs)
	e.f.Hold = "restore epoch not reconciled: boom"
	if err := e.f.ReattributionGuard(context.Background()); err == nil || !strings.Contains(err.Error(), "not reconciled") {
		t.Errorf("hold: %v", err)
	}
}

// A document holding records this machine's transcript lacks (the session
// went on elsewhere) is never deleted from here: its session is left
// alone, not re-sent.
func TestASessionContinuedElsewhereIsNeverResentFromHere(t *testing.T) {
	e := mixedSession(t)
	e.hs.mu.Lock()
	e.hs.docs["conversation:s1@gamma"] = map[string]string{"session_id": "s1", "last_uuid": "u9-from-machine-b", "segment": "1", "project": "gamma"}
	e.hs.tags["conversation:s1@gamma"] = []string{"project:gamma"}
	e.hs.mu.Unlock()
	p := e.plan()
	if len(p.Resends) != 0 || len(p.Held) != 1 || p.Held[0].Reason != HeldElsewhere {
		t.Fatalf("plan %+v", p)
	}
}

// Quarantined, backfill-queued and pending sessions are left alone, and
// so is one whose directories are gone (today's names would be guesses).
func TestUnsettledOrUnknowableSessionsAreLeftAlone(t *testing.T) {
	for name, c := range map[string]struct {
		prep func(e *env)
		why  string
	}{
		"quarantined": {func(e *env) {
			_ = SaveCursor(e.f.Dirs.CursorsDir(), "s1", Cursor{Quarantined: true, Segments: map[string]int{}})
		}, HeldQuarantined},
		"backfill": {func(e *env) { _ = SaveCursor(e.f.Dirs.CursorsDir(), "s1", Cursor{Backfill: true}) }, HeldBackfill},
		"pending": {func(e *env) {
			_ = os.MkdirAll(e.f.Dirs.CursorsDir(), 0o700)
			_ = os.WriteFile(pendingPath(e.f.Dirs.CursorsDir(), "s1"), []byte("2026-10-24T12:00:00Z"), 0o600)
		}, HeldPending},
		"directory gone": {func(e *env) { _ = os.RemoveAll(filepath.Join(filepath.Dir(e.cwd), "beta")) }, HeldGone},
	} {
		t.Run(name, func(t *testing.T) {
			e := mixedSession(t)
			c.prep(e)
			p := e.plan()
			if len(p.Resends)+len(p.Retags) != 0 || len(p.Held) != 1 || p.Held[0].Reason != c.why {
				t.Errorf("plan %+v", p)
			}
		})
	}
}

// A move the server refuses, or applies to the document but not to its
// memories, is a failure, never a silent success.
func TestARetagIsReadBack(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	billing := e.ownRepo("billing", "")
	e.recordIn(billing, "s1", "u1", "user", 0, "export")
	e.hs.mu.Lock()
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "u1", "segment": "1"}
	e.hs.tags = map[string][]string{"conversation:s1": {"project:dist"}}
	e.hs.patchStatus = 503
	e.hs.mu.Unlock()
	p := e.plan()
	if len(p.Retags) != 1 {
		t.Fatalf("plan %+v", p)
	}
	if err := e.f.ApplyRetag(context.Background(), p.Retags[0], nil); err == nil {
		t.Error("a 503 on the move was taken for a success")
	}
	e.hs.mu.Lock()
	e.hs.patchStatus = 0
	e.hs.staleUnits = map[string][]string{"conversation:s1": {"project:dist"}}
	e.hs.mu.Unlock()
	if err := e.f.ApplyRetag(context.Background(), p.Retags[0], nil); err == nil || !strings.Contains(err.Error(), "1 of the document's memories") {
		t.Errorf("memories left behind: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(e.f.Dirs.State, "reattribute.jsonl"))
	if strings.Count(string(raw), `"phase":"intent"`) != 2 || strings.Contains(string(raw), `"phase":"done"`) {
		t.Errorf("journal:\n%s", raw)
	}
}

// A conversation document moves under its session's lock: a flush still
// running keeps it.
func TestARetagWaitsForTheSessionsFlush(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	billing := e.ownRepo("billing", "")
	e.recordIn(billing, "s1", "u1", "user", 0, "export")
	e.hs.mu.Lock()
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "u1", "segment": "1"}
	e.hs.tags = map[string][]string{"conversation:s1": {"project:dist"}}
	e.hs.mu.Unlock()
	unlock, err := e.f.lockSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	p := e.plan()
	if err := e.f.ApplyRetag(context.Background(), p.Retags[0], nil); err == nil || len(e.hs.patched) != 0 {
		t.Errorf("moved under a running flush: %v %v", err, e.hs.patched)
	}
}

// The re-send generation is kept for good: a second re-send after a
// resync rebuilt the cursor, and a re-send of a session with no record in
// the index, both get operation ids Hindsight has not seen.
func TestReSendsNeverReuseOperationIDs(t *testing.T) {
	resendOnce := func(e *env) {
		t.Helper()
		p := e.plan()
		if len(p.Resends) != 1 {
			t.Fatalf("plan %+v", p)
		}
		e.apply(p)
		sent := len(e.hs.retains)
		e.clock = e.clock.Add(time.Hour)
		if res := e.flush("s1", Options{}); res.Status != Sent || len(e.hs.retains)-sent != 2 {
			t.Fatalf("the re-send stored %d item(s): %+v", len(e.hs.retains)-sent, res)
		}
	}
	e := mixedSession(t)
	resendOnce(e)
	// mixed again (an older client), the cursor rebuilt by a resync
	e.hs.mu.Lock()
	delete(e.hs.docs, "conversation:s1@beta")
	e.hs.docs["conversation:s1"] = map[string]string{"session_id": "s1", "last_uuid": "u2", "segment": "1"}
	e.hs.mu.Unlock()
	_ = os.RemoveAll(filepath.Join(e.f.Dirs.CursorsDir(), "s1.json"))
	_ = os.Remove(filepath.Join(e.f.Dirs.CursorsDir(), "s1.dropped"))
	resendOnce(e)

	// sent from this machine, its index lost since
	e = mixedSession(t)
	e.f.Index, _ = LoadSentIndex(t.TempDir())
	resendOnce(e)
}

// A re-send stopped after deleting part of its documents loses nothing:
// the next flush sends what the deleted one held, the one left is planned
// again.
func TestAResendStoppedBetweenItsDeletesLosesNothing(t *testing.T) {
	e := mixedSession(t)
	e.hs.mu.Lock()
	e.hs.docs["conversation:s1@gamma"] = map[string]string{"session_id": "s1", "last_uuid": "u1", "segment": "1", "project": "gamma"}
	e.hs.tags["conversation:s1@gamma"] = []string{"project:gamma"}
	e.hs.mu.Unlock()
	p := e.plan()
	if len(p.Resends) != 1 || len(p.Resends[0].Documents) != 2 {
		t.Fatalf("plan %+v", p)
	}
	r := p.Resends[0]
	r.Documents = []string{"conversation:s1", "conversation:s1@gamma"}
	e.hs.mu.Lock()
	e.hs.failDelete = map[string]bool{"conversation:s1@gamma": true}
	e.hs.mu.Unlock()
	if err := e.f.ApplyResend(context.Background(), r); err == nil {
		t.Fatal("a failed delete was not reported")
	}
	sent := len(e.hs.retains)
	e.clock = clockAt(13, 0)
	if res := e.flush("s1", Options{}); res.Status != Sent || len(e.hs.retains)-sent != 2 {
		t.Fatalf("what the deleted document held was not sent again: %+v", res)
	}
	if again := e.plan(); len(again.Resends) != 1 || !slices.Contains(again.Resends[0].Documents, "conversation:s1@gamma") {
		t.Errorf("the document left is not planned again: %+v", again)
	}
}

// A report whose decision travels in its metadata is not decided again
// from today's disk when this machine lost its record of it.
func TestAReportsOwnDecisionIsKeptWithoutTheLedger(t *testing.T) {
	e, billing, shop, _ := subagentEnv(t)
	e.recordIn(billing, "s1", "u1", "user", 0, "go")
	e.agent("s1", "ag1", billing, "Fix.", [3]string{"Edit", "file_path", filepath.Join(shop, "a.go")})
	e.hs.mu.Lock()
	e.hs.docs["subagent:s1:ag1"] = map[string]string{"source": "subagent", "attribution": "incomplete", "project": "billing"}
	e.hs.tags = map[string][]string{"subagent:s1:ag1": {"project:billing", "source:subagent"}}
	e.hs.mu.Unlock()
	if p := e.plan(); len(p.Retags) != 0 || p.Unchanged != 1 {
		t.Errorf("plan %+v", p)
	}
}
