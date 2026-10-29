package feedback

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

const (
	idA  = "aaaaaaaa-0000-4000-8000-000000000001"
	idA2 = "aaaaaaaa-1111-4000-8000-000000000002"
	idB  = "bbbbbbbb-0000-4000-8000-000000000003"
)

func TestResolve(t *testing.T) {
	shown := []Show{{ID: idA}, {ID: idA2}, {ID: idB}, {ID: idB}, {ID: "m1"}, {ID: "dddddddd-0000"}, {ID: "dddddddd"}}
	for _, c := range []struct {
		ref, want string
		err       error
	}{
		{idB, idB, nil},               // a full id
		{"bbbbbbbb", idB, nil},        // its 8-character prefix, shown twice: still one id
		{"aaaaaaaa-1", idA2, nil},     // a longer prefix tells two ids apart
		{"m1", "m1", nil},             // a short id that is a whole shown id
		{"dddddddd", "dddddddd", nil}, // a whole id wins over the ids it starts
		{"aaaaaaaa", "", ErrAmbiguous},
		{"cccccccc", "", ErrUnknown},
		{"bbbbbbb", "", ErrUnknown}, // 7 characters
		{"BBBBBBBB", "", ErrUnknown},
		{"m", "", ErrUnknown},
		{"", "", ErrUnknown},
	} {
		got, err := Resolve(c.ref, shown)
		if got != c.want || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("Resolve(%q) = %q, %v; want %q, %v", c.ref, got, err, c.want, c.err)
		}
	}
	if _, err := Resolve(idA, nil); !errors.Is(err, ErrUnknown) {
		t.Errorf("nothing shown: %v", err)
	}
}

// Shows lists the session's shows only, one per item, keyed by the show's
// own event so two shows of one id in one turn stay apart.
func TestShowsOfOneSession(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 0, 0, 123, time.UTC)
	events := []Event{
		{Kind: "turn", Session: "s1", Turn: "p-1", TS: at},
		{Kind: "shows", Session: "s1", Turn: "p-1", Agent: "a7", Project: "billing", Path: "search", TS: at, Items: []Item{{ID: idA, Rank: 1, Project: "infra"}, {ID: idB, Rank: 2}}},
		{Kind: "shows", Session: "s2", Turn: "q-1", Path: "prompt", TS: at, Items: []Item{{ID: "other", Rank: 1}}},
		{Kind: "credit", Session: "s1", Turn: "p-1", IDs: []string{idA}, TS: at},
	}
	got := Shows(events, "s1")
	if len(got) != 2 {
		t.Fatalf("shows %+v", got)
	}
	want := Show{ID: idB, Turn: "p-1", Agent: "a7", Path: "search", Project: "billing", Rank: 2, At: at,
		Key: "s1|p-1|" + "1793178000000000123" + "|" + idB}
	if got[1] != want {
		t.Errorf("show\n got %+v\nwant %+v", got[1], want)
	}
	// an item's own project wins; a line without one (older) keeps the session's
	if got[0].Project != "infra" {
		t.Errorf("item project: %+v", got[0])
	}
	if len(Shows(events, "s3")) != 0 {
		t.Error("another session's shows")
	}
}

// CheckTurn keeps one session's, one turn's and one agent's shows (the
// main agent's first turn adds the briefing), and tells whether that turn
// and agent declared, and was already asked.
func TestCheckTurn(t *testing.T) {
	events := []Event{
		{Kind: "shows", Session: "s1", Turn: "", Path: "briefing", Items: []Item{{ID: "brief", Rank: 1}}},
		{Kind: "shows", Session: "s1", Turn: "t1", Path: "prompt", Items: []Item{{ID: idA, Rank: 1}}},
		{Kind: "shows", Session: "s1", Turn: "t1", Agent: "a1", Path: "search", Items: []Item{{ID: idB, Rank: 1}}},
		{Kind: "shows", Session: "s1", Turn: "t0", Path: "prompt", Items: []Item{{ID: "old", Rank: 1}}},
		{Kind: "shows", Session: "s2", Turn: "t1", Path: "prompt", Items: []Item{{ID: "other", Rank: 1}}},
		{Kind: "credit", Session: "s1", Turn: "t1", Verdict: "none"},
		{Kind: "credit", Session: "s1", Turn: "t1", Agent: "a1", Verdict: "used", IDs: []string{idB}},
		{Kind: "credit", Session: "s1", Turn: "t1", Agent: "a1", Verdict: "wrong", IDs: []string{idA}}, // a report, not a use
		{Kind: "stop", Session: "s1", Turn: "t1", Agent: "a1", Outcome: "blocked"},
		{Kind: "stop", Session: "s1", Turn: "t1", Outcome: "undeclared"},
		{Kind: "credit", Session: "s2", Turn: "t2", Verdict: "none"},
		{Kind: "stop", Session: "s2", Turn: "t2", Outcome: "blocked"},
	}
	ids := func(c Check) string {
		var out []string
		for _, s := range c.Shown {
			out = append(out, s.ID)
		}
		return strings.Join(out, ",")
	}
	main := CheckTurn(events, "s1", "t1", "", false)
	if ids(main) != idA || !main.Declared || main.Blocked || !main.None || len(main.Credited) != 0 {
		t.Errorf("main %s %+v", ids(main), main)
	}
	if got := CheckTurn(events, "s1", "t1", "", true); ids(got) != "brief,"+idA {
		t.Errorf("first turn: %s", ids(got))
	}
	sub := CheckTurn(events, "s1", "t1", "a1", true) // a subagent never takes the main agent's briefing
	if ids(sub) != idB || !sub.Declared || !sub.Blocked || sub.None || strings.Join(sub.Credited, ",") != idB {
		t.Errorf("subagent %s %+v", ids(sub), sub)
	}
	other := CheckTurn(events, "s2", "t1", "", false) // s2's credit and block are of another turn
	if ids(other) != "other" || other.Declared || other.Blocked {
		t.Errorf("s2 %s %+v", ids(other), other)
	}
}

