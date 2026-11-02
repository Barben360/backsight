package retain

import (
	"context"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