func TestOverlap(t *testing.T) {
	memory := "Billing keeps PostgreSQL 16 for partial indexes."
	for _, c := range []struct {
		reply string
		want  float64
	}{
		{"We keep PostgreSQL 16 on billing because partial indexes need it.", 0.8}, // billing postgresql partial indexes of 5 (keeps ≠ keep)
		{"BILLING, POSTGRESQL!", 0.4},
		{"Nothing to do with it.", 0},
		{"", 0},
	} {
		if got := Overlap(c.reply, memory); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("Overlap(%q) = %v, want %v", c.reply, got, c.want)
		}
	}
	if got := Overlap("anything at all", "a b 16"); got != 0 {
		t.Errorf("a memory without a word of 4 letters: %v", got)
	}
	// distinct words: a repeated word counts once
	if got := Overlap("billing", "billing billing invoices"); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("repeated word: %v", got)
	}
}

// The flags compare the reply with the texts of the memories shown and
// credited: a none that repeats a shown memory, a credit whose memory the
// reply does not echo. A memory without a kept text flags nothing.
func TestHonestyFlags(t *testing.T) {
	texts := map[string]string{idA: "Billing keeps PostgreSQL 16 for partial indexes.", idB: "Deploys go through the staging cluster first."}
	shown := []Show{{ID: idA}, {ID: idB}, {ID: "untold"}}
	reply := "Billing keeps PostgreSQL 16 because of partial indexes."
	for _, c := range []struct {
		name  string
		check Check
		want  string
	}{
		{"none repeating a shown memory", Check{Shown: shown, Declared: true, None: true}, FlagNoneWithOverlap},
		{"credit the reply does not echo", Check{Shown: shown, Declared: true, Credited: []string{idB}}, FlagCreditWithoutOverlap},
		{"credit the reply echoes", Check{Shown: shown, Declared: true, Credited: []string{idA}}, ""},
		{"credit without a kept text", Check{Shown: shown, Declared: true, Credited: []string{"untold"}}, ""},
		{"none and an unechoed credit", Check{Shown: shown, Declared: true, None: true, Credited: []string{idB}}, FlagNoneWithOverlap + "," + FlagCreditWithoutOverlap},
		{"undeclared", Check{Shown: shown}, ""},
	} {
		if got := strings.Join(c.check.Flags(reply, texts), ","); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := (Check{Shown: shown, Declared: true, None: true}).Flags("Unrelated answer about the weather.", texts); len(got) != 0 {
		t.Errorf("none without overlap flagged %v", got)
	}
	// A blank reply (cut, or the agent ended on a tool call) says nothing
	// about what it used.
	for _, blank := range []string{"", " \n\t"} {
		if got := (Check{Shown: shown, Declared: true, Credited: []string{idB}}).Flags(blank, texts); len(got) != 0 {
			t.Errorf("blank reply %q flagged %v", blank, got)
		}
	}
}

// A reply without a word to compare (blank, or a word too short to
// count) cannot tell whether a credited memory was used: no flag. A reply
// with words still flags an unechoed credit.
func TestNoOverlapFlagOnEmptyReply(t *testing.T) {
	texts := map[string]string{idB: "Deploys go through the staging cluster first."}
	c := Check{Shown: []Show{{ID: idB}}, Declared: true, Credited: []string{idB}}
	for _, reply := range []string{"", " \n\t", "OK.", "👍", "--- ok ---"} {
		if got := c.Flags(reply, texts); len(got) != 0 {
			t.Errorf("reply %q flagged %v", reply, got)
		}
	}
	if got := strings.Join(c.Flags("Fixed the billing query.", texts), ","); got != FlagCreditWithoutOverlap {
		t.Errorf("a reply that does not echo the credit: %q", got)
	}
}

func TestRetirementsCountTheSessionsRetiredIDs(t *testing.T) {
	events := []Event{
		{Kind: KindRetire, Session: "s1", Turn: "p-1", IDs: []string{idA}},
		{Kind: KindHide, Session: "s1", Turn: "p-2", IDs: []string{idB}},
		{Kind: KindRetire, Session: "s1", Turn: "p-3", IDs: []string{idA2, "m1"}},
		{Kind: KindRetire, Session: "s2", IDs: []string{idA}},
		{Kind: "credit", Session: "s1", Verdict: "wrong", IDs: []string{idA}},
		{Kind: "forget", Session: "s1", IDs: []string{idB}},
	}
	if n := Retirements(events, "s1"); n != 4 {
		t.Errorf("s1: %d, want 4", n)
	}
	if n := Retirements(events, "s3"); n != 0 {
		t.Errorf("s3: %d", n)
	}
}
